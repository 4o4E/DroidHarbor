package adb

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const (
	MDNSLegacy  = "_adb._tcp"
	MDNSConnect = "_adb-tls-connect._tcp"
	MDNSPairing = "_adb-tls-pairing._tcp"
)

type MDNSService struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
}

// MDNSServices reads the configured server's discovery snapshot. It neither
// starts discovery processes nor connects or pairs any advertised device.
func (c *Client) MDNSServices(ctx context.Context) ([]MDNSService, error) {
	payload, err := c.request(ctx, "host:mdns:services")
	if err != nil {
		return nil, err
	}
	return parseMDNSServices(string(payload))
}

func parseMDNSServices(payload string) ([]MDNSService, error) {
	result := make([]MDNSService, 0)
	seen := make(map[MDNSService]bool)
	for index, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "List of discovered mdns services" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid ADB mDNS service at line %d", index+1)
		}
		kind := strings.TrimSuffix(fields[1], ".")
		switch kind {
		case MDNSLegacy, MDNSConnect, MDNSPairing:
		default:
			return nil, fmt.Errorf("unsupported ADB mDNS service type at line %d", index+1)
		}
		address, err := netip.ParseAddrPort(fields[2])
		if err != nil || address.Port() == 0 || address.Addr().IsUnspecified() || address.Addr().IsMulticast() {
			return nil, fmt.Errorf("invalid ADB mDNS address at line %d", index+1)
		}
		service := MDNSService{Name: fields[0], Type: kind, Endpoint: address.String()}
		if !seen[service] {
			seen[service] = true
			result = append(result, service)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		if result[i].Type != result[j].Type {
			return result[i].Type < result[j].Type
		}
		return result[i].Endpoint < result[j].Endpoint
	})
	return result, nil
}
