package main

import (
	"strings"
)

// ── Codex backend routing (Round 4) ──
//
// Helpers used by createSessionWithOpts and spawn paths to decide whether a
// requested session should run on the CC stream-json backend (default) or the
// codex JSON-RPC app-server backend. Both decisions are pure functions of the
// session opts + global config — no I/O, no side effects — so callers can
// compute them under their own locks safely.

// resolveBackendKind returns the BackendKind for a session given an explicit
// caller hint (from the API body / spawn flag) and the model name. Rules,
// in priority order:
//
//  1. explicit == BackendCodex             → codex (caller intent wins)
//  2. explicit == BackendCC                → cc (caller intent wins)
//  3. agents.codex.enabled is true AND
//       (a) model has "codex/" prefix, OR
//       (b) model is a key in agents.codex.model_map
//                                            → codex (auto-route by model)
//  4. otherwise                             → cc (default, current behavior)
//
// Round 4 deliberately treats absence of the codex binary as a config error
// surfaced at spawn time (spawnCodex returns an error, createSessionWithOpts
// propagates it). We don't downgrade-to-cc silently here because that would
// hide misconfiguration from users who explicitly asked for codex.
func resolveBackendKind(explicit BackendKind, model string) BackendKind {
	switch explicit {
	case BackendCodex:
		return BackendCodex
	case BackendCC:
		return BackendCC
	}
	if !codexEnabled {
		return BackendCC
	}
	if isCodexModel(model) {
		return BackendCodex
	}
	return BackendCC
}

// isCodexModel reports whether a model name should auto-route to the codex
// backend. The two heuristics:
//
//   - "codex/" prefix (e.g. "codex/gpt-5.1-codex-max") — same convention as
//     "zai/" / "minimax/" / etc. that providerModelName already understands
//   - presence in agents.codex.model_map — explicit allowlist of weiran model
//     names that map to codex. Lets the user keep using "opus[1m]" as the
//     identifier in the UI/config while routing the wire calls to codex.
//
// Both checks are case-sensitive — model names are conventionally lowercase
// in this codebase and matching upper/lower would mask typos.
func isCodexModel(model string) bool {
	if model == "" {
		return false
	}
	if strings.HasPrefix(model, "codex/") {
		return true
	}
	if codexModelMap != nil {
		if _, ok := codexModelMap[model]; ok {
			return true
		}
	}
	return false
}

// codexResolveModel returns the codex-side model name the backend should pass
// to thread/start. Resolution order:
//
//   1. agents.codex.model_map[model]        (explicit override)
//   2. strip "codex/" prefix if present     (codex/gpt-5.1-codex-max → gpt-5.1-codex-max)
//   3. fall through unchanged
//
// Empty input returns empty (codex falls back to its own default in that
// case).
func codexResolveModel(model string) string {
	if model == "" {
		return ""
	}
	if codexModelMap != nil {
		if mapped, ok := codexModelMap[model]; ok && mapped != "" {
			return mapped
		}
	}
	if strings.HasPrefix(model, "codex/") {
		return strings.TrimPrefix(model, "codex/")
	}
	return model
}
