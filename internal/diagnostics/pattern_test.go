package diagnostics

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPatternUsesUTC8AndDeviceBeforeMessageWithoutFieldLabels(t *testing.T) {
	var output bytes.Buffer
	handler := newPatternHandler(&output, slog.LevelInfo)
	record := slog.NewRecord(time.Date(2026, 11, 14, 6, 7, 8, 123000000, time.UTC), slog.LevelError, "device_failed", 0)
	record.AddAttrs(slog.String("deviceId", "fc99e836dca8702f"), slog.String("requestId", "complete-request-id"), slog.Any("cause", Cause(WrapStage("capture_start.encoder", New("codec configuration rejected")))))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	want := "2026-11-14 14:07:08.123 ERROR [fc99e836] device_failed | complete-request-id | (*diagnostics.stageError / capture_start.encoder / (*diagnostics.fixedError / codec configuration rejected))\n"
	if output.String() != want {
		t.Fatalf("unexpected human-readable diagnostic:\n%s", output.String())
	}
}

func TestPatternKeepsConcurrentContextAndEscapesInjectedLines(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(newPatternHandler(&output, slog.LevelWarn))
	logger.Info("filtered")
	const writers = 12
	var wait sync.WaitGroup
	for i := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			logger.With("deviceId", fmt.Sprintf("%08x-long-id", i)).WithGroup("operation").With("requestId", fmt.Sprintf("request-%d", i)).Warn("failed", "cause", "alpha\nbeta")
		}()
	}
	wait.Wait()
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != writers {
		t.Fatalf("records split, lost, or interleaved: %q", output.String())
	}
	for i := range writers {
		matches := 0
		for _, line := range lines {
			if strings.Contains(line, fmt.Sprintf("[%08x] failed", i)) && strings.Contains(line, fmt.Sprintf("request-%d)", i)) && strings.Contains(line, `"alpha\nbeta"`) {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("device/request context crossed for writer %d: %s", i, output.String())
		}
	}
}
