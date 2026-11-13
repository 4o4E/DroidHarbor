package platform

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/previews"
)

func (s *Server) devicePreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, false); !ok {
		return
	}
	if s.Previews == nil {
		s.fail(w, r, 503, "PREVIEW_UNAVAILABLE", "设备预览暂不可用")
		return
	}
	cachedOnly := false
	if value := r.URL.Query().Get("cached"); value != "" {
		var err error
		cachedOnly, err = strconv.ParseBool(value)
		if err != nil {
			s.fail(w, r, 400, "INVALID_REQUEST", "封面缓存参数无效")
			return
		}
	}
	var snapshot previews.Snapshot
	var err error
	if cachedOnly {
		snapshot, err = s.Previews.Cached(r.Context(), r.PathValue("id"))
	} else {
		snapshot, err = s.Previews.Get(r.Context(), r.PathValue("id"))
	}
	if err != nil {
		s.previewError(w, r, err)
		return
	}
	// Authentication is checked again after any cache wait or device I/O.
	if _, ok := s.authorize(w, r, false); !ok {
		return
	}
	if !s.Previews.Validate(r.Context(), snapshot) {
		s.previewError(w, r, previews.ErrLinkChanged)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-DroidHarbor-Captured-At", snapshot.CapturedAt.UTC().Format(time.RFC3339Nano))
	w.Header().Set("X-DroidHarbor-Device-ID", snapshot.DeviceID)
	w.Header().Set("X-DroidHarbor-Link-Generation", strconv.FormatUint(snapshot.LinkGeneration, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(snapshot.JPEG); err != nil {
		s.Logger.Info("preview_response_write_failed", "requestId", r.Context().Value(requestKey{}), "cause", diagnostics.Cause(err))
	}
}

func (s *Server) previewError(w http.ResponseWriter, r *http.Request, err error) {
	var deviceErr *devices.Error
	if errors.As(err, &deviceErr) {
		s.domainError(w, r, deviceErr)
		return
	}
	switch {
	case errors.Is(err, previews.ErrNotReady):
		s.fail(w, r, 404, "PREVIEW_NOT_READY", "设备初始封面尚不可用")
	case errors.Is(err, previews.ErrRateLimited):
		s.fail(w, r, 503, "PREVIEW_RATE_LIMITED", "预览采集频率受限，请稍后重试")
	case errors.Is(err, previews.ErrCapacity):
		s.fail(w, r, 503, "PREVIEW_BUSY", "预览处理容量已满")
	case errors.Is(err, previews.ErrLinkChanged):
		s.fail(w, r, 503, "DEVICE_UNAVAILABLE", "设备链路已变化，请重新请求")
	case errors.Is(err, previews.ErrInputTooLarge), errors.Is(err, previews.ErrImageTooLarge), errors.Is(err, previews.ErrInvalidImage):
		s.fail(w, r, 503, "PREVIEW_CAPTURE_FAILED", "设备截图无效或超出预览限制")
	default:
		s.Logger.Warn("preview_capture_failed", "requestId", r.Context().Value(requestKey{}), "deviceId", r.PathValue("id"), "cause", diagnostics.Cause(err))
		s.fail(w, r, 503, "PREVIEW_CAPTURE_FAILED", "无法获取设备预览")
	}
}
