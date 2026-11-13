package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/leases"
	"droidharbor/internal/scrcpy"
)

var ErrResultUnknown = diagnostics.New("scrcpy control result unknown")

func (s *Stream) ID() string                            { return s.id }
func (s *Stream) Link() leases.Link                     { return s.link }
func (s *Stream) Frames() <-chan scrcpy.Packet          { return s.frames }
func (s *Stream) Messages() <-chan scrcpy.DeviceMessage { return s.messages }
func (s *Stream) Done() <-chan struct{}                 { return s.done }
func (s *Stream) DisplayPowerChanges() <-chan bool      { return s.displayPower }
func (s *Stream) Err() error                            { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Stream) cleanupError() error                   { s.mu.Lock(); defer s.mu.Unlock(); return s.cleanupErr }

func (s *Stream) Send(ctx context.Context, msg scrcpy.ControlMessage, authorizeOperation func() error) error {
	if ctx == nil {
		return diagnostics.New("capture operation context is required")
	}
	if authorizeOperation == nil {
		return diagnostics.New("capture operation authorization is required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := s.authorizeSystem(); err != nil {
		if s.ctx.Err() == nil {
			s.requestStop(err)
			go s.finishAsync()
		}
		return err
	}
	if err := authorizeOperation(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.control == nil {
		return diagnostics.New("capture control socket is closed")
	}
	payload, err := scrcpy.AppendControl(s.controlBuffer[:0], msg)
	if err != nil {
		return err
	}
	s.controlBuffer = payload
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := s.control.SetWriteDeadline(deadline); err != nil {
		failure := fmt.Errorf("set capture control write deadline: %w", err)
		s.requestStop(failure)
		go s.finishAsync()
		return failure
	}
	writeCanceled := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = s.control.SetWriteDeadline(time.Now())
		close(writeCanceled)
	})
	written, writeErr := s.control.Write(payload)
	if !stopCancellation() {
		<-writeCanceled
	}
	restoreErr := s.control.SetWriteDeadline(time.Time{})
	failure := writeErr
	if failure == nil && restoreErr != nil {
		failure = restoreErr
	}
	if failure == nil && written == len(payload) {
		return nil
	}
	if written > 0 {
		if failure == nil {
			failure = io.ErrShortWrite
		}
		unknown := fmt.Errorf("%w: %w", ErrResultUnknown, failure)
		s.requestStop(unknown)
		go s.finishAsync()
		return unknown
	}
	if failure == nil {
		failure = io.ErrShortWrite
	}
	if ctx.Err() != nil && restoreErr == nil && isWriteCancellation(failure) && s.ctx.Err() == nil && s.authorizeSystem() == nil {
		return ctx.Err()
	}
	s.requestStop(fmt.Errorf("capture control write failed: %w", failure))
	go s.finishAsync()
	return failure
}
func isWriteCancellation(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func (s *Stream) Stop(ctx context.Context) error {
	s.requestStop(nil)
	return s.finish(ctx)
}

func (s *Stream) failAndClose(err error) {
	s.requestStop(err)
	ctx, cancel := cleanupContext()
	defer cancel()
	if cleanupErr := s.finish(ctx); cleanupErr != nil {
		s.manager.logger.Error("capture_cleanup_failed", "deviceId", s.deviceID, "captureId", s.id, "cause", diagnostics.Cause(cleanupErr))
	}
}
func (s *Stream) finishAsync() {
	ctx, cancel := cleanupContext()
	defer cancel()
	if err := s.finish(ctx); err != nil {
		s.manager.logger.Error("capture_async_cleanup_failed", "deviceId", s.deviceID, "captureId", s.id, "cause", diagnostics.Cause(err))
	}
}
func (s *Stream) scheduleFinalizer() {
	s.finishMu.Lock()
	if s.finalizerStarted {
		s.finishMu.Unlock()
		return
	}
	select {
	case <-s.done:
		s.finishMu.Unlock()
		return
	default:
	}
	s.finalizerStarted = true
	s.finishMu.Unlock()
	go func() {
		<-s.started
		s.workerWaitOnce.Do(func() { go func() { s.workers.Wait(); close(s.workersDone) }() })
		<-s.workersDone
		s.finishAsync()
	}()
}
func (s *Stream) completeStart() { s.startedOnce.Do(func() { close(s.started) }) }
func (s *Stream) watchContextCancellation() {
	context.AfterFunc(s.ctx, func() {
		if s.stopRequested.Load() {
			return
		}
		s.requestStop(nil)
		s.scheduleFinalizer()
	})
}
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cleanupTimeout)
}

