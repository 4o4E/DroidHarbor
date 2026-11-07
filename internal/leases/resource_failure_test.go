package leases

import (
	"context"
	"errors"
	"testing"

	"droidharbor/internal/identity"
)

func TestResourceFailureBlocksNewAllocationsWithoutRevokingActiveLease(t *testing.T) {
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "human-resource-task", secret, "human-resource-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err = manager.ResourceFailure(context.Background(), lease.DeviceID); err != nil {
		t.Fatal(err)
	}
	if err = manager.RetryRecovery(context.Background(), lease.DeviceID); !errors.Is(err, ErrConflict) {
		t.Fatalf("explicit recovery attempt against active lease error=%v, want conflict", err)
	}
	if err = grant.Context.Err(); err != nil {
		t.Fatalf("resource failure revoked active lease grant: %v", err)
	}
	var allocation, status string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&allocation); err != nil || allocation != "leased" {
		t.Fatalf("active lease allocation changed: state=%q err=%v", allocation, err)
	}
	if err = db.QueryRow(`SELECT status FROM leases WHERE id=?`, lease.ID).Scan(&status); err != nil || status != LeaseActive {
		t.Fatalf("active lease was ended: status=%q err=%v", status, err)
	}
	manager.mu.Lock()
	_, blocked := manager.blockedDevices[lease.DeviceID]
	_, pending := manager.pendingRecovery[lease.DeviceID]
	manager.mu.Unlock()
	if !blocked || !pending {
		t.Fatalf("uncertain resource was not retained in runtime guard: blocked=%v pending=%v", blocked, pending)
	}

	source.setRecovery(func(context.Context, string) (bool, error) { return true, nil })
	if _, err = manager.Release(context.Background(), principal, lease.ID); err != nil {
		t.Fatalf("release did not run resource recovery: %v", err)
	}
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&allocation); err != nil || allocation != "available" {
		t.Fatalf("confirmed recovery did not make device available: state=%q err=%v", allocation, err)
	}
	if source.calls(lease.DeviceID) != 1 {
		t.Fatalf("composite recovery calls=%d, want 1 after active lease release", source.calls(lease.DeviceID))
	}
	if err = db.QueryRow(`SELECT status FROM operation_records WHERE device_id=? AND kind='capture.resource_failure'`, lease.DeviceID).Scan(&status); err != nil || status != "succeeded" {
		t.Fatalf("resource-failure operation status=%q err=%v", status, err)
	}
}

func TestRetryRecoveryRepairsResourceFailurePersistenceFromAvailableState(t *testing.T) {
	manager, db, source, _, _ := newLeaseTestManager(t, 1)
	if _, err := db.Exec(`CREATE TRIGGER refuse_resource_failure BEFORE INSERT ON operation_records WHEN NEW.kind='capture.resource_failure' BEGIN SELECT RAISE(FAIL,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	err := manager.ResourceFailure(context.Background(), "a")
	if err == nil || !errors.Is(err, ErrStorage) {
		t.Fatalf("ResourceFailure error=%v, want storage failure", err)
	}
	var state string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id='a'`).Scan(&state); err != nil || state != "available" {
		t.Fatalf("failed persistence changed durable allocation: state=%q err=%v", state, err)
	}
	manager.mu.Lock()
	_, blocked := manager.blockedDevices["a"]
	_, pending := manager.pendingRecovery["a"]
	_, resourceFailure := manager.resourceFailures["a"]
	manager.mu.Unlock()
	if !blocked || !pending || !resourceFailure {
		t.Fatalf("failed persistence markers: blocked=%v pending=%v resourceFailure=%v", blocked, pending, resourceFailure)
	}
	if _, err = db.Exec(`DROP TRIGGER refuse_resource_failure`); err != nil {
		t.Fatal(err)
	}
	source.setRecovery(func(context.Context, string) (bool, error) { return true, nil })
	if err = manager.RetryRecovery(context.Background(), "a"); err != nil {
		t.Fatalf("RetryRecovery() from blocked available state: %v", err)
	}
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id='a'`).Scan(&state); err != nil || state != "available" {
		t.Fatalf("confirmed recovery did not restore availability: state=%q err=%v", state, err)
	}
	if source.calls("a") != 1 {
		t.Fatalf("composite recovery calls=%d, want 1", source.calls("a"))
	}
	manager.mu.Lock()
	_, blocked = manager.blockedDevices["a"]
	_, pending = manager.pendingRecovery["a"]
	_, resourceFailure = manager.resourceFailures["a"]
	manager.mu.Unlock()
	if blocked || pending || resourceFailure {
		t.Fatalf("successful recovery retained markers: blocked=%v pending=%v resourceFailure=%v", blocked, pending, resourceFailure)
	}
}
