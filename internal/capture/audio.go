package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"droidharbor/internal/diagnostics"
)

func (m *Manager) NewAudioManager() *Manager {
	ctx, cancel := context.WithCancel(m.ctx)
	return &Manager{db: m.db, adb: m.adb, source: m.source, server: m.server, logger: m.logger, ctx: ctx, cancel: cancel, audioOnly: true, streams: make(map[string]*Stream), recovering: make(map[string]*recoveryFlight)}
}

func startupAudioCommand(jar, nonce string, scid int32) (string, error) {
	if scid <= 0 {
		return "", diagnostics.New("invalid audio capture scid")
	}
	return gatedStartupCommand(jar, nonce, []string{
		"scid=" + fmt.Sprintf("%08x", uint32(scid)),
		"video=false", "audio=true", "audio_source=output", "audio_codec=opus", "audio_bit_rate=128000", "control=false",
		"cleanup=false", "power_on=false", "stay_awake=false", "keep_active=false", "power_off_on_close=false",
		"send_dummy_byte=true", "send_device_meta=false", "send_stream_meta=true", "send_frame_meta=true", "tunnel_forward=true",
	}), nil
}

func readAudioPreamble(r io.Reader) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return fmt.Errorf("audio codec metadata: %w", err)
	}
	switch binary.BigEndian.Uint32(header[:]) {
	case 0x6f707573:
		return nil
	case 0:
		return diagnostics.New("设备无法采集媒体声音；Android 11 需要解锁后启动，Android 10 及更早版本不支持")
	case 1:
		return diagnostics.New("设备音频编码初始化失败，请检查 Opus 编码器及原生日志")
	default:
		return diagnostics.New("设备未协商 Opus 音频")
	}
}
