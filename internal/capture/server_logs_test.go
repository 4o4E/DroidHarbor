package capture

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
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
