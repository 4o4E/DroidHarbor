package adbgateway

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadPacketRejectsBadMagicChecksumAndLength(t *testing.T) {
	for name, mutate := range map[string]func([]byte){
		"magic":    func(b []byte) { b[20] ^= 1 },
		"checksum": func(b []byte) { b[16] ^= 1 },
		"length":   func(b []byte) { binary.LittleEndian.PutUint32(b[12:16], maxPacketSize+1) },
	} {
		t.Run(name, func(t *testing.T) {
			var wire [25]byte
			binary.LittleEndian.PutUint32(wire[0:4], cmdWrte)
			binary.LittleEndian.PutUint32(wire[12:16], 1)
			binary.LittleEndian.PutUint32(wire[16:20], 0x41)
			binary.LittleEndian.PutUint32(wire[20:24], cmdWrte^0xffffffff)
			wire[24] = 0x41
			mutate(wire[:])
			if _, err := readPacket(&sliceReader{b: wire[:]}, maxPacketSize, false); err == nil {
				t.Fatal("malformed packet accepted")
			}
		})
	}
}

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, net.ErrClosed
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestParsePublicKeyRejectsNonADBEncoding(t *testing.T) {
	if _, err := ParsePublicKey("not-a-standard-adb-public-key"); err == nil {
		t.Fatal("invalid ADB key accepted")
	}
	if _, err := ParsePublicKey("-----BEGIN PUBLIC KEY-----"); err == nil {
		t.Fatal("X.509 key encoding accepted")
	}
}

func TestServeRejectsWrongADBSignatureWithoutOpeningService(t *testing.T) {
	registered, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer client.Close()
	opened := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), server, Session{
			PublicKey: &registered.PublicKey,
			Check:     func() error { return nil },
			Open:      func(context.Context, string) (net.Conn, error) { opened <- struct{}{}; return nil, nil },
		})
	}()
	if err := writePacket(client, packet{cmd: cmdCnxn, arg0: protocolVersion, arg1: maxPacketSize, data: []byte("host::features=shell_v2;\x00")}, false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		challenge, err := readPacket(client, maxPacketSize, true)
		if err != nil || challenge.cmd != cmdAuth || challenge.arg0 != 1 {
			t.Fatal("missing ADB challenge")
		}
		signature, err := rsa.SignPKCS1v15(rand.Reader, attacker, crypto.SHA1, challenge.data)
		if err != nil {
			t.Fatal(err)
		}
		if err := writePacket(client, packet{cmd: cmdAuth, arg0: 2, data: signature}, true); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wrong key unexpectedly authenticated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed authentication did not terminate")
	}
	select {
	case <-opened:
		t.Fatal("untrusted key opened an upstream service")
	default:
	}
}

