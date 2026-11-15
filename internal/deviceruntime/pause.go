package deviceruntime

import "context"

func (m *Manager) PauseDevice(ctx context.Context, deviceID string) (func(), error) {
	m.mu.Lock()
	m.paused[deviceID] = true
	w := m.workers[deviceID]
	if w != nil {
		w.cancel()
	}
	m.mu.Unlock()
	release := func() { m.mu.Lock(); delete(m.paused, deviceID); m.mu.Unlock(); m.Notify() }
	if w != nil {
		select {
		case <-w.done:
		case <-ctx.Done():
			return release, ctx.Err()
		}
	}
	return release, nil
}
