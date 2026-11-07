package adbgateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeKeyNegotiationRequiresFreshTaskSignature(t *testing.T) {
	taskKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	defaultKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, replay := range []bool{false, true} {
		name := "default_then_task"
		if replay {
			name = "replayed_task_signature_rejected"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server, client := net.Pipe()
			defer client.Close()
			if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var opened atomic.Bool
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, server, Session{
					PublicKey: &taskKey.PublicKey,
					Check:     func() error { return nil },
					Open: func(context.Context, string) (net.Conn, error) {
						opened.Store(true)
						return nil, errors.New("unexpected service")
					},
				})
			}()
			if err := writePacket(client, packet{cmd: cmdCnxn, arg0: protocolVersion, arg1: maxPacketSize, data: []byte("host::features=shell_v2;\x00")}, false); err != nil {
				t.Fatal(err)
			}
			first, err := readPacket(client, maxPacketSize, true)
			if err != nil || first.cmd != cmdAuth || first.arg0 != 1 {
				t.Fatal("missing first AUTH challenge", err)
			}
			signature, err := rsa.SignPKCS1v15(rand.Reader, defaultKey, crypto.SHA1, first.data)
			if err != nil {
				t.Fatal(err)
			}
			if err := writePacket(client, packet{cmd: cmdAuth, arg0: 2, data: signature}, true); err != nil {
				t.Fatal(err)
			}
			second, err := readPacket(client, maxPacketSize, true)
			if err != nil || second.cmd != cmdAuth || second.arg0 != 1 {
				t.Fatal("missing second AUTH challenge", err)
			}
			if bytes.Equal(first.data, second.data) {
				t.Fatal("AUTH challenge was reused")
			}
			digest := second.data
			if replay {
				digest = first.data
			}
			signature, err = rsa.SignPKCS1v15(rand.Reader, taskKey, crypto.SHA1, digest)
			if err != nil {
				t.Fatal(err)
			}
			if err := writePacket(client, packet{cmd: cmdAuth, arg0: 2, data: signature}, true); err != nil {
				t.Fatal(err)
			}
			response, err := readPacket(client, maxPacketSize, true)
			if replay {
				if err == nil {
					t.Fatal("stale task signature authenticated", response.cmd)
				}
			} else if err != nil || response.cmd != cmdCnxn {
				t.Fatal("task key did not authenticate after default key", err)
			}
			if opened.Load() {
				t.Fatal("authentication opened an upstream service")
			}
			cancel()
			_ = client.Close()
			select {
			case result := <-done:
				if replay && result == nil {
					t.Fatal("replayed signature did not fail")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("authentication did not terminate")
			}
		})
	}
}
