package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin:      func(r *http.Request) bool { return true },
	ReadBufferSize:   4096,
	WriteBufferSize:  16384,
	HandshakeTimeout: 10 * time.Second,
}

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = (wsPongWait * 9) / 10
	wsMaxMsgSize = 64 * 1024
)

// ── WebSocket Hub ──

// wsHub manages all WebSocket client connections and broadcasts events.
type wsHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	sm      *sessionManager
	rl      *rateLimiter
	token   string
}

func newWSHub(token string, rl *rateLimiter) *wsHub {
	return &wsHub{
		clients: make(map[*wsClient]struct{}),
		rl:      rl,
		token:   token,
	}
}

// wsClient is a single WebSocket connection.
type wsClient struct {
	hub    *wsHub
	conn   *websocket.Conn
	send   chan []byte
	subSID string // subscribed session ID for detailed events
	mu     sync.Mutex
}

func (h *wsHub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

func (h *wsHub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// broadcastAll sends raw JSON to every connected client.
func (h *wsHub) broadcastAll(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// slow consumer — will be cleaned up by writePump
		}
	}
}

// broadcastSessionEvent sends a session stream event to clients subscribed to that session.
func (h *wsHub) broadcastSessionEvent(sessionID, event string, data json.RawMessage) {
	msg, _ := json.Marshal(map[string]any{
		"type":       "event",
		"session_id": sessionID,
		"event":      event,
		"data":       data,
	})
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.mu.Lock()
		sub := c.subSID
		c.mu.Unlock()
		if sub == sessionID {
			select {
			case c.send <- msg:
			default:
			}
		}
	}
}

// notifySessions broadcasts updated session list to all connected clients.
func (h *wsHub) notifySessions() {
	if h.sm == nil {
		return
	}
	sessions := h.sm.listSessions()
	msg, _ := json.Marshal(map[string]any{
		"type":     "sessions",
		"sessions": sessions,
	})
	h.broadcastAll(msg)
}

// ── HTTP Handler ──

func (h *wsHub) serveWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token != h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] ws: upgrade failed: %v\n", appName, err)
		return
	}

	client := &wsClient{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 256),
	}

	h.register(client)
	n := h.clientCount()
	fmt.Fprintf(os.Stderr, "[%s] ws: client connected (%d total)\n", appName, n)

	// Send initial session list
	if h.sm != nil {
		sessions := h.sm.listSessions()
		initMsg, _ := json.Marshal(map[string]any{
			"type":     "sessions",
			"sessions": sessions,
		})
		select {
		case client.send <- initMsg:
		default:
		}
	}

	go client.writePump()
	go client.readPump()
}

// ── Read / Write Pumps ──

func (c *wsClient) readPump() {
	defer func() {
		c.hub.unregister(c)
		c.conn.Close()
		n := c.hub.clientCount()
		fmt.Fprintf(os.Stderr, "[%s] ws: client disconnected (%d remain)\n", appName, n)
	}()

	c.conn.SetReadLimit(wsMaxMsgSize)
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				fmt.Fprintf(os.Stderr, "[%s] ws: read error: %v\n", appName, err)
			}
			return
		}
		c.handleMessage(message)
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ── Message Handler ──

