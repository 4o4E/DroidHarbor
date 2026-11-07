package leases

import (
	"errors"

	"droidharbor/internal/events"
)

// SetEvents binds the notification bus without contending with lease state.
func (m *Manager) SetEvents(bus *events.Bus) { m.events.Store(bus) }

// EventTask reports the current owner solely for notification routing.
// It is intentionally independent of authorization and lease state locks.
func (m *Manager) EventTask(deviceID string) string {
	value, ok := m.owners.Load(deviceID)
	if !ok {
		return ""
	}
	return value.(string)
}

func (m *Manager) setEventOwner(deviceID, taskID string) {
	m.owners.Store(deviceID, taskID)
}

func (m *Manager) clearEventOwner(deviceID, taskID string) {
	m.owners.CompareAndDelete(deviceID, taskID)
}

func (m *Manager) publishLeaseChange(lease Lease, terminal bool) {
	if terminal {
		m.clearEventOwner(lease.DeviceID, lease.TaskID)
	}
	bus := m.events.Load()
	if bus == nil {
		return
	}
	m.publishChange(bus, events.Change{EntityType: "lease", EntityID: lease.ID, TaskID: lease.TaskID, DeviceID: lease.DeviceID})
	m.publishChange(bus, events.Change{EntityType: "device", EntityID: lease.DeviceID, TaskID: lease.TaskID, DeviceID: lease.DeviceID})
}

func (m *Manager) publishDeviceChange(deviceID, taskID string) {
	bus := m.events.Load()
	if bus == nil {
		return
	}
	m.publishChange(bus, events.Change{EntityType: "device", EntityID: deviceID, TaskID: taskID, DeviceID: deviceID})
}

func (m *Manager) publishChange(bus *events.Bus, change events.Change) {
	if _, err := bus.Publish(change); err != nil && !errors.Is(err, events.ErrClosed) {
		m.logFailure("event_publish", err)
	}
}
