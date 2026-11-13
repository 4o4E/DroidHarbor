package adb

import (
	"context"
	"io"
	"net"
	"reflect"
	"testing"
)

func TestMDNSServicesPreservesServiceTypesAndActualPorts(t *testing.T) {
	client := testClient(t, func(conn net.Conn) {
		if service := readTestService(t, conn); service != "host:mdns:services" {
			t.Errorf("unexpected discovery service %q", service)
			return
		}
		writeTestResponse(t, conn, "List of discovered mdns services\r\nphone\t_adb-tls-connect._tcp.\t192.0.2.8:43217\r\nphone _adb-tls-connect._tcp 192.0.2.8:43217\nlegacy _adb._tcp 192.0.2.7:5555\npair _adb-tls-pairing._tcp [fe80::8%12]:37641\n")
	})
	actual, err := client.MDNSServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []MDNSService{
		{Name: "legacy", Type: MDNSLegacy, Endpoint: "192.0.2.7:5555"},
		{Name: "pair", Type: MDNSPairing, Endpoint: "[fe80::8%12]:37641"},
		{Name: "phone", Type: MDNSConnect, Endpoint: "192.0.2.8:43217"},
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("services = %#v, want %#v", actual, want)
	}
}

func TestMDNSDiscoveryFailuresDoNotBecomeEmptySuccess(t *testing.T) {
	for _, payload := range []string{
		"ERROR: mdns discovery disabled\n",
		"phone _adb-tls-connect._tcp\n",
		"phone _adb-tls-connect._tcp 192.0.2.8:0\n",
		"phone _adb-tls-connect._tcp 192.0.2.8:65536\n",
		"phone _adb-tls-connect._tcp 0.0.0.0:44321\n",
		"valid _adb._tcp 192.0.2.9:5555\nbroken entry\n",
	} {
		services, err := parseMDNSServices(payload)
		if err == nil || services != nil {
			t.Fatalf("invalid discovery became a usable snapshot: %#v, %v", services, err)
		}
	}
	client := testClient(t, func(conn net.Conn) {
		_ = readTestService(t, conn)
		_, _ = io.WriteString(conn, "FAIL0014unknown host service")
	})
	if services, err := client.MDNSServices(context.Background()); err == nil || services != nil {
		t.Fatalf("unsupported mDNS became empty success: %#v, %v", services, err)
	}
}
