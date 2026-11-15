package platform

import "net/http"

func (s *Server) sessionVolume(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, r.Method == http.MethodPatch)
	if !ok {
		return
	}
	var level *int
	if r.Method == http.MethodPatch {
		var body struct {
			Level *int `json:"level"`
		}
		if !s.decode(w, r, &body, false) {
			return
		}
		if body.Level == nil {
			s.fail(w, r, 422, "INVALID_VOLUME", "请指定设备媒体音量")
			return
		}
		level = body.Level
	}
	volume, err := s.Media.DeviceMediaVolume(r.Context(), owner, r.PathValue("id"), level)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 200, volume)
}
