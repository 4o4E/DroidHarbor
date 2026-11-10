package diagnostics

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWriteFailureRemainsVisibleOnConsole(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "closed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	writer := tee{file: file, console: &console}
	record := []byte("{\"msg\":\"device_cleanup_failed\"}\n")
	if _, err := writer.Write(record); err == nil {
		t.Fatal("file write failure was hidden")
	}
	if !strings.Contains(console.String(), "log_file_write_failed") {
		t.Fatalf("console lost the file failure: %s", console.String())
	}
	if bytes.Count(console.Bytes(), record) != 1 {
		t.Fatalf("console lost or duplicated the runtime event: %s", console.String())
	}
}
