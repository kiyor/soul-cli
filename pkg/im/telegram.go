// Package im provides instant messaging protocol implementations.
// Currently supports Telegram Bot API with long-polling.
package im

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ── Telegram API types ──

type Update struct {
	UpdateID int      `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
	Text      string `json:"text"`
	Date      int    `json:"date"`

	// Voice/audio
	Voice *Voice `json:"voice,omitempty"`
	Audio *Audio `json:"audio,omitempty"`

	// Photo
	Photo []PhotoSize `json:"photo,omitempty"`

	// Caption (for photo/video/document messages)
	Caption string `json:"caption,omitempty"`

	// Sticker
	Sticker *Sticker `json:"sticker,omitempty"`

	// Reply
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private, group, supergroup, channel
}

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Voice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type Audio struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type PhotoSize struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type Sticker struct {
	FileID string `json:"file_id"`
	Emoji  string `json:"emoji"`
}

// ── Telegram Bot ──

const (
	PollTimeout    = 30 // long-poll seconds
	MaxMessageLen  = 4096
)

// Handler is called for each incoming message. Return value is ignored.
type Handler func(chatID string, msg *Message)

// Bot manages the Telegram long-polling loop.
type Bot struct {
	Token   string
	Allowed map[string]bool // allowed chat IDs (empty = allow all)
	Handler Handler

	// Offset persistence hooks (optional). If set, the bot persists poll offset
	// across restarts so no updates are missed or double-processed.
	LoadOffset func() int
	SaveOffset func(int)

	stop   chan struct{}
	done   chan struct{}
	logger func(format string, args ...any)
}

// NewBot creates a Telegram bot. Logger is optional (nil = stderr).
func NewBot(token string, allowedIDs []string, handler Handler, logger func(string, ...any)) *Bot {
	allowed := make(map[string]bool, len(allowedIDs))
	for _, id := range allowedIDs {
		allowed[id] = true
	}
	return &Bot{
		Token:   token,
		Allowed: allowed,
		Handler: handler,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		logger:  logger,
	}
}

func (b *Bot) log(format string, args ...any) {
	if b.logger != nil {
		b.logger(format, args...)
	} else {
		fmt.Fprintf(nil, format+"\n", args...)
	}
}

// Start begins the long-polling loop in a goroutine.
func (b *Bot) Start() {
	go b.pollLoop()
}

// Stop gracefully shuts down the polling loop.
func (b *Bot) Stop() {
	close(b.stop)
	<-b.done
}

// AllowedList returns the list of allowed chat IDs.
func (b *Bot) AllowedList() []string {
	out := make([]string, 0, len(b.Allowed))
	for id := range b.Allowed {
		out = append(out, id)
	}
	return out
}

func (b *Bot) pollLoop() {
	defer close(b.done)

	offset := 0
	if b.LoadOffset != nil {
		offset = b.LoadOffset()
	}
	client := &http.Client{Timeout: time.Duration(PollTimeout+5) * time.Second}

	for {
		select {
		case <-b.stop:
			return
		default:
		}

		updates, err := b.getUpdates(client, offset)
		if err != nil {
			b.log("poll error: %v", err)
			select {
			case <-b.stop:
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.dispatch(u)
		}

		// Persist offset after processing batch
		if len(updates) > 0 && b.SaveOffset != nil {
			b.SaveOffset(offset)
		}
	}
}

func (b *Bot) getUpdates(client *http.Client, offset int) ([]Update, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d&allowed_updates=[\"message\"]",
		b.Token, offset, PollTimeout)

	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
		Desc   string   `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if !result.OK {
		return nil, fmt.Errorf("API: %s", result.Desc)
	}
	return result.Result, nil
}

func (b *Bot) dispatch(u Update) {
	msg := u.Message
	if msg == nil {
		return
	}

	chatID := strconv.FormatInt(msg.Chat.ID, 10)

	// Auth check (empty allowed = allow all)
	if len(b.Allowed) > 0 && !b.Allowed[chatID] {
		b.log("rejected message from chat %s", chatID)
		return
	}

	if b.Handler != nil {
		b.Handler(chatID, msg)
	}
}

