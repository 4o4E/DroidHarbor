package capture

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"droidharbor/internal/leases"
	"droidharbor/internal/scrcpy"
)

type testSource struct {
	link      leases.Link
	available bool
}

func (s *testSource) ReadLink(context.Context, string) (leases.Link, context.Context, error) {
	return s.link, context.Background(), nil
}
func (s *testSource) HasLink(link leases.Link) bool { return s.available && s.link == link }

func TestSendAuthorizationRefusalDoesNotStopSharedStream(t *testing.T) {
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: true}
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &Manager{source: source, ctx: root, streams: map[string]*Stream{}}
	streamCtx, stop := context.WithCancel(root)
	defer stop()
	stream := &Stream{manager: manager, id: "capture", deviceID: "device", link: link, ctx: streamCtx, cancel: stop, done: make(chan struct{})}
	manager.streams["device"] = stream
	denied := errors.New("operation not authorized")
	if err := stream.Send(context.Background(), scrcpy.ControlMessage{Type: scrcpy.ControlCollapsePanels}, func() error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("Send error = %v, want authorization refusal", err)
	}
	if streamCtx.Err() != nil {
		t.Fatalf("authorization refusal canceled shared stream: %v", streamCtx.Err())
	}
	select {
	case <-stream.Done():
		t.Fatal("authorization refusal closed shared stream")
	default:
	}
}

func TestPinnedLinkAuthorizationRejectsReplacement(t *testing.T) {
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: false}
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &Manager{source: source, ctx: root, streams: map[string]*Stream{}}
	streamCtx, stop := context.WithCancel(root)
	defer stop()
	stream := &Stream{manager: manager, id: "capture", deviceID: "device", link: link, ctx: streamCtx, cancel: stop}
	manager.streams["device"] = stream
	if err := stream.authorizeSystem(); err == nil {
		t.Fatal("lost pinned link was authorized")
	}
	if streamCtx.Err() != nil {
		t.Fatal("link validation itself canceled stream before failure handling")
	}
}

func TestStreamCancellationClosesHeldSockets(t *testing.T) {
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: true}
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	manager := &Manager{source: source, ctx: root, streams: map[string]*Stream{}}
	streamCtx, cancel := context.WithCancel(root)
	left, right := net.Pipe()
	defer right.Close()
	stream := &Stream{manager: manager, id: "capture", deviceID: "device", link: link, ctx: streamCtx, cancel: cancel, shell: left}
	stream.requestStop(nil)
	if streamCtx.Err() == nil {
		t.Fatal("stream cancellation did not cancel its context")
	}
	readResult := make(chan error, 1)
	go func() {
		_, err := right.Read(make([]byte, 1))
		readResult <- err
	}()
	select {
	case err := <-readResult:
		if err == nil {
			t.Fatal("held socket remained open after stream cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("held socket remained open after stream cancellation")
	}
}

type partialControlConn struct{}

func (partialControlConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (partialControlConn) Write(p []byte) (int, error)      { return len(p) / 2, nil }
func (partialControlConn) Close() error                     { return nil }
func (partialControlConn) LocalAddr() net.Addr              { return testAddr("local") }
func (partialControlConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (partialControlConn) SetDeadline(time.Time) error      { return nil }
func (partialControlConn) SetReadDeadline(time.Time) error  { return nil }
func (partialControlConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func TestPartialControlWriteHasUnknownResult(t *testing.T) {
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: true}
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	streamCtx, cancel := context.WithCancel(root)
	manager := &Manager{source: source, ctx: root, streams: make(map[string]*Stream)}
	stream := &Stream{manager: manager, id: "capture", deviceID: "device", link: link, ctx: streamCtx, cancel: cancel, control: partialControlConn{}, frames: make(chan scrcpy.Packet), messages: make(chan scrcpy.DeviceMessage), done: make(chan struct{}), started: make(chan struct{}), workersDone: make(chan struct{})}
	close(stream.started)
	manager.streams["device"] = stream
	err := stream.Send(context.Background(), scrcpy.ControlMessage{Type: scrcpy.ControlInjectText, Text: "x"}, func() error { return nil })
	if !errors.Is(err, ErrResultUnknown) {
		t.Fatalf("Send error=%v, want ErrResultUnknown", err)
	}
	if !errors.Is(stream.Err(), ErrResultUnknown) {
		t.Fatalf("Stream.Err()=%v, want ErrResultUnknown", stream.Err())
	}
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("partial control write did not begin stream cleanup")
	}
}

type resultControlConn struct {
	n          int
	writeErr   error
	restoreErr error
	cancel     context.CancelFunc
}

func (c *resultControlConn) Read([]byte) (int, error) { return 0, net.ErrClosed }
func (c *resultControlConn) Write(p []byte) (int, error) {
	if c.cancel != nil {
		c.cancel()
	}
	n := c.n
	if n < 0 {
		n = len(p)
	}
	if n > len(p) {
		n = len(p)
	}
	return n, c.writeErr
}
func (*resultControlConn) Close() error                    { return nil }
func (c *resultControlConn) LocalAddr() net.Addr           { return testAddr("local") }
func (c *resultControlConn) RemoteAddr() net.Addr          { return testAddr("remote") }
func (*resultControlConn) SetDeadline(time.Time) error     { return nil }
func (*resultControlConn) SetReadDeadline(time.Time) error { return nil }
func (c *resultControlConn) SetWriteDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return c.restoreErr
	}
	return nil
}

