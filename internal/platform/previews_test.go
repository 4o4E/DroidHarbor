package platform

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreviewCancellationDoesNotMasqueradeAsCaptureFailure(t *testing.T) {
	for _, test := range []struct {
		name           string
		clientCanceled bool
		cause          error
		status         int
	}{
		{"client-canceled", true, context.Canceled, 499},
		{"capture-canceled", false, context.Canceled, 503},
		{"capture-timeout", false, context.DeadlineExceeded, 503},
		{"capture-failed-after-client-left", true, errors.New("capture failed"), 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			app := &Server{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			req := httptest.NewRequest("GET", "http://platform.test/api/v1/devices/phone/preview", nil)
			req.SetPathValue("id", "phone")
			if test.clientCanceled {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			app.previewError(response, req, test.cause)
			if response.Code != test.status {
				t.Fatalf("wrong preview result: %d, %s", response.Code, response.Body.String())
			}
			if test.status == 499 {
				if response.Body.Len() != 0 || !strings.Contains(logs.String(), "preview_request_canceled") || strings.Contains(logs.String(), "PREVIEW_CAPTURE_FAILED") {
					t.Fatal("client cancellation was reported as a capture failure")
				}
			} else if !strings.Contains(response.Body.String(), "PREVIEW_CAPTURE_FAILED") || !strings.Contains(logs.String(), "request_rejected") {
				t.Fatal("real capture failure was hidden")
			}
		})
	}
}
