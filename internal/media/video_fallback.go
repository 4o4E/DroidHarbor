package media

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"droidharbor/internal/capture"
	"droidharbor/internal/diagnostics"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Caller holds changeMu. A target is read only for a genuinely new capture;
// joining an existing or retained capture never changes its parameters.
func (h *hub) startSource(ctx context.Context, authorize func() error) error {
	defer func() {
		h.mu.Lock()
		h.starting = false
		h.mu.Unlock()
	}()
	target, deviceRevision, globalRevision, lastGood, err := h.manager.captureConfiguration(ctx, h.deviceID)
	if err != nil {
		h.fail(err)
		return err
	}
	choices := captureChoices(target, globalRevision, lastGood)
	var failure error
	for attempt, parameters := range choices.values[:choices.count] {
		if err := authorize(); err != nil {
			h.fail(err)
			return err
		}
		h.mu.Lock()
		h.requested, h.revision, h.globalRevision, h.targetQuality = parameters, deviceRevision, globalRevision, target.quality()
		h.status, h.errorCode, h.effective, h.startFailure = "starting", "", nil, nil
		h.starting, h.profileRecorded = true, false
		h.sourceReady, h.sourceReadyClosed, h.sourceFailed = make(chan struct{}), false, make(chan struct{})
		if parameters.quality() != target.quality() {
			reason := "目标编码配置启动失败"
			if attempt == 0 {
				reason = "该目标版本已验证兼容档位"
			}
			h.fallback = &VideoFallback{DeviceID: h.deviceID, Revision: globalRevision, Target: target.quality(), Applied: parameters.quality(), Status: "trying", Reason: reason}
		} else {
			h.fallback = nil
		}
		fallback := h.fallback
		h.mu.Unlock()
		if fallback != nil {
			h.manager.deps.Logger.Warn("media_video_fallback_attempt", "deviceId", h.deviceID, "globalRevision", globalRevision, "targetMaxFps", target.MaxFPS, "targetBitRate", target.BitRate, "actualMaxFps", parameters.MaxFPS, "actualBitRate", parameters.BitRate, "reason", fallback.Reason)
		}
		h.manager.notifyDevice(h.deviceID)
		source, startErr := h.manager.deps.Captures.Start(ctx, h.deviceID, parameters.capture(deviceRevision), authorize)
		if startErr == nil {
			h.mu.Lock()
			h.source, h.captureRevision, h.waitingSession = source, deviceRevision, true
			if h.packetizer == nil {
				h.packetizer = rtp.NewPacketizer(1200, 102, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
				h.lastTimestamp = rand.Uint32()
			}
			ready, failed := h.sourceReady, h.sourceFailed
			h.mu.Unlock()
			go h.pump(source)
			startErr = h.activateDisplay(ctx, source, authorize)
			if startErr == nil {
				timer := time.NewTimer(keyFrameTimeout)
				select {
				case <-ready:
				case <-failed:
					h.mu.Lock()
					startErr = h.startFailure
					h.mu.Unlock()
				case <-source.Done():
					startErr = source.Err()
					if startErr == nil {
						startErr = problem(503, "CAPTURE_ENDED", "采集在首帧前结束")
					}
				case <-ctx.Done():
					startErr = ctx.Err()
				case <-timer.C:
					startErr = problem(503, "KEY_FRAME_TIMEOUT", "采集未能及时产生有效关键帧")
				}
				timer.Stop()
			}
		}
		if startErr == nil {
			h.mu.Lock()
			var applied *VideoFallback
			if h.fallback != nil {
				h.fallback.Status = "applied"
				copied := *h.fallback
				applied = &copied
			}
			h.mu.Unlock()
			if applied != nil {
				h.manager.deps.Logger.Warn("media_video_fallback_applied", "deviceId", h.deviceID, "globalRevision", globalRevision, "targetMaxFps", applied.Target.MaxFPS, "targetBitRate", applied.Target.BitRate, "actualMaxFps", applied.Applied.MaxFPS, "actualBitRate", applied.Applied.BitRate, "reason", applied.Reason)
			}
			h.manager.notifyDevice(h.deviceID)
			return nil
		}
		if source != nil {
			startErr = errors.Join(startErr, source.Err())
			if cleanupErr := h.stopStartupSource(source); cleanupErr != nil {
				failure = &Error{Status: 503, Code: "CAPTURE_CLEANUP_FAILED", Message: "失败采集尚未确认回收，已停止回退", Cause: errors.Join(startErr, cleanupErr)}
				break
			}
		}
		if ctx.Err() != nil {
			failure = ctx.Err()
			break
		}
		if !errors.Is(startErr, capture.ErrVideoParametersUnsupported) || attempt+1 == choices.count {
			failure = captureStartFailure(ctx, startErr)
			break
		}
		complete, cleanupErr := h.manager.deps.Captures.Recover(ctx, h.deviceID)
		if cleanupErr != nil || !complete {
			failure = &Error{Status: 503, Code: "CAPTURE_CLEANUP_PENDING", Message: "旧采集未完成回收，已停止回退", Cause: cleanupErr}
			break
		}
		h.manager.deps.Logger.Warn("media_video_parameters_rejected", "deviceId", h.deviceID, "globalRevision", globalRevision, "maxFps", parameters.MaxFPS, "bitRate", parameters.BitRate, "cause", diagnostics.Cause(startErr))
	}
	h.mu.Lock()
	if h.fallback != nil {
		h.fallback.Status = "failed"
		h.fallback.Reason = "兼容参数未能产生有效视频：" + errorCode(failure)
	}
	h.mu.Unlock()
	h.fail(failure)
	return failure
}

func (h *hub) stopStartupSource(source *capture.Stream) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	h.mu.Lock()
	if h.source == source {
		h.source = nil
		h.cleanupSource = source
		h.cleanupPending = true
	}
	h.mu.Unlock()
	err := source.Stop(ctx)
	if err == nil {
		h.mu.Lock()
		if h.cleanupSource == source {
			h.cleanupSource = nil
			h.cleanupPending = false
		}
		h.mu.Unlock()
	}
	return err
}

func (h *hub) captureError(source *capture.Stream, err error) {
	h.mu.Lock()
	if h.source != source {
		h.mu.Unlock()
		return
	}
	if h.starting && !h.sourceReadyClosed {
		if h.startFailure == nil {
			h.startFailure = err
			close(h.sourceFailed)
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.failSource(source, err)
}
