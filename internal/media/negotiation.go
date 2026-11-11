package media

import (
	"context"
	"errors"

	"droidharbor/internal/rtc"
)

// Capture bootstrap and SDP/ICE preparation have independent I/O. Join still
// waits for both, and cancels/closes a prepared peer if capture startup fails.
func (m *Manager) preparePeerAndCapture(ctx context.Context, s *session, offer string) (*rtc.Peer, error) {
	workCtx, cancel := context.WithCancelCause(m.ctx)
	stopRequest := context.AfterFunc(ctx, func() { cancel(ctx.Err()) })
	defer func() { stopRequest(); cancel(nil) }()
	acceptCtx, stopAccept := context.WithTimeout(workCtx, negotiationTimeout)
	defer stopAccept()
	type accepted struct {
		peer *rtc.Peer
		err  error
	}
	result := make(chan accepted, 1)
	go func() {
		peer, err := m.deps.Transport.Accept(acceptCtx, offer, rtcCallbacks(s))
		if err != nil {
			err = &Error{Status: 422, Code: "MEDIA_NEGOTIATION_FAILED", Message: "WebRTC 协商失败", Cause: err}
			cancel(err)
		}
		result <- accepted{peer: peer, err: err}
	}()
	startErr := s.hub.ensureSource(workCtx, func() error { return s.check(workCtx, false) })
	if startErr != nil {
		cancel(startErr)
	}
	answer := <-result
	if answer.err != nil && context.Cause(workCtx) == answer.err {
		if answer.peer != nil {
			answer.err = errors.Join(answer.err, answer.peer.Close())
		}
		return nil, errors.Join(answer.err, startErr)
	}
	if startErr != nil {
		if answer.peer != nil {
			startErr = errors.Join(startErr, answer.peer.Close())
		}
		return nil, startErr
	}
	if answer.err != nil {
		return nil, answer.err
	}
	return answer.peer, nil
}
