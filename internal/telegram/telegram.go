// Package telegram is a minimal Telegram Bot API client plus the update
// dispatcher and long-poller that cocote uses to receive taps and replies.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrConflict is returned when Telegram reports that another consumer is
// already polling getUpdates for this bot (HTTP 409).
var ErrConflict = errors.New("telegram getUpdates conflict: another consumer is already polling this bot")

// Client is a tiny Telegram Bot API client scoped to a single chat.
type Client struct {
	token  string
	chatID int64
	hc     *http.Client
}

// NewClient builds a client for the given bot token and target chat.
func NewClient(token string, chatID int64) *Client {
	return &Client{
		token:  token,
		chatID: chatID,
		hc:     &http.Client{Timeout: 75 * time.Second},
	}
}

// ChatID returns the configured target chat id.
func (c *Client) ChatID() int64 { return c.chatID }

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

func (c *Client) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("telegram %s: unexpected response (status %d): %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if !ar.OK {
		if ar.ErrorCode == http.StatusConflict {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("telegram %s failed (%d): %s", method, ar.ErrorCode, ar.Description)
	}
	return ar.Result, nil
}

// --- API types ---------------------------------------------------------------

// InlineKeyboardButton is a single tappable button.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

// InlineKeyboardMarkup is a grid of inline buttons attached to a message.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// Update is a single entry returned by getUpdates.
type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Message is an incoming or outgoing chat message (only the fields we use).
type Message struct {
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
}

// Chat identifies a Telegram conversation.
type Chat struct {
	ID int64 `json:"id"`
}

// User identifies a Telegram user.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// CallbackQuery is produced when a user taps an inline button.
type CallbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
	From    *User    `json:"from"`
}

// --- API methods -------------------------------------------------------------

// SendMessage posts a message to the configured chat and returns its id.
func (c *Client) SendMessage(ctx context.Context, text, parseMode string, markup *InlineKeyboardMarkup) (int, error) {
	p := map[string]any{
		"chat_id": c.chatID,
		"text":    text,
	}
	if parseMode != "" {
		p["parse_mode"] = parseMode
	}
	if markup != nil {
		p["reply_markup"] = markup
	}
	raw, err := c.call(ctx, "sendMessage", p)
	if err != nil {
		return 0, err
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// EditMessageText replaces the text (and optional markup) of an existing message.
func (c *Client) EditMessageText(ctx context.Context, messageID int, text, parseMode string, markup *InlineKeyboardMarkup) error {
	p := map[string]any{
		"chat_id":    c.chatID,
		"message_id": messageID,
		"text":       text,
	}
	if parseMode != "" {
		p["parse_mode"] = parseMode
	}
	if markup != nil {
		p["reply_markup"] = markup
	}
	_, err := c.call(ctx, "editMessageText", p)
	return err
}

// AnswerCallbackQuery acknowledges a button tap, optionally showing a toast.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	p := map[string]any{"callback_query_id": id}
	if text != "" {
		p["text"] = text
	}
	_, err := c.call(ctx, "answerCallbackQuery", p)
	return err
}

func (c *Client) getUpdates(ctx context.Context, offset, timeout int) ([]Update, error) {
	p := map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}
	raw, err := c.call(ctx, "getUpdates", p)
	if err != nil {
		return nil, err
	}
	var ups []Update
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

// Poll runs the long-polling loop, dispatching updates until ctx is cancelled
// or a non-recoverable error (such as ErrConflict) occurs.
func (c *Client) Poll(ctx context.Context, longPoll time.Duration, d *Dispatcher) error {
	offset := c.drainBacklog(ctx)
	timeout := int(longPoll.Seconds())
	if timeout <= 0 {
		timeout = 30
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ups, err := c.getUpdates(ctx, offset, timeout)
		if err != nil {
			if errors.Is(err, ErrConflict) || ctx.Err() != nil {
				return err
			}
			// Transient network error: back off briefly and retry.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			d.Dispatch(u)
		}
	}
}

// drainBacklog skips any messages that arrived before cocote started so the
// session doesn't react to stale taps. It returns the offset to start from.
func (c *Client) drainBacklog(ctx context.Context) int {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ups, err := c.getUpdates(dctx, -1, 0)
	if err != nil || len(ups) == 0 {
		return 0
	}
	return ups[len(ups)-1].UpdateID + 1
}
