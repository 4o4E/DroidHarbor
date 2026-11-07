package leases

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"droidharbor/internal/events"
	"droidharbor/internal/identity"
)

type expiryTicket struct {
	deadline time.Time
	timer    atomic.Pointer[time.Timer]
}

type leaseState struct {
	lease    Lease
	deadline time.Time
	link     Link
	ctx      context.Context
	cancel   context.CancelFunc
	grantsMu sync.Mutex
	grants   map[uint64]context.CancelFunc
	endErr   error
	opMu     sync.Mutex
	expiry   atomic.Pointer[expiryTicket]
	expired  atomic.Pointer[expiryTicket]
}

type recoveryCall struct {
	done chan struct{}
	err  error
}

// Manager is the single in-process authority for lease state and grants.
type Manager struct {
	db     *sql.DB
	source DeviceSource
	logger *slog.Logger
	events atomic.Pointer[events.Bus]
	owners sync.Map // deviceID -> taskID; notification routing only

	bootID              string
	mu                  sync.Mutex
	closed              bool
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	reclaimDone         chan struct{}
	wake                chan struct{}
	reclaimWake         chan struct{}
	recoverySlots       chan struct{}
	recoveryWG          sync.WaitGroup
	now                 func() time.Time
	leases              map[string]*leaseState
	revokedCallers      map[string]struct{}
	recovering          map[string]*recoveryCall
	pendingRecovery     map[string]struct{}
	blockedDevices      map[string]struct{}
	resourceFailures    map[string]struct{}
	maintenanceFailures map[string]struct{}
	nextGrant           uint64
}

