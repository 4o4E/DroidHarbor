package rtc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const (
	controlChannelLabel = "droidharbor.control.v1"
	h264Fmtp            = "profile-level-id=42001f;packetization-mode=1;level-asymmetry-allowed=1"
)

var h264Feedback = []webrtc.RTCPFeedback{
	{Type: "nack"},
	{Type: "nack", Parameter: "pli"},
	{Type: "ccm", Parameter: "fir"},
}

type Config struct {
	ListenAddress string
	AdvertisedIP  string // Optional candidate-address override; empty gathers local IPv4 addresses.
}

type Callbacks struct {
	Control           func([]byte)
	Ready             func()
	Closed            func(error)
	KeyFrameRequested func()
}

type Endpoint struct {
	mu         sync.Mutex
	closed     bool
	mux        *ice.UDPMuxDefault
	api        *webrtc.API
	advertised net.IP
	peers      map[*Peer]struct{}
	acceptWG   sync.WaitGroup
}

func New(config Config) (*Endpoint, error) {
	listenIP, port, err := parseListenAddress(config.ListenAddress)
	if err != nil {
		return nil, err
	}
	var advertised net.IP
	if config.AdvertisedIP != "" {
		advertised, err = parseAdvertisedIP(config.AdvertisedIP)
		if err != nil {
			return nil, err
		}
		if advertised.IsLoopback() && !listenIP.IsLoopback() && !listenIP.IsUnspecified() {
			return nil, errors.New("a loopback advertised IP requires a loopback or wildcard listener")
		}
		if listenIP.IsLoopback() && !advertised.IsLoopback() {
			return nil, errors.New("a loopback listener requires a loopback advertised IP")
		}
	}

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: listenIP, Port: port})
	if err != nil {
		return nil, fmt.Errorf("listen for WebRTC UDP: %w", err)
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: listener})

	media := &webrtc.MediaEngine{}
	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  h264Fmtp,
			RTCPFeedback: append([]webrtc.RTCPFeedback(nil), h264Feedback...),
		},
		PayloadType: 102,
	}
	if err = media.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
		_ = mux.Close()
		return nil, fmt.Errorf("register H.264 codec: %w", err)
	}
	registry := &interceptor.Registry{}
	if err = webrtc.RegisterDefaultInterceptors(media, registry); err != nil {
		_ = mux.Close()
		return nil, fmt.Errorf("register WebRTC interceptors: %w", err)
	}

	settings := webrtc.SettingEngine{}
	settings.SetICEUDPMux(mux)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	if advertised != nil {
		if err = settings.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{advertised.String()},
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
			Networks:        []webrtc.NetworkType{webrtc.NetworkTypeUDP4},
		}); err != nil {
			_ = mux.Close()
			return nil, fmt.Errorf("configure advertised ICE address: %w", err)
		}
	}
	settings.SetIncludeLoopbackCandidate(advertised == nil || advertised.IsLoopback())
	if listenIP.IsLoopback() || advertised.IsLoopback() {
		settings.SetRemoteIPFilter(func(ip net.IP) bool { return ip.To4() != nil && ip.IsLoopback() })
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(media),
		webrtc.WithInterceptorRegistry(registry),
		webrtc.WithSettingEngine(settings),
	)

	return &Endpoint{
		mux:        mux,
		api:        api,
		advertised: advertised,
		peers:      make(map[*Peer]struct{}),
	}, nil
}

func parseListenAddress(address string) (net.IP, int, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid explicit UDP listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if strings.Contains(host, ":") || ip == nil || ip.To4() == nil || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return nil, 0, errors.New("listen address must contain an explicit unicast IPv4 address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, 0, errors.New("listen address must contain a valid UDP port")
	}
	return append(net.IP(nil), ip.To4()...), port, nil
}

func parseAdvertisedIP(address string) (net.IP, error) {
	ip := net.ParseIP(address)
	if strings.Contains(address, ":") || ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return nil, errors.New("advertised IP must be an explicit unicast IPv4 address")
	}
	return append(net.IP(nil), ip.To4()...), nil
}

func (e *Endpoint) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	peers := make([]*Peer, 0, len(e.peers))
	for peer := range e.peers {
		peers = append(peers, peer)
	}
	e.mu.Unlock()

	var closeErr error
	for _, peer := range peers {
		_ = peer.Close()
		<-peer.closeComplete
		closeErr = errors.Join(closeErr, peer.shutdownError())
	}
	e.acceptWG.Wait()
	closeErr = errors.Join(closeErr, e.mux.Close())
	return closeErr
}

func (e *Endpoint) register(peer *Peer) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("WebRTC endpoint is closed")
	}
	e.peers[peer] = struct{}{}
	return nil
}

func (e *Endpoint) unregister(peer *Peer) {
	e.mu.Lock()
	delete(e.peers, peer)
	e.mu.Unlock()
}

