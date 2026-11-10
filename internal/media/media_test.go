package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"testing"

	"droidharbor/internal/capture"
	"droidharbor/internal/scrcpy"
)

func TestInputKeyOwnershipReleasesOnlyLastOwner(t *testing.T) {
	var state inputState
	var sent []scrcpy.ControlMessage
	send := func(_ context.Context, message scrcpy.ControlMessage, authorize func() error) error {
		if err := authorize(); err != nil {
			return err
		}
		sent = append(sent, message)
		return nil
	}
	authorize := func() error { return nil }
	ctx := context.Background()
	if err := state.key(ctx, "viewer-a", scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 29, Action: 0}, send, authorize); err != nil {
		t.Fatal(err)
	}
	if err := state.key(ctx, "viewer-b", scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 29, Action: 0}, send, authorize); err != nil {
		t.Fatal(err)
	}
	if err := state.release(ctx, "viewer-a", 1080, 1920, send, authorize); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatalf("releasing one of two owners sent an unintended key event: %#v", sent)
	}
	if err := state.release(ctx, "viewer-b", 1080, 1920, send, authorize); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Action != 1 || sent[1].Keycode != 29 {
		t.Fatalf("last owner did not release the original key: %#v", sent)
	}
}

func TestFrameAcknowledgementAcceptsSentPresentationBoundaryOnly(t *testing.T) {
	s := &session{
		hub:                 &hub{epoch: 7},
		frameTimestamps:     [maxFrameBoundaries]uint32{1234, 1240},
		frameTimestampCount: 2,
	}
	if s.acknowledgeFrame(6, 1240) {
		t.Fatal("accepted stale capture epoch")
	}
	if s.acknowledgeFrame(7, 4321) {
		t.Fatal("accepted an RTP timestamp not sent by this viewer")
	}
	if !s.acknowledgeFrame(7, 1240) {
		t.Fatal("rejected a sent post-keyframe presentation boundary")
	}
	if !s.frameReady(7) || s.frameReady(6) {
		t.Fatal("frame readiness was not scoped to the acknowledged epoch")
	}
}

func TestControlPayloadRejectsUnknownAndTrailingFields(t *testing.T) {
	for _, payload := range []string{`{"x":1,"unexpected":true}`, `{"x":1}{"x":2}`} {
		var decoded struct {
			X int `json:"x"`
		}
		if err := decodeControlPayload(json.RawMessage(payload), &decoded); err == nil {
			t.Fatalf("accepted invalid payload %s", payload)
		}
	}
}

func TestFinalViewerCleanupPreservesFailureAndSeparatesResourceCleanup(t *testing.T) {
	startFailure := &hub{status: "failed", errorCode: "CAPTURE_START_FAILED"}
	startFailure.captureStopped(nil, nil)
	if startFailure.status != "failed" || startFailure.errorCode != "CAPTURE_START_FAILED" || startFailure.cleanupPending {
		t.Fatalf("clean viewer stop erased capture start failure: %#v", startFailure)
	}

	inputFailure := &hub{status: "running"}
	inputFailure.captureStopped(nil, problem(503, "INPUT_RELEASE_FAILED", "input release failed"))
	if inputFailure.status != "failed" || inputFailure.errorCode != "INPUT_RELEASE_FAILED" || inputFailure.cleanupPending {
		t.Fatalf("input release failure was not preserved independently of resource cleanup: %#v", inputFailure)
	}

	stopFailure := &hub{status: "stopping"}
	stopFailure.captureStopped(problem(503, "CAPTURE_CLEANUP_FAILED", "capture cleanup failed"), nil)
	if stopFailure.status != "failed" || !stopFailure.cleanupPending || stopFailure.errorCode != "CAPTURE_CLEANUP_FAILED" {
		t.Fatalf("capture cleanup failure was not retained as pending: %#v", stopFailure)
	}
}

func TestRetiringSourceEventsCannotFailCurrentHub(t *testing.T) {
	old := new(capture.Stream)
	current := new(capture.Stream)
	h := &hub{source: current, status: "running"}
	h.sourceEnded(old)
	if h.status != "running" {
		t.Fatalf("retired source termination changed current hub state to %q", h.status)
	}
	retiring := &hub{cleanupSource: old, cleanupPending: true, status: "stopping"}
	retiring.sourceEnded(old)
	if retiring.status != "stopping" || !retiring.cleanupPending {
		t.Fatalf("normal retirement event changed pending cleanup state: %#v", retiring)
	}
}

func TestRetiredSessionPacketDoesNotReleaseInputs(t *testing.T) {
	ctx := context.Background()
	sent := 0
	send := func(context.Context, scrcpy.ControlMessage, func() error) error { sent++; return nil }
	authorize := func() error { return nil }
	var inputs inputState
	if err := inputs.key(ctx, "viewer", scrcpy.ControlMessage{Type: scrcpy.ControlInjectKeycode, Keycode: 29, Action: 0}, send, authorize); err != nil {
		t.Fatal(err)
	}
	old, current := new(capture.Stream), new(capture.Stream)
	h := &hub{source: current, inputs: inputs, epoch: 4}
	if err := h.beginEpoch(old, scrcpy.Packet{Session: true, Width: 1080, Height: 1920}); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || h.inputs.keys[29].count != 1 {
		t.Fatalf("retired Session packet injected a key-up: sends=%d keys=%#v", sent, h.inputs.keys)
	}
}

