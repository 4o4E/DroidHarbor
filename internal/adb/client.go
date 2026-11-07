package adb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxHostPayload = 0xffff
	maxServiceSize = 0xffff
)

// Device is one entry from the ADB server's host:devices-l snapshot.
type Device struct {
	Serial      string
	State       string
	TransportID uint64
	Properties  map[string]string
}

// Client speaks the ADB server smart-socket protocol at an explicitly configured
// management address. It never starts or stops an ADB server.
type Client struct {
	address string
	timeout time.Duration
	dialer  net.Dialer
}

// NewClient creates a client for an explicit TCP ADB server address.
func NewClient(address string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(address) != address || address == "" {
		return nil, errors.New("ADB server address is required")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("ADB server address must be host:port")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return nil, errors.New("ADB server address has an invalid port")
	}
	if timeout <= 0 {
		return nil, errors.New("ADB timeout must be positive")
	}
	return &Client{address: address, timeout: timeout, dialer: net.Dialer{Timeout: timeout}}, nil
}

// Version returns the ADB smart-socket protocol version, not a platform-tools version.
func (c *Client) Version(ctx context.Context) (int, error) {
	payload, err := c.request(ctx, "host:version")
	if err != nil {
		return 0, err
	}
	if len(payload) != 4 {
		return 0, errors.New("ADB server returned an invalid protocol version")
	}
	v, err := strconv.ParseUint(string(payload), 16, 32)
	if err != nil {
		return 0, errors.New("ADB server returned an invalid protocol version")
	}
	return int(v), nil
}

// Features returns the target device's advertised ADB feature names.
func (c *Client) Features(ctx context.Context, transportID uint64) ([]string, error) {
	if transportID == 0 {
		return nil, errors.New("ADB transport id must be positive")
	}
	service := fmt.Sprintf("host-transport-id:%d:features", transportID)
	payload, err := c.request(ctx, service)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return []string{}, nil
	}
	features := strings.Split(string(payload), ",")
	seen := make(map[string]struct{}, len(features))
	for _, feature := range features {
		if feature == "" || strings.TrimSpace(feature) != feature || strings.ContainsRune(feature, '\x00') {
			return nil, errors.New("ADB server returned a malformed feature list")
		}
		if _, ok := seen[feature]; ok {
			return nil, errors.New("ADB server returned a duplicate feature")
		}
		seen[feature] = struct{}{}
	}
	return features, nil
}

// Devices returns a complete point-in-time host:devices-l result.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	payload, err := c.request(ctx, "host:devices-l")
	if err != nil {
		return nil, err
	}
	return parseDevices(payload)
}

func parseDevices(payload []byte) ([]Device, error) {
	if len(payload) == 0 {
		return []Device{}, nil
	}
	if payload[len(payload)-1] != '\n' {
		return nil, errors.New("ADB server returned an incomplete device list")
	}
	lines := strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n")
	devices := make([]Device, 0, len(lines))
	for _, line := range lines {
		serial := ""
		deviceLine := line
		if strings.HasPrefix(deviceLine, "(no serial number) ") {
			serial = "(no serial number)"
			deviceLine = strings.TrimPrefix(deviceLine, "(no serial number) ")
		}
		fields := strings.Fields(deviceLine)
		if serial == "" {
			if len(fields) < 2 || strings.ContainsRune(fields[0], '\x00') {
				return nil, errors.New("ADB server returned a malformed device entry")
			}
			serial = fields[0]
			fields = fields[1:]
		}
		if len(fields) == 0 {
			return nil, errors.New("ADB server returned a malformed device entry")
		}
		noPermissions := fields[0] == "no" && len(fields) > 1 && fields[1] == "permissions"
		propertyStart := len(fields)
		if noPermissions {
			// The official no-permissions help text is variable and may contain
			// URL colons. Do not mistake those for long-format properties.
			for i := 2; i < len(fields); i++ {
				key, _, hasValue := strings.Cut(fields[i], ":")
				switch key {
				case "product", "model", "device", "transport_id", "usb", "features":
					if hasValue {
						propertyStart = i
						i = len(fields)
					}
				}
			}
		} else {
			for i, field := range fields {
				key, _, hasValue := strings.Cut(field, ":")
				if hasValue && key != "" {
					propertyStart = i
					break
				}
			}
		}
		if propertyStart == 0 {
			return nil, errors.New("ADB server returned a malformed device state")
		}
		d := Device{
			Serial:     serial,
			State:      strings.Join(fields[:propertyStart], " "),
			Properties: make(map[string]string),
		}
		for _, field := range fields[propertyStart:] {
			key, value, ok := strings.Cut(field, ":")
			if !ok || key == "" {
				return nil, errors.New("ADB server returned a malformed device property")
			}
			if _, exists := d.Properties[key]; exists {
				return nil, errors.New("ADB server returned a duplicate device property")
			}
			d.Properties[key] = value
			if key == "transport_id" {
				transportID, err := strconv.ParseUint(value, 10, 64)
				if err != nil || transportID == 0 {
					return nil, errors.New("ADB server returned an invalid transport id")
				}
				d.TransportID = transportID
			}
		}
		devices = append(devices, d)
	}
	return devices, nil
}

