package leases

import (
	"context"
	"errors"
	"testing"
	"time"

	"droidharbor/internal/identity"
)

func TestGrantDispatchAndExpiryDoNotWaitForControlPlane(t *testing.T) {
	manager, _, _, _, bearer := newLeaseTestManager(t, 1)
	start := time.Now()
	clock := &controllableClock{value: start}
	manager.mu.Lock()
	manager.now = clock.Now
	manager.mu.Unlock()
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "dispatch-task", secret, "dispatch-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Close()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, expired := range []bool{false, true} {
		if expired {
			clock.Set(start.Add(901 * time.Second))
		}
		checked := make(chan error, 1)
		go func() { checked <- grant.Check() }()
		select {
		case err = <-checked:
			if expired && !errors.Is(err, ErrLeaseEnded) {
				t.Fatalf("expired dispatch error = %v", err)
			}
			if !expired && err != nil {
				t.Fatalf("active dispatch error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("packet authorization waited for a control-plane transaction")
		}
	}
	select {
	case <-grant.Done():
	default:
		t.Fatal("expiry left granted work running")
	}
}

func TestGrantSurvivesRenewalButNeverChangesPinnedTransport(t *testing.T) {
	manager, _, source, _, bearer := newLeaseTestManager(t, 1)
	start := time.Now()
	clock := &controllableClock{value: start}
	manager.mu.Lock()
	manager.now = clock.Now
	manager.mu.Unlock()
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "pinned-task", secret, "pinned-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	old, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	clock.Set(start.Add(600 * time.Second))
	if _, err = manager.Renew(context.Background(), principal, lease.ID); err != nil {
		t.Fatal(err)
	}
	clock.Set(start.Add(901 * time.Second))
	if err = old.Check(); err != nil {
		t.Fatalf("old deadline ended renewed grant: %v", err)
	}
	replacement := old.Link()
	replacement.Generation++
	replacement.TransportID++
	source.setLinks(replacement)
	if err = old.Check(); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("changed transport accepted by old grant: %v", err)
	}
	current, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err = current.Check(); err != nil {
		t.Fatalf("explicit new grant failed: %v", err)
	}
	if current.Link() != replacement {
		t.Fatal("new grant selected another transport")
	}
	if err = old.Check(); err == nil {
		t.Fatal("new grant revived old transport authorization")
	}
}

func TestRejectedRollbackRenewalPreservesOriginalExpiry(t *testing.T) {
	manager, _, _, _, bearer := newLeaseTestManager(t, 1)
	start := time.Now()
	clock := &controllableClock{value: start}
	manager.mu.Lock()
	manager.now = clock.Now
	manager.mu.Unlock()
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "rollback-task", secret, "rollback-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Close()
	clock.Set(start.Add(-time.Hour))
	if _, err = manager.Renew(context.Background(), principal, lease.ID); !errors.Is(err, ErrStorage) {
		t.Fatalf("rollback violating persisted time constraint: %v", err)
	}
	current, err := manager.Get(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != LeaseActive || !current.ExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatalf("failed renewal changed lease: %+v", current)
	}
	if err = grant.Check(); err != nil {
		t.Fatalf("failed renewal revoked valid grant: %v", err)
	}
	clock.Set(start.Add(901 * time.Second))
	if err = grant.Check(); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("failed renewal extended authorization: %v", err)
	}
}
