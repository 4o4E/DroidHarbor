// Package diagnostics provides bounded runtime logs and payload-free error causes.
package diagnostics

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Open keeps console diagnostics visible while bounding the trial's disk usage.
// Failure to create the file is fatal; later write failures are reported directly
// to stderr rather than disappearing inside slog.Handler's ignored return value.
func Open(path string, level slog.Level) (*slog.Logger, io.Closer, error) {
	var sink io.WriteCloser = nopCloser{os.Stderr}
	var output io.Writer = os.Stderr
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, nil, fmt.Errorf("create log directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, nil, fmt.Errorf("close log preflight: %w", err)
		}
		rotation := &lumberjack.Logger{Filename: path, MaxSize: 20, MaxBackups: 5, Compress: true}
		sink = rotation
		output = tee{file: rotation, console: os.Stderr}
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})), sink, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

type tee struct{ file, console io.Writer }

func (t tee) Write(data []byte) (int, error) {
	n, err := t.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// Do not repeat the original record: it may contain security-sensitive data.
		if _, reportErr := fmt.Fprintf(t.console, "DroidHarbor log_file_write_failed: %v\n", err); reportErr != nil {
			err = errors.Join(err, reportErr)
		}
	}
	_, consoleErr := t.console.Write(data)
	return n, errors.Join(err, consoleErr)
}
