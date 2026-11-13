package capture

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"droidharbor/internal/leases"
	"droidharbor/internal/storage"
)

func TestRecoverOfflineCompletesOnlyWithoutOwnedCaptureResources(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "capture.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES('device','test-device','available',0)`); err != nil {
		t.Fatal(err)
	}
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: false}
	manager := &Manager{logger: slog.Default(), db: db, source: source, streams: make(map[string]*Stream), recovering: make(map[string]*recoveryFlight)}

	complete, err := manager.Recover(ctx, "device")
	if err != nil || !complete {
		t.Fatalf("offline recovery without owned resources = %v, %v; want complete", complete, err)
	}
	if _, err := db.Exec(`INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES('capture','device','device-capture','capture','boot','scrcpy','{}','failed',?)`, time.Now().UTC().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	complete, err = manager.Recover(ctx, "device")
	if err != nil || complete {
		t.Fatalf("offline recovery with an unclean capture resource = %v, %v; want incomplete", complete, err)
	}
}

func TestConcurrentRecoveryWaitsForConfirmedResultAndAllowsCancellation(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	flight := &recoveryFlight{done: make(chan struct{})}
	manager := &Manager{db: db, streams: make(map[string]*Stream), recovering: map[string]*recoveryFlight{"recovering-phone": flight}}
	// A different device must not wait on this phone's pending ownership check.
	if complete, err := manager.Recover(ctx, "other-phone"); err != nil || !complete {
		t.Fatalf("unrelated recovery was blocked: complete=%v err=%v", complete, err)
	}
	waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if complete, err := manager.Recover(waiting, "recovering-phone"); complete || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("in-progress cleanup was mistaken for a terminal result: complete=%v err=%v", complete, err)
	}
	timer := time.AfterFunc(20*time.Millisecond, func() {
		flight.complete = true
		close(flight.done)
	})
	defer timer.Stop()
	if complete, err := manager.Recover(ctx, "recovering-phone"); err != nil || !complete {
		t.Fatalf("joined cleanup did not return its confirmed outcome: complete=%v err=%v", complete, err)
	}
}
