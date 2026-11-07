// Package devices maintains verified device links; discovery alone never grants allocation.
package devices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/events"
	"droidharbor/internal/leases"
)

type Evidence struct {
	Serial       string `json:"serial"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Product      string `json:"product"`
	Android      string `json:"android"`
	SDK          string `json:"sdk"`
}

// A link must agree on a hardware serial and independent product properties.
// Addresses and transport IDs are routing information, never device identity.
func (e Evidence) SameHardware(other Evidence) bool {
	return e.Serial == other.Serial && e.Manufacturer == other.Manufacturer && e.Model == other.Model && e.Product == other.Product
}

type Error struct {
	Code    string
	Message string
	Status  int
	Cause   error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

type liveLink struct {
	link     leases.Link
	serial   string
	evidence Evidence
	ctx      context.Context
	cancel   context.CancelFunc
}

type inventoryUpdate struct {
	version uint64
	epoch   context.Context
}

type registeredLink struct {
	id, deviceID, endpoint, serial string
	evidence                       sql.NullString
	enabled                        bool
}

type Registry struct {
	db          *sql.DB
	client      *adb.Client
	logger      *slog.Logger
	timeout     time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	updates     chan inventoryUpdate
	manager     *leases.Manager
	taskCleanup func(context.Context, string) (bool, error)
	events      *events.Bus

	mutations            sync.Mutex
	mu                   sync.RWMutex
	bootID               string
	version              uint64
	nextLink             uint64
	ready                bool
	epoch                context.Context
	unavailableAnnounced bool
	discovered           map[string]adb.Device
	ambiguous            map[string]bool
	live                 map[string]*liveLink
	issues               map[string]string
	configVersions       map[string]uint64
	linkStates           map[string]string
}

func New(ctx context.Context, db *sql.DB, client *adb.Client, logger *slog.Logger, timeout time.Duration) *Registry {
	lifetime, cancel := context.WithCancel(ctx)
	return &Registry{db: db, client: client, logger: logger, timeout: timeout, ctx: lifetime, cancel: cancel,
		updates: make(chan inventoryUpdate, 1), discovered: make(map[string]adb.Device), ambiguous: make(map[string]bool),
		live: make(map[string]*liveLink), issues: make(map[string]string), configVersions: make(map[string]uint64), linkStates: make(map[string]string)}
}

// SetTaskCleanup must be called before constructing the lease authority, whose
// startup recovery can run before Registry.Start. A false completion result
// keeps allocation recovery pending without converting it to an error.
func (r *Registry) SetTaskCleanup(cleanup func(context.Context, string) (bool, error)) {
	r.taskCleanup = cleanup
}

// SetEvents binds the notification bus used for device snapshot invalidation.
func (r *Registry) SetEvents(bus *events.Bus) {
	r.mu.Lock()
	r.events = bus
	r.mu.Unlock()
}

// Start is called once after the lease authority is constructed, before HTTP is served.
func (r *Registry) Start(manager *leases.Manager) {
	r.mu.Lock()
	r.manager = manager
	r.bootID = manager.BootID()
	r.mu.Unlock()
	r.wg.Add(2)
	go r.watch()
	go r.verifyInventory()
}

func (r *Registry) Close() error {
	r.cancel()
	r.wg.Wait()
	return nil
}

func (r *Registry) OnlineLinks() []leases.Link {
	r.mu.RLock()
	defer r.mu.RUnlock()
	links := make([]leases.Link, 0, len(r.live))
	for _, current := range r.live {
		if current.ctx.Err() == nil {
			links = append(links, current.link)
		}
	}
	return links
}

func (r *Registry) HasLink(link leases.Link) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	current := r.live[link.LinkID]
	return current != nil && current.link == link && current.ctx.Err() == nil
}

func (r *Registry) LinkContext(link leases.Link) (context.Context, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	current := r.live[link.LinkID]
	if current == nil || current.link != link || current.ctx.Err() != nil {
		return nil, &Error{Code: "DEVICE_DISCONNECTED", Message: "指定链路代次已失效", Status: 503}
	}
	return current.ctx, nil
}

func (r *Registry) Recover(ctx context.Context, deviceID string) (bool, error) {
	if r.taskCleanup != nil {
		complete, err := r.taskCleanup(ctx, deviceID)
		if err != nil {
			return false, err
		}
		if !complete {
			return false, nil
		}
	}
	r.mu.RLock()
	bootID := r.bootID
	r.mu.RUnlock()
	// Current-boot human/capture ownership is independent of AI leases and is
	// checked by the composite hook. Old-boot and task resources are never
	// assumed cleaned. Device presence is not itself a cleanup prerequisite.
	includeCapture := 0
	if r.taskCleanup == nil {
		includeCapture = 1
	}
	var outstanding bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owned_resources WHERE device_id=? AND state!='cleaned' AND (owner_kind='task' OR boot_id!=? OR (?=1 AND owner_kind='device-capture')))`, deviceID, bootID, includeCapture).Scan(&outstanding); err != nil {
		return false, err
	}
	if outstanding {
		return false, &Error{Code: "RESOURCE_RECOVERY_REQUIRED", Message: "设备仍有尚未确认清理的平台资源", Status: 503}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, current := range r.live {
		if current.link.DeviceID == deviceID && current.ctx.Err() == nil {
			return true, nil
		}
	}
	return true, nil
}