func TestControlPayloadRequiresExplicitBooleanAndRejectsNull(t *testing.T) {
	for _, payload := range []string{"", "null", "{}", `{"on":null}`} {
		var decoded displayPowerPayload
		if err := decodeControlPayload(json.RawMessage(payload), &decoded, "on"); err == nil {
			t.Fatalf("accepted missing display-power value %q", payload)
		}
	}
	var decoded displayPowerPayload
	if err := decodeControlPayload(json.RawMessage(`{"on":false}`), &decoded, "on"); err != nil || decoded.On {
		t.Fatalf("rejected explicit display-power off: %#v, %v", decoded, err)
	}
	for _, payload := range []string{`{}`, `{"action":0}`, `{"keyCode":0}`} {
		var key keyPayload
		if err := decodeControlPayload(json.RawMessage(payload), &key, "keyCode", "action"); err == nil {
			t.Fatalf("accepted key payload missing a required field: %s", payload)
		}
	}
	var key keyPayload
	if err := decodeControlPayload(json.RawMessage(`{"keyCode":0,"action":0}`), &key, "keyCode", "action"); err != nil {
		t.Fatalf("rejected explicit zero-valued key fields: %v", err)
	}
	var touch touchPayload
	if err := decodeControlPayload(json.RawMessage(`{"pointerId":0,"action":0,"x":0,"y":0,"width":1,"height":1}`), &touch, "pointerId", "action", "x", "y", "width", "height", "pressure"); err == nil {
		t.Fatal("accepted touch payload without required pressure")
	}
}

func TestPNGValidationChecksChunksAndRejectsTrailingBytes(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	valid := encoded.Bytes()
	if err := validatePNGStructure(valid); err != nil {
		t.Fatalf("rejected valid PNG: %v", err)
	}
	if err := validatePNGStructure(valid[:len(valid)-1]); err == nil {
		t.Fatal("accepted PNG truncated before IEND CRC")
	}
	if err := validatePNGStructure(append(append([]byte(nil), valid...), 0)); err == nil {
		t.Fatal("accepted bytes after IEND")
	}
	corrupt := append([]byte(nil), valid...)
	corrupt[29] ^= 1
	if err := validatePNGStructure(corrupt); err == nil {
		t.Fatal("accepted invalid IHDR CRC")
	}
}

func TestFrameInvalidationFlushesAndUnlocks(t *testing.T) {
	frame := &videoFrame{size: 12}
	s := &session{
		frames:           make(chan *videoFrame, maxQueuedFrames),
		queuedFrameBytes: 12,
		frameGenerations: map[*videoFrame]uint64{frame: 4},
	}
	s.frames <- frame
	s.invalidateFrame()
	if s.queuedFrameBytes != 0 || len(s.frames) != 0 || len(s.frameGenerations) != 0 {
		t.Fatal("frame invalidation did not flush bounded viewer state")
	}
	if !s.mu.TryLock() {
		t.Fatal("frame invalidation returned with the session mutex locked")
	}
	s.mu.Unlock()
}

