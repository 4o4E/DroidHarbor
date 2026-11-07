package leases

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"droidharbor/internal/identity"
)

func (m *Manager) Claim(ctx context.Context, bearer string, request ClaimRequest) (Lease, error) {
	var empty Lease
	if ctx == nil || strings.TrimSpace(request.TaskID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || !validTaskSecret(request.TaskSecret) {
		return empty, ErrInvalidRequest
	}
	tags, err := canonicalTags(request.RequiredTags)
	if err != nil {
		return empty, err
	}
	request.RequiredTags = tags
	if !utf8.ValidString(request.TaskID) || !utf8.ValidString(request.IdempotencyKey) {
		return empty, ErrInvalidRequest
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return empty, ErrClosed
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, domain(ErrStorage, err)
	}
	defer tx.Rollback()
	principal, err := authenticate(ctx, tx, bearer)
	if err != nil {
		return empty, err
	}
	if principal.Kind != PrincipalCaller {
		return empty, ErrForbidden
	}
	if _, revoked := m.revokedCallers[principal.CallerID]; revoked {
		return empty, ErrUnauthenticated
	}
	secretHash := identity.Digest(request.TaskSecret)
	requestHash, err := claimRequestHash(request.TaskID, secretHash, tags)
	if err != nil {
		return empty, domain(ErrInvalidRequest, err)
	}

	existing, taskSecret, err := findIdempotentLease(ctx, tx, principal.CallerID, request.IdempotencyKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return empty, domain(ErrStorage, err)
	}
	if err == nil {
		if existing.externalTaskID != request.TaskID || subtle.ConstantTimeCompare(taskSecret, secretHash) != 1 || subtle.ConstantTimeCompare(existing.requestHash, requestHash[:]) != 1 {
			return empty, ErrConflict
		}
		if existing.lease.Status == LeaseActive {
			state := m.leases[existing.lease.ID]
			if state == nil {
				return empty, domain(ErrStorage, errors.New("active lease has no monotonic runtime state"))
			}
			if state.lease.Status != LeaseActive {
				if state.endErr != nil {
					return empty, state.endErr
				}
				return empty, ErrLeaseEnded
			}
			if now := m.now(); !now.Before(state.deadline) {
				if rollbackErr := tx.Rollback(); rollbackErr != nil {
					return empty, domain(ErrStorage, rollbackErr)
				}
				if _, endErr := m.endLocked(m.ctx, state, "expired", "system", m.bootID, now); endErr != nil {
					return empty, endErr
				}
				m.pendingRecovery[state.lease.DeviceID] = struct{}{}
				m.signalReclaims()
				return leaseSnapshot(state.lease), nil
			}
			return leaseSnapshot(state.lease), nil
		}
		return existing.lease, nil
	}

	var taskID string
	var storedHash []byte
	err = tx.QueryRowContext(ctx, `SELECT id,task_secret_hash FROM tasks WHERE caller_id=? AND external_task_id=?`, principal.CallerID, request.TaskID).Scan(&taskID, &storedHash)
	if err == nil {
		if subtle.ConstantTimeCompare(storedHash, secretHash) != 1 {
			return empty, ErrConflict
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		var otherTask string
		err = tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE task_secret_hash=?`, secretHash).Scan(&otherTask)
		if err == nil {
			return empty, ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return empty, domain(ErrStorage, err)
		}
	} else {
		return empty, domain(ErrStorage, err)
	}
	if taskID != "" {
		var activeLease string
		err = tx.QueryRowContext(ctx, `SELECT id FROM leases WHERE task_id=? AND status='active'`, taskID).Scan(&activeLease)
		if err == nil {
			return empty, ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return empty, domain(ErrStorage, err)
		}
	}

	links := append([]Link(nil), m.source.OnlineLinks()...)
	sort.Slice(links, func(i, j int) bool { return lessLink(links[i], links[j]) })
	linksByDevice := make(map[string][]Link, len(links))
	for _, link := range links {
		if link.DeviceID != "" && link.LinkID != "" {
			linksByDevice[link.DeviceID] = append(linksByDevice[link.DeviceID], link)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.generation FROM devices d WHERE d.allocation_state='available' AND d.maintenance_requested=0 AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.device_id=d.id AND l.status='active') ORDER BY d.id`)
	if err != nil {
		return empty, domain(ErrStorage, err)
	}
	type candidate struct {
		id         string
		generation int64
	}
	var candidates []candidate
	for rows.Next() {
		var current candidate
		if err = rows.Scan(&current.id, &current.generation); err != nil {
			rows.Close()
			return empty, domain(ErrStorage, err)
		}
		candidates = append(candidates, current)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return empty, domain(ErrStorage, err)
	}
	if err = rows.Close(); err != nil {
		return empty, domain(ErrStorage, err)
	}

	var selected candidate
	var selectedLink Link
	found := false
	for _, device := range candidates {
		if _, blocked := m.blockedDevices[device.id]; blocked {
			continue
		}
		deviceLinks := linksByDevice[device.id]
		if len(deviceLinks) == 0 {
			continue
		}
		tagMatch, tagErr := matchesTags(ctx, tx, device.id, tags)
		if tagErr != nil {
			return empty, domain(ErrStorage, tagErr)
		}
		if !tagMatch {
			continue
		}
		if device.generation == math.MaxInt64 {
			continue
		}
		selected, selectedLink, found = device, deviceLinks[0], true
		break
	}
	if !found {
		return empty, ErrUnavailable
	}

	now := m.now()
	deadline := now.Add(LeaseDuration)
	claimedAt := time.UnixMilli(now.UnixMilli()).UTC()
	expiresAt := time.UnixMilli(deadline.UnixMilli()).UTC()
	if !expiresAt.After(claimedAt) {
		return empty, ErrInvalidRequest
	}
	if taskID == "" {
		taskID = identity.Secret()
		if _, err = tx.ExecContext(ctx, `INSERT INTO tasks(id,caller_id,external_task_id,task_secret_hash,created_at) VALUES(?,?,?,?,?)`, taskID, principal.CallerID, request.TaskID, secretHash, claimedAt.UnixMilli()); err != nil {
			return empty, domain(ErrStorage, err)
		}
	}
	generation := selected.generation + 1
	leaseID := identity.Secret()
	if err = updateDeviceClaim(ctx, tx, selected, generation); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO leases(id,task_id,caller_id,device_id,generation,boot_id,status,claimed_at,expires_at,claim_idempotency_key,claim_request_hash) VALUES(?,?,?,?,?,?,'active',?,?,?,?)`, leaseID, taskID, principal.CallerID, selected.id, generation, m.bootID, claimedAt.UnixMilli(), expiresAt.UnixMilli(), request.IdempotencyKey, requestHash[:]); err != nil {
		return empty, domain(ErrStorage, err)
	}
	if err = recordOperation(ctx, tx, PrincipalTask, taskID, selected.id, leaseID, "lease.claim", "succeeded", claimedAt, claimedAt, "CLAIMED"); err != nil {
		return empty, domain(ErrStorage, err)
	}
	if err = tx.Commit(); err != nil {
		return empty, domain(ErrStorage, err)
	}

	lease := Lease{ID: leaseID, CallerID: principal.CallerID, TaskID: taskID, DeviceID: selected.id, BootID: m.bootID, Generation: generation, Status: LeaseActive, ClaimedAt: claimedAt, ExpiresAt: expiresAt}
	leaseCtx, cancel := context.WithCancel(m.ctx)
	state := &leaseState{lease: lease, deadline: deadline, link: selectedLink, ctx: leaseCtx, cancel: cancel, grants: make(map[uint64]context.CancelFunc)}
	m.leases[leaseID] = state
	m.installExpiry(state, deadline)
	m.setEventOwner(lease.DeviceID, lease.TaskID)
	m.publishLeaseChange(lease, false)
	m.signalExpiry()
	return lease, nil
}

func validTaskSecret(secret string) bool {
	if len(secret) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(secret)
	return err == nil && len(decoded) == 32
}

func canonicalTags(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	tags := make([]string, 0, len(values))
	for _, value := range values {
		tag := strings.TrimSpace(value)
		if tag == "" || !utf8.ValidString(tag) {
			return nil, ErrInvalidRequest
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags, nil
}

func claimRequestHash(taskID string, secretHash []byte, tags []string) ([32]byte, error) {
	payload := struct {
		TaskID       string   `json:"taskId"`
		SecretDigest string   `json:"secretDigest"`
		Tags         []string `json:"tags"`
	}{TaskID: taskID, SecretDigest: hex.EncodeToString(secretHash), Tags: tags}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

type idempotentLease struct {
	lease          Lease
	externalTaskID string
	requestHash    []byte
}

func findIdempotentLease(ctx context.Context, tx *sql.Tx, callerID, key string) (idempotentLease, []byte, error) {
	var result idempotentLease
	var claimed, expires int64
	var ended sql.NullInt64
	var reason sql.NullString
	var taskSecret []byte
	err := tx.QueryRowContext(ctx, `SELECT l.id,l.task_id,l.caller_id,l.device_id,l.boot_id,l.generation,l.status,l.claimed_at,l.expires_at,l.ended_at,l.end_reason,l.claim_request_hash,t.external_task_id,t.task_secret_hash FROM leases l JOIN tasks t ON t.id=l.task_id AND t.caller_id=l.caller_id WHERE l.caller_id=? AND l.claim_idempotency_key=?`, callerID, key).Scan(&result.lease.ID, &result.lease.TaskID, &result.lease.CallerID, &result.lease.DeviceID, &result.lease.BootID, &result.lease.Generation, &result.lease.Status, &claimed, &expires, &ended, &reason, &result.requestHash, &result.externalTaskID, &taskSecret)
	if err != nil {
		return idempotentLease{}, nil, err
	}
	result.lease.ClaimedAt = time.UnixMilli(claimed).UTC()
	result.lease.ExpiresAt = time.UnixMilli(expires).UTC()
	if ended.Valid {
		value := time.UnixMilli(ended.Int64).UTC()
		result.lease.EndedAt = &value
	}
	result.lease.EndReason = reason.String
	return result, taskSecret, nil
}

func matchesTags(ctx context.Context, tx *sql.Tx, deviceID string, required []string) (bool, error) {
	if len(required) == 0 {
		return true, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT tag FROM device_tags WHERE device_id=?`, deviceID)
	if err != nil {
		return false, err
	}
	present := make(map[string]struct{}, len(required))
	for rows.Next() {
		var tag string
		if err = rows.Scan(&tag); err != nil {
			rows.Close()
			return false, err
		}
		present[tag] = struct{}{}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	for _, tag := range required {
		if _, exists := present[tag]; !exists {
			return false, nil
		}
	}
	return true, nil
}

func updateDeviceClaim(ctx context.Context, tx *sql.Tx, current struct {
	id         string
	generation int64
}, generation int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE devices SET allocation_state='leased',generation=? WHERE id=? AND generation=? AND allocation_state='available' AND maintenance_requested=0`, generation, current.id, current.generation)
	if err != nil {
		return domain(ErrStorage, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain(ErrStorage, err)
	}
	if changed != 1 {
		return ErrUnavailable
	}
	return nil
}

func (m *Manager) expireLocked(_ context.Context) {
	now := m.now()
	for _, state := range m.leases {
		if state.lease.Status != LeaseActive {
			continue
		}
		ticket := state.expiry.Load()
		if ticket != nil && now.Before(ticket.deadline) {
			continue
		}
		if ticket != nil && state.expiry.CompareAndSwap(ticket, nil) {
			state.expired.Store(ticket)
			if timer := ticket.timer.Load(); timer != nil {
				timer.Stop()
			}
		}
		if state.expiry.Load() != nil {
			continue
		}
		state.lease.Status = LeaseEnded
		state.lease.EndReason = "expired"
		ended := now
		if expired := state.expired.Load(); expired != nil {
			ended = expired.deadline
		}
		state.lease.EndedAt = &ended
		m.closeGrantsLocked(state)
		m.publishLeaseChange(state.lease, true)
		m.signalExpiry()
	}
}
