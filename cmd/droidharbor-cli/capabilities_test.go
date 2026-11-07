package main

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func capabilityTestServer(t *testing.T, handler http.HandlerFunc) []string {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	directory := t.TempDir()
	caFile, err := os.OpenFile(filepath.Join(directory, "ca.pem"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = pem.Encode(caFile, &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}); err != nil {
		t.Fatal(err)
	}
	if err = caFile.Close(); err != nil {
		t.Fatal(err)
	}
	credentialFile := filepath.Join(directory, "caller.json")
	if err = os.WriteFile(credentialFile, []byte(`{"token":"test-private-bearer"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"--server", server.URL, "--credential-file", credentialFile, "--ca-file", caFile.Name()}
}

func TestCapabilitiesDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	var received atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		fmt.Fprintln(w, `{"apiMajor":1,"features":{"callerCredentials":true}}`)
	}))
	defer destination.Close()
	args := capabilityTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-private-bearer" {
			w.WriteHeader(401)
			return
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	_, failure := capabilities(args)
	if failure == nil || failure.Code != "HTTP_ERROR" || !strings.Contains(failure.Message, "307") {
		t.Fatal("redirect was not rejected as an HTTP failure")
	}
	if received.Load() != 0 {
		t.Fatal("CLI contacted the redirect destination and could expose the bearer")
	}
}

func TestCapabilitiesRejectsIncompatibleAPIMajor(t *testing.T) {
	args := capabilityTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"apiMajor":2,"features":{"callerCredentials":true},"optionalFutureField":true}`)
	})
	_, failure := capabilities(args)
	if failure == nil || failure.Code != "API_INCOMPATIBLE" || failure.exitCode != 5 {
		t.Fatal("incompatible API was accepted")
	}
}
