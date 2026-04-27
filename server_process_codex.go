package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ── Codex backend bridge → SSE / approvals (Round 4) ──
//
// This file is the codex counterpart to attachProcessBridge in
// server_session.go: it consumes the unified-event stream a *codexBackend
// emits and pumps it into the per-session sseBroadcaster the Web UI / IPC
// peers / Telegram relay all read from. It also routes codex's
// server-initiated approval requests through the existing tool-hook
// PreToolUse chain.
//
// SSE schema choices
// ------------------
//
// CC's bridge re-broadcasts the raw stream-json events (init / system /
// assistant / tool_use / tool_result / result) so the front-end already
// knows that schema. Codex events don't fit the same shape — they're a
// typed thread/turn/item stream, not a flat assistant-message dump — so
// we emit a small set of *new* event types that mirror the unified-event
// shape one-for-one:
//
//   "init"              — when the codex thread is ready (intentionally the
//                         same event name as CC so the Web UI's session-ready
//                         hint reuses the existing handler unchanged)
//   "codex_turn_started"
//   "codex_turn_completed"   (final status / error)
//   "codex_item_started"     (with kind: agent_message / tool_call / …)
//   "codex_item_delta"       (DeltaType discriminates text / output / …)
//   "codex_item_completed"
//   "codex_backend_error"
//   "ask_user_question"      (re-used from CC schema when a hook says "ask")
//   "result"                 (CC-shaped fallback for downstream consumers
//                             that already key off result for is_error /
//                             total_cost; emitted alongside codex_turn_completed)
//   "close"                  (re-used from CC schema on backend exit)
//
// Front-end work to render these natively is Round 5+; for Round 4 the
// tests + smoke confirm the events flow and the codex turn doesn't deadlock.
//
// Approval bridging
// -----------------
//
// codexBackend's handleApprovalRequest registers the request in
// pendingApprovals and emits UEvtApproval. We translate the unified
// approval into a ToolHookInput, run it through the in-process
// PreToolUse evaluator (evaluateToolHookForApproval), and call
// cb.sendPermissionDecision with the resulting allow/deny verdict. If
// the hook chain says "ask" we surface an ask_user_question SSE for the
// Web UI; the user's answer eventually flows back through the existing
// /answer-question handler, which calls sess.process.sendPermissionDecision
// — and that codepath is backend-agnostic because the Backend interface
// already has sendPermissionDecision.
//
// Default decision when no hook fires: allow. This matches CC's
// bypassPermissions behavior for tools that slipped past the harness.
// The 30s timeout in the codex backend's handleApprovalRequest is the
// safety net if the bridge dies between emit and decision.

// attachCodexBridge starts the goroutines that drain a *codexBackend's
// event stream, watch for process exit, and route approvals through the
// hook chain. Mirrors attachProcessBridge in server_session.go.
//
// source: "server-create" / "server-resume" for full init (record agent,
// sync session id), "" for reload paths.
// fullSync: true on create/resume so the session_id mapping lands in the
// DB; false on mode/model toggles which preserve the existing mapping.
func attachCodexBridge(cb *codexBackend, sess *serverSession, source string, fullSync bool) {
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		runCodexEventLoop(cb, sess, source, fullSync)
		// Backend exited: invalidate pending AUQ + cancel running tasks,
		// same as the CC bridge does after bridgeStdout returns.
		sess.dismissAllPendingAUQ("session_dead")
		if cancelled := sess.tasks.markAllRunningAsCancelled(); len(cancelled) > 0 {
			for _, t := range cancelled {
				if data, err := json.Marshal(t); err == nil {
					sess.broadcaster.broadcast(sseEvent{Event: "task_event", Data: data})
				}
			}
		}
		// Trailing close event so the front-end stops the spinner — unless
		// this exit was an intentional reload (chrome / mode / model swap).
		if !cb.suppressNextCloseFlag.Load() {
			sess.broadcaster.broadcast(sseEvent{
				Event: "close",
				Data:  []byte(`{"reason":"process_exited","backend":"codex"}`),
			})
		}
		// Clear bridgeDone if it still points at this generation's channel
		// so subsequent waitBridgeDone() doesn't return on a stale close.
		sess.mu.Lock()
		if sess.bridgeDone == doneCh {
			sess.bridgeDone = nil
		}
		sess.mu.Unlock()
	}()
	sess.mu.Lock()
	sess.bridgeDone = doneCh
	sess.mu.Unlock()
	go watchCodexExit(cb, sess)
}

