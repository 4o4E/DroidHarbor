package platform

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"droidharbor/internal/media"
	"github.com/pion/webrtc/v4"
)

func (s *Server) mediaAdmin(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if r.Header.Get("Authorization") != "" {
		s.fail(w, r, 403, "ADMIN_SESSION_REQUIRED", "网页操作需要管理员网页会话")
		return "", false
	}
	owner, ok := s.authorize(w, r, write)
	if !ok {
		return "", false
	}
	if s.Media == nil {
		s.fail(w, r, 503, "MEDIA_UNAVAILABLE", "投屏服务暂不可用")
		return "", false
	}
	return owner, true
}

func (s *Server) createMediaSession(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Mode  string `json:"mode"`
		Offer string `json:"offer"`
	}
	if !s.decodeLimit(w, r, &body, false, 96<<10) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	snapshot, err := s.Media.Join(ctx, owner, r.PathValue("id"), body.Mode, body.Offer)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, http.StatusCreated, snapshot)
}

func (s *Server) addMediaCandidate(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, true)
	if !ok {
		return
	}
	var body webrtc.ICECandidateInit
	if !s.decode(w, r, &body, false) {
		return
	}
	if err := s.Media.AddCandidate(r.Context(), owner, r.PathValue("id"), body); err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) endMediaSession(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, true)
	if !ok {
		return
	}
	reason := r.URL.Query().Get("reason")
	nextDeviceID := r.URL.Query().Get("nextDeviceId")
	if reason != "" && reason != "switch" && reason != "cancel-switch" || (reason == "") != (nextDeviceID == "") {
		s.fail(w, r, 422, "INVALID_REQUEST", "切换原因和目标设备必须同时提供")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var err error
	if reason == "cancel-switch" {
		err = s.Media.CancelSwitch(ctx, owner, r.PathValue("id"), nextDeviceID)
	} else {
		err = s.Media.End(ctx, owner, r.PathValue("id"), nextDeviceID)
	}
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) videoSettings(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, false)
	if !ok {
		return
	}
	snapshot, err := s.Media.Video(r.Context(), owner, r.PathValue("id"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, snapshot)
}

func (s *Server) patchVideoSettings(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, true)
	if !ok {
		return
	}
	var body media.VideoPatch
	if !s.decode(w, r, &body, false) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	snapshot, err := s.Media.PatchVideo(ctx, owner, r.PathValue("id"), body)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, snapshot)
}

func (s *Server) originalScreenshot(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.mediaAdmin(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	shot, err := s.Media.OriginalScreenshot(ctx, owner, r.PathValue("id"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	defer func() {
		if err := shot.Reader.Close(); err != nil {
			s.Logger.Info("screenshot_close_failed", "requestId", r.Context().Value(requestKey{}))
		}
	}()
	if _, ok := s.authorize(w, r, false); !ok {
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		s.fail(w, r, 503, "SCREENSHOT_DELIVERY_UNAVAILABLE", "无法限定截图下载连接的等待时间")
		return
	}
	defer func() {
		if err := controller.SetWriteDeadline(time.Time{}); err != nil {
			s.Logger.Info("screenshot_deadline_reset_failed", "requestId", r.Context().Value(requestKey{}))
		}
	}()
	w.Header().Set("Content-Type", shot.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(shot.Size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="screenshot.png"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-DroidHarbor-Image-Width", strconv.Itoa(shot.Width))
	w.Header().Set("X-DroidHarbor-Image-Height", strconv.Itoa(shot.Height))
	if _, err := io.Copy(w, shot.Reader); err != nil {
		s.Logger.Info("screenshot_delivery_interrupted", "requestId", r.Context().Value(requestKey{}))
		// A partial image is never followed by a JSON success/error body.
		panic(http.ErrAbortHandler)
	}
}
