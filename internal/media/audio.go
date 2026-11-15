package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"droidharbor/internal/capture"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/rtc"
	"github.com/pion/rtp"
)

type AudioSnapshot struct {
	Route          string `json:"route"`
	Target         string `json:"target"`
	Status         string `json:"status"`
	OwnerSessionID string `json:"ownerSessionId,omitempty"`
	Error          string `json:"error,omitempty"`
	Revision       uint64 `json:"revision"`
}

type deviceAudio struct {
	source                                *capture.Stream
	initialized                           bool
	route, target, status, owner, failure string
	revision                              uint64
}

func (h *hub) audioSnapshot() AudioSnapshot {
	return AudioSnapshot{Route: h.audio.route, Target: h.audio.target, Status: h.audio.status, OwnerSessionID: h.audio.owner, Error: h.audio.failure, Revision: h.audio.revision}
}

func (m *Manager) Audio(ctx context.Context, owner, id string) (AudioSnapshot, error) {
	s, err := m.getSession(ctx, owner, id, false)
	if err != nil {
		return AudioSnapshot{}, err
	}
	h := s.hub
	h.audioMu.Lock()
	defer h.audioMu.Unlock()
	return h.audioSnapshot(), nil
}

func (m *Manager) PatchAudio(ctx context.Context, owner, id, route string, revision uint64) (AudioSnapshot, error) {
	s, err := m.getSession(ctx, owner, id, true)
	if err != nil {
		return AudioSnapshot{}, err
	}
	if route != "muted" && route != "device" && route != "browser" {
		return AudioSnapshot{}, problem(422, "INVALID_AUDIO_ROUTE", "请选择媒体声音的播放位置")
	}
	h := s.hub
	h.audioMu.Lock()
	defer h.audioMu.Unlock()
	if h.audio.revision != revision {
		return h.audioSnapshot(), problem(409, "AUDIO_CONFLICT", "播放位置已被另一个窗口修改，请读取最新状态")
	}
	if err = s.check(ctx, true); err != nil {
		return AudioSnapshot{}, err
	}
	err = h.setAudio(ctx, route, s.id, func() error { return s.check(ctx, true) })
	snapshot := h.audioSnapshot()
	if err != nil {
		return snapshot, &Error{Status: 503, Code: "AUDIO_ROUTE_FAILED", Message: "媒体音频切换失败，视频保持；" + err.Error(), Cause: err}
	}
	return snapshot, nil
}

func (h *hub) initializeAudio(s *session) {
	h.audioMu.Lock()
	defer h.audioMu.Unlock()
	if h.audio.initialized {
		return
	}
	h.mu.Lock()
	alive := h.members[s.id] == s && s.ctx.Err() == nil
	h.mu.Unlock()
	if !alive {
		return
	}
	var route string
	if err := h.manager.deps.DB.QueryRowContext(s.ctx, `SELECT audio_default FROM system_preferences WHERE id=1`).Scan(&route); err != nil {
		h.audioFailure("muted", err)
		return
	}
	ctx, cancel := context.WithTimeout(h.manager.ctx, 30*time.Second)
	defer cancel()
	// Failure is retained in the shared audio snapshot and logged by setAudio.
	_ = h.setAudio(ctx, route, s.id, func() error {
		h.mu.Lock()
		var active *session
		for _, member := range h.members {
			if member.ctx.Err() == nil {
				active = member
				break
			}
		}
		h.mu.Unlock()
		if active == nil {
			return context.Canceled
		}
		return active.check(ctx, false)
	})
}

// audioMu serializes routing only. Video epochs and peer connections are not
// changed by this method. Failure never silently switches to another route.
func (h *hub) setAudio(ctx context.Context, route, owner string, check func() error) error {
	ctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.audioCancel = cancel
	h.mu.Unlock()
	defer func() { cancel(); h.mu.Lock(); h.audioCancel = nil; h.mu.Unlock() }()
	h.audio.initialized = true
	h.audio.target = route
	h.audio.revision++
	if route == "device" {
		if h.audio.source != nil {
			source := h.audio.source
			h.audio.source = nil
			if err := source.Stop(ctx); err != nil {
				h.audioFailure(route, err)
				return err
			}
		}
		h.audio.route, h.audio.status, h.audio.owner, h.audio.failure = "device", "ready", "", ""
		h.manager.deps.Logger.Info("media_audio_route_changed", "deviceId", h.deviceID, "route", route)
		return nil
	}
	if h.audio.source == nil {
		if h.manager.deps.AudioCaptures == nil {
			err := errors.New("音频采集服务未初始化")
			h.audioFailure(route, err)
			return err
		}
		complete, err := h.manager.deps.AudioCaptures.Recover(ctx, h.deviceID)
		if err == nil && !complete {
			err = errors.New("原音频进程尚未确认回收")
		}
		if err != nil {
			h.audioFailure(route, err)
			return err
		}
		h.audio.status = "starting"
		source, err := h.manager.deps.AudioCaptures.Start(ctx, h.deviceID, capture.Settings{}, check)
		if err != nil {
			h.audioFailure(route, err)
			return err
		}
		h.audio.source = source
		go h.runAudio(source)
	}
	h.audio.route, h.audio.owner, h.audio.failure = route, "", ""
	if route == "browser" {
		h.audio.owner = owner
	}
	if h.audio.status != "starting" {
		h.audio.status = "ready"
	}
	h.manager.deps.Logger.Info("media_audio_route_changed", "deviceId", h.deviceID, "route", route, "sessionId", h.audio.owner)
	return nil
}

