// Package server wires cocote's Telegram bridge into MCP tools that Claude
// Code can call: notify, ask_approval, wait_for_reply and mirror_screen.
package server

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ifcoid/cocote/internal/booking"
	"github.com/ifcoid/cocote/internal/config"
	"github.com/ifcoid/cocote/internal/telegram"
)

const notBookedMsg = "cocote is not the booked Telegram consumer for this session, so interactive tools are unavailable. " +
	"Another active Claude Code session currently holds the booking. " +
	"notify and mirror_screen still work (send-only); approvals and replies require the booking."

// Server holds shared state for the MCP tool handlers.
type Server struct {
	cfg   *config.Config
	tg    *telegram.Client
	lease *booking.Lease
	disp  *telegram.Dispatcher

	active      atomic.Bool // true once this process holds the booking and runs the poller
	pollerAlive atomic.Bool
	reqCounter  atomic.Uint64

	mirrorMu  sync.Mutex
	mirrorMsg int
}

// New builds a Server. active reports whether this process acquired the
// booking lease (and therefore consumes Telegram updates) at startup. It may
// be upgraded later via SetActive if the booking frees up.
func New(cfg *config.Config, tg *telegram.Client, lease *booking.Lease, disp *telegram.Dispatcher, active bool) *Server {
	s := &Server{cfg: cfg, tg: tg, lease: lease, disp: disp}
	s.active.Store(active)
	return s
}

// SetActive records whether this process now holds the booking. It is called
// when the lease is acquired late (after starting in send-only mode).
func (s *Server) SetActive(v bool) { s.active.Store(v) }

// SetPollerAlive records whether the long-poller is currently running. When it
// dies (e.g. on a 409 conflict), interactive tools start reporting send-only.
func (s *Server) SetPollerAlive(v bool) { s.pollerAlive.Store(v) }

// Register adds all cocote tools to the MCP server.
func (s *Server) Register(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name:        "notify",
		Description: "Send a progress update or completion notification to Telegram. Works regardless of booking (send-only is fine).",
	}, s.handleNotify)

	mcp.AddTool(m, &mcp.Tool{
		Name:        "ask_approval",
		Description: "Ask the user to approve an action via tappable Telegram buttons and block until they choose or it times out. Set allow_freetext to also offer a 'reply with text' button so the user can talk back with custom instructions instead of just picking an option (the reply is returned in the 'reply' field). Requires this session to hold the cocote booking.",
	}, s.handleAskApproval)

	mcp.AddTool(m, &mcp.Tool{
		Name:        "wait_for_reply",
		Description: "Ask the user a free-form question on Telegram and block until they reply with text or it times out. Requires this session to hold the cocote booking.",
	}, s.handleWaitForReply)

	mcp.AddTool(m, &mcp.Tool{
		Name:        "mirror_screen",
		Description: "Mirror the current Claude Code screen/output to Telegram, editing a single message in place so it acts like a live clone. Send-only.",
	}, s.handleMirror)
}

// --- notify ------------------------------------------------------------------

// NotifyInput is the argument schema for notify.
type NotifyInput struct {
	Message string `json:"message" jsonschema:"The progress update or completion message to send to Telegram"`
	Level   string `json:"level,omitempty" jsonschema:"Severity: info (default), success, warning, or error"`
}

// NotifyOutput is the result of notify.
type NotifyOutput struct {
	Sent      bool   `json:"sent"`
	MessageID int    `json:"message_id"`
	Mode      string `json:"mode"`
}

func (s *Server) handleNotify(ctx context.Context, _ *mcp.CallToolRequest, in NotifyInput) (*mcp.CallToolResult, NotifyOutput, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, NotifyOutput{}, fmt.Errorf("message must not be empty")
	}
	text := fmt.Sprintf("%s %s\n\n%s", levelIcon(in.Level), s.header(), html.EscapeString(in.Message))
	id, err := s.tg.SendMessage(ctx, text, "HTML", nil)
	if err != nil {
		return nil, NotifyOutput{}, fmt.Errorf("send to telegram: %w", err)
	}
	return nil, NotifyOutput{Sent: true, MessageID: id, Mode: s.mode()}, nil
}

// --- ask_approval ------------------------------------------------------------

// AskApprovalInput is the argument schema for ask_approval.
type AskApprovalInput struct {
	Question       string   `json:"question" jsonschema:"The question or action that requires the user's approval"`
	Options        []string `json:"options,omitempty" jsonschema:"Custom button labels to offer, in order. Defaults to [Approve, Deny]"`
	Columns        int      `json:"columns,omitempty" jsonschema:"How many buttons per row in the inline keyboard. Default 2; use 1 for long labels"`
	AllowFreetext  bool     `json:"allow_freetext,omitempty" jsonschema:"Add a button that lets the user reply with free text (talk back) instead of choosing an option"`
	FreetextLabel  string   `json:"freetext_label,omitempty" jsonschema:"Label for the free-text button (setting it implies allow_freetext). Default is a pencil 'reply with text' label"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"Seconds to wait for a response before timing out. Default 300"`
}

