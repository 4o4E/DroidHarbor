package devices

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

func TestRemovedRegistrationPreservesHistoryAndRequiresExplicitRestore(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "removed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := New(ctx, db, nil, logger, time.Second)
	defer registry.Close()
	entry, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "usb", Endpoint: "phone"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE devices SET generation=7 WHERE id=?; INSERT INTO device_tags VALUES(?,'keep'); INSERT INTO operation_records(id,actor_kind,actor_id,device_id,kind,status,created_at) VALUES('historical','admin','admin',?,'old','succeeded',1)`, entry.DeviceID, entry.DeviceID, entry.DeviceID); err != nil {
		t.Fatal(err)
	}
	manager, err := leases.New(ctx, db, registry, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	registry.manager, registry.bootID = manager, manager.BootID()
	released := false
	registry.SetRemovalCleanup(func(context.Context, string) (func(), error) { return func() { released = true }, nil })
	views, _, err := registry.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Remove(ctx, "admin", entry.DeviceID, Removal{Version: "stale"}); err == nil {
		t.Fatal("stale removal was accepted")
	}
	if err = registry.Remove(ctx, "admin", entry.DeviceID, Removal{Version: views[0].Version}); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("removal boundary was not released")
	}
	views, _, err = registry.List(ctx, true)
	if err != nil || len(views) != 0 {
		t.Fatalf("removed registration still visible: %+v %v", views, err)
	}
	registry.publish([]adb.Device{{Serial: "phone", State: "unauthorized"}}, ctx)
	found, err := registry.Discover(ctx)
	if err != nil || len(found) != 1 || found[0].Serial != "phone" {
		t.Fatalf("removed phone cannot be explicitly re-added: %+v %v", found, err)
	}
	restored, err := registry.RegisterDiscovered(ctx, "admin", "phone", "")
	if err != nil {
		t.Fatal(err)
	}
	if restored.DeviceID != entry.DeviceID || restored.Verified {
		t.Fatalf("restore replaced identity or trusted an unauthorized phone: %+v", restored)
	}
	var generation, history int
	var state, tags string
	if err = db.QueryRow(`SELECT generation,allocation_state,(SELECT group_concat(tag) FROM device_tags WHERE device_id=d.id),(SELECT count(*) FROM operation_records WHERE id='historical') FROM devices d WHERE id=?`, entry.DeviceID).Scan(&generation, &state, &tags, &history); err != nil {
		t.Fatal(err)
	}
	if generation != 7 || state != "maintenance" || tags != "keep" || history != 1 {
		t.Fatalf("restore altered retained history: %d %s %s %d", generation, state, tags, history)
	}
}

func TestUnconfirmedRemovalKeepsManageableRegistration(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := New(ctx, db, nil, logger, time.Second)
	defer registry.Close()
	entry, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "usb", Endpoint: "phone"}})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := leases.New(ctx, db, registry, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	registry.manager, registry.bootID = manager, manager.BootID()
	failure := errors.New("owned process could not be verified")
	registry.SetRemovalCleanup(func(context.Context, string) (func(), error) { return func() {}, failure })
	views, _, err := registry.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Remove(ctx, "admin", entry.DeviceID, Removal{Version: views[0].Version}); !errors.Is(err, failure) {
		t.Fatalf("missing cleanup failure: %v", err)
	}
	views, _, err = registry.List(ctx, true)
	if err != nil || len(views) != 1 || views[0].AllocationState != "maintenance" {
		t.Fatalf("failed removal lost manageable blocked record: %+v %v", views, err)
	}
	registry.SetRemovalCleanup(func(context.Context, string) (func(), error) { return func() {}, nil })
	if err = registry.Remove(ctx, "admin", entry.DeviceID, Removal{Version: views[0].Version}); err != nil {
		t.Fatal(err)
	}
}
