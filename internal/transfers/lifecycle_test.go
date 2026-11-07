package transfers

import (
	"context"
	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
	"droidharbor/internal/media"
	"droidharbor/internal/storage"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type scopeProviderFunc func(context.Context, string, string, bool) (media.Scope, error)

func (f scopeProviderFunc) SessionScope(ctx context.Context, owner, sessionID string, write bool) (media.Scope, error) {
	return f(ctx, owner, sessionID, write)
}

func newLifecycleManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "transfer.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO devices(id,display_name,allocation_state) VALUES('device-1','test device','available')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	adbClient, err := adb.NewClient("127.0.0.1:5037", time.Second)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	provider := scopeProviderFunc(func(ctx context.Context, _, _ string, _ bool) (media.Scope, error) {
		return media.Scope{
			DeviceID: "device-1",
			Link:     leases.Link{DeviceID: "device-1", TransportID: 1},
			Context:  ctx,
			Close:    func() {},
			Check:    func() error { return ctx.Err() },
		}, nil
	})
	manager, err := New(ctx, Dependencies{DB: db, ADB: adbClient, Scopes: provider, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, Config{
		TempDir:          filepath.Join(t.TempDir(), "staging"),
		BootID:           "boot-1",
		MaxUploadBytes:   1 << 20,
		MaxActive:        2,
		MaxCommandOutput: 1024,
	})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return manager, func() { _ = manager.Close(context.Background()); _ = db.Close() }
}

func TestCreateScopeOutlivesHTTPRequestContext(t *testing.T) {
	manager, cleanup := newLifecycleManager(t)
	defer cleanup()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	transfer, err := manager.Create(requestCtx, "owner-1", "session-1", CreateRequest{FileName: "file.txt", Purpose: "file", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()

	manager.mu.Lock()
	lifetimeErr := manager.items[transfer.ID].ctx.Err()
	manager.mu.Unlock()
	if lifetimeErr != nil {
		t.Fatalf("transfer lifetime followed the create request context: %v", lifetimeErr)
	}
	if _, err := manager.Get(context.Background(), "owner-1", transfer.ID); err != nil {
		t.Fatalf("GET after create request ended: %v", err)
	}
	if err := manager.Cancel(context.Background(), "owner-1", transfer.ID); err != nil {
		t.Fatalf("cancel transfer after create request ended: %v", err)
	}
}

func TestCancelAndCloseReturnFinalizationErrors(t *testing.T) {
	manager, cleanup := newLifecycleManager(t)
	defer cleanup()
	transfer, err := manager.Create(context.Background(), "owner-1", "session-1", CreateRequest{FileName: "file.txt", Purpose: "file", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Exec(`CREATE TRIGGER reject_transfer_finalization BEFORE UPDATE ON operation_records WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT, 'finalization rejected'); END`); err != nil {
		t.Fatal(err)
	}
	cancelErr := manager.Cancel(context.Background(), "owner-1", transfer.ID)
	if cancelErr == nil {
		t.Fatal("Cancel reported success despite failed finalization persistence")
	}
	got, err := manager.Get(context.Background(), "owner-1", transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateUnknown {
		t.Fatalf("state after failed finalization = %q, want unknown", got.State)
	}
	if err := manager.Close(context.Background()); err == nil {
		t.Fatal("Close did not aggregate the transfer finalization error")
	}
}

type blockingUploadBody struct {
	readStarted  chan struct{}
	closeStarted chan struct{}
	allowClose   chan struct{}
	readOnce     sync.Once
	closeOnce    sync.Once
}

func (b *blockingUploadBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.readStarted) })
	<-b.closeStarted
	return 0, errors.New("request body closed")
}

func (b *blockingUploadBody) Close() error {
	b.closeOnce.Do(func() { close(b.closeStarted) })
	<-b.allowClose
	return nil
}

func TestUploadWaitsForCancellationBodyCloseCallback(t *testing.T) {
	manager, cleanup := newLifecycleManager(t)
	defer cleanup()
	transfer, err := manager.Create(context.Background(), "owner-1", "session-1", CreateRequest{FileName: "file.txt", Purpose: "file", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	body := &blockingUploadBody{
		readStarted:  make(chan struct{}),
		closeStarted: make(chan struct{}),
		allowClose:   make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	uploadDone := make(chan error, 1)
	go func() {
		_, err := manager.Upload(ctx, "owner-1", transfer.ID, 1, body)
		uploadDone <- err
	}()
	<-body.readStarted
	cancel()
	<-body.closeStarted
	select {
	case err := <-uploadDone:
		t.Fatalf("Upload returned before its cancellation Close callback finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(body.allowClose)
	if err := <-uploadDone; err == nil {
		t.Fatal("cancelled upload unexpectedly succeeded")
	}
}
func TestClosePersistsCancellationAfterManagerContextIsCancelled(t *testing.T) {
	manager, cleanup := newLifecycleManager(t)
	defer cleanup()
	transfer, err := manager.Create(context.Background(), "owner-1", "session-1", CreateRequest{FileName: "file.txt", Purpose: "file", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var status, resourceState string
	if err := manager.db.QueryRow(`SELECT status FROM operation_records WHERE id=?`, transfer.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.QueryRow(`SELECT state FROM owned_resources WHERE id=?`, transfer.ID).Scan(&resourceState); err != nil {
		t.Fatal(err)
	}
	if status != StateCancelled || resourceState != "cleaned" {
		t.Fatalf("persisted state = (%q, %q), want (%q, cleaned)", status, resourceState, StateCancelled)
	}
}
func TestFinishDoesNotPublishTerminalStateBeforePersistence(t *testing.T) {
	manager, cleanup := newLifecycleManager(t)
	defer cleanup()
	transfer, err := manager.Create(context.Background(), "owner-1", "session-1", CreateRequest{FileName: "file.txt", Purpose: "file", Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := manager.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	finishDone := make(chan error, 1)
	manager.mu.Lock()
	itemToFinish := manager.items[transfer.ID]
	manager.mu.Unlock()
	go func() { finishDone <- manager.finish(itemToFinish, StateSucceeded, "", "", "", nil) }()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		item := manager.items[transfer.ID]
		finishing, state := item.finishing, item.value.State
		manager.mu.Unlock()
		if finishing {
			if state != StateUploading {
				_ = tx.Rollback()
				t.Fatalf("terminal state was published before persistence: %q", state)
			}
			break
		}
		if time.Now().After(deadline) {
			_ = tx.Rollback()
			t.Fatal("finalization did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-finishDone; err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	state := manager.items[transfer.ID].value.State
	manager.mu.Unlock()
	if state != StateSucceeded {
		t.Fatalf("final state = %q, want succeeded", state)
	}
}
