package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kiyor/soul-cli/pkg/im"
)

// ── Telegram ↔ Session Bridge ──

const (
	CategoryTelegram = "telegram" // TG sessions — persistent, not ephemeral

	tgEditDebounce    = 800 * time.Millisecond // debounce edits to avoid TG rate limit
	tgSummaryInterval = 20                     // generate summary every N turns
	tgSummaryDir      = "memory/telegram"      // relative to workspace
	tgMsgQueueSize    = 32                     // per-chat message queue capacity
)

// telegramBridge connects the im.Bot to the session manager.
type telegramBridge struct {
	bot   *im.Bot
	sm    *sessionManager
	hub   *wsHub
	token string // bot token (for sending from bridges)

	// Per-chat state: message queue + session mapping
	chats  map[string]*tgChat
	chatMu sync.Mutex

	// Per-session output bridge
	bridges  map[string]*tgOutputBridge
	bridgeMu sync.Mutex

	// Session creation lock — prevents two goroutines from creating
	// sessions for the same chatID simultaneously
	createMu sync.Mutex

	stop chan struct{}
}

// tgChat holds per-chat state including the message queue.
type tgChat struct {
	chatID    string
	sessionID string           // current server session ID
	queue     chan string       // serialized message queue
	busy      chan struct{}     // closed when Claude finishes a turn (signals queue consumer)
	stopOnce  sync.Once
	stop      chan struct{}
}

// tgOutputBridge forwards one session's output to a Telegram chat.
type tgOutputBridge struct {
	chatID    string
	token     string
	sessionID string
	tb        *telegramBridge // back-reference for summary generation

	// Current turn: accumulate text, send/edit progressively
	mu          sync.Mutex
	turnText    strings.Builder
	msgID       int // TG message_id of partial message (0 = not sent)
	editTimer   *time.Timer
	lastEditLen int

	// Turn tracking for summary generation
	turnCount int

	// Signal that Claude finished processing (for queue draining)
	onTurnDone func()
}

func newTelegramBridge(token string, allowedIDs []string, sm *sessionManager, hub *wsHub) *telegramBridge {
	tb := &telegramBridge{
		sm:      sm,
		hub:     hub,
		token:   token,
		chats:   make(map[string]*tgChat),
		bridges: make(map[string]*tgOutputBridge),
		stop:    make(chan struct{}),
	}

	tb.bot = im.NewBot(token, allowedIDs, tb.handleMessage,
		func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[%s] telegram: "+format+"\n", append([]any{appName}, args...)...)
		},
	)

	// Offset persistence hooks
	tb.bot.LoadOffset = loadTGOffset
	tb.bot.SaveOffset = saveTGOffset

	// Restore chat→session mappings from DB
	tb.restoreChatMappings()

	return tb
}

func (tb *telegramBridge) start() {
	tb.bot.Start()
	fmt.Fprintf(os.Stderr, "[%s] telegram: bot started (allowed: %v)\n", appName, tb.bot.AllowedList())
}

func (tb *telegramBridge) shutdown() {
	close(tb.stop)

	// Trigger summary save for all active bridges before stopping
	tb.bridgeMu.Lock()
	for _, bridge := range tb.bridges {
		bridge.requestSummary()
	}
	tb.bridgeMu.Unlock()

	// Stop all chat queues
	tb.chatMu.Lock()
	for _, chat := range tb.chats {
		chat.stopOnce.Do(func() { close(chat.stop) })
	}
	tb.chatMu.Unlock()

	tb.bot.Stop()
	fmt.Fprintf(os.Stderr, "[%s] telegram: bot stopped\n", appName)
}

// ── Per-chat state ──

func (tb *telegramBridge) getOrCreateChat(chatID string) *tgChat {
	tb.chatMu.Lock()
	defer tb.chatMu.Unlock()

	if chat, ok := tb.chats[chatID]; ok {
		return chat
	}

	chat := &tgChat{
		chatID: chatID,
		queue:  make(chan string, tgMsgQueueSize),
		busy:   make(chan struct{}),
		stop:   make(chan struct{}),
	}
	// Initially not busy — close the channel to signal "ready"
	close(chat.busy)

	tb.chats[chatID] = chat

	// Start queue consumer goroutine
	go tb.consumeQueue(chat)

	return chat
}

