// Command cocote is an external MCP server that bridges an active Claude Code
// session to Telegram: it sends progress updates and completion notifications,
// asks for interactive approvals (tappable buttons), waits for free-text
// replies, and mirrors the current screen.
//
// Telegram allows only one getUpdates consumer per bot, so cocote uses a
// "booking" lease: the first session to start becomes the ACTIVE consumer and
// gets the full, interactive bridge; other concurrent sessions run in
// SEND-ONLY mode (notify and mirror_screen still work, since sending never
// collides). When the booked session exits, the lease is released and the
// next session can take over.
package main

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ifcoid/cocote/internal/booking"
	"github.com/ifcoid/cocote/internal/config"
	"github.com/ifcoid/cocote/internal/hook"
	"github.com/ifcoid/cocote/internal/server"
	"github.com/ifcoid/cocote/internal/telegram"
)

const version = "0.1.0"

func main() {
	// `cocote mirror` is the Claude Code Stop-hook entrypoint: it reads the
	// hook payload from stdin and mirrors the latest assistant turn to
	// Telegram. It must never block the session, so it always exits 0.
	if len(os.Args) > 1 && os.Args[1] == "mirror" {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cocote mirror:", err)
			return
		}
		hook.RunMirror(cfg, os.Stdin)
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cocote:", err)
		os.Exit(1)
	}
}

func run() error {
	// MCP uses stdout for the protocol, so all logs must go to stderr.
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "cocote: "+format+"\n", a...) }

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	tg := telegram.NewClient(cfg.BotToken, cfg.ChatID)

	lease, err := booking.New(cfg.LeaseDir, cfg.SessionName, cfg.LeaseTTL)
	if err != nil {
		return fmt.Errorf("init booking: %w", err)
	}
	held, holder, err := lease.TryAcquire()
	if err != nil {
		return fmt.Errorf("acquire booking: %w", err)
	}
	defer lease.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	disp := telegram.NewDispatcher(cfg.ChatID)
	srv := server.New(cfg, tg, lease, disp, held)

	// startActive begins consuming Telegram updates and flips the server into
	// ACTIVE mode. Safe to call once, the moment the booking is held.
	startActive := func() {
		srv.SetActive(true)
		srv.SetPollerAlive(true)
		go func() {
			perr := tg.Poll(ctx, cfg.PollTimeout, disp)
			srv.SetPollerAlive(false)
			if perr != nil && ctx.Err() == nil {
				logf("telegram poller stopped: %v — interactive tools are now unavailable", perr)
			}
		}()
		go announce(ctx, tg, cfg.SessionName)
	}

	if held {
		logf("booking acquired by session %q — ACTIVE Telegram consumer (interactive tools enabled)", cfg.SessionName)
		startActive()
	} else {
		who := "another session"
		if holder != nil {
			who = fmt.Sprintf("session %q (pid %d)", holder.Session, holder.PID)
		}
		logf("booking held by %s — SEND-ONLY for now; will take over automatically when it frees", who)
		// Keep trying to acquire the booking. This recovers from the common
		// race where a previous session was killed before releasing its lease:
		// once that stale lease expires (COCOTE_LEASE_TTL), we upgrade to ACTIVE
		// without needing a manual restart.
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if ok, _, err := lease.TryAcquire(); err == nil && ok {
						logf("booking acquired late — upgrading session %q to ACTIVE consumer", cfg.SessionName)
						startActive()
						return
					}
				}
			}
		}()
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "cocote", Version: version}, nil)
	srv.Register(mcpServer)

	logf("MCP server ready on stdio")
	if err := mcpServer.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}

// announce sends a best-effort startup notice once the booking is acquired.
func announce(ctx context.Context, tg *telegram.Client, session string) {
	nctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	text := fmt.Sprintf("✅ <b>cocote</b> booked by session <code>%s</code> — ready for approvals & replies.",
		html.EscapeString(session))
	_, _ = tg.SendMessage(nctx, text, "HTML", nil)
}
