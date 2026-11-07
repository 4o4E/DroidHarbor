package adbgateway

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"testing"
	"time"
)

func TestClosedStreamPacketsDoNotDisconnectOrReachReusedPeerID(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, client := net.Pipe()
	first, firstPeer := net.Pipe()
	next, nextPeer := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, server, Session{PublicKey: &key.PublicKey, Check: func() error { return nil }, Open: func(_ context.Context, service string) (net.Conn, error) {
			if service == "exec:first" {
				return first, nil
			}
			return next, nil
		}})
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = firstPeer.Close()
		_ = nextPeer.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("transport workers survived cancellation")
		}
	})
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := nextPeer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	send := func(p packet) {
		t.Helper()
		if err := writePacket(client, p, true); err != nil {
			t.Fatal(err)
		}
	}
	receive := func(command uint32) packet {
		t.Helper()
		p, err := readPacket(client, maxPacketSize, true)
		if err != nil || p.cmd != command {
			t.Fatalf("packet command=%x want=%x error=%v", p.cmd, command, err)
		}
		return p
	}
	if err := writePacket(client, packet{cmd: cmdCnxn, arg0: protocolVersion, arg1: 4096, data: []byte("host::features=;\x00")}, false); err != nil {
		t.Fatal(err)
	}
	challenge := receive(cmdAuth)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, challenge.data)
	if err != nil {
		t.Fatal(err)
	}
	send(packet{cmd: cmdAuth, arg0: 2, data: signature})
	receive(cmdCnxn)
	send(packet{cmd: cmdOpen, arg0: 7, data: []byte("exec:first\x00")})
	old := receive(cmdOkay)
	if err := firstPeer.Close(); err != nil {
		t.Fatal(err)
	}
	closed := receive(cmdClse)
	if closed.arg0 != old.arg0 || closed.arg1 != 7 {
		t.Fatal("wrong stream closed")
	}
	late := func() {
		send(packet{cmd: cmdOkay, arg0: 7, arg1: old.arg0})
		send(packet{cmd: cmdWrte, arg0: 7, arg1: old.arg0, data: []byte("stale")})
	}
	late()
	send(packet{cmd: cmdOpen, arg0: 7, data: []byte("exec:next\x00")})
	current := receive(cmdOkay)
	if current.arg0 == old.arg0 || current.arg1 != 7 {
		t.Fatal("stream incarnation was reused")
	}
	late()
	send(packet{cmd: cmdWrte, arg0: 7, arg1: current.arg0, data: []byte("current")})
	actual := make([]byte, len("current"))
	if _, err := io.ReadFull(nextPeer, actual); err != nil {
		t.Fatal(err)
	}
	if string(actual) != "current" {
		t.Fatalf("stale data reached the new stream: %q", actual)
	}
	ack := receive(cmdOkay)
	if ack.arg0 != current.arg0 || ack.arg1 != 7 {
		t.Fatal("current stream did not remain usable")
	}
	if err := nextPeer.Close(); err != nil {
		t.Fatal(err)
	}
	receive(cmdClse)
}