// consumeQueue drains the per-chat message queue, sending one message at a time.
func (tb *telegramBridge) consumeQueue(chat *tgChat) {
	for {
		select {
		case <-chat.stop:
			return
		case <-tb.stop:
			return
		case text := <-chat.queue:
			tb.processMessage(chat, text)
		}
	}
}

// ── Incoming message handler ──

func (tb *telegramBridge) handleMessage(chatID string, msg *im.Message) {
	text := strings.TrimSpace(msg.Text)

	// Handle photo messages: download largest size, pass local path to Claude
	if len(msg.Photo) > 0 && text == "" {
		photo := msg.Photo[len(msg.Photo)-1] // largest size
		text = tb.handleIncomingPhoto(photo.FileID, msg.Caption)
		if text == "" {
			return
		}
	}

	if text == "" {
		return
	}

	// Commands bypass the queue (they're fast and don't involve Claude)
	if cmd, args := im.ParseCommand(text); cmd != "" {
		tb.handleCommand(chatID, cmd, args)
		return
	}

	// Enqueue message for serial processing
	chat := tb.getOrCreateChat(chatID)
	select {
	case chat.queue <- text:
		// queued
	default:
		im.SendMessage(tb.token, chatID, "Too many queued messages. Please wait.")
	}
}

// processMessage handles one message from the queue — ensures Claude is idle first.
func (tb *telegramBridge) processMessage(chat *tgChat, text string) {
	chatID := chat.chatID

	// Track whether this is the first message (need context prefix)
	isNewSession := false

	// Ensure we have a live session
	tb.chatMu.Lock()
	hadSession := chat.sessionID != ""
	tb.chatMu.Unlock()

	sid := tb.ensureSession(chatID)
	if sid == "" {
		return // error already sent to user
	}

	if !hadSession {
		isNewSession = true
	}

	sess := tb.sm.getSession(sid)
	if sess == nil {
		im.SendMessage(tb.token, chatID, "Session lost. Try /new")
		return
	}

	// Set up turn-done signal so queue waits for Claude to finish
	tb.bridgeMu.Lock()
	bridge := tb.bridges[sid]
	tb.bridgeMu.Unlock()

	busyCh := make(chan struct{})
	chat.busy = busyCh
	if bridge != nil {
		bridge.onTurnDone = func() {
			select {
			case <-busyCh:
			default:
				close(busyCh)
			}
		}
	}

	// Send typing indicator
	im.SendChatAction(tb.token, chatID, "typing")

	sess.touch()
	sess.setStatus("running")

	// For new sessions, prepend context to the first message so Claude
	// gets everything as one atomic input (no split responses).
	msgToSend := text
	if isNewSession {
		msgToSend = buildTGContext(chatID) + text
	}

	// Broadcast user message to Web UI SSE/WS (show original text, not with context prefix)
	userEvent, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})

	if err := sess.process.sendMessage(msgToSend); err != nil {
		im.SendMessage(tb.token, chatID, "Send failed: "+err.Error())
		// Unblock queue
		select {
		case <-busyCh:
		default:
			close(busyCh)
		}
		return
	}

	// Wait for Claude to finish this turn before processing next queued message
	select {
	case <-chat.busy:
		// turn complete
	case <-tb.stop:
		return
	case <-time.After(5 * time.Minute):
		// safety timeout — unblock queue
	}
}

