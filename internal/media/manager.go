package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"droidharbor/internal/events"
	"droidharbor/internal/identity"
	"github.com/pion/webrtc/v4"
)

func New(ctx context.Context, deps Dependencies) *Manager {
	lifetime, cancel := context.WithCancel(ctx)
	m := &Manager{
		deps:            deps,
		ctx:             lifetime,
		cancel:          cancel,
		sessions:        make(map[string]*session),
		hubs:            make(map[string]*hub),
		screenshotSlots: make(chan struct{}, 2),
	}
	m.watcher.Add(1)
	go m.watch()
	return m
}

func (m *Manager) checkAdmin(ctx context.Context, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var valid int
	err := m.deps.DB.QueryRowContext(ctx, `SELECT 1 FROM admin_sessions WHERE id=? AND admin_credential_version=? AND revoked_at IS NULL AND expires_at>?`, owner, m.deps.AdminVersion, time.Now().UnixMilli()).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return problem(401, "SESSION_EXPIRED", "管理会话已失效")
	}
	if err != nil {
		return &Error{Status: 503, Code: "AUTHORIZATION_UNAVAILABLE", Message: "无法核验当前会话权限", Cause: err}
	}
	return nil
}

func (s *session) check(ctx context.Context, write bool) error {
	if s.ctx.Err() != nil {
		return problem(410, "HUMAN_SESSION_CLOSED", "人工会话已结束")
	}
	if write && s.mode != "control" {
		return problem(403, "READ_ONLY_SESSION", "只读会话不能执行设备写操作")
	}
	return s.manager.checkAdmin(ctx, s.owner)
}

func (m *Manager) getSession(ctx context.Context, owner, id string, write bool) (*session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil, problem(410, "HUMAN_SESSION_CLOSED", "人工会话已结束")
	}
	if s.owner != owner {
		return nil, problem(403, "SESSION_OWNER_MISMATCH", "不能访问其他登录会话的窗口")
	}
	if err := s.check(ctx, write); err != nil {
		return nil, err
	}
	return s, nil
}

func (m *Manager) Join(ctx context.Context, owner, deviceID, mode, offer string) (SessionSnapshot, error) {
	if mode != "readonly" && mode != "control" {
		return SessionSnapshot{}, problem(422, "INVALID_MODE", "必须明确指定只读或控制模式")
	}
	if len(offer) == 0 || len(offer) > 64<<10 {
		return SessionSnapshot{}, problem(422, "INVALID_OFFER", "WebRTC offer 无效")
	}
	if err := m.checkAdmin(ctx, owner); err != nil {
		return SessionSnapshot{}, err
	}
	h, err := m.getHub(ctx, deviceID)
	if err != nil {
		return SessionSnapshot{}, err
	}
	lifetime, cancel := context.WithCancel(m.ctx)
	now := time.Now()
	s := &session{
		manager: m, hub: h, id: identity.Secret(), owner: owner, mode: mode,
		ctx: lifetime, cancel: cancel, created: now, awaitingKey: true,
		peerReady: make(chan struct{}), frames: make(chan *videoFrame, maxQueuedFrames),
		commands: make(chan []byte, 64), output: make(chan []byte, 64), done: make(chan struct{}),
	}
	m.mu.Lock()
	if m.closed || len(m.sessions) >= maxSessions {
		m.mu.Unlock()
		cancel()
		return SessionSnapshot{}, problem(503, "MEDIA_CAPACITY", "媒体会话暂不可用")
	}
	m.sessions[s.id] = s
	m.mu.Unlock()

	h.changeMu.Lock()
	h.mu.Lock()
	h.members[s.id] = s
	h.mu.Unlock()
	if err = s.check(ctx, false); err == nil {
		err = h.ensureSource(ctx, func() error { return s.check(ctx, false) })
	}
	h.changeMu.Unlock()
	if err != nil {
		s.beginEnd(err)
		return SessionSnapshot{}, err
	}
	if err = ctx.Err(); err != nil {
		s.beginEnd(err)
		return SessionSnapshot{}, err
	}
	acceptCtx, cancelAccept := context.WithTimeout(m.ctx, negotiationTimeout)
	stopRequest := context.AfterFunc(ctx, cancelAccept)
	peer, err := m.deps.Transport.Accept(acceptCtx, offer, rtcCallbacks(s))
	stopRequest()
	cancelAccept()
	if err != nil {
		s.beginEnd(err)
		return SessionSnapshot{}, &Error{Status: 422, Code: "MEDIA_NEGOTIATION_FAILED", Message: "WebRTC 协商失败", Cause: err}
	}
	s.mu.Lock()
	s.peer = peer
	s.peerStarted = time.Now()
	alive := s.ctx.Err() == nil
	if alive {
		s.workers.Add(3)
	}
	s.mu.Unlock()
	if !alive {
		_ = peer.Close()
		return SessionSnapshot{}, problem(410, "HUMAN_SESSION_CLOSED", "人工会话已结束")
	}
	go s.runVideo()
	go s.runCommands()
	go s.runOutput()
	if err := m.record(ctx, s, "human-session-open", "succeeded", ""); err != nil {
		s.beginEnd(err)
		return SessionSnapshot{}, &Error{Status: 503, Code: "STORAGE_UNAVAILABLE", Message: "无法记录人工会话", Cause: err}
	}
	m.notifyDevice(deviceID)
	return SessionSnapshot{SessionID: s.id, DeviceID: deviceID, Mode: mode, Answer: peer.Answer(), ControlProtocol: "droidharbor.control.v1"}, nil
}

