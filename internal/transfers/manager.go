package transfers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"droidharbor/internal/adb"
	"droidharbor/internal/identity"
	"droidharbor/internal/media"
)

const (
	Kind                          = "human-transfer-staging"
	DefaultMaxUploadBytes   int64 = 256 << 20
	DefaultMaxActive              = 8
	DefaultMaxCommandOutput int64 = 64 << 10
	MaxUploadBytesLimit     int64 = 1 << 30
	MaxActiveLimit                = 64
	MaxCommandOutputLimit   int64 = 1 << 20
)

const (
	StateUploading    = "uploading"
	StateTransferring = "transferring"
	StateInstalling   = "installing"
	StateSucceeded    = "succeeded"
	StateFailed       = "failed"
	StateCancelled    = "cancelled"
	StateUnknown      = "unknown"
)

func terminal(state string) bool {
	return state == StateSucceeded || state == StateFailed || state == StateCancelled || state == StateUnknown
}

const localOwnerKind = "human-session"
const tempPrefix = "droidharbor-transfer-"
const tempSuffix = ".part"
const maxRetainedTransfers = 1024

// Config selects the restricted staging directory, process boot identity,
// and explicit externally visible resource limits.
type Config struct {
	TempDir          string
	BootID           string
	MaxUploadBytes   int64
	MaxActive        int
	MaxCommandOutput int64
}

type ScopeProvider interface {
	SessionScope(context.Context, string, string, bool) (media.Scope, error)
}

type Dependencies struct {
	DB     *sql.DB
	ADB    *adb.Client
	Scopes ScopeProvider
	Logger *slog.Logger
}

type CreateRequest struct {
	FileName   string `json:"fileName"`
	Purpose    string `json:"purpose"`
	TargetPath string `json:"targetPath,omitempty"`
	Overwrite  bool   `json:"overwrite"`
	Size       int64  `json:"size"`
}

type Limits struct {
	MaxUploadBytes   int64 `json:"maxUploadBytes"`
	MaxActive        int   `json:"maxActive"`
	MaxCommandOutput int64 `json:"maxCommandOutput"`
}

