package platform

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestTrashIndexCannotRestoreOutsideSharedStorage(t *testing.T) {
	id := strings.Repeat("a", 64)
	encode := func(path string) []byte {
		return []byte(id + "\x00" + base64.StdEncoding.EncodeToString([]byte(path)) + "\x00directory\x001795161151\x00")
	}
	original := "/sdcard/Download/folder\nwith space"
	entries, ok := decodeTrashEntries(encode(original))
	if !ok || len(entries) != 1 || entries[0].OriginalPath != original || entries[0].ID != id {
		t.Fatalf("listing lost original path identity: %+v (valid=%t)", entries, ok)
	}
	for _, invalid := range []string{"/sdcard", "/sdcard/../data", "/data/local/tmp/target", "/sdcard/.droidharbor-trash/item"} {
		if _, ok := decodeTrashEntries(encode(invalid)); ok {
			t.Errorf("accepted undeletable restore destination %q", invalid)
		}
	}
}
