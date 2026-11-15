// Package diagnostics provides bounded runtime logs and payload-free error causes.
package diagnostics

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Open keeps console diagnostics visible while bounding the trial's disk usage.
// Failure to create the file is fatal; later write failures are reported directly
// to stderr rather than disappearing inside slog.Handler's ignored return value.
func Open(path string, level slog.Level) (*slog.Logger, io.Closer, error) {
	return open(path, level, time.Now)
}

func open(path string, level slog.Level, now func() time.Time) (*slog.Logger, io.Closer, error) {
	var sink io.WriteCloser = nopCloser{os.Stderr}
	var output io.Writer = os.Stderr
	if path != "" {
		// A configuration names the log's base path. Text output always uses
		// .log, keeping existing JSON histories intact on upgrade.
		path = strings.TrimSuffix(path, filepath.Ext(path)) + ".log"
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, nil, fmt.Errorf("create log directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file: %w", err)
		}
		info, statErr := file.Stat()
		if err := errors.Join(statErr, file.Close()); err != nil {
			return nil, nil, fmt.Errorf("inspect log file: %w", err)
		}
		day := now().In(logZone).Format(time.DateOnly)
		if info.Size() > 0 {
			day = info.ModTime().In(logZone).Format(time.DateOnly)
		}
		rotation := &dailyWriter{
			Logger: &lumberjack.Logger{Filename: path, MaxSize: 20, MaxBackups: 5, Compress: true, LocalTime: true},
			day:    day, now: now,
		}
		sink = rotation
		output = tee{file: rotation, console: os.Stderr}
	}
	return slog.New(newPatternHandler(output, level)), sink, nil
}

// Rotate before the first record of a new local calendar day, including after
// downtime. The same lumberjack instance bounds both daily and size archives.
type dailyWriter struct {
	*lumberjack.Logger
	mu  sync.Mutex
	day string
	now func() time.Time
}

func (w *dailyWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	day := w.now().In(logZone).Format(time.DateOnly)
	if day != w.day {
		if err := w.Rotate(); err != nil {
			return 0, fmt.Errorf("rotate daily log: %w", err)
		}
		w.day = day
	}
	return w.Logger.Write(data)
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
		if _, reportErr := fmt.Fprintf(t.console, "%s ERROR [--------] log_file_write_failed | %v\n", time.Now().In(logZone).Format("2006-01-02 15:04:05.000"), err); reportErr != nil {
			err = errors.Join(err, reportErr)
		}
	}
	_, consoleErr := t.console.Write(data)
	return n, errors.Join(err, consoleErr)
}
