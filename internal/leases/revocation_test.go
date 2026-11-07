package leases

import (
	"context"
	"errors"
	"testing"

	"droidharbor/internal/identity"
)

func TestRevokePersistenceFailureStillDeniesRuntimeAuthorization(t *testing.T) {
	manager, db, source, callerID, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "failed-revoke-task", secret, "failed-revoke-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER block_caller_revoke BEFORE UPDATE OF revoked_at ON caller_credentials BEGIN SELECT RAISE(FAIL,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err = manager.RevokeCaller(context.Background(), callerID, "admin-session"); !errors.Is(err, ErrStorage) {
		t.Fatalf("failed revocation persistence was hidden: %v", err)
	}
	select {
	case <-grant.Done():
	default:
		t.Fatal("persistence failure restored an existing grant")
	}
	if _, err = manager.Authenticate(context.Background(), bearer); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("runtime still authenticated revoked caller: %v", err)
	}
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); !errors.Is(err, ErrStorage) {
		t.Fatalf("runtime authorization failure was hidden after failed revocation persistence: %v", err)
	}
	var state string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&state); err != nil || state != "leased" {
		t.Fatalf("failed persistence incorrectly returned device to pool: state=%q err=%v", state, err)
	}
	if source.calls(lease.DeviceID) != 0 {
		t.Fatal("cleanup claimed or ran without persisted revocation")
	}
}

func TestCallerRevokeRecordsOnceWithoutActiveLeases(t *testing.T) {
	manager, db, _, callerID, _ := newLeaseTestManager(t, 1)
	if err := manager.RevokeCaller(context.Background(), callerID, "admin-session"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RevokeCaller(context.Background(), callerID, "admin-session"); err != nil {
		t.Fatal(err)
	}
	var count int
	var details string
	if err := db.QueryRow(`SELECT count(*),min(safe_details) FROM operation_records WHERE kind='caller.revoke'`).Scan(&count, &details); err != nil {
		t.Fatal(err)
	}
	if count != 1 || details != `{"callerId":"caller-test"}` {
		t.Fatalf("caller revocation record is missing, duplicated, or contains excess details: count=%d details=%q", count, details)
	}
}

func TestReleaseRetriesUndurableTerminalState(t *testing.T) {
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "failed-release-task", secret, "failed-release-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER block_lease_end BEFORE UPDATE OF status ON leases WHEN NEW.status='ended' BEGIN SELECT RAISE(FAIL,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Release(context.Background(), principal, lease.ID); !errors.Is(err, ErrStorage) {
		t.Fatalf("terminal persistence failure was hidden: %v", err)
	}
	if _, err = db.Exec(`DROP TRIGGER block_lease_end`); err != nil {
		t.Fatal(err)
	}
	ended, err := manager.Release(context.Background(), principal, lease.ID)
	if err != nil || ended.Status != LeaseEnded {
		t.Fatalf("explicit Release retry did not persist the original termination: lease=%+v err=%v", ended, err)
	}
	manager.mu.Lock()
	_, retained := manager.leases[lease.ID]
	manager.mu.Unlock()
	if retained {
		t.Fatal("persisted terminal lease remained in runtime state")
	}
	var status string
	if err = db.QueryRow(`SELECT status FROM leases WHERE id=?`, lease.ID).Scan(&status); err != nil || status != LeaseEnded {
		t.Fatalf("retry left the database lease active: status=%q err=%v", status, err)
	}
	if source.calls(lease.DeviceID) != 1 {
		t.Fatalf("release retry unexpectedly replayed cleanup: calls=%d", source.calls(lease.DeviceID))
	}
}

func TestRetryRecoveryFinishesUndurableExpiryTermination(t *testing.T) {
	manager, db, _, _, bearer := newLeaseTestManager(t, 1)
	lease := claimForTest(t, manager, bearer, "retry-expiry-task", identity.Secret(), "retry-expiry-key")
	if _, err := db.Exec(`CREATE TRIGGER block_lease_end BEFORE UPDATE OF status ON leases WHEN NEW.status='ended' BEGIN SELECT RAISE(FAIL,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	state := manager.leases[lease.ID]
	_, endErr := manager.endLocked(context.Background(), state, "expired", "system", manager.BootID(), lease.ExpiresAt)
	manager.mu.Unlock()
	if !errors.Is(endErr, ErrStorage) {
		t.Fatalf("expiry terminal persistence failure was hidden: %v", endErr)
	}
	if _, err := db.Exec(`DROP TRIGGER block_lease_end`); err != nil {
		t.Fatal(err)
	}
	if err := manager.RetryRecovery(context.Background(), lease.DeviceID); err != nil {
		t.Fatal(err)
	}
	var status, allocation string
	if err := db.QueryRow(`SELECT l.status,d.allocation_state FROM leases l JOIN devices d ON d.id=l.device_id WHERE l.id=?`, lease.ID).Scan(&status, &allocation); err != nil {
		t.Fatal(err)
	}
	if status != LeaseEnded || allocation != "available" {
		t.Fatalf("explicit recovery did not finish terminal persistence and cleanup: lease=%q device=%q", status, allocation)
	}
	manager.mu.Lock()
	_, retained := manager.leases[lease.ID]
	manager.mu.Unlock()
	if retained {
		t.Fatal("persisted lease remained indexed in runtime state after recovery retry")
	}
}