// runCodexEventLoop is the synchronous body of the bridge goroutine. Reads
// events off cb.events() until the channel is closed (on shutdown) or the
// backend dies (cb.done fires).
func runCodexEventLoop(cb *codexBackend, sess *serverSession, source string, fullSync bool) {
	// Synthetic init: emit as soon as the backend reports its thread id so
	// downstream consumers know the session is live. Matches CC's init
	// emission timing — driven off the backend's first useful event.
	initEmitted := false

	for {
		select {
		case ev := <-cb.events():
			// eventsCh is never closed by the producer; backend exit is
			// signaled exclusively via cb.done. No `!ok` check needed.
			handleCodexUnifiedEvent(cb, sess, ev, &initEmitted, source, fullSync)
		case <-cb.done:
			// Drain any events the producer wrote before close so the front-end
			// sees the final turn_completed / error before close arrives. Done
			// non-blockingly: if no events queued we exit immediately.
			drainCodexEvents(cb, sess, &initEmitted, source, fullSync)
			return
		}
	}
}

// drainCodexEvents flushes whatever is left in cb.events() after backend
// exit. Bounded by the channel capacity (256 events) so it terminates.
func drainCodexEvents(cb *codexBackend, sess *serverSession, initEmitted *bool, source string, fullSync bool) {
	for {
		select {
		case ev := <-cb.events():
			// eventsCh is never closed; we drain whatever is buffered then
			// exit on the default branch when empty.
			handleCodexUnifiedEvent(cb, sess, ev, initEmitted, source, fullSync)
		default:
			return
		}
	}
}

// handleCodexUnifiedEvent converts one UnifiedEvent into the matching SSE
// emissions and (for approvals) drives the tool-hook chain.
func handleCodexUnifiedEvent(cb *codexBackend, sess *serverSession, ev UnifiedEvent, initEmitted *bool, source string, fullSync bool) {
	// First useful event triggers our synthetic init. Codex doesn't have a
	// dedicated "I'm ready, here are my tools" message after thread/start,
	// so we synthesize one when the first turn or first item lands. This
	// gives the Web UI's "session ready" hint a stable signal.
	if !*initEmitted && (ev.Kind == UEvtTurnStarted || ev.Kind == UEvtItemStarted) {
		*initEmitted = true
		emitCodexSyntheticInit(cb, sess, source, fullSync)
	}

	switch ev.Kind {
	case UEvtTurnStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_turn_started", Data: codexEventEnvelope(ev)})
		sess.touch()

	case UEvtTurnCompleted:
		// Emit both the typed codex event AND a CC-shaped "result" so any
		// downstream consumer that keys off result for is_error / status
		// keeps working. This is also what flips the session to "idle".
		sess.broadcaster.broadcast(sseEvent{Event: "codex_turn_completed", Data: codexEventEnvelope(ev)})
		sess.broadcaster.broadcast(sseEvent{Event: "result", Data: codexResultEnvelope(ev)})
		// Mirror CC: turn_completed transitions session back to idle.
		newStatus := "idle"
		var payload UnifiedTurnPayload
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload.Status == "error" {
			newStatus = "error"
		}
		sess.setStatus(newStatus)

	case UEvtItemStarted:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_item_started", Data: codexEventEnvelope(ev)})

	case UEvtItemDelta:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_item_delta", Data: codexEventEnvelope(ev)})

	case UEvtItemCompleted:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_item_completed", Data: codexEventEnvelope(ev)})

	case UEvtBackendError:
		sess.broadcaster.broadcast(sseEvent{Event: "codex_backend_error", Data: codexEventEnvelope(ev)})

	case UEvtApproval:
		go handleCodexApproval(cb, sess, ev)
	}
}

