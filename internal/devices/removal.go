package devices

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"droidharbor/internal/leases"
)

type Removal struct {
	Version                  string `json:"version"`
	ConfirmedLeaseGeneration *int64 `json:"confirmedLeaseGeneration,omitempty"`
}

// SetRemovalCleanup withdraws new human sessions and pauses the online controller
// while retaining verified links for strictly-owned native resource cleanup.
func (r *Registry) SetRemovalCleanup(cleanup func(context.Context, string) (func(), error)) {
	r.removalCleanup = cleanup
}

func (r *Registry) Remove(ctx context.Context, actorID, deviceID string, request Removal) error {
	r.mutations.Lock()
	defer r.mutations.Unlock()
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id=? AND removed_at IS NULL)`, deviceID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return &Error{Code: "DEVICE_NOT_FOUND", Message: "设备不存在", Status: 404}
	}
	r.mu.RLock()
	matches := request.Version != "" && request.Version == r.configVersionLocked(deviceID)
	r.mu.RUnlock()
	if !matches {
		return &Error{Code: "VERSION_CONFLICT", Message: "设备配置已变化，请重新读取后确认删除", Status: 409}
	}
	if r.removalCleanup == nil {
		return &Error{Code: "REMOVAL_UNAVAILABLE", Message: "设备删除服务尚未初始化", Status: 503}
	}
	// Maintenance blocks racing claims and revokes the confirmed AI generation.
	if err := r.manager.SetMaintenance(ctx, deviceID, true, actorID, request.ConfirmedLeaseGeneration); err != nil {
		if errors.Is(err, leases.ErrAllocationConfirmation) {
			return &Error{Code: "ALLOCATION_CONFIRMATION_REQUIRED", Message: "AI 使用状态已变化，请重新确认删除", Status: 409}
		}
		return err
	}
	release, err := r.removalCleanup(ctx, deviceID)
	if release != nil {
		defer release()
	}
	if err != nil {
		r.changed(deviceID)
		return &Error{Code: "DEVICE_REMOVAL_FAILED", Message: "平台资源尚未完整回收，设备保留为不可分配，可恢复连接后重试删除", Status: 503, Cause: err}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var outstanding bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owned_resources WHERE device_id=? AND state!='cleaned')`, deviceID).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding {
		return &Error{Code: "RESOURCE_RECOVERY_REQUIRED", Message: "仍有未确认回收的平台资源，设备保留，请恢复连接后重试", Status: 503}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET removed_at=?,maintenance_requested=1,allocation_state='maintenance' WHERE id=? AND removed_at IS NULL`, time.Now().UnixMilli(), deviceID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE device_links SET enabled=0 WHERE device_id=?`, deviceID); err != nil {
		return err
	}
	if err = record(ctx, tx, actorID, deviceID, "", "device.remove"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	r.mu.Lock()
	for id, current := range r.live {
		if current.link.DeviceID == deviceID {
			current.cancel()
			delete(r.live, id)
		}
	}
	r.version++
	r.configVersions[deviceID]++
	r.notifyLocked(deviceID)
	r.enqueueLocked()
	r.mu.Unlock()
	return nil
}

// Explicit registration may revive the original hardware identity, not its old
// tasks. Generation/history remain intact and allocation starts disabled.
func restoreRegistration(ctx context.Context, tx *sql.Tx, deviceID string) error {
	var removed bool
	if err := tx.QueryRowContext(ctx, `SELECT removed_at IS NOT NULL FROM devices WHERE id=?`, deviceID).Scan(&removed); err != nil {
		return err
	}
	if !removed {
		return nil
	}
	var outstanding bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owned_resources WHERE device_id=? AND state!='cleaned')`, deviceID).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding {
		return &Error{Code: "RESOURCE_RECOVERY_REQUIRED", Message: "原设备资源尚未回收，不能重新添加", Status: 409}
	}
	_, err := tx.ExecContext(ctx, `UPDATE devices SET removed_at=NULL,maintenance_requested=1,allocation_state='maintenance',last_error=NULL WHERE id=? AND removed_at IS NOT NULL`, deviceID)
	return err
}
