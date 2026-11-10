package devices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"droidharbor/internal/identity"
)

type LinkRequest struct {
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
}

type Registration struct {
	DisplayName string `json:"displayName"`
	LinkRequest
}

type Registered struct {
	DeviceID string `json:"deviceId"`
	LinkID   string `json:"linkId"`
}

func validateLink(request LinkRequest) error {
	if request.Endpoint == "" || strings.ContainsAny(request.Endpoint, " \t\r\n\x00") {
		return &Error{Code: "INVALID_REQUEST", Message: "设备 endpoint 无效", Status: 422}
	}
	switch request.Kind {
	case "usb":
		return nil
	case "wifi":
		host, port, err := net.SplitHostPort(request.Endpoint)
		if err == nil && host != "" {
			number, parseErr := strconv.ParseUint(port, 10, 16)
			if parseErr == nil && number > 0 {
				return nil
			}
		}
	}
	return &Error{Code: "INVALID_REQUEST", Message: "链路必须为 USB serial 或含端口的 Wi-Fi 地址", Status: 422}
}

// Register only records administrator intent. A discovered endpoint is never
// opened or made allocatable until explicit identity verification succeeds.
func (r *Registry) Register(ctx context.Context, actorID string, request Registration) (Registered, error) {
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if request.DisplayName == "" || utf8.RuneCountInString(request.DisplayName) > 120 {
		return Registered{}, &Error{Code: "INVALID_REQUEST", Message: "设备名称须为 1 至 120 个字符", Status: 422}
	}
	if err := validateLink(request.LinkRequest); err != nil {
		return Registered{}, err
	}
	r.mutations.Lock()
	defer r.mutations.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Registered{}, err
	}
	defer tx.Rollback()
	if err = unusedEndpoint(ctx, tx, request.Endpoint); err != nil {
		return Registered{}, err
	}
	result := Registered{DeviceID: identity.Secret(), LinkID: identity.Secret()}
	if _, err = tx.ExecContext(ctx, `INSERT INTO devices(id,display_name,allocation_state) VALUES(?,?,'maintenance')`, result.DeviceID, request.DisplayName); err != nil {
		return Registered{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO device_links(id,device_id,kind,endpoint) VALUES(?,?,?,?)`, result.LinkID, result.DeviceID, request.Kind, request.Endpoint); err != nil {
		return Registered{}, err
	}
	if err = record(ctx, tx, actorID, result.DeviceID, result.LinkID, "device.register"); err != nil {
		return Registered{}, err
	}
	if err = tx.Commit(); err != nil {
		return Registered{}, err
	}
	r.changed(result.DeviceID)
	return result, nil
}

func (r *Registry) AddLink(ctx context.Context, actorID, deviceID string, request LinkRequest) (Registered, error) {
	if err := validateLink(request); err != nil {
		return Registered{}, err
	}
	r.mutations.Lock()
	defer r.mutations.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Registered{}, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id=?)`, deviceID).Scan(&exists); err != nil {
		return Registered{}, err
	}
	if !exists {
		return Registered{}, &Error{Code: "DEVICE_NOT_FOUND", Message: "设备不存在", Status: 404}
	}
	if err = unusedEndpoint(ctx, tx, request.Endpoint); err != nil {
		return Registered{}, err
	}
	result := Registered{DeviceID: deviceID, LinkID: identity.Secret()}
	if _, err = tx.ExecContext(ctx, `INSERT INTO device_links(id,device_id,kind,endpoint) VALUES(?,?,?,?)`, result.LinkID, deviceID, request.Kind, request.Endpoint); err != nil {
		return Registered{}, err
	}
	if err = record(ctx, tx, actorID, deviceID, result.LinkID, "device.link.add"); err != nil {
		return Registered{}, err
	}
	if err = tx.Commit(); err != nil {
		return Registered{}, err
	}
	r.changed(deviceID)
	return result, nil
}

type LinkPatch struct {
	Version  string `json:"version"`
	Endpoint string `json:"endpoint"`
}

