package rtc

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

func (p *Peer) WriteAudioRTP(packet *rtp.Packet) error {
	if packet == nil {
		return errors.New("nil Opus packet")
	}
	p.mu.Lock()
	closed, ready, track := p.closed, p.ready, p.audioTrack
	p.mu.Unlock()
	if closed {
		return ErrPeerClosed
	}
	if !ready {
		return ErrPeerNotReady
	}
	if track == nil {
		return errors.New("peer did not negotiate Opus audio")
	}
	if err := track.WriteRTP(packet); err != nil {
		return fmt.Errorf("write WebRTC Opus packet: %w", err)
	}
	return nil
}

func (p *Peer) drainAudioRTCP() {
	buffer := make([]byte, 1500)
	for {
		if _, _, err := p.audioSender.Read(buffer); err != nil {
			if !p.isClosed() {
				failure := fmt.Errorf("read Opus RTCP: %w", err)
				if p.callbacks.AudioFailed != nil {
					p.callbacks.AudioFailed(failure)
				} else {
					p.fail(failure)
				}
			}
			return
		}
	}
}