// ensureSession returns a live session ID for the chatID, creating one if needed.
func (tb *telegramBridge) ensureSession(chatID string) string {
	tb.chatMu.Lock()
	chat := tb.chats[chatID]
	sid := ""
	if chat != nil {
		sid = chat.sessionID
	}
	tb.chatMu.Unlock()

	// Check if existing session is still alive
	if sid != "" {
		sess := tb.sm.getSession(sid)
		if sess != nil && sess.process.alive() {
			return sid
		}
		// Dead session — clean up
		tb.removeBridge(sid)
		tb.chatMu.Lock()
		if chat != nil {
			chat.sessionID = ""
		}
		tb.chatMu.Unlock()
		saveTGChatMapping(chatID, "")
		sid = ""
	}

	// Create new session with creation lock to prevent races
	tb.createMu.Lock()
	defer tb.createMu.Unlock()

	// Double-check after acquiring lock (another goroutine may have created it)
	tb.chatMu.Lock()
	if chat != nil && chat.sessionID != "" {
		sid = chat.sessionID
		tb.chatMu.Unlock()
		if sess := tb.sm.getSession(sid); sess != nil && sess.process.alive() {
			return sid
		}
	} else {
		tb.chatMu.Unlock()
	}

	sess, err := tb.createSession(chatID)
	if err != nil {
		im.SendMessage(tb.token, chatID, "Failed to create session: "+err.Error())
		return ""
	}

	// Wait for Claude Code init
	time.Sleep(800 * time.Millisecond)
	return sess.ID
}

// handleIncomingPhoto downloads a TG photo and returns a message string for Claude.
// Claude Code can read images via the Read tool, so we pass the local file path.
func (tb *telegramBridge) handleIncomingPhoto(fileID, caption string) string {
	fileURL := im.GetFileURL(tb.token, fileID)
	if fileURL == "" {
		fmt.Fprintf(os.Stderr, "[%s] telegram: failed to get file URL for %s\n", appName, fileID)
		return ""
	}

	// Download to temp file
	tmpFile := fmt.Sprintf("/tmp/tg-photo-%d.jpg", time.Now().UnixMilli())
	if err := im.DownloadFile(fileURL, tmpFile); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] telegram: failed to download photo: %v\n", appName, err)
		return ""
	}

	// Build message for Claude: instruct it to look at the image
	msg := fmt.Sprintf("[User sent a photo: %s]", tmpFile)
	if caption != "" {
		msg = fmt.Sprintf("%s\n%s", caption, msg)
	}
	return msg
}

func (tb *telegramBridge) handleCommand(chatID, cmd, args string) {
	switch cmd {
	case "/new":
		tb.chatMu.Lock()
		chat := tb.chats[chatID]
		oldSID := ""
		if chat != nil {
			oldSID = chat.sessionID
			chat.sessionID = ""
		}
		tb.chatMu.Unlock()

		if oldSID != "" {
			tb.bridgeMu.Lock()
			if bridge := tb.bridges[oldSID]; bridge != nil {
				bridge.requestSummary()
			}
			tb.bridgeMu.Unlock()
			time.Sleep(200 * time.Millisecond)
			tb.removeBridge(oldSID)
			tb.sm.destroySession(oldSID)
			saveTGChatMapping(chatID, "")
		}
		im.SendMessage(tb.token, chatID, "New session. Send a message to begin.")

	case "/status":
		tb.chatMu.Lock()
		sid := ""
		if chat := tb.chats[chatID]; chat != nil {
			sid = chat.sessionID
		}
		tb.chatMu.Unlock()

		if sid == "" {
			im.SendMessage(tb.token, chatID, "No active session. Send a message to start one.")
			return
		}
		sess := tb.sm.getSession(sid)
		if sess == nil {
			im.SendMessage(tb.token, chatID, "Session expired. Send a message to start a new one.")
			tb.chatMu.Lock()
			if chat := tb.chats[chatID]; chat != nil {
				chat.sessionID = ""
			}
			tb.chatMu.Unlock()
			saveTGChatMapping(chatID, "")
			return
		}
		sess.mu.Lock()
		status := fmt.Sprintf("Session: %s\nStatus: %s\nModel: %s\nTurns: %d\nCost: $%.4f",
			sess.Name, sess.Status, sess.Model, sess.NumTurns, sess.TotalCost)
		sess.mu.Unlock()
		im.SendMessage(tb.token, chatID, status)

	case "/sessions":
		all := tb.sm.listSessions()
		if len(all) == 0 {
			im.SendMessage(tb.token, chatID, "No active sessions.")
			return
		}
		var sb strings.Builder
		for _, s := range all {
			sb.WriteString(fmt.Sprintf("- %s [%s] %s\n", s["name"], s["status"], shortID(s["id"].(string))))
		}
		im.SendMessage(tb.token, chatID, sb.String())

	default:
		im.SendMessage(tb.token, chatID, "Commands: /new, /status, /sessions")
	}
}

