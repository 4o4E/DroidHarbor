package platform

import (
	"fmt"
	"strings"
	"testing"
)

func TestFilePreviewTruncatesAtUTF8Boundary(t *testing.T) {
	prefix := strings.Repeat("a", previewTextLimit-1)
	body := prefix + "文"
	response := []byte(fmt.Sprintf("%d\x00%s", len(body), body[:previewTextLimit]))
	preview, ok := decodeFilePreview(response, previewTextLimit, 0)
	if !ok || preview.Kind != "text" || !preview.Truncated || preview.Text != prefix || preview.Size != int64(len(body)) {
		t.Fatalf("valid=%t kind=%s truncated=%t text-bytes=%d size=%d", ok, preview.Kind, preview.Truncated, len(preview.Text), preview.Size)
	}
}

func TestFilePreviewRejectsIncompleteReadsAndPreservesBinaryBytes(t *testing.T) {
	if _, ok := decodeFilePreview([]byte("20\x00short response"), previewTextLimit, 0); ok {
		t.Fatal("accepted an incomplete device file read")
	}
	preview, ok := decodeFilePreview([]byte("5\x00\x00\x01abc"), previewTextLimit, 0)
	if !ok || preview.Kind != "binary" || preview.Size != 5 || string(preview.Bytes) != "\x00\x01abc" || preview.Text != "" || len(preview.Image) != 0 {
		t.Fatalf("binary file did not return exact hex-view bytes: %+v (valid=%t)", preview, ok)
	}
}
