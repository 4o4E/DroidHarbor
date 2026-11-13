package devices

import (
	"context"
	"encoding/json"
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

func TestManualRefreshReconnectsSavedPhoneWithoutRebindingOrAllocating(t *testing.T) {
	for _, wrongPhone := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrongPhone=%t", wrongPhone), func(t *testing.T) {
			const endpoint = "192.0.2.8:43217"
			connected := false
			registry := wirelessRegistry(t, func(request string) string {
				switch request {
				case "host:devices-l":
					if connected {
						return endpoint + " device product:product model:Phone transport_id:8\n"
					}
					return ""
				case "host:connect:" + endpoint:
					connected = true
					return "connected to " + endpoint
				default:
					t.Errorf("unexpected ADB request %q", request)
					return ""
				}
			})
			ctx := context.Background()
			registered, err := registry.Register(ctx, "admin", Registration{DisplayName: "saved phone", LinkRequest: LinkRequest{Kind: "wifi", Endpoint: endpoint}})
			if err != nil {
				t.Fatal(err)
			}
			evidence := Evidence{Serial: "hardware-one", Manufacturer: "Maker", Model: "Phone", Product: "product", Android: "15", SDK: "35"}
			if wrongPhone {
				evidence.Serial = "original-phone"
			}
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = registry.db.Exec(`UPDATE device_links SET identity_evidence=? WHERE id=?`, string(encoded), registered.LinkID); err != nil {
				t.Fatal(err)
			}
			err = registry.Refresh(ctx, "admin", registered.DeviceID)
			if wrongPhone {
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != "IDENTITY_CONFLICT" {
					t.Fatalf("saved address rebound to a different phone: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			views, _, err := registry.List(ctx, true)
			if err != nil {
				t.Fatal(err)
			}
			view := views[0]
			if !*view.MaintenanceRequested || view.AllocationState != "maintenance" {
				t.Fatalf("refresh enabled AI allocation: %+v", view)
			}
			if !wrongPhone && !view.Links[0].IdentityVerified {
				t.Fatal("reconnected phone was not identity-verified")
			}
			var saved string
			if err = registry.db.QueryRow(`SELECT identity_evidence FROM device_links WHERE id=?`, registered.LinkID).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			var retained Evidence
			if err = json.Unmarshal([]byte(saved), &retained); err != nil || !retained.SameHardware(evidence) {
				t.Fatalf("refresh replaced the saved hardware identity: err=%v saved=%+v", err, retained)
			}
		})
	}
}

func TestManualRefreshRepairsOnlineDisplayMetadataWithoutInterruptingLiveLink(t *testing.T) {
	const serial = "saved-usb"
	registry := wirelessRegistry(t, func(request string) string {
		if request != "host:devices-l" {
			t.Errorf("unexpected ADB request %q", request)
		}
		return serial + " device product:product model:Phone transport_id:8\n"
	})
	ctx := context.Background()
	registry.publish([]adb.Device{{Serial: serial, State: "device", TransportID: 8}}, ctx)
	registered, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "usb", Endpoint: serial}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Verify(ctx, "admin", registered.DeviceID, registered.LinkID); err != nil {
		t.Fatal(err)
	}
	if err = registry.verifySnapshot(inventoryUpdate{version: registry.version, epoch: ctx}); err != nil {
		t.Fatal(err)
	}
	links := registry.OnlineLinks()
	if len(links) != 1 {
		t.Fatalf("expected verified online device: %+v", links)
	}
	before := links[0]
	linkCtx, err := registry.LinkContext(before)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce an online identity with a failed initial display observation.
	if _, err = registry.db.Exec(`UPDATE devices SET display_info=NULL WHERE id=?`, registered.DeviceID); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	registry.issues[registered.LinkID] = "读取屏幕尺寸失败"
	registry.mu.Unlock()
	if err = registry.Refresh(ctx, "admin", registered.DeviceID); err != nil {
		t.Fatal(err)
	}
	if linkCtx.Err() != nil {
		t.Fatal("metadata refresh interrupted the current live context")
	}
	if _, err = registry.LinkContext(before); err != nil {
		t.Fatalf("metadata refresh replaced the current live generation: %v", err)
	}
	views, _, err := registry.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if views[0].ConnectionState != "online" || views[0].Links[0].LastError != "" || views[0].AllocationState != "maintenance" {
		t.Fatalf("metadata recovery lost online state, retained the error or changed allocation intent: %+v", views[0])
	}
	var width, height int
	if err = registry.db.QueryRow(`SELECT json_extract(display_info,'$.screen.width'),json_extract(display_info,'$.screen.height') FROM devices WHERE id=?`, registered.DeviceID).Scan(&width, &height); err != nil {
		t.Fatal(err)
	}
	if width != 1080 || height != 2400 {
		t.Fatalf("missing recovered screen geometry: %dx%d", width, height)
	}
}
