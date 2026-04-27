package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ── Codex bridge tests (Round 4) ──
//
// These tests validate the UnifiedEvent → SSE translation in
// server_process_codex.go without spawning a real codex process. We build
// a *codexBackend by hand (no fake-server harness needed because the
// bridge consumes from cb.events() / cb.done — both controllable from
// test-side), wire it to a real *serverSession + sseBroadcaster, push
// events, and assert on the SSE stream.

// newCodexBridgeTestSession builds a minimal serverSession+codexBackend pair
// suitable for bridge tests. The codex backend has no underlying process —
// we drive its eventsCh directly.
func newCodexBridgeTestSession(t *testing.T) (*serverSession, *codexBackend, *subscriber) {
	t.Helper()
	bc := newBroadcaster()
	sess := &serverSession{
		ID:          "test-session",
		Project:     "/tmp",
		broadcaster: bc,
		Backend:     BackendCodex,
		tasks:       newTaskTracker(),
	}
	cb := newCodexBackend(SessionOpts{Model: "gpt-test"})
	// Pre-populate thread id so emitCodexSyntheticInit has a session_id to
	// emit. Also signal init so anything that calls waitInit returns true.
	thr := "thr_test"
	cb.threadID.Store(&thr)
	cb.signalInit()

	sub := bc.subscribe()
	t.Cleanup(func() {
		bc.unsubscribe(sub)
		cb.markDone()
	})
	return sess, cb, sub
}

// readSSEEvents drains up to maxEvents events from a subscriber within
// the given timeout. Returns the events seen so far on timeout.
func readSSEEvents(t *testing.T, sub *subscriber, maxEvents int, timeout time.Duration) []sseEvent {
	t.Helper()
	deadline := time.After(timeout)
	out := make([]sseEvent, 0, maxEvents)
	for len(out) < maxEvents {
		select {
		case ev := <-sub.ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestAttachCodexBridge_TurnFlowEmitsSSE(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)

	attachCodexBridge(cb, sess, "server-create", false)

	// Push: turn_started → item_started (agent_message) → item_delta → item_completed → turn_completed.
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "turn-1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemStarted,
		TurnID:   "turn-1",
		ItemID:   "item-1",
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	cb.emit(UnifiedEvent{
		Kind:      UEvtItemDelta,
		TurnID:    "turn-1",
		ItemID:    "item-1",
		ItemKind:  UItemAgentMessage,
		DeltaType: "text",
		Payload:   mustMarshalRaw(UnifiedDeltaPayload{Text: "Hi"}),
	})
	cb.emit(UnifiedEvent{
		Kind:     UEvtItemCompleted,
		TurnID:   "turn-1",
		ItemID:   "item-1",
		ItemKind: UItemAgentMessage,
		Payload:  mustMarshalRaw(UnifiedItemPayload{}),
	})
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "turn-1",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "ok"}),
	})

	// Expected SSE events (in order):
	//   init (synthetic, fires on first useful event)
	//   codex_turn_started
	//   codex_item_started
	//   codex_item_delta
	//   codex_item_completed
	//   codex_turn_completed
	//   result   (CC-shaped fallback alongside turn_completed)
	want := []string{
		"init",
		"codex_turn_started",
		"codex_item_started",
		"codex_item_delta",
		"codex_item_completed",
		"codex_turn_completed",
		"result",
	}
	got := readSSEEvents(t, sub, len(want), 2*time.Second)
	if len(got) < len(want) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(want), eventNames(got))
	}
	for i, w := range want {
		if got[i].Event != w {
			t.Errorf("event[%d] = %q, want %q (full sequence: %v)", i, got[i].Event, w, eventNames(got))
		}
	}

	// Verify the synthetic init carries backend=codex and session_id=thread id.
	var initPayload map[string]any
	if err := json.Unmarshal(got[0].Data, &initPayload); err != nil {
		t.Fatalf("decode init payload: %v", err)
	}
	if initPayload["session_id"] != "thr_test" {
		t.Errorf("init session_id = %v, want thr_test", initPayload["session_id"])
	}
	backendField, _ := initPayload["backend"].(map[string]any)
	if backendField["kind"] != string(BackendCodex) {
		t.Errorf("init backend.kind = %v, want codex", backendField["kind"])
	}

	// Verify the result envelope carries subtype=success on a clean turn.
	var resultPayload map[string]any
	if err := json.Unmarshal(got[6].Data, &resultPayload); err != nil {
		t.Fatalf("decode result payload: %v", err)
	}
	if resultPayload["subtype"] != "success" {
		t.Errorf("result subtype = %v, want success", resultPayload["subtype"])
	}
	if resultPayload["backend"] != "codex" {
		t.Errorf("result backend = %v, want codex", resultPayload["backend"])
	}

	// Session should be in idle status after turn_completed.
	sess.mu.Lock()
	gotStatus := sess.Status
	sess.mu.Unlock()
	if gotStatus != "idle" {
		t.Errorf("session status = %q, want idle", gotStatus)
	}
}

