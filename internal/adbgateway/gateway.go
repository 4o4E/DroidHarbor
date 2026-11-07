package adbgateway

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxStreams     = 16
	maxOpenService = 4096
	ioDeadline     = 10 * time.Second
)

var (
	errStreamClosed = errors.New("ADB stream is closed")
	errUnauthorized = errors.New("ADB session is not authorized")
)

type Session struct {
	PublicKey *rsa.PublicKey
	Features  []string
	Check     func() error
	Open      func(context.Context, string) (net.Conn, error)
}

type inputFrame struct {
	data, buffer []byte
	pool         *payloadPool
}

type stream struct {
	local, remote uint32
	input         chan inputFrame
	ack           chan struct{}
	done          chan struct{}
	started       chan struct{}
	openCtx       context.Context
	cancel        context.CancelFunc
	sendMu        sync.Mutex
	mu            sync.Mutex
	conn          net.Conn
	closed        bool
	active        atomic.Bool
	inputBusy     atomic.Bool
	outputBusy    atomic.Bool
}

func newStream(ctx context.Context, local, remote uint32) *stream {
	openCtx, cancel := context.WithCancel(ctx)
	return &stream{local: local, remote: remote, input: make(chan inputFrame, 1), ack: make(chan struct{}, 1), done: make(chan struct{}), started: make(chan struct{}), openCtx: openCtx, cancel: cancel}
}

func (s *stream) isClosed() bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	return closed
}

func (s *stream) attach(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conn = conn
	return true
}

func (s *stream) upstream() net.Conn {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	return conn
}

// markClosed is called while sendMu is held, ordering stream teardown against
// packet emission so that a peer close cannot race a late WRTE/OKAY.
func (s *stream) markClosed() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	conn := s.conn
	for {
		select {
		case item := <-s.input:
			item.pool.put(item.buffer)
		default:
			s.mu.Unlock()
			s.cancel()
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
	}

}
func (s *stream) close() {
	s.sendMu.Lock()
	s.markClosed()
	s.sendMu.Unlock()
}

type inboundPacket struct {
	p   packet
	err error
}
type openResult struct {
	s    *stream
	conn net.Conn
	err  error
}

