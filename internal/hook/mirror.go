// Package hook implements cocote's Claude Code hook integrations. The mirror
// command is meant to be wired to the Stop hook so that the latest assistant
// message is automatically mirrored to Telegram after every turn, editing a
// single per-session message in place to act like a live clone.
package hook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ifcoid/cocote/internal/config"
	"github.com/ifcoid/cocote/internal/telegram"
)

const mirrorMaxContent = 3500 // keep the whole message under Telegram's 4096 limit

// payload is the subset of the Claude Code hook stdin JSON that we use.
type payload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	HookEventName  string `json:"hook_event_name"`
}

// RunMirror reads a hook payload from stdin and mirrors the latest assistant
// message to Telegram. It is best-effort: problems are logged to stderr but
// never reported as an error, so the caller can exit 0 and never block the
// session (a Stop hook exiting non-zero would interfere with stopping).
func RunMirror(cfg *config.Config, stdin io.Reader) {
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "cocote mirror: "+format+"\n", a...) }

	var p payload
	if err := json.NewDecoder(stdin).Decode(&p); err != nil {
		logf("read hook payload: %v", err)
		return
	}
	if p.TranscriptPath == "" {
		logf("no transcript_path in hook payload; nothing to mirror")
		return
	}

	f, err := os.Open(p.TranscriptPath)
	if err != nil {
		logf("open transcript: %v", err)
		return
	}
	defer f.Close()

	text, err := LastAssistantText(f)
	if err != nil {
		logf("parse transcript: %v", err)
		return
	}
	if strings.TrimSpace(text) == "" {
		return // nothing to show yet
	}

	session := p.SessionID
	if session == "" {
		session = "default"
	}

	if len(text) > mirrorMaxContent {
		text = text[:mirrorMaxContent] + "\n…"
	}
	msg := fmt.Sprintf("🖥️ <b>Claude Code</b> · <code>%s</code>\n<pre>%s</pre>",
		html.EscapeString(cfg.SessionName), html.EscapeString(text))

	tg := telegram.NewClient(cfg.BotToken, cfg.ChatID)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	statePath := filepath.Join(cfg.LeaseDir, "mirror-"+sanitize(session)+".id")
	if id := readID(statePath); id != 0 {
		if err := tg.EditMessageText(ctx, id, msg, "HTML", nil); err == nil || isNotModified(err) {
			return
		}
		// Editing failed (message deleted/too old) → fall through to a fresh send.
	}
	id, err := tg.SendMessage(ctx, msg, "HTML", nil)
	if err != nil {
		logf("send mirror: %v", err)
		return
	}
	if err := writeID(statePath, id); err != nil {
		logf("persist mirror id: %v", err)
	}
}

// transcriptEntry is one line of a Claude Code transcript (.jsonl).
type transcriptEntry struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// LastAssistantText scans a transcript (JSONL) and returns the concatenated
// text of the most recent assistant message that contains any text.
func LastAssistantText(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	last := ""
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e transcriptEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip malformed/irrelevant lines
		}
		role := e.Message.Role
		if role == "" {
			role = e.Type
		}
		if role != "assistant" {
			continue
		}
		if txt := extractText(e.Message.Content); strings.TrimSpace(txt) != "" {
			last = txt
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return last, nil
}

// extractText pulls the text out of a message's content, which may be either a
// plain string or an array of typed content blocks.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func isNotModified(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not modified")
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func readID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	id, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return id
}

func writeID(path string, id int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(id)), 0o644)
}
