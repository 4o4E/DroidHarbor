package media

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"droidharbor/internal/diagnostics"
	"droidharbor/internal/scrcpy"
)

type DeviceVolume struct {
	Current int `json:"current"`
	Min     int `json:"min"`
	Max     int `json:"max"`
}

var mediaVolumePattern = regexp.MustCompile(`(?m)^\[V\] volume is ([0-9]+) in range \[([0-9]+)\.\.([0-9]+)\]\r?$`)
var normalAudioModePattern = regexp.MustCompile(`(?m)^[ \t]*- Actual mode = MODE_NORMAL[ \t]*\r?$`)

func parseDeviceVolume(output []byte) (DeviceVolume, error) {
	parts := mediaVolumePattern.FindSubmatch(output)
	if len(parts) != 4 {
		return DeviceVolume{}, diagnostics.New("media volume response is missing")
	}
	var values [3]int
	for i := range values {
		value, err := strconv.Atoi(string(parts[i+1]))
		if err != nil {
			return DeviceVolume{}, diagnostics.New("media volume response has invalid integer")
		}
		values[i] = value
	}
	volume := DeviceVolume{Current: values[0], Min: values[1], Max: values[2]}
	if volume.Max <= volume.Min || volume.Current < volume.Min || volume.Current > volume.Max {
		return DeviceVolume{}, diagnostics.New("media volume response has invalid range")
	}
	return volume, nil
}

// DeviceMediaVolume targets STREAM_MUSIC through the device's volume-key policy,
// confirming real values. The original interaction/link lifetime cancels on leave.
func (m *Manager) DeviceMediaVolume(ctx context.Context, owner, id string, level *int) (DeviceVolume, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s, err := m.getSession(ctx, owner, id, level != nil)
	if err != nil {
		return DeviceVolume{}, err
	}
	h := s.hub
	h.audioMu.Lock()
	defer h.audioMu.Unlock()
	if h.audio.route != "device" || h.audio.status != "ready" {
		return DeviceVolume{}, problem(409, "VOLUME_ROUTE_CHANGED", "设备音量仅在设备播放模式可用，请读取最新播放位置")
	}
	scope, err := m.SessionScope(ctx, owner, id, level != nil)
	if err != nil {
		return DeviceVolume{}, err
	}
	defer scope.Close()
	run := func(command string) (DeviceVolume, error) {
		result, err := m.deps.ADB.RunShell(scope.Context, scope.Link.TransportID, command, 16<<10, scope.Check)
		if err == nil && result.ExitCode != 0 {
			err = diagnostics.New(fmt.Sprintf("media volume command exited with status %d", result.ExitCode))
		}
		var volume DeviceVolume
		if err == nil {
			volume, err = parseDeviceVolume(result.Stdout)
		}
		if err != nil {
			return DeviceVolume{}, &Error{Status: 503, Code: "DEVICE_VOLUME_FAILED", Message: "无法读取或设置手机媒体音量", Cause: err}
		}
		return volume, nil
	}
	volume, err := run("cmd media_session volume --stream 3 --get")
	if err != nil || level == nil {
		return volume, err
	}
	if *level < volume.Min || *level > volume.Max {
		return DeviceVolume{}, problem(422, "INVALID_VOLUME", "媒体音量超出设备范围")
	}
	if volume.Current == *level {
		return volume, nil
	}
	// Refuse non-normal modes; the device still owns normal-mode key routing.
	audioMode, err := m.deps.ADB.RunShell(scope.Context, scope.Link.TransportID, "dumpsys audio", 256<<10, scope.Check)
	if err != nil {
		return DeviceVolume{}, &Error{Status: 503, Code: "DEVICE_VOLUME_FAILED", Message: "无法确认设备音频模式，音量未调整", Cause: err}
	}
	if audioMode.ExitCode != 0 || !normalAudioModePattern.Match(audioMode.Stdout) {
		return DeviceVolume{}, problem(409, "VOLUME_MODE_UNAVAILABLE", "设备未处于可确认的普通媒体模式，音量未调整")
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	unchanged := 0
	for steps := 0; volume.Current != *level; steps++ {
		if steps >= volume.Max-volume.Min+2 {
			m.deps.Logger.Warn("device_media_volume_incomplete", "deviceId", h.deviceID, "sessionId", id, "target", *level, "actual", volume.Current)
			return DeviceVolume{}, problem(409, "VOLUME_NOT_APPLIED", fmt.Sprintf("手机未达到目标音量，实际为 %d/%d", volume.Current, volume.Max))
		}
		if err := scope.Check(); err != nil {
			return DeviceVolume{}, err
		}
		key := int32(25)
		if volume.Current < *level {
			key = 24
		}
		previous := volume.Current
		if err := s.tapKey(scope.Context, s.currentEpoch(), scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: key}); err != nil {
			return DeviceVolume{}, &Error{Status: 503, Code: "DEVICE_VOLUME_FAILED", Message: "设备音量按键未完成", Cause: err}
		}
		// Confirm this key before dispatching the next. A first key may only
		// activate the device's volume controller; never assume it changed a step.
		deadline := time.Now().Add(500 * time.Millisecond)
		for {
			volume, err = run("cmd media_session volume --stream 3 --get")
			if err != nil {
				return DeviceVolume{}, err
			}
			if volume.Current != previous || !time.Now().Before(deadline) {
				break
			}
			select {
			case <-scope.Context.Done():
				return DeviceVolume{}, scope.Context.Err()
			case <-tick.C:
			}
		}
		if volume.Current == previous {
			unchanged++
		} else {
			unchanged = 0
		}
		if unchanged == 2 {
			m.deps.Logger.Warn("device_media_volume_incomplete", "deviceId", h.deviceID, "sessionId", id, "target", *level, "actual", volume.Current)
			return DeviceVolume{}, problem(409, "VOLUME_NOT_APPLIED", fmt.Sprintf("音量键未改变媒体音量，实际为 %d/%d；请检查手机音量键策略", volume.Current, volume.Max))
		}
	}
	m.deps.Logger.Info("device_media_volume_changed", "deviceId", h.deviceID, "sessionId", id, "volume", volume.Current, "min", volume.Min, "max", volume.Max)
	return volume, nil
}
