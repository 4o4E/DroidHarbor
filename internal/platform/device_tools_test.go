package platform

import (
	"slices"
	"testing"
)

func TestDeviceDirectoryPreservesNamesAndEntryTypes(t *testing.T) {
	directory, ok := decodeDeviceDirectory("/", []byte("0\x000\x003\x00./report\n2026.txt\x00./sdcard\x00./data\x0081a4 22 1730000000\na1ff 9 1730000001\n41ed 12 1730000002\x00"))
	if !ok || !directory.RootAvailable || directory.Writable {
		t.Fatalf("root listing = %+v, valid = %t", directory, ok)
	}
	want := []deviceFile{
		{Name: "data", Kind: "directory", Size: 12, ModifiedAt: 1730000002},
		{Name: "report\n2026.txt", Kind: "file", Size: 22, ModifiedAt: 1730000000},
		{Name: "sdcard", Kind: "link", Size: 9, ModifiedAt: 1730000001},
	}
	if !slices.Equal(directory.Entries, want) {
		t.Fatalf("entries = %+v, want %+v", directory.Entries, want)
	}
	if _, ok := decodeDeviceDirectory("/sdcard", []byte("2000\x001\x002\x00./one\x00./two\x0081a4 3 1730000000\x00")); ok {
		t.Fatal("accepted a partial device directory listing")
	}
}

func TestDeviceAppsRequiresConsistentUserPackageLists(t *testing.T) {
	apps, ok := decodeDeviceApps([]byte("10\npackage:org.example.client\npackage:android\n\npackage:org.example.client\n"), false)
	if !ok || apps.UserID != 10 {
		t.Fatalf("package list = %+v, valid = %t", apps, ok)
	}
	want := []deviceApp{{PackageName: "android"}, {PackageName: "org.example.client", ThirdParty: true}}
	if !slices.Equal(apps.Apps, want) {
		t.Fatalf("apps = %+v, want %+v", apps.Apps, want)
	}
	if _, ok := decodeDeviceApps([]byte("10\npackage:android\n\npackage:org.example.client\n"), false); ok {
		t.Fatal("accepted a third-party package missing from the current user's package list")
	}
}

func TestVersionedInventoryKeepsTheFullVersionAndRejectsPartialData(t *testing.T) {
	const header = "10\n29\nzh-CN\nzh-CN,en-US\n"
	const packages = "package:org.example.client versionCode:4294967298\npackage:android versionCode:29\n\npackage:org.example.client\n"
	apps, ok := decodeDeviceApps([]byte(header+packages), true)
	if !ok || apps.UserID != 10 || apps.locale != "zh-CN\nzh-CN,en-US" ||
		len(apps.Apps) != 2 || apps.Apps[1].version != 4294967298 || !apps.Apps[1].ThirdParty {
		t.Fatalf("versioned package list = %+v, valid = %t", apps, ok)
	}
	if _, ok := decodeDeviceApps([]byte(header+"package:android\n\n"), true); ok {
		t.Fatal("accepted inventory without the required version")
	}
	legacy, ok := decodeDeviceApps([]byte("10\n28\n\nnull\npackage:android\n\n"), true)
	if !ok || legacy.sdk != 28 || len(legacy.Apps) != 1 || legacy.Apps[0].PackageName != "android" {
		t.Fatalf("legacy package list = %+v, valid = %t", legacy, ok)
	}
}
