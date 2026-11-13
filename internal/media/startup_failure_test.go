package media

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"droidharbor/internal/diagnostics"
)

func TestCaptureStartupRecoveryKeepsPermanentFailureAndCancellationDistinct(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		code  string
	}{
		{"socket closed", diagnostics.WrapStage("capture_start.socket_ready", &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}), "CAPTURE_START_INTERRUPTED"},
		{"truncated transport", io.ErrUnexpectedEOF, "CAPTURE_START_INTERRUPTED"},
		{"startup timeout", context.DeadlineExceeded, "CAPTURE_START_INTERRUPTED"},
		{"encoder configuration", diagnostics.New("unsupported encoder configuration"), "CAPTURE_START_FAILED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := captureStartFailure(context.Background(), test.cause)
			var mediaError *Error
			if !errors.As(failure, &mediaError) || mediaError.Status != 503 || mediaError.Code != test.code || !errors.Is(failure, test.cause) {
				t.Fatalf("incorrect consumer recovery decision or lost cause: %#v", failure)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failure := captureStartFailure(ctx, &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed})
	var mediaError *Error
	if !errors.Is(failure, context.Canceled) || errors.As(failure, &mediaError) {
		t.Fatalf("user-canceled capture became a retriable startup failure: %#v", failure)
	}
}
