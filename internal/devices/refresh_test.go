package devices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/leases"
)

func TestManualRefreshPreservesNewerInventoryAndTrackerLifetime(t *testing.T) {
	for _, test := range []struct {
		name     string
		endEpoch bool
		code     string
	}{
		{name: "new transport wins", code: "DEVICE_STATE_CHANGED"},
		{name: "ended tracker stays ended", endEpoch: true, code: "ADB_UNAVAILABLE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			queried, respond, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(finished)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Error(err)
					return
				}
				var prefix [4]byte
				if _, err := io.ReadFull(conn, prefix[:]); err != nil {
					t.Error(err)
					return
				}
				n, err := strconv.ParseUint(string(prefix[:]), 16, 16)
				if err != nil {
					t.Error(err)
					return
				}
				service := make([]byte, int(n))
				if _, err := io.ReadFull(conn, service); err != nil {
					t.Error(err)
					return
				}
				if string(service) != "host:devices-l" {
					t.Error("manual refresh performed an unexpected ADB action")
					return
				}
				close(queried)
				select {
				case <-respond:
				case <-ctx.Done():
					return
				}
				payload := "phone\tdevice transport_id:7\n"
				if _, err := fmt.Fprintf(conn, "OKAY%04x%s", len(payload), payload); err != nil {
					t.Error(err)
				}
			}()
			t.Cleanup(func() { cancel(); listener.Close(); <-finished })
			client, err := adb.NewClient(listener.Addr().String(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			registry := New(ctx, nil, client, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
			defer registry.Close()
			epoch, endEpoch := context.WithCancel(ctx)
			defer endEpoch()
			registry.publish([]adb.Device{{Serial: "phone", State: "device", TransportID: 1}}, epoch)
			result := make(chan error, 1)
			go func() { result <- registry.refreshInventory(ctx) }()
			select {
			case <-queried:
			case err := <-result:
				t.Fatalf("query ended before the guarded interval: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var current context.Context
			if test.endEpoch {
				endEpoch()
			} else {
				registry.publish([]adb.Device{{Serial: "phone", State: "device", TransportID: 2}}, epoch)
				link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 2, Generation: 2}
				liveCtx, stop := context.WithCancel(epoch)
				defer stop()
				registry.mu.Lock()
				registry.live[link.LinkID] = &liveLink{link: link, serial: "phone", ctx: liveCtx, cancel: stop}
				registry.mu.Unlock()
				current = liveCtx
			}
			close(respond)
			var failure *Error
			select {
			case err := <-result:
				if !errors.As(err, &failure) || failure.Code != test.code {
					t.Fatalf("guard returned %v, want %s", err, test.code)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			registry.mu.RLock()
			transport := registry.discovered["phone"].TransportID
			registry.mu.RUnlock()
			if test.endEpoch {
				if transport != 1 || registry.epoch.Err() == nil {
					t.Fatal("one-shot query revived an ended tracker")
				}
			} else if transport != 2 || current.Err() != nil {
				t.Fatal("stale query replaced the current transport or canceled its authorization")
			}
		})
	}
}