// ── Send / Edit helpers ──

// SendResult holds the result of a send operation.
type SendResult struct {
	OK        bool
	MessageID int
}

// SendMessage sends a text message. Tries Markdown, falls back to plain.
// Returns the message_id on success.
func SendMessage(token, chatID, text string) SendResult {
	if len(text) > MaxMessageLen {
		text = text[:MaxMessageLen-20] + "\n\n...(truncated)"
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	for _, parseMode := range []string{"Markdown", ""} {
		params := url.Values{
			"chat_id": {chatID},
			"text":    {text},
		}
		if parseMode != "" {
			params.Set("parse_mode", parseMode)
		}

		resp, err := http.PostForm(apiURL, params)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			OK     bool `json:"ok"`
			Result struct {
				MessageID int `json:"message_id"`
			} `json:"result"`
		}
		json.Unmarshal(body, &result)
		if result.OK {
			return SendResult{OK: true, MessageID: result.Result.MessageID}
		}
		if parseMode != "" {
			continue // retry without markdown
		}
	}
	return SendResult{}
}

// EditMessage edits an existing message text. Tries Markdown, falls back to plain.
func EditMessage(token, chatID string, messageID int, text string) bool {
	if len(text) > MaxMessageLen {
		text = text[:MaxMessageLen-20] + "\n\n...(truncated)"
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", token)

	for _, parseMode := range []string{"Markdown", ""} {
		params := url.Values{
			"chat_id":    {chatID},
			"message_id": {strconv.Itoa(messageID)},
			"text":       {text},
		}
		if parseMode != "" {
			params.Set("parse_mode", parseMode)
		}

		resp, err := http.PostForm(apiURL, params)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			OK bool `json:"ok"`
		}
		json.Unmarshal(body, &result)
		if result.OK {
			return true
		}
		if parseMode != "" {
			continue
		}
	}
	return false
}

// SendChatAction sends a "typing" indicator.
func SendChatAction(token, chatID, action string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", token)
	http.PostForm(apiURL, url.Values{
		"chat_id": {chatID},
		"action":  {action},
	})
}

// SendPhoto sends a photo by URL with optional caption.
func SendPhoto(token, chatID, photoURL, caption string) SendResult {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", token)

	for _, parseMode := range []string{"Markdown", ""} {
		params := url.Values{
			"chat_id": {chatID},
			"photo":   {photoURL},
		}
		if caption != "" {
			params.Set("caption", caption)
		}
		if parseMode != "" {
			params.Set("parse_mode", parseMode)
		}

		resp, err := http.PostForm(apiURL, params)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result struct {
			OK     bool `json:"ok"`
			Result struct {
				MessageID int `json:"message_id"`
			} `json:"result"`
		}
		json.Unmarshal(body, &result)
		if result.OK {
			return SendResult{OK: true, MessageID: result.Result.MessageID}
		}
		if parseMode != "" {
			continue
		}
	}
	return SendResult{}
}

// GetFileURL returns a direct download URL for a Telegram file_id.
// Returns "" on failure.
func GetFileURL(token, fileID string) string {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", token, url.QueryEscape(fileID))
	resp, err := http.Get(apiURL)
	if err != nil {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &result) != nil || !result.OK || result.Result.FilePath == "" {
		return ""
	}
	return fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", token, result.Result.FilePath)
}

// DownloadFile downloads a URL to a local path. Returns error on failure.
func DownloadFile(url, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// DeleteMessage deletes a message. Returns true on success.
func DeleteMessage(token, chatID string, messageID int) bool {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", token)
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id":    {chatID},
		"message_id": {strconv.Itoa(messageID)},
	})
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK bool `json:"ok"`
	}
	json.Unmarshal(body, &result)
	return result.OK
}

// ParseCommand extracts command and args from a message text.
// Returns ("", "") if not a command.
func ParseCommand(text string) (cmd, args string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	parts := strings.SplitN(text, " ", 2)
	cmd = strings.Split(parts[0], "@")[0] // strip @botname
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	return cmd, args
}
