package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func boundedViewer() *session {
	return &session{manager: &Manager{deps: Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}},
		hub: &hub{epoch: 1}, ctx: context.Background(), peerReadyClosed: true,
		frames: make(chan *videoFrame, maxQueuedFrames), frameGenerations: make(map[*videoFrame]uint64)}
}

func TestViewerBurstRecoversAtCompleteKeyframeWithoutClosingSession(t *testing.T) {
	for _, test := range []struct {
		name        string
		count, size int
	}{
		{"frame-count", maxQueuedFrames, 1}, {"bytes", 4, maxFrameQueueBytes / 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, other := boundedViewer(), boundedViewer()
			other.hub = s.hub
			initial := &videoFrame{epoch: 1, key: true, timestamp: 1}
			if sent, _ := s.markFrameSent(initial, 0); !sent || !s.acknowledgeFrame(1, 1) {
				t.Fatal("initial frame was not ready")
			}
			if err := other.offerFrame(initial); err != nil {
				t.Fatal(err)
			}
			for i := range test.count {
				if err := s.offerFrame(&videoFrame{epoch: 1, size: test.size, key: i == 0}); err != nil {
					t.Fatal(err)
				}
			}
			generation := s.frameGeneration
			if err := s.offerFrame(&videoFrame{epoch: 1, size: 1}); err != nil {
				t.Fatalf("transient burst ended viewer: %v", err)
			}
			if !s.awaitingKey || s.queuedFrameBytes != 0 || len(s.frames) != 0 || s.frameReady(1) || s.frameCurrent(initial, generation) {
				t.Fatal("incomplete GOP or stale input readiness survived overflow")
			}
			if len(other.frames) != 1 || other.frameGeneration != 0 {
				t.Fatal("other viewer's queue was changed")
			}
			for range 20 {
				if err := s.offerFrame(&videoFrame{epoch: 1, size: 1}); err != nil {
					t.Fatal(err)
				}
			}
			if len(s.frames) != 0 {
				t.Fatal("P frames were forwarded without a keyframe")
			}
			deadline := s.backpressureSince.Add(keyFrameTimeout)
			key := &videoFrame{epoch: 1, size: 16, key: true, timestamp: 100}
			if err := s.offerFrame(key); err != nil {
				t.Fatal(err)
			}
			generation = s.frameGenerations[key]
			s.requestKeyFrame()
			if s.frameGenerations[key] != generation || len(s.frames) != 1 {
				t.Fatal("duplicate PLI discarded the pending recovery keyframe")
			}
			if err := s.frameWaitError(deadline); err == nil {
				t.Fatal("queuing a keyframe incorrectly cleared recovery deadline")
			}
			if s.frameReady(1) || s.acknowledgeFrame(1, 100) {
				t.Fatal("queued frame granted input before transmission")
			}
			if sent, notice := s.markFrameSent(key, generation); !sent || !notice {
				t.Fatal("complete recovery keyframe was not accepted")
			}
			if err := s.frameWaitError(deadline); err != nil {
				t.Fatalf("successful sending retained backpressure failure: %v", err)
			}
			if s.frameReady(1) || !s.acknowledgeFrame(1, 100) || !s.frameReady(1) {
				t.Fatal("recovery bypassed actual presentation acknowledgement")
			}
		})
	}
}

func TestRepeatedOverflowCannotExtendRecoveryDeadline(t *testing.T) {
	s := boundedViewer()
	for range 2 {
		for i := range maxQueuedFrames {
			if err := s.offerFrame(&videoFrame{size: 1, key: i == 0}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.offerFrame(&videoFrame{size: 1}); err != nil {
			t.Fatal(err)
		}
		if s.queuedFrameBytes != 0 || len(s.frames) != 0 {
			t.Fatal("overflow exceeded bounded queue")
		}
		if s.backpressureSince.IsZero() {
			t.Fatal("missing recovery deadline")
		}
		// Keep the original deadline even after another GOP arrives and overflows.
		if s.waitingSince != s.backpressureSince {
			t.Fatal("repeat overflow restarted recovery timer")
		}
	}
	err := s.frameWaitError(s.backpressureSince.Add(keyFrameTimeout))
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "SLOW_VIEWER" || failure.Cause == nil {
		t.Fatalf("persistent congestion did not retain a diagnostic failure: %v", err)
	}
}

func TestDequeuedOldGenerationDoesNotConsumeNewQueueBudget(t *testing.T) {
	s := boundedViewer()
	old := &videoFrame{key: true, size: maxFrameQueueBytes}
	if err := s.offerFrame(old); err != nil {
		t.Fatal(err)
	}
	generation := s.frameGenerations[old]
	<-s.frames // Sender has dequeued, but has not yet acquired the accounting lock.
	key := &videoFrame{key: true, size: 16}
	if err := s.offerFrame(key); err != nil {
		t.Fatal(err)
	}
	if len(s.frames) != 1 || <-s.frames != key || s.queuedFrameBytes != 16 || s.frameCurrent(old, generation) {
		t.Fatal("dequeued old frame leaked budget or survived queue invalidation")
	}
	if _, exists := s.frameGenerations[old]; exists {
		t.Fatal("old dequeued generation remained eligible for sending")
	}
}

func TestOversizedFrameStillFailsWithoutChangingQueuedContent(t *testing.T) {
	s := boundedViewer()
	key := &videoFrame{key: true, size: 8}
	if err := s.offerFrame(key); err != nil {
		t.Fatal(err)
	}
	err := s.offerFrame(&videoFrame{key: true, size: maxFrameQueueBytes + 1})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "SLOW_VIEWER" {
		t.Fatalf("oversized frame was accepted: %v", err)
	}
	if s.queuedFrameBytes != 8 || len(s.frames) != 1 || <-s.frames != key {
		t.Fatal("oversized frame altered bounded queue")
	}
}

func TestRepeatedKeyframeFeedbackSharesOutstandingRequest(t *testing.T) {
	s := boundedViewer()
	s.requestKeyFrame()
	generation, deadline := s.frameGeneration, s.waitingSince
	s.requestKeyFrame()
	if s.frameGeneration != generation || s.waitingSince != deadline {
		t.Fatal("duplicate feedback restarted generation or deadline")
	}
	key := &videoFrame{epoch: 1, key: true, size: 16, timestamp: 100}
	if err := s.offerFrame(key); err != nil {
		t.Fatal(err)
	}
	s.requestKeyFrame()
	if s.frameGeneration != generation || s.waitingSince != deadline || len(s.frames) != 1 {
		t.Fatal("duplicate feedback discarded a queued but unsent keyframe")
	}
	if err := s.frameWaitError(deadline.Add(keyFrameTimeout + time.Millisecond)); err == nil {
		t.Fatal("keyframe request never timed out")
	}
	if sent, _ := s.markFrameSent(key, generation); !sent {
		t.Fatal("requested keyframe could not complete")
	}
	if err := s.frameWaitError(deadline.Add(keyFrameTimeout + time.Millisecond)); err != nil {
		t.Fatal("sent keyframe left the request outstanding")
	}
	s.requestKeyFrame()
	if s.frameGeneration == generation || s.waitingSince.IsZero() {
		t.Fatal("completed recovery prevented a later genuine keyframe request")
	}
}
