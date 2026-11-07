package previews

import (
	"testing"
	"time"

	"droidharbor/internal/leases"
)

func TestCacheRoomEvictsLeastRecentlyUsedEntryAtEntryBound(t *testing.T) {
	manager := &Manager{entries: make(map[cacheKey]cacheEntry, maxEntries)}
	base := time.Now()
	for i := range maxEntries {
		key := cacheKey{link: leases.Link{DeviceID: string(rune(i + 1)), LinkID: "link", Generation: 1}}
		manager.entries[key] = cacheEntry{result: result{snapshot: Snapshot{JPEG: []byte{byte(i)}}}, lastUsed: base.Add(time.Duration(i) * time.Second)}
		manager.cacheSize++
	}
	if !manager.makeRoomLocked(1) {
		t.Fatal("cache could not make room at the entry bound")
	}
	if len(manager.entries) != maxEntries-1 || manager.cacheSize != maxEntries-1 {
		t.Fatalf("unexpected eviction state: entries=%d bytes=%d", len(manager.entries), manager.cacheSize)
	}
	for key := range manager.entries {
		if key.link.DeviceID == string(rune(1)) {
			t.Fatal("least-recently-used entry was retained")
		}
	}
}