// ── Session creation ──

func (tb *telegramBridge) createSession(chatID string) (*serverSession, error) {
	sessName := fmt.Sprintf("telegram-%s", time.Now().Format("0102-1504"))

	sess, err := tb.sm.createSessionWithOpts(sessionCreateOpts{
		Name:        sessName,
		Project:     workspace,
		Soul:        true,
		Category:    CategoryTelegram,
		Tags:        []string{"telegram", "chat:" + chatID},
		EnvOverride: telegramModeEnv,
	})
	if err != nil {
		return nil, err
	}

	// Update in-memory mapping + persist to DB
	tb.chatMu.Lock()
	chat := tb.chats[chatID]
	if chat == nil {
		chat = tb.getOrCreateChat(chatID)
	}
	chat.sessionID = sess.ID
	tb.chatMu.Unlock()
	saveTGChatMapping(chatID, sess.ID)

	// Start output bridge
	tb.startOutputBridge(chatID, sess)

	fmt.Fprintf(os.Stderr, "[%s] telegram: new session %s for chat %s\n",
		appName, shortID(sess.ID), chatID)
	return sess, nil
}

// buildTGContext builds the system-reminder prefix for a TG message.
// This is prepended to the FIRST user message in a new session so Claude
// gets context + user message as a single atomic input (no split responses).
func buildTGContext(chatID string) string {
	summary := loadTGSummary(chatID)
	if summary != "" {
		return fmt.Sprintf("<system-reminder>\nThis is a Telegram conversation with Kiyor. Previous conversation summary:\n\n%s\n\nContinue naturally from where we left off. Respond concisely (Telegram-friendly).\n</system-reminder>\n\n", summary)
	}
	return "<system-reminder>\nThis is a new Telegram conversation with Kiyor. Respond naturally, keep messages concise (Telegram-friendly). You have full access to your tools and memory.\n</system-reminder>\n\n"
}

// ── Output bridge (session → TG) ──

func (tb *telegramBridge) startOutputBridge(chatID string, sess *serverSession) {
	bridge := &tgOutputBridge{
		chatID:    chatID,
		token:     tb.token,
		sessionID: sess.ID,
		tb:        tb,
	}

	tb.bridgeMu.Lock()
	tb.bridges[sess.ID] = bridge
	tb.bridgeMu.Unlock()

	sub := sess.broadcaster.subscribe()

	go func() {
		defer sess.broadcaster.unsubscribe(sub)
		defer func() {
			bridge.flush()
			bridge.requestSummary()
			tb.bridgeMu.Lock()
			delete(tb.bridges, sess.ID)
			tb.bridgeMu.Unlock()
		}()

		for {
			select {
			case <-tb.stop:
				return
			case <-sub.closed:
				return
			case ev := <-sub.ch:
				bridge.handleEvent(ev)
			}
		}
	}()
}

func (tb *telegramBridge) removeBridge(sessionID string) {
	tb.bridgeMu.Lock()
	delete(tb.bridges, sessionID)
	tb.bridgeMu.Unlock()
}

