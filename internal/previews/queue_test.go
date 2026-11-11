package previews

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
)

type previewQueueSource struct {
	links    map[string]leases.Link
	contexts map[string]context.Context
	reads    chan string
}

func (s *previewQueueSource) ReadLink(_ context.Context, id string) (leases.Link, context.Context, error) {
	select {
	case s.reads <- id:
	default:
	}
	return s.links[id], s.contexts[id], nil
}

func (s *previewQueueSource) HasLink(link leases.Link) bool {
	return s.links[link.DeviceID] == link && s.contexts[link.DeviceID].Err() == nil
}

func queuedPreviewSource() (*previewQueueSource, context.CancelFunc) {
	third, cancelThird := context.WithCancel(context.Background())
	source := &previewQueueSource{links: make(map[string]leases.Link), contexts: make(map[string]context.Context), reads: make(chan string, 64)}
	for i, id := range []string{"a", "b", "c"} {
		source.links[id] = leases.Link{DeviceID: id, LinkID: id, TransportID: uint64(i + 1), Generation: 1}
		source.contexts[id] = context.Background()
	}
	source.contexts["c"] = third
	return source, cancelThird
}

func TestDifferentDevicesWaitForPreviewCapacity(t *testing.T) {
	fixture := newPreviewADB(t, screenshotPNG(t))
	releaseAll := sync.OnceFunc(func() { close(fixture.release) })
	client, err := adb.NewClient(fixture.address, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	source, cancelThird := queuedPreviewSource()
	defer cancelThird()
	manager := New(context.Background(), source, client)
	defer manager.Close()
	defer releaseAll()
	results := make(chan error, 3)
	request := func(id string) {
		snapshot, err := manager.Get(context.Background(), id)
		if err == nil && snapshot.DeviceID != id {
			err = fmt.Errorf("preview for %s contains device %s", id, snapshot.DeviceID)
		}
		results <- err
	}
	for _, id := range []string{"a", "b"} {
		go request(id)
	}
	for range 2 {
		select {
		case <-fixture.started:
		case err := <-results:
			t.Fatalf("a preview finished before either occupied slot was released: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("initial captures did not start")
		}
	}
	go request("c")
	// 排队超过五秒后，首次取得空位的设备仍应保留完整的新鲜度窗口。
	select {
	case err := <-results:
		t.Fatalf("queued preview returned before capacity was released: %v", err)
	case <-time.After(minInterval + 20*time.Millisecond):
	}
	fixture.release <- struct{}{}
	completed := 0
	waiting := true
	for waiting {
		select {
		case <-fixture.started:
			waiting = false
		case err := <-results:
			if err != nil {
				t.Fatalf("queued device failed instead of waiting: %v", err)
			}
			completed++
		case <-time.After(2 * time.Second):
			t.Fatal("queued device did not start when capacity became available")
		}
	}
	releaseAll()
	for completed < 3 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
			completed++
		case <-time.After(2 * time.Second):
			t.Fatal("preview did not complete")
		}
	}
	if _, err := manager.Get(context.Background(), "c"); err != nil {
		t.Fatalf("freshly captured queued preview could not be reused: %v", err)
	}
	fixture.mu.Lock()
	calls, peak := fixture.calls, fixture.peak
	fixture.mu.Unlock()
	if calls != 3 || peak != 2 {
		t.Fatalf("captures = %d, peak concurrent = %d; want three captures with at most two active", calls, peak)
	}
}

func TestQueuedPreviewCancellationDoesNotStartDeviceCapture(t *testing.T) {
	for _, mode := range []string{"link", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPreviewADB(t, screenshotPNG(t))
			client, err := adb.NewClient(fixture.address, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			source, cancelThird := queuedPreviewSource()
			defer cancelThird()
			manager := New(context.Background(), source, client)
			defer manager.Close()
			defer close(fixture.release)
			for _, id := range []string{"a", "b"} {
				go func() { _, _ = manager.Get(context.Background(), id) }()
				<-fixture.started
			}
			result := make(chan error, 1)
			go func() { _, err := manager.Get(context.Background(), "c"); result <- err }()
			for id := range source.reads {
				if id == "c" {
					break
				}
			}
			if mode == "link" {
				cancelThird()
			} else if err := manager.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err == nil || (mode == "link" && err != ErrLinkChanged) {
					t.Fatalf("queued cancellation returned %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("queued capture ignored cancellation")
			}
			if calls := fixture.callCount(); calls != 2 {
				t.Fatalf("canceled queued device started capture: calls = %d", calls)
			}
		})
	}
}