// emitCodexSyntheticInit broadcasts a CC-shaped init event so the front-end
// sees a familiar "init" payload. We embed the codex-specific fields
// (backend kind, thread id) under "codex" so future Web UI work can render
// a backend badge.
func emitCodexSyntheticInit(cb *codexBackend, sess *serverSession, source string, fullSync bool) {
	info := cb.info()
	msg := map[string]any{
		"type":      "system",
		"subtype":   "init",
		"cwd":       cb.cwd,
		"session_id": info.SessionID,
		"model":     info.Model,
		"tools":     []string{},
		"mcp_servers": []map[string]any{},
		"permissionMode": "bypassPermissions",
		"backend": map[string]any{
			"kind":  string(info.Kind),
			"model": info.Model,
		},
	}
	raw, _ := json.Marshal(msg)
	sess.broadcaster.broadcast(sseEvent{Event: "init", Data: raw})

	// On full-sync paths (server-create / server-resume) record the
	// session id mapping so resume / IPC peers can find the session by
	// codex-side id. Mirrors CC's setClaudeSessionID + recordSessionAgent
	// in makeOnInit.
	if fullSync && info.SessionID != "" {
		sess.mu.Lock()
		sess.ClaudeSID = info.SessionID
		hub := sess.hub
		sess.mu.Unlock()
		setClaudeSessionID(sess.ID, info.SessionID)
		recordSessionAgent(info.SessionID, "main", appName, source)
		if hub != nil {
			hub.notifySessions()
		}
	}
}

// codexEventEnvelope serializes one UnifiedEvent for SSE transport. We
// hand the whole envelope through (kind/turn/item/payload/raw) so the
// front-end has full fidelity for any future renderer.
func codexEventEnvelope(ev UnifiedEvent) []byte {
	raw, _ := json.Marshal(ev)
	return raw
}

// codexResultEnvelope produces a CC-shaped result message for the browser's
// existing result-event handler. Only the fields downstream consumers
// actually read (subtype, is_error, total_cost_usd) are populated; codex
// doesn't currently expose per-turn cost so it's left at 0 for now.
func codexResultEnvelope(ev UnifiedEvent) []byte {
	var payload UnifiedTurnPayload
	_ = json.Unmarshal(ev.Payload, &payload)
	subtype := "success"
	isErr := false
	if payload.Status == "error" {
		subtype = "error"
		isErr = true
	} else if payload.Status == "cancelled" {
		subtype = "cancelled"
	}
	out := map[string]any{
		"type":           "result",
		"subtype":        subtype,
		"is_error":       isErr,
		"result":         payload.Error,
		"session_id":     ev.TurnID,
		"total_cost_usd": payload.CostUSD,
		"backend":        "codex",
	}
	raw, _ := json.Marshal(out)
	return raw
}