// Serve terminates one Android device-side ADB transport. downstream is a byte
// stream; callers may carry it over WSS without preserving message boundaries.
func Serve(ctx context.Context, downstream net.Conn, session Session) error {
	if ctx == nil || downstream == nil || session.PublicKey == nil || session.PublicKey.N == nil || session.Open == nil || session.Check == nil {
		return errors.New("incomplete ADB gateway session")
	}
	if session.PublicKey.N.BitLen() != 2048 || session.PublicKey.E != 65537 {
		return errors.New("unsupported ADB gateway key")
	}

	lifetime, cancel := context.WithCancel(ctx)
	pool := newPayloadPool()
	var writeMu sync.Mutex
	var streamsMu sync.Mutex
	streams := make(map[uint32]*stream, maxStreams)
	inbound := make(chan inboundPacket, 1)
	openResults := make(chan openResult, maxStreams)
	slots := make(chan struct{}, maxStreams)
	var workers sync.WaitGroup
	var checksumSkip bool
	var peerMaxData uint32
	watcherDone := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-lifetime.Done():
			_ = downstream.Close()
		case <-watchDone:
		}
	}()
	removeStream := func(s *stream) {
		streamsMu.Lock()
		if streams[s.local] == s {
			delete(streams, s.local)
		}
		streamsMu.Unlock()
	}
	closeAll := func() {
		streamsMu.Lock()
		all := make([]*stream, 0, len(streams))
		for _, s := range streams {
			all = append(all, s)
		}
		streams = make(map[uint32]*stream, maxStreams)
		streamsMu.Unlock()
		for _, s := range all {
			s.close()
		}
	}
	defer func() {
		cancel()
		_ = downstream.Close()
		closeAll()
		workers.Wait()
		for {
			select {
			case p := <-inbound:
				releasePacket(pool, &p.p)
			default:
				goto drainedInbound
			}
		}
	drainedInbound:
		for {
			select {
			case result := <-openResults:
				if result.conn != nil {
					_ = result.conn.Close()
				}
			default:
				close(watchDone)
				<-watcherDone
				return
			}
		}
	}()

	sendChecked := func(p packet, before func()) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if lifetime.Err() != nil {
			return context.Canceled
		}
		if session.Check() != nil {
			return errUnauthorized
		}
		if err := downstream.SetWriteDeadline(time.Now().Add(ioDeadline)); err != nil {
			return err
		}
		if before != nil {
			before()
		}
		err := writePacket(downstream, p, checksumSkip)
		clearErr := downstream.SetWriteDeadline(time.Time{})
		if err != nil {
			return err
		}
		if clearErr != nil {
			return clearErr
		}
		return nil
	}
	abortTransport := func() { cancel(); _ = downstream.Close() }
	writeStream := func(s *stream, p packet, before func()) error {
		s.sendMu.Lock()
		defer s.sendMu.Unlock()
		if s.isClosed() {
			return errStreamClosed
		}
		return sendChecked(p, before)
	}
	finishStream := func(s *stream) {
		s.sendMu.Lock()
		if !s.isClosed() {
			// Publish closed before CLSE is visible to the peer, so an in-flight
			// WRTE/OKAY cannot be mistaken for traffic on a still-active stream.
			s.markClosed()
			err := sendChecked(packet{cmd: cmdClse, arg0: s.remote, arg1: s.local}, nil)
			if err != nil && !errors.Is(err, errStreamClosed) {
				abortTransport()
			}
		}
		removeStream(s)
		s.sendMu.Unlock()
	}
	lookupStream := func(id uint32) *stream {
		streamsMu.Lock()
		s := streams[id]
		streamsMu.Unlock()
		return s
	}
	supervise := func(s *stream, service string) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				s.close()
				removeStream(s)
				<-slots
			}()
			conn, err := session.Open(s.openCtx, service)
			if err == nil && conn == nil {
				err = errors.New("ADB service unavailable")
			}
			select {
			case openResults <- openResult{s: s, conn: conn, err: err}:
			case <-lifetime.Done():
				if conn != nil {
					_ = conn.Close()
				}
				return
			}
			// Keep the slot until the event loop has disposed of or attached
			// the Open result, even if the peer canceled while Open was pending.
			select {
			case <-s.started:
			case <-lifetime.Done():
				return
			}
			if err != nil || s.isClosed() {
				return
			}
			var writer sync.WaitGroup
			writer.Add(1)
			go func() {
				defer writer.Done()
				streamWriter(lifetime, s, writeStream, finishStream, session.Check, abortTransport)
			}()
			streamReader(lifetime, s, writeStream, finishStream, peerMaxData, session.Check, abortTransport)
			s.close()
			writer.Wait()
		}()
	}

	// The initial CNXN and AUTH exchange has a total bounded read window. A
	// cancellation closes downstream independently of whichever read is blocked.
	if err := downstream.SetReadDeadline(time.Now().Add(ioDeadline)); err != nil {
		return errors.New("ADB handshake deadline failed")
	}
	cnxn, err := readPacket(downstream, maxPacketSize, false)
	if err != nil || cnxn.cmd != cmdCnxn || cnxn.arg0 < 0x01000000 || cnxn.arg1 == 0 {
		return errors.New("invalid ADB connection negotiation")
	}
	version := cnxn.arg0
	if version > protocolVersion {
		version = protocolVersion
	}
	skipChecksum := version >= 0x01000001
	checksumSkip = skipChecksum
	peerMaxData = cnxn.arg1
	if peerMaxData > maxPacketSize {
		peerMaxData = maxPacketSize
	}
	// The task-specific native Server loads one task key, plus the official Windows
	// user key. Try at most those two signatures; never enroll an unknown key.
	var challenge [20]byte
	authenticated := false
	for range 2 {
		if _, err := rand.Read(challenge[:]); err != nil {
			return errors.New("unable to create ADB challenge")
		}
		if err := sendChecked(packet{cmd: cmdAuth, arg0: 1, data: challenge[:]}, nil); err != nil {
			return errors.New("ADB authentication write failed")
		}
		p, err := readPacket(downstream, uint32(session.PublicKey.Size()), skipChecksum)
		if err != nil || p.cmd != cmdAuth || p.arg0 != 2 || p.arg1 != 0 || len(p.data) != session.PublicKey.Size() {
			return errors.New("ADB authentication failed")
		}
		if rsa.VerifyPKCS1v15(session.PublicKey, crypto.SHA1, challenge[:], p.data) == nil {
			authenticated = true
			break
		}
	}
	if !authenticated {
		return errors.New("ADB authentication failed")
	}
	if err := downstream.SetReadDeadline(time.Time{}); err != nil {
		return errors.New("ADB handshake deadline reset failed")
	}
	if err := session.Check(); err != nil {
		return errUnauthorized
	}
	targetFeatures := make(map[string]bool, len(session.Features))
	for _, f := range session.Features {
		targetFeatures[f] = true
	}
	hostFeatures := advertisedFeatures(cnxn.data)
	features := negotiatedFeatures(targetFeatures, hostFeatures)
	banner := []byte("device::features=" + strings.Join(features, ",") + ";\x00")
	if err := sendChecked(packet{cmd: cmdCnxn, arg0: version, arg1: maxPacketSize, data: banner}, nil); err != nil {
		return errors.New("ADB connection negotiation failed")
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			p, err := readPacketWithPool(downstream, maxPacketSize, skipChecksum, pool)
			select {
			case inbound <- inboundPacket{p: p, err: err}:
			case <-lifetime.Done():
				releasePacket(pool, &p)
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var nextRemote uint32 = 1
	for {
		select {
		case <-lifetime.Done():
			return nil
		case result := <-openResults:
			err := func() error {
				s := result.s
				defer close(s.started)
				if s.isClosed() || lookupStream(s.local) != s {
					if result.conn != nil {
						_ = result.conn.Close()
					}
					return nil
				}
				if result.err != nil {
					if result.conn != nil {
						_ = result.conn.Close()
					}
					removeStream(s)
					s.close()
					if err := sendChecked(packet{cmd: cmdClse, arg0: 0, arg1: s.local}, nil); err != nil {
						return errors.New("ADB stream rejection failed")
					}
					return nil
				}
				if err := session.Check(); err != nil {
					_ = result.conn.Close()
					return errUnauthorized
				}
				if !s.attach(result.conn) {
					_ = result.conn.Close()
					return nil
				}
				if err := writeStream(s, packet{cmd: cmdOkay, arg0: s.remote, arg1: s.local}, nil); err != nil {
					s.close()
					removeStream(s)
					if errors.Is(err, errStreamClosed) {
						return nil
					}
					return errors.New("ADB stream setup failed")
				}
				s.active.Store(true)
				return nil
			}()
			if err != nil {
				return err
			}
		case incoming := <-inbound:
			if incoming.err != nil {
				releasePacket(pool, &incoming.p)
				return errors.New("ADB transport ended")
			}
			p := incoming.p
			err := func() error {
				if err := session.Check(); err != nil {
					return errUnauthorized
				}
				switch p.cmd {
				case cmdOpen:
					service, e := requireString(p.data)
					if e != nil || p.arg0 == 0 || p.arg1 != 0 || len(p.data) > maxOpenService {
						return errors.New("invalid ADB OPEN packet")
					}
					if !validService(service) || strings.HasPrefix(service, "shell,v2") && (!targetFeatures["shell_v2"] || !hostFeatures["shell_v2"]) {
						return sendChecked(packet{cmd: cmdClse, arg0: 0, arg1: p.arg0}, nil)
					}
					streamsMu.Lock()
					duplicate := streams[p.arg0] != nil
					if duplicate {
						streamsMu.Unlock()
						return errors.New("ADB stream identifier reused")
					}
					select {
					case slots <- struct{}{}:
						remote := nextRemote
						for attempts := 0; attempts <= maxStreams; attempts++ {
							used := false
							for _, active := range streams {
								if active.remote == remote {
									used = true
									break
								}
							}
							if !used {
								break
							}
							remote += 2
							if remote == 0 {
								remote = 1
							}
						}
						nextRemote = remote + 2
						if nextRemote == 0 {
							nextRemote = 1
						}
						s := newStream(lifetime, p.arg0, remote)
						streams[p.arg0] = s
						streamsMu.Unlock()
						if err := session.Check(); err != nil {
							s.close()
							removeStream(s)
							<-slots
							return errUnauthorized
						}
						supervise(s, service)
						return nil
					default:
						streamsMu.Unlock()
						return sendChecked(packet{cmd: cmdClse, arg0: 0, arg1: p.arg0}, nil)
					}
				case cmdWrte:
					s := lookupStream(p.arg0)
					// ADB permits packets already in flight when a stream closes.
					// A reused peer ID does not make its old remote ID current.
					if s == nil || s.remote != p.arg1 {
						return nil
					}
					s.mu.Lock()
					if s.closed {
						s.mu.Unlock()
						return nil
					}
					if !s.active.Load() || !s.inputBusy.CompareAndSwap(false, true) {
						s.mu.Unlock()
						return errors.New("ADB write references an inactive or busy stream")
					}
					item := inputFrame{data: p.data, buffer: p.buffer, pool: pool}
					select {
					case s.input <- item:
						p.buffer = nil
						p.data = nil
						s.mu.Unlock()
						return nil
					default:
						s.mu.Unlock()
						return errors.New("ADB stream flow-control violation")
					}
				case cmdOkay:
					s := lookupStream(p.arg0)
					if s == nil || p.arg1 != s.remote {
						return nil
					}
					s.mu.Lock()
					if s.closed {
						s.mu.Unlock()
						return nil
					}
					ready := s.active.Load() && s.outputBusy.CompareAndSwap(true, false)
					s.mu.Unlock()
					if !ready {
						return errors.New("ADB acknowledgement references an inactive or idle stream")
					}
					select {
					case s.ack <- struct{}{}:
					default:
						return errors.New("ADB duplicate acknowledgement")
					}
					return nil
				case cmdClse:
					s := lookupStream(p.arg0)
					if s == nil {
						return nil
					} // Late CLSE after a completed local close is harmless.
					if p.arg1 != 0 && p.arg1 != s.remote {
						return nil
					} // A late close may refer to an earlier incarnation of the peer ID.
					s.sendMu.Lock()
					if !s.isClosed() {
						s.markClosed()
						if p.arg1 != 0 {
							if err := sendChecked(packet{cmd: cmdClse, arg0: s.remote, arg1: s.local}, nil); err != nil {
								s.sendMu.Unlock()
								removeStream(s)
								return errors.New("ADB stream close failed")
							}
						}
					}
					s.sendMu.Unlock()
					removeStream(s)
					return nil
				default:
					return fmt.Errorf("unsupported ADB command 0x%08x", p.cmd)
				}
			}()
			releasePacket(pool, &p)
			if err != nil {
				return err
			}
		}
	}
}

func streamWriter(ctx context.Context, s *stream, send func(*stream, packet, func()) error, finish func(*stream), check func() error, abort func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case item := <-s.input:
			if check() != nil {
				item.pool.put(item.buffer)
				abort()
				return
			}
			conn := s.upstream()
			err := conn.SetWriteDeadline(time.Now().Add(ioDeadline))
			if err == nil {
				err = writeFull(conn, item.data)
				clearErr := conn.SetWriteDeadline(time.Time{})
				if err == nil {
					err = clearErr
				}
			}
			if err != nil {
				item.pool.put(item.buffer)
				if !s.isClosed() {
					finish(s)
				}
				return
			}
			item.pool.put(item.buffer)
			if err := send(s, packet{cmd: cmdOkay, arg0: s.remote, arg1: s.local}, func() { s.inputBusy.Store(false) }); err != nil {
				if errors.Is(err, errStreamClosed) {
					return
				}
				abort()
				return
			}
		}
	}
}

func streamReader(ctx context.Context, s *stream, send func(*stream, packet, func()) error, finish func(*stream), maxData uint32, check func() error, abort func()) {
	buf := make([]byte, int(maxData))
	for {
		if check() != nil {
			abort()
			return
		}
		n, err := s.upstream().Read(buf)
		if s.isClosed() {
			return
		}
		if n > 0 {
			if check() != nil {
				abort()
				return
			}
			if !s.outputBusy.CompareAndSwap(false, true) {
				abort()
				return
			}
			if send(s, packet{cmd: cmdWrte, arg0: s.remote, arg1: s.local, data: buf[:n]}, nil) != nil {
				if !s.isClosed() {
					abort()
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case <-s.ack:
			}
		}
		if err != nil {
			finish(s)
			return
		}
		if n == 0 {
			continue
		}
	}
}
