package media

import (
	"context"
	"errors"
	"time"

	"droidharbor/internal/capture"
	"droidharbor/internal/diagnostics"
)

const captureRetention = 15 * time.Second

type retainedCapture struct {
	hub             *hub
	source          *capture.Stream
	owner, switchID string
	until           time.Time
	timer           *time.Timer
	done            chan struct{}
	err             error // published by closing done
}

// Caller owns hub.changeMu. Only the most recently departed device is idle;
// an explicitly selected warm target is transferred to a bounded joining slot.
func (m *Manager) retainCapture(ctx context.Context, h *hub, source *capture.Stream, owner, switchID string, until time.Time) bool {
	if !until.After(time.Now()) {
		return false
	}
	if err := m.checkAdmin(ctx, owner); err != nil {
		m.deps.Logger.Warn("media_retention_not_authorized", "deviceId", h.deviceID, "code", errorCode(err), "cause", diagnostics.Cause(err))
		return false
	}
	h.mu.Lock()
	eligible := h.source == source && source != nil && h.status != "failed" && !h.cleanupPending && len(h.members) == 0
	h.mu.Unlock()
	if !eligible || !m.deps.Source.HasLink(source.Link()) {
		return false
	}
	m.mu.Lock()
	switcher := m.sessions[switchID]
	if m.closed || switcher == nil || switcher.switchCancelled {
		m.mu.Unlock()
		return false
	}
	entry := &retainedCapture{hub: h, source: source, owner: owner, switchID: switchID, until: until, done: make(chan struct{})}
	previous := m.retained
	m.watcher.Add(1)
	entry.timer = time.AfterFunc(time.Until(until), func() { m.finishRetention(entry, "expired") })
	m.retained = entry
	if previous != nil && previous.timer.Stop() {
		go m.finishRetention(previous, "evicted")
	}
	m.mu.Unlock()
	m.deps.Logger.Info("media_capture_retained", "deviceId", h.deviceID, "remainingMs", time.Until(until).Milliseconds())
	return true
}

// Claim the requested target before retaining the departing device. Without
// this transition A -> B -> A evicts A while the browser gathers its new offer.
// This never starts an encoder or extends the target's original deadline.
func (m *Manager) claimSwitchTarget(owner, switchID, deviceID string) {
	m.mu.Lock()
	previous := m.retained
	switcher := m.sessions[switchID]
	if m.closed || switcher == nil || switcher.owner != owner || switcher.switchCancelled || previous == nil || previous.hub.deviceID != deviceID || !previous.until.After(time.Now()) {
		m.mu.Unlock()
		return
	}
	entry := &retainedCapture{hub: previous.hub, source: previous.source, owner: owner, switchID: switchID, until: previous.until, done: make(chan struct{})}
	m.retained = nil
	m.joining[deviceID] = entry
	m.watcher.Add(1)
	entry.timer = time.AfterFunc(time.Until(entry.until), func() { m.finishRetention(entry, "switch_expired") })
	if previous.timer.Stop() {
		close(previous.done)
		m.watcher.Done()
	}
	m.mu.Unlock()
	m.deps.Logger.Info("media_capture_switch_pending", "deviceId", deviceID, "remainingMs", time.Until(entry.until).Milliseconds())
}

// Caller owns hub.changeMu and adds its viewer before releasing that lock.
func (m *Manager) resumeRetained(h *hub) {
	m.mu.Lock()
	entry := m.retained
	if entry != nil && entry.hub == h {
		m.retained = nil
	} else {
		entry = m.joining[h.deviceID]
		delete(m.joining, h.deviceID)
	}
	if entry == nil {
		m.mu.Unlock()
		return
	}
	cancelled := entry.timer.Stop()
	m.mu.Unlock()
	if cancelled {
		close(entry.done)
		m.watcher.Done()
	}
	m.deps.Logger.Info("media_capture_resumed", "deviceId", h.deviceID)
}

