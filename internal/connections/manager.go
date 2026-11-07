// Package connections owns task-scoped, in-process ADB transports and their
// durable resource journal. It never owns persistent ADB forwards or phone files.
package connections

import (
	"context"
	"crypto/rsa"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/adbgateway"
	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
)

const Kind = "adb-connection"
const Subprotocol = "droidharbor.adb.v1"

var (
	ErrClosed      = &leases.Error{Code: "CONNECTION_CLOSED", Message: "连接已关闭，不能恢复旧通道", HTTPStatus: http.StatusGone}
	ErrUsed        = &leases.Error{Code: "CONNECTION_USED", Message: "此连接已使用，不能重新建立传输", HTTPStatus: http.StatusConflict}
	ErrProtocol    = &leases.Error{Code: "PROTOCOL_UNSUPPORTED", Message: "不支持请求的 ADB 网关协议版本", HTTPStatus: http.StatusConflict}
	ErrIdempotency = &leases.Error{Code: "IDEMPOTENCY_CONFLICT", Message: "连接幂等键已绑定其他请求", HTTPStatus: http.StatusConflict}
)

type Request struct {
	ProtocolMajor int    `json:"protocolMajor"`
	ADBPublicKey  string `json:"adbPublicKey"`
}

type View struct {
	ConnectionID  string    `json:"connectionId"`
	LeaseID       string    `json:"leaseId"`
	DeviceID      string    `json:"deviceId"`
	Generation    int64     `json:"generation"`
	BootID        string    `json:"bootId"`
	ProtocolMajor int       `json:"protocolMajor"`
	Status        string    `json:"status"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Features      []string  `json:"features"`
	TunnelPath    string    `json:"tunnelPath"`
}

type metadata struct {
	LeaseID       string      `json:"leaseId"`
	Generation    int64       `json:"generation"`
	Link          leases.Link `json:"link"`
	ProtocolMajor int         `json:"protocolMajor"`
	PublicKey     string      `json:"publicKey"`
	Features      []string    `json:"features"`
	OperationID   string      `json:"operationId"`
}

type entry struct {
	id           string
	lease        leases.Lease
	metadata     metadata
	key          *rsa.PublicKey
	grant        *leases.Grant
	initialized  chan struct{}
	stopLink     func() bool
	cleanup      sync.Mutex
	cleaned      bool
	mu           sync.Mutex
	persisted    bool
	used         bool
	closing      bool
	cleanupError error
	finished     chan struct{}
	conn         net.Conn
}

type linkSource interface {
	LinkContext(leases.Link) (context.Context, error)
}

type Manager struct {
	ctx     context.Context
	cancel  context.CancelFunc
	db      *sql.DB
	adb     *adb.Client
	links   linkSource
	logger  *slog.Logger
	mu      sync.Mutex
	entries map[string]*entry
	closed  bool
	workers sync.WaitGroup
}

func New(ctx context.Context, db *sql.DB, client *adb.Client, links linkSource, logger *slog.Logger) *Manager {
	lifetime, cancel := context.WithCancel(ctx)
	return &Manager{ctx: lifetime, cancel: cancel, db: db, adb: client, links: links, logger: logger, entries: make(map[string]*entry)}
}

func storageError(err error) error {
	return &leases.Error{Code: leases.ErrStorage.Code, Message: leases.ErrStorage.Message, HTTPStatus: leases.ErrStorage.HTTPStatus, Cause: err}
}

// Create receives the lease authority per operation so the resource cleaner can
// be installed before leases.New starts recovery. Recovery needs no lease grant.
func (m *Manager) Create(ctx context.Context, authority *leases.Manager, principal leases.Principal, leaseID, idempotency string, request Request) (View, bool, error) {
	if request.ProtocolMajor != 1 {
		return View{}, false, ErrProtocol
	}
	if len(idempotency) != 64 {
		return View{}, false, leases.ErrInvalidRequest
	}
	if _, err := hex.DecodeString(idempotency); err != nil {
		return View{}, false, leases.ErrInvalidRequest
	}
	key, err := adbgateway.ParsePublicKey(request.ADBPublicKey)
	if err != nil {
		return View{}, false, leases.ErrInvalidRequest
	}
	publicKey := strings.Fields(request.ADBPublicKey)[0]
	lease, err := authority.Get(ctx, principal, leaseID)
	if err != nil {
		return View{}, false, err
	}
	if principal.Kind != leases.PrincipalTask {
		return View{}, false, leases.ErrForbidden
	}
	id := hex.EncodeToString(identity.Digest("adb-connection:" + lease.ID + ":" + idempotency))
	if existing, _, readErr := m.read(ctx, lease, id); readErr == nil {
		if existing.ProtocolMajor != request.ProtocolMajor || existing.PublicKey != publicKey {
			return View{}, false, ErrIdempotency
		}
		view, getErr := m.Get(ctx, lease, id)
		return view, false, getErr
	} else if !errors.Is(readErr, leases.ErrNotFound) {
		return View{}, false, readErr
	}
	grant, err := authority.Grant(m.ctx, principal, lease.ID)
	if err != nil {
		return View{}, false, err
	}
	linkCtx, err := m.links.LinkContext(grant.Link())
	if err != nil {
		grant.Close()
		return View{}, false, err
	}
	e := &entry{id: id, lease: lease, grant: grant, key: key, initialized: make(chan struct{}), metadata: metadata{
		LeaseID: lease.ID, Generation: lease.Generation, Link: grant.Link(), ProtocolMajor: 1, PublicKey: publicKey, Features: []string{}, OperationID: identity.Secret(),
	}}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		grant.Close()
		return View{}, false, leases.ErrClosed
	}
	if previous := m.entries[id]; previous != nil {
		m.mu.Unlock()
		grant.Close()
		select {
		case <-previous.initialized:
		case <-ctx.Done():
			return View{}, false, ctx.Err()
		}
		existing, _, readErr := m.read(ctx, lease, id)
		if readErr != nil {
			return View{}, false, readErr
		}
		if existing.PublicKey != publicKey || existing.ProtocolMajor != request.ProtocolMajor {
			return View{}, false, ErrIdempotency
		}
		view, getErr := m.Get(ctx, lease, id)
		return view, false, getErr
	}
	// Recovery scans this same map lock after canceling lease grants. Register
	// before journaling, and journal before any upstream I/O, closing both races.
	if err = grant.Check(); err != nil {
		m.mu.Unlock()
		grant.Close()
		return View{}, false, err
	}
	if err = linkCtx.Err(); err != nil {
		m.mu.Unlock()
		grant.Close()
		return View{}, false, leases.ErrDeviceOffline
	}
	m.entries[id] = e
	m.workers.Add(2)
	m.mu.Unlock()
	e.stopLink = context.AfterFunc(linkCtx, grant.Close)
	go m.watch(e)
	defer func() { close(e.initialized); m.workers.Done() }()
	success := false
	defer func() {
		if !success {
			grant.Close()
		}
	}()
	data, err := json.Marshal(e.metadata)
	if err != nil {
		return View{}, false, err
	}
	inserted, err := m.register(ctx, e, string(data))
	if err != nil {
		return View{}, false, err
	}
	if !inserted {
		existing, state, readErr := m.read(ctx, lease, id)
		if readErr != nil {
			return View{}, false, readErr
		}
		if existing.PublicKey != publicKey || existing.ProtocolMajor != 1 {
			return View{}, false, ErrIdempotency
		}
		return viewFor(lease, id, existing, state), false, nil
	}
	e.persisted = true
	features, err := m.adb.Features(grant, grant.Link().TransportID)
	if err != nil {
		return View{}, false, &leases.Error{Code: "ADB_UNAVAILABLE", Message: "无法读取租约设备的 ADB 能力", HTTPStatus: 503, Cause: err}
	}
	e.metadata.Features = adbgateway.SupportedFeatures(features)
	if err = grant.Check(); err != nil {
		return View{}, false, err
	}
	data, err = json.Marshal(e.metadata)
	if err != nil {
		return View{}, false, err
	}
	if err = m.finishCreation(ctx, e, string(data)); err != nil {
		return View{}, false, err
	}
	if err = grant.Check(); err != nil {
		return View{}, false, err
	}
	success = true
	return viewFor(lease, id, e.metadata, "pending"), true, nil
}

func (m *Manager) register(ctx context.Context, e *entry, locator string) (bool, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, storageError(err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES(?,?,'task',?,?,?,?,'pending',?) ON CONFLICT(id) DO NOTHING`, e.id, e.lease.DeviceID, e.lease.TaskID, e.lease.BootID, Kind, locator, time.Now().UTC().UnixMilli())
	if err != nil {
		return false, storageError(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, storageError(err)
	}
	if changed == 0 {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,lease_id,kind,status,created_at,safe_details) VALUES(?,'task',?,?,?,'connection.create','pending',?,?)`, e.metadata.OperationID, e.lease.TaskID, e.lease.DeviceID, e.lease.ID, time.Now().UTC().UnixMilli(), `{"connectionId":"`+e.id+`"}`); err != nil {
		return false, storageError(err)
	}
	if err = tx.Commit(); err != nil {
		return false, storageError(err)
	}
	return true, nil
}

func (m *Manager) finishCreation(ctx context.Context, e *entry, locator string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return storageError(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE owned_resources SET locator=? WHERE id=? AND state='pending'`, locator, e.id); err != nil {
		return storageError(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operation_records SET status='succeeded',finished_at=?,result_code='CREATED' WHERE id=? AND status='pending'`, time.Now().UTC().UnixMilli(), e.metadata.OperationID); err != nil {
		return storageError(err)
	}
	if err = tx.Commit(); err != nil {
		return storageError(err)
	}
	return nil
}

func (m *Manager) read(ctx context.Context, lease leases.Lease, id string) (metadata, string, error) {
	var encoded, state string
	err := m.db.QueryRowContext(ctx, `SELECT locator,state FROM owned_resources WHERE id=? AND device_id=? AND owner_kind='task' AND owner_id=? AND boot_id=? AND kind=?`, id, lease.DeviceID, lease.TaskID, lease.BootID, Kind).Scan(&encoded, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return metadata{}, "", leases.ErrNotFound
	}
	if err != nil {
		return metadata{}, "", storageError(err)
	}
	var data metadata
	if err = json.Unmarshal([]byte(encoded), &data); err != nil {
		return metadata{}, "", storageError(err)
	}
	if data.LeaseID != lease.ID || data.Generation != lease.Generation {
		return metadata{}, "", leases.ErrNotFound
	}
	return data, state, nil
}

func viewFor(lease leases.Lease, id string, data metadata, state string) View {
	if state == "cleaned" {
		state = "closed"
	}
	if state == "cleaning" {
		state = "closing"
	}
	return View{ConnectionID: id, LeaseID: lease.ID, DeviceID: lease.DeviceID, Generation: lease.Generation, BootID: lease.BootID, ProtocolMajor: data.ProtocolMajor, Status: state, ExpiresAt: lease.ExpiresAt, Features: data.Features, TunnelPath: "/api/v1/leases/" + lease.ID + "/connections/" + id + "/tunnel"}
}

func (m *Manager) Get(ctx context.Context, lease leases.Lease, id string) (View, error) {
	m.mu.Lock()
	e := m.entries[id]
	m.mu.Unlock()
	if e != nil {
		select {
		case <-e.initialized:
		case <-ctx.Done():
			return View{}, ctx.Err()
		}
	}
	data, state, err := m.read(ctx, lease, id)
	if err != nil {
		return View{}, err
	}
	if e != nil && state != "cleaned" {
		e.mu.Lock()
		if e.cleanupError != nil {
			state = "failed"
		} else if e.grant.Err() != nil {
			state = "cleaning"
		}
		e.mu.Unlock()
	}
	return viewFor(lease, id, data, state), nil
}
