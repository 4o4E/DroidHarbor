package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"droidharbor/internal/capture"
)

func retainedTestSession(t *testing.T, m *Manager, id string) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	interactionCtx, cancelInteraction := context.WithCancel(ctx)
	s := &session{
		manager: m, id: id, owner: "owner", mode: "control", ctx: ctx, cancel: cancel,
		interactionCtx: interactionCtx, cancelInteraction: cancelInteraction,
		hub:             &hub{deviceID: id, source: &capture.Stream{}, epoch: 1},
		frameTimestamps: [maxFrameBoundaries]uint32{42}, frameTimestampCount: 1, ready: true,
	}
	t.Cleanup(func() { m.clearRetention(s); cancel() })
	return s
}

func TestSuspensionRevokesOperationsWithoutEndingMedia(t *testing.T) {
	m := &Manager{deps: Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	s := retainedTestSession(t, m, "a")
	operationContext := s.interactionCtx
	if err := s.suspend(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(operationContext.Err(), context.Canceled) || s.ctx.Err() != nil {
		t.Fatal("suspension must cancel window operations while keeping media alive")
	}
	if !s.acknowledgeFrame(1, 42) {
		t.Fatal("a retained decoder could not acknowledge its received frame")
	}
	if err := s.check(context.Background(), true); errorCode(err) != "SESSION_SUSPENDED" {
		t.Fatalf("a received frame restored background input authority: %v", err)
	}
	if _, err := s.interaction(); errorCode(err) != "SESSION_SUSPENDED" {
		t.Fatalf("background window admitted a new HTTP operation: %v", err)
	}
}

func TestReturningClaimsExistingSessionWithoutExtendingItsDeadline(t *testing.T) {
	m := &Manager{deps: Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	a, b := retainedTestSession(t, m, "a"), retainedTestSession(t, m, "b")
	if err := m.retainSession(a, ""); err != nil {
		t.Fatal(err)
	}
	deadline := a.retention.until
	if err := m.retainSession(b, a.id); err != nil {
		t.Fatal(err)
	}
	if a.ctx.Err() != nil || b.ctx.Err() != nil || !a.retention.until.Equal(deadline) {
		t.Fatal("returning closed an existing stream or extended its deadline")
	}
	m.clearRetention(a)
	if a.ctx.Err() != nil || b.ctx.Err() != nil || b.retention == nil {
		t.Fatal("resuming the target ended a live transport or consumed the departing hold")
	}
}
