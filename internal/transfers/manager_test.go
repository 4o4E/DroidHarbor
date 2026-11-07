package transfers

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestValidateMetadataAppliesFileTargetAndRejectsUnsafeNames(t *testing.T) {
	req := CreateRequest{FileName: "report.txt", Purpose: "file", Size: 12}
	if err := validateMetadata(&req); err != nil {
		t.Fatal(err)
	}
	if req.TargetPath != "/sdcard/Download/report.txt" {
		t.Fatalf("default target = %q", req.TargetPath)
	}
	for _, name := range []string{"", ".", "..", "../report.txt", `folder\\file.txt`, "bad\x00name"} {
		t.Run(name, func(t *testing.T) {
			bad := CreateRequest{FileName: name, Purpose: "file"}
			if err := validateMetadata(&bad); err == nil {
				t.Fatal("expected invalid file name")
			}
		})
	}
}

func TestValidateMetadataRequiresAPKAndForbidsTargetPath(t *testing.T) {
	valid := CreateRequest{FileName: "app.apk", Purpose: "apk", Size: 1, Overwrite: true}
	if err := validateMetadata(&valid); err != nil {
		t.Fatal(err)
	}
	if valid.Overwrite {
		t.Fatal("APK overwrite flag must not change package installation semantics")
	}
	for _, bad := range []CreateRequest{
		{FileName: "app.apk", Purpose: "apk"},
		{FileName: "app.bin", Purpose: "apk", Size: 1},
		{FileName: "app.apk", Purpose: "apk", Size: 1, TargetPath: "/sdcard/app.apk"},
	} {
		if err := validateMetadata(&bad); err == nil {
			t.Fatalf("expected invalid APK request: %+v", bad)
		}
	}
}

func TestValidateJournalPathRequiresExactGeneratedPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "staging")
	id := "transfer-id"
	valid := filepath.Join(dir, tempPrefix+id+tempSuffix)
	encodedData, _ := json.Marshal(resourceLocator{Path: valid})
	got, err := validateJournalPath(dir, id, string(encodedData))
	if err != nil || got != valid {
		t.Fatalf("validateJournalPath() = %q, %v", got, err)
	}
	for _, bad := range []struct{ id, path string }{
		{"../outside", filepath.Join(dir, tempPrefix+"../outside"+tempSuffix)},
		{id, filepath.Join(dir, "..", "outside")},
	} {
		encodedData, _ := json.Marshal(resourceLocator{Path: bad.path})
		if _, err := validateJournalPath(dir, bad.id, string(encodedData)); err == nil {
			t.Fatalf("expected invalid recovery locator: %+v", bad)
		}
	}
}

func TestLimitsSnapshotUsesResolvedConfiguration(t *testing.T) {
	manager := &Manager{config: Config{MaxUploadBytes: 123, MaxActive: 4, MaxCommandOutput: 456}}
	if got, want := manager.Limits(), (Limits{MaxUploadBytes: 123, MaxActive: 4, MaxCommandOutput: 456}); got != want {
		t.Fatalf("Limits() = %+v, want %+v", got, want)
	}
}
func TestValidateConfigRequiresExplicitLimitsAndBootOnlyForNew(t *testing.T) {
	config := Config{
		TempDir:          t.TempDir(),
		MaxUploadBytes:   DefaultMaxUploadBytes,
		MaxActive:        DefaultMaxActive,
		MaxCommandOutput: DefaultMaxCommandOutput,
	}
	if err := validateConfig(config, true); err == nil {
		t.Fatal("New config must require a boot ID")
	}
	config.BootID = "boot-1"
	if err := validateConfig(config, true); err != nil {
		t.Fatal(err)
	}
	config.BootID = ""
	if err := validateConfig(config, false); err != nil {
		t.Fatalf("recovery must not require the new boot ID: %v", err)
	}
	config.MaxActive = 0
	if err := validateConfig(config, false); err == nil {
		t.Fatal("zero resource limits must not silently default")
	}
}