func (e *Endpoint) Accept(ctx context.Context, offer string, callbacks Callbacks) (*Peer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateOffer(offer); err != nil {
		return nil, err
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("WebRTC endpoint is closed")
	}
	e.acceptWG.Add(1)
	e.mu.Unlock()
	defer e.acceptWG.Done()

	pc, err := e.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("create WebRTC peer: %w", err)
	}
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:     webrtc.MimeTypeH264,
		ClockRate:    90000,
		SDPFmtpLine:  h264Fmtp,
		RTCPFeedback: append([]webrtc.RTCPFeedback(nil), h264Feedback...),
	}, "video", "droidharbor")
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create H.264 RTP track: %w", err)
	}
	transceiver, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add send-only H.264 track: %w", err)
	}
	peer := newPeer(e, pc, track, transceiver.Sender(), callbacks)
	if err = e.register(peer); err != nil {
		_ = peer.Close()
		return nil, err
	}
	peer.installHandlers()

	if err = ctx.Err(); err == nil {
		err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer})
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		} else {
			err = fmt.Errorf("set WebRTC offer: %w", err)
		}
		peer.fail(err)
		return nil, err
	}
	if err = ctx.Err(); err == nil {
		var answer webrtc.SessionDescription
		answer, err = pc.CreateAnswer(nil)
		if err == nil {
			gathered := webrtc.GatheringCompletePromise(pc)
			err = pc.SetLocalDescription(answer)
			if err == nil {
				select {
				case <-ctx.Done():
					err = ctx.Err()
				case <-peer.Done():
					err = peer.closeError()
					if err == nil {
						err = errors.New("WebRTC peer closed while gathering candidates")
					}
				case <-gathered:
				}
			}
		}
	}
	if err != nil {
		err = fmt.Errorf("complete WebRTC answer: %w", err)
		peer.fail(err)
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		peer.fail(err)
		return nil, err
	}
	local := pc.LocalDescription()
	if local == nil {
		err = errors.New("Pion produced no local WebRTC answer")
		peer.fail(err)
		return nil, err
	}
	answer, err := constrainLocalCandidates(local.SDP, e.advertised)
	if err != nil {
		peer.fail(err)
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		peer.fail(err)
		return nil, err
	}
	peer.answer = answer
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed || !peer.enableCallbacks() {
		_ = peer.Close()
		return nil, ErrPeerClosed
	}
	return peer, nil
}

func validateOffer(offer string) error {
	if strings.TrimSpace(offer) == "" {
		return errors.New("WebRTC offer is empty")
	}
	var description sdp.SessionDescription
	if err := description.UnmarshalString(offer); err != nil {
		return fmt.Errorf("parse WebRTC offer: %w", err)
	}
	var video, application *sdp.MediaDescription
	for _, media := range description.MediaDescriptions {
		if media.MediaName.Port.Value == 0 {
			return fmt.Errorf("rejected %s media sections are not supported", media.MediaName.Media)
		}
		switch media.MediaName.Media {
		case "video":
			if video != nil {
				return errors.New("WebRTC offer must contain exactly one video section")
			}
			video = media
		case "application":
			if application != nil {
				return errors.New("WebRTC offer must contain exactly one DataChannel section")
			}
			application = media
		default:
			return fmt.Errorf("unsupported WebRTC offer media section %q", media.MediaName.Media)
		}
	}
	if video == nil || application == nil {
		return errors.New("WebRTC offer must contain one video section and one DataChannel section")
	}
	if direction := effectiveDirection(description.Attributes, video.Attributes); direction != "recvonly" {
		return fmt.Errorf("WebRTC video offer direction must be recvonly, got %q", direction)
	}
	if !hasCompatibleH264Payload(video) {
		return errors.New("WebRTC video offer does not include packetization-mode=1 H.264")
	}
	return nil
}

func effectiveDirection(session, media []sdp.Attribute) string {
	if direction := mediaDirection(media); direction != "" {
		return direction
	}
	if direction := mediaDirection(session); direction != "" {
		return direction
	}
	return "sendrecv"
}

func mediaDirection(attributes []sdp.Attribute) string {
	for _, attribute := range attributes {
		switch attribute.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			return attribute.Key
		}
	}
	return ""
}

func hasCompatibleH264Payload(media *sdp.MediaDescription) bool {
	formats := make(map[string]struct{}, len(media.MediaName.Formats))
	for _, payload := range media.MediaName.Formats {
		formats[payload] = struct{}{}
	}
	h264 := make(map[string]bool)
	fmtps := make(map[string]string)
	for _, attribute := range media.Attributes {
		switch attribute.Key {
		case "rtpmap":
			fields := strings.Fields(attribute.Value)
			if len(fields) != 2 {
				continue
			}
			codec := strings.SplitN(fields[1], "/", 2)
			_, offered := formats[fields[0]]
			if len(codec) == 2 && offered && strings.EqualFold(codec[0], "H264") && codec[1] == "90000" {
				h264[fields[0]] = true
			}
		case "fmtp":
			payload, parameters, ok := strings.Cut(attribute.Value, " ")
			if ok {
				fmtps[payload] = strings.TrimSpace(parameters)
			}
		}
	}
	for payload := range h264 {
		parameters := make(map[string]string)
		for _, parameter := range strings.Split(fmtps[payload], ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok {
				parameters[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
			}
		}
		profile := strings.ToLower(parameters["profile-level-id"])
		_, profileErr := strconv.ParseUint(profile, 16, 32)
		if parameters["packetization-mode"] == "1" && len(profile) == 6 && strings.HasPrefix(profile, "4200") && profileErr == nil {
			return true
		}
	}
	return false
}

func constrainLocalCandidates(answer string, advertised net.IP) (string, error) {
	found := false
	for line := range strings.SplitSeq(answer, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "a="))
		if len(fields) < 8 || !strings.EqualFold(fields[2], "udp") || fields[6] != "typ" || fields[7] != "host" {
			return "", errors.New("Pion gathered a candidate outside the configured UDP host endpoint")
		}
		ip := net.ParseIP(fields[4])
		if ip == nil || ip.To4() == nil || strings.Contains(fields[4], ":") || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) || (advertised != nil && !advertised.Equal(ip)) {
			return "", errors.New("Pion gathered a candidate outside the configured UDP host endpoint")
		}
		found = true
	}
	if !found {
		return "", errors.New("Pion gathered no usable UDP4 host candidate")
	}
	return answer, nil
}
