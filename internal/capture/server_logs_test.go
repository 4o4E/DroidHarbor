package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"droidharbor/internal/leases"
)

func TestServerDiagnosticsPreserveErrorsAcrossPacketsWithoutRawContent(t *testing.T) {
	var output bytes.Buffer
	observer := serverDiagnostics{logger: slog.New(slog.NewJSONHandler(&output, nil)), deviceID: "device", captureID: "capture"}
	const privateText = "PRIVATE-CLIPBOARD-CONTENT"
	observer.write(1, []byte("[ser"))
	observer.write(2, []byte("[server] WARN: "+privateText+"\n"))
	observer.write(1, []byte("ver] ERROR: "+privateText+strings.Repeat("x", serverDiagnosticLineLimit)+"\n"))
	observer.write(1, []byte("java.lang.IllegalStateException: "+privateText))
	observer.flush()
	if strings.Contains(output.String(), privateText) {
		t.Fatal("raw server content leaked into platform logs")
	}
	decoder := json.NewDecoder(&output)
	observed := make(map[string]string)
	for {
		var event struct {
			Message string `json:"msg"`
			Level   string `json:"level"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		observed[event.Message] = event.Level
	}
	if observed["scrcpy_server_error"] != "ERROR" || observed["scrcpy_server_exception"] != "ERROR" || observed["scrcpy_server_warning"] != "WARN" || observed["scrcpy_server_diagnostic_truncated"] != "WARN" {
		t.Fatal("fragmented, interleaved, or truncated diagnostics lost their severity")
	}
}

func TestOnlyExplicitEncoderConfigurationRejectionAllowsFallback(t *testing.T) {
	for _, test := range []struct {
		line     string
		rejected bool
	}{
		{"[server] ERROR: Capture/encoding error: java.lang.IllegalArgumentException: unsupported bitrate", true},
		{"[server] ERROR: Capture/encoding error: android.media.MediaCodec$CodecException: unsupported", true},
		{"[server] ERROR: Capture/encoding error: java.io.IOException: socket closed", false},
		{"[server] ERROR: Capture/encoding error: java.lang.SecurityException: permission denied", false},
		{"[server] ERROR: Capture/encoding error: java.lang.IllegalStateException: stopped", false},
		{"[server] ERROR: java.lang.IllegalArgumentException: unrelated", false},
	} {
		t.Run(test.line, func(t *testing.T) {
			rejected := false
			observer := serverDiagnostics{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), encodingFailed: func() { rejected = true }}
			observer.write(1, []byte(test.line[:12]))
			observer.write(1, []byte(test.line[12:]+"\n"))
			if rejected != test.rejected {
				t.Fatalf("fallback classification=%v; want %v", rejected, test.rejected)
			}
		})
	}
}

func TestInputDenialIsSpecificToLiveCaptureAndRecentObservation(t *testing.T) {
	link := leases.Link{DeviceID: "phone", LinkID: "usb", Generation: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Stream{link: link, ctx: ctx, live: true}
	m := &Manager{streams: map[string]*Stream{"phone": s}}
	log := serverDiagnostics{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), inputDenied: func() { s.inputDeniedAt.Store(time.Now().UnixMilli()) }}
	log.write(1, []byte("[server] ERROR: java.lang.SecurityException: unrelated\n"))
	if m.RecentInputDenial(link) {
		t.Fatal("unrelated security failure became input evidence")
	}
	log.write(1, []byte(`[server] ERROR: Make sure you have enabled "USB debugging (Security Settings)" and then rebooted your device.`+"\n"))
	if !m.RecentInputDenial(link) {
		t.Fatal("explicit INJECT_EVENTS rejection missing")
	}
	next := link
	next.Generation++
	if m.RecentInputDenial(next) {
		t.Fatal("input denial crossed reconnect")
	}
	s.inputDeniedAt.Store(time.Now().Add(-time.Minute).UnixMilli())
	if m.RecentInputDenial(link) {
		t.Fatal("expired denial reported as current")
	}
	s.inputDeniedAt.Store(time.Now().UnixMilli())
	cancel()
	if m.RecentInputDenial(link) {
		t.Fatal("ended capture supplied input evidence")
	}
}