func (s *Stream) requestStop(err error) {
	s.stopOnce.Do(func() {
		s.stopRequested.Store(true)
		if err != nil {
			s.manager.logger.Warn("capture_stream_failed", "deviceId", s.deviceID, "captureId", s.id, "cause", diagnostics.Cause(err))
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
		}
		s.cancel()
		if s.stopLink != nil {
			s.stopLink()
		}
		s.connMu.Lock()
		for _, conn := range []net.Conn{s.video, s.control, s.shell} {
			if conn != nil {
				_ = conn.Close()
			}
		}
		s.connMu.Unlock()
	})
}

func (s *Stream) finish(ctx context.Context) error {
	if ctx == nil {
		s.scheduleFinalizer()
		return diagnostics.New("capture cleanup context is required")
	}
	for {
		select {
		case <-s.done:
			return s.cleanupError()
		default:
		}
		if err := ctx.Err(); err != nil {
			s.scheduleFinalizer()
			return err
		}
		s.finishMu.Lock()
		if s.finishing {
			attempt := s.finishAttempt
			s.finishMu.Unlock()
			select {
			case <-attempt:
				continue
			case <-s.done:
				return s.cleanupError()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		s.finishing = true
		attempt := make(chan struct{})
		s.finishAttempt = attempt
		s.finishMu.Unlock()
		finished := false
		defer func() {
			if !finished {
				s.finishMu.Lock()
				s.finishing = false
				close(attempt)
				s.finishMu.Unlock()
			}
		}()
		select {
		case <-s.started:
		case <-ctx.Done():
			s.scheduleFinalizer()
			return ctx.Err()
		}
		s.workerWaitOnce.Do(func() { go func() { s.workers.Wait(); close(s.workersDone) }() })
		select {
		case <-s.workersDone:
		case <-ctx.Done():
			s.scheduleFinalizer()
			return ctx.Err()
		}
		err := s.manager.cleanup(ctx, s)
		if err != nil {
			s.mu.Lock()
			s.cleanupErr = err
			s.mu.Unlock()
		}
		s.manager.remove(s)
		if s.manager.controlOnly {
			close(s.displayPower)
		} else {
			close(s.frames)
		}
		close(s.messages)
		close(s.done)
		s.finishMu.Lock()
		s.finishing = false
		close(attempt)
		s.finishMu.Unlock()
		finished = true
		return s.cleanupError()
	}
}

func (s *Stream) readFrames() {
	defer s.workers.Done()
	for {
		packet, err := scrcpy.ReadPacket(s.video, nil, 8<<20)
		if err != nil {
			if s.ctx.Err() == nil {
				s.requestStop(fmt.Errorf("scrcpy video stream failed: %w", err))
				go s.finishAsync()
			}
			return
		}
		select {
		case s.frames <- packet:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Stream) readMessages() {
	defer s.workers.Done()
	var scratch []byte
	for {
		message, err := scrcpy.ReadDeviceMessage(s.control, scratch)
		if err != nil {
			if s.ctx.Err() == nil {
				s.requestStop(fmt.Errorf("scrcpy device message stream failed: %w", err))
				go s.finishAsync()
			}
			return
		}
		select {
		case s.messages <- message:
		case <-s.ctx.Done():
			return
		default:
			s.requestStop(diagnostics.New("capture device-message consumer exceeded bounded queue"))
			go s.finishAsync()
			return
		}
	}
}

func (s *Stream) drainShell() {
	defer s.workers.Done()
	serverLogs := serverDiagnostics{logger: s.manager.logger, deviceID: s.deviceID, captureID: s.id}
	if s.manager.controlOnly {
		serverLogs.powerChanged = func(on bool) {
			select {
			case s.displayPower <- on:
			default:
			}
		}
		serverLogs.keepActiveFailed = func() {
			s.requestStop(diagnostics.New("scrcpy keep-active worker failed"))
			go s.finishAsync()
		}
	}
	defer serverLogs.flush()
	var scratch []byte
	for {
		packet, err := adb.ReadShellPacket(s.shell, scratch, maxShellPacket)
		if err != nil {
			if s.ctx.Err() == nil {
				s.requestStop(fmt.Errorf("foreground scrcpy shell exited unexpectedly: %w", err))
				go s.finishAsync()
			}
			return
		}
		scratch = packet.Data
		switch packet.ID {
		case 1, 2:
			serverLogs.write(packet.ID, packet.Data)
		case 3:
			if s.ctx.Err() == nil {
				var exitCode any
				if len(packet.Data) == 1 {
					exitCode = int(packet.Data[0])
				}
				s.manager.logger.Warn("scrcpy_server_exited", "deviceId", s.deviceID, "captureId", s.id, "exitCode", exitCode)
				s.requestStop(diagnostics.New("foreground scrcpy shell exited"))
				go s.finishAsync()
			}
			return
		default:
			s.requestStop(diagnostics.New("foreground shell sent a client-only shell-v2 packet"))
			go s.finishAsync()
			return
		}
	}
}
