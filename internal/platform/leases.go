package platform

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/leases"
	"droidharbor/internal/media"
	"droidharbor/internal/transfers"
)

func (s *Server) domainError(w http.ResponseWriter, r *http.Request, err error) {
	var leaseError *leases.Error
	var deviceError *devices.Error
	var mediaError *media.Error
	var transferError *transfers.Error
	var status int
	var code, message string
	switch {
	case errors.As(err, &transferError):
		status, code, message = transferError.Status, transferError.Code, transferError.Message
	case errors.As(err, &mediaError):
		status, code, message = mediaError.Status, mediaError.Code, mediaError.Message
	case errors.As(err, &leaseError):
		status, code, message = leaseError.HTTPStatus, leaseError.Code, leaseError.Message
	case errors.As(err, &deviceError):
		status, code, message = deviceError.Status, deviceError.Code, deviceError.Message
	default:
		s.internal(w, r, err)
		return
	}
	if status >= 500 {
		s.Logger.Error("operation_failed", "requestId", r.Context().Value(requestKey{}), "code", code, "cause", diagnostics.Cause(err))
	}
	s.fail(w, r, status, code, message)
}

func (s *Server) bearer(w http.ResponseWriter, r *http.Request) (string, bool) {
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || secret == "" || strings.ContainsAny(secret, " \t\r\n") {
		s.fail(w, r, 401, "INVALID_CREDENTIAL", "调用或任务凭证无效")
		return "", false
	}
	return secret, true
}

// An explicit bearer never falls back to an administrator cookie on failure.
func (s *Server) principal(w http.ResponseWriter, r *http.Request, write bool) (leases.Principal, bool) {
	if r.Header.Get("Authorization") != "" {
		secret, ok := s.bearer(w, r)
		if !ok {
			return leases.Principal{}, false
		}
		principal, err := s.Leases.Authenticate(r.Context(), secret)
		if err != nil {
			s.domainError(w, r, err)
			return leases.Principal{}, false
		}
		return principal, true
	}
	id, ok := s.authorize(w, r, write)
	return leases.Principal{Kind: leases.PrincipalAdmin, ID: id}, ok
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, value any, allowEmpty bool) bool {
	return s.decodeLimit(w, r, value, allowEmpty, 16384)
}

func (s *Server) decodeLimit(w http.ResponseWriter, r *http.Request, value any, allowEmpty bool, limit int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return true
		}
		s.fail(w, r, 422, "INVALID_REQUEST", "请求正文无效")
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		s.fail(w, r, 422, "INVALID_REQUEST", "请求包含尾随内容")
		return false
	}
	return true
}

type leaseView struct {
	LeaseID         string     `json:"leaseId"`
	DeviceID        string     `json:"deviceId"`
	Generation      int64      `json:"generation"`
	BootID          string     `json:"bootId"`
	Status          string     `json:"status"`
	ClaimedAt       time.Time  `json:"claimedAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	EndedAt         *time.Time `json:"endedAt"`
	EndReason       string     `json:"endReason,omitempty"`
	AllocationState string     `json:"allocationState"`
}

func (s *Server) leaseResponse(w http.ResponseWriter, r *http.Request, status int, lease leases.Lease) {
	var allocation string
	if err := s.DB.QueryRowContext(r.Context(), `SELECT allocation_state FROM devices WHERE id=?`, lease.DeviceID).Scan(&allocation); err != nil {
		s.internal(w, r, err)
		return
	}
	if r.Method == http.MethodDelete && allocation == "reclaiming" {
		status = http.StatusAccepted
	}
	s.json(w, status, leaseView{LeaseID: lease.ID, DeviceID: lease.DeviceID, Generation: lease.Generation, BootID: lease.BootID, Status: lease.Status, ClaimedAt: lease.ClaimedAt, ExpiresAt: lease.ExpiresAt, EndedAt: lease.EndedAt, EndReason: lease.EndReason, AllocationState: allocation})
}

func (s *Server) claimLease(w http.ResponseWriter, r *http.Request) {
	secret, ok := s.bearer(w, r)
	if !ok {
		return
	}
	var body struct {
		TaskID       string   `json:"taskId"`
		TaskSecret   string   `json:"taskSecret"`
		RequiredTags []string `json:"requiredTags"`
	}
	if !s.decode(w, r, &body, false) {
		return
	}
	lease, err := s.Leases.Claim(r.Context(), secret, leases.ClaimRequest{TaskID: body.TaskID, TaskSecret: body.TaskSecret, RequiredTags: body.RequiredTags, IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.leaseResponse(w, r, http.StatusOK, lease)
}

func (s *Server) getLease(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r, false)
	if !ok {
		return
	}
	lease, err := s.Leases.Get(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.leaseResponse(w, r, http.StatusOK, lease)
}

func (s *Server) renewLease(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r, true)
	if !ok {
		return
	}
	if !s.decode(w, r, &struct{}{}, true) {
		return
	}
	lease, err := s.Leases.Renew(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.leaseResponse(w, r, http.StatusOK, lease)
}

func (s *Server) releaseLease(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r, true)
	if !ok {
		return
	}
	lease, err := s.Leases.Release(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.leaseResponse(w, r, http.StatusOK, lease)
}