// DeviceTracker consumes successive snapshots from host:track-devices-l.
// Call Next from a single goroutine; Close may be called concurrently.
type DeviceTracker struct {
	conn *cancelConn
	ctx  context.Context
}

// Track subscribes to device-list snapshots without owning or restarting the ADB server.
func (c *Client) Track(ctx context.Context) (*DeviceTracker, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	fail := func(err error) (*DeviceTracker, error) {
		stopCancel()
		_ = conn.Close()
		return nil, err
	}
	if err := setHandshakeDeadline(ctx, conn, c.timeout); err != nil {
		return fail(err)
	}
	if err := writeService(conn, "host:track-devices-l"); err != nil {
		return fail(err)
	}
	if err := readStatus(conn); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return &DeviceTracker{
		conn: &cancelConn{Conn: conn, stop: stopCancel},
		ctx:  ctx,
	}, nil
}

// Next returns the next complete snapshot. A zero-length initial snapshot is valid.
func (t *DeviceTracker) Next() ([]Device, error) {
	payload, err := readLengthPayload(t.conn, maxHostPayload)
	if err != nil {
		t.Close()
		if t.ctx.Err() != nil {
			return nil, t.ctx.Err()
		}
		return nil, fmt.Errorf("read ADB device tracker snapshot: %w", err)
	}
	devices, err := parseDevices(payload)
	if err != nil {
		t.Close()
		return nil, err
	}
	return devices, nil
}

// Close stops cancellation observation and interrupts a pending Next call.
func (t *DeviceTracker) Close() error {
	return t.conn.Close()
}

// Open pins transportID and checks authorization at each actual socket write,
// including the service-open request that can itself execute a device action.
// The caller owns the returned stream and must close it.
func (c *Client) Open(ctx context.Context, transportID uint64, service string, authorize func() error) (net.Conn, error) {
	if transportID == 0 {
		return nil, errors.New("ADB transport id must be positive")
	}
	if service == "" || len(service) > maxServiceSize || strings.ContainsRune(service, '\x00') {
		return nil, errors.New("invalid ADB device service")
	}
	if authorize == nil {
		return nil, errors.New("ADB service authorization is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := authorize(); err != nil {
		return nil, err
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	conn = GuardWrites(conn, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return authorize()
	})
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	fail := func(err error) (net.Conn, error) {
		stopCancel()
		_ = conn.Close()
		return nil, err
	}
	if err := setHandshakeDeadline(ctx, conn, c.timeout); err != nil {
		return fail(err)
	}
	if err := writeService(conn, fmt.Sprintf("host:transport-id:%d", transportID)); err != nil {
		return fail(err)
	}
	if err := readStatus(conn); err != nil {
		return fail(err)
	}
	if err := writeService(conn, service); err != nil {
		return fail(err)
	}
	if err := readStatus(conn); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return &cancelConn{Conn: conn, stop: stopCancel}, nil
}

// Connect explicitly asks the configured ADB server to connect to an endpoint.
func (c *Client) Connect(ctx context.Context, endpoint string) (string, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return "", err
	}
	return c.hostAction(ctx, "host:connect:"+endpoint, "connect")
}