// Changing a wireless address preserves device/link identity, but withdraws
// the old transport. The new endpoint must be explicitly verified against the
// retained hardware evidence before it can serve the existing lease again.
func (r *Registry) PatchLink(ctx context.Context, actorID, deviceID, linkID string, patch LinkPatch) error {
	if err := validateLink(LinkRequest{Kind: "wifi", Endpoint: patch.Endpoint}); err != nil {
		return err
	}
	r.mutations.Lock()
	defer r.mutations.Unlock()
	link, kind, err := r.registered(ctx, deviceID, linkID)
	if err != nil {
		return err
	}
	if kind != "wifi" {
		return &Error{Code: "INVALID_REQUEST", Message: "只有 Wi-Fi 链路支持更新连接地址", Status: 422}
	}
	r.mu.RLock()
	matches := patch.Version != "" && patch.Version == r.configVersionLocked(deviceID)
	r.mu.RUnlock()
	if !matches {
		return &Error{Code: "VERSION_CONFLICT", Message: "设备配置已变化，请刷新后重试", Status: 409}
	}
	if patch.Endpoint == link.endpoint {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = unusedEndpoint(ctx, tx, patch.Endpoint); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE device_links SET endpoint=?,observed_serial=NULL,enabled=0 WHERE id=?`, patch.Endpoint, linkID); err != nil {
		return err
	}
	if err = record(ctx, tx, actorID, deviceID, linkID, "device.link.update"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	r.configVersions[deviceID]++
	if old := r.live[linkID]; old != nil {
		old.cancel()
		delete(r.live, linkID)
	}
	delete(r.issues, linkID)
	delete(r.linkStates, linkID)
	r.notifyLocked(deviceID)
	if r.ready {
		r.enqueueLocked()
	}
	return nil
}

func unusedEndpoint(ctx context.Context, tx *sql.Tx, endpoint string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM device_links WHERE endpoint=?)`, endpoint).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return &Error{Code: "ENDPOINT_REGISTERED", Message: "该 endpoint 已登记，请使用已有链路", Status: 409}
	}
	return nil
}

