package leases

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"droidharbor/internal/identity"
	"droidharbor/internal/storage"
)

func testManagerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type testDeviceSource struct {
	mu           sync.Mutex
	links        []Link
	recoverCalls map[string]int
	recoverFn    func(context.Context, string) (bool, error)
}

func (s *testDeviceSource) OnlineLinks() []Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Link(nil), s.links...)
}

func (s *testDeviceSource) HasLink(want Link) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, link := range s.links {
		if link == want {
			return true
		}
	}
	return false
}

func (s *testDeviceSource) Recover(ctx context.Context, deviceID string) (bool, error) {
	s.mu.Lock()
	s.recoverCalls[deviceID]++
	fn := s.recoverFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, deviceID)
	}
	return true, nil
}

func (s *testDeviceSource) setLinks(links ...Link) {
	s.mu.Lock()
	s.links = append([]Link(nil), links...)
	s.mu.Unlock()
}

func (s *testDeviceSource) setRecovery(fn func(context.Context, string) (bool, error)) {
	s.mu.Lock()
	s.recoverFn = fn
	s.mu.Unlock()
}

func (s *testDeviceSource) calls(deviceID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverCalls[deviceID]
}

type controllableClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *controllableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *controllableClock) Set(value time.Time) {
	c.mu.Lock()
	c.value = value
	c.mu.Unlock()
}

func newLeaseTestManager(t *testing.T, deviceCount int) (*Manager, *sql.DB, *testDeviceSource, string, string) {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "leases.db"))
	if err != nil {
		t.Fatal(err)
	}
	callerID, bearer := "caller-test", "dhc_"+identity.Secret()
	if _, err = db.Exec(`INSERT INTO caller_credentials(id,name,secret_hash,created_at) VALUES(?,?,?,?)`, callerID, "test", identity.Digest(bearer), time.Now().UTC().UnixMilli()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	source := &testDeviceSource{recoverCalls: make(map[string]int)}
	links := make([]Link, 0, deviceCount)
	for i := range deviceCount {
		deviceID := string(rune('a' + i))
		linkID := "link-" + deviceID
		if _, err = db.Exec(`INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES(?,?,'available',0)`, deviceID, deviceID); err != nil {
			db.Close()
			t.Fatal(err)
		}
		links = append(links, Link{DeviceID: deviceID, LinkID: linkID, TransportID: uint64(i + 1), Generation: 1})
	}
	source.setLinks(links...)
	manager, err := New(context.Background(), db, source, testManagerLogger())
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = manager.Close()
		_ = db.Close()
	})
	return manager, db, source, callerID, bearer
}

func claimForTest(t *testing.T, manager *Manager, bearer, externalTaskID, secret, key string, tags ...string) Lease {
	t.Helper()
	lease, err := manager.Claim(context.Background(), bearer, ClaimRequest{TaskID: externalTaskID, TaskSecret: secret, IdempotencyKey: key, RequiredTags: tags})
	if err != nil {
		t.Fatalf("Claim(%q): %v", externalTaskID, err)
	}
	return lease
}

