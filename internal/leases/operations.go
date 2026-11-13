package leases

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

func (m *Manager) Get(ctx context.Context, principal Principal, leaseID string) (Lease, error) {
	var empty Lease
	if ctx == nil || leaseID == "" {
		return empty, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return empty, ErrClosed
	}
	m.expireLocked(ctx)
	lease, err := readLease(ctx, m.db, leaseID)
	if err != nil {
		return empty, err
	}
	if err = m.authorizeLeaseLocked(ctx, principal, lease, true); err != nil {
		return empty, err
	}
	m.expireLocked(ctx)
	if state := m.leases[leaseID]; state != nil {
		return leaseSnapshot(state.lease), state.endErr
	}
	return lease, nil
}

func (m *Manager) Renew(ctx context.Context, principal Principal, leaseID string) (Lease, error) {
	var empty Lease
	if ctx == nil || leaseID == "" {
		return empty, ErrInvalidRequest
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return empty, ErrClosed
	}
	m.expireLocked(ctx)
	persisted, err := readLease(ctx, m.db, leaseID)
	if err != nil {
		m.mu.Unlock()
		return empty, err
	}
	if err = m.authorizeLeaseLocked(ctx, principal, persisted, false); err != nil {
		m.mu.Unlock()
		return empty, err
	}
	state := m.leases[leaseID]
	stateActive := state != nil && state.lease.Status == LeaseActive
	var stateErr error
	if state != nil {
		stateErr = state.endErr
	}
	m.mu.Unlock()
	if persisted.Status != LeaseActive || !stateActive {
		if stateErr != nil {
			return empty, stateErr
		}
		return empty, ErrLeaseEnded
	}
	state.opMu.Lock()
	defer state.opMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return empty, ErrClosed
	}
	m.expireLocked(ctx)
	persisted, err = readLease(ctx, m.db, leaseID)
	if err != nil {
		m.mu.Unlock()
		return empty, err
	}
	if err = m.authorizeLeaseLocked(ctx, principal, persisted, false); err != nil {
		m.mu.Unlock()
		return empty, err
	}
	if m.leases[leaseID] != state || persisted.Status != LeaseActive || state.lease.Status != LeaseActive {
		stateErr = state.endErr
		m.mu.Unlock()
		if stateErr != nil {
			return empty, stateErr
		}
		return empty, ErrLeaseEnded
	}
	oldTicket := state.expiry.Load()
	now := m.now()
	if oldTicket == nil || !now.Before(oldTicket.deadline) {
		m.mu.Unlock()
		return empty, ErrLeaseEnded
	}
	deadline := now.Add(LeaseDuration)
	expiresAt := time.UnixMilli(deadline.UnixMilli()).UTC()
	if !expiresAt.After(time.UnixMilli(now.UnixMilli()).UTC()) {
		m.mu.Unlock()
		return empty, ErrInvalidRequest
	}
	m.mu.Unlock()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, domain(ErrStorage, err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE leases SET expires_at=? WHERE id=? AND status='active' AND boot_id=? AND generation=?`, expiresAt.UnixMilli(), leaseID, m.bootID, state.lease.Generation)
	if err != nil {
		rollbackErr := tx.Rollback()
		return empty, domain(ErrStorage, errors.Join(err, rollbackErr))
	}
	changed, err := result.RowsAffected()
	if err != nil {
		rollbackErr := tx.Rollback()
		return empty, domain(ErrStorage, errors.Join(err, rollbackErr))
	}
	if changed != 1 {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil {
			return empty, domain(ErrStorage, rollbackErr)
		}
		return empty, ErrLeaseEnded
	}
	if err = tx.Commit(); err != nil {
		return empty, domain(ErrStorage, err)
	}

	// Publish the ticket and its public snapshot under the same authority lock.
	// Otherwise an idempotent claim can observe the old deadline after the new
	// ticket is installed and incorrectly terminate a successful renewal.
	m.mu.Lock()
	if m.closed || m.leases[leaseID] != state || (state.lease.Status != LeaseActive && state.lease.EndReason != "expired") {
		m.mu.Unlock()
		return empty, ErrLeaseEnded
	}
	afterCommit := m.now()
	if !afterCommit.Before(oldTicket.deadline) {
		m.mu.Unlock()
		if state.expiry.CompareAndSwap(oldTicket, nil) {
			state.expired.Store(oldTicket)
			state.cancel()
			m.signalExpiry()
		}
		if err = m.persistRenewFailure(state, principal, "expired", "RENEW_EXPIRED_DURING_COMMIT", now.UTC()); err != nil {
			return empty, err
		}
		if err = m.recoverDevice(ctx, state.lease.DeviceID); err != nil {
			return empty, err
		}
		return empty, ErrLeaseEnded
	}
	newTicket := &expiryTicket{deadline: deadline}
	if !state.expiry.CompareAndSwap(oldTicket, newTicket) {
		m.mu.Unlock()
		if !m.now().Before(oldTicket.deadline) {
			if err = m.persistRenewFailure(state, principal, "expired", "RENEW_EXPIRED_DURING_COMMIT", now.UTC()); err != nil {
				return empty, err
			}
			if err = m.recoverDevice(ctx, state.lease.DeviceID); err != nil {
				return empty, err
			}
		}
		return empty, ErrLeaseEnded
	}
	if timer := oldTicket.timer.Load(); timer != nil {
		timer.Stop()
	}
	state.expired.Store(nil)
	m.startExpiryTimer(state, newTicket)
	state.deadline = deadline
	state.lease.ExpiresAt = expiresAt
	renewed := leaseSnapshot(state.lease)
	m.mu.Unlock()
	m.publishLeaseChange(renewed, false)
	if err = m.recordRenewSuccess(ctx, principal, state, now); err != nil {
		m.logFailure("lease_renew_record", err)
		if errors.Is(err, ErrLeaseEnded) {
			return empty, ErrLeaseEnded
		}
		if persistErr := m.persistRenewFailure(state, principal, "renew_failed", "RENEW_RECORD_FAILED", now.UTC()); persistErr != nil {
			return empty, errors.Join(domain(ErrStorage, err), persistErr)
		}
		if recoveryErr := m.recoverDevice(ctx, state.lease.DeviceID); recoveryErr != nil {
			return empty, recoveryErr
		}
		return empty, domain(ErrStorage, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || state.lease.Status != LeaseActive || state.expiry.Load() != newTicket {
		return empty, ErrLeaseEnded
	}
	return leaseSnapshot(state.lease), nil
}

func (m *Manager) recordRenewSuccess(ctx context.Context, principal Principal, state *leaseState, now time.Time) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return domain(ErrStorage, err)
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM leases WHERE id=? AND boot_id=? AND generation=?`, state.lease.ID, m.bootID, state.lease.Generation).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseEnded
		}
		return domain(ErrStorage, err)
	}
	if status != LeaseActive {
		return ErrLeaseEnded
	}
	if err = recordOperation(ctx, tx, PrincipalTask, principal.ID, state.lease.DeviceID, state.lease.ID, "lease.renew", "succeeded", now.UTC(), now.UTC(), "RENEWED"); err != nil {
		return domain(ErrStorage, err)
	}
	if err = tx.Commit(); err != nil {
		return domain(ErrStorage, err)
	}
	return nil
}