// handleEvent processes SSE events and forwards assistant text to TG.
func (b *tgOutputBridge) handleEvent(ev sseEvent) {
	switch ev.Event {
	case "assistant":
		var msg AssistantMsg
		if json.Unmarshal(ev.Data, &msg) != nil {
			return
		}
		for _, block := range msg.Message.Content {
			if block.Type == "text" && block.Text != "" {
				b.appendText(block.Text)
			}
		}

	case "result":
		// Turn complete — flush and signal queue
		b.flush()
		b.turnCount++

		// Signal that Claude is done with this turn (unblocks queue consumer)
		if b.onTurnDone != nil {
			b.onTurnDone()
		}

		// Periodic summary
		if b.turnCount > 0 && b.turnCount%tgSummaryInterval == 0 {
			b.requestSummary()
		}

	case "close":
		b.flush()
		if b.onTurnDone != nil {
			b.onTurnDone()
		}
	}
}

// requestSummary asks the Claude session to generate a conversation summary.
func (b *tgOutputBridge) requestSummary() {
	if b.tb == nil {
		return
	}
	sess := b.tb.sm.getSession(b.sessionID)
	if sess == nil || !sess.process.alive() {
		b.saveSummaryFromHistory()
		return
	}

	summaryPrompt := `<system-reminder>
Generate a concise conversation summary for continuity across sessions. Include:
- Key topics discussed
- Decisions made or actions taken
- Any pending tasks or context the next session needs
- Emotional context if relevant

Format as plain text, 200-500 words max. Write ONLY the summary, no preamble.
Tag your response with [TG_SUMMARY] at the start so I can extract it.
</system-reminder>`

	sess.process.sendMessage(summaryPrompt)
}

// saveSummaryFromHistory extracts a basic summary from broadcaster history.
func (b *tgOutputBridge) saveSummaryFromHistory() {
	if b.tb == nil {
		return
	}
	sess := b.tb.sm.getSession(b.sessionID)
	if sess == nil {
		return
	}

	sess.broadcaster.mu.RLock()
	var snippets []string
	for _, ev := range sess.broadcaster.history {
		if ev.Event == "assistant" {
			var msg AssistantMsg
			if json.Unmarshal(ev.Data, &msg) == nil {
				for _, block := range msg.Message.Content {
					if block.Type == "text" && block.Text != "" {
						text := block.Text
						if len(text) > 300 {
							text = text[:300] + "..."
						}
						snippets = append(snippets, text)
					}
				}
			}
		}
	}
	sess.broadcaster.mu.RUnlock()

	if len(snippets) == 0 {
		return
	}

	summary := fmt.Sprintf("[Auto-generated summary from %d messages, %s]\n\n%s",
		len(snippets), time.Now().Format("2006-01-02 15:04 MST"),
		strings.Join(snippets, "\n---\n"))
	if len(summary) > 3000 {
		summary = summary[:3000] + "\n...(truncated)"
	}
	saveTGSummary(b.chatID, summary)
}

// appendText adds text and schedules a debounced send/edit.
func (b *tgOutputBridge) appendText(text string) {
	// Intercept [TG_SUMMARY] tagged responses
	if strings.Contains(text, "[TG_SUMMARY]") {
		summary := strings.Replace(text, "[TG_SUMMARY]", "", 1)
		summary = strings.TrimSpace(summary)
		if summary != "" {
			saveTGSummary(b.chatID, summary)
			fmt.Fprintf(os.Stderr, "[%s] telegram: saved summary for chat %s (%d chars)\n",
				appName, b.chatID, len(summary))
		}
		return // don't forward summary to TG
	}

	// Convert Web UI components to TG-friendly format
	text = convertWeiranComponents(text)

	b.mu.Lock()
	defer b.mu.Unlock()

	b.turnText.WriteString(text)

	if b.editTimer != nil {
		b.editTimer.Stop()
	}
	b.editTimer = time.AfterFunc(tgEditDebounce, func() {
		b.sendOrEdit()
	})
}

