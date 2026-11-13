package capture

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
	"droidharbor/internal/scrcpy"
)

const (
	resourceKind     = "scrcpy"
	ownerKind        = "device-capture"
	startupTimeout   = 30 * time.Second
	cleanupTimeout   = 5 * time.Second
	videoQueueSize   = 2
	messageQueueSize = 8
	maxShellLine     = 1024
	maxShellPacket   = 64 << 10
)

type Source interface {
	ReadLink(context.Context, string) (leases.Link, context.Context, error)
	HasLink(leases.Link) bool
}

type Config struct{ ServerPath string }

type Manager struct {
	db              *sql.DB
	adb             *adb.Client
	source          Source
	server          []byte
	logger          *slog.Logger
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	bootID          string
	resourceFailure func(context.Context, string) error
	closed          bool
	controlOnly     bool
	videoOwner      *Manager
	streams         map[string]*Stream
	recovering      map[string]*recoveryFlight
}

type locator struct {
	Jar        string `json:"jar"`
	SCID       int32  `json:"scid"`
	Socket     string `json:"socket"`
	BootUUID   string `json:"bootUuid,omitempty"`
	PID        int    `json:"pid,omitempty"`
	StartTicks uint64 `json:"startTicks,omitempty"`
}

type Stream struct {
	manager          *Manager
	id, deviceID     string
	link             leases.Link
	resource         locator
	ctx              context.Context
	cancel           context.CancelFunc
	frames           chan scrcpy.Packet
	messages         chan scrcpy.DeviceMessage
	displayPower     chan bool
	done             chan struct{}
	started          chan struct{}
	startedOnce      sync.Once
	mu               sync.Mutex
	connMu           sync.Mutex
	err              error
	cleanupErr       error
	shell            net.Conn
	video            net.Conn
	control          net.Conn
	controlBuffer    []byte
	writeMu          sync.Mutex
	startupAuthMu    sync.RWMutex
	startupAuth      func() error
	stopOnce         sync.Once
	finishMu         sync.Mutex
	finishing        bool
	finalizerStarted bool
	finishAttempt    chan struct{}
	workerWaitOnce   sync.Once
	workersDone      chan struct{}
	workers          sync.WaitGroup
	stopLink         func() bool
	live             bool
	stopRequested    atomic.Bool
	journaled        atomic.Bool
}

func New(ctx context.Context, db *sql.DB, client *adb.Client, source Source, config Config, logger *slog.Logger) (*Manager, error) {
	if ctx == nil {
		return nil, diagnostics.New("capture manager context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db == nil || client == nil || source == nil {
		return nil, diagnostics.New("capture dependencies are required")
	}
	if logger == nil {
		return nil, diagnostics.New("capture logger is required")
	}
	artifact, err := loadServerArtifact(config.ServerPath)
	if err != nil {
		return nil, err
	}
	root, cancel := context.WithCancel(ctx)
	return &Manager{db: db, adb: client, source: source, server: artifact, logger: logger, ctx: root, cancel: cancel, streams: make(map[string]*Stream), recovering: make(map[string]*recoveryFlight)}, nil
}

func (m *Manager) SetBootID(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		panic("capture boot ID cannot be empty")
	}
	if m.bootID != "" {
		panic("capture boot ID already bound")
	}
	m.bootID = id
}