func (r *Registry) watch() {
	defer r.wg.Done()
	for r.ctx.Err() == nil {
		epoch, cancel := context.WithCancel(r.ctx)
		tracker, err := r.client.Track(epoch)
		if err == nil {
			for {
				devices, nextErr := tracker.Next()
				if nextErr != nil {
					err = nextErr
					break
				}
				r.publish(devices, epoch)
			}
			err = errors.Join(err, tracker.Close())
		}
		cancel()
		r.mu.Lock()
		wasReady := r.ready
		r.ready = false
		announce := !r.unavailableAnnounced
		r.unavailableAnnounced = true
		r.version++
		r.epoch = r.ctx
		for id, current := range r.live {
			current.cancel()
			delete(r.live, id)
		}
		clear(r.discovered)
		clear(r.ambiguous)
		r.enqueueLocked()
		r.mu.Unlock()
		if r.ctx.Err() != nil {
			return
		}
		if wasReady {
			r.logger.Warn("adb_inventory_disconnected", "code", "ADB_UNAVAILABLE")
		} else if err != nil && announce {
			r.logger.Warn("adb_inventory_unavailable", "code", "ADB_UNAVAILABLE")
		}
		// Resubscription observes server state only; it never reconnects a
		// device, renews a lease, or replays an Android operation.
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *Registry) publish(devices []adb.Device, epoch context.Context) {
	r.mu.Lock()
	r.version++
	r.ready = true
	r.epoch = epoch
	r.unavailableAnnounced = false
	clear(r.discovered)
	clear(r.ambiguous)
	for _, device := range devices {
		if _, duplicate := r.discovered[device.Serial]; duplicate {
			r.ambiguous[device.Serial] = true
		}
		r.discovered[device.Serial] = device
	}
	// Invalidation happens in the reader, before any SQL or identity probes.
	// Slow metadata work cannot retain a known-dead transport generation.
	for id, current := range r.live {
		observed, found := r.discovered[current.serial]
		if !found || r.ambiguous[current.serial] || observed.State != "device" || observed.TransportID != current.link.TransportID {
			current.cancel()
			delete(r.live, id)
		}
	}
	r.enqueueLocked()
	r.mu.Unlock()
}

func (r *Registry) enqueueLocked() {
	update := inventoryUpdate{version: r.version, epoch: r.epoch}
	select {
	case r.updates <- update:
	default:
		select {
		case <-r.updates:
		default:
		}
		select {
		case r.updates <- update:
		case <-r.ctx.Done():
		}
	}
}
func (r *Registry) notifyLocked(deviceID string) {
	if r.events == nil {
		return
	}
	taskID := ""
	if r.manager != nil {
		taskID = r.manager.EventTask(deviceID)
	}
	if _, err := r.events.Publish(events.Change{EntityType: "device", EntityID: deviceID, DeviceID: deviceID, TaskID: taskID}); err != nil && !errors.Is(err, events.ErrClosed) {
		r.logger.Error("device_event_publish_failed", "code", "EVENT_PUBLISH_FAILED")
	}
}

func (r *Registry) setLinkStateLocked(link registeredLink, state string) {
	if r.linkStates[link.id] == state {
		return
	}
	r.linkStates[link.id] = state
	r.notifyLocked(link.deviceID)
}

func (r *Registry) verifyInventory() {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case update := <-r.updates:
			if err := r.verifySnapshot(update); err != nil && r.ctx.Err() == nil {
				r.logger.Error("device_identity_refresh_failed", "code", "IDENTITY_UNAVAILABLE")
			}
		}
	}
}

