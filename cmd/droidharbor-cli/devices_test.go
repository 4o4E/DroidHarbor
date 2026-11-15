package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestDevicesReturnsMinimalCallerInventory(t *testing.T) {
	args := capabilityTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/devices" || r.Header.Get("Authorization") != "Bearer test-private-bearer" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintln(w, `{"discoveryReady":true,"devices":[{"deviceId":"full-device-id","displayName":"9R","tags":["shopping"],"connectionState":"online","allocationState":"available","links":[{"endpoint":"private-endpoint"}],"taskId":"private-task"}]}`)
	})
	result, failure := listDevices(args)
	if failure != nil || !result.DiscoveryReady || len(result.Devices) != 1 || result.Devices[0].DisplayName != "9R" {
		t.Fatalf("invalid inventory: %+v %+v", result, failure)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private-endpoint") || strings.Contains(string(encoded), "private-task") {
		t.Fatalf("management data escaped caller summary: %s %v", encoded, err)
	}
}

func TestClaimRejectsEmptyOrConflictingSelectors(t *testing.T) {
	base := []string{"--server", "http://localhost:8080", "--credential-file", "caller.json", "--task-dir", "task"}
	for _, selector := range [][]string{{"--device-name", ""}, {"--device-name", " "}, {"--device-id", ""}, {"--device-id", "a", "--device-name", "9R"}} {
		args := append(append([]string(nil), base...), selector...)
		if _, failure := parseLeaseFlags("claim", args, true); failure == nil || failure.Code != "INVALID_ARGUMENT" {
			t.Fatalf("unsafe selector accepted: %q %+v", selector, failure)
		}
	}
}
