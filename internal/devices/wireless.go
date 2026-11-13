package devices

import (
	"context"
	"errors"
	"strings"
	"time"

	"droidharbor/internal/adb"
	"droidharbor/internal/identity"
)

// ScanWireless reads the ADB server's LAN, not the browser's or platform
// container's interfaces. An advertisement is routing data, not hardware identity.
func (r *Registry) ScanWireless(ctx context.Context) ([]adb.MDNSService, error) {
	services, err := r.wirelessServices(ctx)
	if err != nil {
		return nil, err
	}
	registered, err := r.registeredEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	result := services[:0]
	for _, service := range services {
		serial := service.Name + "." + service.Type
		if registered[service.Endpoint] || registered[serial] || registered[serial+"."] {
			continue
		}
		result = append(result, service)
	}
	return result, nil
}

func (r *Registry) wirelessServices(ctx context.Context) ([]adb.MDNSService, error) {
	services, err := r.client.MDNSServices(ctx)
	if err != nil {
		return nil, &Error{Code: "WIRELESS_DISCOVERY_UNAVAILABLE", Message: "无法扫描无线 ADB，请检查 ADB Server 的 mDNS 支持及局域网发现状态", Status: 503, Cause: err}
	}
	return services, nil
}

func (r *Registry) checkWirelessService(ctx context.Context, selected adb.MDNSService, pairing bool) error {
	if (pairing && selected.Type != adb.MDNSPairing) || (!pairing && selected.Type != adb.MDNSLegacy && selected.Type != adb.MDNSConnect) {
		return &Error{Code: "INVALID_REQUEST", Message: "请选择对应的无线连接或配对服务", Status: 422}
	}
	services, err := r.wirelessServices(ctx)
	if err != nil {
		return err
	}
	for _, service := range services {
		if service == selected {
			return nil
		}
	}
	return &Error{Code: "WIRELESS_SERVICE_CHANGED", Message: "该无线服务已离开或地址已变化，请重新扫描", Status: 409}
}

// Network actions are recorded before ADB I/O without creating a device row.
// This allows the later authorized identity probe to associate an existing phone.
func (r *Registry) beginWirelessAction(ctx context.Context, actorID, kind string) (string, error) {
	id := identity.Secret()
	_, err := r.db.ExecContext(ctx, `INSERT INTO operation_records(id,actor_kind,actor_id,kind,status,created_at,result_code,safe_details) VALUES(?,'admin',?,?,'unknown',?,'ADB_RESULT_UNKNOWN','{}')`, id, actorID, kind, time.Now().UTC().UnixMilli())
	return id, err
}

func (r *Registry) finishWirelessAction(ctx context.Context, operationID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE operation_records SET status='succeeded',result_code='OK',finished_at=? WHERE id=?`, time.Now().UTC().UnixMilli(), operationID)
	if err != nil {
		return unknownAction(err)
	}
	return nil
}

func (r *Registry) PairWireless(ctx context.Context, actorID string, service adb.MDNSService, code string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if code == "" || strings.ContainsAny(code, ":\x00\r\n") {
		return &Error{Code: "INVALID_REQUEST", Message: "请填写手机显示的配对码", Status: 422}
	}
	if err := r.checkWirelessService(ctx, service, true); err != nil {
		return err
	}
	operationID, err := r.beginWirelessAction(ctx, actorID, "device.wireless.pair")
	if err != nil {
		return err
	}
	if _, err := r.client.Pair(ctx, service.Endpoint, code); err != nil {
		return unknownAction(err)
	}
	return r.finishWirelessAction(ctx, operationID)
}

func (r *Registry) ConnectWireless(ctx context.Context, actorID string, service adb.MDNSService) ([]Discovered, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if err := r.checkWirelessService(ctx, service, false); err != nil {
		return nil, err
	}
	operationID, err := r.beginWirelessAction(ctx, actorID, "device.wireless.connect")
	if err != nil {
		return nil, err
	}
	if _, err := r.client.Connect(ctx, service.Endpoint); err != nil {
		return nil, unknownAction(err)
	}
	if err := r.finishWirelessAction(ctx, operationID); err != nil {
		return nil, err
	}
	if err := r.refreshInventory(ctx); err != nil {
		// A tracker may publish the connected endpoint while the one-shot read
		// is in flight. Accept only that newer, authoritative observation.
		var stateError *Error
		r.mu.RLock()
		_, observed := r.discovered[service.Endpoint]
		current := r.ready && observed && !r.ambiguous[service.Endpoint]
		r.mu.RUnlock()
		if !errors.As(err, &stateError) || stateError.Code != "DEVICE_STATE_CHANGED" || !current {
			return nil, &Error{Code: "WIRELESS_INVENTORY_UNAVAILABLE", Message: "无线连接已确认，但设备清单尚未同步，请读取已发现设备；无需重复连接", Status: 503, Cause: err}
		}
	}
	return r.Discover(ctx)
}
