package capture

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/leases"
)

var ErrCleanupDeferred = diagnostics.New("capture cleanup deferred")

func (m *Manager) recordCleanupFailure(id, code string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := m.setCleanupFailure(ctx, id, code); err != nil {
		m.logger.Error("capture_cleanup_failure_persistence_failed", "captureId", id, "code", code, "cause", diagnostics.Cause(err))
	}
}

func (s *Stream) authorizeSystem() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if !s.manager.source.HasLink(s.link) {
		return diagnostics.New("pinned capture link lost")
	}
	s.manager.mu.Lock()
	owner := !s.manager.closed && s.manager.streams[s.deviceID] == s
	s.manager.mu.Unlock()
	if !owner {
		return diagnostics.New("capture is no longer registered owner")
	}
	return nil
}

func (m *Manager) authorizeCleanup(ctx context.Context, link leases.Link, id, bootID string, loc locator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.source.HasLink(link) {
		return diagnostics.New("cleanup link is no longer verified")
	}
	var owner, device, storedBoot, kind, state, encoded string
	err := m.db.QueryRowContext(ctx, `SELECT owner_id,device_id,boot_id,kind,state,locator FROM owned_resources WHERE id=? AND owner_kind=?`, id, ownerKind).Scan(&owner, &device, &storedBoot, &kind, &state, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return diagnostics.New("capture cleanup ownership missing")
	}
	if err != nil {
		return err
	}
	if owner != id || device != link.DeviceID || storedBoot != bootID || kind != m.kind() || state == "cleaned" {
		return diagnostics.New("capture cleanup ownership changed")
	}
	var current locator
	if json.Unmarshal([]byte(encoded), &current) != nil || current != loc {
		return diagnostics.New("capture cleanup locator changed")
	}
	return nil
}

func (m *Manager) cleanup(ctx context.Context, s *Stream) (cleanupErr error) {
	if !s.journaled.Load() {
		return nil
	}
	defer func() {
		if cleanupErr == nil {
			return
		}
		reportCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := m.notifyResourceFailure(reportCtx, s.deviceID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("report unconfirmed capture cleanup: %w", err))
		}
	}()
	if ctx == nil {
		return diagnostics.New("cleanup context is required")
	}
	if err := ctx.Err(); err != nil {
		m.recordCleanupFailure(s.id, "CLEANUP_DEFERRED")
		return errors.Join(ErrCleanupDeferred, err)
	}
	if !m.source.HasLink(s.link) {
		m.recordCleanupFailure(s.id, "LINK_UNAVAILABLE")
		return fmt.Errorf("%w: pinned link unavailable", ErrCleanupDeferred)
	}
	cleaning, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET state='cleaning',cleanup_error=NULL WHERE id=? AND owner_kind=? AND owner_id=? AND device_id=? AND boot_id=? AND kind=? AND state!='cleaned'`, s.id, ownerKind, s.id, s.deviceID, m.bootID, m.kind())
	if err != nil {
		return err
	}
	n, err := cleaning.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return diagnostics.New("capture ownership changed before cleanup")
	}
	if err := m.cleanupLocator(ctx, s.link, s.id, m.bootID, s.resource); err != nil {
		if !m.source.HasLink(s.link) {
			m.recordCleanupFailure(s.id, "LINK_UNAVAILABLE")
			return errors.Join(ErrCleanupDeferred, err)
		}
		m.recordCleanupFailure(s.id, "CLEANUP_FAILED")
		return err
	}
	res, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET state='cleaned',cleanup_error=NULL WHERE id=? AND owner_kind=? AND owner_id=? AND device_id=? AND boot_id=? AND kind=? AND locator=? AND state!='cleaned'`, s.id, ownerKind, s.id, s.deviceID, m.bootID, m.kind(), mustJSON(s.resource))
	if err != nil {
		return err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return diagnostics.New("capture ownership changed before cleanup completion")
	}
	return nil
}

