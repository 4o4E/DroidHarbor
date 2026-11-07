package adb

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDevicesPreservesADBStatesAndProperties(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		service := readTestService(t, conn)
		if service != "host:devices-l" {
			t.Errorf("service = %q", service)
			return
		}
		writeTestResponse(t, conn, "ZX1 device product:Pixel model:Phone transport_id:7\nZX2 offline usb:1-2 transport_id:9\n(no serial number) no permissions (see https://developer.android.com/studio/run/device.html) model:Phone transport_id:12\n")
	})
	devices, err := client.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 3 || devices[0].State != "device" || devices[0].TransportID != 7 ||
		devices[0].Properties["model"] != "Phone" || devices[1].State != "offline" ||
		devices[2].Serial != "(no serial number)" ||
		devices[2].State != "no permissions (see https://developer.android.com/studio/run/device.html)" ||
		devices[2].Properties["model"] != "Phone" || devices[2].TransportID != 12 {
		t.Fatalf("unexpected device snapshot: %#v", devices)
	}
}

func TestVersionReturnsSmartSocketProtocolVersion(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:version" {
			t.Errorf("service = %q", service)
			return
		}
		writeTestResponse(t, conn, "0029")
	})
	version, err := client.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != 0x29 {
		t.Fatalf("version = %#x, want 0x29", version)
	}
}

func TestFeaturesQueriesSelectedTransport(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host-transport-id:42:features" {
			t.Errorf("service = %q", service)
			return
		}
		writeTestResponse(t, conn, "shell_v2,cmd,stat_v2")
	})
	features, err := client.Features(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(features, ",") != "shell_v2,cmd,stat_v2" {
		t.Fatalf("features = %#v", features)
	}
}

func TestFeaturesRejectsMalformedFeatureList(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		_ = readTestService(t, conn)
		writeTestResponse(t, conn, "shell_v2,,cmd")
	})
	if _, err := client.Features(context.Background(), 9); err == nil {
		t.Fatal("expected malformed feature-list error")
	}
}

func TestPairReturnsOfficialSuccessText(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:pair:917263:192.0.2.10:37123" {
			t.Errorf("service = %q", service)
			return
		}
		writeTestResponse(t, conn, "Successfully paired to 192.0.2.10:37123 [guid=00000000]")
	})
	result, err := client.Pair(context.Background(), "192.0.2.10:37123", "917263")
	if err != nil {
		t.Fatal(err)
	}
	if result != "Successfully paired to 192.0.2.10:37123 [guid=00000000]" {
		t.Fatalf("result = %q", result)
	}
}

func TestDevicesRejectsIncompletePayload(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		_ = readTestService(t, conn)
		writeTestResponse(t, conn, "serial\tdevice\ntruncated")
	})
	if _, err := client.Devices(context.Background()); err == nil {
		t.Fatal("expected incomplete device-list error")
	}
}

func TestDevicesRejectsDuplicateProperties(t *testing.T) {
	for _, list := range []string{
		"ZX1 device transport_id:1 transport_id:2\n",
		"ZX1 device model:Phone model:Other\n",
	} {
		client := testClient(t, func(conn net.Conn) {
			_ = readTestService(t, conn)
			writeTestResponse(t, conn, list)
		})
		if _, err := client.Devices(context.Background()); err == nil {
			t.Fatalf("expected duplicate-property error for %q", list)
		}
	}
}

func TestOpenSelectsTransportAndCancellationClosesBinaryStream(t *testing.T) {
	accepted := make(chan struct{})
	peerClosed := make(chan struct{})
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:transport-id:42" {
			t.Errorf("transport service = %q", service)
			return
		}
		_, _ = io.WriteString(conn, "OKAY")
		if service := readTestService(t, conn); service != "shell:printf 'hello\nworld'" {
			t.Errorf("device service = %q", service)
			return
		}
		_, _ = io.WriteString(conn, "OKAY")
		close(accepted)
		var data [1]byte
		_, _ = conn.Read(data[:])
		close(peerClosed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Open(ctx, 42, "shell:printf 'hello\nworld'", ctx.Err)
	if err != nil {
		t.Fatal(err)
	}
	<-accepted
	cancel()
	select {
	case <-peerClosed:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not close the stream")
	}
	_ = stream.Close()
}

func TestOpenCancellationInterruptsHandshake(t *testing.T) {
	connected := make(chan struct{})
	client := testClient(t, func(conn net.Conn) {
		_ = readTestService(t, conn)
		close(connected)
		var b [1]byte
		_, _ = conn.Read(b[:])
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Open(ctx, 1, "shell:", ctx.Err)
		result <- err
	}()
	<-connected
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected canceled handshake")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt handshake")
	}
}

func TestConnectRejectsBusinessFailureAfterOKAY(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:connect:192.0.2.10:5555" {
			t.Errorf("service = %q", service)
			return
		}
		writeTestResponse(t, conn, "unable to connect to 192.0.2.10:5555")
	})
	if _, err := client.Connect(context.Background(), "192.0.2.10:5555"); err == nil {
		t.Fatal("expected semantic connect failure")
	}
}

