package media

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"droidharbor/internal/capture"
)

func TestSwitchClaimsWarmTargetBeforeRetainingDepartingDevice(t *testing.T) {
	m := &Manager{joining: make(map[string]*retainedCapture), deps: Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	m.sessions = map[string]*session{"b-to-a": {owner: "owner"}}
	// Native streams serve only as identities here. The transition must cancel
	// timers without ever invoking native cleanup on the resumed target.
	aSource, bSource := &capture.Stream{}, &capture.Stream{}
	a, b := &hub{deviceID: "a", source: aSource}, &hub{deviceID: "b", source: bSource}
	deadline := time.Now().Add(time.Hour)
	entry := func(h *hub, source *capture.Stream, id string) *retainedCapture {
		e := &retainedCapture{hub: h, source: source, owner: "owner", switchID: id, until: deadline, done: make(chan struct{})}
		m.watcher.Add(1)
		e.timer = time.AfterFunc(time.Hour, func() { t.Error("test retention timer unexpectedly expired") })
		return e
	}
	originalA := entry(a, aSource, "a-to-b")
	m.retained = originalA
	t.Cleanup(func() { m.resumeRetained(a); m.resumeRetained(b); m.watcher.Wait() })
	m.claimSwitchTarget("owner", "b-to-a", "a")
	pendingA := m.joining["a"]
	if m.retained != nil || pendingA == nil || pendingA.source != aSource || !pendingA.until.Equal(deadline) {
		t.Fatal("switch did not claim its existing target without extending the deadline")
	}
	select {
	case <-originalA.done:
	default:
		t.Fatal("original idle hold remained live after target handoff")
	}
	departingB := entry(b, bSource, "b-to-a")
	m.retained = departingB
	m.resumeRetained(a)
	if len(m.joining) != 0 || m.retained != departingB || a.source != aSource {
		t.Fatal("returning viewer lost its target or evicted the newly departed device")
	}
	select {
	case <-pendingA.done:
	default:
		t.Fatal("joining hold was not consumed by its returning viewer")
	}
	m.sessions["cancelled-switch"] = &session{owner: "owner", switchCancelled: true}
	m.claimSwitchTarget("owner", "cancelled-switch", "b")
	if m.retained != departingB || len(m.joining) != 0 {
		t.Fatal("a cancellation arriving before its end request recreated a joining hold")
	}
}
