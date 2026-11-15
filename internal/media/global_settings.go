package media

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"time"

	"droidharbor/internal/diagnostics"
	"droidharbor/internal/events"
	"droidharbor/internal/identity"
)

func (m *Manager) GlobalVideo(ctx context.Context, owner string) (GlobalVideoSnapshot, error) {
	if err := m.checkAdmin(ctx, owner); err != nil {
		return GlobalVideoSnapshot{}, err
	}
	var quality VideoQuality
	var revision int64
	if err := m.deps.DB.QueryRowContext(ctx, `SELECT max_fps,bit_rate,revision FROM global_video_settings WHERE id=1`).Scan(&quality.MaxFPS, &quality.BitRate, &revision); err != nil {
		return GlobalVideoSnapshot{}, &Error{Status: 503, Code: "STORAGE_UNAVAILABLE", Message: "无法读取全局画质", Cause: err}
	}
	return m.globalVideoSnapshot(quality, revision), nil
}

func (m *Manager) globalVideoSnapshot(quality VideoQuality, revision int64) GlobalVideoSnapshot {
	result := GlobalVideoSnapshot{Requested: quality, Revision: revision, Fallbacks: make([]VideoFallback, 0)}
	m.mu.Lock()
	for _, h := range m.hubs {
		h.mu.Lock()
		if h.fallback != nil {
			result.Fallbacks = append(result.Fallbacks, *h.fallback)
		}
		h.mu.Unlock()
	}
	m.mu.Unlock()
	sort.Slice(result.Fallbacks, func(i, j int) bool { return result.Fallbacks[i].DeviceID < result.Fallbacks[j].DeviceID })
	return result
}

// Saving a target never touches a live capture, input owner, or peer connection.
// A fresh capture reads its target atomically from SQLite, including after restart.
func (m *Manager) PatchGlobalVideo(ctx context.Context, owner string, patch GlobalVideoPatch) (snapshot GlobalVideoSnapshot, err error) {
	if err = m.checkAdmin(ctx, owner); err != nil {
		return snapshot, err
	}
	if patch.SessionID != "" {
		scope, scopeErr := m.SessionScope(ctx, owner, patch.SessionID, true)
		if scopeErr != nil {
			return snapshot, scopeErr
		}
		defer scope.Close()
		ctx = scope.Context
		if err = scope.Check(); err != nil {
			return snapshot, err
		}
	}
	if patch.Revision < 1 || (patch.MaxFPS == nil && patch.BitRate == nil) {
		return snapshot, problem(422, "INVALID_VIDEO_SETTINGS", "必须提供全局 revision 和要修改的参数")
	}
	tx, err := m.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return snapshot, err
	}
	defer func() {
		if rollback := tx.Rollback(); rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}()
	var quality VideoQuality
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT max_fps,bit_rate,revision FROM global_video_settings WHERE id=1`).Scan(&quality.MaxFPS, &quality.BitRate, &revision); err != nil {
		return snapshot, err
	}
	if revision != patch.Revision {
		return snapshot, problem(409, "VIDEO_SETTINGS_CONFLICT", "全局画质已改变，请读取最新设置")
	}
	before := quality
	if patch.MaxFPS != nil {
		quality.MaxFPS = *patch.MaxFPS
	}
	if patch.BitRate != nil {
		quality.BitRate = *patch.BitRate
	}
	if !validVideoQuality(quality) {
		return snapshot, problem(422, "INVALID_VIDEO_SETTINGS", "全局帧率或码率超出设备协议范围")
	}
	if quality == before {
		if err = tx.Commit(); err != nil {
			return snapshot, err
		}
		return m.globalVideoSnapshot(quality, revision), nil
	}
	if revision == math.MaxInt64 {
		return snapshot, problem(409, "VIDEO_REVISION_EXHAUSTED", "全局画质版本已达到上限")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE global_video_settings SET max_fps=?,bit_rate=?,revision=revision+1 WHERE id=1 AND revision=?`, quality.MaxFPS, quality.BitRate, revision); err != nil {
		return snapshot, err
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,kind,status,created_at,finished_at,result_code) VALUES(?,'admin-session',?,'video.global-settings','succeeded',?,?,'TARGET_SAVED')`, identity.Secret(), owner, now, now); err != nil {
		return snapshot, err
	}
	if err = tx.Commit(); err != nil {
		return snapshot, err
	}
	m.mu.Lock()
	for _, h := range m.hubs {
		h.mu.Lock()
		if h.source == nil && !h.starting {
			h.fallback = nil
		}
		h.mu.Unlock()
	}
	m.mu.Unlock()
	m.notifyGlobalVideo()
	return m.globalVideoSnapshot(quality, revision+1), nil
}

func validVideoQuality(quality VideoQuality) bool {
	return quality.MaxFPS > 0 && quality.MaxFPS <= math.MaxInt32 && quality.BitRate > 0 && quality.BitRate <= math.MaxInt32
}

func (m *Manager) notifyGlobalVideo() {
	if m.deps.Events == nil {
		return
	}
	if _, err := m.deps.Events.Publish(events.Change{EntityType: "video-settings", EntityID: "global"}); err != nil {
		m.deps.Logger.Error("global_video_notification_failed", "cause", diagnostics.Cause(err))
	}
}
