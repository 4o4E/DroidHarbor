package previews

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
)

func TestOnlineInitialCoverSurvivesIdleAndCachedReadsNeverCapture(t *testing.T) {
	fixture := newPreviewADB(t, screenshotPNG(t))
	close(fixture.release)
	client, err := adb.NewClient(fixture.address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 9, Generation: 1}
	source := &previewTestSource{link: link, ctx: ctx, live: true}
	manager := New(ctx, source, client)
	defer manager.Close()
	if _, err := manager.Cached(ctx, "device"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("uncaptured initial cover = %v", err)
	}
	if fixture.callCount() != 0 {
		t.Fatal("cache-only read captured a screenshot")
	}
	first, err := manager.Get(ctx, "device")
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	entry := manager.entries[cacheKey{link: link}]
	entry.lastUsed = time.Now().Add(-2 * idleLifetime)
	manager.entries[cacheKey{link: link}] = entry
	manager.evictIdleLocked(time.Now())
	manager.mu.Unlock()
	cached, err := manager.Cached(ctx, "device")
	if err != nil || !cached.CapturedAt.Equal(first.CapturedAt) || !bytes.Equal(cached.JPEG, first.JPEG) {
		t.Fatalf("online initial cover was lost or relabeled: err=%v", err)
	}
	if fixture.callCount() != 1 {
		t.Fatal("idle cache read started another screenshot")
	}
	source.setLink(leases.Link{DeviceID: "device", LinkID: "link", TransportID: 10, Generation: 2})
	if _, err := manager.Cached(ctx, "device"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("old-generation cover leaked into new link: %v", err)
	}
	if fixture.callCount() != 1 {
		t.Fatal("new-generation cache miss started a screenshot")
	}
}
