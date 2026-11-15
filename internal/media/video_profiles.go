package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
)

// A profile is evidence from an actual key frame, not a per-device preference.
type videoProfile struct {
	TargetRevision int64           `json:"targetRevision"`
	Target         VideoQuality    `json:"target"`
	Applied        VideoParameters `json:"applied"`
}

func (p VideoParameters) quality() VideoQuality {
	return VideoQuality{MaxFPS: p.MaxFPS, BitRate: p.BitRate}
}

func (m *Manager) captureConfiguration(ctx context.Context, deviceID string) (VideoParameters, int64, int64, *videoProfile, error) {
	var target VideoParameters
	var deviceRevision, globalRevision int64
	var encoded sql.NullString
	err := m.deps.DB.QueryRowContext(ctx, `SELECT d.video_max_size,g.max_fps,g.bit_rate,d.video_revision,g.revision,d.video_last_good FROM devices d LEFT JOIN global_video_settings g ON g.id=1 WHERE d.id=?`, deviceID).Scan(&target.MaxSize, &target.MaxFPS, &target.BitRate, &deviceRevision, &globalRevision, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return target, 0, 0, nil, problem(404, "DEVICE_NOT_FOUND", "设备不存在")
	}
	if err != nil {
		return target, 0, 0, nil, &Error{Status: 503, Code: "STORAGE_UNAVAILABLE", Message: "无法读取全局采集配置", Cause: err}
	}
	var profile *videoProfile
	if encoded.Valid {
		profile = new(videoProfile)
		if err := json.Unmarshal([]byte(encoded.String), profile); err != nil || profile.TargetRevision < 1 || !validVideoQuality(profile.Target) || !validVideoQuality(profile.Applied.quality()) || profile.Applied.MaxSize < 1 || profile.Applied.MaxSize > math.MaxUint16 {
			return target, 0, 0, nil, problem(503, "INVALID_VIDEO_PROFILE", "设备成功画质缓存无效")
		}
	}
	return target, deviceRevision, globalRevision, profile, nil
}

type videoChoices struct {
	values [3]VideoParameters
	count  int
}

func captureChoices(target VideoParameters, revision int64, lastGood *videoProfile) videoChoices {
	var choices videoChoices
	add := func(value VideoParameters) {
		for _, previous := range choices.values[:choices.count] {
			if previous == value {
				return
			}
		}
		choices.values[choices.count] = value
		choices.count++
	}
	usable := lastGood != nil && lastGood.Applied.MaxSize == target.MaxSize && lastGood.Applied.MaxFPS <= target.MaxFPS && lastGood.Applied.BitRate <= target.BitRate
	if usable && lastGood.TargetRevision == revision && lastGood.Target == target.quality() {
		// This target has already succeeded at a compatible fallback profile.
		add(lastGood.Applied)
	} else {
		add(target)
	}
	if usable {
		add(lastGood.Applied)
	}
	add(VideoParameters{MaxSize: target.MaxSize, MaxFPS: min(target.MaxFPS, 30), BitRate: min(target.BitRate, 4_000_000)})
	return choices
}

func (m *Manager) saveVideoProfile(ctx context.Context, deviceID string, profile videoProfile) error {
	encoded, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	result, err := m.deps.DB.ExecContext(ctx, `UPDATE devices SET video_last_good=? WHERE id=?`, string(encoded), deviceID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return problem(404, "DEVICE_NOT_FOUND", "保存成功画质时设备已移除")
	}
	return nil
}