func (m *Manager) AddCandidate(ctx context.Context, owner, id string, candidate webrtc.ICECandidateInit) error {
	s, err := m.getSession(ctx, owner, id, false)
	if err != nil {
		return err
	}
	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()
	if peer == nil {
		return problem(409, "SESSION_STARTING", "人工会话尚未完成协商")
	}
	if err := peer.AddCandidate(candidate); err != nil {
		return &Error{Status: 422, Code: "INVALID_CANDIDATE", Message: "ICE candidate 无效", Cause: err}
	}
	return nil
}

func (m *Manager) End(ctx context.Context, owner, id string) error {
	if err := m.checkAdmin(ctx, owner); err != nil {
		return err
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	if s.owner != owner {
		return problem(403, "SESSION_OWNER_MISMATCH", "不能关闭其他登录会话的窗口")
	}
	s.beginEnd(nil)
	select {
	case <-s.done:
		return s.endErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) CloseAdmin(ctx context.Context, owner string) error {
	m.mu.Lock()
	var affected []*session
	for _, s := range m.sessions {
		if s.owner == owner {
			affected = append(affected, s)
		}
	}
	m.mu.Unlock()
	for _, s := range affected {
		s.beginEnd(nil)
	}
	var result error
	for _, s := range affected {
		select {
		case <-s.done:
			result = errors.Join(result, s.endErr)
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	return result
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	m.cancel()
	for _, s := range all {
		s.beginEnd(nil)
	}
	var result error
	for _, s := range all {
		select {
		case <-s.done:
			result = errors.Join(result, s.endErr)
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	m.watcher.Wait()
	return result
}

func (s *session) beginEnd(reason error) {
	s.endOnce.Do(func() {
		s.cancel()
		// At most one finisher per reserved session. Its capacity slot is retained
		// until cleanup completes; peer callbacks never wait on device cleanup.
		go s.finish(reason)
	})
}

func (s *session) finish(reason error) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	h := s.hub
	h.changeMu.Lock()
	h.inputMu.Lock()
	h.mu.Lock()
	source, width, height := h.source, h.width, h.height
	cleanupPending := h.cleanupPending
	h.mu.Unlock()
	h.cancelClipboard(s)
	var cleanupErr, inputErr, stopErr error
	if source != nil {
		inputErr = h.inputs.release(ctx, s.id, width, height, source.Send, func() error { return ctx.Err() })
	} else {
		h.inputs.discard(s.id)
	}
	cleanupErr = errors.Join(cleanupErr, inputErr)

	h.mu.Lock()
	delete(h.members, s.id)
	last := len(h.members) == 0
	if last {
		if h.status != "failed" {
			h.status = "stopping"
		}
		h.effective = nil
		if source != nil && !cleanupPending {
			h.source = nil
			h.cleanupSource = source
			h.cleanupPending = true
		}
	}
	h.mu.Unlock()
	h.inputMu.Unlock()
	if last && source != nil && !cleanupPending {
		stopErr = source.Stop(ctx)
	}
	if last {
		h.cancelAllClipboard()
	}
	cleanupErr = errors.Join(cleanupErr, stopErr)
	if last && !cleanupPending {
		h.captureStopped(stopErr, inputErr)
	}
	if inputErr != nil && stopErr == nil {
		h.fail(inputErr)
	}
	h.changeMu.Unlock()

	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()
	if peer != nil {
		cleanupErr = errors.Join(cleanupErr, peer.Close())
	}
	s.workers.Wait()
	code := errorCode(reason)
	status := "succeeded"
	if cleanupErr != nil {
		status = "failed"
		code = "SESSION_CLEANUP_FAILED"
	}
	cleanupErr = errors.Join(cleanupErr, s.manager.record(ctx, s, "human-session-close", status, code))
	s.manager.mu.Lock()
	delete(s.manager.sessions, s.id)
	s.manager.mu.Unlock()
	if cleanupErr != nil {
		s.endErr = &Error{Status: 503, Code: "SESSION_CLEANUP_FAILED", Message: "人工会话回收未完整完成", Cause: cleanupErr}
	}
	close(s.done)
	if last {
		s.manager.notifyDevice(h.deviceID)
	}
}

// captureStopped keeps runtime/start/reconfiguration failures visible after the
// final viewer leaves; only a clean, nonfailed stop returns the hub to idle.
func (h *hub) captureStopped(stopErr, inputErr error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if stopErr != nil {
		h.cleanupPending = true
		h.status, h.errorCode, h.effective = "failed", errorCode(stopErr), nil
		return
	}
	h.source, h.cleanupSource = nil, nil
	h.cleanupPending = false
	wasFailed := h.status == "failed"
	if wasFailed || inputErr != nil {
		h.status, h.effective = "failed", nil
		if !wasFailed && inputErr != nil {
			h.errorCode = errorCode(inputErr)
		}
		return
	}
	h.status, h.errorCode, h.effective = "idle", "", nil
}

func (m *Manager) SessionScope(ctx context.Context, owner, id string, write bool) (Scope, error) {
	s, err := m.getSession(ctx, owner, id, write)
	if err != nil {
		return Scope{}, err
	}
	s.hub.mu.Lock()
	source := s.hub.source
	s.hub.mu.Unlock()
	if source == nil {
		return Scope{}, problem(503, "CAPTURE_UNAVAILABLE", "采集当前不可用")
	}
	link := source.Link()
	linkCtx, err := m.deps.Source.LinkContext(link)
	if err != nil {
		return Scope{}, err
	}
	lifetime, cancel := context.WithCancel(s.ctx)
	stopLink := context.AfterFunc(linkCtx, cancel)
	stopRequest := context.AfterFunc(ctx, cancel)
	scope := Scope{
		DeviceID: s.hub.deviceID, Link: link, Context: lifetime,
		Close: func() { stopRequest(); stopLink(); cancel() },
	}
	scope.Check = func() error {
		if err := lifetime.Err(); err != nil {
			return err
		}
		if !m.deps.Source.HasLink(link) {
			return problem(503, "DEVICE_DISCONNECTED", "原设备链路已断开")
		}
		return s.check(lifetime, write)
	}
	if err := scope.Check(); err != nil {
		scope.Close()
		return Scope{}, err
	}
	return scope, nil
}

func (m *Manager) record(ctx context.Context, s *session, kind, status, code string) error {
	details, err := json.Marshal(struct {
		Owner string `json:"adminSessionId"`
		Mode  string `json:"mode"`
	}{s.owner, s.mode})
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err = m.deps.DB.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,device_id,kind,status,created_at,finished_at,result_code,safe_details) VALUES(?,'human-session',?,?,?,?,?,?,?,?)`, identity.Secret(), s.id, s.hub.deviceID, kind, status, now, now, code, string(details))
	return err
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "DEADLINE_EXCEEDED"
	}
	return "MEDIA_FAILED"
}
func (m *Manager) notifyDevice(id string) {
	_, err := m.deps.Events.Publish(events.Change{EntityType: "device", EntityID: id, DeviceID: id, TaskID: m.deps.TaskOwner(id)})
	if err != nil && !errors.Is(err, events.ErrClosed) {
		m.deps.Logger.Error("media_notification_failed", "deviceId", id)
	}
}

func (m *Manager) watch() {
	defer m.watcher.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			sessions := make([]*session, 0, len(m.sessions))
			for _, s := range m.sessions {
				sessions = append(sessions, s)
			}
			m.mu.Unlock()
			for _, s := range sessions {
				s.mu.Lock()
				waitingForKey := s.peerReadyClosed && s.awaitingKey && !s.waitingSince.IsZero() && now.Sub(s.waitingSince) > keyFrameTimeout
				connectionExpired := s.peer != nil && !s.peerReadyClosed && !s.peerStarted.IsZero() && now.Sub(s.peerStarted) > peerConnectionTimeout
				s.mu.Unlock()
				if waitingForKey {
					s.beginEnd(problem(503, "KEY_FRAME_TIMEOUT", "设备未能及时提供可解码关键帧"))
					continue
				}
				if connectionExpired {
					s.beginEnd(problem(503, "MEDIA_CONNECTION_TIMEOUT", "WebRTC 媒体连接超时"))
					continue
				}
				if err := m.checkAdmin(m.ctx, s.owner); err != nil {
					s.beginEnd(err)
				}
			}
		}
	}
}
