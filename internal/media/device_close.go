package media

import (
	"context"
	"errors"
)

// BlockDevice holds the no-new-viewer boundary through registration removal.
// The caller releases it after committing removal or reporting cleanup failure.
func (m *Manager) BlockDevice(ctx context.Context, deviceID string) (func(), error) {
	m.mu.Lock()
	m.closingDevices[deviceID] = true
	var all []*session
	for _, s := range m.sessions {
		if s.hub.deviceID == deviceID {
			all = append(all, s)
		}
	}
	m.mu.Unlock()
	release := func() { m.mu.Lock(); delete(m.closingDevices, deviceID); m.mu.Unlock() }
	for _, s := range all {
		s.beginEnd(nil)
	}
	var result error
	for _, s := range all {
		select {
		case <-s.done:
			result = errors.Join(result, s.endErr)
		case <-ctx.Done():
			return release, errors.Join(result, ctx.Err())
		}
	}
	return release, result
}