func newControlTestStream(conn net.Conn) (*Stream, func()) {
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: true}
	root, cancelRoot := context.WithCancel(context.Background())
	streamCtx, cancelStream := context.WithCancel(root)
	manager := &Manager{source: source, ctx: root, streams: make(map[string]*Stream)}
	stream := &Stream{manager: manager, id: "capture", deviceID: "device", link: link, ctx: streamCtx, cancel: cancelStream, control: conn, frames: make(chan scrcpy.Packet), messages: make(chan scrcpy.DeviceMessage), done: make(chan struct{}), started: make(chan struct{}), workersDone: make(chan struct{})}
	close(stream.started)
	manager.streams["device"] = stream
	return stream, func() { cancelStream(); cancelRoot() }
}

func TestZeroByteEOFIsNotTreatedAsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &resultControlConn{n: 0, writeErr: io.EOF, cancel: cancel}
	stream, release := newControlTestStream(conn)
	defer release()
	err := stream.Send(ctx, scrcpy.ControlMessage{Type: scrcpy.ControlCollapsePanels}, func() error { return nil })
	if !errors.Is(err, io.EOF) || errors.Is(err, ErrResultUnknown) {
		t.Fatalf("Send error=%v, want ordinary EOF", err)
	}
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("zero-byte network failure did not terminate capture")
	}
	if !errors.Is(stream.Err(), io.EOF) {
		t.Fatalf("Stream.Err()=%v, want EOF", stream.Err())
	}
}

func TestFullControlWriteWithErrorIsResultUnknown(t *testing.T) {
	stream, release := newControlTestStream(&resultControlConn{n: -1, writeErr: io.EOF})
	defer release()
	err := stream.Send(context.Background(), scrcpy.ControlMessage{Type: scrcpy.ControlCollapsePanels}, func() error { return nil })
	if !errors.Is(err, ErrResultUnknown) || !errors.Is(err, io.EOF) {
		t.Fatalf("Send error=%v, want unknown result wrapping EOF", err)
	}
}

func TestDeadlineRestoreFailureAfterWriteIsResultUnknown(t *testing.T) {
	stream, release := newControlTestStream(&resultControlConn{n: -1, restoreErr: io.EOF})
	defer release()
	err := stream.Send(context.Background(), scrcpy.ControlMessage{Type: scrcpy.ControlCollapsePanels}, func() error { return nil })
	if !errors.Is(err, ErrResultUnknown) || !errors.Is(err, io.EOF) {
		t.Fatalf("Send error=%v, want unknown result wrapping deadline error", err)
	}
}

func TestLinkContextCancellationFinalizesForStreamConsumers(t *testing.T) {
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 7, Generation: 3}
	source := &testSource{link: link, available: true}
	managerCtx, cancelManager := context.WithCancel(context.Background())
	defer cancelManager()
	manager := &Manager{source: source, ctx: managerCtx, streams: make(map[string]*Stream)}
	linkCtx, cancelLink := context.WithCancel(context.Background())
	defer cancelLink()
	streamCtx, cancelStream := context.WithCancel(managerCtx)
	video, videoPeer := net.Pipe()
	control, controlPeer := net.Pipe()
	shell, shellPeer := net.Pipe()
	defer videoPeer.Close()
	defer controlPeer.Close()
	defer shellPeer.Close()
	stream := &Stream{
		manager: manager, id: "capture", deviceID: "device", link: link,
		ctx: streamCtx, cancel: cancelStream, video: video, control: control, shell: shell,
		frames: make(chan scrcpy.Packet, videoQueueSize), messages: make(chan scrcpy.DeviceMessage, messageQueueSize),
		done: make(chan struct{}), started: make(chan struct{}), workersDone: make(chan struct{}),
		stopLink: context.AfterFunc(linkCtx, cancelStream),
	}
	manager.streams[link.DeviceID] = stream
	stream.workers.Add(3)
	go stream.readFrames()
	go stream.readMessages()
	go stream.drainShell()
	stream.completeStart()
	stream.watchContextCancellation()

	source.available = false
	cancelLink()
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("link cancellation did not finalize capture")
	}
	select {
	case _, open := <-stream.Frames():
		if open {
			t.Fatal("video frame channel remained open after link cancellation")
		}
	default:
		t.Fatal("video frame channel was not closed with Done")
	}
	select {
	case _, open := <-stream.Messages():
		if open {
			t.Fatal("device-message channel remained open after link cancellation")
		}
	default:
		t.Fatal("device-message channel was not closed with Done")
	}
}