func record(ctx context.Context, tx *sql.Tx, actorID, deviceID, linkID, kind string) error {
	details, err := json.Marshal(struct {
		LinkID string `json:"linkId,omitempty"`
	}{linkID})
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,kind,status,created_at,finished_at,result_code,safe_details) VALUES(?,'admin',?,?,?,'succeeded',?,?,'OK',?)`, identity.Secret(), actorID, deviceID, kind, now, now, string(details))
	return err
}

func (r *Registry) changed(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configVersions[deviceID]++
	r.version++
	r.notifyLocked(deviceID)
	if r.ready {
		r.enqueueLocked()
	}
}

func (r *Registry) registered(ctx context.Context, deviceID, linkID string) (registeredLink, string, error) {
	var link registeredLink
	var kind string
	err := r.db.QueryRowContext(ctx, `SELECT id,device_id,endpoint,coalesce(observed_serial,endpoint),identity_evidence,enabled,kind FROM device_links WHERE id=? AND device_id=?`, linkID, deviceID).Scan(&link.id, &link.deviceID, &link.endpoint, &link.serial, &link.evidence, &link.enabled, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return link, kind, &Error{Code: "LINK_NOT_FOUND", Message: "设备链路不存在", Status: 404}
	}
	return link, kind, err
}

// Pair does not persist the pairing code, nor treat successful pairing as proof
// of device identity. The pairing port is independent of the connect endpoint.
func (r *Registry) Pair(ctx context.Context, actorID, deviceID, linkID, pairingEndpoint, code string) error {
	_, kind, err := r.registered(ctx, deviceID, linkID)
	if err != nil {
		return err
	}
	if kind != "wifi" {
		return &Error{Code: "INVALID_REQUEST", Message: "只有 Wi-Fi 链路支持配对", Status: 422}
	}
	if err = validateLink(LinkRequest{Kind: "wifi", Endpoint: pairingEndpoint}); err != nil {
		return err
	}
	if code == "" || strings.ContainsAny(code, ":\x00\r\n") {
		return &Error{Code: "INVALID_REQUEST", Message: "配对码格式无效", Status: 422}
	}
	operationID, err := r.beginNetworkAction(ctx, actorID, deviceID, linkID, "device.link.pair")
	if err != nil {
		return err
	}
	if _, err = r.client.Pair(ctx, pairingEndpoint, code); err != nil {
		return unknownAction(err)
	}
	if _, err = r.db.ExecContext(ctx, `UPDATE operation_records SET status='succeeded',result_code='OK',finished_at=? WHERE id=?`, time.Now().UTC().UnixMilli(), operationID); err != nil {
		return unknownAction(err)
	}
	return nil
}

func (r *Registry) Connect(ctx context.Context, actorID, deviceID, linkID string) error {
	link, kind, err := r.registered(ctx, deviceID, linkID)
	if err != nil {
		return err
	}
	if kind != "wifi" {
		return &Error{Code: "INVALID_REQUEST", Message: "USB 链路由设备发现接入，无需网络连接", Status: 422}
	}
	operationID, err := r.beginNetworkAction(ctx, actorID, deviceID, linkID, "device.link.connect")
	if err != nil {
		return err
	}
	result, err := r.client.Connect(ctx, link.endpoint)
	if err != nil {
		return unknownAction(err)
	}
	prefix := "connected to "
	if strings.HasPrefix(strings.ToLower(result), "already connected to ") {
		prefix = "already connected to "
	}
	serial := result[len(prefix):]
	if err = validateLink(LinkRequest{Kind: "wifi", Endpoint: serial}); err != nil {
		return &Error{Code: "ADB_RESULT_UNKNOWN", Message: "连接请求已发送，但无法确认返回的设备地址", Status: 502}
	}
	r.mutations.Lock()
	defer r.mutations.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return unknownAction(err)
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE device_links SET observed_serial=? WHERE id=? AND endpoint=?`, serial, linkID, link.endpoint)
	if err != nil {
		return unknownAction(err)
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return unknownAction(err)
	}
	if count != 1 {
		return unknownAction(errors.New("link endpoint changed during ADB connect"))
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operation_records SET status='succeeded',result_code='OK',finished_at=? WHERE id=?`, time.Now().UTC().UnixMilli(), operationID); err != nil {
		return unknownAction(err)
	}
	if err = tx.Commit(); err != nil {
		return unknownAction(err)
	}
	// A changed endpoint mapping invalidates existing streams before any probe.
	r.mu.Lock()
	r.version++
	r.configVersions[deviceID]++
	if old := r.live[linkID]; old != nil && old.serial != serial {
		old.cancel()
		delete(r.live, linkID)
	}
	delete(r.linkStates, linkID)
	r.notifyLocked(deviceID)
	if r.ready {
		r.enqueueLocked()
	}
	r.mu.Unlock()
	return nil
}

func (r *Registry) Verify(ctx context.Context, actorID, deviceID, linkID string) (Evidence, error) {
	r.mutations.Lock()
	defer r.mutations.Unlock()
	link, _, err := r.registered(ctx, deviceID, linkID)
	if err != nil {
		return Evidence{}, err
	}
	r.mu.RLock()
	observed, found := r.discovered[link.serial]
	epoch, version := r.epoch, r.version
	usable := r.ready && found && !r.ambiguous[link.serial] && observed.State == "device" && observed.TransportID != 0
	r.mu.RUnlock()
	if !usable {
		return Evidence{}, &Error{Code: "DEVICE_DISCONNECTED", Message: "链路尚未连接或手机尚未授权", Status: 503}
	}
	probeCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()
	evidence, err := r.probe(probeCtx, observed.TransportID)
	if err != nil {
		return Evidence{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Evidence{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT device_id,identity_evidence FROM device_links WHERE identity_evidence IS NOT NULL ORDER BY id`)
	if err != nil {
		return Evidence{}, err
	}
	for rows.Next() {
		var owner, encoded string
		if err = rows.Scan(&owner, &encoded); err != nil {
			rows.Close()
			return Evidence{}, err
		}
		var existing Evidence
		if err = json.Unmarshal([]byte(encoded), &existing); err != nil {
			rows.Close()
			return Evidence{}, err
		}
		if (owner == deviceID && !existing.SameHardware(evidence)) || (owner != deviceID && existing.Serial == evidence.Serial) {
			rows.Close()
			return Evidence{}, &Error{Code: "IDENTITY_CONFLICT", Message: "设备身份冲突；请关联正确设备，不创建第二个可分配对象", Status: 409}
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return Evidence{}, err
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return Evidence{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE device_links SET identity_evidence=?,observed_serial=?,enabled=1,last_seen_at=? WHERE id=?`, string(encoded), observed.Serial, time.Now().UTC().UnixMilli(), linkID); err != nil {
		return Evidence{}, err
	}
	if err = record(ctx, tx, actorID, deviceID, linkID, "device.link.verify"); err != nil {
		return Evidence{}, err
	}
	r.mu.RLock()
	current := r.ready && r.version == version && epoch.Err() == nil
	r.mu.RUnlock()
	if !current {
		return Evidence{}, &Error{Code: "DEVICE_DISCONNECTED", Message: "身份核验期间设备连接发生变化，请重新核验", Status: 503}
	}
	if err = tx.Commit(); err != nil {
		return Evidence{}, err
	}
	r.changed(deviceID)
	return evidence, nil
}

// Refresh reconciles an already registered phone after its authorization was
// handled on-device. It neither pairs/connects endpoints nor changes allocation.
func (r *Registry) Refresh(ctx context.Context, actorID, deviceID string) error {
	if err := r.refreshInventory(ctx); err != nil {
		return err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT l.id,coalesce(l.observed_serial,l.endpoint) FROM devices d LEFT JOIN device_links l ON l.device_id=d.id WHERE d.id=? ORDER BY CASE WHEN l.kind='usb' THEN 0 ELSE 1 END,l.id`, deviceID)
	if err != nil {
		return err
	}
	found, online, candidate := false, false, ""
	for rows.Next() {
		var linkID, serial sql.NullString
		if err = rows.Scan(&linkID, &serial); err != nil {
			rows.Close()
			return err
		}
		found = true
		if !linkID.Valid {
			continue
		}
		r.mu.RLock()
		if current := r.live[linkID.String]; current != nil && current.ctx.Err() == nil {
			online = true
		}
		observed, exists := r.discovered[serial.String]
		if candidate == "" && r.ready && exists && !r.ambiguous[serial.String] && observed.State == "device" && observed.TransportID != 0 {
			candidate = linkID.String
		}
		r.mu.RUnlock()
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if !found {
		return &Error{Code: "DEVICE_NOT_FOUND", Message: "设备不存在", Status: 404}
	}
	if !online {
		if candidate == "" {
			return &Error{Code: "DEVICE_DISCONNECTED", Message: "手机仍未授权或链路未连接；请完成手机确认后再同步", Status: 503}
		}
		if _, err = r.Verify(ctx, actorID, deviceID, candidate); err != nil {
			return err
		}
	}
	r.logger.Info("device_state_refreshed", "deviceId", deviceID)
	return nil
}

// Persist the action before external I/O. Until a confirmed result is durable,
// a crash or canceled request must leave an unknown result, not a false failure
// that encourages automatic replay. Neither endpoint nor pairing code is logged.
func (r *Registry) beginNetworkAction(ctx context.Context, actorID, deviceID, linkID, kind string) (string, error) {
	id := identity.Secret()
	details, err := json.Marshal(struct {
		LinkID string `json:"linkId"`
	}{linkID})
	if err != nil {
		return "", err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,kind,status,created_at,result_code,safe_details) VALUES(?,'admin',?,?,?,'unknown',?,'ADB_RESULT_UNKNOWN',?)`, id, actorID, deviceID, kind, time.Now().UTC().UnixMilli(), string(details))
	return id, err
}

func unknownAction(cause error) error {
	return &Error{Code: "ADB_RESULT_UNKNOWN", Message: "无法确认 ADB 操作结果；请检查手机和连接状态，不要自动重试", Status: 502, Cause: cause}
}
