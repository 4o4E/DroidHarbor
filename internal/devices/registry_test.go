package devices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/events"
	"droidharbor/internal/leases"
	"droidharbor/internal/storage"
)

func TestCommittedRegistrationPublishesDeviceInvalidation(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := New(ctx, db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	bus := events.New("boot")
	defer bus.Close()
	registry.SetEvents(bus)
	sub, err := bus.Subscribe(events.Scope{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	<-sub.C // initial resync
	registered, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "usb", Endpoint: "usb-one"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sub.C:
		if event.EntityType != "device" || event.EntityID != registered.DeviceID || event.DeviceID != registered.DeviceID {
			t.Fatalf("unexpected registration invalidation: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("committed registration did not publish a device invalidation")
	}
}

func TestUnverifiedRegisteredLinksPublishObservableState(t *testing.T) {
	for _, test := range []struct {
		name    string
		devices []adb.Device
	}{
		{name: "offline"},
		{name: "unauthorized", devices: []adb.Device{{Serial: "phone", State: "unauthorized"}}},
		{name: "identity-conflict", devices: []adb.Device{{Serial: "phone", State: "device", TransportID: 1}, {Serial: "phone", State: "device", TransportID: 2}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "events.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			registry := New(ctx, db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
			defer registry.Close()
			bus := events.New("boot")
			defer bus.Close()
			registry.SetEvents(bus)
			sub, err := bus.Subscribe(events.Scope{Admin: true})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			<-sub.C // initial resync
			registered, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "usb", Endpoint: "phone"}})
			if err != nil {
				t.Fatal(err)
			}
			<-sub.C // registration invalidation
			registry.publish(test.devices, ctx)
			registry.mu.RLock()
			update := inventoryUpdate{version: registry.version, epoch: ctx}
			registry.mu.RUnlock()
			if err := registry.verifySnapshot(update); err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-sub.C:
				if event.EntityID != registered.DeviceID {
					t.Fatalf("unverified link invalidated unrelated device: %+v", event)
				}
			case <-time.After(time.Second):
				t.Fatalf("%s state for unverified registered link was not published", test.name)
			}
			registry.publish(append(test.devices, adb.Device{Serial: "unrelated", State: "unauthorized"}), ctx)
			registry.mu.RLock()
			update = inventoryUpdate{version: registry.version, epoch: ctx}
			registry.mu.RUnlock()
			if err := registry.verifySnapshot(update); err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-sub.C:
				t.Fatalf("unrelated inventory change invalidated unchanged registered device: %+v", event)
			default:
			}
		})
	}
}

