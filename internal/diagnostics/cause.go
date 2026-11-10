package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"syscall"
)

type fixedError struct{ message string }

func (e *fixedError) Error() string { return e.message }

// New marks fixed, code-defined diagnostic text as safe for runtime logs.
// Device output, SQL values and credentials remain ordinary wrapped causes.
func New(message string) error { return &fixedError{message: message} }

type stageError struct {
	stage string
	cause error
}

func (e *stageError) Error() string { return e.stage + ": " + e.cause.Error() }
func (e *stageError) Unwrap() error { return e.cause }

// WrapStage adds a code-defined operation stage without trusting cause text.
func WrapStage(stage string, cause error) error { return &stageError{stage: stage, cause: cause} }

// Cause preserves the error chain, SQLite result code and OS/network operation.
// Arbitrary error text may contain SQL values, shell commands or device payloads;
// only known payload-free system errors are rendered verbatim.
func Cause(err error) slog.Value {
	if err == nil {
		return slog.AnyValue(nil)
	}
	attrs := []slog.Attr{slog.String("type", fmt.Sprintf("%T", err))}
	if coded, ok := err.(interface{ Code() int }); ok {
		attrs = append(attrs, slog.Int("code", coded.Code()))
	}
	switch value := err.(type) {
	case *fixedError:
		attrs = append(attrs, slog.String("message", value.Error()))
	case *stageError:
		attrs = append(attrs, slog.String("stage", value.stage))
	case syscall.Errno:
		attrs = append(attrs, slog.Int("errno", int(value)), slog.String("message", value.Error()))
	case *net.OpError:
		attrs = append(attrs, slog.String("operation", value.Op), slog.String("network", value.Net))
	case *fs.PathError:
		attrs = append(attrs, slog.String("operation", value.Op))
	default:
		for _, known := range []error{context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, io.ErrShortWrite, net.ErrClosed, fs.ErrNotExist, fs.ErrPermission, fs.ErrExist} {
			if err == known {
				attrs = append(attrs, slog.String("message", known.Error()))
				break
			}
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for index, child := range joined.Unwrap() {
			attrs = append(attrs, slog.Any(fmt.Sprintf("cause%d", index), Cause(child)))
		}
	} else if child := errors.Unwrap(err); child != nil {
		attrs = append(attrs, slog.Any("cause", Cause(child)))
	}
	return slog.GroupValue(attrs...)
}
