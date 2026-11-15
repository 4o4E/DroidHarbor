package diagnostics

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileWriteFailureRemainsVisibleOnConsole(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "closed.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	writer := tee{file: file, console: &console}
	record := []byte("2026-11-14 14:00:00.000 ERROR [--------] device_cleanup_failed\n")
	if _, err := writer.Write(record); err == nil {
		t.Fatal("file write failure was hidden")
	}
	if !strings.Contains(console.String(), "log_file_write_failed") {
		t.Fatalf("console lost the file failure: %s", console.String())
	}
	if bytes.Count(console.Bytes(), record) != 1 {
		t.Fatalf("console lost or duplicated the runtime event: %s", console.String())
	}
}

func TestDailyLogsSeparateRecordsAcrossMidnightAndRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "continuous"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			configured := filepath.Join(dir, "platform.jsonl")
			legacy := []byte("{\"legacy\":true}\n")
			if err := os.WriteFile(configured, legacy, 0600); err != nil {
				t.Fatal(err)
			}
			clock := time.Date(2026, 11, 13, 23, 59, 59, 0, logZone)
			logger, closer, err := open(configured, slog.LevelInfo, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			closer.(*dailyWriter).Compress = false
			logger.Info("old_day")
			clock = clock.Add(2 * time.Second)
			if restart {
				if err := closer.Close(); err != nil {
					t.Fatal(err)
				}
				oldTime := clock.Add(-24 * time.Hour)
				if err := os.Chtimes(filepath.Join(dir, "platform.log"), oldTime, oldTime); err != nil {
					t.Fatal(err)
				}
				logger, closer, err = open(configured, slog.LevelInfo, func() time.Time { return clock })
				if err != nil {
					t.Fatal(err)
				}
				closer.(*dailyWriter).Compress = false
			}
			logger.Info("new_day", "requestId", "request-marker")
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
			current, err := os.ReadFile(filepath.Join(dir, "platform.log"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(current), "old_day") || !strings.Contains(string(current), "new_day | request-marker") {
				t.Fatalf("daily records mixed or diagnosis lost: %s", current)
			}
			archives, err := filepath.Glob(filepath.Join(dir, "platform-*.log"))
			if err != nil || len(archives) != 1 {
				t.Fatalf("previous day archive missing: %v, %v", archives, err)
			}
			previous, err := os.ReadFile(archives[0])
			if err != nil || !strings.Contains(string(previous), "old_day") || strings.Contains(string(previous), "new_day") {
				t.Fatalf("previous day records changed: %s, %v", previous, err)
			}
			preserved, err := os.ReadFile(configured)
			if err != nil || !bytes.Equal(preserved, legacy) {
				t.Fatalf("legacy JSON log was modified: %q, %v", preserved, err)
			}
		})
	}
}
