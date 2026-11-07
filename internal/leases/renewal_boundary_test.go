package leases

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"droidharbor/internal/identity"
)

func TestRenewReplacesExpiryTicketBeforeOldCallbackRuns(t *testing.T) {
	manager, _, _, _, bearer := newLeaseTestManager(t, 1)
	clock := &controllableClock{value: time.Now()}
	manager.mu.Lock()
	manager.now = clock.Now
	manager.mu.Unlock()
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "ticket-task", secret, "ticket-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	state := manager.leases[lease.ID]
	oldTicket := state.expiry.Load()
	manager.mu.Unlock()
	if oldTicket == nil {
		t.Fatal("claimed lease has no expiry ticket")
	}
	if _, err = manager.Renew(context.Background(), principal, lease.ID); err != nil {
		t.Fatal(err)
	}
	newTicket := state.expiry.Load()
	if newTicket == nil || newTicket == oldTicket {
		t.Fatal("successful renewal did not atomically replace its expiry ticket")
	}
	if manager.expireTicket(state, oldTicket) {
		t.Fatal("stale expiry callback canceled the renewed lease")
	}
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); err != nil {
		t.Fatalf("stale callback removed renewed authorization: %v", err)
	}
}

func TestRenewCommitCrossingOldDeadlineEndsLeaseWithoutSuccessRecord(t *testing.T) {
	manager, db, _, _, bearer := newLeaseTestManager(t, 1)
	db.SetMaxOpenConns(2)
	clock := &controllableClock{value: time.Now()}
	manager.mu.Lock()
	manager.now = clock.Now
	manager.mu.Unlock()
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "late-renew-task", secret, "late-renew-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.ExecContext(context.Background(), `UPDATE devices SET display_name=display_name WHERE id=?`, lease.DeviceID); err != nil {
		_ = blocker.Rollback()
		t.Fatal(err)
	}
	type renewResult struct {
		lease Lease
		err   error
	}
	result := make(chan renewResult, 1)
	go func() {
		renewed, renewErr := manager.Renew(context.Background(), principal, lease.ID)
		result <- renewResult{lease: renewed, err: renewErr}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for db.Stats().InUse < 2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if db.Stats().InUse < 2 {
		_ = blocker.Rollback()
		t.Fatal("Renew did not reach the blocked SQLite write")
	}
	time.Sleep(20 * time.Millisecond)
	clock.Set(lease.ExpiresAt.Add(time.Millisecond))
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case renewed := <-result:
		if !errors.Is(renewed.err, ErrLeaseEnded) {
			t.Fatalf("renew crossing the prior deadline unexpectedly succeeded: lease=%+v err=%v", renewed.lease, renewed.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("late renewal did not finish after SQLite unlocked")
	}
	var status, reason string
	if err = db.QueryRow(`SELECT status,end_reason FROM leases WHERE id=?`, lease.ID).Scan(&status, &reason); err != nil || status != LeaseEnded || reason != "expired" {
		t.Fatalf("late renewal did not persist expiry: status=%q reason=%q err=%v", status, reason, err)
	}
	var successes, failures int
	if err = db.QueryRow(`SELECT sum(status='succeeded'),sum(status='failed') FROM operation_records WHERE lease_id=? AND kind='lease.renew'`, lease.ID).Scan(&successes, &failures); err != nil {
		t.Fatal(err)
	}
	if successes != 0 || failures != 1 {
		t.Fatalf("late renewal operation records=%d successful, %d failed; want 0,1", successes, failures)
	}
}
func TestExpiryTicketCancelsGrantWithoutManagerMutex(t *testing.T) {
	manager, _, _, _, bearer := newLeaseTestManager(t, 1)
	clock := &controllableClock{value: time.Now()}
	manager.mu.Lock()
	manager.now = clock.Now
	manager.mu.Unlock()
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "expiry-cancel-task", secret, "expiry-cancel-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	state := manager.leases[lease.ID]
	ticket := state.expiry.Load()
	expired := make(chan bool, 1)
	go func() { expired <- manager.expireTicket(state, ticket) }()
	select {
	case ok := <-expired:
		if !ok {
			manager.mu.Unlock()
			t.Fatal("current expiry ticket lost its cancellation race")
		}
	case <-time.After(time.Second):
		manager.mu.Unlock()
		t.Fatal("expiry callback waited for the manager mutex")
	}
	select {
	case <-grant.Done():
	case <-time.After(time.Second):
		manager.mu.Unlock()
		t.Fatal("expiry callback did not cancel the granted context")
	}
	manager.mu.Unlock()
}
