package adb

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenRechecksAuthorizationBeforeDeviceService(t *testing.T) {
	var revoked atomic.Bool
	denied := errors.New("authorization revoked")
	observed := make(chan error, 1)
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:transport-id:42" {
			observed <- errors.New("unexpected transport selection")
			return
		}
		revoked.Store(true)
		if _, err := io.WriteString(conn, "OKAY"); err != nil {
			observed <- err
			return
		}
		var data [1]byte
		n, err := conn.Read(data[:])
		if n != 0 {
			observed <- errors.New("revoked device service was dispatched")
			return
		}
		if !errors.Is(err, io.EOF) {
			observed <- err
			return
		}
		observed <- nil
	})
	stream, err := client.Open(context.Background(), 42, "shell:must-not-run", func() error {
		if revoked.Load() {
			return denied
		}
		return nil
	})
	if stream != nil {
		_ = stream.Close()
		t.Fatal("revoked service returned a stream")
	}
	if !errors.Is(err, denied) {
		t.Fatalf("Open error = %v, want revoked authorization", err)
	}
	select {
	case err = <-observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rejected service did not close its transport")
	}
}

func TestGuardCloseInterruptsAndJoinsBlockedWrite(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	entered := make(chan struct{})
	guarded := GuardWrites(local, func() error { close(entered); return nil })
	defer guarded.Close()
	written := make(chan error, 1)
	go func() { _, err := guarded.Write([]byte("must not reach the peer")); written <- err }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- guarded.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close waited behind the blocked network write")
	}
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("unread write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close left the write running")
	}
	var data [1]byte
	if n, err := peer.Read(data[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("closed stream delivered payload: n=%d err=%v", n, err)
	}
	if n, err := guarded.Write(data[:]); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed guard accepted another write: n=%d err=%v", n, err)
	}
}
