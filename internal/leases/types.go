// Package leases owns lease authorization, allocation, and reclaim state.
package leases

import (
	"context"
	"net/http"
	"time"
)

const (
	PrincipalCaller = "caller"
	PrincipalTask   = "task"
	PrincipalAdmin  = "admin"

	LeaseActive = "active"
	LeaseEnded  = "ended"

	LeaseDuration = 900 * time.Second
)

// Link identifies one currently online device path. Link identity is stable
// across reconnects; transport and generation describe its current incarnation.
type Link struct {
	DeviceID    string
	LinkID      string
	TransportID uint64
	Generation  uint64
}

// DeviceSource supplies observed links and confirms complete recovery of
// platform-owned resources. A false result keeps the device reclaiming.
type DeviceSource interface {
	OnlineLinks() []Link
	HasLink(Link) bool
	Recover(context.Context, string) (complete bool, err error)
}

type Principal struct {
	Kind     string
	ID       string
	CallerID string
}

// Lease is an internal domain snapshot. It deliberately contains no bearer,
// task secret, endpoint, or connection address; callers must construct an
// explicitly authorized response rather than serialize internal identity.
type Lease struct {
	ID         string
	CallerID   string
	TaskID     string
	DeviceID   string
	BootID     string
	Generation int64
	Status     string
	ClaimedAt  time.Time
	ExpiresAt  time.Time
	EndedAt    *time.Time
	EndReason  string
}

type ClaimRequest struct {
	TaskID         string
	TaskSecret     string
	IdempotencyKey string
	RequiredTags   []string
}

// Error is the stable domain error contract consumed by the HTTP layer.
// Cause is retained for errors.Is/errors.As but is never included in Message.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other.Code != "" && e.Code == other.Code
}

var (
	ErrUnauthenticated        = &Error{Code: "INVALID_CREDENTIAL", Message: "凭证无效", HTTPStatus: http.StatusUnauthorized}
	ErrForbidden              = &Error{Code: "FORBIDDEN", Message: "无权访问该资源", HTTPStatus: http.StatusForbidden}
	ErrNotFound               = &Error{Code: "NOT_FOUND", Message: "资源不存在", HTTPStatus: http.StatusNotFound}
	ErrInvalidRequest         = &Error{Code: "INVALID_REQUEST", Message: "请求参数无效", HTTPStatus: http.StatusUnprocessableEntity}
	ErrConflict               = &Error{Code: "CONFLICT", Message: "请求与已绑定状态冲突", HTTPStatus: http.StatusConflict}
	ErrAllocationConfirmation = &Error{Code: "ALLOCATION_CONFIRMATION_REQUIRED", Message: "设备正在被 AI 使用，请确认结束当前任务后再关闭分配", HTTPStatus: http.StatusConflict}
	ErrUnavailable            = &Error{Code: "NO_MATCHING_DEVICE", Message: "没有满足条件的可用设备", HTTPStatus: http.StatusConflict}
	ErrLeaseEnded             = &Error{Code: "LEASE_ENDED", Message: "租约已结束或已过期", HTTPStatus: http.StatusGone}
	ErrDeviceOffline          = &Error{Code: "DEVICE_DISCONNECTED", Message: "租约链路当前不可用", HTTPStatus: http.StatusServiceUnavailable}
	ErrClosed                 = &Error{Code: "SERVICE_CLOSED", Message: "租约管理器已关闭", HTTPStatus: http.StatusServiceUnavailable}
	ErrStorage                = &Error{Code: "STORAGE_UNAVAILABLE", Message: "持久化操作暂不可用", HTTPStatus: http.StatusServiceUnavailable}
	ErrReclaimFailed          = &Error{Code: "RECLAIM_FAILED", Message: "平台资源回收失败，设备已隔离", HTTPStatus: http.StatusServiceUnavailable}
)

func domain(base *Error, cause error) error {
	if cause == nil {
		return base
	}
	return &Error{Code: base.Code, Message: base.Message, HTTPStatus: base.HTTPStatus, Cause: cause}
}

func leaseSnapshot(lease Lease) Lease {
	if lease.EndedAt != nil {
		ended := *lease.EndedAt
		lease.EndedAt = &ended
	}
	return lease
}
