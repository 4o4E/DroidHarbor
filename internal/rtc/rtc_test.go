package rtc

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

const validOffer = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 102\r\n" +
	"a=mid:0\r\n" +
	"a=recvonly\r\n" +
	"a=rtpmap:102 H264/90000\r\n" +
	"a=fmtp:102 profile-level-id=42001f;packetization-mode=1;level-asymmetry-allowed=1\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"a=mid:1\r\n" +
	"a=sctp-port:5000\r\n"

func TestValidateOfferRequiresSendOnlyCompatibleMedia(t *testing.T) {
	if err := validateOffer(validOffer); err != nil {
		t.Fatalf("valid offer rejected: %v", err)
	}
	if err := validateOffer(strings.ReplaceAll(validOffer, ";", "; ")); err != nil {
		t.Fatalf("valid spaced codec parameters rejected: %v", err)
	}

	cases := map[string]string{
		"video direction":              strings.Replace(validOffer, "a=recvonly", "a=sendonly", 1),
		"packetization mode":           strings.Replace(validOffer, "packetization-mode=1", "packetization-mode=0", 1),
		"baseline profile":             strings.Replace(validOffer, "profile-level-id=42001f", "profile-level-id=42e01f", 1),
		"audio section":                strings.Replace(validOffer, "m=application", "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\nm=application", 1),
		"missing data channel section": strings.Replace(validOffer, "m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n", "", 1),
	}
	for name, offer := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateOffer(offer); err == nil {
				t.Fatal("incompatible offer accepted")
			}
		})
	}
}

func TestListenAddressRequiresExplicitIPv4(t *testing.T) {
	ip, port, err := parseListenAddress("127.0.0.1:0")
	if err != nil || !ip.Equal(net.IPv4(127, 0, 0, 1)) || port != 0 {
		t.Fatalf("unexpected parsed listen address: %v, %d, %v", ip, port, err)
	}
	for _, address := range []string{"localhost:5555", "[::1]:5555", ":5555", "127.0.0.1:notaport", "224.0.0.1:5555", "255.255.255.255:5555"} {
		if _, _, err := parseListenAddress(address); err == nil {
			t.Errorf("accepted non-explicit unicast IPv4 listen address %q", address)
		}
	}
}

func TestAdvertisedAddressRequiresUnicastIPv4(t *testing.T) {
	ip, err := parseAdvertisedIP("192.0.2.10")
	if err != nil || !ip.Equal(net.IPv4(192, 0, 2, 10)) {
		t.Fatalf("unexpected parsed advertised address: %v, %v", ip, err)
	}
	for _, address := range []string{"", "localhost", "::1", "::ffff:192.0.2.10", "0.0.0.0", "224.0.0.1", "255.255.255.255"} {
		if _, err := parseAdvertisedIP(address); err == nil {
			t.Errorf("accepted non-explicit unicast IPv4 advertised address %q", address)
		}
	}
}

func TestLocalAnswerRejectsUnexpectedNetworkCandidates(t *testing.T) {
	answer := "v=0\r\n" +
		"a=candidate:1 1 udp 2130706431 192.0.2.10 5000 typ host\r\n"
	if got, err := constrainLocalCandidates(answer, net.ParseIP("192.0.2.10")); err != nil || got != answer {
		t.Fatalf("valid answer changed or rejected: %q, %v", got, err)
	}
	for _, extra := range []string{
		"a=candidate:2 1 udp 2130706430 198.51.100.20 5000 typ host\r\n",
		"a=candidate:3 1 tcp 2130706429 192.0.2.10 5000 typ host\r\n",
	} {
		if _, err := constrainLocalCandidates(answer+extra, net.ParseIP("192.0.2.10")); err == nil {
			t.Fatal("unexpected network candidate was silently hidden")
		}
	}
	if _, err := constrainLocalCandidates(answer, net.ParseIP("203.0.113.2")); err == nil {
		t.Fatal("answer without the configured advertised endpoint was accepted")
	}
}

func TestFramingHandlesSplitHeadersAndChunksWithoutAliasing(t *testing.T) {
	peer := testPeer()
	payload := make([]byte, 20_001)
	for i := range payload {
		payload[i] = byte(i)
	}
	wire := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(wire[:4], uint32(len(payload)))
	copy(wire[4:], payload)

	var messages [][]byte
	initialChunks := []int{1, 2, 1}
	for chunkIndex := 0; len(wire) > 0; chunkIndex++ {
		chunkSize := maxDataChannelChunk
		if chunkIndex < len(initialChunks) {
			chunkSize = initialChunks[chunkIndex]
		}
		if chunkSize > len(wire) {
			chunkSize = len(wire)
		}
		complete, err := peer.receiver.consume(wire[:chunkSize])
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, complete...)
		for i := range chunkSize {
			wire[i] = 0xff
		}
		wire = wire[chunkSize:]
	}
	if len(messages) != 1 || string(messages[0]) != string(payload) {
		t.Fatal("framer did not preserve a split logical message")
	}
}