func TestADBFeatureAdvertisementIsIntersection(t *testing.T) {
	features := advertisedFeatures([]byte("host::features=shell_v2,cmd;\x00"))
	if !features["shell_v2"] || features["sync"] || features["tcpip"] {
		t.Fatalf("unexpected features: %#v", features)
	}
	if !validService("shell:") || !validService("shell,v2:") || !validService("shell,v2,pty:") || !validService("shell,v2,TERM=x,raw:id") || validService("shell,v2,pty,raw:id") || validService("shell,v2,unknown:id") {
		t.Fatal("shell service validation failed")
	}
	got := negotiatedFeatures(
		map[string]bool{"shell_v2": true, "stat_v2": true, "sendrecv_v2_zstd": true, "sendrecv_v2_lz4": true, "delayed_ack": true},
		map[string]bool{"shell_v2": true, "stat_v2": true, "sendrecv_v2_zstd": true, "ls_v2": true, "delayed_ack": true},
	)
	want := []string{"shell_v2", "stat_v2", "sendrecv_v2_zstd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feature intersection = %v, want %v", got, want)
	}
}
func TestParsePublicKeyAcceptsAndroidRSAPublicKeyStructure(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const words = 64
	wire := make([]byte, 4+4+words*4+words*4+4)
	binary.LittleEndian.PutUint32(wire[:4], words)
	n := private.N.Bytes()
	for i := range words {
		binary.LittleEndian.PutUint32(wire[8+i*4:], binary.BigEndian.Uint32(n[len(n)-4*(i+1):len(n)-4*i]))
	}
	radix := new(big.Int).Lsh(big.NewInt(1), 2048)
	rr := new(big.Int).Mul(radix, radix)
	rr.Mod(rr, private.N)
	rrBytes := rr.Bytes()
	rrPadded := make([]byte, words*4)
	copy(rrPadded[len(rrPadded)-len(rrBytes):], rrBytes)
	for i := range words {
		word := binary.BigEndian.Uint32(rrPadded[len(rrPadded)-4*(i+1) : len(rrPadded)-4*i])
		binary.LittleEndian.PutUint32(wire[8+words*4+i*4:], word)
	}
	binary.LittleEndian.PutUint32(wire[len(wire)-4:], 65537)
	radix32 := new(big.Int).Lsh(big.NewInt(1), 32)
	inv := new(big.Int).ModInverse(new(big.Int).Mod(private.N, radix32), radix32)
	n0 := new(big.Int).Neg(inv)
	n0.Mod(n0, radix32)
	binary.LittleEndian.PutUint32(wire[4:8], uint32(n0.Uint64()))
	parsed, err := ParsePublicKey(base64.StdEncoding.EncodeToString(wire) + " test@example")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.E != private.PublicKey.E || parsed.N.Cmp(private.N) != 0 {
		t.Fatal("parsed key differs from Android public key")
	}
}
func TestServeUsesPerStreamWriteAcknowledgementAndCancelsUpstream(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, client := net.Pipe()
	upstream, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, server, Session{
			PublicKey: &key.PublicKey,
			Features:  []string{"shell_v2"},
			Check:     func() error { return nil },
			Open:      func(context.Context, string) (net.Conn, error) { return upstream, nil },
		})
	}()
	if err := writePacket(client, packet{cmd: cmdCnxn, arg0: protocolVersion, arg1: 4096, data: []byte("host::features=shell_v2;\x00")}, false); err != nil {
		t.Fatal(err)
	}
	challenge, err := readPacket(client, maxPacketSize, true)
	if err != nil || challenge.cmd != cmdAuth {
		t.Fatal("missing ADB challenge")
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, challenge.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePacket(client, packet{cmd: cmdAuth, arg0: 2, data: sig}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := readPacket(client, maxPacketSize, true); err != nil {
		t.Fatal(err)
	}
	if err := writePacket(client, packet{cmd: cmdOpen, arg0: 7, data: []byte("shell,v2,raw:id\x00")}, true); err != nil {
		t.Fatal(err)
	}
	okay, err := readPacket(client, maxPacketSize, true)
	if err != nil || okay.cmd != cmdOkay || okay.arg1 != 7 {
		t.Fatal("stream did not open with the expected identifiers")
	}
	if err := writePacket(client, packet{cmd: cmdWrte, arg0: 7, arg1: okay.arg0, data: []byte("input-one")}, true); err != nil {
		t.Fatal(err)
	}
	readUpstream := make([]byte, 32)
	n, err := peer.Read(readUpstream)
	if err != nil || string(readUpstream[:n]) != "input-one" {
		t.Fatal("host WRTE was not forwarded to the upstream service")
	}
	inputAck, err := readPacket(client, maxPacketSize, true)
	if err != nil || inputAck.cmd != cmdOkay || inputAck.arg0 != okay.arg0 || inputAck.arg1 != 7 {
		t.Fatal("upstream write was not acknowledged with the correct stream IDs")
	}
	if err := writePacket(client, packet{cmd: cmdWrte, arg0: 7, arg1: okay.arg0, data: []byte("input-two")}, true); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := readPacket(client, maxPacketSize, true); err == nil {
		t.Fatal("host WRTE was acknowledged before the upstream accepted it")
	}
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	n, err = peer.Read(readUpstream)
	if err != nil || string(readUpstream[:n]) != "input-two" {
		t.Fatal("second host WRTE was not forwarded")
	}
	inputAck, err = readPacket(client, maxPacketSize, true)
	if err != nil || inputAck.cmd != cmdOkay {
		t.Fatal("second upstream write was not acknowledged")
	}
	if _, err := peer.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	first, err := readPacket(client, 4096, true)
	if err != nil || first.cmd != cmdWrte || first.arg0 != okay.arg0 || first.arg1 != 7 || string(first.data) != "one" {
		t.Fatal("first upstream write was not forwarded")
	}
	secondWrite := make(chan error, 1)
	go func() { _, err := peer.Write([]byte("two")); secondWrite <- err }()
	select {
	case <-secondWrite:
		t.Fatal("upstream data advanced without an ADB OKAY")
	case <-time.After(25 * time.Millisecond):
	}
	if err := writePacket(client, packet{cmd: cmdOkay, arg0: 7, arg1: okay.arg0}, true); err != nil {
		t.Fatal(err)
	}
	second, err := readPacket(client, 4096, true)
	if err != nil || second.cmd != cmdWrte || string(second.data) != "two" {
		t.Fatal("ADB OKAY did not release the next upstream write")
	}
	thirdWrite := make(chan error, 1)
	go func() { _, err := peer.Write([]byte("three")); thirdWrite <- err }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("transport cancellation did not terminate Serve")
	}
	select {
	case err := <-thirdWrite:
		if err == nil {
			t.Fatal("upstream remained open after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation left upstream write blocked")
	}
}

type ackGateConn struct {
	net.Conn
	armed       atomic.Bool
	releaseOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (c *ackGateConn) Write(p []byte) (int, error) {
	hold := len(p) == 24 && binary.LittleEndian.Uint32(p[:4]) == cmdOkay && c.armed.CompareAndSwap(true, false)
	n, err := c.Conn.Write(p)
	if err == nil && hold {
		close(c.entered)
		<-c.release
	}
	return n, err
}
func (c *ackGateConn) unblock() { c.releaseOnce.Do(func() { close(c.release) }) }

func TestInputAckReleasesFlowBeforeDownstreamWriteReturns(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rawServer, client := net.Pipe()
	gate := &ackGateConn{Conn: rawServer, entered: make(chan struct{}), release: make(chan struct{})}
	upstream, peer := net.Pipe()
	defer gate.unblock()
	defer client.Close()
	defer peer.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, gate, Session{
			PublicKey: &key.PublicKey,
			Features:  []string{"shell_v2"},
			Check:     func() error { return nil },
			Open: func(openCtx context.Context, service string) (net.Conn, error) {
				if service == "exec:barrier" {
					close(barrier)
					<-openCtx.Done()
					return nil, openCtx.Err()
				}
				return upstream, nil
			},
		})
	}()
	if err := writePacket(client, packet{cmd: cmdCnxn, arg0: protocolVersion, arg1: 4096, data: []byte("host::features=shell_v2;\x00")}, false); err != nil {
		t.Fatal(err)
	}
	challenge, err := readPacket(client, maxPacketSize, true)
	if err != nil || challenge.cmd != cmdAuth {
		t.Fatal("missing ADB challenge")
	}
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, challenge.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePacket(client, packet{cmd: cmdAuth, arg0: 2, data: signature}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := readPacket(client, maxPacketSize, true); err != nil {
		t.Fatal(err)
	}
	if err := writePacket(client, packet{cmd: cmdOpen, arg0: 9, data: []byte("shell,v2,raw:id\x00")}, true); err != nil {
		t.Fatal(err)
	}
	okay, err := readPacket(client, maxPacketSize, true)
	if err != nil || okay.cmd != cmdOkay {
		t.Fatal("stream did not open")
	}

	gate.armed.Store(true)
	if err := writePacket(client, packet{cmd: cmdWrte, arg0: 9, arg1: okay.arg0, data: []byte("one")}, true); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	n, err := peer.Read(buf)
	if err != nil || string(buf[:n]) != "one" {
		t.Fatal("first WRTE was not forwarded")
	}
	firstAck, err := readPacket(client, maxPacketSize, true)
	if err != nil || firstAck.cmd != cmdOkay {
		t.Fatal("first WRTE was not acknowledged")
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("downstream ACK write did not enter its controlled boundary")
	}
	if err := writePacket(client, packet{cmd: cmdWrte, arg0: 9, arg1: okay.arg0, data: []byte("two")}, true); err != nil {
		t.Fatal(err)
	}
	if err := writePacket(client, packet{cmd: cmdOpen, arg0: 10, data: []byte("exec:barrier\x00")}, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-barrier:
	case <-done:
		t.Fatal("transport rejected the next legal WRTE")
	case <-time.After(5 * time.Second):
		t.Fatal("next WRTE prevented another stream from opening")
	}
	gate.unblock()
	n, err = peer.Read(buf)
	if err != nil || string(buf[:n]) != "two" {
		t.Fatal("next legal WRTE was rejected before the prior Write returned")
	}
	secondAck, err := readPacket(client, maxPacketSize, true)
	if err != nil || secondAck.cmd != cmdOkay {
		t.Fatal("second WRTE was not acknowledged")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not terminate after cancellation")
	}
}

func TestCanceledOpenRetainsCapacityUntilBackendReturns(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, client := net.Pipe()
	release := make(chan struct{})
	opened := make(chan struct{}, maxStreams+1)
	canceled := make(chan struct{}, maxStreams+1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, server, Session{
			PublicKey: &key.PublicKey,
			Check:     func() error { return nil },
			Open: func(openCtx context.Context, _ string) (net.Conn, error) {
				opened <- struct{}{}
				<-openCtx.Done()
				canceled <- struct{}{}
				<-release
				return nil, openCtx.Err()
			},
		})
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("pending Open workers survived transport cancellation")
		}
	})
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := writePacket(client, packet{cmd: cmdCnxn, arg0: protocolVersion, arg1: 4096, data: []byte("host::features=;\x00")}, false); err != nil {
		t.Fatal(err)
	}
	challenge, err := readPacket(client, maxPacketSize, true)
	if err != nil || challenge.cmd != cmdAuth {
		t.Fatal("missing ADB challenge")
	}
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, challenge.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePacket(client, packet{cmd: cmdAuth, arg0: 2, data: signature}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := readPacket(client, maxPacketSize, true); err != nil {
		t.Fatal(err)
	}
	for id := uint32(1); id <= maxStreams; id++ {
		if err := writePacket(client, packet{cmd: cmdOpen, arg0: id, data: []byte("exec:blocked\x00")}, true); err != nil {
			t.Fatal(err)
		}
		select {
		case <-opened:
		case <-time.After(time.Second):
			t.Fatal("backend Open did not start")
		}
		if err := writePacket(client, packet{cmd: cmdClse, arg0: id}, true); err != nil {
			t.Fatal(err)
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("peer CLSE did not cancel the pending Open")
		}
	}
	if err := writePacket(client, packet{cmd: cmdOpen, arg0: maxStreams + 1, data: []byte("exec:must-not-open\x00")}, true); err != nil {
		t.Fatal(err)
	}
	rejected, err := readPacket(client, maxPacketSize, true)
	if err != nil || rejected.cmd != cmdClse || rejected.arg0 != 0 || rejected.arg1 != maxStreams+1 {
		t.Fatalf("closing workers did not retain the stream capacity: packet=%+v err=%v", rejected, err)
	}
	select {
	case <-opened:
		t.Fatal("rejected stream reached the backend while closing workers retained capacity")
	default:
	}
}
