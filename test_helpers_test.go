package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Prevent tests from sending real Telegram messages
	disableTelegram = true
	os.Exit(m.Run())
}
