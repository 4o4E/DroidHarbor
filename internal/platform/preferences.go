package platform

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"droidharbor/internal/diagnostics"
	"droidharbor/internal/events"
	"droidharbor/internal/identity"
)

type systemPreferences struct {
	AudioDefault string    `json:"audioDefault"`
	WindowScales []float64 `json:"windowScales"`
	AudioGainMax float64   `json:"audioGainMax"`
	Revision     int64     `json:"revision"`
}

func (s *Server) systemPreferences(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, r.Method == http.MethodPatch)
	if !ok {
		return
	}
	var current systemPreferences
	var encoded string
	if r.Method == http.MethodGet {
		if err := s.DB.QueryRowContext(r.Context(), `SELECT audio_default,window_scales,audio_gain_max,revision FROM system_preferences WHERE id=1`).Scan(&current.AudioDefault, &encoded, &current.AudioGainMax, &current.Revision); err != nil {
			s.domainError(w, r, err)
			return
		}
		if err := json.Unmarshal([]byte(encoded), &current.WindowScales); err != nil {
			s.domainError(w, r, err)
			return
		}
		s.json(w, 200, current)
		return
	}
	var body systemPreferences
	if !s.decode(w, r, &body, false) {
		return
	}
	if body.Revision < 1 || (body.AudioDefault != "muted" && body.AudioDefault != "device" && body.AudioDefault != "browser") || len(body.WindowScales) == 0 {
		s.fail(w, r, 422, "INVALID_PREFERENCES", "请选择音频默认值并提供至少一个正数倍率")
		return
	}
	if body.AudioGainMax < 1 || math.IsNaN(body.AudioGainMax) || math.IsInf(body.AudioGainMax, 0) {
		s.fail(w, r, 422, "INVALID_PREFERENCES", "音频增强上限必须是至少 1.0 的有限数")
		return
	}
	seen := make(map[float64]bool, len(body.WindowScales))
	for _, value := range body.WindowScales {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) || seen[value] {
			s.fail(w, r, 422, "INVALID_PREFERENCES", "倍率必须是互不重复的有限正数")
			return
		}
		seen[value] = true
	}
	data, err := json.Marshal(body.WindowScales)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(r.Context(), `SELECT revision FROM system_preferences WHERE id=1`).Scan(&current.Revision); err != nil {
		s.domainError(w, r, err)
		return
	}
	if current.Revision != body.Revision || body.Revision == math.MaxInt64 {
		s.fail(w, r, 409, "PREFERENCES_CONFLICT", "系统设置已变化，请重新读取后保存")
		return
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE system_preferences SET audio_default=?,window_scales=?,audio_gain_max=?,revision=revision+1 WHERE id=1`, body.AudioDefault, string(data), body.AudioGainMax); err != nil {
		s.domainError(w, r, err)
		return
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO operation_records(id,actor_kind,actor_id,kind,status,created_at,finished_at,result_code) VALUES(?,'admin-session',?,'system.preferences','succeeded',?,?,'SAVED')`, identity.Secret(), owner, now, now); err != nil {
		s.domainError(w, r, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.domainError(w, r, err)
		return
	}
	body.Revision++
	if s.Events != nil {
		if _, err = s.Events.Publish(events.Change{EntityType: "system-preferences", EntityID: "global"}); err != nil {
			s.Logger.Error("preferences_notification_failed", "cause", diagnostics.Cause(err))
		}
	}
	s.json(w, 200, body)
}
