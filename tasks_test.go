package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatTask(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	task := heartbeatTask()
	if task == "" {
		t.Fatal("heartbeat task is empty")
	}
	if !strings.Contains(task, "heartbeat patrol") {
		t.Error("missing 'heartbeat patrol' in task text")
	}
	if !strings.Contains(task, "HEARTBEAT.md") {
		t.Error("missing HEARTBEAT.md reference")
	}
	if !strings.Contains(task, "report.txt") {
		t.Error("missing report file instruction")
	}
}

func TestCronTask(t *testing.T) {
	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	origSD := sessionDir
	sessionDir = dir
	defer func() {
		dbPath = origDB
		sessionDir = origSD
	}()

	task := cronTask()
	if task == "" {
		t.Fatal("cron task is empty")
	}
	if !strings.Contains(task, "Memory Consolidation") {
		t.Error("missing 'Memory Consolidation' in task text")
	}

	today := time.Now().Format("2006-01-02")
	if !strings.Contains(task, today) {
		t.Errorf("missing today's date %s in task text", today)
	}
}

func TestCronTask_ContainsWeeklyOnSunday(t *testing.T) {
	if time.Now().Weekday() != time.Sunday {
		t.Skip("not Sunday, skip weekly test")
	}

	origDB := dbPath
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	origSD := sessionDir
	sessionDir = dir
	defer func() {
		dbPath = origDB
		sessionDir = origSD
	}()

	task := cronTask()
	if !strings.Contains(task, "Deep Review") {
		t.Error("Sunday cron task should contain 'Deep Review'")
	}
}

func TestWeeklyPreScan(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	task := weeklyPreScan()
	if task == "" {
		t.Fatal("weekly pre-scan task is empty")
	}
	if !strings.Contains(task, "Pre-Scan") {
		t.Error("missing 'Pre-Scan' in task text")
	}
	if !strings.Contains(task, "weekly-scan.md") {
		t.Error("missing weekly-scan.md output instruction")
	}
}

func TestWeeklyPreScan_ListsFiles(t *testing.T) {
	origSD := sessionDir
	sessionDir = t.TempDir()
	defer func() { sessionDir = origSD }()

	// Create a today daily note for it to find
	today := time.Now().Format("2006-01-02")
	noteDir := filepath.Join(workspace, "memory")
	notePath := filepath.Join(noteDir, today+".md")

	// Check if the file actually exists (it should on Kiyor's machine)
	if _, err := os.Stat(notePath); err != nil {
		t.Skip("no today's daily note, skip file listing test")
	}

	task := weeklyPreScan()
	if !strings.Contains(task, today) {
		t.Errorf("should reference today's note %s", today)
	}
}
