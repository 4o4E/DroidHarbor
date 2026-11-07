package connections

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
)

func (m *Manager) watch(e *entry) {
	defer m.workers.Done()
	<-e.grant.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.closeEntry(ctx, e); err != nil {
		m.logger.Error("connection_cleanup_failed", "connectionId", e.id, "leaseId", e.lease.ID)
	}
}

func (m *Manager) closeEntry(ctx context.Context, e *entry) (resultErr error) {
	e.grant.Close()
	defer func() {
		if resultErr != nil {
			e.mu.Lock()
			if !e.cleaned {
				e.cleanupError = resultErr
			}
			e.mu.Unlock()
		}
	}()
	select {
	case <-e.initialized:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.cleanup.Lock()
	defer e.cleanup.Unlock()
	if e.cleaned {
		return nil
	}
	e.stopLink()
	e.mu.Lock()
	e.closing = true
	stream, finished := e.conn, e.finished
	e.mu.Unlock()
	// Close the actual socket before waiting for workers; neither a peer ACK nor
	// a stalled network write may keep revoked work alive during recovery.
	if stream != nil {
		if err := stream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	if finished != nil {
		select {
		case <-finished:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if e.persisted {
		if err := m.markCleaned(ctx, e.id, e.lease.TaskID, e.lease.BootID, e.lease.ID, e.metadata.OperationID); err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.cleanupError = nil
	e.cleaned = true
	e.mu.Unlock()
	m.mu.Lock()
	if m.entries[e.id] == e {
		delete(m.entries, e.id)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) markCleaned(ctx context.Context, id, owner, boot, leaseID, operationID string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return storageError(err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,lease_id,kind,status,created_at,finished_at,result_code,safe_details) SELECT ?,'system',?,device_id,?,'connection.close','succeeded',?,?,'CLOSED',? FROM owned_resources WHERE id=? AND owner_kind='task' AND owner_id=? AND boot_id=? AND kind=? AND state!='cleaned'`, identity.Secret(), boot, leaseID, now, now, `{"connectionId":"`+id+`"}`, id, owner, boot, Kind); err != nil {
		return storageError(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE owned_resources SET state='cleaned',cleanup_error=NULL WHERE id=? AND owner_kind='task' AND owner_id=? AND boot_id=? AND kind=?`, id, owner, boot, Kind)
	if err != nil {
		return storageError(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return storageError(err)
	}
	if changed != 1 {
		return storageError(errors.New("connection resource ownership changed"))
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operation_records SET status='failed',finished_at=?,result_code='CONNECTION_CLOSED' WHERE id=? AND status='pending'`, time.Now().UTC().UnixMilli(), operationID); err != nil {
		return storageError(err)
	}
	if err = tx.Commit(); err != nil {
		return storageError(err)
	}
	return nil
}

func (m *Manager) Delete(ctx context.Context, lease leases.Lease, id string) (View, error) {
	data, state, err := m.read(ctx, lease, id)
	if err != nil {
		return View{}, err
	}
	m.mu.Lock()
	e := m.entries[id]
	if e == nil {
		// Hold registration exclusion while disposing of an orphan journal row.
		// This kind owns only in-process sockets, never ADB-server forwards/files.
		if state != "cleaned" {
			err = m.markCleaned(ctx, id, lease.TaskID, lease.BootID, lease.ID, data.OperationID)
		}
		m.mu.Unlock()
		if err != nil {
			return View{}, err
		}
		return viewFor(lease, id, data, "cleaned"), nil
	}
	m.mu.Unlock()
	if err = m.closeEntry(ctx, e); err != nil {
		return View{}, err
	}
	return m.Get(ctx, lease, id)
}

// Recover is called only after the lease authority has revoked the device's AI
// grants. Current human/capture resources belong to other lifetimes and are not
// touched. The single platform process lock proves previous-process sockets
// cannot remain alive; absence from the live map is meaningful for this kind.
func (m *Manager) Recover(ctx context.Context, deviceID string) error {
	m.mu.Lock()
	pending := make([]*entry, 0)
	for _, e := range m.entries {
		if e.lease.DeviceID == deviceID {
			pending = append(pending, e)
		}
	}
	m.mu.Unlock()
	for _, e := range pending {
		if e.grant.Err() == nil {
			return leases.ErrConflict
		}
		if err := m.closeEntry(ctx, e); err != nil {
			return err
		}
	}
	rows, err := m.db.QueryContext(ctx, `SELECT id,owner_id,boot_id,locator FROM owned_resources WHERE device_id=? AND owner_kind='task' AND kind=? AND state!='cleaned'`, deviceID, Kind)
	if err != nil {
		return storageError(err)
	}
	type orphan struct{ id, owner, boot, locator string }
	var remaining []orphan
	for rows.Next() {
		var item orphan
		if err = rows.Scan(&item.id, &item.owner, &item.boot, &item.locator); err != nil {
			_ = rows.Close()
			return storageError(err)
		}
		remaining = append(remaining, item)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return storageError(err)
	}
	for _, item := range remaining {
		var data metadata
		if err = json.Unmarshal([]byte(item.locator), &data); err != nil {
			return storageError(err)
		}
		if data.LeaseID == "" || data.OperationID == "" || data.Link.DeviceID != deviceID {
			return storageError(errors.New("invalid connection recovery journal"))
		}
		m.mu.Lock()
		e := m.entries[item.id]
		if e == nil {
			err = m.markCleaned(ctx, item.id, item.owner, item.boot, data.LeaseID, data.OperationID)
		}
		m.mu.Unlock()
		if e != nil {
			if e.grant.Err() == nil {
				return leases.ErrConflict
			}
			err = m.closeEntry(ctx, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.workers.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	var failures []error
	for _, e := range m.entries {
		e.mu.Lock()
		err := e.cleanupError
		e.mu.Unlock()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
