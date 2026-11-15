package platform

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) sessionAudio(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, r.Method == http.MethodPatch)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		snapshot, err := s.Media.Audio(r.Context(), owner, r.PathValue("id"))
		if err != nil {
			s.domainError(w, r, err)
			return
		}
		s.json(w, 200, snapshot)
		return
	}
	var body struct {
		Route    string `json:"route"`
		Revision uint64 `json:"revision"`
	}
	if !s.decode(w, r, &body, false) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	snapshot, err := s.Media.PatchAudio(ctx, owner, r.PathValue("id"), body.Route, body.Revision)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 200, snapshot)
}
