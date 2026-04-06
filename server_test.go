package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthMiddleware_NoToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_QueryToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test?token=secret", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with query token, got %d", w.Code)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3)

	for i := 0; i < 3; i++ {
		if !rl.allow() {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}

	if rl.allow() {
		t.Fatal("4th request should be rate limited")
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"hello": "world"})

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}

	var result map[string]string
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["hello"] != "world" {
		t.Errorf("expected world, got %s", result["hello"])
	}
}

func TestDefaultServerConfig(t *testing.T) {
	cfg := defaultServerConfig()
	if cfg.Host != "0.0.0.0" {
		t.Errorf("expected 0.0.0.0, got %s", cfg.Host)
	}
	if cfg.Port != 9847 {
		t.Errorf("expected 9847, got %d", cfg.Port)
	}
	if cfg.MaxSessions != 5 {
		t.Errorf("expected 5, got %d", cfg.MaxSessions)
	}
}

// ── Broadcaster tests ──

func TestBroadcaster_SubscribeUnsubscribe(t *testing.T) {
	b := newBroadcaster()

	sub := b.subscribe()
	if b.count() != 1 {
		t.Errorf("expected 1 subscriber, got %d", b.count())
	}

	b.unsubscribe(sub)
	if b.count() != 0 {
		t.Errorf("expected 0 subscribers, got %d", b.count())
	}
}

func TestBroadcaster_Broadcast(t *testing.T) {
	b := newBroadcaster()
	sub := b.subscribe()
	defer b.unsubscribe(sub)

	ev := sseEvent{Event: "test", Data: []byte(`{"msg":"hello"}`)}
	b.broadcast(ev)

	select {
	case got := <-sub.ch:
		if got.Event != "test" {
			t.Errorf("expected event 'test', got '%s'", got.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broadcast")
	}
}

func TestBroadcaster_SlowConsumerDropped(t *testing.T) {
	b := newBroadcaster()
	sub := b.subscribe()

	// Fill the buffer
	for i := 0; i < subscriberBufSize+10; i++ {
		b.broadcast(sseEvent{Event: "flood", Data: []byte(fmt.Sprintf(`{"i":%d}`, i))})
	}

	// Slow consumer should be dropped
	select {
	case <-sub.closed:
		// expected
	case <-time.After(time.Second):
		t.Fatal("slow consumer was not dropped")
	}

	if b.count() != 0 {
		t.Errorf("expected 0 subscribers after drop, got %d", b.count())
	}
}

func TestBroadcaster_HistoryCatchup(t *testing.T) {
	b := newBroadcaster()

	// Broadcast before subscribing
	b.broadcast(sseEvent{Event: "early", Data: []byte(`{"msg":"early"}`)})

	sub := b.subscribe()
	defer b.unsubscribe(sub)

	select {
	case got := <-sub.ch:
		if got.Event != "early" {
			t.Errorf("expected 'early' event from history, got '%s'", got.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for history catchup")
	}
}

// ── MapEventType tests ──

func TestMapEventType(t *testing.T) {
	tests := []struct {
		msgType  string
		raw      string
		expected string
	}{
		{"system", `{"type":"system","subtype":"init"}`, "init"},
		{"system", `{"type":"system","subtype":"task_started"}`, "system"},
		{"assistant", `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`, "assistant"},
		{"assistant", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`, "tool_use"},
		{"user", `{"type":"user"}`, "tool_result"},
		{"result", `{"type":"result","subtype":"success"}`, "result"},
		{"unknown", `{"type":"unknown"}`, "unknown"},
	}

	for _, tt := range tests {
		got := mapEventType(tt.msgType, json.RawMessage(tt.raw))
		if got != tt.expected {
			t.Errorf("mapEventType(%q) = %q, want %q", tt.msgType, got, tt.expected)
		}
	}
}

// ── Session Manager tests (without Claude process) ──

func TestSessionManager_ReaperStops(t *testing.T) {
	sm := newSessionManager(5, time.Hour, time.Hour)
	sm.shutdownAll() // should not panic
}

// ── FilterEnv tests ──

func TestFilterEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=something",
		"CLAUDECODE_STUFF=other",
		"HOME=/root",
	}
	got := filterEnv(env, "CLAUDECODE")
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(got), got)
	}
}

// ── RandHex tests ──

func TestRandHex(t *testing.T) {
	h := randHex(8)
	if len(h) != 8 {
		t.Errorf("expected 8 chars, got %d", len(h))
	}
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("unexpected char: %c", c)
		}
	}
}

// ── Health endpoint integration test ──

func TestHealthEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"app":     appName,
			"version": buildVersion,
		})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("expected ok, got %v", result["status"])
	}
}