func (c *wsClient) handleMessage(raw []byte) {
	var msg struct {
		Type       string `json:"type"`
		SID        string `json:"sid"`
		Message    string `json:"message"`
		Name       string `json:"name"`
		Project    string `json:"project"`
		Model      string `json:"model"`
		SoulFiles  *bool  `json:"soul_files"`
		InitMsg    string `json:"initial_message"`
		SkipReplay bool   `json:"skip_replay"`
		GalID      string `json:"gal_id"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		c.sendJSON(map[string]string{"type": "error", "error": "invalid JSON"})
		return
	}

	switch msg.Type {
	case "ping":
		c.sendJSON(map[string]string{"type": "pong"})

	case "subscribe":
		c.mu.Lock()
		c.subSID = msg.SID
		c.mu.Unlock()
		// Replay history events for catch-up (skip if client already loaded via HTTP)
		if !msg.SkipReplay {
			if sess := c.hub.sm.getSession(msg.SID); sess != nil {
				sess.broadcaster.mu.RLock()
				for _, ev := range sess.broadcaster.history {
					evMsg, _ := json.Marshal(map[string]any{
						"type":       "event",
						"session_id": msg.SID,
						"event":      ev.Event,
						"data":       json.RawMessage(ev.Data),
					})
					select {
					case c.send <- evMsg:
					default:
					}
				}
				sess.broadcaster.mu.RUnlock()
			}
		}
		c.sendJSON(map[string]string{"type": "subscribed", "sid": msg.SID})

	case "unsubscribe":
		c.mu.Lock()
		c.subSID = ""
		c.mu.Unlock()

	case "send":
		if msg.SID == "" || msg.Message == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid and message required"})
			return
		}
		if !c.hub.rl.allow() {
			c.sendJSON(map[string]string{"type": "error", "error": "rate limit exceeded"})
			return
		}
		sess := c.hub.sm.getSession(msg.SID)
		if sess == nil {
			c.sendJSON(map[string]string{"type": "error", "error": "session not found"})
			return
		}
		if !sess.process.alive() {
			c.sendJSON(map[string]string{"type": "error", "error": "session process has exited"})
			return
		}
		sess.touch()
		sess.setStatus("running")
		// Broadcast user message so it persists in history for session switching
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": msg.Message},
		})
		sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
		if err := sess.process.sendMessage(msg.Message); err != nil {
			c.sendJSON(map[string]string{"type": "error", "error": "send failed: " + err.Error()})
			return
		}
		c.sendJSON(map[string]string{"type": "sent", "sid": msg.SID})
		c.hub.notifySessions()

	case "create":
		if !c.hub.rl.allow() {
			c.sendJSON(map[string]string{"type": "error", "error": "rate limit exceeded"})
			return
		}
		name := msg.Name
		if name == "" {
			name = fmt.Sprintf("session-%s", time.Now().Format("0102-1504"))
		}
		project := msg.Project
		if project == "" {
			project = workspace
		}
		soul := true
		if msg.SoulFiles != nil {
			soul = *msg.SoulFiles
		}
		sess, err := c.hub.sm.createSessionWithOpts(sessionCreateOpts{
			Name:     name,
			Project:  project,
			Model:    msg.Model,
			Soul:     soul,
			GalID:    msg.GalID,
			Category: CategoryInteractive,
		})
		if err != nil {
			c.sendJSON(map[string]string{"type": "error", "error": err.Error()})
			return
		}
		if msg.InitMsg != "" {
			time.Sleep(500 * time.Millisecond)
			userEvent, _ := json.Marshal(map[string]any{
				"type":    "user",
				"message": map[string]any{"role": "user", "content": msg.InitMsg},
			})
			sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
			sess.process.sendMessage(msg.InitMsg)
		}
		// Auto-subscribe client to the new session
		c.mu.Lock()
		c.subSID = sess.ID
		c.mu.Unlock()
		c.sendJSON(map[string]any{"type": "created", "session": sess.snapshot()})
		c.hub.notifySessions()

	case "rename":
		if msg.SID == "" || msg.Name == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid and name required"})
			return
		}
		sess := c.hub.sm.getSession(msg.SID)
		if sess == nil {
			c.sendJSON(map[string]string{"type": "error", "error": "session not found"})
			return
		}
		sess.mu.Lock()
		sess.Name = msg.Name
		sess.mu.Unlock()
		markRenamed(sess.ID, msg.Name)
		c.sendJSON(map[string]string{"type": "renamed", "sid": msg.SID, "name": msg.Name})
		c.hub.notifySessions()

	case "destroy":
		if msg.SID == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid required"})
			return
		}
		if err := c.hub.sm.destroySession(msg.SID); err != nil {
			c.sendJSON(map[string]string{"type": "error", "error": err.Error()})
			return
		}
		c.sendJSON(map[string]string{"type": "destroyed", "session_id": msg.SID})
		c.hub.notifySessions()

	case "resume":
		if msg.SID == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid required"})
			return
		}
		if !c.hub.rl.allow() {
			c.sendJSON(map[string]string{"type": "error", "error": "rate limit exceeded"})
			return
		}
		sess, err := c.hub.sm.resumeSession(msg.SID, msg.Message, msg.Name)
		if err != nil {
			c.sendJSON(map[string]string{"type": "error", "error": err.Error()})
			return
		}
		// Auto-subscribe client to the new session before sending resumed
		c.mu.Lock()
		c.subSID = sess.ID
		c.mu.Unlock()
		c.sendJSON(map[string]any{"type": "resumed", "session": sess.snapshot()})
		c.hub.notifySessions()

	default:
		c.sendJSON(map[string]string{"type": "error", "error": "unknown type: " + msg.Type})
	}
}

func (c *wsClient) sendJSON(v any) {
	data, _ := json.Marshal(v)
	select {
	case c.send <- data:
	default:
	}
}
