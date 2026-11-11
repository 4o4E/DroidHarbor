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

type parallelPreviewSource struct {
	links map[string]leases.Link
}

func (s parallelPreviewSource) ReadLink(_ context.Context, id string) (leases.Link, context.Context, error) {
	return s.links[id], context.Background(), nil
}

func (s parallelPreviewSource) HasLink(link leases.Link) bool {
	return s.links[link.DeviceID] == link
}

func TestDifferentDevicesCaptureWithoutGlobalQueue(t *testing.T) {
	for _, mode := range []string{"complete", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPreviewADB(t, screenshotPNG(t))
			releaseAll := sync.OnceFunc(func() { close(fixture.release) })
			client, err := adb.NewClient(fixture.address, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			source := parallelPreviewSource{links: make(map[string]leases.Link)}
			for i, id := range []string{"a", "b", "c"} {
				source.links[id] = leases.Link{DeviceID: id, LinkID: id, TransportID: uint64(i + 1), Generation: 1}
			}
			manager := New(context.Background(), source, client)
			defer manager.Close()
			defer releaseAll()
			results := make(chan error, 3)
			for id := range source.links {
				go func() {
					snapshot, err := manager.Get(context.Background(), id)
					if err == nil && snapshot.DeviceID != id {
						err = fmt.Errorf("preview for %s contains device %s", id, snapshot.DeviceID)
					}
					results <- err
				}()
			}
			// 三个设备均须在任何一张截图完成前启动，捕获全局并发门槛或排队回归。
			for range 3 {
				select {
				case <-fixture.started:
				case err := <-results:
					t.Fatalf("capture ended before all devices started: %v", err)
				case <-time.After(2 * time.Second):
					t.Fatal("a device waited for another device's capture capacity")
				}
			}
			fixture.mu.Lock()
			peak := fixture.peak
			fixture.mu.Unlock()
			if peak != 3 {
				t.Fatalf("parallel captures = %d, want 3 before release", peak)
			}
			if mode == "shutdown" {
				if err := manager.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				releaseAll()
			}
			for range 3 {
				select {
				case err := <-results:
					if mode == "complete" && err != nil {
						t.Fatal(err)
					}
					if mode == "shutdown" && err == nil {
						t.Fatal("shutdown returned a successful capture")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("capture did not finish")
				}
			}
		})
	}
}
