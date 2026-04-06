package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	sseKeepaliveInterval = 15 * time.Second
	subscriberBufSize    = 256
)

// sseEvent is one Server-Sent Event to push to a client.
type sseEvent struct {
	Event string // SSE event type (init, assistant, tool_use, tool_result, result, error, status)
	Data  []byte // JSON payload
}

// subscriber is a single SSE client connection.
type subscriber struct {
	ch     chan sseEvent
	closed chan struct{}
}

// sseBroadcaster fans out events to all subscribers (SSE + WebSocket).
type sseBroadcaster struct {
	mu            sync.RWMutex
	subscribers   map[*subscriber]struct{}
	history       []sseEvent // ring buffer of recent events for late joiners
	historyMax    int
	hub           *wsHub // also push to WebSocket hub (nil = SSE only)
	sessionID     string // server session ID for WS routing
	lastEventTime time.Time // last event broadcast time (for activity detection)
}

func newBroadcaster() *sseBroadcaster {
	return &sseBroadcaster{
		subscribers: make(map[*subscriber]struct{}),
		historyMax:  100,
	}
}

// subscribe creates a new subscriber. Caller must call unsubscribe when done.
func (b *sseBroadcaster) subscribe() *subscriber {
	sub := &subscriber{
		ch:     make(chan sseEvent, subscriberBufSize),
		closed: make(chan struct{}),
	}

	b.mu.Lock()
	// Send history to new subscriber (catch-up)
	for _, ev := range b.history {
		select {
		case sub.ch <- ev:
		default:
			// buffer full, skip old history
		}
	}
	b.subscribers[sub] = struct{}{}
	b.mu.Unlock()

	return sub
}

// unsubscribe removes a subscriber and closes its channel.
func (b *sseBroadcaster) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[sub]; ok {
		delete(b.subscribers, sub)
		close(sub.closed)
	}
}

// broadcast sends an event to all subscribers. Slow consumers are dropped.
func (b *sseBroadcaster) broadcast(ev sseEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Append to history ring buffer
	b.history = append(b.history, ev)
	if len(b.history) > b.historyMax {
		b.history = b.history[len(b.history)-b.historyMax:]
	}
	b.lastEventTime = time.Now()

	for sub := range b.subscribers {
		select {
		case sub.ch <- ev:
		default:
			// Slow consumer: drop and disconnect
			delete(b.subscribers, sub)
			close(sub.closed)
		}
	}

	// Also push to WebSocket hub for bidirectional sync
	if b.hub != nil && b.sessionID != "" {
		b.hub.broadcastSessionEvent(b.sessionID, ev.Event, ev.Data)
	}
}

// count returns the number of active subscribers.
func (b *sseBroadcaster) count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// lastEventAt returns the time of the last broadcast event.
func (b *sseBroadcaster) lastEventAt() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.lastEventTime.IsZero() {
		return ""
	}
	return b.lastEventTime.Format(time.RFC3339)
}

// idleSeconds returns seconds since the last broadcast event.
// Returns -1 if no events have been broadcast yet.
func (b *sseBroadcaster) idleSeconds() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.lastEventTime.IsZero() {
		return -1
	}
	return int(time.Since(b.lastEventTime).Seconds())
}

// ── SSE HTTP handler ──

// serveSSE writes Server-Sent Events to the HTTP response.
// Blocks until client disconnects or session ends.
func serveSSE(w http.ResponseWriter, r *http.Request, broadcaster *sseBroadcaster) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx passthrough
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := broadcaster.subscribe()
	defer broadcaster.unsubscribe(sub)

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.closed:
			// Dropped by broadcaster (slow consumer) or session ended
			return
		case ev := <-sub.ch:
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, ev.Data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// ── stdout → SSE bridge ──

// bridgeStdout reads Claude Code stdout and broadcasts as SSE events.
// Also updates session status based on message types.
// Blocks until the process stdout is closed.
func bridgeStdout(proc *claudeProcess, broadcaster *sseBroadcaster, onInit func(json.RawMessage), onResult func(json.RawMessage)) {
	proc.readLines(func(msgType string, raw json.RawMessage) {
		event := mapEventType(msgType, raw)
		broadcaster.broadcast(sseEvent{Event: event, Data: raw})

		if msgType == "system" && onInit != nil {
			var peek struct {
				Subtype string `json:"subtype"`
			}
			if json.Unmarshal(raw, &peek) == nil && peek.Subtype == "init" {
				onInit(raw)
			}
		}
		if msgType == "result" && onResult != nil {
			onResult(raw)
		}
	})

	// Process exited — notify subscribers
	broadcaster.broadcast(sseEvent{
		Event: "close",
		Data:  []byte(`{"reason":"process_exited"}`),
	})
}

// mapEventType maps stream-json type to SSE event name.
// For assistant messages, peek at content to distinguish tool_use vs text.
func mapEventType(msgType string, raw json.RawMessage) string {
	switch msgType {
	case "system":
		var peek struct {
			Subtype string `json:"subtype"`
		}
		json.Unmarshal(raw, &peek)
		if peek.Subtype == "init" {
			return "init"
		}
		return "system"
	case "assistant":
		// Check if this contains tool_use
		var peek struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &peek) == nil {
			for _, c := range peek.Message.Content {
				if c.Type == "tool_use" {
					return "tool_use"
				}
			}
		}
		return "assistant"
	case "user":
		return "tool_result"
	case "result":
		return "result"
	default:
		return msgType
	}
}