func (m *Manager) finishRetention(entry *retainedCapture, reason string) {
	defer m.watcher.Done()
	defer close(entry.done)
	h := entry.hub
	h.changeMu.Lock()
	defer h.changeMu.Unlock()
	m.mu.Lock()
	newerSameDevice := m.retained != nil && m.retained != entry && m.retained.hub == h || m.joining[h.deviceID] != nil && m.joining[h.deviceID] != entry
	if m.retained == entry {
		m.retained = nil
	}
	if m.joining[h.deviceID] == entry {
		delete(m.joining, h.deviceID)
	}
	m.mu.Unlock()
	h.mu.Lock()
	if newerSameDevice || len(h.members) != 0 || h.source != entry.source {
		h.mu.Unlock()
		return
	}
	if h.status != "failed" {
		h.status = "stopping"
	}
	h.source, h.cleanupSource, h.cleanupPending, h.effective = nil, entry.source, true, nil
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	m.deps.Logger.Info("media_retained_capture_stopping", "deviceId", h.deviceID, "reason", reason)
	entry.err = entry.source.Stop(ctx)
	h.captureStopped(entry.err, nil)
	if entry.err != nil {
		m.deps.Logger.Error("media_retained_cleanup_failed", "deviceId", h.deviceID, "reason", reason, "cause", diagnostics.Cause(entry.err))
	} else {
		m.deps.Logger.Info("media_retained_capture_stopped", "deviceId", h.deviceID, "reason", reason)
	}
	m.notifyDevice(h.deviceID)
}

func waitRetentions(ctx context.Context, entries []*retainedCapture) error {
	var result error
	for _, entry := range entries {
		select {
		case <-entry.done:
			result = errors.Join(result, entry.err)
		case <-ctx.Done():
			return errors.Join(result, problem(503, "CAPTURE_CLEANUP_PENDING", "保留采集尚未确认回收"), ctx.Err())
		}
	}
	return result
}

func (m *Manager) endRetained(ctx context.Context, owner string) error {
	m.mu.Lock()
	var entries []*retainedCapture
	if entry := m.retained; entry != nil && (owner == "" || entry.owner == owner) {
		entries = append(entries, entry)
	}
	for _, entry := range m.joining {
		if owner == "" || entry.owner == owner {
			entries = append(entries, entry)
		}
	}
	for _, entry := range entries {
		if entry.timer.Stop() {
			go m.finishRetention(entry, "authorization_or_shutdown")
		}
	}
	m.mu.Unlock()
	return waitRetentions(ctx, entries)
}

// Cancel only the two sides of this switch, not another window's later handoff.
func (m *Manager) CancelSwitch(ctx context.Context, owner, switchID, deviceID string) error {
	if err := m.checkAdmin(ctx, owner); err != nil {
		return err
	}
	m.mu.Lock()
	var switcher *session
	if candidate := m.sessions[switchID]; candidate != nil && candidate.owner == owner {
		switcher = candidate
		switcher.switchCancelled = true
	}
	var entries []*retainedCapture
	if entry := m.retained; entry != nil && entry.owner == owner {
		// A cancellation can beat the original end request. Its already-warm
		// target predates this session; another window's newer handoff does not.
		previousTarget := switcher != nil && entry.hub.deviceID == deviceID && !entry.until.Add(-captureRetention).After(switcher.created)
		if entry.switchID == switchID || previousTarget {
			entries = append(entries, entry)
		}
	}
	if entry := m.joining[deviceID]; entry != nil && entry.owner == owner && entry.switchID == switchID {
		entries = append(entries, entry)
	}
	for _, entry := range entries {
		if entry.timer.Stop() {
			go m.finishRetention(entry, "switch_cancelled")
		}
	}
	m.mu.Unlock()
	if switcher != nil {
		switcher.beginEnd(nil)
	}
	result := waitRetentions(ctx, entries)
	if switcher != nil {
		select {
		case <-switcher.done:
			result = errors.Join(result, switcher.endErr)
		case <-ctx.Done():
			result = errors.Join(result, ctx.Err())
		}
	}
	return result
}

func (m *Manager) failRetained(h *hub) {
	m.mu.Lock()
	entry := m.retained
	if entry == nil || entry.hub != h {
		entry = m.joining[h.deviceID]
	}
	if entry != nil && entry.timer.Stop() {
		go m.finishRetention(entry, "capture_failed")
	}
	m.mu.Unlock()
}
