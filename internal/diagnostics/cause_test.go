package diagnostics

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCausePreservesSystemDiagnosisWithoutPayloads(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, sqlErr := db.Exec("SELECT sensitive_sql_identifier_marker")
	if sqlErr == nil {
		t.Fatal("expected SQLite to reject the unknown column")
	}
	failure := errors.Join(
		fmt.Errorf("secret-command-marker: %w", sqlErr),
		&fs.PathError{Op: "open", Path: "secret-path-marker", Err: syscall.EACCES},
		errors.New("secret-clipboard-marker"),
	)
	var output bytes.Buffer
	slog.New(slog.NewJSONHandler(&output, nil)).Error("operation_failed", "cause", Cause(failure))
	var entry struct {
		Cause map[string]json.RawMessage `json:"cause"`
	}
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	var database struct {
		Cause struct {
			Code int `json:"code"`
		} `json:"cause"`
	}
	if err := json.Unmarshal(entry.Cause["cause0"], &database); err != nil {
		t.Fatal(err)
	}
	if database.Cause.Code != 1 {
		t.Fatalf("lost SQLite diagnosis: %s", output.String())
	}
	var file struct {
		Operation string `json:"operation"`
		Cause     struct {
			Errno   int    `json:"errno"`
			Message string `json:"message"`
		} `json:"cause"`
	}
	if err := json.Unmarshal(entry.Cause["cause1"], &file); err != nil {
		t.Fatal(err)
	}
	if file.Operation != "open" || file.Cause.Errno != int(syscall.EACCES) || file.Cause.Message != syscall.EACCES.Error() {
		t.Fatalf("lost OS diagnosis: %s", output.String())
	}
	for _, secret := range []string{"sensitive_sql_identifier_marker", "secret-command-marker", "secret-path-marker", "secret-clipboard-marker"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("diagnostic leaked %q", secret)
		}
	}
}
