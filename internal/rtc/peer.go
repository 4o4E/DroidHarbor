package rtc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var (
	ErrPeerClosed          = errors.New("WebRTC peer is closed")
	ErrPeerNotReady        = errors.New("WebRTC peer is not ready")
	ErrControlBackpressure = errors.New("WebRTC control channel backpressure limit exceeded")
	ErrInvalidControl      = errors.New("invalid WebRTC control message length")
)

type Peer struct {
	endpoint  *Endpoint
	pc        *webrtc.PeerConnection
	track     *webrtc.TrackLocalStaticRTP
	sender    *webrtc.RTPSender
	callbacks Callbacks

	mu              sync.Mutex
	closed          bool
	closeErr        error
	shutdownErr     error
	connected       bool
	channel         *webrtc.DataChannel
	channelOpen     bool
	ready           bool
	readyDelivered  bool
	callbacksActive bool
	done            chan struct{}
	closeComplete   chan struct{}
	answer          string

	sendMu   sync.Mutex
	receiver framedReceiver
}

func newPeer(endpoint *Endpoint, pc *webrtc.PeerConnection, track *webrtc.TrackLocalStaticRTP, sender *webrtc.RTPSender, callbacks Callbacks) *Peer {
	peer := &Peer{
		endpoint:      endpoint,
		pc:            pc,
		track:         track,
		sender:        sender,
		callbacks:     callbacks,
		done:          make(chan struct{}),
		closeComplete: make(chan struct{}),
	}
	peer.receiver.peer = peer
	return peer
}

func (p *Peer) installHandlers() {
	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			p.mu.Lock()
			if !p.closed {
				p.connected = true
			}
			ready := p.markReadyLocked()
			p.mu.Unlock()
			if ready {
				if callback := p.readyCallback(); callback != nil {
					callback()
				}
			}
		case webrtc.PeerConnectionStateDisconnected:
			p.fail(errors.New("WebRTC peer disconnected"))
		case webrtc.PeerConnectionStateFailed:
			p.fail(errors.New("WebRTC peer connection failed"))
		case webrtc.PeerConnectionStateClosed:
			p.terminate(nil)
		}
	})
	p.pc.OnDataChannel(p.acceptDataChannel)
	go p.drainRTCP()
}

func (p *Peer) acceptDataChannel(channel *webrtc.DataChannel) {
	if channel.Label() != controlChannelLabel {
		_ = channel.Close()
		p.fail(fmt.Errorf("unexpected WebRTC DataChannel label %q", channel.Label()))
		return
	}
	if !channel.Ordered() || channel.MaxRetransmits() != nil || channel.MaxPacketLifeTime() != nil {
		_ = channel.Close()
		p.fail(errors.New("WebRTC control DataChannel must be reliable and ordered"))
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = channel.Close()
		return
	}
	if p.channel != nil {
		p.mu.Unlock()
		_ = channel.Close()
		p.fail(errors.New("duplicate WebRTC control DataChannel"))
		return
	}
	p.channel = channel
	p.mu.Unlock()

	channel.OnMessage(p.receiver.receive)
	channel.OnClose(func() { p.fail(errors.New("WebRTC control DataChannel closed")) })
	channel.OnOpen(func() { p.markChannelOpen(channel) })
	if channel.ReadyState() == webrtc.DataChannelStateOpen {
		p.markChannelOpen(channel)
	}
}

func (p *Peer) markChannelOpen(channel *webrtc.DataChannel) {
	p.mu.Lock()
	if !p.closed && p.channel == channel {
		p.channelOpen = true
	}
	ready := p.markReadyLocked()
	p.mu.Unlock()
	if ready {
		if callback := p.readyCallback(); callback != nil {
			callback()
		}
	}
}

func (p *Peer) markReadyLocked() bool {
	if !p.closed && !p.ready && p.connected && p.channel != nil && p.channelOpen {
		p.ready = true
		return true
	}
	return false
}

func (p *Peer) drainRTCP() {
	buffer := make([]byte, 1500)
	for {
		n, _, err := p.sender.Read(buffer)
		if err != nil {
			if !p.isClosed() {
				p.fail(fmt.Errorf("read WebRTC RTCP: %w", err))
			}
			return
		}
		packets, err := rtcp.Unmarshal(buffer[:n])
		if err != nil {
			p.fail(fmt.Errorf("parse WebRTC RTCP: %w", err))
			return
		}
		for _, packet := range packets {
			switch packet.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if callback := p.keyFrameCallback(); callback != nil {
					callback()
				}
			}
		}
	}
}

func (p *Peer) Answer() string { return p.answer }