// Transfer is an owner-scoped snapshot. Result preserves bounded native Package
// Manager output for APK operations; Error contains only bounded sanitized status.
type Transfer struct {
	ID               string    `json:"transferId"`
	FileName         string    `json:"fileName"`
	Purpose          string    `json:"purpose"`
	TargetPath       string    `json:"targetPath,omitempty"`
	State            string    `json:"state"`
	BytesReceived    int64     `json:"bytesReceived"`
	BytesTransferred int64     `json:"bytesTransferred"`
	Size             int64     `json:"size"`
	Overwrite        bool      `json:"overwrite"`
	ExitCode         *uint8    `json:"exitCode,omitempty"`
	Stdout           string    `json:"stdout,omitempty"`
	Stderr           string    `json:"stderr,omitempty"`
	Result           string    `json:"result,omitempty"`
	ErrorCode        string    `json:"errorCode,omitempty"`
	Error            string    `json:"error,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type Error struct {
	Status  int
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func problem(status int, code, message string, cause error) *Error {
	return &Error{Status: status, Code: code, Message: message, Cause: cause}
}

type transfer struct {
	value      Transfer
	owner      string
	sessionID  string
	deviceID   string
	path       string
	locator    string
	scope      media.Scope
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	uploadDone chan struct{}
	claimed    bool
	started    bool
	closed     bool
	finishing  bool
	finishErr  error
}

type Manager struct {
	db      *sql.DB
	adb     *adb.Client
	scopes  ScopeProvider
	logger  *slog.Logger
	config  Config
	tempDir string
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	items   map[string]*transfer
	closed  bool
	workers sync.WaitGroup
}

type resourceLocator struct {
	Path string `json:"path"`
}

func New(ctx context.Context, deps Dependencies, config Config) (*Manager, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("transfer manager context must be active")
	}
	if deps.DB == nil || deps.ADB == nil || deps.Scopes == nil || deps.Logger == nil {
		return nil, errors.New("transfer dependencies are required")
	}
	if err := validateConfig(config, true); err != nil {
		return nil, err
	}
	dir, err := ensureTempDir(config.TempDir)
	if err != nil {
		return nil, err
	}
	config.TempDir = dir
	lifetime, cancel := context.WithCancel(ctx)
	return &Manager{db: deps.DB, adb: deps.ADB, scopes: deps.Scopes, logger: deps.Logger, config: config, tempDir: dir, ctx: lifetime, cancel: cancel, items: make(map[string]*transfer)}, nil
}
func (m *Manager) Limits() Limits {
	return Limits{MaxUploadBytes: m.config.MaxUploadBytes, MaxActive: m.config.MaxActive, MaxCommandOutput: m.config.MaxCommandOutput}
}

func validateConfig(config Config, bootRequired bool) error {
	if strings.TrimSpace(config.TempDir) == "" {
		return errors.New("transfer staging directory is required")
	}
	if bootRequired && strings.TrimSpace(config.BootID) == "" {
		return errors.New("transfer boot ID is required")
	}
	if config.MaxUploadBytes <= 0 || config.MaxUploadBytes > MaxUploadBytesLimit {
		return fmt.Errorf("MaxUploadBytes must be between 1 and %d", MaxUploadBytesLimit)
	}
	if config.MaxActive <= 0 || config.MaxActive > MaxActiveLimit {
		return fmt.Errorf("MaxActive must be between 1 and %d", MaxActiveLimit)
	}
	if config.MaxCommandOutput <= 0 || config.MaxCommandOutput > MaxCommandOutputLimit {
		return fmt.Errorf("MaxCommandOutput must be between 1 and %d", MaxCommandOutputLimit)
	}
	return nil
}

func ensureTempDir(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("resolve transfer staging directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return "", fmt.Errorf("create transfer staging directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect transfer staging directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("transfer staging path must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve transfer staging directory: %w", err)
	}
	if err := os.Chmod(resolved, 0700); err != nil {
		return "", fmt.Errorf("secure transfer staging directory: %w", err)
	}
	return resolved, nil
}

// Recover removes only journaled staging files whose exact generated path is
// inside config.TempDir. It is safe before the new process boot ID is assigned.
func Recover(ctx context.Context, db *sql.DB, config Config) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("transfer recovery context must be active")
	}
	if db == nil {
		return errors.New("transfer recovery database is required")
	}
	if err := validateConfig(config, false); err != nil {
		return err
	}
	dir, err := ensureTempDir(config.TempDir)
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,owner_id,boot_id,locator,state FROM owned_resources WHERE owner_kind=? AND kind=? AND state!='cleaned'`, localOwnerKind, Kind)
	if err != nil {
		return fmt.Errorf("load transfer staging recovery records: %w", err)
	}
	type journal struct{ id, owner, boot, locator, state string }
	var pending []journal
	for rows.Next() {
		var item journal
		if err := rows.Scan(&item.id, &item.owner, &item.boot, &item.locator, &item.state); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read transfer staging recovery record: %w", err)
		}
		pending = append(pending, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("finish transfer staging recovery query: %w", err)
	}
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := validateJournalPath(dir, item.id, item.locator)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			_, _ = db.ExecContext(ctx, `UPDATE owned_resources SET state='failed',cleanup_error='TRANSFER_TEMP_CLEANUP_FAILED' WHERE id=? AND owner_kind=? AND owner_id=? AND kind=?`, item.id, localOwnerKind, item.owner, Kind)
			return fmt.Errorf("remove owned transfer staging file: %w", err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transfer staging recovery: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE owned_resources SET state='cleaned',cleanup_error=NULL WHERE id=? AND owner_kind=? AND owner_id=? AND boot_id=? AND kind=? AND locator=? AND state!='cleaned'`, item.id, localOwnerKind, item.owner, item.boot, Kind, item.locator)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("complete transfer staging recovery: %w", err)
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			_ = tx.Rollback()
			return errors.New("transfer staging ownership changed during recovery")
		}
		status, code := StateCancelled, "PROCESS_RESTARTED_DURING_UPLOAD"
		var old string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM operation_records WHERE id=? AND actor_kind=? AND actor_id=?`, item.id, localOwnerKind, item.owner).Scan(&old); err == nil {
			if old == StateTransferring || old == StateInstalling {
				status, code = StateUnknown, "PROCESS_RESTARTED_DURING_DEVICE_OPERATION"
			}
			if terminal(old) {
				status = ""
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return fmt.Errorf("read transfer operation recovery state: %w", err)
		}
		if status != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE operation_records SET status=?,finished_at=?,result_code=? WHERE id=? AND actor_kind=? AND actor_id=?`, status, time.Now().UTC().UnixMilli(), code, item.id, localOwnerKind, item.owner); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("record interrupted transfer recovery: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit transfer staging recovery: %w", err)
		}
	}
	return nil
}

func validateJournalPath(dir, id, encoded string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) || !utf8.ValidString(id) {
		return "", errors.New("invalid transfer staging ownership ID")
	}
	var locator resourceLocator
	if err := json.Unmarshal([]byte(encoded), &locator); err != nil {
		return "", errors.New("invalid transfer staging ownership locator")
	}
	expected := filepath.Join(dir, tempPrefix+id+tempSuffix)
	if locator.Path != expected {
		return "", errors.New("transfer staging ownership path does not match restricted directory")
	}
	return expected, nil
}

func (m *Manager) Create(ctx context.Context, owner, sessionID string, req CreateRequest) (Transfer, error) {
	if err := ctx.Err(); err != nil {
		return Transfer{}, err
	}
	if req.Size < 0 || req.Size > m.config.MaxUploadBytes {
		return Transfer{}, problem(413, "TRANSFER_SIZE_LIMIT", "文件大小不符合传输限制", nil)
	}
	if err := validateMetadata(&req); err != nil {
		return Transfer{}, err
	}
	scope, err := m.scopes.SessionScope(m.ctx, owner, sessionID, true)
	if err != nil {
		return Transfer{}, err
	}
	if err := ctx.Err(); err != nil {
		scope.Close()
		return Transfer{}, err
	}
	if err := scope.Check(); err != nil {
		scope.Close()
		return Transfer{}, err
	}
	id := identity.Secret()
	path := filepath.Join(m.tempDir, tempPrefix+id+tempSuffix)
	locatorData, err := json.Marshal(resourceLocator{Path: path})
	if err != nil {
		scope.Close()
		return Transfer{}, err
	}
	now := time.Now().UTC()
	t := &transfer{owner: owner, sessionID: sessionID, deviceID: scope.DeviceID, path: path, locator: string(locatorData), scope: scope, done: make(chan struct{}), uploadDone: make(chan struct{})}
	t.ctx, t.cancel = context.WithCancel(scope.Context)
	t.value = Transfer{ID: id, FileName: req.FileName, Purpose: req.Purpose, TargetPath: req.TargetPath, Overwrite: req.Overwrite, Size: req.Size, State: StateUploading, CreatedAt: now, UpdatedAt: now}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		t.cancel()
		scope.Close()
		return Transfer{}, problem(503, "TRANSFER_UNAVAILABLE", "文件传输服务已关闭", nil)
	}
	active := 0
	for _, existing := range m.items {
		if !terminal(existing.value.State) {
			active++
		}
	}
	if active >= m.config.MaxActive {
		m.mu.Unlock()
		t.cancel()
		scope.Close()
		return Transfer{}, problem(503, "TRANSFER_CAPACITY", "文件传输容量已满", nil)
	}
	if len(m.items) >= maxRetainedTransfers {
		oldestID := ""
		var oldest time.Time
		for oldID, existing := range m.items {
			if terminal(existing.value.State) && (oldestID == "" || existing.value.UpdatedAt.Before(oldest)) {
				oldestID = oldID
				oldest = existing.value.UpdatedAt
			}
		}
		if oldestID == "" {
			m.mu.Unlock()
			t.cancel()
			scope.Close()
			return Transfer{}, problem(503, "TRANSFER_HISTORY_CAPACITY", "文件传输历史已满", nil)
		}
		delete(m.items, oldestID)
	}
	if _, exists := m.items[id]; exists {
		m.mu.Unlock()
		t.cancel()
		scope.Close()
		return Transfer{}, errors.New("transfer identity collision")
	}
	m.items[id] = t
	m.workers.Add(1)
	m.mu.Unlock()
	if err := m.insertJournal(ctx, t); err != nil {
		m.mu.Lock()
		delete(m.items, id)
		m.mu.Unlock()
		t.cancel()
		scope.Close()
		close(t.done)
		m.workers.Done()
		return Transfer{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = m.finish(t, StateFailed, "STAGING_CREATE_FAILED", err.Error(), "", nil)
		m.mu.Lock()
		delete(m.items, id)
		m.mu.Unlock()
		go m.watch(t)
		return Transfer{}, problem(500, "STAGING_CREATE_FAILED", "无法创建受限临时文件", err)
	}
	if err := file.Close(); err != nil {
		_ = m.finish(t, StateFailed, "STAGING_CREATE_FAILED", err.Error(), "", nil)
		m.mu.Lock()
		delete(m.items, id)
		m.mu.Unlock()
		go m.watch(t)
		return Transfer{}, problem(500, "STAGING_CREATE_FAILED", "无法创建受限临时文件", err)
	}
	go m.watch(t)
	if err := ctx.Err(); err != nil {
		_ = m.finish(t, StateCancelled, "CREATE_CANCELLED", "传输创建已取消", "", nil)
		return Transfer{}, err
	}
	if t.ctx.Err() != nil {
		_ = m.finish(t, StateCancelled, "SESSION_ENDED", "人工会话已结束", "", nil)
		return Transfer{}, problem(410, "HUMAN_SESSION_CLOSED", "人工会话已结束", nil)
	}
	return m.snapshot(t), nil
}

func validateMetadata(req *CreateRequest) error {
	if req.Purpose != "file" && req.Purpose != "apk" {
		return problem(422, "INVALID_TRANSFER_PURPOSE", "传输用途必须是 file 或 apk", nil)
	}
	if req.FileName == "" || !utf8.ValidString(req.FileName) || strings.ContainsRune(req.FileName, 0) || strings.ContainsAny(req.FileName, `/\\`) || req.FileName == "." || req.FileName == ".." || len(req.FileName) > 255 {
		return problem(422, "INVALID_FILE_NAME", "文件名无效", nil)
	}
	if req.Purpose == "file" {
		if req.TargetPath == "" {
			req.TargetPath = "/sdcard/Download/" + req.FileName
		}
		if !utf8.ValidString(req.TargetPath) || strings.IndexByte(req.TargetPath, 0) >= 0 || len(req.TargetPath) > 1024 {
			return problem(422, "INVALID_TARGET_PATH", "设备目标路径无效", nil)
		}
	} else {
		if req.Size == 0 || !strings.EqualFold(filepath.Ext(req.FileName), ".apk") {
			return problem(422, "INVALID_APK", "APK 文件名或大小无效", nil)
		}
		if req.TargetPath != "" {
			return problem(422, "INVALID_TARGET_PATH", "APK 安装不接受目标文件路径", nil)
		}
		req.Overwrite = false
	}
	return nil
}

func (m *Manager) insertJournal(ctx context.Context, t *transfer) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transfer journal: %w", err)
	}
	defer tx.Rollback()
	now := t.value.CreatedAt.UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES(?,?,?, ?,?,?,?,'pending',?)`, t.value.ID, t.deviceID, localOwnerKind, t.sessionID, m.config.BootID, Kind, t.locator, now); err != nil {
		return fmt.Errorf("journal transfer staging file: %w", err)
	}
	details, _ := json.Marshal(struct {
		FileName, Purpose string
		Size              int64
	}{t.value.FileName, t.value.Purpose, t.value.Size})
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,kind,status,created_at,result_code,safe_details) VALUES(?,?,?,?,?,?,?,?,?)`, t.value.ID, localOwnerKind, t.sessionID, t.deviceID, "file-transfer", StateUploading, now, "", string(details)); err != nil {
		return fmt.Errorf("journal transfer operation: %w", err)
	}
	return tx.Commit()
}

func (m *Manager) snapshot(t *transfer) Transfer { m.mu.Lock(); defer m.mu.Unlock(); return t.value }

func (m *Manager) watch(t *transfer) {
	defer m.workers.Done()
	select {
	case <-t.ctx.Done():
		m.mu.Lock()
		started := t.started
		claimed := t.claimed
		state := t.value.State
		m.mu.Unlock()
		if !started && state == StateUploading {
			if claimed {
				<-t.uploadDone
			}
			_ = m.finish(t, StateCancelled, "SESSION_ENDED", "人工会话已结束", "", nil)
		}
	case <-t.done:
	}
}

func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("transfer close context is required")
	}
	m.mu.Lock()
	items := make([]*transfer, 0, len(m.items))
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	for _, t := range m.items {
		items = append(items, t)
		t.cancel()
	}
	m.mu.Unlock()
	var finishErrors []error
	for _, t := range items {
		select {
		case <-t.done:
			m.mu.Lock()
			if t.finishErr != nil {
				finishErrors = append(finishErrors, t.finishErr)
			}
			m.mu.Unlock()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wait := make(chan struct{})
	go func() { m.workers.Wait(); close(wait) }()
	select {
	case <-wait:
		return errors.Join(finishErrors...)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) update(t *transfer, mutate func(*Transfer)) Transfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	mutate(&t.value)
	t.value.UpdatedAt = time.Now().UTC()
	return t.value
}

func (m *Manager) setStatus(t *transfer, state, code, message string) error {
	value := m.update(t, func(v *Transfer) { v.State = state; v.ErrorCode = code; v.Error = message })
	_, err := m.db.ExecContext(m.ctx, `UPDATE operation_records SET status=?,result_code=?,finished_at=CASE WHEN ? IN ('succeeded','failed','cancelled','unknown') THEN ? ELSE NULL END WHERE id=? AND actor_kind=? AND actor_id=?`, state, code, state, time.Now().UTC().UnixMilli(), value.ID, localOwnerKind, t.sessionID)
	return err
}

func (m *Manager) finish(t *transfer, state, code, message, result string, exit *uint8) error {
	m.mu.Lock()
	if t.closed {
		err := t.finishErr
		m.mu.Unlock()
		return err
	}
	if t.finishing {
		done := t.done
		m.mu.Unlock()
		<-done
		m.mu.Lock()
		err := t.finishErr
		m.mu.Unlock()
		return err
	}
	t.finishing = true
	value := t.value
	value.State, value.ErrorCode, value.Error = state, code, message
	value.Result, value.ExitCode, value.UpdatedAt = result, exit, time.Now().UTC()
	m.mu.Unlock()

	var cleanupErr error
	if err := os.Remove(t.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanupErr = err
	}
	resourceState, resourceError := "cleaned", any(nil)
	if cleanupErr != nil {
		resourceState, resourceError = "failed", "TRANSFER_TEMP_CLEANUP_FAILED"
		if state == StateSucceeded {
			code, message = "LOCAL_CLEANUP_FAILED", "设备操作成功，但本地临时文件清理待恢复"
		}
	}
	details, _ := json.Marshal(struct {
		Purpose          string `json:"purpose"`
		Size             int64  `json:"size"`
		BytesTransferred int64  `json:"bytesTransferred"`
	}{value.Purpose, value.Size, value.BytesTransferred})

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	tx, err := m.db.BeginTx(cleanupCtx, nil)
	if err == nil {
		var changed sql.Result
		changed, err = tx.ExecContext(cleanupCtx, `UPDATE owned_resources SET state=?,cleanup_error=? WHERE id=? AND owner_kind=? AND owner_id=? AND boot_id=? AND kind=? AND locator=? AND state!='cleaned'`, resourceState, resourceError, value.ID, localOwnerKind, t.sessionID, m.config.BootID, Kind, t.locator)
		if err == nil {
			var count int64
			count, err = changed.RowsAffected()
			if err == nil && count != 1 {
				err = errors.New("transfer staging ownership changed before completion")
			}
		}
	}
	if err == nil {
		var changed sql.Result
		changed, err = tx.ExecContext(cleanupCtx, `UPDATE operation_records SET status=?,finished_at=?,result_code=?,safe_details=? WHERE id=? AND actor_kind=? AND actor_id=?`, value.State, value.UpdatedAt.UnixMilli(), code, string(details), value.ID, localOwnerKind, t.sessionID)
		if err == nil {
			var count int64
			count, err = changed.RowsAffected()
			if err == nil && count != 1 {
				err = errors.New("transfer operation record disappeared before completion")
			}
		}
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	cancel()

	finalState, finalCode, finalMessage := state, code, message
	if err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
		finalState, finalCode, finalMessage = StateUnknown, "TRANSFER_RESULT_PERSISTENCE_FAILED", "设备操作结果无法持久化"
	}
	finishErr := cleanupErr
	m.mu.Lock()
	t.value.State, t.value.ErrorCode, t.value.Error = finalState, finalCode, finalMessage
	t.value.Result, t.value.ExitCode, t.value.UpdatedAt = result, exit, value.UpdatedAt
	t.closed, t.finishing, t.finishErr = true, false, finishErr
	m.mu.Unlock()
	t.cancel()
	t.scope.Close()
	close(t.done)
	if finishErr != nil {
		m.logger.Error("transfer finalization incomplete", "errorType", "TRANSFER_FINALIZATION_FAILED", "transfer_id", value.ID)
	}
	return finishErr
}

func (m *Manager) authorizedTransfer(ctx context.Context, owner, id string) (*transfer, error) {
	m.mu.Lock()
	t := m.items[id]
	if t == nil {
		m.mu.Unlock()
		return nil, problem(404, "TRANSFER_NOT_FOUND", "文件传输不存在", nil)
	}
	if t.owner != owner {
		m.mu.Unlock()
		return nil, problem(403, "TRANSFER_OWNER_MISMATCH", "不能访问其他登录会话的文件传输", nil)
	}
	sessionID := t.sessionID
	m.mu.Unlock()
	scope, err := m.scopes.SessionScope(ctx, owner, sessionID, true)
	if err != nil {
		return nil, err
	}
	defer scope.Close()
	if scope.DeviceID != t.deviceID {
		return nil, problem(410, "TRANSFER_LINK_CHANGED", "原人工会话设备已变化", nil)
	}
	if err := scope.Check(); err != nil {
		return nil, err
	}
	return t, nil
}

func (m *Manager) Get(ctx context.Context, owner, id string) (Transfer, error) {
	t, err := m.authorizedTransfer(ctx, owner, id)
	if err != nil {
		return Transfer{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return t.value, nil
}
