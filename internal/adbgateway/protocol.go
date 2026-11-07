package adbgateway

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

const (
	cmdCnxn         = 0x4e584e43
	cmdOpen         = 0x4e45504f
	cmdOkay         = 0x59414b4f
	cmdClse         = 0x45534c43
	cmdWrte         = 0x45545257
	cmdAuth         = 0x48545541
	protocolVersion = 0x01000001
	maxPacketSize   = 1024 * 1024
)

type packet struct {
	cmd, arg0, arg1 uint32
	data            []byte
	buffer          []byte
}

type payloadPool struct{ buffers chan []byte }

func newPayloadPool() *payloadPool { return &payloadPool{buffers: make(chan []byte, maxStreams+2)} }
func (p *payloadPool) take() []byte {
	select {
	case b := <-p.buffers:
		return b
	default:
		return make([]byte, maxPacketSize)
	}
}
func (p *payloadPool) put(b []byte) {
	if cap(b) < maxPacketSize {
		return
	}
	b = b[:maxPacketSize]
	select {
	case p.buffers <- b:
	default:
	}
}
func releasePacket(pool *payloadPool, p *packet) {
	if p.buffer != nil {
		pool.put(p.buffer)
		p.buffer = nil
		p.data = nil
	}
}

func checksum(p []byte) uint32 {
	var sum uint32
	for _, b := range p {
		sum += uint32(b)
	}
	return sum
}

func readPacket(r io.Reader, max uint32, skipChecksum bool) (packet, error) {
	return readPacketWithPool(r, max, skipChecksum, nil)
}

func readPacketWithPool(r io.Reader, max uint32, skipChecksum bool, pool *payloadPool) (packet, error) {
	var p packet
	var h [24]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return p, err
	}
	p.cmd = binary.LittleEndian.Uint32(h[0:4])
	p.arg0 = binary.LittleEndian.Uint32(h[4:8])
	p.arg1 = binary.LittleEndian.Uint32(h[8:12])
	n := binary.LittleEndian.Uint32(h[12:16])
	if n > max || n > maxPacketSize {
		return p, errors.New("ADB packet exceeds negotiated limit")
	}
	if binary.LittleEndian.Uint32(h[20:24]) != p.cmd^0xffffffff {
		return p, errors.New("invalid ADB packet magic")
	}
	if n != 0 {
		var body []byte
		if pool != nil {
			body = pool.take()
			p.buffer = body
		} else {
			body = make([]byte, int(n))
		}
		p.data = body[:n]
		if _, err := io.ReadFull(r, p.data); err != nil {
			releasePacket(pool, &p)
			return packet{}, err
		}
	}
	if !skipChecksum && !(p.cmd == cmdCnxn && p.arg0 >= 0x01000001) && checksum(p.data) != binary.LittleEndian.Uint32(h[16:20]) {
		releasePacket(pool, &p)
		return packet{}, errors.New("invalid ADB packet checksum")
	}
	return p, nil
}

func writePacket(w io.Writer, p packet, skipChecksum bool) error {
	if len(p.data) > maxPacketSize {
		return errors.New("ADB packet exceeds limit")
	}
	var h [24]byte
	binary.LittleEndian.PutUint32(h[0:4], p.cmd)
	binary.LittleEndian.PutUint32(h[4:8], p.arg0)
	binary.LittleEndian.PutUint32(h[8:12], p.arg1)
	binary.LittleEndian.PutUint32(h[12:16], uint32(len(p.data)))
	if !skipChecksum {
		binary.LittleEndian.PutUint32(h[16:20], checksum(p.data))
	}
	binary.LittleEndian.PutUint32(h[20:24], p.cmd^0xffffffff)
	if err := writeFull(w, h[:]); err != nil {
		return err
	}
	return writeFull(w, p.data)
}

func writeFull(w io.Writer, b []byte) error {
	for len(b) != 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func validService(s string) bool {
	if s == "sync:" {
		return true
	}
	if strings.HasPrefix(s, "exec:") {
		return len(s) > len("exec:") && !strings.ContainsRune(s, 0)
	}
	if s == "shell:" || strings.HasPrefix(s, "shell:") && !strings.ContainsRune(s, 0) {
		return true
	}
	if strings.HasPrefix(s, "shell,v2:") {
		return true
	}
	if strings.HasPrefix(s, "shell,v2,") {
		rest := strings.TrimPrefix(s, "shell,v2,")
		options, _, found := strings.Cut(rest, ":")
		if !found || options == "" {
			return false
		}
		pty, raw := false, false
		for _, option := range strings.Split(options, ",") {
			switch {
			case option == "pty":
				pty = true
			case option == "raw":
				raw = true
			case strings.HasPrefix(option, "TERM=") && len(option) > len("TERM="):
			default:
				return false
			}
		}
		return !(pty && raw)
	}
	return false
}

func advertisedFeatures(b []byte) map[string]bool {
	result := make(map[string]bool, 8)
	text := strings.TrimRight(string(b), "\x00")
	_, banner, found := strings.Cut(text, "::")
	if !found {
		return result
	}
	for _, field := range strings.Split(banner, ";") {
		if strings.HasPrefix(field, "features=") {
			for _, feature := range strings.Split(strings.TrimPrefix(field, "features="), ",") {
				if feature != "" {
					result[feature] = true
				}
			}
		}
	}
	return result
}

var supportedFeatures = [...]string{"shell_v2", "stat_v2", "ls_v2", "fixed_push_mkdir", "sendrecv_v2", "sendrecv_v2_brotli", "sendrecv_v2_lz4", "sendrecv_v2_zstd"}

// SupportedFeatures reports only target capabilities implemented by this gateway.
// The transport handshake intersects this list with the native host's features.
func SupportedFeatures(target []string) []string {
	features := make([]string, 0, len(supportedFeatures))
	for _, supported := range supportedFeatures {
		for _, advertised := range target {
			if supported == advertised {
				features = append(features, supported)
				break
			}
		}
	}
	return features
}

func negotiatedFeatures(target, host map[string]bool) []string {
	features := make([]string, 0, 8)
	for _, feature := range supportedFeatures {
		if target[feature] && host[feature] {
			features = append(features, feature)
		}
	}
	return features
}

func requireString(b []byte) (string, error) {
	if len(b) < 2 || b[len(b)-1] != 0 || bytes.IndexByte(b[:len(b)-1], 0) >= 0 {
		return "", errors.New("invalid ADB string")
	}
	return string(b[:len(b)-1]), nil
}
