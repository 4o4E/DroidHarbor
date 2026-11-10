package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolatePlatformEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "DROIDHARBOR_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestEnvPathsAreLocalToConfigAndEnvironmentOverridesFile(t *testing.T) {
	isolatePlatformEnvironment(t)
	dir := filepath.Join(t.TempDir(), "配置 folder")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".env")
	data := "\ufeffDROIDHARBOR_ADMIN_TOKEN=test-admin-token\r\nDROIDHARBOR_RTC_ADVERTISED_IP=127.0.0.1\r\nDROIDHARBOR_DATABASE=\"database folder/state.db\"\r\nDROIDHARBOR_ADB_SERVER=127.0.0.1:5037\r\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROIDHARBOR_ADB_SERVER", "127.0.0.1:15037")
	options, err := loadServeOptions(path)
	if err != nil {
		t.Fatal(err)
	}
	if options.database != filepath.Join(dir, "database folder/state.db") || options.adbAddress != "127.0.0.1:15037" || options.rtcAdvertisedIP != "127.0.0.1" {
		t.Fatalf("configuration ignored file base, BOM/CRLF or environment precedence: %+v", options)
	}
}

func TestEnvErrorsNameSettingWithoutExposingValues(t *testing.T) {
	for _, test := range []struct{ name, data, want string }{
		{"ipv4", "DROIDHARBOR_RTC_ADVERTISED_IP=::1\n", "IPv4"},
		{"duration", "DROIDHARBOR_RTC_ADVERTISED_IP=127.0.0.1\nDROIDHARBOR_ADB_TIMEOUT=secret-value-marker\n", "DROIDHARBOR_ADB_TIMEOUT"},
		{"syntax", "DROIDHARBOR_BROKEN='secret-value-marker", "格式无效"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolatePlatformEnvironment(t)
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadServeOptions(path)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "secret-value-marker") {
				t.Fatalf("missing or unsafe configuration error: %v", err)
			}
		})
	}
}

func TestGeneratedConfigurationKeepsTokenAndRejectsDamagedExistingFile(t *testing.T) {
	isolatePlatformEnvironment(t)
	path := filepath.Join(t.TempDir(), ".env")
	first, err := loadServeOptions(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROIDHARBOR_ADMIN_TOKEN", "unrelated-process-token")
	second, err := loadServeOptions(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.configCreated || second.configCreated || first.adminToken != second.adminToken {
		t.Fatal("initialization replaced the persistent login token")
	}
	damaged := []byte("DROIDHARBOR_ADMIN_TOKEN=\n")
	if err := os.WriteFile(path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServeOptions(path); err == nil {
		t.Fatal("existing configuration without a token was silently repaired")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(damaged) {
		t.Fatal("failed loading changed the existing configuration")
	}
}
