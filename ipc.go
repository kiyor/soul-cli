package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
)

// ── IPC CLI: weiran session list/send/read/search ──

// handleSessionIPC dispatches `weiran session <subcommand>` CLI commands.
func handleSessionIPC(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s session <list|send|read|search|close|wait>\n", appName)
		os.Exit(1)
	}

	switch args[0] {
	case "list":
		ipcList()
	case "send":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: %s session send <target_id> \"message\"\n", appName)
			os.Exit(1)
		}
		msg := strings.Join(args[2:], " ")
		ipcSend(args[1], msg)
	case "read":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s session read <target_id>\n", appName)
			os.Exit(1)
		}
		ipcRead(args[1])
	case "search":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: %s session search <target_id> \"keyword\"\n", appName)
			os.Exit(1)
		}
		keyword := strings.Join(args[2:], " ")
		ipcSearch(args[1], keyword)
	case "close":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s session close <target_id>\n", appName)
			os.Exit(1)
		}
		ipcClose(args[1])
	case "wait":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: %s session wait <target_id>\n", appName)
			os.Exit(1)
		}
		ipcWait(args[1])
	case "prepare-restart":
		// Optional: custom message as args[1:]
		msg := ""
		if len(args) > 1 {
			msg = strings.Join(args[1:], " ")
		}
		ipcPrepareRestart(msg)
	default:
		fmt.Fprintf(os.Stderr, "unknown session subcommand: %s\n", args[0])
		fmt.Fprintf(os.Stderr, "usage: %s session <list|send|read|search|close|wait|prepare-restart>\n", appName)
		os.Exit(1)
	}
}

// ipcEnvPrefix returns the uppercase env var prefix derived from appName.
// e.g. "weiran" → "WEIRAN", "my-soul" → "MY_SOUL"
func ipcEnvPrefix() string {
	return strings.ToUpper(strings.ReplaceAll(appName, "-", "_"))
}

// ipcEnv reads IPC env vars and exits if missing.
func ipcEnv() (serverURL, token, sessionID string) {
	prefix := ipcEnvPrefix()
	serverURL = os.Getenv(prefix + "_SERVER_URL")
	token = os.Getenv(prefix + "_AUTH_TOKEN")
	sessionID = os.Getenv(prefix + "_SESSION_ID")

	if serverURL == "" || token == "" {
		fmt.Fprintf(os.Stderr, "error: %s_SERVER_URL and %s_AUTH_TOKEN must be set\n", prefix, prefix)
		fmt.Fprintf(os.Stderr, "  (these are automatically injected when running inside a server session)\n")
		os.Exit(1)
	}
	return
}