func (m *Manager) setCleanupFailure(ctx context.Context, id, code string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET state='failed',cleanup_error=? WHERE id=? AND owner_kind=? AND owner_id=? AND kind=? AND state!='cleaned'`, code, id, ownerKind, id, m.kind())
	return err
}

func (m *Manager) cleanupLocator(ctx context.Context, link leases.Link, id, bootID string, loc locator) (cleanupErr error) {
	stage := "validate"
	defer func() {
		if cleanupErr != nil {
			cleanupErr = diagnostics.WrapStage("capture_cleanup."+stage, cleanupErr)
		}
	}()
	if loc.SCID <= 0 || loc.Socket != fmt.Sprintf("scrcpy_%08x", uint32(loc.SCID)) {
		return diagnostics.New("invalid scrcpy resource locator")
	}
	name := strings.TrimSuffix(strings.TrimPrefix(loc.Jar, "/data/local/tmp/droidharbor-"), ".jar")
	if len(name) != 32 {
		return diagnostics.New("invalid scrcpy artifact locator")
	}
	if _, err := hex.DecodeString(name); err != nil {
		return diagnostics.New("invalid scrcpy artifact locator")
	}
	if loc.Jar != "/data/local/tmp/droidharbor-"+name+".jar" {
		return diagnostics.New("invalid scrcpy artifact path")
	}
	if loc.PID == 0 {
		if loc.BootUUID != "" || loc.StartTicks != 0 {
			return diagnostics.New("invalid pending scrcpy process identity")
		}
	} else if loc.PID < 1 || !validBootUUID(loc.BootUUID) || loc.StartTicks == 0 {
		return diagnostics.New("invalid scrcpy process identity")
	}
	deadline, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	var pause *time.Timer
	defer func() {
		if pause != nil {
			pause.Stop()
		}
	}()
	waitForExit := func() error {
		if pause == nil {
			pause = time.NewTimer(100 * time.Millisecond)
		} else {
			pause.Reset(100 * time.Millisecond)
		}
		select {
		case <-deadline.Done():
			return deadline.Err()
		case <-pause.C:
			return nil
		}
	}
	stage = "process_exit"
	probeCommand := processProbeCommand(loc)
	for {
		if err := m.authorizeCleanup(deadline, link, id, bootID, loc); err != nil {
			return err
		}
		result, err := m.adb.RunShell(deadline, link.TransportID, probeCommand, 32, func() error { return m.authorizeCleanup(deadline, link, id, bootID, loc) })
		if err != nil {
			// The process may be exiting while the probe runs; retry read-only
			// ownership checks within the bounded cleanup window.
			if waitErr := waitForExit(); waitErr != nil {
				return errors.Join(diagnostics.New("capture process ownership probe failed"), err, waitErr)
			}
			continue
		}
		if result.ExitCode != 0 {
			return diagnostics.New("capture process ownership probe returned an invalid status")
		}
		status := strings.TrimSpace(string(result.Stdout))
		if status == "GONE" {
			break
		}
		if status != "MATCH" {
			return diagnostics.New("capture process identity could not be verified")
		}
		if err := waitForExit(); err != nil {
			return errors.Join(diagnostics.New("capture process remained alive through cleanup deadline"), err)
		}
	}
	// The scrcpy helper restores device settings asynchronously after its
	// parent exits. A matching helper is still owned work, not a failed cleanup.
	stage = "helper_exit"
	for {
		if err := m.authorizeCleanup(deadline, link, id, bootID, loc); err != nil {
			return err
		}
		helperResult, err := m.adb.RunShell(deadline, link.TransportID, helperProbeCommand(loc.Jar), 32, func() error { return m.authorizeCleanup(deadline, link, id, bootID, loc) })
		if err != nil {
			return errors.Join(diagnostics.New("scrcpy cleanup helper ownership probe failed"), err)
		}
		if helperResult.ExitCode != 0 {
			return diagnostics.New("scrcpy cleanup helper ownership probe returned an invalid status")
		}
		status := strings.TrimSpace(string(helperResult.Stdout))
		if status == "ABSENT" {
			break
		}
		if status != "FOUND" {
			return diagnostics.New("scrcpy cleanup helper identity could not be verified")
		}
		if err := waitForExit(); err != nil {
			return errors.Join(diagnostics.New("scrcpy cleanup helper remained alive through cleanup deadline"), err)
		}
	}
	stage = "remove_artifact"
	if err := m.authorizeCleanup(deadline, link, id, bootID, loc); err != nil {
		return err
	}
	removeCommand := "rm -f -- " + shellQuote(loc.Jar) + "; [ ! -e " + shellQuote(loc.Jar) + " ] && printf REMOVED"
	removeResult, err := m.adb.RunShell(deadline, link.TransportID, removeCommand, 32, func() error { return m.authorizeCleanup(deadline, link, id, bootID, loc) })
	if err != nil {
		return errors.Join(diagnostics.New("owned scrcpy artifact removal could not be verified"), err)
	}
	if removeResult.ExitCode != 0 || strings.TrimSpace(string(removeResult.Stdout)) != "REMOVED" {
		return diagnostics.New("owned scrcpy artifact removal could not be verified")
	}
	return nil
}

func processProbeCommand(loc locator) string {
	pid := strconv.Itoa(loc.PID)
	ticks := strconv.FormatUint(loc.StartTicks, 10)
	return "boot=$(cat /proc/sys/kernel/random/boot_id) || { printf ERROR; exit; }; [ \"$boot\" = '" + loc.BootUUID + "' ] || { printf GONE; exit; }; p=/proc/" + pid + "/stat; [ -e \"$p\" ] || { printf GONE; exit; }; stat=$(cat \"$p\") || { printf ERROR; exit; }; rest=${stat##*) }; set -- $rest; [ \"${20}\" = '" + ticks + "' ] && printf MATCH || printf GONE"
}

func helperProbeCommand(jar string) string {
	q := shellQuote("CLASSPATH=" + jar)
	// Android's adbd runs as the shell uid but its environ is root-owned and
	// unreadable. Only app_process/scrcpy-server can carry the cleanup helper,
	// so inspect the readable cmdline before requiring environ access.
	return `uid=$(id -u) || { printf ERROR; exit; }; for env in /proc/[0-9]*/environ; do [ -e "$env" ] || continue; proc=${env%/environ}; first=; IFS= read -r first < "$proc/cmdline" 2>/dev/null; case "$first" in app_process*|scrcpy-server*) ;; *) continue;; esac; status=$(awk '$1 == "Uid:" { print $2; exit }' "$proc/status") || { [ -e "$proc" ] && { printf ERROR; exit; }; continue; }; [ -e "$proc" ] || continue; [ -n "$status" ] || { printf ERROR; exit; }; [ "$status" = "$uid" ] || continue; [ -r "$env" ] || { [ -e "$proc" ] && { printf ERROR; exit; }; continue; }; contents=$(tr '\000' '\n' < "$env") || { [ -e "$proc" ] && { printf ERROR; exit; }; continue; }; printf '%s\n' "$contents" | grep -Fqx ` + q + ` && { printf FOUND; exit; }; done; printf ABSENT`
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

type recoveryFlight struct {
	done     chan struct{}
	complete bool
	err      error
}

func (m *Manager) Recover(ctx context.Context, deviceID string) (complete bool, recoveryErr error) {
	if ctx == nil {
		return false, diagnostics.New("capture recovery context is required")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if m.controlOnly {
		// Previous video cleanup can restore panel power or request sleep.
		// Finish that owned work before starting the online power controller.
		complete, err := m.videoOwner.Recover(ctx, deviceID)
		if err != nil || !complete {
			return complete, err
		}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false, diagnostics.New("capture manager is closed")
	}
	if current := m.streams[deviceID]; current != nil {
		live := current.live
		m.mu.Unlock()
		if m.controlOnly {
			selected, _, err := m.source.ReadLink(ctx, deviceID)
			if err == nil && selected == current.link && live && current.ctx.Err() == nil {
				return true, nil
			}
			// Power control follows the selected incarnation. Retire its prior
			// owner before a replacement; viewing captures remain independent.
			if err := current.Stop(ctx); err != nil {
				return false, err
			}
			return m.Recover(ctx, deviceID)
		}
		return live && current.ctx.Err() == nil && m.source.HasLink(current.link), nil
	}
	if flight := m.recovering[deviceID]; flight != nil {
		m.mu.Unlock()
		select {
		case <-flight.done:
			return flight.complete, flight.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	// Foreground media recovery joins the same ownership check already running
	// after reconnect; "in progress" is not an unconfirmed cleanup result.
	flight := &recoveryFlight{done: make(chan struct{})}
	m.recovering[deviceID] = flight
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		flight.complete, flight.err = complete, recoveryErr
		delete(m.recovering, deviceID)
		close(flight.done)
		m.mu.Unlock()
	}()
	rows, err := m.db.QueryContext(ctx, `SELECT id,owner_id,boot_id,locator FROM owned_resources WHERE device_id=? AND owner_kind=? AND kind=? AND state!='cleaned'`, deviceID, ownerKind, m.kind())
	if err != nil {
		return false, err
	}
	type item struct{ id, owner, boot, encoded string }
	var items []item
	for rows.Next() {
		var x item
		if err = rows.Scan(&x.id, &x.owner, &x.boot, &x.encoded); err != nil {
			_ = rows.Close()
			return false, err
		}
		items = append(items, x)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	if len(items) == 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return true, nil
	}
	link, linkCtx, err := m.source.ReadLink(ctx, deviceID)
	if err != nil {
		var deviceErr *devices.Error
		if errors.As(err, &deviceErr) && deviceErr.Status == 503 && deviceErr.Cause == nil {
			return false, nil
		}
		return false, err
	}
	if link.DeviceID != deviceID || link.TransportID == 0 || linkCtx == nil || linkCtx.Err() != nil || !m.source.HasLink(link) {
		return false, nil
	}
	for _, x := range items {
		if x.owner != x.id || x.boot == "" {
			return false, diagnostics.New("invalid capture resource ownership journal")
		}
		var loc locator
		if err = json.Unmarshal([]byte(x.encoded), &loc); err != nil {
			return false, err
		}
		if loc.PID != 0 && (!validBootUUID(loc.BootUUID) || loc.StartTicks == 0) {
			return false, diagnostics.New("invalid capture process identity journal")
		}
		cleaning, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET state='cleaning',cleanup_error=NULL WHERE id=? AND owner_kind=? AND owner_id=? AND device_id=? AND boot_id=? AND kind=? AND locator=? AND state!='cleaned'`, x.id, ownerKind, x.id, deviceID, x.boot, m.kind(), x.encoded)
		if err != nil {
			return false, err
		}
		n, err := cleaning.RowsAffected()
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, diagnostics.New("capture resource journal changed before recovery")
		}
		if err = m.cleanupLocator(ctx, link, x.id, x.boot, loc); err != nil {
			if !m.source.HasLink(link) {
				err = errors.Join(ErrCleanupDeferred, err)
			}
			m.recordCleanupFailure(x.id, "RECOVERY_DEFERRED")
			if errors.Is(err, ErrCleanupDeferred) {
				return false, nil
			}
			return false, err
		}
		result, err := m.db.ExecContext(ctx, `UPDATE owned_resources SET state='cleaned',cleanup_error=NULL WHERE id=? AND owner_kind=? AND owner_id=? AND device_id=? AND boot_id=? AND kind=? AND locator=? AND state!='cleaned'`, x.id, ownerKind, x.id, deviceID, x.boot, m.kind(), x.encoded)
		if err != nil {
			return false, err
		}
		n, err = result.RowsAffected()
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, diagnostics.New("capture resource journal changed during recovery")
		}
	}
	return true, nil
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	streams := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		streams = append(streams, s)
	}
	m.mu.Unlock()
	m.cancel()
	var failures []error
	for _, s := range streams {
		if err := s.Stop(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
