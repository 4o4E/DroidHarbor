package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"droidharbor/internal/capture"
)

func TestRetiredAudioViewerCannotStopCurrentPlayback(t *testing.T) {
	for _, retired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		id := "other-window"
		if retired {
			id = "playing-window"
			cancel()
		}
		source := new(capture.Stream)
		h := &hub{manager: &Manager{deps: Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}, audio: deviceAudio{source: source, initialized: true, route: "browser", target: "browser", status: "ready", owner: "playing-window", revision: 7}}
		before := h.audioSnapshot()
		h.audioPeerFailed(&session{id: id, ctx: ctx, hub: h}, errors.New("viewer RTCP ended"))
		cancel()
		if h.audioSnapshot() != before || h.audio.source != source {
			t.Fatalf("unrelated viewer failure stopped playback: before=%+v after=%+v", before, h.audioSnapshot())
		}
	}
}
