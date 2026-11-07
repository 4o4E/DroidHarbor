package media

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"droidharbor/internal/identity"
)

func (m *Manager) getHub(ctx context.Context, deviceID string) (*hub, error) {
	m.mu.Lock()
	h := m.hubs[deviceID]
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, problem(503, "MEDIA_CLOSED", "媒体服务正在停止")
	}
	if h != nil {
		return h, nil
	}
	var requested VideoParameters
	var revision int64
	err := m.deps.DB.QueryRowContext(ctx, `SELECT video_max_size,video_max_fps,video_bit_rate,video_revision FROM devices WHERE id=?`, deviceID).Scan(&requested.MaxSize, &requested.MaxFPS, &requested.BitRate, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "DEVICE_NOT_FOUND", "设备不存在")
	}
	if err != nil {
		return nil, &Error{Status: 503, Code: "STORAGE_UNAVAILABLE", Message: "无法读取画质配置", Cause: err}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, problem(503, "MEDIA_CLOSED", "媒体服务正在停止")
	}
	if h = m.hubs[deviceID]; h == nil {
		h = &hub{manager: m, deviceID: deviceID, members: make(map[string]*session), requested: requested, revision: revision, status: "pending", clipboardWrites: make(map[uint64]*clipboardRequest)}
		m.hubs[deviceID] = h
	}
	return h, nil
}

func (h *hub) snapshot() VideoSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := VideoSnapshot{Requested: h.requested, Revision: h.revision, Status: h.status, CaptureEpoch: h.epoch, ErrorCode: h.errorCode}
	if h.effective != nil {
		effective := *h.effective
		if time.Since(h.lastFrameAt) > 2*time.Second {
			effective.FramesPerSecond = 0
			effective.EncodedBitsPerSecond = 0
		}
		result.Effective = &effective
	}
	return result
}

func (m *Manager) Video(ctx context.Context, owner, deviceID string) (VideoSnapshot, error) {
	if err := m.checkAdmin(ctx, owner); err != nil {
		return VideoSnapshot{}, err
	}
	h, err := m.getHub(ctx, deviceID)
	if err != nil {
		return VideoSnapshot{}, err
	}
	return h.snapshot(), nil
}

func (m *Manager) PatchVideo(ctx context.Context, owner, deviceID string, patch VideoPatch) (VideoSnapshot, error) {
	if err := m.checkAdmin(ctx, owner); err != nil {
		return VideoSnapshot{}, err
	}
	authorize := func() error { return m.checkAdmin(ctx, owner) }
	if patch.SessionID != "" {
		s, err := m.getSession(ctx, owner, patch.SessionID, true)
		if err != nil {
			return VideoSnapshot{}, err
		}
		if s.hub.deviceID != deviceID {
			return VideoSnapshot{}, problem(403, "SESSION_DEVICE_MISMATCH", "人工会话不属于该设备")
		}
		authorize = func() error { return s.check(ctx, true) }
	}
	if patch.Revision < 1 || (patch.MaxSize == nil && patch.MaxFPS == nil && patch.BitRate == nil) {
		return VideoSnapshot{}, problem(422, "INVALID_VIDEO_SETTINGS", "必须提供当前 revision 和要修改的参数")
	}
	h, err := m.getHub(ctx, deviceID)
	if err != nil {
		return VideoSnapshot{}, err
	}
	h.changeMu.Lock()
	defer h.changeMu.Unlock()
	if err := authorize(); err != nil {
		return VideoSnapshot{}, err
	}
	h.mu.Lock()
	requested, revision := h.requested, h.revision
	h.mu.Unlock()
	if patch.Revision != revision {
		return h.snapshot(), problem(409, "VIDEO_SETTINGS_CONFLICT", "画质配置已改变，请读取当前值后重新提交")
	}
	if patch.MaxSize != nil {
		requested.MaxSize = *patch.MaxSize
	}
	if patch.MaxFPS != nil {
		requested.MaxFPS = *patch.MaxFPS
	}
	if patch.BitRate != nil {
		requested.BitRate = *patch.BitRate
	}
	if requested.MaxSize <= 0 || requested.MaxSize > math.MaxUint16 || requested.MaxFPS <= 0 || requested.MaxFPS > math.MaxInt32 || requested.BitRate <= 0 || requested.BitRate > math.MaxInt32 {
		return VideoSnapshot{}, problem(422, "INVALID_VIDEO_SETTINGS", "画质参数超出设备协议的数值范围")
	}
	h.mu.Lock()
	unchanged := requested == h.requested
	h.mu.Unlock()
	if unchanged {
		return h.snapshot(), nil
	}
	if revision == math.MaxInt64 {
		return VideoSnapshot{}, problem(409, "VIDEO_REVISION_EXHAUSTED", "画质配置版本已达到上限")
	}
	tx, err := m.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return VideoSnapshot{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE devices SET video_max_size=?,video_max_fps=?,video_bit_rate=?,video_revision=video_revision+1 WHERE id=? AND video_revision=?`, requested.MaxSize, requested.MaxFPS, requested.BitRate, deviceID, revision)
	if err != nil {
		return VideoSnapshot{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return VideoSnapshot{}, err
	}
	if count != 1 {
		return VideoSnapshot{}, problem(409, "VIDEO_SETTINGS_CONFLICT", "画质配置版本不匹配")
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,kind,status,created_at,finished_at,result_code) VALUES(?,'admin-session',?,?,'video.settings','succeeded',?,?,'REQUESTED_SAVED')`, identity.Secret(), owner, deviceID, now, now)
	if err != nil {
		return VideoSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return VideoSnapshot{}, err
	}
	h.mu.Lock()
	h.requested, h.revision, h.errorCode = requested, revision+1, ""
	active := len(h.members) != 0
	if active {
		h.status = "reconfiguring"
	} else {
		h.status = "pending"
		h.effective = nil
	}
	h.mu.Unlock()
	m.notifyDevice(deviceID)
	if active {
		if err := h.restart(ctx, authorize); err != nil {
			return h.snapshot(), err
		}
	}
	return h.snapshot(), nil
}
