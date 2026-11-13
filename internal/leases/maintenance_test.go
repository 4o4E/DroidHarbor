package leases

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"droidharbor/internal/identity"
)

func TestSetMaintenanceRevokesAndRecoversLease(t *testing.T) {
	manager, db, _, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "maintenance-task", secret, "maintenance-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.SetMaintenance(context.Background(), lease.DeviceID, true, "admin-session", &lease.Generation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-grant.Done():
	default:
		t.Fatal("maintenance did not revoke the active lease grant")
	}
	ended, err := manager.Get(context.Background(), principal, lease.ID)
	if err != nil || ended.Status != LeaseEnded || ended.EndReason != "maintenance_requested" {
		t.Fatalf("maintenance did not end the AI lease: lease=%+v err=%v", ended, err)
	}
	var state string
	var requested int
	if err = db.QueryRow(`SELECT allocation_state,maintenance_requested FROM devices WHERE id=?`, lease.DeviceID).Scan(&state, &requested); err != nil || state != "maintenance" || requested != 1 {
		t.Fatalf("confirmed maintenance not persisted: state=%q requested=%d err=%v", state, requested, err)
	}
	if _, err = manager.Renew(context.Background(), principal, lease.ID); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("clearing maintenance or recovery revived the ended lease: %v", err)
	}
	if err = manager.SetMaintenance(context.Background(), lease.DeviceID, false, "admin-session", nil); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT allocation_state,maintenance_requested FROM devices WHERE id=?`, lease.DeviceID).Scan(&state, &requested); err != nil || state != "available" || requested != 0 {
		t.Fatalf("maintenance clear did not confirm availability: state=%q requested=%d err=%v", state, requested, err)
	}
}

func TestMaintenanceRequiresConfirmedRecoveryWithoutLease(t *testing.T) {
	manager, db, source, _, _ := newLeaseTestManager(t, 1)
	source.setRecovery(func(context.Context, string) (bool, error) {
		return false, errors.New("platform resource cleanup failed")
	})
	if err := manager.SetMaintenance(context.Background(), "a", true, "admin-session", nil); !errors.Is(err, ErrReclaimFailed) {
		t.Fatalf("maintenance ignored unconfirmed cleanup: %v", err)
	}
	var failedState string
	if err := db.QueryRow(`SELECT allocation_state FROM devices WHERE id='a'`).Scan(&failedState); err != nil || failedState != "quarantined" {
		t.Fatalf("failed cleanup entered maintenance: state=%q err=%v", failedState, err)
	}
	source.setRecovery(func(context.Context, string) (bool, error) { return true, nil })
	if err := manager.RetryRecovery(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT allocation_state FROM devices WHERE id='a'`).Scan(&state); err != nil || state != "maintenance" {
		t.Fatalf("maintenance was not set after confirmed cleanup: state=%q err=%v", state, err)
	}
	source.setRecovery(func(context.Context, string) (bool, error) { return false, nil })
	if err := manager.SetMaintenance(context.Background(), "a", false, "admin-session", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT allocation_state FROM devices WHERE id='a'`).Scan(&state); err != nil || state != "reclaiming" {
		t.Fatalf("offline recovery incorrectly made device available: state=%q err=%v", state, err)
	}
	source.setRecovery(func(context.Context, string) (bool, error) { return true, nil })
	if err := manager.RetryRecovery(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT allocation_state FROM devices WHERE id='a'`).Scan(&state); err != nil || state != "available" {
		t.Fatalf("confirmed healthy recovery did not make device available: state=%q err=%v", state, err)
	}
}

func TestMaintenanceWriteFailureLeavesLeaseDeniedAndDeviceBlocked(t *testing.T) {
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "maintenance-failure-task", secret, "maintenance-failure-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER block_maintenance BEFORE UPDATE OF maintenance_requested ON devices BEGIN SELECT RAISE(FAIL,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err = manager.SetMaintenance(context.Background(), lease.DeviceID, true, "admin-session", &lease.Generation); !errors.Is(err, ErrStorage) {
		t.Fatalf("failed maintenance persistence was hidden: %v", err)
	}
	select {
	case <-grant.Done():
	default:
		t.Fatal("failed persistence restored an existing lease grant")
	}
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); !errors.Is(err, ErrStorage) {
		t.Fatalf("failed maintenance restored dispatch authorization: %v", err)
	}
	manager.mu.Lock()
	_, blocked := manager.blockedDevices[lease.DeviceID]
	manager.mu.Unlock()
	if !blocked {
		t.Fatal("database failure left the device assignable in runtime")
	}
	if source.calls(lease.DeviceID) != 0 {
		t.Fatal("cleanup ran before the maintenance transition committed")
	}
	if _, err = manager.Claim(context.Background(), bearer, ClaimRequest{TaskID: "other-task", TaskSecret: identity.Secret(), IdempotencyKey: "other-key"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed maintenance transaction allowed reassignment: %v", err)
	}
}

