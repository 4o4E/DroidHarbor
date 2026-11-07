package previews

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

func TestEncodeScalesWithinBoundsAndPreservesAspect(t *testing.T) {
	input := image.NewRGBA(image.Rect(0, 0, 1000, 500))
	input.Set(10, 10, color.White)
	var encodedPNG bytes.Buffer
	if err := png.Encode(&encodedPNG, input); err != nil {
		t.Fatal(err)
	}
	captured := time.Now().UTC()
	snapshot, err := encode(encodedPNG.Bytes(), captured, "device", 7)
	if err != nil {
		t.Fatal(err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(snapshot.JPEG))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 480 || config.Height != 240 {
		t.Fatalf("unexpected scaled dimensions %dx%d", config.Width, config.Height)
	}
	if snapshot.DeviceID != "device" || snapshot.LinkGeneration != 7 || !snapshot.CapturedAt.Equal(captured) {
		t.Fatalf("metadata not preserved: %#v", snapshot)
	}
}

func TestEncodeRejectsOversizedImageFromHeaderBeforeDecode(t *testing.T) {
	pngHeader := make([]byte, 33)
	copy(pngHeader, []byte("\x89PNG\r\n\x1a\n"))
	binary.BigEndian.PutUint32(pngHeader[8:12], 13)
	copy(pngHeader[12:16], "IHDR")
	binary.BigEndian.PutUint32(pngHeader[16:20], 5000)
	binary.BigEndian.PutUint32(pngHeader[20:24], 4000)
	pngHeader[24] = 8
	pngHeader[25] = 6
	binary.BigEndian.PutUint32(pngHeader[29:33], crc32.ChecksumIEEE(pngHeader[12:29]))
	if _, err := encode(pngHeader, time.Now(), "device", 1); err != ErrImageTooLarge {
		t.Fatalf("oversized dimensions error = %v, want %v", err, ErrImageTooLarge)
	}
}