// AskApprovalOutput is the result of ask_approval.
type AskApprovalOutput struct {
	Choice   string `json:"choice"`          // label of the tapped option (empty if the user replied with text)
	Approved bool   `json:"approved"`        // true if the chosen option looks like an approval
	Freetext bool   `json:"freetext"`        // true if the user chose to reply with free text
	Reply    string `json:"reply,omitempty"` // the free-text reply, when Freetext is true
	TimedOut bool   `json:"timed_out"`
}

// freetextValue is the callback_data marker for the "reply with text" button.
const freetextValue = "ft"

func (s *Server) handleAskApproval(ctx context.Context, _ *mcp.CallToolRequest, in AskApprovalInput) (*mcp.CallToolResult, AskApprovalOutput, error) {
	if !s.interactive() {
		return nil, AskApprovalOutput{}, fmt.Errorf("%s", notBookedMsg)
	}
	if strings.TrimSpace(in.Question) == "" {
		return nil, AskApprovalOutput{}, fmt.Errorf("question must not be empty")
	}

	options := in.Options
	if len(options) == 0 {
		options = []string{"Approve", "Deny"}
	}

	columns := in.Columns
	if columns <= 0 {
		columns = 2
	}
	reqID := s.nextID("ask")
	var rows [][]telegram.InlineKeyboardButton
	for i := 0; i < len(options); i += columns {
		end := i + columns
		if end > len(options) {
			end = len(options)
		}
		row := make([]telegram.InlineKeyboardButton, 0, end-i)
		for j := i; j < end; j++ {
			row = append(row, telegram.InlineKeyboardButton{
				Text:         options[j],
				CallbackData: fmt.Sprintf("%s:%d", reqID, j),
			})
		}
		rows = append(rows, row)
	}
	// Optional "reply with text" (talk back) button on its own row.
	allowFreetext := in.AllowFreetext || strings.TrimSpace(in.FreetextLabel) != ""
	if allowFreetext {
		label := strings.TrimSpace(in.FreetextLabel)
		if label == "" {
			label = "✍️ Reply with text…"
		}
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("%s:%s", reqID, freetextValue),
		}})
	}
	markup := &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}

	ch := s.disp.RegisterCallback(reqID)
	defer s.disp.UnregisterCallback(reqID)

	base := fmt.Sprintf("❓ %s\n\n%s", s.header(), html.EscapeString(in.Question))
	msgID, err := s.tg.SendMessage(ctx, base, "HTML", markup)
	if err != nil {
		return nil, AskApprovalOutput{}, fmt.Errorf("send approval request: %w", err)
	}

	// A single deadline governs both the tap and any follow-up free-text reply.
	deadline := time.Now().Add(timeout(in.TimeoutSeconds))

	select {
	case <-ctx.Done():
		return nil, AskApprovalOutput{}, ctx.Err()

	case <-time.After(time.Until(deadline)):
		_ = s.tg.EditMessageText(context.Background(), msgID, base+"\n\n⏱️ <i>timed out</i>", "HTML", nil)
		return nil, AskApprovalOutput{TimedOut: true}, nil

	case ev := <-ch:
		if ev.Value == freetextValue {
			return s.awaitFreetextReply(ctx, msgID, base, ev.CallbackID, deadline)
		}
		idx, _ := strconv.Atoi(ev.Value)
		choice := ""
		if idx >= 0 && idx < len(options) {
			choice = options[idx]
		}
		// Best-effort acknowledgement/edit; use a fresh context since the
		// tool's own context may end as soon as we return.
		_ = s.tg.AnswerCallbackQuery(context.Background(), ev.CallbackID, "✓ "+choice)
		_ = s.tg.EditMessageText(context.Background(), msgID, base+fmt.Sprintf("\n\n✅ <b>%s</b>", html.EscapeString(choice)), "HTML", nil)
		return nil, AskApprovalOutput{Choice: choice, Approved: isApprove(choice)}, nil
	}
}

// awaitFreetextReply handles the case where the user tapped the "reply with
// text" button: it prompts for and waits on the next text message until the
// shared deadline.
func (s *Server) awaitFreetextReply(ctx context.Context, msgID int, base, callbackID string, deadline time.Time) (*mcp.CallToolResult, AskApprovalOutput, error) {
	_ = s.tg.AnswerCallbackQuery(context.Background(), callbackID, "Type your reply in the chat…")
	_ = s.tg.EditMessageText(context.Background(), msgID, base+"\n\n✍️ <i>waiting for your text reply…</i>", "HTML", nil)

	replyCh := s.disp.RegisterReply()
	select {
	case <-ctx.Done():
		s.disp.UnregisterReply(replyCh)
		return nil, AskApprovalOutput{}, ctx.Err()

	case <-time.After(time.Until(deadline)):
		s.disp.UnregisterReply(replyCh)
		_ = s.tg.EditMessageText(context.Background(), msgID, base+"\n\n⏱️ <i>timed out</i>", "HTML", nil)
		return nil, AskApprovalOutput{Freetext: true, TimedOut: true}, nil

	case reply := <-replyCh:
		_ = s.tg.EditMessageText(context.Background(), msgID,
			base+fmt.Sprintf("\n\n✍️ <b>Reply:</b> %s", html.EscapeString(reply)), "HTML", nil)
		return nil, AskApprovalOutput{Freetext: true, Reply: reply}, nil
	}
}

