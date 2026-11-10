package devices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"droidharbor/internal/identity"
)

type DiscoveredRegistration struct {
	Registered
	Verified bool `json:"verified"`
	Existing bool `json:"existing"`
}

// An explicit add-all action authorizes probing its selected discovered phones.
// Match trusted hardware before creating rows, so USB/Wi-Fi appearances become
// links of one device rather than a second pending device record.
// displayName names only a new physical device; an associated link keeps its owner's name.
func (r *Registry) RegisterDiscovered(ctx context.Context, actorID, endpoint, displayName string) (DiscoveredRegistration, error) {
	r.mutations.Lock()
	defer r.mutations.Unlock()
	displayName = strings.TrimSpace(displayName)
	if utf8.RuneCountInString(displayName) > 120 {
		return DiscoveredRegistration{}, &Error{Code: "INVALID_INPUT", Message: "设备名称须为 1–120 个字符", Status: 422}
	}
	r.mu.RLock()
	observed, found := r.discovered[endpoint]
	epoch, version := r.epoch, r.version
	usable := r.ready && found && !r.ambiguous[endpoint]
	r.mu.RUnlock()
	if !usable {
		return DiscoveredRegistration{}, &Error{Code: "DEVICE_DISCONNECTED", Message: "待添加设备已离开当前发现清单，请重新读取", Status: 503}
	}
	kind := "usb"
	if _, _, err := net.SplitHostPort(endpoint); err == nil {
		kind = "wifi"
	}
	if err := validateLink(LinkRequest{Kind: kind, Endpoint: endpoint}); err != nil {
		return DiscoveredRegistration{}, err
	}
	var result DiscoveredRegistration
	err := r.db.QueryRowContext(ctx, `SELECT device_id,id,enabled FROM device_links WHERE endpoint=? OR observed_serial=? LIMIT 1`, endpoint, endpoint).Scan(&result.DeviceID, &result.LinkID, &result.Verified)
	if err == nil {
		result.Existing = true
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	var evidence *Evidence
	if observed.State == "device" && observed.TransportID != 0 {
		probeCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(epoch, cancel)
		value, err := r.probe(probeCtx, observed.TransportID)
		stop()
		cancel()
		if err != nil {
			return DiscoveredRegistration{}, err
		}
		evidence = &value
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DiscoveredRegistration{}, err
	}
	defer tx.Rollback()
	if evidence != nil {
		rows, err := tx.QueryContext(ctx, `SELECT device_id,identity_evidence FROM device_links WHERE identity_evidence IS NOT NULL`)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var owner, encoded string
			if err := rows.Scan(&owner, &encoded); err != nil {
				rows.Close()
				return result, err
			}
			var existing Evidence
			if err := json.Unmarshal([]byte(encoded), &existing); err != nil {
				rows.Close()
				return result, err
			}
			if existing.Serial != evidence.Serial {
				continue
			}
			if !existing.SameHardware(*evidence) || (result.DeviceID != "" && result.DeviceID != owner) {
				rows.Close()
				return result, &Error{Code: "IDENTITY_CONFLICT", Message: "设备身份冲突，请在设备管理中核对链路归属", Status: 409}
			}
			result.DeviceID = owner
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return result, err
		}
	}
	result.Existing = result.DeviceID != ""
	if result.DeviceID == "" {
		name := displayName
		if name == "" && evidence != nil {
			name = evidence.Model
		}
		if name == "" {
			name = strings.TrimSpace(strings.ReplaceAll(observed.Properties["model"], "_", " "))
		}
		if name == "" {
			name = endpoint
		}
		result.DeviceID = identity.Secret()
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices(id,display_name,allocation_state,video_max_fps,video_bit_rate) VALUES(?,?,'maintenance',60,12000000)`, result.DeviceID, name); err != nil {
			return result, err
		}
	}
	result.LinkID = identity.Secret()
	var encoded any
	if evidence != nil {
		data, err := json.Marshal(evidence)
		if err != nil {
			return result, err
		}
		encoded = string(data)
		result.Verified = true
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_links(id,device_id,kind,endpoint,observed_serial,identity_evidence,enabled,last_seen_at) VALUES(?,?,?,?,?,?,?,?)`, result.LinkID, result.DeviceID, kind, endpoint, observed.Serial, encoded, result.Verified, time.Now().UnixMilli()); err != nil {
		return result, err
	}
	if err := record(ctx, tx, actorID, result.DeviceID, result.LinkID, "device.discovered.register"); err != nil {
		return result, err
	}
	r.mu.RLock()
	current := r.ready && r.version == version && epoch.Err() == nil
	r.mu.RUnlock()
	if !current {
		return result, &Error{Code: "DEVICE_STATE_CHANGED", Message: "添加期间设备状态已变化，请重新读取清单", Status: 409}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	r.changed(result.DeviceID)
	return result, nil
}
