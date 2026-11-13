package media

import (
	"time"

	"droidharbor/internal/diagnostics"
)

// Caller holds s.mu. Drop the entire GOP and invalidate in-flight packets; a
// transient sender stall must not tear down the negotiated peer. One encoder
// reset is outstanding until a complete keyframe is sent or the deadline ends.
func (s *session) recoverBackpressureLocked(frame *videoFrame) {
	first := s.backpressureSince.IsZero()
	if first {
		s.backpressureSince = time.Now()
		s.logBackpressureLocked(frame, "queue_full")
	}
	s.discardQueuedFramesLocked()
	s.awaitingKey = true
	s.ready, s.readyEpoch, s.boundaryTimestamp = false, 0, 0
	s.frameTimestampCount, s.frameTimestampNext = 0, 0
	s.keyNoticeSent = false
	if s.waitingSince.IsZero() {
		s.waitingSince = s.backpressureSince
	}
	if first && !frame.key {
		go s.resetDeviceVideo()
	}
}

func (s *session) logBackpressureLocked(frame *videoFrame, reason string) {
	inFlight := time.Duration(0)
	if !s.sendingFrameAt.IsZero() {
		inFlight = time.Since(s.sendingFrameAt)
	}
	s.manager.deps.Logger.Warn("media_viewer_backpressure", "sessionId", s.id, "deviceId", s.hub.deviceID,
		"reason", reason, "captureEpoch", frame.epoch, "queuedFrames", len(s.frames),
		"queuedBytes", s.queuedFrameBytes, "frameBytes", frame.size, "framePackets", len(frame.packets),
		"lastFrameSendMs", s.lastFrameSendDuration.Milliseconds(), "inFlightFrameMs", inFlight.Milliseconds())
}

func (s *session) frameWaitError(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.backpressureSince.IsZero() && now.Sub(s.backpressureSince) >= keyFrameTimeout {
		return &Error{Status: 503, Code: "SLOW_VIEWER", Message: "观看者持续积压，未能及时恢复视频发送", Cause: diagnostics.New("viewer queue recovery timed out before a complete keyframe was sent")}
	}
	if s.peerReadyClosed && !s.waitingSince.IsZero() && now.Sub(s.waitingSince) > keyFrameTimeout {
		return problem(503, "KEY_FRAME_TIMEOUT", "设备未能及时提供可解码关键帧")
	}
	return nil
}
