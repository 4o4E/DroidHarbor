package media

import (
	"context"
	"testing"
	"time"
)

func startupViewer() *session {
	return &session{
		hub: &hub{epoch: 1}, ctx: context.Background(),
		awaitingKey: true, peerReady: make(chan struct{}),
		frames:           make(chan *videoFrame, maxQueuedFrames),
		frameGenerations: make(map[*videoFrame]uint64),
	}
}

func TestPreparedKeyframeStillRequiresSentPresentationAcknowledgement(t *testing.T) {
	s := startupViewer()
	key := &videoFrame{epoch: 1, size: 120, key: true, timestamp: 100, observedAt: time.Now()}
	if err := s.offerFrame(key); err != nil {
		t.Fatal(err)
	}
	s.onPeerReady()
	if s.awaitingKey || len(s.frames) != 1 {
		t.Fatal("fresh complete startup frame was discarded during connection readiness")
	}
	generation := s.frameGenerations[key]
	if s.frameReady(1) || s.acknowledgeFrame(1, key.timestamp) {
		t.Fatal("prepared frame granted input before presentation was sent")
	}
	if sent, _ := s.markFrameSent(key, generation); !sent {
		t.Fatal("prepared keyframe could not be sent after negotiation")
	}
	if s.frameReady(1) || !s.acknowledgeFrame(1, key.timestamp) || !s.frameReady(1) {
		t.Fatal("input readiness did not follow actual sent-frame acknowledgement")
	}
}

func TestStartupQueueDiscardsOverflowAsGOPAndWaitsForNewKeyframe(t *testing.T) {
	s := startupViewer()
	oldKey := &videoFrame{epoch: 1, size: 120, key: true, timestamp: 100, observedAt: time.Now()}
	if err := s.offerFrame(oldKey); err != nil {
		t.Fatal(err)
	}
	oldGeneration := s.frameGenerations[oldKey]
	for range maxQueuedFrames {
		if err := s.offerFrame(&videoFrame{epoch: 1, size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if !s.awaitingKey || len(s.frames) != 0 || s.queuedFrameBytes != 0 || s.frameCurrent(oldKey, oldGeneration) {
		t.Fatal("startup overflow preserved an incomplete or old-generation GOP")
	}
	if err := s.offerFrame(&videoFrame{epoch: 1, size: 10}); err != nil || len(s.frames) != 0 {
		t.Fatalf("startup accepted a P frame without its keyframe: %v", err)
	}
	newKey := &videoFrame{epoch: 1, size: 80, key: true, timestamp: 300, observedAt: time.Now()}
	if err := s.offerFrame(newKey); err != nil {
		t.Fatal(err)
	}
	s.onPeerReady()
	if s.awaitingKey || len(s.frames) != 1 || <-s.frames != newKey || s.frameCurrent(oldKey, oldGeneration) {
		t.Fatal("new decodable GOP did not replace the overflowed startup generation")
	}
}

func TestStalePreparedFramesCannotGrantInput(t *testing.T) {
	s := startupViewer()
	key := &videoFrame{epoch: 1, size: 100, key: true, timestamp: 50, observedAt: time.Now().Add(-time.Hour)}
	if err := s.offerFrame(key); err != nil {
		t.Fatal(err)
	}
	generation := s.frameGenerations[key]
	s.onPeerReady()
	if !s.awaitingKey || len(s.frames) != 0 || s.frameCurrent(key, generation) || s.acknowledgeFrame(1, key.timestamp) {
		t.Fatal("stale startup image remained eligible for input")
	}
}