func TestAttachCodexBridge_TurnErrorFlipsStatusError(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "turn-err",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnCompleted,
		TurnID:  "turn-err",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "error", Error: "boom"}),
	})

	// init + codex_turn_started + codex_turn_completed + result
	got := readSSEEvents(t, sub, 4, 2*time.Second)
	if len(got) < 4 {
		t.Fatalf("got %d events, want 4: %v", len(got), eventNames(got))
	}

	// Result envelope must reflect the error.
	resultEv := got[3]
	if resultEv.Event != "result" {
		t.Fatalf("event[3] = %q, want result", resultEv.Event)
	}
	var resultPayload map[string]any
	_ = json.Unmarshal(resultEv.Data, &resultPayload)
	if resultPayload["is_error"] != true {
		t.Errorf("result is_error = %v, want true", resultPayload["is_error"])
	}
	if resultPayload["subtype"] != "error" {
		t.Errorf("result subtype = %v, want error", resultPayload["subtype"])
	}
	if resultPayload["result"] != "boom" {
		t.Errorf("result message = %v, want boom", resultPayload["result"])
	}

	sess.mu.Lock()
	gotStatus := sess.Status
	sess.mu.Unlock()
	if gotStatus != "error" {
		t.Errorf("session status = %q, want error", gotStatus)
	}
}

func TestAttachCodexBridge_BackendErrorEvent(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	// First push a turn_started so the synthetic init fires; otherwise the
	// backend_error alone wouldn't trip the "first useful event" gate.
	cb.emit(UnifiedEvent{
		Kind:    UEvtTurnStarted,
		TurnID:  "turn-z",
		Payload: mustMarshalRaw(UnifiedTurnPayload{Status: "running"}),
	})
	cb.emit(UnifiedEvent{
		Kind: UEvtBackendError,
		Payload: mustMarshalRaw(map[string]any{
			"reason": "transport closed",
		}),
	})

	got := readSSEEvents(t, sub, 3, 2*time.Second)
	if len(got) < 3 {
		t.Fatalf("got %d events, want 3: %v", len(got), eventNames(got))
	}
	if got[2].Event != "codex_backend_error" {
		t.Errorf("event[2] = %q, want codex_backend_error", got[2].Event)
	}
}

func TestAttachCodexBridge_CloseEventOnExit(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	// Mark backend done — bridge should drain remaining events and then
	// emit a close SSE event.
	cb.markDone()

	// Allow bridge to process exit + emit close.
	got := readSSEEvents(t, sub, 1, 2*time.Second)
	if len(got) == 0 {
		t.Fatal("no SSE event observed after backend exit")
	}
	// The first (and likely only) event should be close.
	foundClose := false
	for _, ev := range got {
		if ev.Event == "close" {
			foundClose = true
			if !strings.Contains(string(ev.Data), `"backend":"codex"`) {
				t.Errorf("close payload missing backend=codex: %s", ev.Data)
			}
		}
	}
	if !foundClose {
		t.Errorf("never saw close event; got: %v", eventNames(got))
	}
}

func TestAttachCodexBridge_SuppressNextCloseSkipsClose(t *testing.T) {
	sess, cb, sub := newCodexBridgeTestSession(t)
	cb.suppressNextClose()
	attachCodexBridge(cb, sess, "server-create", false)
	cb.markDone()

	// Wait briefly to let the bridge run and verify no close event arrives.
	got := readSSEEvents(t, sub, 1, 500*time.Millisecond)
	for _, ev := range got {
		if ev.Event == "close" {
			t.Errorf("close event fired despite suppressNextClose: %s", ev.Data)
		}
	}
}

func TestAttachCodexBridge_ApprovalDefaultAllowsWhenNoRule(t *testing.T) {
	// Override timeout to keep the test fast.
	saved := codexApprovalWaitTimeout
	codexApprovalWaitTimeout = 100 * time.Millisecond
	defer func() { codexApprovalWaitTimeout = saved }()

	sess, cb, _ := newCodexBridgeTestSession(t)
	attachCodexBridge(cb, sess, "server-create", false)

	// Manually register a pending approval (simulates handleApprovalRequest
	// without spinning a fake JSON-RPC server). We then emit UEvtApproval
	// and expect the bridge's handleCodexApproval goroutine to call
	// sendPermissionDecision(allow), which removes the entry.
	approvalID := "approval-test-1"
	pending := &pendingCodexApproval{
		method: ServerReqCommandExecApproval,
		reply:  make(chan map[string]any, 1),
	}
	cb.approvalsMu.Lock()
	cb.pendingApprovals[approvalID] = pending
	cb.approvalsMu.Unlock()

	cb.emit(UnifiedEvent{
		Kind: UEvtApproval,
		Payload: mustMarshalRaw(UnifiedApproval{
			RequestID: approvalID,
			ToolName:  "Bash",
			Input:     json.RawMessage(`{"command":"ls"}`),
			Subtype:   "permission",
		}),
	})

	// Wait for the bridge to wake and decide.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case decision := <-pending.reply:
			if decision["behavior"] != "allow" {
				t.Errorf("default decision behavior = %v, want allow", decision["behavior"])
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("approval bridge never resolved")
}

func eventNames(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Event
	}
	return out
}