// SetResourceFailure binds the lease manager's allocation block after the
// current boot identity is established and before capture starts are allowed.
func (m *Manager) SetResourceFailure(failure func(context.Context, string) error) error {
	if failure == nil {
		return diagnostics.New("capture resource failure callback is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bootID == "" {
		return diagnostics.New("capture boot identity must be bound before resource failure callback")
	}
	if m.closed {
		return diagnostics.New("capture manager is closed")
	}
	if m.resourceFailure != nil {
		return diagnostics.New("capture resource failure callback already bound")
	}
	m.resourceFailure = failure
	return nil
}

func (m *Manager) notifyResourceFailure(ctx context.Context, deviceID string) error {
	m.mu.Lock()
	failure := m.resourceFailure
	m.mu.Unlock()
	if failure == nil {
		return diagnostics.New("capture resource failure callback is not bound")
	}
	return failure(ctx, deviceID)
}

func (m *Manager) Start(ctx context.Context, deviceID string, settings Settings, authorizeStartup func() error) (result *Stream, startErr error) {
	stage := "preconditions"
	defer func() {
		if startErr != nil {
			startErr = diagnostics.WrapStage("capture_start."+stage, startErr)
		}
	}()
	if ctx == nil {
		return nil, diagnostics.New("capture startup context is required")
	}
	if authorizeStartup == nil {
		return nil, diagnostics.New("capture startup authorization is required")
	}
	if deviceID == "" {
		return nil, diagnostics.New("device ID is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, diagnostics.New("capture manager is closed")
	}
	if m.bootID == "" {
		m.mu.Unlock()
		return nil, diagnostics.New("capture manager boot identity is not bound")
	}
	if m.resourceFailure == nil {
		m.mu.Unlock()
		return nil, diagnostics.New("capture resource failure callback is not bound")
	}
	if m.recovering[deviceID] != nil {
		m.mu.Unlock()
		return nil, diagnostics.New("capture device recovery is in progress")
	}
	for _, current := range m.streams {
		if current.deviceID == deviceID {
			m.mu.Unlock()
			return nil, diagnostics.New("capture already exists for device")
		}
	}
	m.mu.Unlock()
	startCtx, cancelStart := context.WithTimeout(ctx, startupTimeout)
	defer cancelStart()
	if err := authorizeStartup(); err != nil {
		return nil, err
	}
	stage = "read_link"
	link, linkCtx, err := m.source.ReadLink(startCtx, deviceID)
	if err != nil {
		return nil, err
	}
	if link.DeviceID != deviceID || link.TransportID == 0 || linkCtx == nil || !m.source.HasLink(link) {
		return nil, diagnostics.New("verified device link unavailable")
	}
	id := identity.Secret()
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	scid, err := randomSCID()
	if err != nil {
		return nil, err
	}
	jar := "/data/local/tmp/droidharbor-" + nonce + ".jar"
	socket := fmt.Sprintf("scrcpy_%08x", uint32(scid))
	resource := locator{Jar: jar, SCID: scid, Socket: socket}
	streamCtx, cancelStream := context.WithCancel(m.ctx)
	stopLink := context.AfterFunc(linkCtx, cancelStream)
	stopStartup := context.AfterFunc(startCtx, cancelStream)
	var frames chan scrcpy.Packet
	if !m.controlOnly {
		frames = make(chan scrcpy.Packet, videoQueueSize)
	}
	var displayPower chan bool
	if m.controlOnly {
		displayPower = make(chan bool, 1)
	}
	s := &Stream{manager: m, id: id, deviceID: deviceID, link: link, resource: resource, ctx: streamCtx, cancel: cancelStream, frames: frames, messages: make(chan scrcpy.DeviceMessage, messageQueueSize), displayPower: displayPower, done: make(chan struct{}), started: make(chan struct{}), workersDone: make(chan struct{}), stopLink: stopLink, startupAuth: authorizeStartup}
	m.mu.Lock()
	if m.closed || m.recovering[deviceID] != nil || m.streams[deviceID] != nil {
		m.mu.Unlock()
		stopLink()
		cancelStream()
		return nil, diagnostics.New("capture ownership changed during startup")
	}
	m.streams[deviceID] = s
	m.mu.Unlock()
	registered := false
	deliveredToCaller := false
	defer func() {
		if deliveredToCaller {
			return
		}
		m.mu.Lock()
		managerClosing := m.closed
		m.mu.Unlock()
		if managerClosing {
			return
		}
		stopLink()
		if registered {
			s.failAndClose(nil)
		} else {
			s.requestStop(nil)
			m.remove(s)
		}
	}()
	defer s.completeStart()
	ioCtx, cancelIO := context.WithCancel(startCtx)
	stopStreamIO := context.AfterFunc(s.ctx, cancelIO)
	defer cancelIO()
	defer stopStreamIO()
	stage = "resource_journal"
	var outstanding bool
	if err := m.db.QueryRowContext(ioCtx, `SELECT EXISTS(SELECT 1 FROM owned_resources WHERE device_id=? AND owner_kind=? AND kind=? AND state!='cleaned')`, deviceID, ownerKind, m.kind()).Scan(&outstanding); err != nil {
		return nil, err
	}
	if outstanding {
		return nil, diagnostics.New("unrecovered capture resource exists for device")
	}
	if err := m.register(ioCtx, s); err != nil {
		return nil, err
	}
	registered = true
	s.journaled.Store(true)
	authorized := func() error {
		if err := s.authorizeSystem(); err != nil {
			return err
		}
		s.startupAuthMu.RLock()
		startupAuth := s.startupAuth
		s.startupAuthMu.RUnlock()
		if startupAuth != nil {
			return startupAuth()
		}
		return nil
	}
	stage = "prepare_server"
	if err := m.prepareArtifact(ioCtx, s, authorized); err != nil {
		_ = m.markFailureBounded(s, "SERVER_PREPARE_FAILED")
		return nil, err
	}
	gateNonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	var cmd string
	if m.controlOnly {
		cmd, err = startupPowerCommand(jar, gateNonce, scid)
	} else {
		cmd, err = startupCommand(jar, gateNonce, scid, settings)
	}
	if err != nil {
		return nil, err
	}
	stage = "open_shell"
	shell, err := m.adb.Open(s.ctx, link.TransportID, "shell,v2,raw:"+cmd, authorized)
	if err != nil {
		_ = m.markFailureBounded(s, "SHELL_OPEN_FAILED")
		return nil, err
	}
	s.connMu.Lock()
	s.shell = shell
	s.connMu.Unlock()
	stage = "bootstrap_ready"
	ready, err := readReady(startCtx, shell)
	if err != nil {
		_ = m.markFailureBounded(s, "GATE_FAILED")
		return nil, err
	}
	s.resource.BootUUID, s.resource.PID, s.resource.StartTicks = ready.boot, ready.pid, ready.ticks
	if !validBootUUID(ready.boot) || ready.pid <= 0 || ready.ticks == 0 {
		_ = m.markFailureBounded(s, "INVALID_PROCESS_IDENTITY")
		return nil, diagnostics.New("invalid capture process identity")
	}
	stage = "persist_process"
	if err := m.updateLocator(ioCtx, s); err != nil {
		s.resource = resource
		return nil, err
	}
	stage = "release_bootstrap"
	if err := writeShellStdin(shell, []byte(gateNonce+"\n")); err != nil {
		_ = m.markFailureBounded(s, "GATE_WRITE_FAILED")
		return nil, err
	}
	if err := writeShellStdin(shell, []byte("GO\n")); err != nil {
		_ = m.markFailureBounded(s, "GO_UNCERTAIN")
		return nil, err
	}
	if err := writeShellCloseStdin(shell); err != nil {
		_ = m.markFailureBounded(s, "GO_UNCERTAIN")
		return nil, err
	}
	stage = "socket_ready"
	for i := 0; i < 40; i++ {
		if err := ioCtx.Err(); err != nil {
			return nil, err
		}
		if !m.source.HasLink(link) {
			return nil, diagnostics.New("pinned device link lost during startup")
		}
		err := m.probeReady(ioCtx, s, authorized)
		if err == nil {
			break
		}
		if !errors.Is(err, errSocketNotListening) {
			return nil, err
		}
		if i == 39 {
			return nil, diagnostics.New("scrcpy server readiness deadline exceeded")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ioCtx.Done():
			timer.Stop()
			return nil, ioCtx.Err()
		case <-timer.C:
		}
	}
	stage = "open_socket"
	first, err := m.adb.Open(s.ctx, link.TransportID, "localabstract:"+socket, authorized)
	if err != nil {
		return nil, err
	}
	s.connMu.Lock()
	if m.controlOnly {
		s.control = first
	} else {
		s.video = first
	}
	s.connMu.Unlock()
	if deadline, ok := startCtx.Deadline(); ok {
		if err := first.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	stage = "socket_dummy"
	if err := scrcpy.ReadDummy(first); err != nil {
		return nil, err
	}
	if !m.controlOnly {
		stage = "open_control"
		control, err := m.adb.Open(s.ctx, link.TransportID, "localabstract:"+socket, authorized)
		if err != nil {
			return nil, err
		}
		s.connMu.Lock()
		s.control = control
		s.connMu.Unlock()
		stage = "video_preamble"
		metadata, err := scrcpy.ReadPreamble(first)
		if err != nil {
			return nil, err
		}
		if metadata.Codec != scrcpy.CodecH264 {
			return nil, diagnostics.New("scrcpy server did not negotiate H.264")
		}
	}
	if err := first.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	stage = "activate"
	if err := m.activate(ioCtx, s); err != nil {
		return nil, err
	}
	s.workers.Add(2)
	if !m.controlOnly {
		s.workers.Add(1)
		go s.readFrames()
	}
	go s.readMessages()
	go s.drainShell()
	stage = "deliver"
	stopStartup()
	if err := startCtx.Err(); err != nil {
		return nil, err
	}
	if err := authorized(); err != nil {
		return nil, err
	}
	if err := startCtx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed || m.streams[deviceID] != s {
		m.mu.Unlock()
		return nil, diagnostics.New("capture ownership changed before delivery")
	}
	s.live = true
	m.mu.Unlock()
	deliveredToCaller = true
	s.startupAuthMu.Lock()
	s.startupAuth = nil
	s.startupAuthMu.Unlock()
	s.watchContextCancellation()
	return s, nil
}

func (m *Manager) register(ctx context.Context, s *Stream) error {
	encoded, err := json.Marshal(s.resource)
	if err != nil {
		return err
	}
	_, err = m.db.ExecContext(ctx, `INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES(?,?,?, ?,?,?,?,'pending',?)`, s.id, s.deviceID, ownerKind, s.id, m.bootID, m.kind(), string(encoded), time.Now().UTC().UnixMilli())
	return err
}
func (m *Manager) updateLocator(ctx context.Context, s *Stream) error {
	data, err := json.Marshal(s.resource)
	if err != nil {
		return err
	}
	res, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET locator=? WHERE id=? AND device_id=? AND owner_kind=? AND owner_id=? AND boot_id=? AND kind=? AND state='pending'`, string(data), s.id, s.deviceID, ownerKind, s.id, m.bootID, m.kind())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return diagnostics.New("capture ownership journal changed")
	}
	return nil
}
func (m *Manager) activate(ctx context.Context, s *Stream) error {
	res, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET state='active',locator=? WHERE id=? AND device_id=? AND owner_kind=? AND owner_id=? AND boot_id=? AND kind=? AND state='pending'`, mustJSON(s.resource), s.id, s.deviceID, ownerKind, s.id, m.bootID, m.kind())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return diagnostics.New("capture ownership journal changed before activation")
	}
	return nil
}
func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func (m *Manager) markFailure(ctx context.Context, s *Stream, code string) error {
	if len(code) > 64 {
		code = "CAPTURE_FAILED"
	}
	_, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET state='failed',cleanup_error=? WHERE id=? AND device_id=? AND owner_kind=? AND owner_id=? AND boot_id=? AND kind=? AND state!='cleaned'`, code, s.id, s.deviceID, ownerKind, s.id, m.bootID, m.kind())
	return err
}
func (m *Manager) markFailureBounded(s *Stream, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	err := m.markFailure(ctx, s, code)
	if err != nil {
		m.logger.Error("capture_failure_persistence_failed", "deviceId", s.deviceID, "captureId", s.id, "code", code, "cause", diagnostics.Cause(err))
	}
	return err
}
func (m *Manager) remove(s *Stream) {
	m.mu.Lock()
	if m.streams[s.deviceID] == s {
		delete(m.streams, s.deviceID)
	}
	m.mu.Unlock()
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func randomSCID() (int32, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(0x7fffffff))
	if err != nil {
		return 0, err
	}
	return int32(n.Int64() + 1), nil
}
