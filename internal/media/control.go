package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"time"

	"droidharbor/internal/capture"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/rtc"
	"droidharbor/internal/scrcpy"
)

type controlEnvelope struct {
	SessionID    string          `json:"sessionId"`
	CaptureEpoch uint64          `json:"captureEpoch"`
	Sequence     uint64          `json:"sequence"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload"`
}

type controlReply struct {
	SessionID    string `json:"sessionId"`
	CaptureEpoch uint64 `json:"captureEpoch"`
	Sequence     uint64 `json:"sequence"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	ErrorCode    string `json:"errorCode,omitempty"`
	Message      string `json:"message,omitempty"`
	Payload      any    `json:"payload,omitempty"`
}

type keyPayload struct {
	KeyCode   int32 `json:"keyCode"`
	Action    uint8 `json:"action"`
	Repeat    int32 `json:"repeat,omitempty"`
	MetaState int32 `json:"metaState,omitempty"`
}

type touchPayload struct {
	PointerID int64   `json:"pointerId"`
	Action    uint8   `json:"action"`
	X         int     `json:"x"`
	Y         int     `json:"y"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Pressure  float32 `json:"pressure"`
}

type scrollPayload struct {
	X       int     `json:"x"`
	Y       int     `json:"y"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	HScroll float32 `json:"hScroll"`
	VScroll float32 `json:"vScroll"`
	Buttons int32   `json:"buttons,omitempty"`
}

type mousePayload struct {
	Button string `json:"button"`
	Action uint8  `json:"action"`
}

type textPayload struct {
	Text string `json:"text"`
}
type displayPowerPayload struct {
	On bool `json:"on"`
}
type frameReadyPayload struct {
	Timestamp uint32 `json:"timestamp"`
}
type frameBoundaryPayload struct {
	Timestamp uint32 `json:"timestamp"`
	Width     uint16 `json:"width"`
	Height    uint16 `json:"height"`
	KeyFrame  bool   `json:"keyFrame"`
}

func rtcCallbacks(s *session) rtc.Callbacks {
	return rtc.Callbacks{
		Control:           s.enqueueControl,
		Ready:             s.onPeerReady,
		Closed:            func(err error) { s.beginEnd(err) },
		KeyFrameRequested: s.requestKeyFrame,
	}
}

func (s *session) onPeerReady() {
	requested := false
	s.mu.Lock()
	if !s.peerReadyClosed && s.ctx.Err() == nil {
		s.peerReadyClosed = true
		s.awaitingKey = true
		s.waitingSince = time.Now()
		close(s.peerReady)
		requested = true
	}
	s.mu.Unlock()
	if requested {
		go s.resetDeviceVideo()
	}
}

func (s *session) enqueueControl(data []byte) {
	if len(data) == 0 || len(data) > maxControlQueueBytes {
		s.beginEnd(problem(503, "CONTROL_BACKPRESSURE", "控制消息队列已达到上限"))
		return
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	if s.queuedControlBytes+len(data) > maxControlQueueBytes {
		s.mu.Unlock()
		s.beginEnd(problem(503, "CONTROL_BACKPRESSURE", "控制消息队列已达到上限"))
		return
	}
	select {
	case s.commands <- data:
		s.queuedControlBytes += len(data)
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		s.beginEnd(problem(503, "CONTROL_BACKPRESSURE", "控制消息队列已达到上限"))
	}
}

func (s *session) enqueueOutput(data []byte) bool {
	if len(data) == 0 || len(data) > maxControlQueueBytes {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.queuedOutputBytes+len(data) > maxControlQueueBytes {
		return false
	}
	select {
	case s.output <- data:
		s.queuedOutputBytes += len(data)
		return true
	default:
		return false
	}
}

func (s *session) reply(sequence, epoch uint64, kind, status string, err error, payload any) {
	reply := controlReply{SessionID: s.id, CaptureEpoch: epoch, Sequence: sequence, Type: kind, Status: status, Payload: payload}
	if err != nil {
		if errors.Is(err, capture.ErrResultUnknown) {
			reply.ErrorCode, reply.Message, reply.Status = "RESULT_UNKNOWN", "设备操作结果未知", "unknown"
		} else {
			var mediaErr *Error
			if errors.As(err, &mediaErr) {
				reply.ErrorCode, reply.Message = mediaErr.Code, mediaErr.Message
			} else if errors.Is(err, context.DeadlineExceeded) {
				reply.ErrorCode, reply.Message = "DEADLINE_EXCEEDED", "设备操作超时，结果可能未知"
			} else if errors.Is(err, context.Canceled) {
				reply.ErrorCode, reply.Message = "CANCELLED", "设备操作已取消"
			} else {
				reply.ErrorCode, reply.Message = "MEDIA_FAILED", "设备操作失败"
			}
			reply.Status = "failed"
		}
	}
	data, marshalErr := json.Marshal(reply)
	if marshalErr != nil || !s.enqueueOutput(data) {
		s.beginEnd(problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限"))
	}
}

func (s *session) runCommands() {
	defer s.workers.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case data := <-s.commands:
			s.mu.Lock()
			s.queuedControlBytes -= len(data)
			s.mu.Unlock()
			s.handleControl(data)
		}
	}
}

func (s *session) runOutput() {
	defer s.workers.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case data := <-s.output:
			s.mu.Lock()
			s.queuedOutputBytes -= len(data)
			peer := s.peer
			s.mu.Unlock()
			if err := s.check(s.ctx, false); err != nil {
				s.beginEnd(err)
				return
			}
			if peer == nil {
				s.beginEnd(problem(503, "MEDIA_NOT_READY", "WebRTC 会话尚未就绪"))
				return
			}
			if err := peer.SendControl(data); err != nil {
				s.beginEnd(err)
				return
			}
		}
	}
}

func (s *session) handleControl(data []byte) {
	var envelope controlEnvelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		s.reply(0, 0, "result", "failed", problem(422, "INVALID_CONTROL", "控制信封无效"), nil)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(422, "INVALID_CONTROL", "控制信封无效"), nil)
		return
	}
	if envelope.SessionID != s.id || envelope.Sequence == 0 || envelope.Type == "" {
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(422, "INVALID_CONTROL", "控制信封不属于当前会话"), nil)
		return
	}
	s.mu.Lock()
	if envelope.Sequence <= s.sequence {
		s.mu.Unlock()
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(409, "CONTROL_SEQUENCE_INVALID", "控制序号必须严格递增"), nil)
		return
	}
	s.sequence = envelope.Sequence
	s.mu.Unlock()

	if err := s.check(s.ctx, false); err != nil {
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", err, nil)
		return
	}
	if envelope.Type == "ping" {
		h := s.hub
		h.mu.Lock()
		epoch := h.epoch
		h.mu.Unlock()
		s.reply(envelope.Sequence, epoch, "pong", "succeeded", nil, nil)
		return
	}
	if envelope.CaptureEpoch != s.currentEpoch() {
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(409, "STALE_CAPTURE_EPOCH", "控制事件属于旧画面代次"), nil)
		return
	}
	if envelope.Type == "frame-ready" {
		var payload frameReadyPayload
		if err := decodeControlPayload(envelope.Payload, &payload, "timestamp"); err != nil {
			s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(422, "INVALID_CONTROL", "关键帧确认无效"), nil)
			return
		}
		if !s.acknowledgeFrame(envelope.CaptureEpoch, payload.Timestamp) {
			s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(409, "FRAME_NOT_SENT", "确认的关键帧未发送或已失效"), nil)
			return
		}
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "frame-ready", "succeeded", nil, nil)
		return
	}
	if envelope.Type == "request-frame" {
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(422, "INVALID_CONTROL", "关键帧请求无效"), nil)
			return
		}
		s.requestKeyFrame()
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "request-frame", "waiting-key-frame", nil, nil)
		return
	}
	if !s.frameReady(envelope.CaptureEpoch) {
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", problem(409, "FRAME_NOT_READY", "必须先确认当前画面关键帧"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, controlTimeout)
	defer cancel()
	if err := s.applyControl(ctx, envelope); err != nil {
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "failed", err, nil)
		return
	}
	asyncClipboard := envelope.Type == "clipboard.get" || envelope.Type == "clipboard.set" || envelope.Type == "clipboard.paste"
	if envelope.Type == "text" {
		var p textPayload
		if decodeControlPayload(envelope.Payload, &p) == nil && (!asciiText(p.Text) || len(p.Text) > 300) {
			asyncClipboard = true
		}
	}
	if !asyncClipboard {
		s.reply(envelope.Sequence, envelope.CaptureEpoch, "result", "dispatched", nil, nil)
	}
}

func decodeControlPayload(data json.RawMessage, target any, required ...string) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("missing control payload")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("multiple control payload values")
	}
	if len(required) != 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for _, name := range required {
			value, ok := fields[name]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return errors.New("missing required control field")
			}
		}
	}
	return nil
}

func (s *session) currentEpoch() uint64 {
	s.hub.mu.Lock()
	epoch := s.hub.epoch
	s.hub.mu.Unlock()
	return epoch
}

func (s *session) frameReady(epoch uint64) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.mu.Lock()
	ready := s.ready && s.readyEpoch == epoch && epoch == s.hub.epoch
	s.mu.Unlock()
	return ready
}

func (s *session) acknowledgeFrame(epoch uint64, timestamp uint32) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.hub.epoch {
		return false
	}
	found := false
	for _, boundary := range s.frameTimestamps[:s.frameTimestampCount] {
		if boundary == timestamp {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	s.ready, s.readyEpoch, s.boundaryTimestamp = true, epoch, timestamp
	return true

}
func (s *session) offerFrame(frame *videoFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || !s.peerReadyClosed {
		return nil
	}
	if s.awaitingKey && !frame.key {
		return nil
	}
	if len(s.frames) == cap(s.frames) || s.queuedFrameBytes+frame.size > maxFrameQueueBytes {
		return problem(503, "SLOW_VIEWER", "观看者无法及时接收视频帧")
	}
	select {
	case s.frames <- frame:
		s.queuedFrameBytes += frame.size
		if s.frameGenerations == nil {
			s.frameGenerations = make(map[*videoFrame]uint64)
		}
		s.frameGenerations[frame] = s.frameGeneration
		if frame.key {
			s.awaitingKey = false
			s.waitingSince = time.Time{}
		}
		return nil
	default:
		return problem(503, "SLOW_VIEWER", "观看者无法及时接收视频帧")
	}
}

func (s *session) invalidateFrame() {
	s.mu.Lock()
	s.awaitingKey = true
	s.ready, s.readyEpoch, s.boundaryTimestamp = false, 0, 0
	s.frameTimestampCount, s.frameTimestampNext = 0, 0
	s.keyNoticeSent = false
	s.frameGeneration++
	for {
		select {
		case frame := <-s.frames:
			s.queuedFrameBytes -= frame.size
			delete(s.frameGenerations, frame)
		default:
			s.mu.Unlock()
			return
		}
	}
}

func (s *session) requestKeyFrame() {
	s.mu.Lock()
	if s.ctx.Err() == nil {
		s.frameGeneration++
		s.ready, s.readyEpoch, s.boundaryTimestamp = false, 0, 0
		s.awaitingKey = true
		s.frameTimestampCount, s.frameTimestampNext = 0, 0
		s.keyNoticeSent = false
		for {
			select {
			case frame := <-s.frames:
				s.queuedFrameBytes -= frame.size
				delete(s.frameGenerations, frame)
			default:
				s.mu.Unlock()
				go s.resetDeviceVideo()
				return
			}
		}
	}
	s.mu.Unlock()
}

// resetDeviceVideo forces the scrcpy encoder to emit a fresh key frame. The
// server's default I-frame interval is ten seconds, longer than the viewer
// key-frame timeout, so a new or recovering viewer must request one.
func (s *session) resetDeviceVideo() {
	h := s.hub
	h.mu.Lock()
	source := h.source
	h.mu.Unlock()
	if source == nil || s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, controlTimeout)
	defer cancel()
	// RESET_VIDEO restarts capture for all viewers; it is not a device write,
	// so read-only viewers must be able to request a fresh key frame too.
	if err := source.Send(ctx, scrcpy.ControlMessage{Type: scrcpy.ControlResetVideo}, func() error { return s.check(ctx, false) }); err != nil && s.ctx.Err() == nil {
		s.manager.deps.Logger.Warn("media_keyframe_request_failed", "sessionId", s.id, "deviceId", h.deviceID, "cause", diagnostics.Cause(err))
	}
}

func (s *session) frameCurrent(frame *videoFrame, generation uint64) bool {
	h := s.hub
	h.mu.Lock()
	s.mu.Lock()
	current := h.source == frame.source && h.epoch == frame.epoch && s.frameGeneration == generation && s.ctx.Err() == nil && !s.awaitingKey
	s.mu.Unlock()
	h.mu.Unlock()
	return current
}

func (s *session) markFrameSent(frame *videoFrame, generation uint64) (bool, bool) {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.source != frame.source || h.epoch != frame.epoch || s.frameGeneration != generation || s.ctx.Err() != nil || s.awaitingKey {
		return false, false
	}
	notify := false
	if frame.key && !s.keyNoticeSent {
		s.frameTimestampCount, s.frameTimestampNext = 0, 0
		s.keyNoticeSent = true
		notify = true
	}
	if s.frameTimestampCount == 0 && !frame.key {
		return false, false
	}
	s.frameTimestamps[s.frameTimestampNext] = frame.timestamp
	s.frameTimestampNext = (s.frameTimestampNext + 1) % maxFrameBoundaries
	if s.frameTimestampCount < maxFrameBoundaries {
		s.frameTimestampCount++
	}
	return true, notify
}

func (s *session) runVideo() {
	defer s.workers.Done()
	select {
	case <-s.peerReady:
	case <-s.ctx.Done():
		return
	}
	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()
	if peer == nil {
		return
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame := <-s.frames:
			if frame == nil {
				continue
			}
			s.mu.Lock()
			s.queuedFrameBytes -= frame.size
			generation, queued := s.frameGenerations[frame]
			delete(s.frameGenerations, frame)
			s.mu.Unlock()
			if !queued {
				continue
			}
			complete := true
			for _, packet := range frame.packets {
				if !s.frameCurrent(frame, generation) {
					complete = false
					break
				}
				if err := peer.WriteRTP(packet); err != nil {
					s.beginEnd(err)
					return
				}
			}
			sent, notify := false, false
			if complete {
				sent, notify = s.markFrameSent(frame, generation)
			}
			if sent && notify {
				notice := controlReply{
					SessionID: s.id, CaptureEpoch: frame.epoch, Type: "frame-boundary", Status: "sent",
					Payload: frameBoundaryPayload{Timestamp: frame.timestamp, Width: frame.width, Height: frame.height, KeyFrame: frame.key},
				}
				if !s.enqueueOutput(mustJSON(notice)) {
					s.beginEnd(problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限"))
					return
				}
			}
		}
	}
}

func (s *session) applyControl(ctx context.Context, envelope controlEnvelope) error {
	switch envelope.Type {
	case "key":
		var p keyPayload
		if err := decodeControlPayload(envelope.Payload, &p, "keyCode", "action"); err != nil || p.Action > 1 || p.KeyCode < 0 || p.Repeat < 0 || p.MetaState < 0 {
			return problem(422, "INVALID_INPUT", "按键参数无效")
		}
		return s.withInput(ctx, envelope.CaptureEpoch, func(source *capture.Stream, width, height uint16) error {
			send := captureSender(source)
			return s.hub.inputs.key(ctx, s.id, scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Action: p.Action, Keycode: p.KeyCode, Repeat: p.Repeat, MetaState: p.MetaState}, send, func() error { return s.check(ctx, true) })
		})
	case "touch":
		var p touchPayload
		if err := decodeControlPayload(envelope.Payload, &p, "pointerId", "action", "x", "y", "width", "height", "pressure"); err != nil || p.PointerID < 0 || p.Width <= 0 || p.Height <= 0 || p.Width > math.MaxUint16 || p.Height > math.MaxUint16 {
			return problem(422, "INVALID_INPUT", "触控参数无效")
		}
		return s.withInput(ctx, envelope.CaptureEpoch, func(source *capture.Stream, width, height uint16) error {
			if p.Width != int(width) || p.Height != int(height) || p.X < 0 || p.Y < 0 || p.X >= int(width) || p.Y >= int(height) {
				return problem(409, "STALE_FRAME_COORDINATES", "触控坐标不匹配当前画面")
			}
			message := scrcpy.ControlMessage{Type: scrcpy.ControlInjectTouchEvent, Action: p.Action, Position: scrcpy.Position{X: int32(p.X), Y: int32(p.Y), Width: width, Height: height}, Pressure: p.Pressure}
			return s.hub.inputs.touch(ctx, s.id, p.PointerID, message, captureSender(source), func() error { return s.check(ctx, true) })
		})
	case "scroll":
		var p scrollPayload
		if err := decodeControlPayload(envelope.Payload, &p, "x", "y", "width", "height", "hScroll", "vScroll"); err != nil || p.Width <= 0 || p.Height <= 0 || p.Width > math.MaxUint16 || p.Height > math.MaxUint16 {
			return problem(422, "INVALID_INPUT", "滚动参数无效")
		}
		return s.withInput(ctx, envelope.CaptureEpoch, func(source *capture.Stream, width, height uint16) error {
			if p.Width != int(width) || p.Height != int(height) || p.X < 0 || p.Y < 0 || p.X >= int(width) || p.Y >= int(height) {
				return problem(409, "STALE_FRAME_COORDINATES", "滚动坐标不匹配当前画面")
			}
			return source.Send(ctx, scrcpy.ControlMessage{Type: scrcpy.ControlInjectScrollEvent, Position: scrcpy.Position{X: int32(p.X), Y: int32(p.Y), Width: width, Height: height}, HScroll: p.HScroll, VScroll: p.VScroll, Buttons: p.Buttons}, func() error { return s.check(ctx, true) })
		})
	case "mouseButton":
		var p mousePayload
		if err := decodeControlPayload(envelope.Payload, &p, "button", "action"); err != nil || p.Action > 1 {
			return problem(422, "INVALID_INPUT", "鼠标按键参数无效")
		}
		return s.mouseButton(ctx, envelope.CaptureEpoch, p)
	case "text":
		var p textPayload
		if err := decodeControlPayload(envelope.Payload, &p, "text"); err != nil || p.Text == "" {
			return problem(422, "INVALID_INPUT", "文本不能为空")
		}
		if asciiText(p.Text) && len(p.Text) <= 300 {
			return s.sendDevice(ctx, envelope.CaptureEpoch, scrcpy.ControlMessage{Type: scrcpy.ControlInjectText, Text: p.Text})
		}
		return s.queueClipboardWrite(ctx, envelope.Sequence, envelope.CaptureEpoch, p.Text, true)
	case "clipboard.get":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "剪贴板读取参数无效")
		}
		return s.queueClipboardRead(ctx, envelope.Sequence, envelope.CaptureEpoch)
	case "clipboard.set", "clipboard.paste":
		var p textPayload
		if err := decodeControlPayload(envelope.Payload, &p, "text"); err != nil {
			return problem(422, "INVALID_INPUT", "剪贴板参数无效")
		}
		return s.queueClipboardWrite(ctx, envelope.Sequence, envelope.CaptureEpoch, p.Text, envelope.Type == "clipboard.paste")
	case "displayPower":
		var p displayPowerPayload
		if err := decodeControlPayload(envelope.Payload, &p, "on"); err != nil {
			return problem(422, "INVALID_INPUT", "显示电源参数无效")
		}
		return s.sendDevice(ctx, envelope.CaptureEpoch, scrcpy.ControlMessage{Type: scrcpy.ControlSetDisplayPower, On: p.On})
	case "back":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "返回操作参数无效")
		}
		return s.tapKey(ctx, envelope.CaptureEpoch, scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 4})
	case "home":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "Home 操作参数无效")
		}
		return s.tapKey(ctx, envelope.CaptureEpoch, scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 3})
	case "recent":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "最近任务操作参数无效")
		}
		return s.tapKey(ctx, envelope.CaptureEpoch, scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 187})
	case "notification":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "通知栏操作参数无效")
		}
		return s.sendButtonless(ctx, envelope.CaptureEpoch, scrcpy.ControlExpandNotificationPanel)
	case "settings":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "设置面板操作参数无效")
		}
		return s.sendButtonless(ctx, envelope.CaptureEpoch, scrcpy.ControlExpandSettingsPanel)
	case "collapse":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "面板收起参数无效")
		}
		return s.sendButtonless(ctx, envelope.CaptureEpoch, scrcpy.ControlCollapsePanels)
	case "rotate":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "旋转操作参数无效")
		}
		return s.sendButtonless(ctx, envelope.CaptureEpoch, scrcpy.ControlRotateDevice)
	case "resetVideo":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "视频重置参数无效")
		}
		return s.sendButtonless(ctx, envelope.CaptureEpoch, scrcpy.ControlResetVideo)
	case "release":
		if err := decodeControlPayload(envelope.Payload, &struct{}{}); err != nil {
			return problem(422, "INVALID_INPUT", "输入释放参数无效")
		}
		return s.withInput(ctx, envelope.CaptureEpoch, func(source *capture.Stream, width, height uint16) error {
			return s.hub.inputs.release(ctx, s.id, width, height, captureSender(source), func() error { return s.check(ctx, true) })
		})
	default:
		return problem(422, "UNSUPPORTED_CONTROL", "不支持该控制操作")
	}
}

func (s *session) mouseButton(ctx context.Context, epoch uint64, p mousePayload) error {
	var message scrcpy.ControlMessage
	switch p.Button {
	case "right":
		// Keycode 仅用于共享 Back 输入的所有权；线上保留 scrcpy 的返回/唤醒语义。
		message = scrcpy.ControlMessage{Type: scrcpy.ControlBackOrScreenOn, Keycode: 4}
	case "middle":
		message = scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 3}
	case "back":
		message = scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 187}
	case "forward":
		if p.Action == 1 {
			return s.check(ctx, true)
		}
		return s.sendButtonless(ctx, epoch, scrcpy.ControlExpandNotificationPanel)
	case "recent":
		message = scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 187}
	case "notification":
		if p.Action == 1 {
			return s.check(ctx, true)
		}
		return s.sendButtonless(ctx, epoch, scrcpy.ControlExpandNotificationPanel)
	default:
		return problem(422, "INVALID_INPUT", "鼠标按键不受支持")
	}
	message.Action = p.Action
	return s.withInput(ctx, epoch, func(source *capture.Stream, width, height uint16) error {
		return s.hub.inputs.key(ctx, s.id, message, captureSender(source), func() error { return s.check(ctx, true) })
	})
}

func (s *session) tapKey(ctx context.Context, epoch uint64, message scrcpy.ControlMessage) error {
	return s.withInput(ctx, epoch, func(source *capture.Stream, width, height uint16) error {
		send := captureSender(source)
		authorize := func() error { return s.check(ctx, true) }
		message.Action = 0
		if err := s.hub.inputs.key(ctx, s.id, message, send, authorize); err != nil {
			return err
		}
		message.Action = 1
		return s.hub.inputs.key(ctx, s.id, message, send, authorize)
	})
}

func (s *session) sendButtonless(ctx context.Context, epoch uint64, kind scrcpy.ControlType) error {
	return s.sendDevice(ctx, epoch, scrcpy.ControlMessage{Type: kind})
}

func (s *session) withInput(ctx context.Context, epoch uint64, action func(*capture.Stream, uint16, uint16) error) error {
	h := s.hub
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	h.mu.Lock()
	if h.epoch != epoch || h.source == nil {
		h.mu.Unlock()
		return problem(409, "STALE_CAPTURE_EPOCH", "控制事件属于旧画面代次")
	}
	source, width, height := h.source, h.width, h.height
	h.mu.Unlock()
	return action(source, width, height)
}

func captureSender(source *capture.Stream) controlSender {
	return func(ctx context.Context, message scrcpy.ControlMessage, authorize func() error) error {
		return source.Send(ctx, message, authorize)
	}
}

func asciiText(text string) bool {
	for _, value := range []byte(text) {
		if value >= 0x80 {
			return false
		}
	}
	return true
}

func (s *session) sendDevice(ctx context.Context, epoch uint64, message scrcpy.ControlMessage) error {
	h := s.hub
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	h.mu.Lock()
	if h.epoch != epoch || h.source == nil {
		h.mu.Unlock()
		return problem(409, "STALE_CAPTURE_EPOCH", "控制事件属于旧画面代次")
	}
	source := h.source
	h.mu.Unlock()
	return source.Send(ctx, message, func() error { return s.check(ctx, true) })
}

func (s *session) queueClipboardRead(ctx context.Context, sequence, epoch uint64) error {
	h := s.hub
	h.mu.Lock()
	if h.epoch != epoch || h.source == nil {
		h.mu.Unlock()
		return problem(409, "STALE_CAPTURE_EPOCH", "控制事件属于旧画面代次")
	}
	if h.clipboardRead != nil {
		h.mu.Unlock()
		return problem(409, "CLIPBOARD_READ_PENDING", "剪贴板读取仍在等待设备响应")
	}
	request := &clipboardRequest{session: s, controlSequence: sequence, dispatching: true}
	h.clipboardRead = request
	request.timer = time.AfterFunc(clipboardTimeout, func() { h.timeoutClipboardRead(request) })
	h.mu.Unlock()
	err := s.sendDevice(ctx, epoch, scrcpy.ControlMessage{Type: scrcpy.ControlGetClipboard})
	h.mu.Lock()
	deliver := request.acked && !request.tombstone && request.session.ctx.Err() == nil
	text := request.text
	acked := request.acked
	if h.clipboardRead == request {
		request.dispatching = false
		if request.acked {
			h.clipboardRead = nil
			if request.timer != nil {
				request.timer.Stop()
				request.timer = nil
			}
		} else if err != nil && !errors.Is(err, capture.ErrResultUnknown) {
			h.clipboardRead = nil
			if request.timer != nil {
				request.timer.Stop()
				request.timer = nil
			}
		}
	}
	h.mu.Unlock()
	if err != nil && !acked {
		if errors.Is(err, capture.ErrResultUnknown) {
			h.mu.Lock()
			if h.clipboardRead == request {
				request.tombstone = true
				if request.timer != nil {
					request.timer.Stop()
					request.timer = nil
				}
			}
			h.mu.Unlock()
			s.reply(sequence, epoch, "clipboard.get", "unknown", capture.ErrResultUnknown, nil)
			return nil
		}
		return err
	}
	if !s.enqueueOutput(mustJSON(controlReply{SessionID: s.id, CaptureEpoch: epoch, Sequence: sequence, Type: "clipboard.get", Status: "dispatched"})) {
		return problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限")
	}
	if deliver {
		s.reply(sequence, epoch, "clipboard.result", "received", nil, map[string]any{"text": text})
	}
	return nil
}

func (s *session) queueClipboardWrite(ctx context.Context, sequence, epoch uint64, text string, paste bool) error {
	h := s.hub
	h.mu.Lock()
	if h.epoch != epoch || h.source == nil {
		h.mu.Unlock()
		return problem(409, "STALE_CAPTURE_EPOCH", "控制事件属于旧画面代次")
	}
	if len(h.clipboardWrites) >= 64 {
		h.mu.Unlock()
		return problem(503, "CLIPBOARD_BACKPRESSURE", "剪贴板写入队列已达到上限")
	}
	if h.nextClipboardSequence == math.MaxUint64 {
		h.mu.Unlock()
		return problem(503, "CLIPBOARD_SEQUENCE_EXHAUSTED", "剪贴板操作序号已耗尽")
	}
	h.nextClipboardSequence++
	nativeSequence := h.nextClipboardSequence
	request := &clipboardRequest{session: s, controlSequence: sequence, nativeSequence: nativeSequence, dispatching: true}
	h.clipboardWrites[nativeSequence] = request
	request.timer = time.AfterFunc(clipboardTimeout, func() { h.timeoutClipboardWrite(request) })
	h.mu.Unlock()
	message := scrcpy.ControlMessage{Type: scrcpy.ControlSetClipboard, Sequence: nativeSequence, Text: text, Paste: paste}
	err := s.sendDevice(ctx, epoch, message)
	h.mu.Lock()
	if h.clipboardWrites[nativeSequence] == request {
		request.dispatching = false
		if request.acked {
			delete(h.clipboardWrites, nativeSequence)
			if request.timer != nil {
				request.timer.Stop()
				request.timer = nil
			}
		} else if err != nil && !errors.Is(err, capture.ErrResultUnknown) {
			delete(h.clipboardWrites, nativeSequence)
			if request.timer != nil {
				request.timer.Stop()
				request.timer = nil
			}
		}
	}
	acked := request.acked
	h.mu.Unlock()
	if err != nil && !acked {
		if errors.Is(err, capture.ErrResultUnknown) {
			if !s.enqueueOutput(mustJSON(controlReply{SessionID: s.id, CaptureEpoch: epoch, Sequence: sequence, Type: "clipboard.set", Status: "unknown", ErrorCode: "RESULT_UNKNOWN", Message: "设备操作结果未知"})) {
				return problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限")
			}
			return nil
		}
		return err
	}
	if !s.enqueueOutput(mustJSON(controlReply{SessionID: s.id, CaptureEpoch: epoch, Sequence: sequence, Type: "clipboard.set", Status: "dispatched"})) {
		return problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限")
	}
	if acked {
		if !s.enqueueOutput(mustJSON(controlReply{SessionID: s.id, CaptureEpoch: epoch, Sequence: sequence, Type: "clipboard.result", Status: "acknowledged", Payload: map[string]bool{"applied": false}})) {
			return problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限")
		}
	}
	return nil
}

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }

func (h *hub) timeoutClipboardRead(request *clipboardRequest) {
	h.mu.Lock()
	if h.clipboardRead != request || request.tombstone {
		h.mu.Unlock()
		return
	}
	request.tombstone = true
	request.timer = nil
	h.mu.Unlock()
	request.session.reply(request.controlSequence, request.session.currentEpoch(), "clipboard.result", "timed-out", problem(504, "CLIPBOARD_TIMEOUT", "设备剪贴板响应超时"), nil)
}

func (h *hub) timeoutClipboardWrite(request *clipboardRequest) {
	h.mu.Lock()
	if h.clipboardWrites[request.nativeSequence] != request {
		h.mu.Unlock()
		return
	}
	delete(h.clipboardWrites, request.nativeSequence)
	request.timer = nil
	session, sequence := request.session, request.controlSequence
	h.mu.Unlock()
	if session.ctx.Err() == nil {
		session.reply(sequence, session.currentEpoch(), "clipboard.result", "unknown", errors.Join(capture.ErrResultUnknown, errors.New("clipboard acknowledgement timeout")), map[string]bool{"applied": false})
	}
}

func (h *hub) deviceMessage(source *capture.Stream, message scrcpy.DeviceMessage) {
	h.mu.Lock()
	if h.source != source {
		h.mu.Unlock()
		return
	}
	var deliver *clipboardRequest
	switch message.Type {
	case scrcpy.DeviceClipboard:
		request := h.clipboardRead
		if request != nil {
			if request.dispatching {
				request.acked, request.text = true, message.Text
			} else {
				h.clipboardRead = nil
				if request.timer != nil {
					request.timer.Stop()
					request.timer = nil
				}
				if !request.tombstone && request.session.ctx.Err() == nil {
					deliver = request
					request.text = message.Text
				}
			}
		}
	case scrcpy.DeviceClipboardAck:
		request := h.clipboardWrites[message.Sequence]
		if request != nil {
			request.acked = true
			if !request.dispatching {
				delete(h.clipboardWrites, message.Sequence)
				if request.timer != nil {
					request.timer.Stop()
					request.timer = nil
				}
				if request.session.ctx.Err() == nil {
					deliver = request
				}
			}
		}
	default:
		h.mu.Unlock()
		h.failSource(source, problem(503, "INVALID_DEVICE_MESSAGE", "设备返回不支持的媒体消息"))
		return
	}
	epoch := h.epoch
	var listeners []*session
	if message.Type == scrcpy.DeviceClipboard {
		for _, member := range h.members {
			if member.mode == "control" && member.ctx.Err() == nil {
				listeners = append(listeners, member)
			}
		}
	}
	h.mu.Unlock()
	if deliver != nil {
		session := deliver.session
		var payload any
		status := "acknowledged"
		if message.Type == scrcpy.DeviceClipboard {
			status, payload = "received", map[string]string{"text": deliver.text}
		} else {
			payload = map[string]bool{"applied": false}
		}
		if !session.enqueueOutput(mustJSON(controlReply{SessionID: session.id, CaptureEpoch: epoch, Sequence: deliver.controlSequence, Type: "clipboard.result", Status: status, Payload: payload})) {
			session.beginEnd(problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限"))
		}
	}
	// Clipboard text stays on authorized control channels; readonly viewers,
	// logs and the desktop system clipboard never receive these events.
	for _, member := range listeners {
		ctx, cancel := context.WithTimeout(member.ctx, controlTimeout)
		err := member.check(ctx, true)
		cancel()
		if err != nil {
			member.beginEnd(err)
			continue
		}
		if !member.enqueueOutput(mustJSON(controlReply{SessionID: member.id, CaptureEpoch: epoch, Type: "clipboard.changed", Status: "received", Payload: map[string]string{"text": message.Text}})) {
			member.beginEnd(problem(503, "CONTROL_BACKPRESSURE", "控制响应队列已达到上限"))
		}
	}
}

func (h *hub) cancelClipboard(s *session) {
	h.mu.Lock()
	if request := h.clipboardRead; request != nil && request.session == s {
		request.tombstone = true
		if request.timer != nil {
			request.timer.Stop()
			request.timer = nil
		}
	}
	for sequence, request := range h.clipboardWrites {
		if request.session == s {
			delete(h.clipboardWrites, sequence)
			if request.timer != nil {
				request.timer.Stop()
				request.timer = nil
			}
		}
	}
	h.mu.Unlock()
}

func (h *hub) cancelAllClipboard() {
	h.mu.Lock()
	read := h.clipboardRead
	if read != nil && read.timer != nil {
		read.timer.Stop()
		read.timer = nil
	}
	h.clipboardRead = nil
	writes := make([]*clipboardRequest, 0, len(h.clipboardWrites))
	for sequence, request := range h.clipboardWrites {
		if request.timer != nil {
			request.timer.Stop()
			request.timer = nil
		}
		writes = append(writes, request)
		delete(h.clipboardWrites, sequence)
	}
	epoch := h.epoch
	h.mu.Unlock()
	if read != nil && !read.tombstone && read.session.ctx.Err() == nil {
		if read.acked {
			read.session.reply(read.controlSequence, epoch, "clipboard.result", "received", nil, map[string]string{"text": read.text})
		} else {
			read.session.reply(read.controlSequence, epoch, "clipboard.result", "unknown", capture.ErrResultUnknown, nil)
		}
	}
	for _, request := range writes {
		if request.session.ctx.Err() != nil {
			continue
		}
		if request.acked {
			request.session.reply(request.controlSequence, epoch, "clipboard.result", "acknowledged", nil, map[string]bool{"applied": false})
		} else {
			request.session.reply(request.controlSequence, epoch, "clipboard.result", "unknown", capture.ErrResultUnknown, map[string]bool{"applied": false})
		}
	}
}