func TestClearingMaintenanceDoesNotRevokeActiveLease(t *testing.T) {
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "active-lease-maintenance", secret, "active-lease-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.SetMaintenance(context.Background(), lease.DeviceID, false, "admin-session", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("maintenance clear interrupted an active lease: %v", err)
	}
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); err != nil {
		t.Fatalf("maintenance clear revoked a valid lease: %v", err)
	}
	if source.calls(lease.DeviceID) != 0 {
		t.Fatal("maintenance clear ran cleanup against an active lease")
	}
	if _, err = manager.Get(context.Background(), principal, lease.ID); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&state); err != nil || state != "leased" {
		t.Fatalf("maintenance conflict changed allocation state: state=%q err=%v", state, err)
	}
}

func TestStaleRecoveryPreservesActiveTaskResource(t *testing.T) {
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	lease := claimForTest(t, manager, bearer, "new-task", identity.Secret(), "new-task-key")
	resource := filepath.Join(t.TempDir(), "new-task-resource")
	if _, err := db.Exec(`INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES('current-resource',?,'task',?,?,'temporary-file',?,'active',?)`, lease.DeviceID, lease.TaskID, lease.BootID, resource, lease.ClaimedAt.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("owned by the current task"), 0600); err != nil {
		t.Fatal(err)
	}
	source.setRecovery(func(context.Context, string) (bool, error) {
		return true, os.Remove(resource)
	})
	// A queued old recovery can run after a successful retry and a new claim.
	// It must check current allocation before invoking external cleanup.
	if err := manager.recoverDevice(context.Background(), lease.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resource); err != nil {
		t.Fatal("stale recovery removed the active task's resource")
	}
	if err := manager.Check(lease.ID, lease.Generation, lease.BootID); err != nil {
		t.Fatalf("stale recovery changed current authorization: %v", err)
	}
}

func TestMaintenanceRequiresConfirmationOfCurrentTask(t *testing.T) {
	ctx := context.Background()
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	first := claimForTest(t, manager, bearer, "first-task", secret, "first-key")
	principal, err := manager.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	wrongGeneration := first.Generation + 1
	for _, confirmation := range []*int64{nil, &wrongGeneration} {
		if err = manager.SetMaintenance(ctx, first.DeviceID, true, "admin-session", confirmation); !errors.Is(err, ErrAllocationConfirmation) {
			t.Fatalf("unconfirmed task was not protected: %v", err)
		}
		if err = manager.Check(first.ID, first.Generation, first.BootID); err != nil {
			t.Fatalf("rejected change interrupted the current task: %v", err)
		}
	}
	var requested bool
	var allocation string
	if err = db.QueryRow(`SELECT maintenance_requested,allocation_state FROM devices WHERE id=?`, first.DeviceID).Scan(&requested, &allocation); err != nil || requested || allocation != "leased" {
		t.Fatalf("rejected change altered allocation: requested=%v state=%s err=%v", requested, allocation, err)
	}
	if source.calls(first.DeviceID) != 0 {
		t.Fatal("unconfirmed change cleaned up an active task")
	}
	if _, err = manager.Release(ctx, principal, first.ID); err != nil {
		t.Fatal(err)
	}
	second := claimForTest(t, manager, bearer, "second-task", identity.Secret(), "second-key")
	if err = manager.SetMaintenance(ctx, second.DeviceID, true, "admin-session", &first.Generation); !errors.Is(err, ErrAllocationConfirmation) {
		t.Fatalf("old dialog could end a later task: %v", err)
	}
	if err = manager.Check(second.ID, second.Generation, second.BootID); err != nil {
		t.Fatalf("old confirmation interrupted the new task: %v", err)
	}
	if err = manager.SetMaintenance(ctx, second.DeviceID, true, "admin-session", &second.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Claim(ctx, bearer, ClaimRequest{TaskID: "third-task", TaskSecret: identity.Secret(), IdempotencyKey: "third-key"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("confirmed allocation shutdown still allowed a new task: %v", err)
	}
}
