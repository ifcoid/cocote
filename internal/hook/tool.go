package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ifcoid/cocote/internal/config"
	"github.com/ifcoid/cocote/internal/telegram"
)

const toolSummaryMax = 600

// toolPayload is the subset of the Claude Code PostToolUse hook stdin JSON we use.
type toolPayload struct {
	SessionID     string          `json:"session_id"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`
}

// RunToolMirror reads a PostToolUse hook payload from stdin and sends a concise
// per-tool activity message to Telegram. It is send-only (no booking needed)
// and always returns without error so it can never block the session.
func RunToolMirror(cfg *config.Config, stdin io.Reader) {
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "cocote tool: "+format+"\n", a...) }

	var p toolPayload
	if err := json.NewDecoder(stdin).Decode(&p); err != nil {
		logf("read hook payload: %v", err)
		return
	}
	if strings.TrimSpace(p.ToolName) == "" {
		return
	}

	summary := summarizeTool(p.ToolName, p.ToolInput)
	status := toolStatus(p.ToolResponse)

	body := summary
	if len(body) > toolSummaryMax {
		body = body[:toolSummaryMax] + "\n…"
	}
	text := fmt.Sprintf("🔧 <b>%s</b>%s · <code>%s</code>",
		html.EscapeString(p.ToolName), status, html.EscapeString(cfg.SessionName))
	if strings.TrimSpace(body) != "" {
		text += fmt.Sprintf("\n<pre>%s</pre>", html.EscapeString(body))
	}

	tg := telegram.NewClient(cfg.BotToken, cfg.ChatID)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := tg.SendMessage(ctx, text, "HTML", nil); err != nil {
		logf("send tool mirror: %v", err)
	}
}

// summarizeTool produces a short, human-readable description of a tool call by
// pulling the most relevant field(s) out of its input.
func summarizeTool(name string, raw json.RawMessage) string {
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
		return ""
	}

	switch name {
	case "Bash":
		if c := pick("command"); c != "" {
			return c
		}
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		if f := pick("file_path", "notebook_path"); f != "" {
			return f
		}
	case "Glob", "Grep":
		pat := pick("pattern")
		if g := pick("glob", "path"); g != "" && pat != "" {
			return pat + "  (" + g + ")"
		}
		return pat
	case "Task", "Agent":
		if d := pick("description"); d != "" {
			return d
		}
	}

	// Generic fallback: compact JSON of the input.
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// toolStatus returns a short error indicator if the tool response looks like a
// failure, otherwise an empty string.
func toolStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"is_error", "isError", "error"} {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case bool:
				if t {
					return " ❌"
				}
			case string:
				if strings.TrimSpace(t) != "" {
					return " ❌"
				}
			}
		}
	}
	return ""
}
