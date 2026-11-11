package media

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"droidharbor/internal/capture"
	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/scrcpy"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Caller holds changeMu; a viewer joining never starts a second encoder.
func (h *hub) ensureSource(ctx context.Context, authorize func() error) error {
	h.mu.Lock()
	if h.cleanupPending {
		retiring := h.cleanupSource
		h.mu.Unlock()
		if retiring == nil {
			return problem(503, "CAPTURE_CLEANUP_PENDING", "旧采集资源尚未确认回收")
		}
		select {
		case <-retiring.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
		complete, err := h.manager.deps.Captures.Recover(ctx, h.deviceID)
		if err != nil || !complete {
			if err == nil {
				err = errors.New("capture resource cleanup is still pending")
			}
			return &Error{Status: 503, Code: "CAPTURE_CLEANUP_PENDING", Message: "旧采集资源尚未确认回收", Cause: err}
		}
		h.mu.Lock()
		if h.cleanupSource == retiring && h.cleanupPending {
			h.cleanupSource, h.cleanupPending = nil, false
			h.status, h.errorCode, h.effective = "idle", "", nil
		}
		h.mu.Unlock()
		return h.ensureSource(ctx, authorize)
	}
	if current := h.source; current != nil {
		failed := h.status == "failed"
		outdated := h.captureRevision != h.revision
		h.mu.Unlock()
		if failed {
			return problem(503, "CAPTURE_FAILED", "已有采集失败，未启动替代采集")
		}
		if outdated {
			return h.restart(ctx, authorize)
		}
		return nil
	}
	h.status, h.errorCode, h.effective = "starting", "", nil
	h.sourceReady, h.sourceReadyClosed = make(chan struct{}), false
	requested, revision := h.requested, h.revision
	h.mu.Unlock()
	h.manager.notifyDevice(h.deviceID)
	source, err := h.manager.deps.Captures.Start(ctx, h.deviceID, requested.capture(revision), authorize)
	if err != nil {
		h.fail(err)
		return &Error{Status: 503, Code: "CAPTURE_START_FAILED", Message: "无法启动设备采集", Cause: err}
	}
	h.mu.Lock()
	h.source = source
	h.captureRevision = revision
	h.waitingSession = true
	if h.packetizer == nil {
		h.packetizer = rtp.NewPacketizer(1200, 102, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
		h.lastTimestamp = rand.Uint32()
	}
	h.mu.Unlock()
	go h.pump(source)
	return nil
}

func (h *hub) restart(ctx context.Context, authorize func() error) error {
	h.inputMu.Lock()
	h.mu.Lock()
	old, width, height := h.source, h.width, h.height
	if h.status == "failed" {
		h.mu.Unlock()
		h.inputMu.Unlock()
		return problem(503, "CAPTURE_FAILED", "已有采集失败，未自动替换采集")
	}
	h.status, h.effective, h.waitingSession = "reconfiguring", nil, true
	if old != nil {
		h.source = nil
		h.cleanupSource = old
		h.cleanupPending = true
	}
	for _, s := range h.members {
		s.invalidateFrame()
	}
	h.mu.Unlock()
	var err error
	if old != nil {
		err = h.inputs.releaseAll(ctx, width, height, old.Send, func() error { return ctx.Err() })
	}
	h.inputMu.Unlock()
	h.manager.notifyDevice(h.deviceID)
	var stopErr error
	if old != nil {
		stopErr = old.Stop(ctx)
	}
	if old != nil {
		h.cancelAllClipboard()
	}
	err = errors.Join(err, stopErr)
	if stopErr != nil {
		h.mu.Lock()
		h.status, h.effective = "failed", nil
		h.mu.Unlock()
	} else if old != nil {
		h.mu.Lock()
		if h.cleanupSource == old {
			h.cleanupSource, h.cleanupPending = nil, false
		}
		h.mu.Unlock()
	}
	if err != nil {
		h.fail(err)
		return &Error{Status: 503, Code: "CAPTURE_CLEANUP_FAILED", Message: "旧采集未能完成回收，未启动替代采集", Cause: err}
	}
	if err := authorize(); err != nil {
		h.fail(err)
		return err
	}
	if err := h.ensureSource(ctx, authorize); err != nil {
		return err
	}
	h.mu.Lock()
	ready, source := h.sourceReady, h.source
	h.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-source.Done():
		err := source.Err()
		if err == nil {
			err = errors.New("capture ended before its first key frame")
		}
		h.fail(err)
		return &Error{Status: 503, Code: "CAPTURE_FAILED", Message: "新画质尚未产生可解码画面", Cause: err}
	case <-ctx.Done():
		h.fail(ctx.Err())
		return ctx.Err()
	}
}

func (h *hub) fail(err error) { h.failSource(nil, err) }

func (h *hub) failSource(source *capture.Stream, err error) {
	h.mu.Lock()
	if source != nil && h.source != source {
		h.mu.Unlock()
		return
	}
	h.status, h.errorCode, h.effective = "failed", errorCode(err), nil
	members := make([]*session, 0, len(h.members))
	for _, s := range h.members {
		members = append(members, s)
	}
	h.mu.Unlock()
	if err != nil {
		h.manager.deps.Logger.Error("media_source_failed", "deviceId", h.deviceID, "code", errorCode(err), "cause", diagnostics.Cause(err))
	}
	h.cancelAllClipboard()
	h.manager.failRetained(h)
	h.manager.notifyDevice(h.deviceID)
	for _, s := range members {
		s.beginEnd(err)
	}
}

func (h *hub) pump(source *capture.Stream) {
	frames, messages := source.Frames(), source.Messages()
	for {
		select {
		case packet, ok := <-frames:
			if !ok {
				h.sourceEnded(source)
				return
			}
			var err error
			if packet.Session {
				err = h.beginEpoch(source, packet)
			} else {
				err = h.publishFrame(source, packet)
			}
			if err != nil {
				h.failSource(source, err)
				return
			}
		case message, ok := <-messages:
			if !ok {
				h.sourceEnded(source)
				return
			}
			h.deviceMessage(source, message)
		case <-source.Done():
			h.sourceEnded(source)
			return
		}
	}
}

func (h *hub) sourceEnded(source *capture.Stream) {
	err := source.Err()
	if err == nil {
		err = problem(503, "CAPTURE_ENDED", "设备采集已结束")
	}
	h.failSource(source, err)
}

func (h *hub) beginEpoch(source *capture.Stream, packet scrcpy.Packet) error {
	if packet.Width == 0 || packet.Height == 0 || packet.Width > math.MaxUint16 || packet.Height > math.MaxUint16 {
		return problem(422, "UNSUPPORTED_VIDEO_DIMENSIONS", "设备画面尺寸超出 SDK 坐标协议范围")
	}
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	h.mu.Lock()
	if h.source != source {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	if err := h.inputs.releaseAll(ctx, uint16(packet.Width), uint16(packet.Height), source.Send, func() error { return ctx.Err() }); err != nil {
		return err
	}
	h.mu.Lock()
	h.width, h.height = uint16(packet.Width), uint16(packet.Height)
	h.epoch++
	epoch := h.epoch
	h.config, h.effective, h.ptsSet, h.waitingSession = nil, nil, false, false
	h.lastKeyFrameAt = time.Time{}
	h.statisticsAt, h.statisticsFrames, h.statisticsBytes = time.Now(), 0, 0
	h.status = "starting"
	for _, s := range h.members {
		s.invalidateFrame()
	}
	h.mu.Unlock()
	if err := devices.RecordVideoGeometry(ctx, h.manager.deps.DB, h.deviceID, int(packet.Width), int(packet.Height)); err != nil {
		return &Error{Status: 503, Code: "STORAGE_UNAVAILABLE", Message: "无法保存设备画面尺寸", Cause: err}
	}
	h.manager.deps.Logger.Info("media_capture_epoch_started", "deviceId", h.deviceID, "captureEpoch", epoch, "width", packet.Width, "height", packet.Height)
	h.manager.notifyDevice(h.deviceID)
	return nil
}

func (h *hub) publishFrame(source *capture.Stream, packet scrcpy.Packet) error {
	h.mu.Lock()
	if h.source != source || h.captureRevision != h.revision {
		h.mu.Unlock()
		return nil
	}
	if h.waitingSession {
		h.mu.Unlock()
		return nil
	}
	if packet.Config {
		if err := validateH264Config(packet.Data); err != nil {
			h.mu.Unlock()
			return err
		}
		h.config = packet.Data
		h.statisticsBytes += int64(len(packet.Data))
		h.mu.Unlock()
		return nil
	}
	if len(h.members) == 0 {
		h.mu.Unlock()
		return nil
	}
	if len(packet.Data) == 0 {
		h.mu.Unlock()
		return problem(503, "INVALID_VIDEO_PACKET", "采集返回空视频包")
	}
	if h.config == nil {
		h.mu.Unlock()
		return problem(503, "MISSING_VIDEO_CONFIG", "采集缺少 H.264 参数集")
	}
	if h.ptsSet && packet.PTS <= h.lastPTS {
		h.mu.Unlock()
		return problem(503, "INVALID_VIDEO_TIMESTAMP", "设备视频时间戳没有递增")
	}
	now := time.Now()
	if !h.ptsSet {
		gap := uint32(1)
		if !h.lastFrameAt.IsZero() {
			elapsed := now.Sub(h.lastFrameAt)
			gap64 := uint64(elapsed/time.Second)*90000 + uint64(elapsed%time.Second)*90000/uint64(time.Second)
			if gap64 == 0 {
				gap64 = 1
			}
			gap = uint32(gap64)
		}
		h.timestampBase, h.firstPTS, h.ptsSet = h.lastTimestamp+gap, packet.PTS, true
	}
	delta := packet.PTS - h.firstPTS
	timestamp := h.timestampBase + uint32((delta/100)*9+(delta%100)*9/100)
	h.lastPTS, h.lastTimestamp, h.lastFrameAt = packet.PTS, timestamp, now
	if packet.KeyFrame {
		h.lastKeyFrameAt = now
	}
	var packets []*rtp.Packet
	if packet.KeyFrame {
		packets = h.packetizer.Packetize(h.config, 0)
	}
	mediaPackets := h.packetizer.Packetize(packet.Data, 0)
	if len(packets) == 0 {
		packets = mediaPackets
	} else {
		packets = append(packets, mediaPackets...)
	}
	if len(packets) == 0 {
		h.mu.Unlock()
		return problem(503, "INVALID_H264_PAYLOAD", "设备视频包没有可发送的 H.264 数据")
	}
	size := 0
	for i, p := range packets {
		p.Timestamp = timestamp
		p.Marker = i == len(packets)-1
		size += 12 + len(p.Payload)
	}
	becameEffective := h.effective == nil && packet.KeyFrame
	if becameEffective {
		h.effective = &EffectiveVideo{Width: h.width, Height: h.height, Revision: h.captureRevision}
		h.status, h.errorCode = "running", ""
		if !h.sourceReadyClosed {
			close(h.sourceReady)
			h.sourceReadyClosed = true
		}
	}
	h.statisticsFrames++
	h.statisticsBytes += int64(len(packet.Data))
	if elapsed := now.Sub(h.statisticsAt); elapsed >= time.Second {
		if h.effective != nil {
			h.effective.FramesPerSecond = float64(h.statisticsFrames) / elapsed.Seconds()
			h.effective.EncodedBitsPerSecond = int64(float64(h.statisticsBytes*8) / elapsed.Seconds())
		}
		h.statisticsAt, h.statisticsFrames, h.statisticsBytes = now, 0, 0
	}
	frame := &videoFrame{packets: packets, size: size, source: source, epoch: h.epoch, width: h.width, height: h.height, timestamp: timestamp, key: packet.KeyFrame, observedAt: now}
	for _, s := range h.members {
		if err := s.offerFrame(frame); err != nil {
			s.beginEnd(err)
		}
	}
	h.mu.Unlock()
	if becameEffective {
		h.manager.notifyDevice(h.deviceID)
	}
	return nil
}

// Parameter sets are retained once per capture epoch, then fed before each IDR.
// A new viewer waits for a fresh key frame instead of replaying an old timeline.
func validateH264Config(data []byte) error {
	if len(data) > 64<<10 {
		return problem(503, "INVALID_VIDEO_CONFIG", "H.264 参数集超出长度限制")
	}
	sps, pps := false, false
	for offset := 0; offset < len(data); {
		start, prefix := annexBStart(data, offset)
		if start < 0 {
			break
		}
		next, _ := annexBStart(data, start+prefix)
		end := next
		if end < 0 {
			end = len(data)
		}
		nal := data[start+prefix : end]
		if len(nal) != 0 {
			switch nal[0] & 31 {
			case 7:
				if len(nal) < 4 {
					return problem(503, "INVALID_VIDEO_CONFIG", "H.264 SPS 不完整")
				}
				// Android encoders commonly emit Main/High even when a baseline
				// profile is requested; browsers decode all three.
				switch nal[1] {
				case 66, 77, 100:
				default:
					return problem(422, "UNSUPPORTED_H264_PROFILE", "设备输出的 H.264 profile 不受支持")
				}
				sps = true
			case 8:
				pps = true
			}
		}
		offset = end
	}
	if !sps || !pps {
		return problem(503, "INVALID_VIDEO_CONFIG", "H.264 参数集缺少 SPS 或 PPS")
	}
	return nil
}

func annexBStart(data []byte, offset int) (int, int) {
	for i := offset; i+2 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		if data[i+2] == 1 {
			return i, 3
		}
		if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			return i, 4
		}
	}
	return -1, 0
}
