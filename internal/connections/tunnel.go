package connections

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/adbgateway"
	"droidharbor/internal/leases"
)

// Tunnel is reserved before HTTP Upgrade. Its owner must call Close after Serve
// returns, or after a failed Upgrade; cleanup waits for that completion barrier.
type Tunnel struct {
	manager    *Manager
	entry      *entry
	finishOnce sync.Once
}

func (m *Manager) Begin(ctx context.Context, lease leases.Lease, id string) (*Tunnel, error) {
	if _, _, err := m.read(ctx, lease, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	e := m.entries[id]
	m.mu.Unlock()
	if e == nil {
		return nil, ErrClosed
	}
	select {
	case <-e.initialized:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	e.mu.Lock()
	if e.used {
		e.mu.Unlock()
		return nil, ErrUsed
	}
	if e.closing || e.grant.Err() != nil {
		e.mu.Unlock()
		return nil, ErrClosed
	}
	if err := e.grant.Check(); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	e.used = true
	e.finished = make(chan struct{})
	e.mu.Unlock()
	return &Tunnel{manager: m, entry: e}, nil
}

func (t *Tunnel) Context() context.Context { return t.entry.grant }

func (t *Tunnel) Serve(conn net.Conn) error {
	e := t.entry
	conn = adb.GuardWrites(conn, e.grant.Check)
	e.mu.Lock()
	if e.closing || e.grant.Err() != nil {
		e.mu.Unlock()
		_ = conn.Close()
		return ErrClosed
	}
	e.conn = conn
	e.mu.Unlock()
	result, err := t.manager.db.ExecContext(e.grant, `UPDATE owned_resources SET state='active' WHERE id=? AND state='pending'`, e.id)
	if err != nil {
		return storageError(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return storageError(err)
	}
	if changed != 1 {
		return storageError(errors.New("connection state changed before tunnel activation"))
	}
	return adbgateway.Serve(e.grant, conn, adbgateway.Session{
		PublicKey: e.key,
		Features:  e.metadata.Features,
		Check:     e.grant.Check,
		Open: func(ctx context.Context, service string) (net.Conn, error) {
			return t.manager.adb.Open(ctx, e.grant.Link().TransportID, service, e.grant.Check)
		},
	})
}

func (t *Tunnel) Close() error {
	t.finishOnce.Do(func() { close(t.entry.finished) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return t.manager.closeEntry(ctx, t.entry)
}
