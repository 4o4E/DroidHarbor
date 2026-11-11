package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

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
	if err := s.suspend(context.Background()); err != nil {
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

func TestThreeDevicesKeepIndependentStreamsAndRestartDepartureDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &Manager{deps: Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
		a, b, c := retainedTestSession(t, m, "a"), retainedTestSession(t, m, "b"), retainedTestSession(t, m, "c")
		for _, s := range []*session{a, b, c} {
			if err := m.retainSession(s); err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Second)
			if s == b {
				// Returning A cancels its first deadline, while B keeps its own.
				m.clearRetention(a)
			}
		}
		if a.ctx.Err() != nil || b.ctx.Err() != nil || c.ctx.Err() != nil {
			t.Fatal("switching a third device ended an existing stream")
		}
		if err := m.retainSession(a); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if a.ctx.Err() != nil || b.ctx.Err() != nil || c.ctx.Err() != nil {
			t.Fatal("a previous departure deadline ended a returned or unrelated stream")
		}
	})
}