func TestUnknownCaptureOutcomeIsNotReportedAsFailed(t *testing.T) {
	s := &session{id: "human", ctx: context.Background(), output: make(chan []byte, 1)}
	s.reply(3, 7, "clipboard.result", "failed", errors.Join(capture.ErrResultUnknown, errors.New("late acknowledgement")), nil)
	var response controlReply
	if err := json.Unmarshal(<-s.output, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "unknown" || response.ErrorCode != "RESULT_UNKNOWN" {
		t.Fatalf("uncertain capture result was reported as a definite failure: %#v", response)
	}
}

func TestViewerQueueRejectsOverLimitFramesWithoutChangingQueuedContent(t *testing.T) {
	h := &hub{}
	newViewer := func() *session {
		return &session{
			hub:              h,
			ctx:              context.Background(),
			peerReadyClosed:  true,
			awaitingKey:      true,
			frames:           make(chan *videoFrame, maxQueuedFrames),
			frameGenerations: make(map[*videoFrame]uint64),
		}
	}

	byteBoundViewer := newViewer()
	frameSize := maxFrameQueueBytes / 4
	byteBoundFrames := make([]*videoFrame, 4)
	for i := range byteBoundFrames {
		frame := &videoFrame{size: frameSize, key: i == 0}
		byteBoundFrames[i] = frame
		if err := byteBoundViewer.offerFrame(frame); err != nil {
			t.Fatalf("viewer rejected frame %d before reaching its byte bound: %v", i, err)
		}
	}
	if err := byteBoundViewer.offerFrame(&videoFrame{size: 1}); err == nil {
		t.Fatal("viewer accepted a frame beyond its queued-byte bound")
	} else {
		var mediaErr *Error
		if !errors.As(err, &mediaErr) || mediaErr.Code != "SLOW_VIEWER" {
			t.Fatalf("byte-bound overflow returned the wrong error: %v", err)
		}
	}
	if byteBoundViewer.queuedFrameBytes != maxFrameQueueBytes || len(byteBoundViewer.frames) != len(byteBoundFrames) {
		t.Fatalf("byte-bound rejection changed queue limits: bytes=%d frames=%d", byteBoundViewer.queuedFrameBytes, len(byteBoundViewer.frames))
	}
	for i, want := range byteBoundFrames {
		if got := <-byteBoundViewer.frames; got != want {
			t.Fatalf("byte-bound rejection changed queued frame %d: got %p, want %p", i, got, want)
		}
	}

	countBoundViewer := newViewer()
	countBoundFrames := make([]*videoFrame, maxQueuedFrames)
	for i := range countBoundFrames {
		frame := &videoFrame{size: 1, key: i == 0}
		countBoundFrames[i] = frame
		if err := countBoundViewer.offerFrame(frame); err != nil {
			t.Fatalf("viewer rejected frame %d before reaching its frame-count bound: %v", i, err)
		}
	}
	if err := countBoundViewer.offerFrame(&videoFrame{size: 1}); err == nil {
		t.Fatal("viewer accepted a frame beyond its frame-count bound")
	} else {
		var mediaErr *Error
		if !errors.As(err, &mediaErr) || mediaErr.Code != "SLOW_VIEWER" {
			t.Fatalf("frame-count overflow returned the wrong error: %v", err)
		}
	}
	if countBoundViewer.queuedFrameBytes != maxQueuedFrames || len(countBoundViewer.frames) != maxQueuedFrames {
		t.Fatalf("frame-count rejection changed queue limits: bytes=%d frames=%d", countBoundViewer.queuedFrameBytes, len(countBoundViewer.frames))
	}
	for i, want := range countBoundFrames {
		if got := <-countBoundViewer.frames; got != want {
			t.Fatalf("frame-count rejection changed queued frame %d: got %p, want %p", i, got, want)
		}
	}
}

func TestFrameRecoveryWaitsForKeyframeAndRejectsOldGeneration(t *testing.T) {
	var source *capture.Stream // Frame generations are independent of native encoder I/O.
	h := &hub{source: source, epoch: 1}
	s := &session{
		hub:              h,
		ctx:              context.Background(),
		peerReadyClosed:  true,
		frames:           make(chan *videoFrame, maxQueuedFrames),
		frameGenerations: make(map[*videoFrame]uint64),
	}

	initialKey := &videoFrame{source: source, epoch: 1, key: true, timestamp: 100}
	sent, _ := s.markFrameSent(initialKey, s.frameGeneration)
	if !sent || !s.acknowledgeFrame(1, initialKey.timestamp) || !s.frameReady(1) {
		t.Fatal("initial sent keyframe did not establish frame readiness")
	}

	oldFrame := &videoFrame{source: source, epoch: 1, size: 120, timestamp: 200}
	if err := s.offerFrame(oldFrame); err != nil {
		t.Fatalf("could not queue pre-recovery frame: %v", err)
	}
	s.mu.Lock()
	oldGeneration := s.frameGenerations[oldFrame]
	s.mu.Unlock()

	s.requestKeyFrame()
	if s.frameReady(1) || s.acknowledgeFrame(1, initialKey.timestamp) {
		t.Fatal("recovery retained frame readiness or accepted its pre-recovery boundary")
	}
	if s.frameCurrent(oldFrame, oldGeneration) {
		t.Fatal("pre-recovery frame remained current after keyframe recovery")
	}
	if sent, _ := s.markFrameSent(oldFrame, oldGeneration); sent {
		t.Fatal("pre-recovery frame was marked sent after its generation was invalidated")
	}
	if len(s.frames) != 0 || s.queuedFrameBytes != 0 {
		t.Fatal("recovery did not clear the queued old generation")
	}
	nonKey := &videoFrame{source: source, epoch: 1, size: 40, timestamp: 220}
	if err := s.offerFrame(nonKey); err != nil {
		t.Fatalf("non-key recovery frame unexpectedly failed the session: %v", err)
	}
	if len(s.frames) != 0 || s.queuedFrameBytes != 0 {
		t.Fatal("recovery queued a non-key frame before a decodable keyframe")
	}

	newKey := &videoFrame{source: source, epoch: 1, size: 80, key: true, timestamp: 300}
	if err := s.offerFrame(newKey); err != nil {
		t.Fatalf("recovery rejected a decodable keyframe: %v", err)
	}
	s.mu.Lock()
	newGeneration := s.frameGenerations[newKey]
	s.mu.Unlock()
	if newGeneration == oldGeneration || !s.frameCurrent(newKey, newGeneration) {
		t.Fatal("new keyframe did not belong to a current recovery generation")
	}
	if s.frameReady(1) {
		t.Fatal("queued keyframe became ready before it was marked sent and acknowledged")
	}
	sent, _ = s.markFrameSent(newKey, newGeneration)
	if !sent || !s.acknowledgeFrame(1, newKey.timestamp) || !s.frameReady(1) {
		t.Fatal("new sent keyframe boundary did not restore frame readiness")
	}
}