func (m *Manager) persistRenewFailure(state *leaseState, principal Principal, reason, resultCode string, created time.Time) error {
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return ErrClosed
	}
	if state.lease.Status != LeaseActive && (state.lease.Status != LeaseEnded || reason != "expired" || state.lease.EndReason != "expired") {
		m.mu.Unlock()
		return ErrLeaseEnded
	}
	now := m.now().UTC()
	wasActive := state.lease.Status == LeaseActive
	if wasActive {
		state.lease.Status = LeaseEnded
		state.lease.EndReason = reason
		state.lease.EndedAt = &now
	} else if state.lease.EndedAt != nil {
		now = *state.lease.EndedAt
	}
	stopExpiryTicket(state)
	m.closeGrantsLocked(state)
	if wasActive {
		m.publishLeaseChange(state.lease, true)
	}
	tx, err := m.db.BeginTx(m.ctx, nil)
	if err != nil {
		err = domain(ErrStorage, err)
		state.endErr = err
		m.mu.Unlock()
		m.logFailure("lease_renew_termination", err)
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(m.ctx, `UPDATE leases SET status='ended',ended_at=?,end_reason=? WHERE id=? AND status='active' AND boot_id=? AND generation=?`, now.UnixMilli(), reason, state.lease.ID, m.bootID, state.lease.Generation)
	if err == nil {
		var changed int64
		changed, err = result.RowsAffected()
		if err == nil && changed != 1 {
			err = ErrLeaseEnded
		}
	}
	if err == nil {
		_, err = tx.ExecContext(m.ctx, `UPDATE devices SET allocation_state='reclaiming' WHERE id=? AND allocation_state='leased'`, state.lease.DeviceID)
	}
	if err == nil {
		err = recordOperation(m.ctx, tx, PrincipalTask, principal.ID, state.lease.DeviceID, state.lease.ID, "lease.renew", "failed", created.UTC(), now, resultCode)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		err = domain(ErrStorage, err)
		state.endErr = err
		m.mu.Unlock()
		m.logFailure("lease_renew_termination", err)
		return err
	}
	delete(m.leases, state.lease.ID)
	m.mu.Unlock()
	return nil
}

