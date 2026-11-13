// Package deviceruntime initializes verified phones independently of browsers.
// Its control-only scrcpy processes keep Android active without encoding video;
// metrics and initial covers share the same pinned device incarnation.
package deviceruntime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/capture"
	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/leases"
	"droidharbor/internal/previews"
	"droidharbor/internal/scrcpy"
)

var errPowerCleanupPending = errors.New("device power ownership cleanup is pending")

type Manager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	registry    *devices.Registry
	power       *capture.Manager
	previews    *previews.Manager
	adb         *adb.Client
	logger      *slog.Logger
	changes     chan struct{}
	mu          sync.RWMutex
	workers     map[string]*worker
	wg          sync.WaitGroup
	closeErrors []error
}

type worker struct {
	manager        *Manager
	link           leases.Link
	ctx            context.Context
	cancel         context.CancelFunc
	stopLink       func() bool
	state          devices.RuntimeInfo
	snapshot       *devices.RuntimeInfo
	issues         map[string]devices.RuntimeIssue
	causes         map[string]string
	cpu            cpuSample
	power          *capture.Stream
	coverAttempted bool
}

func New(ctx context.Context, registry *devices.Registry, power *capture.Manager, preview *previews.Manager, client *adb.Client, logger *slog.Logger) *Manager {
	lifetime, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: lifetime, cancel: cancel, registry: registry, power: power, previews: preview, adb: client, logger: logger, changes: make(chan struct{}, 1), workers: make(map[string]*worker)}
	registry.BindRuntime(m.View, m.Notify)
	m.wg.Add(1)
	go m.loop()
	m.Notify()
	return m
}

func (m *Manager) Notify() {
	select {
	case m.changes <- struct{}{}:
	default:
	}
}

func (m *Manager) loop() {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		for _, w := range m.workers {
			w.cancel()
		}
		m.mu.Unlock()
	}()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.changes:
			m.syncLinks()
		}
	}
}

func (m *Manager) syncLinks() {
	type selection struct {
		link leases.Link
		ctx  context.Context
	}
	selected := make(map[string]selection)
	for _, observed := range m.registry.OnlineLinks() {
		if _, known := selected[observed.DeviceID]; known {
			continue
		}
		link, ctx, err := m.registry.ReadLink(m.ctx, observed.DeviceID)
		if err == nil {
			selected[observed.DeviceID] = selection{link, ctx}
		}
	}
	var started []string
	m.mu.Lock()
	for id, old := range m.workers {
		current, online := selected[id]
		if !online || current.link != old.link {
			old.cancel()
			delete(m.workers, id)
		}
	}
	for id, current := range selected {
		if m.workers[id] != nil || m.ctx.Err() != nil {
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		w := &worker{manager: m, link: current.link, ctx: ctx, cancel: cancel, issues: make(map[string]devices.RuntimeIssue), causes: make(map[string]string)}
		w.stopLink = context.AfterFunc(current.ctx, cancel)
		w.state = devices.RuntimeInfo{LinkID: current.link.LinkID, LinkGeneration: current.link.Generation, PowerState: "starting", CoverState: "pending", Issues: []devices.RuntimeIssue{}}
		initial := w.state
		w.snapshot = &initial
		m.workers[id] = w
		m.wg.Add(1)
		go w.run()
		started = append(started, id)
	}
	m.mu.Unlock()
	for _, id := range started {
		m.registry.PublishRuntime(id)
	}
}

func (m *Manager) View(deviceID string) *devices.RuntimeInfo {
	m.mu.RLock()
	w := m.workers[deviceID]
	if w == nil {
		m.mu.RUnlock()
		return nil
	}
	link, snapshot := w.link, w.snapshot
	m.mu.RUnlock()
	if !m.registry.HasLink(link) {
		return nil
	}
	current, _, err := m.registry.ReadLink(m.ctx, deviceID)
	if err != nil || current != link {
		return nil
	}
	return snapshot
}

func (w *worker) authorize() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	current, _, err := w.manager.registry.ReadLink(w.ctx, w.link.DeviceID)
	if err != nil {
		return err
	}
	if current != w.link {
		return diagnostics.New("device runtime link changed")
	}
	return nil
}

func (w *worker) publish() {
	if w.authorize() != nil {
		return
	}
	snapshot := w.state
	snapshot.Issues = make([]devices.RuntimeIssue, 0, len(w.issues))
	for _, stage := range [...]string{"power", "cover", "cpu", "memory", "battery"} {
		if issue, found := w.issues[stage]; found {
			snapshot.Issues = append(snapshot.Issues, issue)
		}
	}
	m := w.manager
	m.mu.Lock()
	current := m.workers[w.link.DeviceID] == w && w.ctx.Err() == nil
	if current {
		w.snapshot = &snapshot
	}
	m.mu.Unlock()
	if current {
		m.registry.PublishRuntime(w.link.DeviceID)
	}
}

