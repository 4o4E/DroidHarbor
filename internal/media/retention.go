package media

import (
	"context"
	"time"
)

const sessionRetention = 15 * time.Second

type retainedSession struct {
	timer *time.Timer
}

// Suspend/resume share the ordered control channel with device input. Commands
// before suspension finish first; HTTP operations lose their activation context.
// The peer, RTP sender and browser decoder remain alive without input authority.
func (s *session) suspend(ctx context.Context) error {
	h := s.hub
	h.changeMu.Lock()
	h.inputMu.Lock()
	s.mu.Lock()
	if s.suspended {
		s.mu.Unlock()
		h.inputMu.Unlock()
		h.changeMu.Unlock()
		return nil
	}
	s.suspended = true
	s.cancelInteraction()
	s.mu.Unlock()
	h.cancelClipboard(s)
	h.mu.Lock()
	source, width, height := h.source, h.width, h.height
	h.mu.Unlock()
	var err error
	if source != nil {
		err = h.inputs.release(ctx, s.id, width, height, source.Send, func() error { return ctx.Err() })
	} else {
		h.inputs.discard(s.id)
		err = problem(503, "CAPTURE_UNAVAILABLE", "采集当前不可用")
	}
	h.inputMu.Unlock()
	h.changeMu.Unlock()
	if err == nil {
		err = s.manager.retainSession(s)
	}
	if err != nil {
		s.beginEnd(err)
	}
	return err
}

func (s *session) resume(ctx context.Context) error {
	h := s.hub
	h.changeMu.Lock()
	defer h.changeMu.Unlock()
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	s.mu.Lock()
	suspended := s.suspended
	s.mu.Unlock()
	if !suspended {
		return s.check(ctx, false)
	}
	s.manager.clearRetention(s)
	if err := s.check(ctx, false); err != nil {
		s.beginEnd(err)
		return err
	}
	h.mu.Lock()
	source := h.source
	h.mu.Unlock()
	if source == nil {
		err := problem(503, "CAPTURE_UNAVAILABLE", "采集当前不可用")
		s.beginEnd(err)
		return err
	}
	if err := h.activateDisplay(ctx, source, func() error { return s.check(ctx, false) }); err != nil {
		s.beginEnd(err)
		return err
	}
	s.mu.Lock()
	s.interactionCtx, s.cancelInteraction = context.WithCancel(s.ctx)
	s.suspended, s.ready = false, false
	s.mu.Unlock()
	s.manager.deps.Logger.Info("media_session_resumed", "sessionId", s.id, "deviceId", h.deviceID)
	return nil
}

func (s *session) interaction() (context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended {
		return nil, problem(409, "SESSION_SUSPENDED", "后台窗口仅保留视频，请先切回该设备")
	}
	return s.interactionCtx, nil
}

// Each suspended window owns its deadline. Returning cancels only that timer;
// the next departure starts a fresh interval. All windows count as sessions.
func (m *Manager) retainSession(s *session) error {
	m.mu.Lock()
	if m.closed || s.ctx.Err() != nil {
		m.mu.Unlock()
		return problem(410, "HUMAN_SESSION_CLOSED", "人工会话已结束")
	}
	if s.retention != nil {
		m.mu.Unlock()
		return nil
	}
	entry := &retainedSession{}
	s.retention = entry
	entry.timer = time.AfterFunc(sessionRetention, func() {
		m.mu.Lock()
		current := s.retention == entry
		if current {
			m.detachRetentionLocked(s)
			s.cancel()
		}
		m.mu.Unlock()
		if current {
			m.deps.Logger.Info("media_session_retention_expired", "sessionId", s.id, "deviceId", s.hub.deviceID)
			s.beginEnd(nil)
		}
	})
	m.mu.Unlock()
	m.deps.Logger.Info("media_session_retained", "sessionId", s.id, "deviceId", s.hub.deviceID, "retentionMs", sessionRetention.Milliseconds())
	return nil
}

func (m *Manager) clearRetention(s *session) {
	m.mu.Lock()
	m.detachRetentionLocked(s)
	m.mu.Unlock()
}

func (m *Manager) detachRetentionLocked(s *session) {
	entry := s.retention
	if entry == nil {
		return
	}
	entry.timer.Stop()
	s.retention = nil
}
