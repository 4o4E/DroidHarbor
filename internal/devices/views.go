package devices

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type LinkView struct {
	ID               string `json:"linkId"`
	Kind             string `json:"kind"`
	Endpoint         string `json:"endpoint"`
	ConnectionState  string `json:"connectionState"`
	IdentityVerified bool   `json:"identityVerified"`
	LastError        string `json:"lastError,omitempty"`
}

type View struct {
	ID                   string     `json:"deviceId"`
	DisplayName          string     `json:"displayName"`
	Tags                 []string   `json:"tags"`
	ConnectionState      string     `json:"connectionState"`
	AllocationState      string     `json:"allocationState"`
	Version              string     `json:"version,omitempty"`
	MaintenanceRequested *bool      `json:"maintenanceRequested,omitempty"`
	Links                []LinkView `json:"links,omitempty"`
}

type Discovered struct {
	Serial      string `json:"serial"`
	State       string `json:"state"`
	TransportID uint64 `json:"transportId"`
	Model       string `json:"model,omitempty"`
	Product     string `json:"product,omitempty"`
}

func (r *Registry) Discover() ([]Discovered, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready {
		return nil, &Error{Code: "ADB_UNAVAILABLE", Message: "ADB 设备发现当前不可用", Status: 503}
	}
	result := make([]Discovered, 0, len(r.discovered))
	for _, device := range r.discovered {
		result = append(result, Discovered{Serial: device.Serial, State: device.State, TransportID: device.TransportID, Model: device.Properties["model"], Product: device.Properties["product"]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Serial < result[j].Serial })
	return result, nil
}

func (r *Registry) List(ctx context.Context, admin bool) ([]View, bool, error) {
	r.mutations.Lock()
	defer r.mutations.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,display_name,allocation_state,maintenance_requested FROM devices ORDER BY id`)
	if err != nil {
		return nil, false, err
	}
	result := []View{}
	indices := make(map[string]int)
	for rows.Next() {
		var view View
		var maintenance bool
		if err = rows.Scan(&view.ID, &view.DisplayName, &view.AllocationState, &maintenance); err != nil {
			rows.Close()
			return nil, false, err
		}
		view.Tags = []string{}
		view.ConnectionState = "offline"
		if admin {
			view.MaintenanceRequested = &maintenance
			view.Links = []LinkView{}
		}
		indices[view.ID] = len(result)
		result = append(result, view)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT device_id,tag FROM device_tags ORDER BY device_id,tag`)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var deviceID, tag string
		if err = rows.Scan(&deviceID, &tag); err != nil {
			rows.Close()
			return nil, false, err
		}
		index := indices[deviceID]
		result[index].Tags = append(result[index].Tags, tag)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT device_id,id,kind,endpoint,coalesce(observed_serial,endpoint),enabled FROM device_links ORDER BY id`)
	if err != nil {
		return nil, false, err
	}
	type storedLink struct {
		deviceID, serial string
		view             LinkView
	}
	var links []storedLink
	for rows.Next() {
		var link storedLink
		if err = rows.Scan(&link.deviceID, &link.view.ID, &link.view.Kind, &link.view.Endpoint, &link.serial, &link.view.IdentityVerified); err != nil {
			rows.Close()
			return nil, false, err
		}
		links = append(links, link)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	// Finish all SQL before acquiring the inventory lock: disk waits must not
	// delay cancellation of a disconnected transport.
	r.mu.RLock()
	ready := r.ready
	for _, link := range links {
		view, serial, deviceID := link.view, link.serial, link.deviceID
		view.ConnectionState = "offline"
		observed, exists := r.discovered[serial]
		if ready && exists && !r.ambiguous[serial] {
			switch observed.State {
			case "device":
				view.ConnectionState = "online"
			case "unauthorized", "authorizing":
				view.ConnectionState = "unauthorized"
			}
		}
		index := indices[deviceID]
		if current := r.live[view.ID]; current != nil && current.ctx.Err() == nil {
			result[index].ConnectionState = "online"
		} else if result[index].ConnectionState != "online" && view.ConnectionState == "unauthorized" {
			result[index].ConnectionState = "unauthorized"
		}
		view.LastError = r.issues[view.ID]
		if admin {
			result[index].Links = append(result[index].Links, view)
		}
	}
	if admin {
		for index := range result {
			result[index].Version = r.configVersionLocked(result[index].ID)
		}
	}
	r.mu.RUnlock()
	return result, ready, nil
}

func (r *Registry) configVersionLocked(deviceID string) string {
	return fmt.Sprintf("%s:%d", r.bootID, r.configVersions[deviceID])
}

type Patch struct {
	Version              string    `json:"version"`
	DisplayName          *string   `json:"displayName,omitempty"`
	Tags                 *[]string `json:"tags,omitempty"`
	MaintenanceRequested *bool     `json:"maintenanceRequested,omitempty"`
}

func (r *Registry) Patch(ctx context.Context, actorID, deviceID string, patch Patch) error {
	metadata := patch.DisplayName != nil || patch.Tags != nil
	// Allocation changes include asynchronous external cleanup. They are a
	// separate action, never a falsely atomic batch with name/tag changes.
	if (patch.MaintenanceRequested != nil && metadata) || (patch.MaintenanceRequested == nil && !metadata) {
		return &Error{Code: "INVALID_REQUEST", Message: "请分别提交设备信息修改与分配启停", Status: 422}
	}
	if patch.DisplayName != nil {
		*patch.DisplayName = strings.TrimSpace(*patch.DisplayName)
		if *patch.DisplayName == "" || utf8.RuneCountInString(*patch.DisplayName) > 120 {
			return &Error{Code: "INVALID_REQUEST", Message: "设备名称须为 1 至 120 个字符", Status: 422}
		}
	}
	var tags []string
	if patch.Tags != nil {
		seen := make(map[string]bool, len(*patch.Tags))
		for _, value := range *patch.Tags {
			tag := strings.TrimSpace(value)
			if tag == "" || !utf8.ValidString(tag) {
				return &Error{Code: "INVALID_REQUEST", Message: "标签不能为空", Status: 422}
			}
			if !seen[tag] {
				seen[tag] = true
				tags = append(tags, tag)
			}
		}
		sort.Strings(tags)
	}
	r.mutations.Lock()
	defer r.mutations.Unlock()
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id=?)`, deviceID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return &Error{Code: "DEVICE_NOT_FOUND", Message: "设备不存在", Status: 404}
	}
	r.mu.RLock()
	matches := patch.Version != "" && patch.Version == r.configVersionLocked(deviceID)
	r.mu.RUnlock()
	if !matches {
		return &Error{Code: "VERSION_CONFLICT", Message: "设备配置已变化，请刷新后重试", Status: 409}
	}
	if patch.MaintenanceRequested != nil {
		err := r.manager.SetMaintenance(ctx, deviceID, *patch.MaintenanceRequested, actorID)
		// The intent may be durable even when cleanup fails; all clients must
		// refresh before another allocation action rather than reuse the revision.
		r.changed(deviceID)
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if patch.DisplayName != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET display_name=? WHERE id=?`, *patch.DisplayName, deviceID); err != nil {
			return err
		}
	}
	if patch.Tags != nil {
		if _, err = tx.ExecContext(ctx, `DELETE FROM device_tags WHERE device_id=?`, deviceID); err != nil {
			return err
		}
		for _, tag := range tags {
			if _, err = tx.ExecContext(ctx, `INSERT INTO device_tags(device_id,tag) VALUES(?,?)`, deviceID, tag); err != nil {
				return err
			}
		}
	}
	if err = record(ctx, tx, actorID, deviceID, "", "device.update"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	r.changed(deviceID)
	return nil
}