func (w *worker) issue(stage, code, message string, cause error) {
	if w.ctx.Err() != nil {
		return
	}
	if cause == nil {
		if _, existed := w.issues[stage]; existed {
			w.manager.logger.Info("device_runtime_stage_recovered", "deviceId", w.link.DeviceID, "stage", stage)
		}
		delete(w.issues, stage)
		delete(w.causes, stage)
		return
	}
	old, existed := w.issues[stage]
	fingerprint := cause.Error()
	if !existed || old.Code != code || w.causes[stage] != fingerprint {
		w.manager.logger.Error("device_runtime_stage_failed", "deviceId", w.link.DeviceID, "linkId", w.link.LinkID, "stage", stage, "code", code, "cause", diagnostics.Cause(cause))
	}
	w.issues[stage] = devices.RuntimeIssue{Stage: stage, Code: code, Message: message}
	w.causes[stage] = fingerprint
}

func (w *worker) run() {
	defer w.manager.wg.Done()
	defer w.cancel()
	defer w.stopLink()
	defer func() {
		if w.power == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.power.Stop(ctx); err != nil {
			w.manager.logger.Error("device_power_cleanup_failed", "deviceId", w.link.DeviceID, "cause", diagnostics.Cause(err))
			if w.manager.ctx.Err() != nil {
				w.manager.mu.Lock()
				w.manager.closeErrors = append(w.manager.closeErrors, err)
				w.manager.mu.Unlock()
			}
		}
	}()
	w.manager.logger.Info("device_runtime_started", "deviceId", w.link.DeviceID, "linkId", w.link.LinkID, "linkGeneration", w.link.Generation)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	w.sampleMetrics()
	w.maintainPower()
	for {
		var powerDone <-chan struct{}
		if w.power != nil {
			powerDone = w.power.Done()
		}
		select {
		case <-w.ctx.Done():
			return
		case <-powerDone:
			if w.ctx.Err() != nil {
				return
			}
			cause := w.power.Err()
			if cause == nil {
				cause = diagnostics.New("device power controller ended")
			}
			w.issue("power", "DEVICE_POWER_INTERRUPTED", "设备在线保活已中断", cause)
			w.power = nil
			w.state.PowerState = "failed"
			w.publish()
		case <-ticker.C:
			w.maintainPower()
			w.sampleMetrics()
		}
	}
}

func (w *worker) maintainPower() {
	if w.authorize() != nil {
		return
	}
	if w.power == nil {
		complete, err := w.manager.power.Recover(w.ctx, w.link.DeviceID)
		if err == nil && !complete {
			err = errPowerCleanupPending
		}
		if err == nil {
			w.power, err = w.manager.power.Start(w.ctx, w.link.DeviceID, capture.Settings{}, w.authorize)
		}
		if err == nil {
			err = w.activate()
		}
		if err != nil {
			if w.power != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = errors.Join(err, w.power.Stop(ctx))
				cancel()
				w.power = nil
			}
			w.state.PowerState = "failed"
			w.issue("power", "DEVICE_POWER_UNAVAILABLE", "无法保持设备逻辑唤醒及实体黑屏", err)
			w.publish()
			return
		}
		w.state.PowerState = "active"
		w.issue("power", "", "", nil)
		w.manager.logger.Info("device_power_active", "deviceId", w.link.DeviceID, "captureId", w.power.ID(), "videoEnabled", false)
		w.publish()
	}
	if !w.coverAttempted {
		w.coverAttempted = true
		_, err := w.manager.previews.Get(w.ctx, w.link.DeviceID)
		if err == nil {
			w.state.CoverState = "ready"
			w.manager.logger.Info("device_initial_cover_ready", "deviceId", w.link.DeviceID, "linkGeneration", w.link.Generation)
		} else {
			w.state.CoverState = "failed"
		}
		w.issue("cover", "INITIAL_COVER_UNAVAILABLE", "无法获取设备初始封面", err)
		w.publish()
	} else if w.state.CoverState != "ready" {
		if _, err := w.manager.previews.Cached(w.ctx, w.link.DeviceID); err == nil {
			w.state.CoverState = "ready"
			w.issue("cover", "", "", nil)
			w.publish()
		}
	}
}

func (w *worker) activate() error {
	for _, message := range [...]scrcpy.ControlMessage{
		{Type: scrcpy.ControlInjectKeycode, Keycode: 224, Action: 0},
		{Type: scrcpy.ControlInjectKeycode, Keycode: 224, Action: 1},
		{Type: scrcpy.ControlSetDisplayPower, On: false},
	} {
		if err := w.power.Send(w.ctx, message, w.authorize); err != nil {
			return err
		}
	}
	// The native controller may still be finishing its standard asynchronous
	// wake-up. Read actual power state before taking the one initial cover.
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	defer cancel()
	for {
		result, err := w.manager.adb.RunShell(ctx, w.link.TransportID, "dumpsys power | grep -E '^[[:space:]]*mWakefulness='", 1024, w.authorize)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return diagnostics.New("device power state query failed")
		}
		if bytes.Contains(result.Stdout, []byte("mWakefulness=Awake")) {
			if err := w.power.Send(ctx, scrcpy.ControlMessage{Type: scrcpy.ControlSetDisplayPower, On: false}, w.authorize); err != nil {
				return err
			}
			for {
				select {
				case on, open := <-w.power.DisplayPowerChanges():
					if !open {
						return diagnostics.New("display power confirmation stream closed")
					}
					if !on {
						return w.authorize()
					}
				case <-w.power.Done():
					return diagnostics.New("device power process ended before display confirmation")
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) Close() error {
	m.cancel()
	m.wg.Wait()
	return errors.Join(m.closeErrors...)
}
