package rtc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	maxDataChannelChunk         = 16 << 10
	maxLogicalMessage           = 2 << 20
	maxBufferedDataChannelBytes = 4 << 20
	incompleteFrameLifetime     = 5 * time.Second
)

type framedReceiver struct {
	peer *Peer
	mu   sync.Mutex

	header     [4]byte
	headerLen  int
	payload    []byte
	readBytes  int
	timer      *time.Timer
	generation uint64
	stopped    bool
}

func (r *framedReceiver) receive(message webrtc.DataChannelMessage) {
	if r.peer.isClosed() {
		return
	}
	if message.IsString {
		r.peer.fail(errors.New("string WebRTC control DataChannel message is not supported"))
		return
	}
	if len(message.Data) > maxDataChannelChunk {
		r.peer.fail(fmt.Errorf("WebRTC control DataChannel chunk exceeds %d bytes", maxDataChannelChunk))
		return
	}
	if len(message.Data) == 0 {
		return
	}
	completed, err := r.consume(message.Data)
	for _, logical := range completed {
		if callback := r.peer.controlCallback(); callback != nil {
			callback(logical)
		}
	}
	if err != nil {
		r.peer.fail(err)
	}
}

func (r *framedReceiver) consume(data []byte) ([][]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, ErrPeerClosed
	}

	var completed [][]byte
	for len(data) != 0 {
		if r.headerLen < len(r.header) {
			if r.headerLen == 0 {
				r.startTimerLocked()
			}
			n := copy(r.header[r.headerLen:], data)
			r.headerLen += n
			data = data[n:]
			if r.headerLen < len(r.header) {
				continue
			}
			length := binary.BigEndian.Uint32(r.header[:])
			if length == 0 || length > maxLogicalMessage {
				r.resetLocked()
				return completed, fmt.Errorf("invalid framed WebRTC control message length %d", length)
			}
			r.payload = make([]byte, int(length))
		}

		n := copy(r.payload[r.readBytes:], data)
		r.readBytes += n
		data = data[n:]
		if r.readBytes == len(r.payload) {
			completed = append(completed, r.payload)
			r.resetLocked()
		}
	}
	return completed, nil
}

func (r *framedReceiver) startTimerLocked() {
	r.generation++
	generation := r.generation
	r.timer = time.AfterFunc(incompleteFrameLifetime, func() {
		r.mu.Lock()
		incomplete := generation == r.generation && (r.headerLen != 0 || r.payload != nil)
		if incomplete {
			r.timer = nil
		}
		r.mu.Unlock()
		if incomplete {
			r.peer.fail(errors.New("incomplete WebRTC control frame exceeded five seconds"))
		}
	})
}

func (r *framedReceiver) resetLocked() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.headerLen = 0
	r.payload = nil
	r.readBytes = 0
}

func (r *framedReceiver) stop() {
	r.mu.Lock()
	r.stopped = true
	r.resetLocked()
	r.mu.Unlock()
}