// --- wait_for_reply ----------------------------------------------------------

// WaitForReplyInput is the argument schema for wait_for_reply.
type WaitForReplyInput struct {
	Prompt         string `json:"prompt" jsonschema:"The question to ask; cocote waits for the user's next text reply"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Seconds to wait for a reply before timing out. Default 300"`
}

// WaitForReplyOutput is the result of wait_for_reply.
type WaitForReplyOutput struct {
	Reply    string `json:"reply"`
	TimedOut bool   `json:"timed_out"`
}

func (s *Server) handleWaitForReply(ctx context.Context, _ *mcp.CallToolRequest, in WaitForReplyInput) (*mcp.CallToolResult, WaitForReplyOutput, error) {
	if !s.interactive() {
		return nil, WaitForReplyOutput{}, fmt.Errorf("%s", notBookedMsg)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, WaitForReplyOutput{}, fmt.Errorf("prompt must not be empty")
	}

	ch := s.disp.RegisterReply()
	text := fmt.Sprintf("💬 %s\n\n%s", s.header(), html.EscapeString(in.Prompt))
	if _, err := s.tg.SendMessage(ctx, text, "HTML", nil); err != nil {
		s.disp.UnregisterReply(ch)
		return nil, WaitForReplyOutput{}, fmt.Errorf("send prompt: %w", err)
	}

	select {
	case <-ctx.Done():
		s.disp.UnregisterReply(ch)
		return nil, WaitForReplyOutput{}, ctx.Err()
	case <-time.After(timeout(in.TimeoutSeconds)):
		s.disp.UnregisterReply(ch)
		return nil, WaitForReplyOutput{TimedOut: true}, nil
	case reply := <-ch:
		return nil, WaitForReplyOutput{Reply: reply}, nil
	}
}

// --- mirror_screen -----------------------------------------------------------

// MirrorInput is the argument schema for mirror_screen.
type MirrorInput struct {
	Content string `json:"content" jsonschema:"The current Claude Code screen/output content to mirror to Telegram"`
	Title   string `json:"title,omitempty" jsonschema:"Optional heading shown above the mirrored content"`
	New     bool   `json:"new,omitempty" jsonschema:"If true, start a fresh mirror message instead of editing the existing one"`
}

// MirrorOutput is the result of mirror_screen.
type MirrorOutput struct {
	MessageID int  `json:"message_id"`
	Edited    bool `json:"edited"`
}

const mirrorMaxContent = 3500 // keep the whole message comfortably under Telegram's 4096 limit

func (s *Server) handleMirror(ctx context.Context, _ *mcp.CallToolRequest, in MirrorInput) (*mcp.CallToolResult, MirrorOutput, error) {
	if strings.TrimSpace(in.Content) == "" {
		return nil, MirrorOutput{}, fmt.Errorf("content must not be empty")
	}
	title := in.Title
	if strings.TrimSpace(title) == "" {
		title = "Claude Code"
	}
	content := in.Content
	if len(content) > mirrorMaxContent {
		content = "…" + content[len(content)-mirrorMaxContent:] // keep the most recent tail
	}
	text := fmt.Sprintf("🖥️ <b>%s</b> · %s\n<pre>%s</pre>",
		html.EscapeString(title), html.EscapeString(s.cfg.SessionName), html.EscapeString(content))

	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	if s.mirrorMsg != 0 && !in.New {
		if err := s.tg.EditMessageText(ctx, s.mirrorMsg, text, "HTML", nil); err == nil {
			return nil, MirrorOutput{MessageID: s.mirrorMsg, Edited: true}, nil
		}
		// Editing failed (message deleted/too old); fall through to a new send.
	}
	id, err := s.tg.SendMessage(ctx, text, "HTML", nil)
	if err != nil {
		return nil, MirrorOutput{}, fmt.Errorf("mirror send: %w", err)
	}
	s.mirrorMsg = id
	return nil, MirrorOutput{MessageID: id, Edited: false}, nil
}

// --- helpers -----------------------------------------------------------------

// interactive reports whether tools that need to receive updates can work:
// this process must hold the booking and the poller must still be running.
func (s *Server) interactive() bool {
	return s.active.Load() && s.pollerAlive.Load() && s.lease.Held()
}

func (s *Server) mode() string {
	if s.interactive() {
		return "active"
	}
	return "send-only"
}

func (s *Server) header() string {
	return fmt.Sprintf("<b>cocote</b> · <code>%s</code>", html.EscapeString(s.cfg.SessionName))
}

func (s *Server) nextID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, s.reqCounter.Add(1))
}

func timeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 300 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func levelIcon(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "success", "done", "ok":
		return "✅"
	case "warning", "warn":
		return "⚠️"
	case "error", "fail", "failed":
		return "❌"
	default:
		return "ℹ️"
	}
}

func isApprove(choice string) bool {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "approve", "approved", "yes", "ok", "okay", "allow", "accept", "ya", "setuju", "izinkan":
		return true
	default:
		return false
	}
}
