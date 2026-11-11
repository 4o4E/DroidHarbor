package capture

import (
	"bytes"
	"log/slog"
)

const serverDiagnosticLineLimit = 4096

type serverDiagnostics struct {
	logger              *slog.Logger
	deviceID, captureID string
	pending             [2][]byte
	dropping            [2]bool
}

// Shell output can contain device/user text. Record severity and known exception
// types, never the raw message; preserve headers split across shell-v2 packets.
func (s *serverDiagnostics) write(stream byte, data []byte) {
	index := int(stream - 1)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		complete := end >= 0
		if !complete {
			end = len(data)
		}
		part := data[:end]
		if !s.dropping[index] {
			pending := s.pending[index]
			if len(pending) == 0 && complete && len(part) <= serverDiagnosticLineLimit {
				s.observe(part)
			} else if len(pending)+len(part) <= serverDiagnosticLineLimit {
				pending = append(pending, part...)
				if complete {
					s.observe(pending)
					pending = pending[:0]
				}
				s.pending[index] = pending
			} else {
				pending = append(pending, part[:serverDiagnosticLineLimit-len(pending)]...)
				s.observe(pending)
				s.logger.Warn("scrcpy_server_diagnostic_truncated", "deviceId", s.deviceID, "captureId", s.captureID)
				s.pending[index] = pending[:0]
				s.dropping[index] = !complete
			}
		} else if complete {
			s.dropping[index] = false
		}
		if !complete {
			return
		}
		data = data[end+1:]
	}
}
func (s *serverDiagnostics) flush() {
	for _, pending := range s.pending {
		if len(pending) > 0 {
			s.observe(pending)
		}
	}
}
func (s *serverDiagnostics) observe(line []byte) {
	line = bytes.TrimSpace(line)
	if bytes.Equal(line, []byte("[server] INFO: Device display turned off")) || bytes.Equal(line, []byte("[server] INFO: Device display turned on")) {
		s.logger.Info("scrcpy_display_power_changed", "deviceId", s.deviceID, "captureId", s.captureID, "physicalDisplayOn", bytes.HasSuffix(line, []byte(" on")))
		return
	}
	if bytes.Equal(line, []byte("[server] INFO: Device rotation requested: portrait")) || bytes.Equal(line, []byte("[server] INFO: Device rotation requested: landscape")) {
		s.logger.Info("scrcpy_device_rotation_requested", "deviceId", s.deviceID, "captureId", s.captureID, "landscape", bytes.HasSuffix(line, []byte("landscape")))
		return
	}
	if bytes.HasPrefix(line, []byte("[server] ERROR:")) {
		s.logger.Error("scrcpy_server_error", "deviceId", s.deviceID, "captureId", s.captureID)
		return
	}
	if bytes.HasPrefix(line, []byte("[server] WARN:")) {
		s.logger.Warn("scrcpy_server_warning", "deviceId", s.deviceID, "captureId", s.captureID)
		return
	}
	line = bytes.TrimPrefix(line, []byte("Caused by: "))
	for _, kind := range serverExceptionTypes {
		if len(line) >= len(kind) && bytes.Equal(line[:len(kind)], []byte(kind)) && (len(line) == len(kind) || line[len(kind)] == ':') {
			s.logger.Error("scrcpy_server_exception", "deviceId", s.deviceID, "captureId", s.captureID, "exceptionType", kind)
			return
		}
	}
}

var serverExceptionTypes = [...]string{
	"java.lang.SecurityException", "java.lang.IllegalArgumentException", "java.lang.IllegalStateException",
	"java.lang.NullPointerException", "java.lang.UnsupportedOperationException", "java.lang.RuntimeException",
	"java.lang.AssertionError", "java.io.IOException", "android.os.RemoteException", "android.media.MediaCodec$CodecException",
}
