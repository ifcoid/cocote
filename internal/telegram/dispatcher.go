package telegram

import (
	"strings"
	"sync"
)

// CallbackEvent is delivered to a waiter when its inline button is tapped.
type CallbackEvent struct {
	CallbackID string // Telegram callback_query id, used to acknowledge the tap
	Value      string // the portion of callback_data after "<reqID>:"
	MessageID  int    // id of the message the button belonged to
}

// Dispatcher routes incoming Telegram updates to the tool call that is waiting
// for them: button taps go to the matching approval request, and free-text
// messages are handed to reply waiters in FIFO order.
type Dispatcher struct {
	chatID int64

	mu              sync.Mutex
	callbackWaiters map[string]chan CallbackEvent
	replyWaiters    []chan string
}

// NewDispatcher creates a Dispatcher scoped to a single chat.
func NewDispatcher(chatID int64) *Dispatcher {
	return &Dispatcher{
		chatID:          chatID,
		callbackWaiters: make(map[string]chan CallbackEvent),
	}
}

// RegisterCallback registers interest in button taps for a request id. The
// returned channel is buffered and receives at most one event.
func (d *Dispatcher) RegisterCallback(reqID string) chan CallbackEvent {
	ch := make(chan CallbackEvent, 1)
	d.mu.Lock()
	d.callbackWaiters[reqID] = ch
	d.mu.Unlock()
	return ch
}

// UnregisterCallback removes a previously registered callback waiter.
func (d *Dispatcher) UnregisterCallback(reqID string) {
	d.mu.Lock()
	delete(d.callbackWaiters, reqID)
	d.mu.Unlock()
}

// RegisterReply enqueues a waiter for the next free-text reply from the chat.
func (d *Dispatcher) RegisterReply() chan string {
	ch := make(chan string, 1)
	d.mu.Lock()
	d.replyWaiters = append(d.replyWaiters, ch)
	d.mu.Unlock()
	return ch
}

// UnregisterReply removes a reply waiter that is no longer interested (e.g.
// after a timeout).
func (d *Dispatcher) UnregisterReply(ch chan string) {
	d.mu.Lock()
	for i, w := range d.replyWaiters {
		if w == ch {
			d.replyWaiters = append(d.replyWaiters[:i], d.replyWaiters[i+1:]...)
			break
		}
	}
	d.mu.Unlock()
}

// Dispatch routes one update. Updates from other chats are ignored.
func (d *Dispatcher) Dispatch(u Update) {
	switch {
	case u.CallbackQuery != nil:
		cq := u.CallbackQuery
		if cq.Message != nil && cq.Message.Chat.ID != d.chatID {
			return
		}
		reqID, value := splitData(cq.Data)
		d.mu.Lock()
		ch := d.callbackWaiters[reqID]
		if ch != nil {
			delete(d.callbackWaiters, reqID)
		}
		d.mu.Unlock()
		if ch != nil {
			msgID := 0
			if cq.Message != nil {
				msgID = cq.Message.MessageID
			}
			ch <- CallbackEvent{CallbackID: cq.ID, Value: value, MessageID: msgID}
		}

	case u.Message != nil:
		m := u.Message
		if m.Chat.ID != d.chatID || strings.TrimSpace(m.Text) == "" {
			return
		}
		d.mu.Lock()
		var ch chan string
		if len(d.replyWaiters) > 0 {
			ch = d.replyWaiters[0]
			d.replyWaiters = d.replyWaiters[1:]
		}
		d.mu.Unlock()
		if ch != nil {
			ch <- m.Text
		}
	}
}

// splitData splits "reqID:value" callback data. If there is no colon the whole
// string is treated as the request id with an empty value.
func splitData(data string) (reqID, value string) {
	if i := strings.IndexByte(data, ':'); i >= 0 {
		return data[:i], data[i+1:]
	}
	return data, ""
}