func (r *Registry) verifySnapshot(update inventoryUpdate) error {
	rows, err := r.db.QueryContext(update.epoch, `SELECT id,device_id,endpoint,coalesce(observed_serial,endpoint),identity_evidence,enabled FROM device_links ORDER BY id`)
	if err != nil {
		return err
	}
	var registered []registeredLink
	for rows.Next() {
		var link registeredLink
		if err = rows.Scan(&link.id, &link.deviceID, &link.endpoint, &link.serial, &link.evidence, &link.enabled); err != nil {
			rows.Close()
			return err
		}
		registered = append(registered, link)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, link := range registered {
		r.mu.RLock()
		if r.version != update.version {
			r.mu.RUnlock()
			return nil
		}
		observed, found := r.discovered[link.serial]
		ambiguous := r.ambiguous[link.serial]
		old := r.live[link.id]
		state := "offline"
		if !r.ready {
			state = "unavailable"
		} else if ambiguous {
			state = "identity-conflict"
		} else if found {
			switch observed.State {
			case "device":
				state = "unverified"
			case "unauthorized", "authorizing":
				state = "unauthorized"
			}
		}
		if old != nil && old.ctx.Err() == nil {
			state = "online"
		}
		r.mu.RUnlock()
		r.mu.Lock()
		if r.version == update.version {
			r.setLinkStateLocked(link, state)
		}
		r.mu.Unlock()
		if !link.enabled || !link.evidence.Valid || !found || ambiguous || observed.State != "device" || observed.TransportID == 0 {
			continue
		}
		if old != nil && old.link.TransportID == observed.TransportID && old.ctx.Err() == nil {
			continue
		}
		var expected Evidence
		if err = json.Unmarshal([]byte(link.evidence.String), &expected); err != nil {
			return &Error{Code: "IDENTITY_INVALID", Message: "设备身份记录无效", Status: 503, Cause: err}
		}
		actual, probeErr := r.probe(update.epoch, observed.TransportID)
		if probeErr == nil && !expected.SameHardware(actual) {
			probeErr = &Error{Code: "IDENTITY_CONFLICT", Message: "设备身份与登记记录不一致", Status: 409}
		}
		if probeErr != nil {
			r.mu.Lock()
			if r.version == update.version {
				r.issues[link.id] = "设备身份尚未通过校验"
				var identityErr *Error
				if errors.As(probeErr, &identityErr) && identityErr.Code == "IDENTITY_CONFLICT" {
					r.setLinkStateLocked(link, "identity-conflict")
				} else {
					r.setLinkStateLocked(link, "unauthorized")
				}
			}
			r.mu.Unlock()
			if update.epoch.Err() != nil {
				return update.epoch.Err()
			}
			continue
		}
		if _, err = r.db.ExecContext(update.epoch, `UPDATE device_links SET last_seen_at=? WHERE id=?`, time.Now().UTC().UnixMilli(), link.id); err != nil {
			return err
		}
		r.mu.Lock()
		if r.version != update.version || !r.ready || update.epoch.Err() != nil {
			r.mu.Unlock()
			return nil
		}
		r.nextLink++
		linkCtx, cancel := context.WithCancel(update.epoch)
		r.live[link.id] = &liveLink{link: leases.Link{DeviceID: link.deviceID, LinkID: link.id, TransportID: observed.TransportID, Generation: r.nextLink}, serial: link.serial, evidence: actual, ctx: linkCtx, cancel: cancel}
		delete(r.issues, link.id)
		r.setLinkStateLocked(link, "online")
		r.mu.Unlock()
		var reclaiming bool
		if err = r.db.QueryRowContext(update.epoch, `SELECT allocation_state='reclaiming' FROM devices WHERE id=?`, link.deviceID).Scan(&reclaiming); err != nil {
			return err
		}
		if reclaiming {
			if err = r.manager.RetryRecovery(update.epoch, link.deviceID); err != nil {
				r.logger.Error("device_recovery_recheck_failed", "deviceId", link.deviceID, "code", "RECLAIM_FAILED")
			}
		}
	}
	return nil
}

func (r *Registry) probe(ctx context.Context, transportID uint64) (Evidence, error) {
	probeCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	stream, err := r.client.Open(probeCtx, transportID, "exec:getprop ro.serialno; getprop ro.product.manufacturer; getprop ro.product.model; getprop ro.product.device; getprop ro.build.version.release; getprop ro.build.version.sdk", probeCtx.Err)
	if err != nil {
		return Evidence{}, &Error{Code: "IDENTITY_UNAVAILABLE", Message: "无法读取设备身份", Status: 503, Cause: err}
	}
	data, readErr := io.ReadAll(io.LimitReader(stream, 4097))
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil || len(data) > 4096 {
		return Evidence{}, &Error{Code: "IDENTITY_UNAVAILABLE", Message: "设备身份响应不完整", Status: 503, Cause: errors.Join(readErr, closeErr)}
	}
	values := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(values) != 6 {
		return Evidence{}, &Error{Code: "IDENTITY_UNAVAILABLE", Message: "设备身份响应格式无效", Status: 503}
	}
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
		if values[i] == "" || values[i] == "unknown" {
			return Evidence{}, &Error{Code: "IDENTITY_UNAVAILABLE", Message: "设备未提供足够的可信身份属性", Status: 503}
		}
	}
	return Evidence{Serial: values[0], Manufacturer: values[1], Model: values[2], Product: values[3], Android: values[4], SDK: values[5]}, nil
}
