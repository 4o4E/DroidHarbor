package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppPresentationCacheSurvivesRestartAndInvalidatesOnlyChangedPackages(t *testing.T) {
	directory := t.TempDir()
	cache, err := OpenCache(directory)
	if err != nil {
		t.Fatal(err)
	}
	file, err := cache.appFile("device-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	a := cachedAppPresentation{Version: 1, Locale: "zh-CN", Name: "第一款", Icon: "icon-a"}
	b := cachedAppPresentation{Version: 8, Locale: "zh-CN", Name: "第二款", Icon: "icon-b"}
	if err := cache.mergeApps(file, map[string]cachedAppPresentation{"example.first": a, "example.second": b}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(-2, 0, 0)
	if err := os.Chtimes(filepath.Join(directory, file), old, old); err != nil {
		t.Fatal(err)
	}
	cache, err = OpenCache(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	entries, err := cache.readApps(file)
	if err != nil {
		t.Fatal(err)
	}
	apps := []deviceApp{{PackageName: "example.first", version: 2}, {PackageName: "example.second", version: 8, ThirdParty: true}}
	missing := fillCachedAppPresentations(apps, "zh-CN", entries)
	if len(missing) != 1 || missing[0].PackageName != "example.first" || apps[0].Icon != "" || apps[1].Icon != b.Icon || !apps[1].ThirdParty {
		t.Fatalf("application update did not isolate the stale icon: apps=%+v, missing=%+v", apps, missing)
	}
	a.Version, a.Icon = 2, "icon-a-v2"
	if err := cache.mergeApps(file, map[string]cachedAppPresentation{"example.first": a}); err != nil {
		t.Fatal(err)
	}
	entries, err = cache.readApps(file)
	if err != nil || entries["example.first"] != a || entries["example.second"] != b {
		t.Fatalf("package update lost a valid cached icon: %v, %v", entries, err)
	}
	for _, owner := range []struct {
		device string
		user   int
	}{{"device-b", 10}, {"device-a", 0}} {
		other, err := cache.appFile(owner.device, owner.user)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := cache.readApps(other)
		if err != nil || len(entries) != 0 {
			t.Fatalf("cache leaked across device or Android user for %+v: %v, %v", owner, entries, err)
		}
	}
}

func TestFormatChangeRemovesOnlyDroidHarborCacheDirectories(t *testing.T) {
	directory := t.TempDir()
	old, err := openCache(directory, 1)
	if err != nil {
		t.Fatal(err)
	}
	file, err := old.appFile("device-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.mergeApps(file, map[string]cachedAppPresentation{"example.first": {Version: 1, Icon: "icon"}}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"v99", "user-data"} {
		dir := filepath.Join(directory, keep)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("owned by user"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	current, err := openCache(directory, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	if _, err := os.Stat(filepath.Join(directory, file)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old cached metadata survived a format change: %v", err)
	}
	for _, keep := range []string{"v99", "user-data"} {
		data, err := os.ReadFile(filepath.Join(directory, keep, "keep.txt"))
		if err != nil || string(data) != "owned by user" {
			t.Fatalf("format change touched unrelated %v: %v", keep, err)
		}
	}
}