func TestClaimIdempotencyTaskIsolationAndReclaim(t *testing.T) {
	manager, db, _, _, bearer := newLeaseTestManager(t, 2)
	for _, deviceID := range []string{"a", "b"} {
		if _, err := db.Exec(`INSERT INTO device_tags(device_id,tag) VALUES(?,'blue')`, deviceID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Claim(context.Background(), bearer, ClaimRequest{TaskID: "no-match", TaskSecret: identity.Secret(), IdempotencyKey: "no-match", RequiredTags: []string{"blue", "green"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("partial tag match expanded to an arbitrary device: %v", err)
	}
	if _, err := manager.Claim(context.Background(), bearer, ClaimRequest{TaskID: "blank-tag", TaskSecret: identity.Secret(), IdempotencyKey: "blank-tag", RequiredTags: []string{"  "}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("blank tag was silently discarded: %v", err)
	}
	var taskCount int
	if err := db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&taskCount); err != nil || taskCount != 0 {
		t.Fatalf("failed claims left orphan tasks: count=%d err=%v", taskCount, err)
	}

	ctx := context.Background()
	secretA, secretB := identity.Secret(), identity.Secret()
	requestA := ClaimRequest{TaskID: "external-A", TaskSecret: secretA, IdempotencyKey: "request-A", RequiredTags: []string{" blue ", "blue"}}
	leaseA, err := manager.Claim(ctx, bearer, requestA)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := manager.Claim(ctx, bearer, ClaimRequest{TaskID: "external-A", TaskSecret: secretA, IdempotencyKey: "request-A", RequiredTags: []string{"blue"}})
	if err != nil || replayed.ID != leaseA.ID || replayed.DeviceID != leaseA.DeviceID {
		t.Fatalf("normalized retry changed lease: lease=%+v err=%v", replayed, err)
	}
	leaseB := claimForTest(t, manager, bearer, "external-B", secretB, "request-B", "blue")
	principalA, err := manager.Authenticate(ctx, secretA)
	if err != nil {
		t.Fatal(err)
	}
	principalB, err := manager.Authenticate(ctx, secretB)
	if err != nil {
		t.Fatal(err)
	}
	callerPrincipal, err := manager.Authenticate(ctx, bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Get(ctx, callerPrincipal, leaseA.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("shared caller bearer read task lease: %v", err)
	}
	if _, err = manager.Get(ctx, principalB, leaseA.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("task B read task A lease: %v", err)
	}
	if _, err = manager.Renew(ctx, principalB, leaseA.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("task B renewed task A lease: %v", err)
	}
	if _, err = manager.Release(ctx, principalB, leaseA.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("task B released task A lease: %v", err)
	}
	if _, err = manager.Claim(ctx, bearer, ClaimRequest{TaskID: "external-A", TaskSecret: secretA, IdempotencyKey: "request-A", RequiredTags: []string{"green"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed parameters reused idempotency key: %v", err)
	}
	if _, err = manager.Claim(ctx, bearer, ClaimRequest{TaskID: "external-A", TaskSecret: identity.Secret(), IdempotencyKey: "request-A2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("task secret was rebound: %v", err)
	}
	if _, err = manager.Claim(ctx, bearer, ClaimRequest{TaskID: "external-B", TaskSecret: secretA, IdempotencyKey: "request-B2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("secret was shared across tasks: %v", err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM tasks`).Scan(&taskCount); err != nil || taskCount != 2 {
		t.Fatalf("failed claims created task rows: count=%d err=%v", taskCount, err)
	}
	if _, err = manager.Claim(ctx, bearer, ClaimRequest{TaskID: "external-A", TaskSecret: secretA, IdempotencyKey: "request-A3"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("active task claimed a second lease: %v", err)
	}
	ended, err := manager.Release(ctx, principalA, leaseA.ID)
	if err != nil || ended.Status != LeaseEnded {
		t.Fatalf("release failed: lease=%+v err=%v", ended, err)
	}
	duplicate, err := manager.Release(ctx, principalA, leaseA.ID)
	if err != nil || duplicate.Status != LeaseEnded {
		t.Fatalf("repeat release was not idempotent: lease=%+v err=%v", duplicate, err)
	}
	terminalReplay, err := manager.Claim(ctx, bearer, requestA)
	if err != nil || terminalReplay.ID != leaseA.ID || terminalReplay.Status != LeaseEnded {
		t.Fatalf("terminal idempotent replay reallocated: lease=%+v err=%v", terminalReplay, err)
	}
	reclaimed := claimForTest(t, manager, bearer, "external-A", secretA, "request-A4", "blue")
	if reclaimed.ID == leaseA.ID || reclaimed.Generation <= leaseA.Generation {
		t.Fatalf("reclaim did not create a new generation: first=%+v next=%+v", leaseA, reclaimed)
	}
	if _, err = manager.Get(ctx, principalA, leaseB.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("same caller crossed task boundary: %v", err)
	}
}

func TestConcurrentClaimsCannotDoubleAssignDevice(t *testing.T) {
	manager, _, _, _, bearer := newLeaseTestManager(t, 1)
	start := make(chan struct{})
	type result struct {
		lease Lease
		err   error
	}
	results := make(chan result, 2)
	for _, taskID := range []string{"task-one", "task-two"} {
		go func(taskID string) {
			<-start
			lease, err := manager.Claim(context.Background(), bearer, ClaimRequest{TaskID: taskID, TaskSecret: identity.Secret(), IdempotencyKey: "key-" + taskID})
			results <- result{lease: lease, err: err}
		}(taskID)
	}
	close(start)
	first, second := <-results, <-results
	successes, unavailable := 0, 0
	for _, value := range []result{first, second} {
		if value.err == nil && value.lease.Status == LeaseActive {
			successes++
		} else if errors.Is(value.err, ErrUnavailable) {
			unavailable++
		} else {
			t.Fatalf("unexpected concurrent claim result: %+v", value)
		}
	}
	if successes != 1 || unavailable != 1 {
		t.Fatalf("claims=%d unavailable=%d", successes, unavailable)
	}
}

func TestGrantPinsLinkAndMonotonicExpiryRevokesContext(t *testing.T) {
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	clock := &controllableClock{value: time.Now()}
	manager.mu.Lock()
	manager.now = clock.Now
	manager.mu.Unlock()
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "timed-task", secret, "timed-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE leases SET expires_at=? WHERE id=?`, lease.ExpiresAt.Add(24*time.Hour).UnixMilli(), lease.ID); err != nil {
		t.Fatal(err)
	}
	grant1, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalLink := grant1.Link()
	if originalLink.LinkID != "link-a" {
		t.Fatalf("initial grant failed: link=%+v", originalLink)
	}
	source.setLinks(Link{DeviceID: lease.DeviceID, LinkID: originalLink.LinkID, TransportID: 22, Generation: 2})
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("stale transport remained authorized: %v", err)
	}
	select {
	case <-grant1.Done():
	case <-time.After(time.Second):
		t.Fatal("link generation change did not cancel the old grant")
	}
	grant2, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	renewedLink := grant2.Link()
	if renewedLink.TransportID != 22 || renewedLink.Generation != 2 {
		t.Fatalf("explicit same-link reconnect failed: link=%+v", renewedLink)
	}
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); err != nil {
		t.Fatalf("new grant did not authorize the current link: %v", err)
	}
	source.setLinks(Link{DeviceID: lease.DeviceID, LinkID: "other-link", TransportID: 23, Generation: 1})
	if _, err = manager.Grant(context.Background(), principal, lease.ID); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("grant silently switched to another link: %v", err)
	}
	select {
	case <-grant2.Done():
	case <-time.After(time.Second):
		t.Fatal("link loss did not cancel the old grant")
	}
	if _, err = manager.Renew(context.Background(), principal, lease.ID); err != nil {
		t.Fatalf("offline link ended a still-valid lease: %v", err)
	}
	clock.Set(clock.Now().Add(LeaseDuration + time.Second))
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("persisted UTC expiry overrode the runtime deadline: %v", err)
	}
	if _, err = manager.Renew(context.Background(), principal, lease.ID); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("expired lease was revived: %v", err)
	}
}

func TestReclaimFailureQuarantinesUntilExplicitRetry(t *testing.T) {
	manager, db, source, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "cleanup-task", secret, "cleanup-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	cleanupFailure := errors.New("external cleanup failed")
	source.setRecovery(func(context.Context, string) (bool, error) { return false, cleanupFailure })
	if _, err = manager.Release(context.Background(), principal, lease.ID); !errors.Is(err, ErrReclaimFailed) {
		t.Fatalf("cleanup failure was hidden: %v", err)
	}
	var state string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&state); err != nil || state != "quarantined" {
		t.Fatalf("cleanup failure did not quarantine device: state=%q err=%v", state, err)
	}
	if count := source.calls(lease.DeviceID); count != 1 {
		t.Fatalf("cleanup was retried without an explicit request: calls=%d", count)
	}
	source.setRecovery(func(context.Context, string) (bool, error) { return true, nil })
	if err = manager.RetryRecovery(context.Background(), lease.DeviceID); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&state); err != nil || state != "available" {
		t.Fatalf("confirmed recovery did not return device to pool: state=%q err=%v", state, err)
	}
}

func TestRevokeCallerCancelsGrantAndRevokesTasks(t *testing.T) {
	manager, _, _, callerID, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "revoke-task", secret, "revoke-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.RevokeCaller(context.Background(), callerID, "admin-session"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-grant.Done():
	case <-time.After(time.Second):
		t.Fatal("caller revocation did not cancel existing grant")
	}
	if _, err = manager.Authenticate(context.Background(), secret); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked task bearer remained valid: %v", err)
	}
	if _, err = manager.Get(context.Background(), principal, lease.ID); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("stale task principal remained authorized after revocation: %v", err)
	}
}