// Pair explicitly pairs an endpoint. pairingCode is sent only in the smart-socket
// request and is never included in returned errors.
func (c *Client) Pair(ctx context.Context, endpoint, pairingCode string) (string, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return "", err
	}
	if pairingCode == "" || strings.ContainsAny(pairingCode, ":\x00\r\n") {
		return "", errors.New("invalid ADB pairing code")
	}
	return c.hostAction(ctx, "host:pair:"+pairingCode+":"+endpoint, "pair")
}

func validateEndpoint(endpoint string) error {
	if endpoint == "" || strings.ContainsAny(endpoint, "\x00\r\n") {
		return errors.New("invalid ADB endpoint")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || port == "" {
		return errors.New("ADB endpoint must be host:port")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return errors.New("ADB endpoint has an invalid port")
	}
	return nil
}

func (c *Client) hostAction(ctx context.Context, service, action string) (string, error) {
	payload, err := c.request(ctx, service)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if action == "pair" {
			return "", errors.New("ADB pairing request failed")
		}
		return "", err
	}
	result := strings.TrimSpace(string(payload))
	lower := strings.ToLower(result)
	switch action {
	case "connect":
		if strings.HasPrefix(lower, "connected to ") || strings.HasPrefix(lower, "already connected to ") {
			return result, nil
		}
		if result == "" {
			return "", errors.New("ADB server returned an empty connect result")
		}
		return "", fmt.Errorf("ADB connect failed: %s", result)
	case "pair":
		if strings.HasPrefix(lower, "successfully paired to ") {
			return result, nil
		}
		return "", errors.New("ADB pairing failed")
	default:
		return "", errors.New("unsupported ADB host action")
	}
}

func (c *Client) request(ctx context.Context, service string) ([]byte, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	if err = setHandshakeDeadline(ctx, conn, c.timeout); err != nil {
		return nil, err
	}
	if err = writeService(conn, service); err != nil {
		return nil, err
	}
	if err = readStatus(conn); err != nil {
		return nil, err
	}
	return readLengthPayload(conn, maxHostPayload)
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	return c.dialer.DialContext(ctx, "tcp", c.address)
}

func setHandshakeDeadline(ctx context.Context, conn net.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	return ctx.Err()
}

func writeService(w io.Writer, service string) error {
	if len(service) > maxServiceSize {
		return errors.New("ADB smart-socket service is too long")
	}
	var prefix [4]byte
	copy(prefix[:], fmt.Sprintf("%04x", len(service)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, service)
	return err
}

func readStatus(conn net.Conn) error {
	var status [4]byte
	if _, err := io.ReadFull(conn, status[:]); err != nil {
		return err
	}
	switch string(status[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		message, err := readLengthPayload(conn, 4096)
		if err != nil {
			return errors.New("ADB server returned malformed failure response")
		}
		return fmt.Errorf("ADB server rejected request: %s", strings.TrimSpace(string(message)))
	default:
		return errors.New("ADB server returned an invalid status")
	}
}

func readLengthPayload(r io.Reader, max int) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	for _, digit := range header {
		if !(digit >= '0' && digit <= '9') && !(digit >= 'a' && digit <= 'f') &&
			!(digit >= 'A' && digit <= 'F') {
			return nil, errors.New("ADB server returned an invalid length")
		}
	}
	length, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil {
		return nil, errors.New("ADB server returned an invalid length")
	}
	if length > uint64(max) {
		return nil, errors.New("ADB server response exceeds size limit")
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

type cancelConn struct {
	net.Conn
	stop func() bool
	once sync.Once
}

func (c *cancelConn) Close() error {
	var err error
	c.once.Do(func() {
		c.stop()
		err = c.Conn.Close()
	})
	return err
}