func New(ctx context.Context, db *sql.DB, source DeviceSource, logger *slog.Logger) (*Manager, error) {

	if ctx == nil || db == nil || source == nil || logger == nil {
		return nil, domain(ErrInvalidRequest, errors.New("context, database, device source, and logger are required"))
	}
	bootID := identity.Secret()
	started := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, domain(ErrStorage, err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id, task_id, caller_id, device_id FROM leases WHERE status='active' ORDER BY device_id,id`)
	if err != nil {
		return nil, domain(ErrStorage, err)
	}
	type previousLease struct{ id, taskID, callerID, deviceID string }
	var old []previousLease
	for rows.Next() {
		var lease previousLease
		if err = rows.Scan(&lease.id, &lease.taskID, &lease.callerID, &lease.deviceID); err != nil {
			rows.Close()
			return nil, domain(ErrStorage, err)
		}
		old = append(old, lease)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, domain(ErrStorage, err)
	}
	if err = rows.Close(); err != nil {
		return nil, domain(ErrStorage, err)
	}
	for _, lease := range old {
		if _, err = tx.ExecContext(ctx, `UPDATE leases SET status='ended',ended_at=?,end_reason='platform_restart' WHERE id=? AND status='active'`, started.UnixMilli(), lease.id); err != nil {
			return nil, domain(ErrStorage, err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET allocation_state='reclaiming' WHERE id=?`, lease.deviceID); err != nil {
			return nil, domain(ErrStorage, err)
		}
		if err = recordOperation(ctx, tx, "system", bootID, lease.deviceID, lease.id, "lease.platform_restart", "succeeded", started, started, "BOOT_REVOKED"); err != nil {
			return nil, domain(ErrStorage, err)
		}
	}
	// A persisted lease transition or any unclean external resource is
	// uncertain and must be recovered before a device can be allocated.
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET allocation_state='reclaiming' WHERE allocation_state IN ('leased','reclaiming') OR id IN (SELECT device_id FROM owned_resources WHERE state!='cleaned')`); err != nil {
		return nil, domain(ErrStorage, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, domain(ErrStorage, err)
	}

	managerCtx, cancel := context.WithCancel(ctx)
	manager := &Manager{
		db:                  db,
		source:              source,
		logger:              logger,
		bootID:              bootID,
		ctx:                 managerCtx,
		cancel:              cancel,
		done:                make(chan struct{}),
		reclaimDone:         make(chan struct{}),
		wake:                make(chan struct{}, 1),
		reclaimWake:         make(chan struct{}, 1),
		recoverySlots:       make(chan struct{}, 1),
		now:                 time.Now,
		leases:              make(map[string]*leaseState),
		revokedCallers:      make(map[string]struct{}),
		recovering:          make(map[string]*recoveryCall),
		pendingRecovery:     make(map[string]struct{}),
		blockedDevices:      make(map[string]struct{}),
		resourceFailures:    make(map[string]struct{}),
		maintenanceFailures: make(map[string]struct{}),
	}
	go manager.run()
	go manager.runReclaims()
	return manager, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for _, lease := range m.leases {
			stopExpiryTicket(lease)
			m.closeGrantsLocked(lease)
		}
		m.cancel()
	}
	m.mu.Unlock()
	<-m.done
	<-m.reclaimDone
	m.recoveryWG.Wait()
	return nil
}

// BootID identifies the current manager lifetime. It changes on every New.
func (m *Manager) BootID() string { return m.bootID }
func (m *Manager) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	if ctx == nil {
		return Principal{}, ErrInvalidRequest
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Principal{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	principal, err := authenticate(ctx, m.db, bearer)
	if err != nil {
		return Principal{}, err
	}
	if _, revoked := m.revokedCallers[principal.CallerID]; revoked {
		return Principal{}, ErrUnauthenticated
	}
	return principal, nil
}

func authenticate(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, bearer string) (Principal, error) {
	if bearer == "" {
		return Principal{}, ErrUnauthenticated
	}
	digest := identity.Digest(bearer)
	var callerID string
	var revokedAt sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id,revoked_at FROM caller_credentials WHERE secret_hash=?`, digest).Scan(&callerID, &revokedAt)
	if err == nil {
		if revokedAt.Valid {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{Kind: PrincipalCaller, ID: callerID, CallerID: callerID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Principal{}, domain(ErrStorage, err)
	}
	var taskID, taskCaller string
	var callerRevoked sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT t.id,t.caller_id,c.revoked_at FROM tasks t JOIN caller_credentials c ON c.id=t.caller_id WHERE t.task_secret_hash=?`, digest).Scan(&taskID, &taskCaller, &callerRevoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && callerRevoked.Valid) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, domain(ErrStorage, err)
	}
	return Principal{Kind: PrincipalTask, ID: taskID, CallerID: taskCaller}, nil
}

func (m *Manager) run() {
	defer close(m.done)
	defer func() {
		m.mu.Lock()
		m.closed = true
		for _, lease := range m.leases {
			stopExpiryTicket(lease)
			m.closeGrantsLocked(lease)
		}
		m.cancel()
		m.mu.Unlock()
	}()
	for {
		m.processExpired()
		timer := time.NewTimer(m.nextExpiryDelay())
		select {
		case <-m.ctx.Done():
			stopTimer(timer)
			return
		case <-m.wake:
			stopTimer(timer)
		case <-timer.C:
		}
	}
}

func (m *Manager) runReclaims() {
	defer close(m.reclaimDone)
	if err := m.processReclaims(m.ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.logFailure("startup_recovery", err)
	}
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.reclaimWake:
			if err := m.processPendingReclaims(); err != nil && !errors.Is(err, context.Canceled) {
				m.logFailure("pending_recovery", err)
			}
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (m *Manager) installExpiry(state *leaseState, deadline time.Time) *expiryTicket {
	ticket := &expiryTicket{deadline: deadline}
	state.expired.Store(nil)
	state.expiry.Store(ticket)
	m.startExpiryTimer(state, ticket)
	return ticket
}

func (m *Manager) startExpiryTimer(state *leaseState, ticket *expiryTicket) {
	delay := ticket.deadline.Sub(m.now())
	if delay < 0 {
		delay = 0
	}
	timer := time.AfterFunc(delay, func() {
		m.expireTicket(state, ticket)
	})
	ticket.timer.Store(timer)
	if state.expiry.Load() != ticket {
		timer.Stop()
	}
}

func (m *Manager) expireTicket(state *leaseState, ticket *expiryTicket) bool {
	if !state.expiry.CompareAndSwap(ticket, nil) {
		return false
	}
	state.expired.Store(ticket)
	state.cancel()
	cancelGrantContexts(state)
	m.signalExpiry()
	return true
}

func stopExpiryTicket(state *leaseState) {
	ticket := state.expiry.Swap(nil)
	state.expired.Store(nil)
	if ticket != nil {
		if timer := ticket.timer.Load(); timer != nil {
			timer.Stop()
		}
	}
	state.cancel()
}

func (m *Manager) nextExpiryDelay() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	delay := 24 * time.Hour
	now := m.now()
	for _, state := range m.leases {
		if state.lease.Status != LeaseActive {
			continue
		}
		remaining := state.deadline.Sub(now)
		if remaining < delay {
			delay = remaining
		}
	}
	if delay < 0 {
		return 0
	}
	return delay
}

func (m *Manager) processExpired() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.expireLocked(m.ctx)
	states := make([]*leaseState, 0)
	for _, state := range m.leases {
		if state.lease.Status == LeaseEnded && state.endErr == nil {
			states = append(states, state)
		}
	}
	m.mu.Unlock()
	for _, state := range states {
		state.opMu.Lock()
		m.mu.Lock()
		var err error
		if m.leases[state.lease.ID] == state && state.lease.Status == LeaseEnded && state.endErr == nil {
			endedAt := m.now()
			if state.lease.EndedAt != nil {
				endedAt = *state.lease.EndedAt
			}
			deviceID, endErr := m.endLocked(m.ctx, state, state.lease.EndReason, "system", m.bootID, endedAt)
			if endErr != nil {
				err = endErr
			} else if deviceID != "" {
				m.pendingRecovery[deviceID] = struct{}{}
				m.signalReclaims()
			}
		}
		m.mu.Unlock()
		state.opMu.Unlock()
		if err != nil {
			m.logFailure("lease_expiry", err)
		}
	}
}

func (m *Manager) processPendingReclaims() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	devices := make([]string, 0, len(m.pendingRecovery))
	for deviceID := range m.pendingRecovery {
		devices = append(devices, deviceID)
	}
	m.mu.Unlock()
	sort.Strings(devices)
	var failures []error
	for _, deviceID := range devices {
		if err := m.ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := m.recoverDevice(m.ctx, deviceID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) processReclaims(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `SELECT id FROM devices WHERE allocation_state='reclaiming' ORDER BY id`)
	if err != nil {
		return domain(ErrStorage, err)
	}
	var devices []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return domain(ErrStorage, errors.Join(err, rows.Close()))
		}
		devices = append(devices, id)
	}
	queryErr := rows.Err()
	closeErr := rows.Close()
	if queryErr != nil || closeErr != nil {
		return domain(ErrStorage, errors.Join(queryErr, closeErr))
	}
	var failures []error
	for _, id := range devices {
		if err = ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err = m.recoverDevice(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) signalExpiry() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) signalReclaims() {
	select {
	case m.reclaimWake <- struct{}{}:
	default:
	}
}

func (m *Manager) logFailure(operation string, err error) {
	if err == nil {
		return
	}
	category := "INTERNAL_FAILURE"
	var domainErr *Error
	if errors.As(err, &domainErr) {
		category = domainErr.Code
	}
	m.logger.Error("lease_manager_failure", "operation", operation, "errorType", category, "eventId", identity.Secret())
}

func (m *Manager) leaseContext(parent context.Context, state *leaseState) (*Grant, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	m.nextGrant++
	id := m.nextGrant
	ctx, cancel := context.WithCancel(parent)
	stopLease := context.AfterFunc(state.ctx, cancel)
	state.grantsMu.Lock()
	state.grants[id] = cancel
	state.grantsMu.Unlock()
	context.AfterFunc(ctx, func() {
		stopLease()
		cancel()
		state.grantsMu.Lock()
		delete(state.grants, id)
		state.grantsMu.Unlock()
	})
	return &Grant{Context: ctx, link: state.link, manager: m, state: state, cancel: cancel}, nil
}

func (m *Manager) closeGrantsLocked(state *leaseState) {
	state.cancel()
	cancelGrantContexts(state)
}

func lessLink(a, b Link) bool {
	if a.DeviceID != b.DeviceID {
		return a.DeviceID < b.DeviceID
	}
	if a.LinkID != b.LinkID {
		return a.LinkID < b.LinkID
	}
	if a.TransportID != b.TransportID {
		return a.TransportID < b.TransportID
	}
	return a.Generation < b.Generation
}

func sameLink(a, b Link) bool {
	return a.DeviceID == b.DeviceID && a.LinkID == b.LinkID && a.TransportID == b.TransportID && a.Generation == b.Generation
}

func (m *Manager) endLocked(ctx context.Context, state *leaseState, reason, actorKind, actorID string, now time.Time) (string, error) {
	if state.lease.Status != LeaseEnded {
		state.lease.Status = LeaseEnded
		state.lease.EndReason = reason
		ended := now.UTC()
		state.lease.EndedAt = &ended
		m.publishLeaseChange(state.lease, true)
	}
	if state.lease.EndedAt == nil {
		ended := now.UTC()
		state.lease.EndedAt = &ended
	}
	state.endErr = nil
	ended := state.lease.EndedAt.UTC()
	stopExpiryTicket(state)
	m.closeGrantsLocked(state)
	fail := func(err error) (string, error) {
		state.endErr = err
		return "", err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE leases SET status='ended',ended_at=?,end_reason=? WHERE id=? AND status='active' AND boot_id=?`, ended.UnixMilli(), state.lease.EndReason, state.lease.ID, m.bootID)
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fail(domain(ErrStorage, err))
	}
	if changed != 1 {
		var persistedStatus string
		queryErr := tx.QueryRowContext(ctx, `SELECT status FROM leases WHERE id=? AND boot_id=?`, state.lease.ID, m.bootID).Scan(&persistedStatus)
		if queryErr == nil && persistedStatus == LeaseEnded {
			if err = tx.Rollback(); err != nil {
				return fail(domain(ErrStorage, err))
			}
			state.endErr = nil
			delete(m.leases, state.lease.ID)
			return state.lease.DeviceID, nil
		}
		return fail(domain(ErrLeaseEnded, queryErr))
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET allocation_state='reclaiming' WHERE id=? AND allocation_state='leased'`, state.lease.DeviceID); err != nil {
		return fail(domain(ErrStorage, err))
	}
	if err = recordOperation(ctx, tx, actorKind, actorID, state.lease.DeviceID, state.lease.ID, "lease."+state.lease.EndReason, "succeeded", ended, ended, "ENDED"); err != nil {
		return fail(domain(ErrStorage, err))
	}
	if err = tx.Commit(); err != nil {
		return fail(domain(ErrStorage, err))
	}
	state.endErr = nil
	delete(m.leases, state.lease.ID)
	return state.lease.DeviceID, nil
}

func (m *Manager) recoverDevice(ctx context.Context, deviceID string) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return err
	}
	if current := m.recovering[deviceID]; current != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.ctx.Done():
			return ErrClosed
		case <-current.done:
			return current.err
		}
	}
	// Queued recovery can outlive an explicit retry and a subsequent claim.
	// Admit cleanup only while allocation is still blocked for reclamation.
	var allocation string
	if err := m.db.QueryRowContext(ctx, `SELECT allocation_state FROM devices WHERE id=?`, deviceID).Scan(&allocation); err != nil {
		m.mu.Unlock()
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return domain(ErrStorage, err)
	}
	if allocation != "reclaiming" {
		m.mu.Unlock()
		return nil
	}
	call := &recoveryCall{done: make(chan struct{})}
	m.recovering[deviceID] = call
	m.recoveryWG.Add(1)
	m.mu.Unlock()
	defer m.recoveryWG.Done()

	recoveryCtx, cancel := context.WithCancel(ctx)
	stopManagerCancel := context.AfterFunc(m.ctx, cancel)
	defer func() {
		stopManagerCancel()
		cancel()
	}()
	select {
	case m.recoverySlots <- struct{}{}:
		defer func() { <-m.recoverySlots }()
	case <-recoveryCtx.Done():
		m.mu.Lock()
		call.err = recoveryCtx.Err()
		delete(m.recovering, deviceID)
		close(call.done)
		m.mu.Unlock()
		return recoveryCtx.Err()
	}

	complete, cleanupErr := m.source.Recover(recoveryCtx, deviceID)
	m.mu.Lock()
	if recoveryCtx.Err() != nil || m.ctx.Err() != nil {
		if recoveryCtx.Err() != nil {
			cleanupErr = recoveryCtx.Err()
		} else {
			cleanupErr = m.ctx.Err()
		}
	} else if _, failed := m.maintenanceFailures[deviceID]; failed {
		cleanupErr = domain(ErrStorage, errors.New("maintenance transition persistence failed"))
	} else {
		if persistErr := m.finishRecovery(recoveryCtx, deviceID, complete, cleanupErr); persistErr != nil {
			cleanupErr = domain(ErrStorage, errors.Join(cleanupErr, persistErr))
		} else if cleanupErr != nil {
			cleanupErr = domain(ErrReclaimFailed, cleanupErr)
		}
	}
	completed := cleanupErr == nil && complete && m.ctx.Err() == nil
	call.err = cleanupErr
	delete(m.recovering, deviceID)
	if completed {
		delete(m.pendingRecovery, deviceID)
		delete(m.blockedDevices, deviceID)
		delete(m.resourceFailures, deviceID)
	}
	close(call.done)
	m.mu.Unlock()
	if cleanupErr != nil && !errors.Is(cleanupErr, context.Canceled) && !errors.Is(cleanupErr, context.DeadlineExceeded) {
		m.logFailure("device_recovery", cleanupErr)
	}
	return cleanupErr
}

func (m *Manager) finishRecovery(ctx context.Context, deviceID string, complete bool, cleanupErr error) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return domain(ErrStorage, err)
	}
	defer tx.Rollback()
	var maintenance bool
	err = tx.QueryRowContext(ctx, `SELECT maintenance_requested FROM devices WHERE id=? AND allocation_state='reclaiming'`, deviceID).Scan(&maintenance)
	if errors.Is(err, sql.ErrNoRows) {
		return domain(ErrConflict, errors.New("device is no longer reclaiming"))
	}
	if err != nil {
		return domain(ErrStorage, err)
	}
	state, status, resultCode := "reclaiming", "pending", "RECOVERY_INCOMPLETE"
	var safeError any
	now := time.Now().UTC()
	finished := time.Time{}
	if cleanupErr != nil {
		state, status, resultCode = "quarantined", "failed", "CLEANUP_FAILED"
		safeError = "平台资源清理失败"
		finished = now
	} else if complete && maintenance {
		state, status, resultCode, finished = "maintenance", "succeeded", "CLEANED_HEALTHY_MAINTENANCE", now
	} else if complete {
		state, status, resultCode, finished = "available", "succeeded", "CLEANED_HEALTHY", now
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET allocation_state=?,last_error=? WHERE id=? AND allocation_state='reclaiming'`, state, safeError, deviceID); err != nil {
		return domain(ErrStorage, err)
	}
	if cleanupErr != nil || complete || maintenance {
		if err = recordOperation(ctx, tx, "system", m.bootID, deviceID, "", "lease.recover", status, now, finished, resultCode); err != nil {
			return domain(ErrStorage, err)
		}
	}
	if cleanupErr != nil || complete {
		operationStatus, operationCode := "succeeded", "RESOURCE_RECOVERED"
		if cleanupErr != nil {
			operationStatus, operationCode = "failed", "RESOURCE_CLEANUP_FAILED"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE operation_records SET status=?,finished_at=?,result_code=? WHERE device_id=? AND kind='capture.resource_failure' AND status='pending'`, operationStatus, now.UnixMilli(), operationCode, deviceID); err != nil {
			return domain(ErrStorage, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return domain(ErrStorage, err)
	}
	m.publishDeviceChange(deviceID, m.EventTask(deviceID))
	return nil
}

func recordOperation(ctx context.Context, tx *sql.Tx, actorKind, actorID, deviceID, leaseID, kind, status string, created, finished time.Time, resultCode string) error {
	return recordOperationDetails(ctx, tx, actorKind, actorID, deviceID, leaseID, kind, status, created, finished, resultCode, `{}`)
}

func recordOperationDetails(ctx context.Context, tx *sql.Tx, actorKind, actorID, deviceID, leaseID, kind, status string, created, finished time.Time, resultCode, safeDetails string) error {
	var device, lease any
	if deviceID != "" {
		device = deviceID
	}
	if leaseID != "" {
		lease = leaseID
	}
	var finishedValue any
	if !finished.IsZero() {
		finishedValue = finished.UnixMilli()
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,lease_id,kind,status,created_at,finished_at,result_code,safe_details) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, identity.Secret(), actorKind, actorID, device, lease, kind, status, created.UnixMilli(), finishedValue, resultCode, safeDetails)
	return err
}