func TestPairFailureDoesNotExposePairingCode(t *testing.T) {
	const code = "917263"
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:pair:"+code+":192.0.2.10:37123" {
			t.Errorf("unexpected pair service %q", service)
			return
		}
		writeTestResponse(t, conn, "Failed: invalid pairing code")
	})
	_, err := client.Pair(context.Background(), "192.0.2.10:37123", code)
	if err == nil || strings.Contains(err.Error(), code) {
		t.Fatalf("pair error must fail without exposing code: %v", err)
	}
}

func TestPairPropagatesCancellationWithoutCode(t *testing.T) {
	connected := make(chan struct{})
	client := testClient(t, func(conn net.Conn) {
		_ = readTestService(t, conn)
		close(connected)
		var b [1]byte
		_, _ = conn.Read(b[:])
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Pair(ctx, "192.0.2.10:37123", "917263")
		result <- err
	}()
	<-connected
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt pairing request")
	}
}

func testClient(t *testing.T, serve func(net.Conn)) *Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			serve(conn)
		}
	}()
	client, err := NewClient(listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func readTestService(t *testing.T, conn net.Conn) string {
	t.Helper()
	var prefix [4]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		t.Errorf("read service length: %v", err)
		return ""
	}
	length, err := hex.DecodeString(string(prefix[:]))
	if err != nil || len(length) != 2 {
		t.Errorf("invalid service length %q", prefix)
		return ""
	}
	n := int(length[0])<<8 | int(length[1])
	service := make([]byte, n)
	if _, err := io.ReadFull(conn, service); err != nil {
		t.Errorf("read service: %v", err)
		return ""
	}
	return string(service)
}

func writeTestResponse(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if _, err := io.WriteString(conn, "OKAY"+fmt.Sprintf("%04x", len(payload))+payload); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestTrackReturnsOrderedSnapshotsIncludingEmptyInitialState(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:track-devices-l" {
			t.Errorf("service = %q", service)
			return
		}
		_, _ = io.WriteString(conn, "OKAY")
		writeTestFrame(t, conn, "")
		writeTestFrame(t, conn, "ZX1 device transport_id:7\n")
		writeTestFrame(t, conn, "ZX1 unauthorized transport_id:7\n")
	})
	tracker, err := client.Track(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()

	initial, err := tracker.Next()
	if err != nil || len(initial) != 0 {
		t.Fatalf("initial snapshot = %#v, err = %v", initial, err)
	}
	online, err := tracker.Next()
	if err != nil || len(online) != 1 || online[0].State != "device" {
		t.Fatalf("online snapshot = %#v, err = %v", online, err)
	}
	unauthorized, err := tracker.Next()
	if err != nil || len(unauthorized) != 1 || unauthorized[0].State != "unauthorized" {
		t.Fatalf("unauthorized snapshot = %#v, err = %v", unauthorized, err)
	}
	if _, err := tracker.Next(); err == nil {
		t.Fatal("EOF must not be reported as an empty snapshot")
	}
}

func TestTrackRejectsMalformedFrame(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		_ = readTestService(t, conn)
		_, _ = io.WriteString(conn, "OKAYzzzz")
	})
	tracker, err := client.Track(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	if snapshot, err := tracker.Next(); err == nil || snapshot != nil {
		t.Fatalf("snapshot = %#v, err = %v; malformed frame must fail", snapshot, err)
	}
}

func TestTrackEOFBeforeFirstSnapshotIsError(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		_ = readTestService(t, conn)
		_, _ = io.WriteString(conn, "OKAY")
	})
	tracker, err := client.Track(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	if snapshot, err := tracker.Next(); err == nil || snapshot != nil {
		t.Fatalf("snapshot = %#v, err = %v; EOF must fail", snapshot, err)
	}
}

func TestTrackCancellationAndCloseInterruptNext(t *testing.T) {
	for _, closeTracker := range []bool{false, true} {
		ready := make(chan struct{})
		client := testClient(t, func(conn net.Conn) {
			_ = readTestService(t, conn)
			_, _ = io.WriteString(conn, "OKAY")
			close(ready)
			var b [1]byte
			_, _ = conn.Read(b[:])
		})
		ctx, cancel := context.WithCancel(context.Background())
		tracker, err := client.Track(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := tracker.Next()
			result <- err
		}()
		<-ready
		if closeTracker {
			_ = tracker.Close()
		} else {
			cancel()
		}
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("interrupted Next returned no error")
			}
			if !closeTracker && !errors.Is(err, context.Canceled) {
				t.Fatalf("Next error = %v, want context cancellation", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close/cancellation did not interrupt Next")
		}
		cancel()
		_ = tracker.Close()
	}
}

func writeTestFrame(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if _, err := io.WriteString(conn, fmt.Sprintf("%04x", len(payload))+payload); err != nil {
		t.Errorf("write frame: %v", err)
	}
}
