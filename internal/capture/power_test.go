package capture

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"droidharbor/internal/leases"
	"droidharbor/internal/storage"
)

func TestPowerRecoveryPreservesCurrentVideoOwnership(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO devices(id,display_name,allocation_state) VALUES('device','phone','maintenance')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES('video','device','device-capture','video','boot','scrcpy','{}','active',1)`); err != nil {
		t.Fatal(err)
	}
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 1}
	video := &Manager{ctx: ctx, db: db, source: &testSource{link: link, available: true}, logger: slog.Default(), streams: make(map[string]*Stream), recovering: make(map[string]*recoveryFlight)}
	stream := &Stream{ctx: ctx, deviceID: "device", link: link, live: true}
	video.streams["device"] = stream
	power := video.NewPowerManager()
	defer power.cancel()
	complete, err := power.Recover(ctx, "device")
	if err != nil || !complete {
		t.Fatalf("power recovery blocked a healthy viewing capture: complete=%v err=%v", complete, err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM owned_resources WHERE id='video'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" || stream.stopRequested.Load() {
		t.Fatalf("power recovery retired another owner: state=%s", state)
	}
}