// flush finalizes the turn: sends/edits remaining text, handles images, resets state.
func (b *tgOutputBridge) flush() {
	b.mu.Lock()
	if b.editTimer != nil {
		b.editTimer.Stop()
		b.editTimer = nil
	}
	text := b.turnText.String()
	msgID := b.msgID
	b.mu.Unlock()

	text = strings.TrimSpace(text)
	if text == "" {
		b.resetTurn()
		return
	}

	// Split into text and image segments for final delivery.
	// During streaming (sendOrEdit), images show as raw markdown which is ugly
	// but harmless. At flush we replace the partial message with clean output.
	segments := splitImageSegments(text)

	if len(segments) == 1 && segments[0].kind == "text" {
		// Simple case: no images, just text — use normal send/edit
		b.sendOrEditText(segments[0].content, msgID)
		b.resetTurn()
		return
	}

	// Mixed content: handle the streaming placeholder, then send segments.
	// Delete the streaming placeholder first — it showed [caption] text that
	// we'll now replace with proper photo messages.
	if msgID != 0 {
		im.DeleteMessage(b.token, b.chatID, msgID)
	}

	for _, seg := range segments {
		content := strings.TrimSpace(seg.content)
		if content == "" {
			continue
		}
		switch seg.kind {
		case "text":
			im.SendMessage(b.token, b.chatID, content)
		case "image":
			im.SendPhoto(b.token, b.chatID, seg.url, seg.caption)
		}
	}

	b.resetTurn()
}

func (b *tgOutputBridge) resetTurn() {
	b.mu.Lock()
	b.turnText.Reset()
	b.msgID = 0
	b.lastEditLen = 0
	b.mu.Unlock()
}

// sendOrEdit is used during streaming (before flush) — sends/edits raw text progressively.
func (b *tgOutputBridge) sendOrEdit() {
	b.mu.Lock()
	text := b.turnText.String()
	msgID := b.msgID
	lastLen := b.lastEditLen
	b.mu.Unlock()

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if msgID != 0 && len(text) == lastLen {
		return
	}

	// During streaming, strip image markdown to avoid showing raw URLs.
	// Images will be sent properly at flush time.
	displayText := stripImageMarkdown(text)
	if strings.TrimSpace(displayText) == "" {
		return
	}

	b.sendOrEditText(displayText, msgID)
}

func (b *tgOutputBridge) sendOrEditText(text string, msgID int) {
	if msgID == 0 {
		result := im.SendMessage(b.token, b.chatID, text)
		b.mu.Lock()
		if result.OK {
			b.msgID = result.MessageID
		}
		b.lastEditLen = len(text)
		b.mu.Unlock()
	} else {
		im.EditMessage(b.token, b.chatID, msgID, text)
		b.mu.Lock()
		b.lastEditLen = len(text)
		b.mu.Unlock()
	}
}

// ── Image segment parsing ──

type tgSegment struct {
	kind    string // "text" or "image"
	content string // full text for "text", url for "image"
	url     string // image URL (only for "image")
	caption string // image caption (only for "image")
}

// imageMarkdownRe matches ![caption](url) patterns.
var imageMarkdownRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)

// splitImageSegments splits text into alternating text and image segments.
func splitImageSegments(text string) []tgSegment {
	matches := imageMarkdownRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return []tgSegment{{kind: "text", content: text}}
	}

	var segments []tgSegment
	lastEnd := 0

	for _, match := range matches {
		// match[0]:match[1] = full match
		// match[2]:match[3] = caption group
		// match[4]:match[5] = url group
		if match[0] > lastEnd {
			segments = append(segments, tgSegment{kind: "text", content: text[lastEnd:match[0]]})
		}

		caption := text[match[2]:match[3]]
		url := text[match[4]:match[5]]
		segments = append(segments, tgSegment{
			kind:    "image",
			content: text[match[0]:match[1]],
			url:     url,
			caption: caption,
		})
		lastEnd = match[1]
	}

	if lastEnd < len(text) {
		segments = append(segments, tgSegment{kind: "text", content: text[lastEnd:]})
	}

	return segments
}

// stripImageMarkdown removes ![caption](url) patterns from text for streaming display.
func stripImageMarkdown(text string) string {
	return imageMarkdownRe.ReplaceAllStringFunc(text, func(match string) string {
		sub := imageMarkdownRe.FindStringSubmatch(match)
		if len(sub) >= 2 && sub[1] != "" {
			return "[" + sub[1] + "]" // show caption as placeholder
		}
		return "[image]"
	})
}

