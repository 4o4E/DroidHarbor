package capture

import (
	"context"
	"errors"
)

// StopDevice only stops this manager's journaled process; other scrcpy owners
// are deliberately outside this operation.
func (m *Manager) StopDevice(ctx context.Context, deviceID string) error {
	m.mu.Lock()
	current := m.streams[deviceID]
	m.mu.Unlock()
	if current != nil {
		if err := current.Stop(ctx); err != nil && !errors.Is(err, ErrCleanupDeferred) {
			return err
		}
	}
	complete, err := m.Recover(ctx, deviceID)
	if err != nil {
		return err
	}
	if !complete {
		return errors.New("owned capture cleanup is not confirmed")
	}
	return nil
}
