package devices

import (
	"context"
	"errors"

	"droidharbor/internal/diagnostics"
	"droidharbor/internal/leases"
)

type Removal struct {
	Version                  string `json:"version"`
	ConfirmedLeaseGeneration *int64 `json:"confirmedLeaseGeneration,omitempty"`
}

// SetRemovalCleanup withdraws local access and attempts to retire owned
// resources while the verified link is still available.
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
	if err := r.manager.RevokeForRemoval(ctx, deviceID, actorID, request.ConfirmedLeaseGeneration); err != nil {
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
		r.logger.Warn("device_removal_cleanup_incomplete", "deviceId", deviceID, "cause", diagnostics.Cause(err))
	}
	if err = r.manager.PurgeRemovedDevice(ctx, deviceID); err != nil {
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
	delete(r.configVersions, deviceID)
	r.notifyLocked(deviceID)
	if r.ready {
		r.enqueueLocked()
	}
	r.mu.Unlock()
	return nil
}
