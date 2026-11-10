package capture

import (
	"context"
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
	manager := &Manager{logger: slog.Default(), db: db, source: source, streams: make(map[string]*Stream), recovering: make(map[string]bool)}

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