func TestFramingHandlesMultipleMessagesAndRejectsInvalidLengths(t *testing.T) {
	peer := testPeer()
	first := []byte("first")
	second := []byte("second")
	wire := framedBytes(first, second)
	messages, err := peer.receiver.consume(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || string(messages[0]) != string(first) || string(messages[1]) != string(second) {
		t.Fatalf("unexpected framed messages: %q", messages)
	}

	for _, length := range []uint32{0, maxLogicalMessage + 1} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], length)
		if _, err := peer.receiver.consume(header[:]); err == nil {
			t.Errorf("accepted invalid logical message length %d", length)
		}
	}
}

func TestStringDataChannelMessageFailsPeer(t *testing.T) {
	peer := testPeer()
	peer.receiver.receive(webrtc.DataChannelMessage{IsString: true, Data: []byte("not binary")})
	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("string DataChannel message did not terminate the peer")
	}
	if peer.closeError() == nil {
		t.Fatal("string DataChannel failure was not reported")
	}
}

func TestControlCallbackOwnsMessageAndMayClosePeer(t *testing.T) {
	peer := testPeer()
	want := []byte("logical-message")
	wire := framedBytes(want)
	delivered := make(chan []byte, 1)
	peer.callbacks.Control = func(message []byte) {
		delivered <- message
		_ = peer.Close()
	}
	finished := make(chan struct{})
	go func() {
		peer.receiver.receive(webrtc.DataChannelMessage{Data: wire})
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Control callback deadlocked while closing its peer")
	}
	for i := range wire {
		wire[i] = 0xff
	}
	select {
	case got := <-delivered:
		if string(got) != string(want) {
			t.Fatalf("callback data was not owned: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("complete logical message was not delivered")
	}
}

func TestIncompleteFrameTimeoutClosesPeer(t *testing.T) {
	peer := testPeer()
	if _, err := peer.receiver.consume([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	peer.receiver.mu.Lock()
	timer := peer.receiver.timer
	if timer == nil {
		peer.receiver.mu.Unlock()
		t.Fatal("incomplete frame timer was not started")
	}
	timer.Reset(0)
	peer.receiver.mu.Unlock()

	select {
	case <-peer.Done():
	case <-time.After(time.Second):
		t.Fatal("incomplete frame timeout did not terminate the peer")
	}
	if peer.closeError() == nil {
		t.Fatal("incomplete frame timeout was not reported")
	}
}

func TestCloseCallbackMayClosePeerAndRunsExactlyOnce(t *testing.T) {
	peer := testPeer()
	closedCalls := 0
	peer.callbacks.Closed = func(error) {
		closedCalls++
		_ = peer.Close()
	}

	closed := make(chan error, 1)
	go func() { closed <- peer.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close deadlocked when Closed callback closed the peer")
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("repeated close returned an error: %v", err)
	}
	if closedCalls != 1 {
		t.Fatalf("Closed callback ran %d times, want once", closedCalls)
	}
	select {
	case <-peer.Done():
	default:
		t.Fatal("Done was not closed")
	}
}

func testPeer() *Peer {
	peer := &Peer{done: make(chan struct{}), closeComplete: make(chan struct{}), callbacksActive: true}
	peer.receiver.peer = peer
	return peer
}

func framedBytes(messages ...[]byte) []byte {
	var wire []byte
	for _, message := range messages {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(message)))
		wire = append(wire, header[:]...)
		wire = append(wire, message...)
	}
	return wire
}

func TestReadyBeforeCallbackActivationIsDeliveredOnce(t *testing.T) {
	peer := testPeer()
	peer.callbacksActive = false
	peer.connected = true
	peer.channel = &webrtc.DataChannel{}
	calls := 0
	peer.callbacks.Ready = func() { calls++; _ = peer.Close() }
	peer.markChannelOpen(peer.channel)
	if calls != 0 {
		t.Fatal("readiness escaped before callback activation")
	}
	if !peer.enableCallbacks() || calls != 1 {
		t.Fatalf("early readiness was lost: callback calls=%d", calls)
	}
	peer.markChannelOpen(peer.channel)
	if calls != 1 {
		t.Fatal("readiness was delivered more than once")
	}
}