func TestIdentityAssociationRejectsDuplicateAndWrongHardware(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := identityFixture(t, map[uint64]string{
		1: "hardware-one\nMaker\nModel\nproduct\n15\n35\n",
		2: "hardware-one\nMaker\nModel\nproduct\n15\n35\n",
		3: "hardware-other\nMaker\nModel\nproduct\n15\n35\n",
	})
	registry := New(ctx, db, client, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	registry.publish([]adb.Device{{Serial: "usb-one", State: "device", TransportID: 1}, {Serial: "192.0.2.1:5555", State: "device", TransportID: 2}, {Serial: "usb-other", State: "device", TransportID: 3}}, ctx)
	first, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "usb", Endpoint: "usb-one"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Verify(ctx, "admin", first.DeviceID, first.LinkID); err != nil {
		t.Fatal(err)
	}
	second, err := registry.AddLink(ctx, "admin", first.DeviceID, LinkRequest{Kind: "wifi", Endpoint: "192.0.2.1:5555"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Verify(ctx, "admin", first.DeviceID, second.LinkID); err != nil {
		t.Fatal(err)
	}
	wrong, err := registry.AddLink(ctx, "admin", first.DeviceID, LinkRequest{Kind: "usb", Endpoint: "usb-other"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Verify(ctx, "admin", first.DeviceID, wrong.LinkID)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "IDENTITY_CONFLICT" {
		t.Fatalf("wrong hardware was not rejected: %v", err)
	}
	// The same phone can appear under a new route, but must not become a second
	// independently allocatable device. These property responses are fixtures.
	registry.publish([]adb.Device{{Serial: "another-route", State: "device", TransportID: 2}}, ctx)
	duplicate, err := registry.Register(ctx, "admin", Registration{DisplayName: "duplicate", LinkRequest: LinkRequest{Kind: "usb", Endpoint: "another-route"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Verify(ctx, "admin", duplicate.DeviceID, duplicate.LinkID)
	if !errors.As(err, &conflict) || conflict.Code != "IDENTITY_CONFLICT" {
		t.Fatalf("duplicate identity was not rejected: %v", err)
	}
	var enabled bool
	if err = db.QueryRow(`SELECT enabled FROM device_links WHERE id=?`, duplicate.LinkID).Scan(&enabled); err != nil || enabled {
		t.Fatal("conflicting identity became enabled")
	}
	var state string
	if err = db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, duplicate.DeviceID).Scan(&state); err != nil || state != "maintenance" {
		t.Fatal("conflicting device left maintenance")
	}
	views, _, err := registry.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Links != nil || view.Version != "" || view.MaintenanceRequested != nil {
			t.Fatal("caller summary exposed administrator metadata")
		}
	}
}

func TestTransportSnapshotInvalidatesExistingLinkContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := New(ctx, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	registry.publish([]adb.Device{{Serial: "phone", State: "device", TransportID: 1}}, ctx)
	link := leases.Link{DeviceID: "device", LinkID: "link", TransportID: 1, Generation: 1}
	linkCtx, stop := context.WithCancel(ctx)
	registry.live[link.LinkID] = &liveLink{link: link, serial: "phone", ctx: linkCtx, cancel: stop}
	authorized, err := registry.LinkContext(link)
	if err != nil {
		t.Fatal(err)
	}
	registry.publish([]adb.Device{{Serial: "phone", State: "device", TransportID: 2}}, ctx)
	select {
	case <-authorized.Done():
	default:
		t.Fatal("old transport generation remained authorized")
	}
	if _, err = registry.LinkContext(link); err == nil {
		t.Fatal("old link could be reacquired")
	}
	if len(registry.OnlineLinks()) != 0 {
		t.Fatal("replacement transport was trusted before identity verification")
	}
}

func identityFixture(t *testing.T, properties map[uint64]string) *adb.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
					t.Error(err)
					return
				}
				readService := func() string {
					var prefix [4]byte
					if _, err := io.ReadFull(conn, prefix[:]); err != nil {
						t.Error(err)
						return ""
					}
					size, err := strconv.ParseUint(string(prefix[:]), 16, 16)
					if err != nil {
						t.Error(err)
						return ""
					}
					value := make([]byte, size)
					if _, err = io.ReadFull(conn, value); err != nil {
						t.Error(err)
						return ""
					}
					return string(value)
				}
				transport, err := strconv.ParseUint(strings.TrimPrefix(readService(), "host:transport-id:"), 10, 64)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err = io.WriteString(conn, "OKAY"); err != nil {
					t.Error(err)
					return
				}
				if !strings.HasPrefix(readService(), "exec:") {
					t.Error("unexpected service in identity fixture")
					return
				}
				if _, err = io.WriteString(conn, "OKAY"+properties[transport]); err != nil {
					t.Error(err)
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); workers.Wait() })
	client, err := adb.NewClient(listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestWirelessAddressChangePreservesIdentityAndWithdrawsOldTransport(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := identityFixture(t, map[uint64]string{
		1: "hardware-one\nMaker\nModel\nproduct\n15\n35\n",
		2: "hardware-other\nMaker\nModel\nproduct\n15\n35\n",
		3: "hardware-one\nMaker\nModel\nproduct\n15\n35\n",
	})
	registry := New(ctx, db, client, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	oldEndpoint, newEndpoint := "192.0.2.1:5555", "192.0.2.1:6000"
	registry.publish([]adb.Device{{Serial: oldEndpoint, State: "device", TransportID: 1}}, ctx)
	registered, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "wifi", Endpoint: oldEndpoint}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Verify(ctx, "admin", registered.DeviceID, registered.LinkID); err != nil {
		t.Fatal(err)
	}
	linkCtx, stop := context.WithCancel(ctx)
	defer stop()
	link := leases.Link{DeviceID: registered.DeviceID, LinkID: registered.LinkID, TransportID: 1, Generation: 1}
	registry.live[link.LinkID] = &liveLink{link: link, serial: oldEndpoint, ctx: linkCtx, cancel: stop}
	views, _, err := registry.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	patch := LinkPatch{Version: views[0].Version, Endpoint: newEndpoint}
	if err = registry.PatchLink(ctx, "admin", registered.DeviceID, registered.LinkID, patch); err != nil {
		t.Fatal(err)
	}
	select {
	case <-linkCtx.Done():
	default:
		t.Fatal("old wireless transport remained authorized after address change")
	}
	if _, err = registry.LinkContext(link); err == nil {
		t.Fatal("old transport was reacquired")
	}
	if err = registry.PatchLink(ctx, "admin", registered.DeviceID, registered.LinkID, patch); err == nil {
		t.Fatal("stale configuration overwrote the new endpoint")
	}
	stored, _, err := registry.registered(ctx, registered.DeviceID, registered.LinkID)
	if err != nil || stored.enabled || stored.endpoint != newEndpoint || !stored.evidence.Valid {
		t.Fatalf("address change lost identity or trusted an unverified endpoint: %v", err)
	}
	registry.publish([]adb.Device{{Serial: newEndpoint, State: "device", TransportID: 2}}, ctx)
	_, err = registry.Verify(ctx, "admin", registered.DeviceID, registered.LinkID)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "IDENTITY_CONFLICT" {
		t.Fatalf("reused wireless address accepted a different phone: %v", err)
	}
	registry.publish([]adb.Device{{Serial: newEndpoint, State: "device", TransportID: 3}}, ctx)
	if _, err = registry.Verify(ctx, "admin", registered.DeviceID, registered.LinkID); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Register(ctx, "admin", Registration{DisplayName: "another phone", LinkRequest: LinkRequest{Kind: "wifi", Endpoint: oldEndpoint}}); err != nil {
		t.Fatalf("old endpoint remained permanently reserved: %v", err)
	}
}

func TestDelayedConnectCannotRestoreSupersededEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	received, resume := make(chan struct{}), make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		var prefix [4]byte
		if _, err = io.ReadFull(conn, prefix[:]); err != nil {
			t.Error(err)
			return
		}
		size, err := strconv.ParseUint(string(prefix[:]), 16, 16)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err = io.CopyN(io.Discard, conn, int64(size)); err != nil {
			t.Error(err)
			return
		}
		close(received)
		select {
		case <-resume:
		case <-ctx.Done():
			return
		}
		reply := "connected to 192.0.2.1:5555"
		if _, err = fmt.Fprintf(conn, "OKAY%04x%s", len(reply), reply); err != nil && ctx.Err() == nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); workers.Wait() })
	client, err := adb.NewClient(listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	registry := New(ctx, db, client, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	registered, err := registry.Register(ctx, "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "wifi", Endpoint: "192.0.2.1:5555"}})
	if err != nil {
		t.Fatal(err)
	}
	views, _, err := registry.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		finished <- registry.Connect(ctx, "admin", registered.DeviceID, registered.LinkID)
	}()
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("connect did not reach the controlled ADB endpoint")
	}
	if err = registry.PatchLink(ctx, "admin", registered.DeviceID, registered.LinkID, LinkPatch{Version: views[0].Version, Endpoint: "192.0.2.1:6000"}); err != nil {
		t.Fatal(err)
	}
	close(resume)
	err = <-finished
	var unknown *Error
	if !errors.As(err, &unknown) || unknown.Code != "ADB_RESULT_UNKNOWN" {
		t.Fatalf("superseded connect result was accepted: %v", err)
	}
	stored, _, err := registry.registered(ctx, registered.DeviceID, registered.LinkID)
	if err != nil || stored.serial != "192.0.2.1:6000" || stored.endpoint != stored.serial {
		t.Fatalf("late connect restored the obsolete route: %v", err)
	}
}
func TestIncompleteOwnedResourceCleanupRemainsPending(t *testing.T) {
	registry := &Registry{taskCleanup: func(context.Context, string) (bool, error) {
		return false, nil
	}}
	complete, err := registry.Recover(context.Background(), "device")
	if err != nil {
		t.Fatalf("Recover error=%v, want deferred completion", err)
	}
	if complete {
		t.Fatal("incomplete owned-resource cleanup was reported complete")
	}
}

func TestOfflineRecoveryCompletesWhenNoOwnedResourcesRemain(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES('offline','offline','reclaiming',0)`); err != nil {
		t.Fatal(err)
	}
	registry := New(ctx, db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	registry.bootID = "current-boot"
	registry.SetTaskCleanup(func(context.Context, string) (bool, error) { return true, nil })
	complete, err := registry.Recover(ctx, "offline")
	if err != nil || !complete {
		t.Fatalf("offline recovery with no unclean resources = %v, %v; want complete", complete, err)
	}
	registry.SetTaskCleanup(nil)
	if _, err := db.Exec(`INSERT INTO owned_resources(id,device_id,owner_kind,owner_id,boot_id,kind,locator,state,created_at) VALUES('capture','offline','device-capture','capture','current-boot','scrcpy','{}','failed',?)`, time.Now().UTC().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	complete, err = registry.Recover(ctx, "offline")
	if complete {
		t.Fatal("offline recovery with an unhandled capture resource was reported complete")
	}
	var recoveryErr *Error
	if !errors.As(err, &recoveryErr) || recoveryErr.Code != "RESOURCE_RECOVERY_REQUIRED" {
		t.Fatalf("offline recovery error=%v, want RESOURCE_RECOVERY_REQUIRED", err)
	}
}

func TestOfflineReadLinkDoesNotBlockOtherDeviceDisconnect(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "offline-read.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO devices(id,display_name,allocation_state) VALUES('offline','offline','maintenance')`); err != nil {
		t.Fatal(err)
	}
	registry := New(ctx, db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	registry.publish([]adb.Device{{Serial: "other", State: "device", TransportID: 1}}, ctx)
	link := leases.Link{DeviceID: "other", LinkID: "other-link", TransportID: 1, Generation: 1}
	linkCtx, cancelLink := context.WithCancel(ctx)
	defer cancelLink()
	registry.live[link.LinkID] = &liveLink{link: link, serial: "other", ctx: linkCtx, cancel: cancelLink}

	_, _, err = registry.ReadLink(ctx, "offline")
	var unavailable *Error
	if !errors.As(err, &unavailable) || unavailable.Code != "DEVICE_UNAVAILABLE" {
		t.Fatalf("offline ReadLink = %v, want DEVICE_UNAVAILABLE", err)
	}
	published := make(chan struct{})
	go func() {
		registry.publish(nil, ctx)
		close(published)
	}()
	select {
	case <-linkCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("offline link lookup blocked another device's disconnect")
	}
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("disconnect snapshot did not finish publishing")
	}
	if registry.HasLink(link) {
		t.Fatal("disconnected device retained its previous link authorization")
	}
}

