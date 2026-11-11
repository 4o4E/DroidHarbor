package previews

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
)

type previewTestSource struct {
	mu   sync.RWMutex
	link leases.Link
	ctx  context.Context
	live bool
}

func (s *previewTestSource) ReadLink(_ context.Context, _ string) (leases.Link, context.Context, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.link, s.ctx, nil
}

func (s *previewTestSource) HasLink(link leases.Link) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.live && s.ctx.Err() == nil && s.link == link
}

func (s *previewTestSource) invalidate() {
	s.mu.Lock()
	s.live = false
	s.mu.Unlock()
}
func (s *previewTestSource) setLink(link leases.Link) {
	s.mu.Lock()
	s.link = link
	s.live = true
	s.mu.Unlock()
}

type previewADB struct {
	address  string
	started  chan struct{}
	release  chan struct{}
	listener net.Listener
	mu       sync.Mutex
	payloads [][]byte
	calls    int
	active   int
	peak     int
}

func newPreviewADB(t *testing.T, payload []byte) *previewADB {
	return newPreviewADBSequence(t, payload)
}

func newPreviewADBSequence(t *testing.T, payloads ...[]byte) *previewADB {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &previewADB{address: listener.Addr().String(), started: make(chan struct{}, 8), release: make(chan struct{}), listener: listener, payloads: payloads}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := readADBService(conn); err != nil {
					return
				}
				_, _ = io.WriteString(conn, "OKAY")
				service, err := readADBService(conn)
				if err != nil || service != "exec:screencap -p" {
					return
				}
				_, _ = io.WriteString(conn, "OKAY")
				fixture.mu.Lock()
				n := fixture.calls
				fixture.calls++
				fixture.active++
				fixture.peak = max(fixture.peak, fixture.active)
				payload := fixture.payloads[min(n, len(fixture.payloads)-1)]
				fixture.mu.Unlock()
				defer func() {
					fixture.mu.Lock()
					fixture.active--
					fixture.mu.Unlock()
				}()
				fixture.started <- struct{}{}
				<-fixture.release
				_, _ = conn.Write(payload)
			}()
		}
	}()
	return fixture
}

func (f *previewADB) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func readADBService(r io.Reader) (string, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(size[:]), 16, 16)
	if err != nil {
		return "", err
	}
	service := make([]byte, int(n))
	_, err = io.ReadFull(r, service)
	return string(service), err
}

func screenshotPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 16))
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func screenshotPNGColor(t *testing.T, value color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 16))
	for y := range 16 {
		for x := range 32 {
			img.SetRGBA(x, y, value)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestConcurrentWaiterCancellationDoesNotCancelSharedCapture(t *testing.T) {
	fixture := newPreviewADB(t, screenshotPNG(t))
	client, err := adb.NewClient(fixture.address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	linkCtx, cancelLink := context.WithCancel(context.Background())
	defer cancelLink()
	source := &previewTestSource{link: leases.Link{DeviceID: "device", LinkID: "link", TransportID: 9, Generation: 1}, ctx: linkCtx, live: true}
	manager := New(context.Background(), source, client)
	defer manager.Close()
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := manager.Get(waiterCtx, "device"); first <- err }()
	<-fixture.started
	second := make(chan error, 1)
	go func() { _, err := manager.Get(context.Background(), "device"); second <- err }()
	cancelWaiter()
	if err := <-first; err != context.Canceled {
		t.Fatalf("first waiter error = %v", err)
	}
	close(fixture.release)
	if err := <-second; err != nil {
		t.Fatalf("remaining waiter lost capture: %v", err)
	}
	if got := fixture.callCount(); got != 1 {
		t.Fatalf("coalesced capture count = %d, want 1", got)
	}
}

func TestCaptureRejectsChangedSourceGeneration(t *testing.T) {
	fixture := newPreviewADB(t, screenshotPNG(t))
	client, err := adb.NewClient(fixture.address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	linkCtx, cancelLink := context.WithCancel(context.Background())
	defer cancelLink()
	source := &previewTestSource{link: leases.Link{DeviceID: "device", LinkID: "link", TransportID: 9, Generation: 1}, ctx: linkCtx, live: true}
	manager := New(context.Background(), source, client)
	defer manager.Close()
	result := make(chan error, 1)
	go func() { _, err := manager.Get(context.Background(), "device"); result <- err }()
	<-fixture.started
	source.invalidate()
	close(fixture.release)
	if err := <-result; err != ErrLinkChanged {
		t.Fatalf("stale-generation result = %v, want %v", err, ErrLinkChanged)
	}
}
func TestCaptureFailureDoesNotMaskChangedSourceGeneration(t *testing.T) {
	linkCtx, cancelLink := context.WithCancel(context.Background())
	defer cancelLink()
	link := leases.Link{DeviceID: "device", LinkID: "link-a", TransportID: 9, Generation: 1}
	source := &previewTestSource{link: link, ctx: linkCtx, live: true}
	manager := New(context.Background(), source, nil)
	defer manager.Close()

	source.setLink(leases.Link{DeviceID: "device", LinkID: "link-b", TransportID: 10, Generation: 2})
	_, err := manager.validate(context.Background(), link, Snapshot{}, ErrInvalidImage)
	if err != ErrLinkChanged {
		t.Fatalf("stale-generation result after capture failure = %v, want %v", err, ErrLinkChanged)
	}
}
func TestRefreshesAcrossCyclesAndNeverFallsBackToOldImage(t *testing.T) {
	fixture := newPreviewADBSequence(t,
		screenshotPNGColor(t, color.RGBA{R: 255, A: 255}),
		screenshotPNGColor(t, color.RGBA{B: 255, A: 255}),
		[]byte("invalid refreshed image"),
	)
	close(fixture.release)
	client, err := adb.NewClient(fixture.address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	linkCtx, cancelLink := context.WithCancel(context.Background())
	defer cancelLink()
	link := leases.Link{DeviceID: "device", LinkID: "link-a", TransportID: 9, Generation: 1}
	source := &previewTestSource{link: link, ctx: linkCtx, live: true}
	manager := New(context.Background(), source, client)
	defer manager.Close()

	first, err := manager.Get(context.Background(), "device")
	if err != nil {
		t.Fatal(err)
	}
	linkB := leases.Link{DeviceID: "device", LinkID: "link-b", TransportID: 10, Generation: 1}
	source.setLink(linkB)
	second, err := manager.Get(context.Background(), "device")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.JPEG, second.JPEG) {
		t.Fatal("different selected link returned the prior link's image")
	}

	manager.mu.Lock()
	key := cacheKey{link: linkB}
	entry := manager.entries[key]
	entry.startedAt = time.Now().Add(-minInterval)
	manager.entries[key] = entry
	manager.lastStart[key] = time.Now().Add(-minInterval)
	manager.mu.Unlock()
	if _, err = manager.Get(context.Background(), "device"); err != ErrInvalidImage {
		t.Fatalf("failed refresh error = %v, want %v", err, ErrInvalidImage)
	}
	if fixture.callCount() != 3 {
		t.Fatalf("capture count = %d, want 3", fixture.callCount())
	}
}

func TestLinkCancellationInterruptsScreenshotSocketRead(t *testing.T) {
	fixture := newPreviewADB(t, screenshotPNG(t))
	defer close(fixture.release)
	client, err := adb.NewClient(fixture.address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	linkCtx, cancelLink := context.WithCancel(context.Background())
	source := &previewTestSource{link: leases.Link{DeviceID: "device", LinkID: "link", TransportID: 9, Generation: 1}, ctx: linkCtx, live: true}
	manager := New(context.Background(), source, client)
	defer manager.Close()
	result := make(chan error, 1)
	go func() { _, err := manager.Get(context.Background(), "device"); result <- err }()
	<-fixture.started
	cancelLink()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("link cancellation completed a screenshot successfully")
		}
	case <-time.After(time.Second):
		t.Fatal("link cancellation did not interrupt the socket read")
	}
}

func TestRetainedCaptureKeysAreBoundedAcrossFullLinkIdentity(t *testing.T) {
	manager := &Manager{entries: make(map[cacheKey]cacheEntry), lastStart: make(map[cacheKey]time.Time)}
	for i := range maxEntries {
		link := leases.Link{DeviceID: "device", LinkID: string(rune(i + 1)), TransportID: uint64(i + 1), Generation: uint64(i + 1)}
		manager.lastStart[cacheKey{link: link}] = time.Now()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link := leases.Link{DeviceID: "device", LinkID: "new-link", TransportID: 999, Generation: 999}
	source := &previewTestSource{link: link, ctx: ctx, live: true}
	manager.source = source
	if _, err := manager.Get(context.Background(), "device"); err != ErrCapacity {
		t.Fatalf("new key at the retained-state bound returned %v, want %v", err, ErrCapacity)
	}
}