// handleCodexApproval is invoked once per UEvtApproval. It synthesizes a
// PreToolUse hook payload, runs the in-process evaluator, and replies via
// cb.sendPermissionDecision. If the hook chain says "ask" we surface an
// ask_user_question SSE for the Web UI and let the user's answer arrive
// later through the existing /answer-question endpoint (which calls
// sess.process.sendPermissionDecision — backend-agnostic).
func handleCodexApproval(cb *codexBackend, sess *serverSession, ev UnifiedEvent) {
	var ua UnifiedApproval
	if err := json.Unmarshal(ev.Payload, &ua); err != nil {
		// Malformed payload — default-allow to avoid deadlocking codex.
		// The 30s timeout would catch this anyway but we don't want to
		// burn that budget on a programmer error.
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "allow",
			"message":  "weiran: malformed approval payload, allowing",
		})
		return
	}

	// Synthesize a PreToolUse hook input. session_id is the weiran session
	// id (not the codex thread id) so tool-hook rules that pivot on
	// session_id (e.g. mark_restart_initiator) keep working.
	hookIn := ToolHookInput{
		SessionID:     sess.ID,
		CWD:           sess.Project,
		ToolName:      ua.ToolName,
		ToolInput:     ua.Input,
		HookEventName: HookEventPreToolUse,
	}

	decision, reason, contexts := evaluateToolHookForApproval(hookIn)
	recordCodexApprovalDecision(decision)
	switch decision {
	case "deny":
		msg := reason
		if msg == "" {
			msg = "weiran tool-hook: denied by rule"
		}
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "deny",
			"message":  msg,
		})
		// Surface the deny reason on SSE so the operator sees why.
		emitCodexHookDecision(sess, ua, "deny", msg, contexts)

	case "ask":
		// Mirror CC's permission-prompt path: record a pendingAUQEntry so
		// /answer-question can reply later, broadcast ask_user_question.
		entry := &pendingAUQEntry{
			RequestID: ua.RequestID,
			ToolUseID: ua.ToolUseID,
			Input:     ua.Input,
			CreatedAt: time.Now(),
			Kind:      "permission",
			ToolName:  ua.ToolName,
		}
		sess.recordPendingAUQ(entry)
		synthetic, _ := json.Marshal(map[string]any{
			"request_id":  ua.RequestID,
			"tool_use_id": ua.ToolUseID,
			"kind":        "permission",
			"tool_name":   ua.ToolName,
			"input":       synthesizePermissionAUQInput(ua.ToolName, ua.Input),
			"backend":     "codex",
			"hook_reason": reason,
		})
		sess.broadcaster.broadcast(sseEvent{Event: "ask_user_question", Data: synthetic})
		// Don't send a decision here — it'll come from /answer-question.

	case "allow":
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "allow",
			"message":  reason,
		})
		emitCodexHookDecision(sess, ua, "allow", reason, contexts)

	default:
		// No matching rule — default-allow (matches CC's bypassPermissions).
		_ = cb.sendPermissionDecision(ua.RequestID, map[string]any{
			"behavior": "allow",
			"message":  "weiran: no matching tool-hook rule, default allow",
		})
	}
}

// emitCodexHookDecision broadcasts a small SSE event so the operator can
// see when the hook chain auto-decided an approval. Useful for Round 4
// observability; harmless to ignore on the Web UI side.
func emitCodexHookDecision(sess *serverSession, ua UnifiedApproval, decision, reason string, contexts []string) {
	if reason == "" && len(contexts) == 0 {
		return
	}
	out := map[string]any{
		"request_id":  ua.RequestID,
		"tool_name":   ua.ToolName,
		"tool_use_id": ua.ToolUseID,
		"decision":    decision,
		"reason":      reason,
		"contexts":    contexts,
	}
	raw, _ := json.Marshal(out)
	sess.broadcaster.broadcast(sseEvent{Event: "codex_hook_decision", Data: raw})
}

// watchCodexExit is the codex counterpart to watchExit in server_session.go.
// Blocks on cb.done, then transitions the session to stopped/error and (for
// ephemeral sessions) optionally retries with the next fallback model.
//
// Round 4 keeps fallback minimal — codex sessions don't currently chain
// into the CC-style fallbackModels list. If a codex backend exits with
// rate-limit, we mark the session error and let the operator retry. Round
// 5 can extend this to share the CC retry path if desired.
func watchCodexExit(cb *codexBackend, sess *serverSession) {
	<-cb.done
	sess.mu.Lock()
	alreadyStopped := sess.Status == "stopped"
	sess.mu.Unlock()
	if alreadyStopped {
		return
	}

	if errPtr := cb.initErr.Load(); errPtr != nil && *errPtr != "" {
		fmt.Fprintf(os.Stderr, "[%s] server: codex session %s init error: %s\n",
			appName, shortID(sess.ID), *errPtr)
		sess.setStatus("error")
		return
	}
	if exitCode := cb.exitCode.Load(); exitCode != 0 || cb.rateLimited.Load() {
		fmt.Fprintf(os.Stderr, "[%s] server: codex session %s exited code=%d rate_limited=%v\n",
			appName, shortID(sess.ID), exitCode, cb.rateLimited.Load())
		sess.setStatus("error")
		return
	}
	sess.setStatus("stopped")
}

