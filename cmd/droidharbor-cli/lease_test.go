package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func leaseTestInputs(t *testing.T, handler http.HandlerFunc) ([]string, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	root := t.TempDir()
	credential := filepath.Join(root, "caller.json")
	if err := os.WriteFile(credential, []byte(`{"token":"caller-secret-not-to-persist"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"--server", server.URL, "--credential-file", credential}, root
}

func TestClaimPersistsStateBeforeUnknownResponseAndReusesIt(t *testing.T) {
	var mu sync.Mutex
	var posts int
	var firstKey string
	var firstBody []byte
	args, root := leaseTestInputs(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/capabilities" {
			fmt.Fprintln(w, `{"apiMajor":1,"features":{"leaseClaim":true}}`)
			return
		}
		if r.URL.Path == "/api/v1/leases" {
			mu.Lock()
			defer mu.Unlock()
			posts++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read claim: %v", err)
			}
			if r.Header.Get("Authorization") != "Bearer caller-secret-not-to-persist" {
				t.Error("claim did not use caller credential")
			}
			if posts == 1 {
				firstKey = r.Header.Get("Idempotency-Key")
				firstBody = append([]byte(nil), body...)
				h, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = h.Close()
				}
				return
			}
			if r.Header.Get("Idempotency-Key") != firstKey || !bytes.Equal(body, firstBody) {
				t.Error("explicit recovery changed claim request identity")
			}
			fmt.Fprintln(w, `{"leaseId":"lease-original","deviceId":"device-1","generation":1,"bootId":"boot-1","status":"active","claimedAt":"2026-01-01T00:00:00Z","expiresAt":"2026-01-01T00:15:00Z","allocationState":"leased"}`)
			return
		}
		if r.URL.Path == "/api/v1/leases/lease-original" {
			fmt.Fprintln(w, `{"leaseId":"lease-original","deviceId":"device-1","generation":1,"bootId":"boot-1","status":"active","claimedAt":"2026-01-01T00:00:00Z","expiresAt":"2026-01-01T00:15:00Z","allocationState":"leased"}`)
			return
		}
		http.NotFound(w, r)
	})
	taskDir := filepath.Join(root, "task")
	args = append(args, "--task-dir", taskDir, "--tag", " blue ", "--tag", "blue", "--task-id", "task-fixed")
	_, failure := claim(args)
	if failure == nil || failure.Code != "RESULT_UNKNOWN" {
		t.Fatalf("first claim failure=%+v", failure)
	}
	state, failure := loadTaskState(taskDir)
	if failure != nil {
		t.Fatal(failure)
	}
	if len(state.TaskSecret) != 64 || len(state.IdempotencyKey) != 64 || state.LeaseID != "" || len(state.RequiredTags) != 1 || state.RequiredTags[0] != "blue" {
		t.Fatal("persisted claim identity or normalized tags are incomplete")
	}
	contents, err := os.ReadFile(filepath.Join(taskDir, taskStateName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "caller-secret-not-to-persist") {
		t.Fatal("caller credential persisted in task state")
	}
	_, failure = claim(args)
	if failure != nil {
		t.Fatal(failure)
	}
	updated, failure := loadTaskState(taskDir)
	if failure != nil {
		t.Fatal(failure)
	}
	mu.Lock()
	claimPosts := posts
	mu.Unlock()
	if updated.LeaseID != "lease-original" || updated.TaskSecret != state.TaskSecret || updated.IdempotencyKey != state.IdempotencyKey || claimPosts != 2 {
		t.Fatalf("retry did not reuse original identity: leaseId=%q posts=%d", updated.LeaseID, claimPosts)
	}
	if _, failure = status([]string{"--task-dir", taskDir}); failure != nil {
		t.Fatal(failure)
	}
}

func TestClaimRejectsConflictingTaskDirectory(t *testing.T) {
	args, root := leaseTestInputs(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("conflicting directory must not contact server")
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})
	taskDir := filepath.Join(root, "existing")
	if err := os.Mkdir(taskDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	args = append(args, "--task-dir", taskDir)
	_, failure := claim(args)
	if failure == nil || failure.Code != "TASK_DIRECTORY_CONFLICT" {
		t.Fatalf("expected explicit directory conflict, got %+v", failure)
	}
	if _, err := os.Stat(filepath.Join(taskDir, "unrelated")); err != nil {
		t.Fatal("existing directory data was changed")
	}
}

func TestRenewRejectsUndeclaredSuccessResponseAsUnknown(t *testing.T) {
	server, root := leaseTestInputs(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/capabilities" {
			fmt.Fprintln(w, `{"apiMajor":1,"features":{"leaseClaim":true}}`)
			return
		}
		if r.URL.Path == "/api/v1/leases/lease-fixed/renew" {
			if r.Method != http.MethodPost {
				t.Errorf("renew method=%s", r.Method)
			}
			w.Header().Set("X-Request-ID", "renew-request-id")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})
	taskDir := filepath.Join(root, "task")
	if err := os.Mkdir(taskDir, 0700); err != nil {
		t.Fatal(err)
	}
	state := &taskState{
		Server:         server[1],
		TaskID:         "task-fixed",
		TaskSecret:     strings.Repeat("a", 64),
		IdempotencyKey: strings.Repeat("b", 64),
		RequiredTags:   []string{},
		LeaseID:        "lease-fixed",
	}
	if failure := saveTaskState(taskDir, state); failure != nil {
		t.Fatal(failure)
	}
	_, failure := renew([]string{"--task-dir", taskDir})
	if failure == nil || failure.Code != "RESULT_UNKNOWN" || failure.RequestID != "renew-request-id" {
		t.Fatalf("invalid 204 renew response was not reported as unknown: %+v", failure)
	}
}