// ── Component conversion (weiran-* → TG text) ──

// convertWeiranComponents converts Web UI component blocks to TG-friendly text.
func convertWeiranComponents(text string) string {
	if !strings.Contains(text, "```weiran-") {
		return text
	}

	result := text
	for _, blockType := range []string{"weiran-choices", "weiran-chips", "weiran-rating", "weiran-gallery"} {
		marker := "```" + blockType
		for {
			start := strings.Index(result, marker)
			if start < 0 {
				break
			}
			bodyStart := start + len(marker)
			end := strings.Index(result[bodyStart:], "```")
			if end < 0 {
				break
			}
			end += bodyStart + 3

			blockJSON := strings.TrimSpace(result[bodyStart : end-3])
			converted := convertBlock(blockType, blockJSON)
			result = result[:start] + converted + result[end:]
		}
	}
	return result
}

func convertBlock(blockType, jsonStr string) string {
	switch blockType {
	case "weiran-choices":
		return convertChoices(jsonStr)
	case "weiran-chips":
		return convertChips(jsonStr)
	case "weiran-rating":
		return convertRating(jsonStr)
	case "weiran-gallery":
		return convertGallery(jsonStr)
	default:
		return jsonStr
	}
}

func convertChoices(jsonStr string) string {
	var data struct {
		Type    string `json:"type"`
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Desc  string `json:"desc"`
		} `json:"options"`
	}
	if json.Unmarshal([]byte(jsonStr), &data) != nil {
		return jsonStr
	}
	var sb strings.Builder
	for i, opt := range data.Options {
		id := opt.ID
		if id == "" {
			id = string(rune('A' + i))
		}
		sb.WriteString(fmt.Sprintf("%s. %s", id, opt.Label))
		if opt.Desc != "" {
			sb.WriteString(fmt.Sprintf(" — %s", opt.Desc))
		}
		sb.WriteString("\n")
	}
	if data.Type == "multi" {
		sb.WriteString("\n(multiple choices, reply with e.g. A, C)")
	}
	return sb.String()
}

func convertChips(jsonStr string) string {
	var data struct {
		Options json.RawMessage `json:"options"`
	}
	if json.Unmarshal([]byte(jsonStr), &data) != nil {
		return jsonStr
	}
	var labels []string
	if json.Unmarshal(data.Options, &labels) == nil {
		return strings.Join(labels, " | ")
	}
	var opts []struct {
		Label string `json:"label"`
		Value string `json:"value"`
	}
	if json.Unmarshal(data.Options, &opts) == nil {
		strs := make([]string, len(opts))
		for i, o := range opts {
			strs[i] = o.Label
		}
		return strings.Join(strs, " | ")
	}
	return jsonStr
}

func convertRating(jsonStr string) string {
	var data struct {
		Label string `json:"label"`
		Max   int    `json:"max"`
	}
	if json.Unmarshal([]byte(jsonStr), &data) != nil {
		return jsonStr
	}
	max := data.Max
	if max == 0 {
		max = 5
	}
	return fmt.Sprintf("%s (reply 1-%d)", data.Label, max)
}

func convertGallery(jsonStr string) string {
	var data struct {
		Selectable bool `json:"selectable"`
		Images     []struct {
			URL     string `json:"url"`
			Caption string `json:"caption"`
			ID      string `json:"id"`
		} `json:"images"`
	}
	if json.Unmarshal([]byte(jsonStr), &data) != nil {
		return jsonStr
	}
	var sb strings.Builder
	for i, img := range data.Images {
		label := img.Caption
		if label == "" {
			label = img.ID
		}
		if label == "" {
			label = fmt.Sprintf("Image %d", i+1)
		}
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, label))
	}
	if data.Selectable {
		sb.WriteString("\n(reply with number to select)")
	}
	return sb.String()
}

// ── Summary persistence ──

func tgSummaryPath(chatID string) string {
	return filepath.Join(workspace, tgSummaryDir, chatID+".md")
}

