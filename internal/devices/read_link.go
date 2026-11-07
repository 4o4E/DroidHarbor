package devices

import (
	"context"
	"database/sql"
	"errors"

	"droidharbor/internal/leases"
)

// ReadLink pins a verified live link and its current generation. It never
// changes link selection during the lifetime of the returned context.
func (r *Registry) ReadLink(ctx context.Context, deviceID string) (leases.Link, context.Context, error) {
	if err := ctx.Err(); err != nil {
		return leases.Link{}, nil, err
	}
	r.mu.RLock()
	ready := r.ready
	var selected *liveLink
	if ready {
		for _, current := range r.live {
			if current.link.DeviceID != deviceID || current.ctx.Err() != nil {
				continue
			}
			if selected == nil || lessPreviewLink(current.link, selected.link) {
				selected = current
			}
		}
	}
	if selected != nil {
		link, linkCtx := selected.link, selected.ctx
		r.mu.RUnlock()
		return link, linkCtx, nil
	}
	r.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return leases.Link{}, nil, err
	}
	var exists int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE id=?`, deviceID).Scan(&exists)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return leases.Link{}, nil, ctxErr
	}
	if errors.Is(err, sql.ErrNoRows) {
		return leases.Link{}, nil, &Error{Code: "DEVICE_NOT_FOUND", Message: "设备不存在", Status: 404}
	}
	if err != nil {
		return leases.Link{}, nil, &Error{Code: "DEVICE_UNAVAILABLE", Message: "设备状态暂不可用", Status: 503, Cause: err}
	}
	if !ready {
		return leases.Link{}, nil, &Error{Code: "DEVICE_UNAVAILABLE", Message: "设备发现服务暂不可用", Status: 503}
	}
	return leases.Link{}, nil, &Error{Code: "DEVICE_UNAVAILABLE", Message: "设备当前不可用", Status: 503}
}

func lessPreviewLink(a, b leases.Link) bool {
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