func TestDiscoveredEnrollmentDeduplicatesHardwareAndKeepsUntrustedLinksDisabled(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "discovered.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := identityFixture(t, map[uint64]string{
		1: "one-phone\nMaker\nPhone\nproduct\n15\n35\n",
		2: "one-phone\nMaker\nPhone\nproduct\n15\n35\n",
		3: "one-phone\nMaker\nDifferent\nproduct\n15\n35\n",
	})
	registry := New(ctx, db, client, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	defer registry.Close()
	registry.publish([]adb.Device{
		{Serial: "usb-one", State: "device", TransportID: 1},
		{Serial: "192.0.2.1:5555", State: "device", TransportID: 2},
		{Serial: "usb-conflict", State: "device", TransportID: 3},
		{Serial: "usb-untrusted", State: "unauthorized", TransportID: 4},
	}, ctx)
	usb, err := registry.RegisterDiscovered(ctx, "admin", "usb-one", "")
	if err != nil {
		t.Fatal(err)
	}
	wifi, err := registry.RegisterDiscovered(ctx, "admin", "192.0.2.1:5555", "")
	if err != nil {
		t.Fatal(err)
	}
	if usb.DeviceID != wifi.DeviceID || usb.LinkID == wifi.LinkID {
		t.Fatal("USB and Wi-Fi became separate physical devices or overwrote one link")
	}
	pending, err := registry.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remaining := make(map[string]bool)
	for _, device := range pending {
		remaining[device.Serial] = true
	}
	if remaining["usb-one"] || remaining["192.0.2.1:5555"] || !remaining["usb-untrusted"] {
		t.Fatal("registered links remained pending or an untrusted new phone was hidden")
	}
	untrusted, err := registry.RegisterDiscovered(ctx, "admin", "usb-untrusted", "")
	if err != nil {
		t.Fatal(err)
	}
	var enabled bool
	var state string
	if err := db.QueryRow(`SELECT enabled FROM device_links WHERE id=?`, untrusted.LinkID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT allocation_state FROM devices WHERE id=?`, untrusted.DeviceID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if enabled || state != "maintenance" {
		t.Fatal("an unauthorized phone became usable or allocatable")
	}
	_, err = registry.RegisterDiscovered(ctx, "admin", "usb-conflict", "")
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "IDENTITY_CONFLICT" {
		t.Fatalf("conflicting hardware was accepted: %v", err)
	}
	var persisted bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM device_links WHERE endpoint='usb-conflict')`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted {
		t.Fatal("a rejected identity left a registered link")
	}
}
