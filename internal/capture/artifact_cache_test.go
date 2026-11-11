package capture

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestCachedArtifactRequiresPinnedContentAtExpectedPath(t *testing.T) {
	artifact, err := loadServerArtifact("")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifact)
	valid := fmt.Sprintf("%x  %s\n", digest, serverCachePath)
	if !validCachedArtifact([]byte(valid)) {
		t.Fatal("verified bundled program was rejected from its version cache")
	}
	for name, output := range map[string]string{
		"changed-program":    strings.Repeat("0", 64) + "  " + serverCachePath,
		"other-file":         fmt.Sprintf("%x  /data/local/tmp/another.jar", digest),
		"multiple-records":   valid + valid,
		"truncated-checksum": valid[1:],
		"non-hex-checksum":   strings.Repeat("z", 64) + "  " + serverCachePath,
	} {
		t.Run(name, func(t *testing.T) {
			if validCachedArtifact([]byte(output)) {
				t.Fatal("unverified cached code was accepted for execution")
			}
		})
	}
}