func (p *Peer) AddCandidate(candidate webrtc.ICECandidateInit) error {
	if p.isClosed() {
		return ErrPeerClosed
	}
	if err := p.pc.AddICECandidate(candidate); err != nil {
		wrapped := fmt.Errorf("add remote ICE candidate: %w", err)
		p.fail(wrapped)
		return wrapped
	}
	return nil
}

func (p *Peer) WriteRTP(packet *rtp.Packet) error {
	if packet == nil {
		return errors.New("nil RTP packet")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrPeerClosed
	}
	ready := p.ready
	p.mu.Unlock()
	if !ready {
		return ErrPeerNotReady
	}
	if err := p.track.WriteRTP(packet); err != nil {
		wrapped := fmt.Errorf("write WebRTC RTP packet: %w", err)
		p.fail(wrapped)
		return wrapped
	}
	return nil
}

func (p *Peer) SendControl(message []byte) (err error) {
	if len(message) == 0 || len(message) > maxLogicalMessage {
		return ErrInvalidControl
	}
	p.sendMu.Lock()
	defer func() {
		p.sendMu.Unlock()
		if err != nil && !errors.Is(err, ErrPeerClosed) && !errors.Is(err, ErrPeerNotReady) {
			p.fail(err)
		}
	}()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrPeerClosed
	}
	channel := p.channel
	ready := p.ready
	p.mu.Unlock()
	if !ready || channel == nil || channel.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrPeerNotReady
	}
	if channel.BufferedAmount()+uint64(len(message))+4 > maxBufferedDataChannelBytes {
		return ErrControlBackpressure
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(message)))
	if err := p.sendChunk(channel, header[:]); err != nil {
		return err
	}
	for offset := 0; offset < len(message); {
		end := offset + maxDataChannelChunk
		if end > len(message) {
			end = len(message)
		}
		if err := p.sendChunk(channel, message[offset:end]); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func (p *Peer) sendChunk(channel *webrtc.DataChannel, chunk []byte) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrPeerClosed
	}
	if channel.BufferedAmount()+uint64(len(chunk)) > maxBufferedDataChannelBytes {
		return ErrControlBackpressure
	}
	if err := channel.Send(chunk); err != nil {
		wrapped := fmt.Errorf("send WebRTC control chunk: %w", err)
		return wrapped
	}
	return nil
}

func (p *Peer) Close() error { return p.terminate(nil) }

func (p *Peer) terminate(cause error) error {
	p.mu.Lock()
	if p.closed {
		err := p.shutdownErr
		p.mu.Unlock()
		return err
	}
	p.closed = true
	pc := p.pc
	p.mu.Unlock()

	p.receiver.stop()
	var closeErr error
	if pc != nil {
		closeErr = pc.Close()
	}
	if cause == nil {
		cause = closeErr
	}
	p.mu.Lock()
	p.closeErr = cause
	p.shutdownErr = closeErr
	p.mu.Unlock()
	if p.endpoint != nil {
		p.endpoint.unregister(p)
	}
	close(p.done)
	if callback := p.closedCallback(); callback != nil {
		callback(cause)
	}
	close(p.closeComplete)
	return closeErr
}

func (p *Peer) fail(err error) {
	if err != nil {
		_ = p.terminate(err)
	}
}

func (p *Peer) isClosed() bool {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	return closed
}

func (p *Peer) closeError() error {
	p.mu.Lock()
	err := p.closeErr
	p.mu.Unlock()
	return err
}

func (p *Peer) shutdownError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shutdownErr
}

func (p *Peer) enableCallbacks() bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	p.callbacksActive = true
	p.mu.Unlock()
	if callback := p.readyCallback(); callback != nil {
		callback()
	}
	return true
}

func (p *Peer) readyCallback() func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.callbacksActive || !p.ready || p.readyDelivered {
		return nil
	}
	p.readyDelivered = true
	return p.callbacks.Ready
}

func (p *Peer) controlCallback() func([]byte) {
	p.mu.Lock()
	callback := p.callbacks.Control
	if p.closed || !p.callbacksActive {
		callback = nil
	}
	p.mu.Unlock()
	return callback
}

func (p *Peer) keyFrameCallback() func() {
	p.mu.Lock()
	callback := p.callbacks.KeyFrameRequested
	if p.closed || !p.callbacksActive {
		callback = nil
	}
	p.mu.Unlock()
	return callback
}

func (p *Peer) closedCallback() func(error) {
	p.mu.Lock()
	callback := p.callbacks.Closed
	if !p.callbacksActive {
		callback = nil
	}
	p.mu.Unlock()
	return callback
}

func (p *Peer) Done() <-chan struct{} { return p.done }
