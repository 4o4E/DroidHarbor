package leases

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ResourceFailure blocks future allocations while a human-owned platform
// resource remains uncertain. It deliberately leaves active leases untouched.
func (m *Manager) ResourceFailure(ctx context.Context, deviceID string) error {
	if ctx == nil || deviceID == "" {
		return ErrInvalidRequest
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	m.blockedDevices[deviceID] = struct{}{}
	m.pendingRecovery[deviceID] = struct{}{}
	m.resourceFailures[deviceID] = struct{}{}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	defer tx.Rollback()
	var allocation string
	if err = tx.QueryRowContext(ctx, `SELECT allocation_state FROM devices WHERE id=?`, deviceID).Scan(&allocation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			delete(m.blockedDevices, deviceID)
			delete(m.pendingRecovery, deviceID)
			delete(m.resourceFailures, deviceID)
			m.mu.Unlock()
			return ErrNotFound
		}
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	if allocation != "leased" && allocation != "quarantined" {
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET allocation_state='reclaiming',last_error=NULL WHERE id=? AND allocation_state=?`, deviceID, allocation); err != nil {
			m.mu.Unlock()
			return domain(ErrStorage, err)
		}
	}
	now := m.now().UTC()
	if err = recordOperation(ctx, tx, "system", m.bootID, deviceID, "", "capture.resource_failure", "pending", now, time.Time{}, "CLEANUP_UNCERTAIN"); err != nil {
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	if err = tx.Commit(); err != nil {
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	m.mu.Unlock()
	m.publishDeviceChange(deviceID, m.EventTask(deviceID))
	if allocation != "quarantined" {
		m.signalReclaims()
	}
	return nil
}
