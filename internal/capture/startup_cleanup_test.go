package capture

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
	"droidharbor/internal/storage"
)

func TestStartFailureWithDeferredJournalCleanupReportsResourceFailure(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "capture.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES('device','test-device','available',0)`); err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	client, err := adb.NewClient("127.0.0.1:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: true}
	manager, err := New(ctx, db, client, source, Config{ServerPath: filepath.Join("..", "..", ".tools", "scrcpy-server-v5.0.1")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetBootID("boot")
	reported := make(chan string, 2)
	resourceFailureErr := errors.New("lease persistence unavailable")
	if err := manager.SetResourceFailure(func(ctx context.Context, deviceID string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reported <- deviceID
		return resourceFailureErr
	}); err != nil {
		t.Fatal(err)
	}
	startupFailure := errors.New("startup authorization revoked")
	if _, err := manager.Start(ctx, "", Settings{}, func() error { return nil }); err == nil {
		t.Fatal("Start() accepted an empty device ID")
	}
	if _, err := manager.Start(ctx, "device", Settings{}, func() error { return startupFailure }); !errors.Is(err, startupFailure) {
		t.Fatalf("pre-journal authorization error = %v, want authorization failure", err)
	}
	select {
	case deviceID := <-reported:
		t.Fatalf("pre-journal rejection reported resource failure for %q", deviceID)
	default:
	}

	calls := 0
	_, startErr := manager.Start(ctx, "device", Settings{MaxSize: 1080, MaxFPS: 30, BitRate: 2_000_000}, func() error {
		calls++
		if calls == 2 {
			source.available = false
			return startupFailure
		}
		return nil
	})
	if !errors.Is(startErr, startupFailure) {
		t.Fatalf("Start() error = %v, want startup authorization failure", startErr)
	}
	select {
	case deviceID := <-reported:
		if deviceID != "device" {
			t.Fatalf("resource failure reported for %q, want device", deviceID)
		}
	default:
		t.Fatal("journaled deferred cleanup did not report resource failure")
	}

	var id, state string
	if err := db.QueryRow(`SELECT id,state FROM owned_resources WHERE device_id='device' AND owner_kind='device-capture'`).Scan(&id, &state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("owned resource state = %q, want failed", state)
	}
	stream := &Stream{manager: manager, id: id, deviceID: "device", link: link}
	stream.journaled.Store(true)
	cleanupErr := manager.cleanup(ctx, stream)
	if !errors.Is(cleanupErr, ErrCleanupDeferred) || !errors.Is(cleanupErr, resourceFailureErr) {
		t.Fatalf("cleanup() error = %v, want deferred cleanup and resource failure", cleanupErr)
	}
	if err := db.QueryRow(`SELECT state FROM owned_resources WHERE id=?`, id).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("failed reporting changed journal state: state=%q err=%v", state, err)
	}
}
