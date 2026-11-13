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
	"sync/atomic"
	"testing"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/storage"
)

func wirelessRegistry(t *testing.T, respond func(string) string) *Registry {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "wireless.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
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
					n, err := strconv.ParseUint(string(prefix[:]), 16, 16)
					if err != nil {
						t.Error(err)
						return ""
					}
					body := make([]byte, int(n))
					if _, err := io.ReadFull(conn, body); err != nil {
						t.Error(err)
						return ""
					}
					return string(body)
				}
				service := readService()
				if strings.HasPrefix(service, "host:transport-id:") {
					if _, err := io.WriteString(conn, "OKAY"); err != nil {
						t.Error(err)
						return
					}
					command := readService()
					output := "hardware-one\nMaker\nPhone\nproduct\n15\n35\n"
					if command == "exec:dumpsys display" {
						output = "Logical Displays:\n  Display 0:\n    mBaseDisplayInfo=DisplayInfo{real 1080 x 2400, rotation 0}\n    mOverrideDisplayInfo=null\n"
					}
					if _, err := io.WriteString(conn, "OKAY"+output); err != nil {
						t.Error(err)
					}
					return
				}
				payload := respond(service)
				if _, err := fmt.Fprintf(conn, "OKAY%04x%s", len(payload), payload); err != nil {
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
	registry := New(context.Background(), db, client, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	t.Cleanup(func() { registry.Close() })
	registry.publish(nil, context.Background())
	return registry
}

func TestMDNSAliasEnrollmentKeepsWirelessRouteAndExistingHardware(t *testing.T) {
	const name = "adb-advertisement-not-hardware-identity"
	const endpoint = "192.0.2.8:43217"
	var actions atomic.Int32
	registry := wirelessRegistry(t, func(service string) string {
		if service == "host:mdns:services" {
			return name + " _adb-tls-connect._tcp " + endpoint + "\nchanged _adb-tls-connect._tcp 192.0.2.8:43218\npair _adb-tls-pairing._tcp 192.0.2.8:37641\n"
		}
		actions.Add(1)
		t.Errorf("unexpected network action %q", service)
		return ""
	})
	alias := name + "." + adb.MDNSConnect
	registry.publish([]adb.Device{{Serial: "usb-one", State: "device", TransportID: 1}, {Serial: alias, State: "device", TransportID: 7}}, context.Background())
	usb, err := registry.RegisterDiscovered(context.Background(), "admin", "usb-one", "")
	if err != nil {
		t.Fatal(err)
	}
	wifi, err := registry.RegisterDiscovered(context.Background(), "admin", alias, "")
	if err != nil {
		t.Fatal(err)
	}
	if usb.DeviceID != wifi.DeviceID || usb.LinkID == wifi.LinkID {
		t.Fatal("mDNS alias created another physical device or replaced USB")
	}
	var kind, route, observed string
	if err := registry.db.QueryRow(`SELECT kind,endpoint,observed_serial FROM device_links WHERE id=?`, wifi.LinkID).Scan(&kind, &route, &observed); err != nil {
		t.Fatal(err)
	}
	if kind != "wifi" || route != endpoint || observed != alias {
		t.Fatalf("wireless route = %q/%q/%q", kind, route, observed)
	}
	services, err := registry.ScanWireless(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 || services[0].Endpoint != "192.0.2.8:43218" || services[1].Type != adb.MDNSPairing {
		t.Fatalf("registered route not removed, or same-IP unverified services hidden: %#v", services)
	}
	if actions.Load() != 0 {
		t.Fatal("scanning or enrollment initiated an unsolicited connection/pairing")
	}
}

func TestWirelessActionRejectsChangedBroadcastBeforeSending(t *testing.T) {
	for _, pairing := range []bool{false, true} {
		t.Run(fmt.Sprintf("pairing=%t", pairing), func(t *testing.T) {
			kind := adb.MDNSConnect
			if pairing {
				kind = adb.MDNSPairing
			}
			selected := adb.MDNSService{Name: "phone", Type: kind, Endpoint: "192.0.2.8:43217"}
			var payload atomic.Value
			payload.Store("phone " + kind + " 192.0.2.8:43217\n")
			var actions atomic.Int32
			registry := wirelessRegistry(t, func(service string) string {
				if service == "host:mdns:services" {
					return payload.Load().(string)
				}
				actions.Add(1)
				return ""
			})
			if _, err := registry.ScanWireless(context.Background()); err != nil {
				t.Fatal(err)
			}
			payload.Store("phone " + kind + " 192.0.2.8:43218\n")
			var err error
			if pairing {
				err = registry.PairWireless(context.Background(), "admin", selected, "917263")
			} else {
				_, err = registry.ConnectWireless(context.Background(), "admin", selected)
			}
			var rejection *Error
			if !errors.As(err, &rejection) || rejection.Code != "WIRELESS_SERVICE_CHANGED" {
				t.Fatalf("stale broadcast result = %v", err)
			}
			var records int
			if err := registry.db.QueryRow(`SELECT count(*) FROM operation_records`).Scan(&records); err != nil {
				t.Fatal(err)
			}
			if actions.Load() != 0 || records != 0 {
				t.Fatal("stale selection reached ADB or created an action record")
			}
		})
	}
}

func TestWirelessPairingDoesNotRegisterPhoneOrPersistSecret(t *testing.T) {
	service := adb.MDNSService{Name: "pairing-guid", Type: adb.MDNSPairing, Endpoint: "192.0.2.8:37641"}
	registry := wirelessRegistry(t, func(request string) string {
		switch request {
		case "host:mdns:services":
			return service.Name + " " + service.Type + " " + service.Endpoint + "\n"
		case "host:pair:917263:" + service.Endpoint:
			return "Successfully paired to " + service.Endpoint
		default:
			t.Errorf("pairing performed unexpected action %q", request)
			return ""
		}
	})
	if err := registry.PairWireless(context.Background(), "admin", service, "917263"); err != nil {
		t.Fatal(err)
	}
	var devices int
	if err := registry.db.QueryRow(`SELECT count(*) FROM devices`).Scan(&devices); err != nil {
		t.Fatal(err)
	}
	var kind, status, details string
	if err := registry.db.QueryRow(`SELECT kind,status,safe_details FROM operation_records`).Scan(&kind, &status, &details); err != nil {
		t.Fatal(err)
	}
	if devices != 0 || kind != "device.wireless.pair" || status != "succeeded" || strings.Contains(details, "917263") {
		t.Fatalf("pairing changed registration or leaked its code: %d/%s/%s/%s", devices, kind, status, details)
	}
}

func TestWirelessConnectUpdatesPendingInventoryWithoutRegistering(t *testing.T) {
	service := adb.MDNSService{Name: "phone", Type: adb.MDNSConnect, Endpoint: "192.0.2.8:43217"}
	registry := wirelessRegistry(t, func(request string) string {
		switch request {
		case "host:mdns:services":
			return service.Name + " " + service.Type + " " + service.Endpoint + "\n"
		case "host:connect:" + service.Endpoint:
			return "connected to " + service.Endpoint
		case "host:devices-l":
			return service.Endpoint + " device model:Phone transport_id:7\n"
		default:
			t.Errorf("unexpected connection action %q", request)
			return ""
		}
	})
	found, err := registry.ConnectWireless(context.Background(), "admin", service)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Serial != service.Endpoint || found[0].State != "device" || found[0].TransportID != 7 {
		t.Fatalf("connected device not ready for explicit enrollment: %#v", found)
	}
	var devices int
	if err := registry.db.QueryRow(`SELECT count(*) FROM devices`).Scan(&devices); err != nil {
		t.Fatal(err)
	}
	if devices != 0 {
		t.Fatal("connecting implicitly registered the device")
	}
}

func TestRegisteredAddressAlsoHidesItsMDNSAlias(t *testing.T) {
	registry := wirelessRegistry(t, func(request string) string {
		if request == "host:mdns:services" {
			return "known _adb-tls-connect._tcp 192.0.2.8:43217\nother _adb-tls-connect._tcp 192.0.2.8:43218\n"
		}
		t.Errorf("alias filtering performed an unexpected action %q", request)
		return ""
	})
	if _, err := registry.Register(context.Background(), "admin", Registration{DisplayName: "phone", LinkRequest: LinkRequest{Kind: "wifi", Endpoint: "192.0.2.8:43217"}}); err != nil {
		t.Fatal(err)
	}
	registry.publish([]adb.Device{
		{Serial: "192.0.2.8:43217", State: "device", TransportID: 1},
		{Serial: "known." + adb.MDNSConnect, State: "device", TransportID: 2},
		{Serial: "other." + adb.MDNSConnect, State: "device", TransportID: 3},
	}, context.Background())
	found, err := registry.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Serial != "other."+adb.MDNSConnect {
		t.Fatalf("known route was duplicated, or another unregistered port was hidden: %#v", found)
	}
}