func (m *Manager) Release(ctx context.Context, principal Principal, leaseID string) (Lease, error) {
	var empty Lease
	if ctx == nil || leaseID == "" {
		return empty, ErrInvalidRequest
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return empty, ErrClosed
	}
	m.expireLocked(ctx)
	persisted, err := readLease(ctx, m.db, leaseID)
	if err != nil {
		m.mu.Unlock()
		return empty, err
	}
	if err = m.authorizeLeaseLocked(ctx, principal, persisted, true); err != nil {
		m.mu.Unlock()
		return empty, err
	}
	state := m.leases[leaseID]
	if state != nil && state.lease.Status == LeaseEnded {
		deviceID, endErr := m.endLocked(ctx, state, state.lease.EndReason, principal.Kind, principal.ID, m.now())
		result := leaseSnapshot(state.lease)
		m.mu.Unlock()
		if endErr != nil {
			return result, endErr
		}
		if deviceID != "" {
			if err = m.recoverDevice(ctx, deviceID); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	if persisted.Status == LeaseActive {
		if state == nil {
			m.mu.Unlock()
			return empty, domain(ErrStorage, errors.New("active lease has no runtime state"))
		}
		deviceID, endErr := m.endLocked(ctx, state, "released", principal.Kind, principal.ID, m.now())
		result := leaseSnapshot(state.lease)
		m.mu.Unlock()
		if endErr != nil {
			return result, endErr
		}
		if deviceID != "" {
			if err = m.recoverDevice(ctx, deviceID); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	m.mu.Unlock()
	return persisted, nil
}

func (m *Manager) RevokeCaller(ctx context.Context, callerID, adminSessionID string) error {
	if ctx == nil || callerID == "" || adminSessionID == "" {
		return ErrInvalidRequest
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	m.revokedCallers[callerID] = struct{}{}
	now := m.now().UTC()
	var runtimeLeases []*leaseState
	for _, state := range m.leases {
		if state.lease.CallerID == callerID && state.lease.Status == LeaseActive {
			state.lease.Status = LeaseEnded
			state.lease.EndReason = "caller_revoked"
			ended := now
			state.lease.EndedAt = &ended
			state.endErr = nil
			stopExpiryTicket(state)
			m.closeGrantsLocked(state)
			m.publishLeaseChange(state.lease, true)
			runtimeLeases = append(runtimeLeases, state)
		}
	}
	fail := func(err error) error {
		for _, state := range runtimeLeases {
			state.endErr = err
		}
		m.mu.Unlock()
		if errors.Is(err, ErrStorage) {
			m.logFailure("caller_revocation", err)
		}
		return err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	defer tx.Rollback()
	var revokedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT revoked_at FROM caller_credentials WHERE id=?`, callerID).Scan(&revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNotFound)
	}
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	newlyRevoked := !revokedAt.Valid
	if _, err = tx.ExecContext(ctx, `UPDATE caller_credentials SET revoked_at=coalesce(revoked_at,?) WHERE id=?`, now.UnixMilli(), callerID); err != nil {
		return fail(domain(ErrStorage, err))
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,task_id,device_id FROM leases WHERE caller_id=? AND status='active' ORDER BY device_id,id`, callerID)
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	type revokedLease struct{ id, taskID, deviceID string }
	var revoked []revokedLease
	for rows.Next() {
		var item revokedLease
		if err = rows.Scan(&item.id, &item.taskID, &item.deviceID); err != nil {
			return fail(domain(ErrStorage, errors.Join(err, rows.Close())))
		}
		revoked = append(revoked, item)
	}
	queryErr := rows.Err()
	closeErr := rows.Close()
	if queryErr != nil || closeErr != nil {
		return fail(domain(ErrStorage, errors.Join(queryErr, closeErr)))
	}
	for _, item := range revoked {
		if _, err = tx.ExecContext(ctx, `UPDATE leases SET status='ended',ended_at=?,end_reason='caller_revoked' WHERE id=? AND status='active'`, now.UnixMilli(), item.id); err != nil {
			return fail(domain(ErrStorage, err))
		}
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET allocation_state='reclaiming' WHERE id=? AND allocation_state='leased'`, item.deviceID); err != nil {
			return fail(domain(ErrStorage, err))
		}
		if err = recordOperation(ctx, tx, PrincipalAdmin, adminSessionID, item.deviceID, item.id, "lease.caller_revoked", "succeeded", now, now, "REVOKED"); err != nil {
			return fail(domain(ErrStorage, err))
		}
		if state := m.leases[item.id]; state != nil {
			state.lease.Status = LeaseEnded
			state.lease.EndReason = "caller_revoked"
			ended := now
			state.lease.EndedAt = &ended
		}
	}
	if newlyRevoked {
		details, marshalErr := json.Marshal(struct {
			CallerID string `json:"callerId"`
		}{CallerID: callerID})
		if marshalErr != nil {
			return fail(domain(ErrStorage, marshalErr))
		}
		if err = recordOperationDetails(ctx, tx, PrincipalAdmin, adminSessionID, "", "", "caller.revoke", "succeeded", now, now, "REVOKED", string(details)); err != nil {
			return fail(domain(ErrStorage, err))
		}
	}
	if err = tx.Commit(); err != nil {
		return fail(domain(ErrStorage, err))
	}
	for _, item := range revoked {
		if state := m.leases[item.id]; state != nil {
			state.endErr = nil
			delete(m.leases, item.id)
		}
	}
	devices := make([]string, 0, len(revoked))
	seen := make(map[string]struct{}, len(revoked))
	for _, item := range revoked {
		if _, found := seen[item.deviceID]; !found {
			seen[item.deviceID] = struct{}{}
			devices = append(devices, item.deviceID)
		}
	}
	m.mu.Unlock()
	sort.Strings(devices)
	var recoveryErrors []error
	for _, deviceID := range devices {
		if err = m.recoverDevice(ctx, deviceID); err != nil {
			recoveryErrors = append(recoveryErrors, err)
		}
	}
	return errors.Join(recoveryErrors...)
}

// SetMaintenance requires confirmation of the current AI lease before revoking it.
// DeviceSource.Recover must preserve independent human-session resources.
func (m *Manager) SetMaintenance(ctx context.Context, deviceID string, requested bool, adminSessionID string, confirmedGeneration *int64) error {
	if ctx == nil || deviceID == "" || adminSessionID == "" || (!requested && confirmedGeneration != nil) {
		return ErrInvalidRequest
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	now := m.now().UTC()
	var runtimeLeases []*leaseState
	if requested {
		// Bind confirmation to this lease generation: a new task must receive
		// a new confirmation, even if the dialog was already open.
		for _, state := range m.leases {
			if state.lease.DeviceID == deviceID && state.lease.Status == LeaseActive {
				if confirmedGeneration == nil || *confirmedGeneration != state.lease.Generation {
					m.mu.Unlock()
					return ErrAllocationConfirmation
				}
				runtimeLeases = append(runtimeLeases, state)
			}
		}
		m.blockedDevices[deviceID] = struct{}{}
		for _, state := range runtimeLeases {
			state.lease.Status = LeaseEnded
			state.lease.EndReason = "maintenance_requested"
			ended := now
			state.lease.EndedAt = &ended
			stopExpiryTicket(state)
			m.closeGrantsLocked(state)
			m.publishLeaseChange(state.lease, true)
		}
	}
	fail := func(err error) error {
		for _, state := range runtimeLeases {
			state.endErr = err
		}
		if _, blocked := m.blockedDevices[deviceID]; blocked {
			m.maintenanceFailures[deviceID] = struct{}{}
		}
		m.mu.Unlock()
		return err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	defer tx.Rollback()
	var allocationState string
	if err = tx.QueryRowContext(ctx, `SELECT allocation_state FROM devices WHERE id=?`, deviceID).Scan(&allocationState); errors.Is(err, sql.ErrNoRows) {
		delete(m.blockedDevices, deviceID)
		return fail(ErrNotFound)
	} else if err != nil {
		return fail(domain(ErrStorage, err))
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,task_id FROM leases WHERE device_id=? AND status='active' ORDER BY id`, deviceID)
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	type activeLease struct{ id, taskID string }
	var active []activeLease
	for rows.Next() {
		var item activeLease
		if err = rows.Scan(&item.id, &item.taskID); err != nil {
			rows.Close()
			return fail(domain(ErrStorage, err))
		}
		active = append(active, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return fail(domain(ErrStorage, err))
	}
	if err = rows.Close(); err != nil {
		return fail(domain(ErrStorage, err))
	}
	if !requested && len(active) != 0 {
		m.mu.Unlock()
		return ErrConflict
	}
	m.blockedDevices[deviceID] = struct{}{}
	for _, item := range active {
		if _, err = tx.ExecContext(ctx, `UPDATE leases SET status='ended',ended_at=?,end_reason='maintenance_requested' WHERE id=? AND status='active'`, now.UnixMilli(), item.id); err != nil {
			return fail(domain(ErrStorage, err))
		}
		if err = recordOperation(ctx, tx, PrincipalAdmin, adminSessionID, deviceID, item.id, "lease.maintenance_requested", "succeeded", now, now, "REVOKED"); err != nil {
			return fail(domain(ErrStorage, err))
		}
		if state := m.leases[item.id]; state != nil {
			state.lease.Status = LeaseEnded
			state.lease.EndReason = "maintenance_requested"
			ended := now
			state.lease.EndedAt = &ended
			state.endErr = nil
		}
	}
	maintenanceValue := 0
	resultCode := "MAINTENANCE_CLEARED"
	if requested {
		maintenanceValue = 1
		resultCode = "MAINTENANCE_REQUESTED"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET maintenance_requested=?,allocation_state='reclaiming',last_error=NULL WHERE id=?`, maintenanceValue, deviceID); err != nil {
		return fail(domain(ErrStorage, err))
	}
	if err = recordOperation(ctx, tx, PrincipalAdmin, adminSessionID, deviceID, "", "device.maintenance", "pending", now, time.Time{}, resultCode); err != nil {
		return fail(domain(ErrStorage, err))
	}
	if err = tx.Commit(); err != nil {
		return fail(domain(ErrStorage, err))
	}
	delete(m.maintenanceFailures, deviceID)
	for _, item := range active {
		delete(m.leases, item.id)
	}
	m.mu.Unlock()
	m.publishDeviceChange(deviceID, m.EventTask(deviceID))
	return m.recoverDevice(ctx, deviceID)
}

func (m *Manager) retryUndurableTermination(ctx context.Context, deviceID string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	var states []*leaseState
	for _, state := range m.leases {
		if state.lease.DeviceID == deviceID && state.lease.Status == LeaseEnded &&
			(state.lease.EndReason == "expired" || state.lease.EndReason == "released" || state.lease.EndReason == "renew_failed") {
			states = append(states, state)
		}
	}
	m.mu.Unlock()
	sort.Slice(states, func(i, j int) bool { return states[i].lease.ID < states[j].lease.ID })
	for _, state := range states {
		state.opMu.Lock()
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			state.opMu.Unlock()
			return ErrClosed
		}
		if m.leases[state.lease.ID] == state && state.lease.Status == LeaseEnded &&
			(state.lease.EndReason == "expired" || state.lease.EndReason == "released" || state.lease.EndReason == "renew_failed") {
			endedAt := m.now()
			if state.lease.EndedAt != nil {
				endedAt = *state.lease.EndedAt
			}
			if _, err := m.endLocked(ctx, state, state.lease.EndReason, "system", m.bootID, endedAt); err != nil {
				m.mu.Unlock()
				state.opMu.Unlock()
				return err
			}
		}
		m.mu.Unlock()
		state.opMu.Unlock()
	}
	return nil
}

// RetryRecovery explicitly rechecks a device left reclaiming or quarantined,
// or a non-leased device still blocked by an undurable resource failure.
// Callers must authorize this action as an administrator or confirmed
// connection-recovery event before invoking it.
func (m *Manager) RetryRecovery(ctx context.Context, deviceID string) error {
	if ctx == nil || deviceID == "" {
		return ErrInvalidRequest
	}
	if err := m.retryUndurableTermination(ctx, deviceID); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT allocation_state FROM devices WHERE id=?`, deviceID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		m.mu.Unlock()
		return ErrNotFound
	} else if err != nil {
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	_, retryResourceFailure := m.resourceFailures[deviceID]
	_, maintenanceFailed := m.maintenanceFailures[deviceID]
	recoverBlockedResource := retryResourceFailure && !maintenanceFailed && (state == "available" || state == "maintenance")
	if state != "reclaiming" && state != "quarantined" && !recoverBlockedResource {
		m.mu.Unlock()
		return ErrConflict
	}
	if state == "quarantined" || recoverBlockedResource {
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET allocation_state='reclaiming',last_error=NULL WHERE id=? AND allocation_state=?`, deviceID, state); err != nil {
			m.mu.Unlock()
			return domain(ErrStorage, err)
		}
	}
	now := m.now().UTC()
	if err = recordOperation(ctx, tx, "system", m.bootID, deviceID, "", "lease.recovery_retry", "pending", now, time.Time{}, "RETRY_REQUESTED"); err != nil {
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	if err = tx.Commit(); err != nil {
		m.mu.Unlock()
		return domain(ErrStorage, err)
	}
	m.mu.Unlock()
	return m.recoverDevice(ctx, deviceID)
}

func (m *Manager) Grant(ctx context.Context, principal Principal, leaseID string) (*Grant, error) {
	if ctx == nil || leaseID == "" {
		return nil, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	m.expireLocked(ctx)
	persisted, err := readLease(ctx, m.db, leaseID)
	if err != nil {
		return nil, err
	}
	if err = m.authorizeLeaseLocked(ctx, principal, persisted, false); err != nil {
		return nil, err
	}
	m.expireLocked(ctx)
	state := m.leases[leaseID]
	if persisted.Status != LeaseActive || state == nil || state.lease.Status != LeaseActive {
		if state != nil && state.endErr != nil {
			return nil, state.endErr
		}
		return nil, ErrLeaseEnded
	}
	now := m.now()
	ticket := state.expiry.Load()
	if ticket == nil || !now.Before(ticket.deadline) {
		m.expireLocked(ctx)
		return nil, ErrLeaseEnded
	}
	links := m.source.OnlineLinks()
	var current Link
	found := false
	for _, link := range links {
		if link.DeviceID == state.lease.DeviceID && link.LinkID == state.link.LinkID {
			current, found = link, true
			break
		}
	}
	if !found {
		cancelGrantContexts(state)
		return nil, ErrDeviceOffline
	}
	if !sameLink(current, state.link) {
		cancelGrantContexts(state)
		state.link = current
	}
	expirationCheck := m.now()
	if state.expiry.Load() != ticket || !expirationCheck.Before(ticket.deadline) {
		m.expireLocked(ctx)
		return nil, ErrLeaseEnded
	}
	grantCtx, err := m.leaseContext(ctx, state)
	if err != nil {
		return nil, err
	}
	if state.expiry.Load() != ticket || !m.now().Before(ticket.deadline) {
		cancelGrantContexts(state)
		m.expireLocked(ctx)
		return nil, ErrLeaseEnded
	}
	return grantCtx, nil
}

func (m *Manager) Check(leaseID string, generation int64, bootID string) error {
	if leaseID == "" || generation < 1 || bootID == "" {
		return ErrForbidden
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	m.expireLocked(m.ctx)
	state := m.leases[leaseID]
	if state == nil {
		persisted, err := readLease(m.ctx, m.db, leaseID)
		m.mu.Unlock()
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrForbidden
			}
			return err
		}
		if persisted.Generation == generation && persisted.BootID == bootID && bootID == m.bootID && persisted.Status == LeaseEnded {
			return ErrLeaseEnded
		}
		if persisted.Generation == generation && persisted.BootID == bootID && persisted.Status == LeaseActive {
			return domain(ErrStorage, errors.New("active lease is missing runtime state"))
		}
		return ErrForbidden
	}
	if state.lease.Generation != generation || state.lease.BootID != bootID || bootID != m.bootID {
		m.mu.Unlock()
		return ErrForbidden
	}
	if state.lease.Status != LeaseActive {
		endErr := state.endErr
		m.mu.Unlock()
		if endErr != nil {
			return endErr
		}
		return ErrLeaseEnded
	}
	if _, revoked := m.revokedCallers[state.lease.CallerID]; revoked {
		m.mu.Unlock()
		return ErrForbidden
	}
	ticket := state.expiry.Load()
	now := m.now()
	if ticket == nil || !now.Before(ticket.deadline) {
		m.expireLocked(m.ctx)
		m.mu.Unlock()
		return ErrLeaseEnded
	}
	current := m.source.HasLink(state.link)
	expirationCheck := m.now()
	if state.expiry.Load() != ticket || !expirationCheck.Before(ticket.deadline) {
		m.expireLocked(m.ctx)
		m.mu.Unlock()
		return ErrLeaseEnded
	}
	if !current {
		cancelGrantContexts(state)
		m.mu.Unlock()
		return ErrDeviceOffline
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) authorizeLeaseLocked(ctx context.Context, principal Principal, lease Lease, allowAdmin bool) error {
	if principal.Kind == PrincipalAdmin && allowAdmin && principal.ID != "" {
		return nil
	}
	if principal.Kind != PrincipalTask || principal.ID == "" || principal.CallerID == "" || lease.TaskID != principal.ID || lease.CallerID != principal.CallerID {
		return ErrForbidden
	}
	if _, revoked := m.revokedCallers[principal.CallerID]; revoked {
		return ErrUnauthenticated
	}
	var active int
	err := m.db.QueryRowContext(ctx, `SELECT 1 FROM caller_credentials WHERE id=? AND revoked_at IS NULL`, principal.CallerID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthenticated
	}
	if err != nil {
		return domain(ErrStorage, err)
	}
	return nil
}

func readLease(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, leaseID string) (Lease, error) {
	var lease Lease
	var claimed, expires int64
	var ended sql.NullInt64
	var reason sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id,caller_id,task_id,device_id,boot_id,generation,status,claimed_at,expires_at,ended_at,end_reason FROM leases WHERE id=?`, leaseID).Scan(&lease.ID, &lease.CallerID, &lease.TaskID, &lease.DeviceID, &lease.BootID, &lease.Generation, &lease.Status, &claimed, &expires, &ended, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	if err != nil {
		return Lease{}, domain(ErrStorage, err)
	}
	lease.ClaimedAt = time.UnixMilli(claimed).UTC()
	lease.ExpiresAt = time.UnixMilli(expires).UTC()
	if ended.Valid {
		value := time.UnixMilli(ended.Int64).UTC()
		lease.EndedAt = &value
	}
	lease.EndReason = reason.String
	return lease, nil
}

func cancelGrantContexts(state *leaseState) {
	state.grantsMu.Lock()
	defer state.grantsMu.Unlock()
	for id, cancel := range state.grants {
		cancel()
		delete(state.grants, id)
	}
}
