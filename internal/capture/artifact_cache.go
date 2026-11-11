package capture

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"droidharbor/internal/diagnostics"
)

const serverCachePath = "/data/local/tmp/droidharbor-cache-scrcpy-v5.0.1.jar"

func validCachedArtifact(output []byte) bool {
	output = bytes.TrimSpace(output)
	hash, path, found := bytes.Cut(output, []byte(" "))
	if !found || len(hash) != 64 || !bytes.Equal(bytes.TrimSpace(path), []byte(serverCachePath)) {
		return false
	}
	var digest [32]byte
	_, err := hex.Decode(digest[:], hash)
	return err == nil && digest == serverArtifactDigest
}

// A shared immutable cache avoids another ADB upload. Each launch still uses
// its journaled unique JAR symlink, preserving process/helper cleanup identity.
func (m *Manager) prepareArtifact(ctx context.Context, s *Stream, authorize func() error) error {
	cache, jar := shellQuote(serverCachePath), shellQuote(s.resource.Jar)
	probe := "if [ -f " + cache + " ]; then sha256sum " + cache + "; else printf MISSING; fi"
	result, err := m.adb.RunShell(ctx, s.link.TransportID, probe, 256, authorize)
	if err != nil {
		return diagnostics.WrapStage("capture_cache.probe", err)
	}
	if result.ExitCode != 0 {
		return diagnostics.WrapStage("capture_cache.probe", diagnostics.New(fmt.Sprintf("capture cache inspection failed (exit %d)", result.ExitCode)))
	}
	if bytes.Equal(bytes.TrimSpace(result.Stdout), []byte("MISSING")) {
		if err := m.adb.Push(ctx, s.link.TransportID, s.resource.Jar, 0644, time.Now().UTC(), bytes.NewReader(m.server), authorize, nil); err != nil {
			return diagnostics.WrapStage("capture_cache.push", err)
		}
		// Publish a complete file by rename, never a partly uploaded cache.
		// A concurrent publisher may win; its pinned content must still match.
		publish := "if [ ! -e " + cache + " ]; then mv -n -- " + jar + " " + cache + " || exit; fi; if [ ! -f " + jar + " ]; then ln -s -- " + cache + " " + jar + " || exit; fi; sha256sum " + cache
		result, err = m.adb.RunShell(ctx, s.link.TransportID, publish, 256, authorize)
		if err != nil {
			return diagnostics.WrapStage("capture_cache.publish", err)
		}
		if result.ExitCode != 0 {
			return diagnostics.WrapStage("capture_cache.publish", diagnostics.New(fmt.Sprintf("capture cache publication failed (exit %d)", result.ExitCode)))
		}
		if !validCachedArtifact(result.Stdout) {
			return diagnostics.WrapStage("capture_cache.verify", diagnostics.New("published capture cache content does not match locked artifact"))
		}
		m.logger.Info("capture_artifact_uploaded", "deviceId", s.deviceID, "captureId", s.id, "bytes", len(m.server))
		return nil
	}
	if !validCachedArtifact(result.Stdout) {
		return diagnostics.WrapStage("capture_cache.verify", diagnostics.New("capture cache content conflicts with locked artifact"))
	}
	linked, err := m.adb.RunShell(ctx, s.link.TransportID, "ln -s -- "+cache+" "+jar+" && printf LINKED", 32, authorize)
	if err != nil {
		return diagnostics.WrapStage("capture_cache.link", err)
	}
	if linked.ExitCode != 0 || string(linked.Stdout) != "LINKED" {
		return diagnostics.WrapStage("capture_cache.link", diagnostics.New(fmt.Sprintf("capture artifact runtime link failed (exit %d)", linked.ExitCode)))
	}
	m.logger.Info("capture_artifact_reused", "deviceId", s.deviceID, "captureId", s.id)
	return nil
}