func loadTGSummary(chatID string) string {
	data, err := os.ReadFile(tgSummaryPath(chatID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveTGSummary(chatID, summary string) {
	dir := filepath.Join(workspace, tgSummaryDir)
	os.MkdirAll(dir, 0755)

	content := fmt.Sprintf("---\nchat_id: %s\nupdated: %s\n---\n\n%s\n",
		chatID, time.Now().Format(time.RFC3339), summary)

	if err := os.WriteFile(tgSummaryPath(chatID), []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] telegram: failed to save summary for chat %s: %v\n", appName, chatID, err)
	}
}

// ── SQLite persistence (chat mappings + poll offset) ──

func initTelegramDB() {
	db, err := openServerDB()
	if err != nil {
		return
	}
	// Chat → session mapping
	db.Exec(`CREATE TABLE IF NOT EXISTS telegram_chats (
		chat_id    TEXT PRIMARY KEY,
		session_id TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	)`)
	// Poll offset
	db.Exec(`CREATE TABLE IF NOT EXISTS telegram_state (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
}

func saveTGChatMapping(chatID, sessionID string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	serverDBMu.Lock()
	defer serverDBMu.Unlock()
	now := time.Now().Format(time.RFC3339)
	db.Exec(`INSERT INTO telegram_chats (chat_id, session_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET session_id=?, updated_at=?`,
		chatID, sessionID, now, sessionID, now)
}

func loadTGChatMappings() map[string]string {
	initTelegramDB()
	db, err := openServerDB()
	if err != nil {
		return nil
	}
	rows, err := db.Query(`SELECT chat_id, session_id FROM telegram_chats WHERE session_id != ''`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var chatID, sessionID string
		if rows.Scan(&chatID, &sessionID) == nil {
			result[chatID] = sessionID
		}
	}
	return result
}

func (tb *telegramBridge) restoreChatMappings() {
	mappings := loadTGChatMappings()
	for chatID, sessionID := range mappings {
		// Only restore if the session is still alive
		sess := tb.sm.getSession(sessionID)
		if sess == nil || !sess.process.alive() {
			saveTGChatMapping(chatID, "") // clear stale mapping
			continue
		}
		chat := tb.getOrCreateChat(chatID)
		chat.sessionID = sessionID
		tb.startOutputBridge(chatID, sess)
		fmt.Fprintf(os.Stderr, "[%s] telegram: restored chat %s → session %s\n",
			appName, chatID, shortID(sessionID))
	}
}

func loadTGOffset() int {
	initTelegramDB()
	db, err := openServerDB()
	if err != nil {
		return 0
	}
	var val string
	err = db.QueryRow(`SELECT value FROM telegram_state WHERE key='poll_offset'`).Scan(&val)
	if err != nil {
		return 0
	}
	var offset int
	fmt.Sscanf(val, "%d", &offset)
	return offset
}

func saveTGOffset(offset int) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	serverDBMu.Lock()
	defer serverDBMu.Unlock()
	db.Exec(`INSERT INTO telegram_state (key, value) VALUES ('poll_offset', ?)
		ON CONFLICT(key) DO UPDATE SET value=?`,
		fmt.Sprintf("%d", offset), fmt.Sprintf("%d", offset))
}

// Ensure telegram_chats.session_id is cleared when a session is destroyed.
// Called from sessionManager.destroySession if the session was a telegram session.
func clearTGSessionMapping(sessionID string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	serverDBMu.Lock()
	defer serverDBMu.Unlock()

	var chatID string
	err = db.QueryRow(`SELECT chat_id FROM telegram_chats WHERE session_id=?`, sessionID).Scan(&chatID)
	if err == nil && chatID != "" {
		db.Exec(`UPDATE telegram_chats SET session_id='', updated_at=? WHERE chat_id=?`,
			time.Now().Format(time.RFC3339), chatID)
	}
}

// Ensure openServerDB creates telegram tables (called indirectly from server startup
// via restoreChatMappings → loadTGChatMappings → initTelegramDB).
