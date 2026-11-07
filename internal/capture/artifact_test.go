package capture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadServerArtifactRejectsWrongSizeAndDigest(t *testing.T) {
	for name, contents := range map[string][]byte{"empty": nil, "wrong-size": []byte("not the server"), "wrong-digest": make([]byte, serverArtifactSize)} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.jar")
			if err := os.WriteFile(path, contents, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadServerArtifact(path); err == nil {
				t.Fatal("untrusted artifact was accepted")
			}
		})
	}
}

func TestLoadServerArtifactAcceptsPinnedArtifact(t *testing.T) {
	data, err := loadServerArtifact(filepath.Join("..", "..", ".tools", "scrcpy-server-v5.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != serverArtifactSize {
		t.Fatalf("artifact size = %d, want %d", len(data), serverArtifactSize)
	}
}