func TestCloseCancelsGrantContexts(t *testing.T) {
	manager, _, _, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager, bearer, "close-task", secret, "close-key")
	principal, err := manager.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.Grant(context.Background(), principal, lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-grant.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel grant contexts")
	}
	if err = manager.Check(lease.ID, lease.Generation, lease.BootID); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed manager kept authorization active: %v", err)
	}
}

func TestNewEndsHistoricalLeaseAndNeverRestoresItsDeadline(t *testing.T) {
	manager1, db, _, _, bearer := newLeaseTestManager(t, 1)
	secret := identity.Secret()
	lease := claimForTest(t, manager1, bearer, "restart-task", secret, "restart-key")
	if err := manager1.Close(); err != nil {
		t.Fatal(err)
	}
	source2 := &testDeviceSource{recoverCalls: make(map[string]int), recoverFn: func(context.Context, string) (bool, error) { return false, nil }}
	source2.setLinks(Link{DeviceID: lease.DeviceID, LinkID: "link-a", TransportID: 1, Generation: 1})
	manager2, err := New(context.Background(), db, source2, testManagerLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager2.Close() })
	if manager2.BootID() == lease.BootID {
		t.Fatal("manager restart reused the prior boot ID")
	}
	var status, reason string
	if err = db.QueryRow(`SELECT status,end_reason FROM leases WHERE id=?`, lease.ID).Scan(&status, &reason); err != nil || status != LeaseEnded || reason != "platform_restart" {
		t.Fatalf("historical lease remained active: status=%q reason=%q err=%v", status, reason, err)
	}
	principal, err := manager2.Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager2.Renew(context.Background(), principal, lease.ID); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("old task renewed after restart: %v", err)
	}
	terminal, err := manager2.Claim(context.Background(), bearer, ClaimRequest{TaskID: "restart-task", TaskSecret: secret, IdempotencyKey: "restart-key"})
	if err != nil || terminal.ID != lease.ID || terminal.Status != LeaseEnded {
		t.Fatalf("old idempotency key created a new lease: lease=%+v err=%v", terminal, err)
	}
	var state string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&state); err != nil || state != "reclaiming" {
		t.Fatalf("offline startup recovery made device available: state=%q err=%v", state, err)
	}
}
