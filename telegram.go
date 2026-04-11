package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// ── Telegram Notify ──

// disableTelegram is set to true in tests to prevent real Telegram API calls.
var disableTelegram bool

var cachedTelegramToken string

func getTelegramToken() string {
	if cachedTelegramToken != "" {
		return cachedTelegramToken
	}
	data, err := os.ReadFile(filepath.Join(appHome, "openclaw.json"))
	if err != nil {
		return ""
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	// channels.telegram.botToken
	channels, _ := cfg["channels"].(map[string]interface{})
	tg, _ := channels["telegram"].(map[string]interface{})
	if token, ok := tg["botToken"].(string); ok && token != "" {
		cachedTelegramToken = token
		return token
	}
	// fallback: channels.telegram.accounts.main.botToken
	accounts, _ := tg["accounts"].(map[string]interface{})
	main, _ := accounts["main"].(map[string]interface{})
	token, _ := main["botToken"].(string)
	cachedTelegramToken = token
	return token
}

func sendTelegramPhoto(photoURL, caption string) {
	if err := trySendTelegramPhoto(photoURL, caption); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Println("photo sent")
}

func sendTelegram(text string) {
	if err := trySendTelegram(text); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"]%v\n", err)
		os.Exit(1)
	}
	fmt.Println("sent")
}

// trySendTelegram sends a text message, returns error instead of os.Exit (hook-safe)
// tries Markdown first, falls back to plain text on failure
func trySendTelegram(text string) error {
	if disableTelegram {
		return nil
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	// try Markdown first
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id":    {tgChatID},
		"text":       {text},
		"parse_mode": {"Markdown"},
	})
	if err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &result)

	if result.OK {
		return nil
	}

	// Markdown failed, fallback to plain text (drop parse_mode)
	resp, err = http.PostForm(apiURL, url.Values{
		"chat_id": {tgChatID},
		"text":    {text},
	})
	if err != nil {
		return fmt.Errorf("send failed (fallback): %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	json.Unmarshal(body, &result)
	if !result.OK {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return nil
}

// trySendTelegramPhoto sends a photo, returns error instead of os.Exit
// tries Markdown first, falls back to plain text on failure
func trySendTelegramPhoto(photoURL, caption string) error {
	if disableTelegram {
		return nil
	}
	token := getTelegramToken()
	if token == "" {
		return fmt.Errorf("Telegram bot token not found")
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", token)
	resp, err := http.PostForm(apiURL, url.Values{
		"chat_id":    {tgChatID},
		"photo":      {photoURL},
		"caption":    {caption},
		"parse_mode": {"Markdown"},
	})
	if err != nil {
		return fmt.Errorf("photo send failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.Unmarshal(body, &result)

	if result.OK {
		return nil
	}

	// Markdown failed, fallback to plain text
	resp, err = http.PostForm(apiURL, url.Values{
		"chat_id": {tgChatID},
		"photo":   {photoURL},
		"caption": {caption},
	})
	if err != nil {
		return fmt.Errorf("photo send failed (fallback): %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	json.Unmarshal(body, &result)
	if !result.OK {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}
	return nil
}
