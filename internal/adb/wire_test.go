package adb

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func readyTransport(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	if got := readTestService(t, conn); got != "host:transport-id:42" {
		t.Errorf("transport service = %q", got)
	}
	_, _ = io.WriteString(conn, "OKAY")
	if got := readTestService(t, conn); got != want {
		t.Errorf("device service = %q, want %q", got, want)
	}
	_, _ = io.WriteString(conn, "OKAY")
}

func readSyncRequestTest(t *testing.T, conn net.Conn) (string, []byte) {
	t.Helper()
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatal(err)
	}
	n := binary.LittleEndian.Uint32(header[4:])
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatal(err)
	}
	return string(header[:4]), payload
}

func TestPushStreamsSyncV1AndWaitsForOkay(t *testing.T) {
	content := bytes.Repeat([]byte("x"), syncDataMax+3)
	var sent int64
	closed := make(chan error, 1)
	client := testClient(t, func(conn net.Conn) {
		readyTransport(t, conn, "sync:")
		id, payload := readSyncRequestTest(t, conn)
		if id != "SEND" || string(payload) != "/tmp/file,420" {
			t.Errorf("SEND = %q %q", id, payload)
		}
		for _, size := range []uint32{syncDataMax, 3} {
			id, payload = readSyncRequestTest(t, conn)
			if id != "DATA" || uint32(len(payload)) != size {
				t.Errorf("DATA = %q %d", id, len(payload))
			}
		}
		var done [8]byte
		if _, err := io.ReadFull(conn, done[:]); err != nil {
			t.Fatal(err)
		}
		if string(done[:4]) != "DONE" || binary.LittleEndian.Uint32(done[4:]) != 123 {
			t.Errorf("DONE = %q %d", done[:4], binary.LittleEndian.Uint32(done[4:]))
		}
		var status [8]byte
		copy(status[:4], "OKAY")
		_, _ = conn.Write(status[:])
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var next [1]byte
		_, err := conn.Read(next[:])
		closed <- err
	})
	err := client.Push(context.Background(), 42, "/tmp/file", 0644, time.Unix(123, 0), bytes.NewReader(content), func() error { return nil }, func(n int64) { sent = n })
	if err != nil {
		t.Fatal(err)
	}
	if sent != int64(len(content)) {
		t.Fatalf("progress = %d", sent)
	}
	if err := <-closed; err != io.EOF {
		t.Fatalf("completed upload left its connection open: %v", err)
	}
}

func TestPushHidesRemoteFailText(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		readyTransport(t, conn, "sync:")
		_, _ = readSyncRequestTest(t, conn)
		_, _ = readSyncRequestTest(t, conn)
		var done [8]byte
		_, _ = io.ReadFull(conn, done[:])
		var status [8]byte
		copy(status[:4], "FAIL")
		binary.LittleEndian.PutUint32(status[4:], 11)
		_, _ = conn.Write(status[:])
		_, _ = io.WriteString(conn, "private msg")
	})
	err := client.Push(context.Background(), 42, "/tmp/file", 0644, time.Unix(1, 0), strings.NewReader("x"), func() error { return nil }, nil)
	if err == nil || strings.Contains(err.Error(), "private msg") {
		t.Fatalf("error exposed remote text: %v", err)
	}
}

func TestReadShellPacketAndRunShellExit(t *testing.T) {
	wire := []byte{shellStdout, 3, 0, 0, 0, 'o', 'u', 't', shellStderr, 2, 0, 0, 0, 'e', 'r', shellExit, 1, 0, 0, 0, 7}
	packet, err := ReadShellPacket(bytes.NewReader(wire), make([]byte, 0, 8), 8)
	if err != nil || packet.ID != shellStdout || string(packet.Data) != "out" {
		t.Fatalf("packet=%#v err=%v", packet, err)
	}
	client := testClient(t, func(conn net.Conn) {
		readyTransport(t, conn, "shell,v2,raw:echo 'ok'")
		var closeStdin [5]byte
		if _, err := io.ReadFull(conn, closeStdin[:]); err != nil {
			t.Error(err)
		}
		if closeStdin[0] != shellCloseStdin {
			t.Errorf("stdin packet = %v", closeStdin)
		}
		_, _ = conn.Write(wire)
	})
	result, err := client.RunShell(context.Background(), 42, "echo 'ok'", 5, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "out" || string(result.Stderr) != "er" || result.ExitCode != 7 {
		t.Fatalf("result = %#v", result)
	}
}

func TestShellPacketRejectsInvalidExitAndMissingExit(t *testing.T) {
	bad := []byte{shellExit, 4, 0, 0, 0, 1, 2, 3, 4}
	if _, err := ReadShellPacket(bytes.NewReader(bad), nil, 8); err == nil {
		t.Fatal("accepted four-byte EXIT")
	}
	client := testClient(t, func(conn net.Conn) {
		readyTransport(t, conn, "shell,v2,raw:true")
		var discard [5]byte
		_, _ = io.ReadFull(conn, discard[:])
		_, _ = conn.Write([]byte{shellStdout, 3, 0, 0, 0, 'E', 'O', 'F'})
	})
	_, err := client.RunShell(context.Background(), 42, "true", 100, func() error { return nil })
	if err == nil {
		t.Fatalf("missing EXIT result err=%v", err)
	}
}

func TestShellQuoteSingleArgument(t *testing.T) {
	quoted, err := ShellQuote("a'b")
	if err != nil || quoted != "'a'\\''b'" {
		t.Fatalf("quoted=%q err=%v", quoted, err)
	}
	if _, err := ShellQuote("a\x00b"); err == nil {
		t.Fatal("accepted NUL")
	}
}

func TestPushRejectsOversizedSendNameBeforeOpening(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		t.Error("invalid upload opened a device connection")
	})
	authorized := false
	err := client.Push(context.Background(), 42, "/"+strings.Repeat("a", 1020), 0644, time.Unix(1, 0), strings.NewReader("x"), func() error {
		authorized = true
		return nil
	}, nil)
	if err == nil || authorized {
		t.Fatalf("oversized SEND name reached dispatch: error=%v authorized=%v", err, authorized)
	}
}
