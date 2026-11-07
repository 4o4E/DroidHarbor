package leases

import (
	"context"
	"errors"
	"testing"
	"time"

	"droidharbor/internal/identity"
)

func TestNewReclaimsAvailableDeviceWithUncleanOwnedResource(t *testing.T) {
	manager, db, _, _, bearer := newLeaseTestManager(t, 1)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES('orphan-resource','a','device-capture','capture-a','prior-boot','capture','scid:123','active',?)`, time.Now().UTC().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	cleaned := make(chan struct{})
	source := &testDeviceSource{recoverCalls: make(map[string]int)}
	source.setLinks(Link{DeviceID: "a", LinkID: "link-a", TransportID: 1, Generation: 1})
	source.setRecovery(func(ctx context.Context, deviceID string) (bool, error) {
		close(entered)
		select {
		case <-release:
			_, err := db.Exec(`UPDATE owned_resources SET state='cleaned',cleanup_error=NULL WHERE id='orphan-resource'`)
			close(cleaned)
			return true, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})
	manager, err := New(context.Background(), db, source, testManagerLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not recover the device with an orphan resource")
	}
	var state string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id='a'`).Scan(&state); err != nil || state != "reclaiming" {
		t.Fatalf("unclean resource did not keep prior available device unavailable: state=%q err=%v", state, err)
	}
	if _, err = manager.Claim(context.Background(), bearer, ClaimRequest{TaskID: "after-restart", TaskSecret: identity.Secret(), IdempotencyKey: "after-restart-key"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("device with unclean external resource was reassigned: %v", err)
	}
	close(release)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("test recovery did not finish")
	}
}

func TestOfflineDeviceCanEnterMaintenanceAfterCleanup(t *testing.T) {
	manager, db, source, _, _ := newLeaseTestManager(t, 1)
	source.setLinks()
	// Recover reports resource-cleanup completion, not online health; this
	// offline fixture has no unclean owned resources.
	source.setRecovery(func(context.Context, string) (bool, error) { return true, nil })
	if err := manager.SetMaintenance(context.Background(), "a", true, "admin-session"); err != nil {
		t.Fatal(err)
	}
	var state string
	var requested int
	if err := db.QueryRow(`SELECT allocation_state,maintenance_requested FROM devices WHERE id='a'`).Scan(&state, &requested); err != nil || state != "maintenance" || requested != 1 {
		t.Fatalf("cleaned offline device did not enter maintenance: state=%q requested=%d err=%v", state, requested, err)
	}
}