// ipcRequest makes an authenticated HTTP request to the server.
func ipcRequest(method, path string, body io.Reader) (*http.Response, error) {
	serverURL, token, _ := ipcEnv()
	u := strings.TrimRight(serverURL, "/") + path

	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// ipcList lists all active sessions.
func ipcList() {
	resp, err := ipcRequest("GET", "/api/sessions", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var sessions []map[string]any
	json.NewDecoder(resp.Body).Decode(&sessions)

	_, _, myID := ipcEnv()

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATUS\tMODEL\tCATEGORY")
	for _, s := range sessions {
		id := fmt.Sprintf("%v", s["id"])
		marker := ""
		if id == myID {
			marker = " (me)"
		}
		fmt.Fprintf(w, "%s\t%v%s\t%v\t%v\t%v\n",
			shortID(id), s["name"], marker, s["status"], s["model"], s["category"])
	}
	w.Flush()
}

// ipcSend sends a message from this session to a target session.
func ipcSend(targetID, message string) {
	_, _, myID := ipcEnv()
	if myID == "" {
		fmt.Fprintf(os.Stderr, "error: WEIRAN_SESSION_ID not set (required for send)\n")
		os.Exit(1)
	}

	// Resolve short ID to full ID
	targetID = resolveSessionID(targetID)

	body, _ := json.Marshal(map[string]string{
		"from_session_id": myID,
		"message":         message,
	})
	resp, err := ipcRequest("POST", "/api/sessions/"+targetID+"/message-from", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode == http.StatusTooManyRequests {
		fmt.Fprintf(os.Stderr, "interaction limit reached (round %v/%v)\n",
			result["round"], result["max_rounds"])
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v\n", result["error"])
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "sent (round %v/%v)\n", result["round"], result["max_rounds"])
}

// ipcRead reads full message history of a target session.
func ipcRead(targetID string) {
	targetID = resolveSessionID(targetID)

	resp, err := ipcRequest("GET", "/api/history/"+targetID+"/messages?limit=100", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var messages []map[string]any
	json.NewDecoder(resp.Body).Decode(&messages)

	if len(messages) == 0 {
		fmt.Println("(no messages)")
		return
	}

	for _, m := range messages {
		role := fmt.Sprintf("%v", m["role"])
		content := fmt.Sprintf("%v", m["content"])
		switch role {
		case "user":
			fmt.Printf("🧑 %s\n\n", content)
		case "assistant":
			fmt.Printf("🤖 %s\n\n", content)
		default:
			fmt.Printf("[%s] %s\n\n", role, content)
		}
	}
}

// ipcSearch searches a target session's history using FTS.
func ipcSearch(targetID, keyword string) {
	targetID = resolveSessionID(targetID)

	// Use the server's search API with content scope
	q := url.Values{}
	q.Set("q", keyword)
	q.Set("scope", "content")
	q.Set("limit", "20")
	resp, err := ipcRequest("GET", "/api/search?"+q.Encode(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	hits, ok := result["hits"].([]any)
	if !ok || len(hits) == 0 {
		fmt.Println("(no matches)")
		return
	}

	// Filter hits for the target session
	matched := 0
	for _, h := range hits {
		hit, ok := h.(map[string]any)
		if !ok {
			continue
		}
		// Check if this hit is from the target session
		source := fmt.Sprintf("%v", hit["source"])
		if !strings.Contains(source, shortID(targetID)) && !strings.Contains(source, targetID) {
			continue
		}
		matched++
		snippet := fmt.Sprintf("%v", hit["snippet"])
		fmt.Printf("--- match %d (score: %v) ---\n%s\n\n", matched, hit["score"], snippet)
	}

	if matched == 0 {
		fmt.Println("(no matches in target session)")
	}
}

// resolveSessionID resolves a short ID prefix to a full session ID by listing active sessions.
func resolveSessionID(input string) string {
	// If it looks like a full UUID, use as-is
	if len(input) > 16 {
		return input
	}

	resp, err := ipcRequest("GET", "/api/sessions", nil)
	if err != nil {
		return input // fallback to raw input
	}
	defer resp.Body.Close()

	var sessions []map[string]any
	json.NewDecoder(resp.Body).Decode(&sessions)

	for _, s := range sessions {
		id := fmt.Sprintf("%v", s["id"])
		if strings.HasPrefix(id, input) {
			return id
		}
	}

	// Also check history for non-active sessions
	resp2, err := ipcRequest("GET", "/api/history?limit=50", nil)
	if err != nil {
		return input
	}
	defer resp2.Body.Close()

	var history []map[string]any
	json.NewDecoder(resp2.Body).Decode(&history)

	for _, s := range history {
		id := fmt.Sprintf("%v", s["id"])
		if strings.HasPrefix(id, input) {
			return id
		}
	}

	return input // couldn't resolve, return as-is
}

// ipcClose destroys a target session via DELETE /api/sessions/{id}.
func ipcClose(targetID string) {
	targetID = resolveSessionID(targetID)

	// Safety: don't close yourself
	_, _, myID := ipcEnv()
	if targetID == myID {
		fmt.Fprintf(os.Stderr, "error: cannot close your own session\n")
		os.Exit(1)
	}

	resp, err := ipcRequest("DELETE", "/api/sessions/"+targetID, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v (status %d)\n", result["error"], resp.StatusCode)
		os.Exit(1)
	}

	fmt.Printf("session %s closed\n", shortID(targetID))
}

// ipcPrepareRestart marks the current session for rehydration after server restart.
// The session will be resumed and receive a wake message after the server comes back.
func ipcPrepareRestart(message string) {
	_, _, myID := ipcEnv()
	if myID == "" {
		fmt.Fprintf(os.Stderr, "error: %s_SESSION_ID not set\n", ipcEnvPrefix())
		os.Exit(1)
	}

	if message == "" {
		message = "Server restarted successfully. Continue from where you left off."
	}

	body, _ := json.Marshal(map[string]string{
		"session_id": myID,
		"message":    message,
	})
	resp, err := ipcRequest("POST", "/api/server/prepare-restart", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to decode response (status %d): %v\n", resp.StatusCode, err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v (status %d)\n", result["error"], resp.StatusCode)
		os.Exit(1)
	}

	fmt.Printf("prepared for restart (session %s → claude %s)\n",
		shortID(myID), shortID(fmt.Sprintf("%v", result["claude_session_id"])))
}

// ipcWait blocks until the target session reaches idle/stopped/error state.
func ipcWait(targetID string) {
	targetID = resolveSessionID(targetID)

	fmt.Fprintf(os.Stderr, "waiting for session %s...\n", shortID(targetID))

	resp, err := ipcRequest("GET", "/api/sessions/"+targetID+"/wait", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to decode response (status %d): %v\n", resp.StatusCode, err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %v (status %d)\n", result["error"], resp.StatusCode)
		os.Exit(1)
	}

	status := fmt.Sprintf("%v", result["status"])
	turns := result["num_turns"]
	timedOut, _ := result["timeout"].(bool)

	if timedOut {
		fmt.Printf("timeout (status: %s, turns: %v)\n", status, turns)
	} else {
		fmt.Printf("done (status: %s, turns: %v)\n", status, turns)
	}
}