func (h *hub) audioFailure(target string, err error) {
	h.audio.initialized = true
	h.audio.target = target
	h.audio.status = "failed"
	h.audio.failure = err.Error()
	h.audio.revision++
	if h.audio.source == nil {
		h.audio.route = "unknown"
		h.audio.owner = ""
	}
	h.manager.deps.Logger.Error("media_audio_failed", "deviceId", h.deviceID, "route", target, "cause", diagnostics.Cause(err))
}

func (h *hub) stopAudio(ctx context.Context) error {
	h.mu.Lock()
	cancel := h.audioCancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	h.audioMu.Lock()
	defer h.audioMu.Unlock()
	source := h.audio.source
	h.audio = deviceAudio{}
	if source != nil {
		if err := source.Stop(ctx); err != nil {
			return err
		}
	}
	if h.manager.deps.AudioCaptures != nil {
		return h.manager.deps.AudioCaptures.StopDevice(ctx, h.deviceID)
	}
	return nil
}

func (h *hub) runAudio(source *capture.Stream) {
	var seed [6]byte
	if _, err := rand.Read(seed[:]); err != nil {
		h.failAudioSource(source, err)
		return
	}
	sequence := binary.BigEndian.Uint16(seed[:2])
	ssrc := binary.BigEndian.Uint32(seed[2:])
	configured := false
	var timestamp uint32
	clockStarted := false
	for packet := range source.Frames() {
		if packet.Config {
			if len(packet.Data) < 19 || !bytes.Equal(packet.Data[:8], []byte("OpusHead")) || packet.Data[9] != 2 || packet.Data[18] != 0 {
				h.failAudioSource(source, errors.New("设备 Opus 配置不是支持的双声道映射"))
				return
			}
			configured = true
			h.audioMu.Lock()
			if h.audio.source == source {
				h.audio.status = "ready"
				h.audio.failure = ""
			}
			h.audioMu.Unlock()
			continue
		}
		if !configured || packet.Session || len(packet.Data) == 0 {
			h.failAudioSource(source, errors.New("设备发送了无效的 Opus 音频包"))
			return
		}
		samples, err := opusPacketSamples(packet.Data)
		if err != nil {
			h.failAudioSource(source, fmt.Errorf("设备 Opus 包时长无效: %w", err))
			return
		}
		if !clockStarted {
			timestamp = uint32(packet.PTS/1000*48 + (packet.PTS%1000)*48/1000)
			clockStarted = true
			h.manager.deps.Logger.Info("media_audio_clock_started", "deviceId", h.deviceID, "sampleRate", 48000, "frameSamples", samples)
		}
		packetTimestamp := timestamp
		timestamp += samples
		h.audioMu.Lock()
		current := h.audio.source == source
		route, owner := h.audio.route, h.audio.owner
		h.audioMu.Unlock()
		if !current {
			return
		}
		if route != "browser" || owner == "" {
			continue
		}
		h.mu.Lock()
		s := h.members[owner]
		h.mu.Unlock()
		if s == nil {
			continue
		}
		s.mu.Lock()
		peer, active := s.peer, !s.suspended && s.ctx.Err() == nil
		s.mu.Unlock()
		if !active || peer == nil {
			continue
		}
		sequence++
		if err := peer.WriteAudioRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: sequence, Timestamp: packetTimestamp, SSRC: ssrc}, Payload: packet.Data}); err != nil {
			if s.ctx.Err() == nil && !errors.Is(err, rtc.ErrPeerNotReady) {
				h.failAudioSource(source, fmt.Errorf("网页音频传输失败: %w", err))
				return
			}
		}
	}
	h.audioMu.Lock()
	if h.audio.source == source {
		h.audio.source = nil
		err := source.Err()
		if err == nil {
			err = errors.New("音频采集已中断")
		}
		h.audioFailure(h.audio.target, err)
	}
	h.audioMu.Unlock()
}

// A retiring or non-playing viewer cannot invalidate the active audio owner.
func (h *hub) audioPeerFailed(s *session, err error) {
	h.audioMu.Lock()
	if s.ctx.Err() != nil {
		h.audioMu.Unlock()
		return
	}
	h.manager.deps.Logger.Warn("media_audio_viewer_failed", "deviceId", h.deviceID, "sessionId", s.id, "cause", diagnostics.Cause(err))
	if h.audio.owner != s.id || h.audio.route != "browser" || h.audio.source == nil {
		h.audioMu.Unlock()
		return
	}
	source := h.audio.source
	h.audio.source = nil
	h.audioFailure(h.audio.target, err)
	h.audioMu.Unlock()
	h.cleanupFailedAudio(source)
}

func (h *hub) failAudioSource(source *capture.Stream, err error) {
	h.audioMu.Lock()
	if h.audio.source != source {
		h.audioMu.Unlock()
		return
	}
	h.audio.source = nil
	h.audioFailure(h.audio.target, err)
	h.audioMu.Unlock()
	h.cleanupFailedAudio(source)
}

func (h *hub) cleanupFailedAudio(source *capture.Stream) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if cleanupErr := source.Stop(ctx); cleanupErr != nil {
		h.manager.deps.Logger.Error("media_audio_cleanup_failed", "deviceId", h.deviceID, "cause", diagnostics.Cause(cleanupErr))
	}
}
