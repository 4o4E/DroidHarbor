package devices

import (
	"errors"
	"time"

	"droidharbor/internal/diagnostics"
	"droidharbor/internal/events"
)

type RuntimeIssue struct {
	Stage   string `json:"stage"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Metrics struct {
	ObservedAt          time.Time `json:"observedAt"`
	CPUPercent          *float64  `json:"cpuPercent"`
	MemoryUsedBytes     *uint64   `json:"memoryUsedBytes"`
	MemoryTotalBytes    *uint64   `json:"memoryTotalBytes"`
	BatteryTemperatureC *float64  `json:"batteryTemperatureC"`
	BatteryPercent      *float64  `json:"batteryPercent"`
	PowerConnected      *bool     `json:"powerConnected"`
	ChargingStatus      *string   `json:"chargingStatus"`
}

type RuntimeInfo struct {
	LinkID         string         `json:"linkId"`
	LinkGeneration uint64         `json:"linkGeneration"`
	PowerState     string         `json:"powerState"`
	CoverState     string         `json:"coverState"`
	Metrics        *Metrics       `json:"metrics,omitempty"`
	Issues         []RuntimeIssue `json:"issues"`
}

// BindRuntime is startup-only. The observer only queues invalidation while the
// registry lock is held; device I/O belongs to independent per-device workers.
func (r *Registry) BindRuntime(view func(string) *RuntimeInfo, notify func()) {
	r.runtimeView, r.runtimeNotify = view, notify
}

func (r *Registry) PublishRuntime(deviceID string) {
	r.mu.RLock()
	bus := r.events
	r.mu.RUnlock()
	if bus == nil {
		return
	}
	if _, err := bus.Publish(events.Change{EntityType: "device-runtime", EntityID: deviceID, DeviceID: deviceID}); err != nil && !errors.Is(err, events.ErrClosed) {
		r.logger.Error("device_runtime_event_failed", "deviceId", deviceID, "cause", diagnostics.Cause(err))
	}
}
