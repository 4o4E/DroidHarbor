package capture

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

const serverArtifactSize = 733930

// loadServerArtifact loads only the pinned scrcpy v5.0.1 artifact. The caller
// retains the returned immutable byte slice for the lifetime of its manager.
func loadServerArtifact(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("scrcpy server artifact path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open scrcpy server artifact: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, serverArtifactSize+1))
	if err != nil {
		return nil, fmt.Errorf("read scrcpy server artifact: %w", err)
	}
	if len(data) != serverArtifactSize {
		return nil, errors.New("scrcpy server artifact has unexpected size")
	}
	digest := sha256.Sum256(data)
	want := [32]byte{0x76, 0x4e, 0xb6, 0xf7, 0x98, 0x11, 0xd5, 0x21, 0x1f, 0xe9, 0xdf, 0x34, 0x11, 0x20, 0x88, 0x2b, 0xa9, 0x99, 0x4c, 0x7a, 0x61, 0xb8, 0x97, 0xd7, 0xbf, 0x3f, 0xb6, 0x62, 0xe5, 0x3b, 0xc5, 0x36}
	if digest != want {
		return nil, errors.New("scrcpy server artifact checksum mismatch")
	}
	return data, nil
}
