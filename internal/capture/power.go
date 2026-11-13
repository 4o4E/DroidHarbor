package capture

import "context"

// NewPowerManager shares the immutable server artifact but owns separate control
// processes and journal records. Closing a viewer must not put an online device
// to sleep or restore its physical panel power.
func (m *Manager) NewPowerManager() *Manager {
	ctx, cancel := context.WithCancel(m.ctx)
	return &Manager{
		db: m.db, adb: m.adb, source: m.source, server: m.server, logger: m.logger,
		ctx: ctx, cancel: cancel, controlOnly: true, videoOwner: m,
		streams: make(map[string]*Stream), recovering: make(map[string]*recoveryFlight),
	}
}

func (m *Manager) kind() string {
	if m.controlOnly {
		return "scrcpy-power"
	}
	return resourceKind
}
