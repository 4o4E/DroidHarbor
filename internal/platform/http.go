// Package platform 提供单服务 HTTP 入口；会话认证不依赖前端隐藏控件。
package platform

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"droidharbor/internal/connections"
	"droidharbor/internal/devices"
	"droidharbor/internal/diagnostics"
	"droidharbor/internal/events"
	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
	"droidharbor/internal/media"
	"droidharbor/internal/previews"
	"droidharbor/internal/transfers"
)

const sessionCookie = "droidharbor_admin"

type Server struct {
	DB          *sql.DB
	Admin       identity.AdminCredential
	SessionTTL  time.Duration
	Logger      *slog.Logger
	Leases      *leases.Manager
	Devices     *devices.Registry
	Connections *connections.Manager
	Events      *events.Bus
	Previews    *previews.Manager
	Media       *media.Manager
	Transfers   *transfers.Manager
}

type requestKey struct{}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

func (s *Server) Handler(static http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { s.json(w, 200, map[string]string{"status": "alive"}) })
	mux.HandleFunc("POST /api/v1/admin/sessions", s.login)
	mux.HandleFunc("GET /api/v1/admin/sessions/current", s.session)
	mux.HandleFunc("DELETE /api/v1/admin/sessions/current", s.logout)
	mux.HandleFunc("GET /api/v1/callers", s.listCallers)
	mux.HandleFunc("POST /api/v1/callers", s.createCaller)
	mux.HandleFunc("DELETE /api/v1/callers/{id}", s.revokeCaller)
	mux.HandleFunc("GET /api/v1/capabilities", s.capabilities)
	mux.HandleFunc("GET /api/v1/device-discovery", s.discoverDevices)
	mux.HandleFunc("GET /api/v1/devices", s.listDevices)
	mux.HandleFunc("GET /api/v1/devices/{id}", s.listDevices)
	mux.HandleFunc("GET /api/v1/devices/{id}/preview", s.devicePreview)
	mux.HandleFunc("POST /api/v1/devices/{id}/sessions", s.createMediaSession)
	mux.HandleFunc("GET /api/v1/devices/{id}/video-settings", s.videoSettings)
	mux.HandleFunc("PATCH /api/v1/devices/{id}/video-settings", s.patchVideoSettings)
	mux.HandleFunc("POST /api/v1/sessions/{id}/candidates", s.addMediaCandidate)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.endMediaSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}/screenshot", s.originalScreenshot)
	mux.HandleFunc("POST /api/v1/sessions/{id}/transfers", s.createTransfer)
	mux.HandleFunc("PUT /api/v1/transfers/{id}/content", s.uploadTransfer)
	mux.HandleFunc("GET /api/v1/transfers/{id}", s.getTransfer)
	mux.HandleFunc("DELETE /api/v1/transfers/{id}", s.cancelTransfer)
	mux.HandleFunc("POST /api/v1/devices", s.registerDevice)
	mux.HandleFunc("POST /api/v1/devices/discovered", s.registerDiscoveredDevice)
	mux.HandleFunc("PATCH /api/v1/devices/{id}", s.patchDevice)
	mux.HandleFunc("POST /api/v1/devices/{id}/links", s.addDeviceLink)
	mux.HandleFunc("PATCH /api/v1/devices/{id}/links/{linkId}", s.patchDeviceLink)
	mux.HandleFunc("POST /api/v1/devices/{id}/links/{linkId}/pair", s.pairDeviceLink)
	mux.HandleFunc("POST /api/v1/devices/{id}/links/{linkId}/connect", s.connectDeviceLink)
	mux.HandleFunc("POST /api/v1/devices/{id}/links/{linkId}/verify", s.verifyDeviceLink)
	mux.HandleFunc("POST /api/v1/devices/{id}/refresh", s.refreshDevice)
	mux.HandleFunc("POST /api/v1/devices/{id}/recover", s.recoverDevice)
	mux.HandleFunc("POST /api/v1/leases", s.claimLease)
	mux.HandleFunc("GET /api/v1/leases/{id}", s.getLease)
	mux.HandleFunc("POST /api/v1/leases/{id}/renew", s.renewLease)
	mux.HandleFunc("DELETE /api/v1/leases/{id}", s.releaseLease)
	mux.HandleFunc("POST /api/v1/leases/{id}/connections", s.createConnection)
	mux.HandleFunc("GET /api/v1/leases/{id}/connections/{connectionId}", s.getConnection)
	mux.HandleFunc("DELETE /api/v1/leases/{id}/connections/{connectionId}", s.deleteConnection)
	mux.HandleFunc("GET /api/v1/leases/{id}/connections/{connectionId}/tunnel", s.connectionTunnel)
	mux.HandleFunc("GET /api/v1/events", s.streamEvents)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { s.fail(w, r, 404, "NOT_FOUND", "接口不存在") })
	mux.Handle("/", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := identity.Secret()
		started := time.Now()
		defer func() {
			if value := recover(); value != nil {
				if value != http.ErrAbortHandler {
					s.Logger.Error("request_panicked", "requestId", id, "method", r.Method, "route", r.Pattern, "panicType", fmt.Sprintf("%T", value), "stack", string(debug.Stack()))
				}
				// Preserve net/http's aborted-connection behavior without letting
				// its default panic logger print a potentially secret panic value.
				panic(http.ErrAbortHandler)
			}
			s.Logger.Debug("request_completed", "requestId", id, "method", r.Method, "route", r.Pattern, "durationMs", time.Since(started).Milliseconds())
		}()
		r = r.WithContext(context.WithValue(r.Context(), requestKey{}, id))
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		// Compare against this request's Host; deployment needs no configured origin.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != "" && !sameRequestOrigin(r) {
			s.fail(w, r, 403, "ORIGIN_DENIED", "请求来源不匹配")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) json(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		s.Logger.Warn("response_write_failed", "error", err.Error())
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	id, _ := r.Context().Value(requestKey{}).(string)
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	}
	s.Logger.Log(r.Context(), level, "request_rejected", "requestId", id, "method", r.Method, "route", r.Pattern, "code", code, "status", status)
	s.json(w, status, apiError{Code: code, Message: message, RequestID: id})
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	// 数据库错误可能含绑定参数，不把原始错误写入普通运行日志。
	s.Logger.Error("storage_operation_failed", "requestId", r.Context().Value(requestKey{}), "errorType", fmtErrorType(err), "cause", diagnostics.Cause(err))
	s.fail(w, r, 503, "STORAGE_UNAVAILABLE", "持久化操作失败")
}

func fmtErrorType(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "database"
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.requireOrigin(w, r) {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		s.fail(w, r, 422, "INVALID_REQUEST", "登录请求无效")
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		s.fail(w, r, 422, "INVALID_REQUEST", "登录请求无效")
		return
	}
	if !s.Admin.Matches(body.Token) {
		s.fail(w, r, 401, "INVALID_CREDENTIAL", "管理令牌无效")
		return
	}
	secret, id := identity.Secret(), identity.Secret()
	now := time.Now().UTC()
	expires := now.Add(s.SessionTTL)
	_, err := s.DB.ExecContext(r.Context(), "INSERT INTO admin_sessions(id,secret_hash,admin_credential_version,created_at,expires_at) VALUES(?,?,?,?,?)", id, identity.Digest(secret), s.Admin.Version, now.UnixMilli(), expires.UnixMilli())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: secret, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires})
	s.json(w, 201, map[string]any{"sessionId": id, "expiresAt": expires.Format(time.RFC3339), "csrfToken": csrf(secret)})
}

func csrf(secret string) string { return hex.EncodeToString(identity.Digest("csrf:" + secret)) }

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if write && !s.requireOrigin(w, r) {
		return "", false
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		s.fail(w, r, 401, "AUTHENTICATION_REQUIRED", "请先登录")
		return "", false
	}
	var id string
	err = s.DB.QueryRowContext(r.Context(), "SELECT id FROM admin_sessions WHERE secret_hash=? AND admin_credential_version=? AND revoked_at IS NULL AND expires_at>?", identity.Digest(cookie.Value), s.Admin.Version, time.Now().UTC().UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, r, 401, "SESSION_EXPIRED", "管理会话已失效")
		return "", false
	}
	if err != nil {
		s.internal(w, r, err)
		return "", false
	}
	if write && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf(cookie.Value))) != 1 {
		s.fail(w, r, 403, "CSRF_DENIED", "请求校验失败")
		return "", false
	}
	return id, true
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authorize(w, r, false)
	if !ok {
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		s.fail(w, r, 401, "AUTHENTICATION_REQUIRED", "请先登录")
		return
	}
	s.json(w, 200, map[string]string{"sessionId": id, "csrfToken": csrf(cookie.Value)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.requireOrigin(w, r) {
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if errors.Is(err, http.ErrNoCookie) {
		w.WriteHeader(204)
		return
	}
	if err != nil {
		s.fail(w, r, 401, "AUTHENTICATION_REQUIRED", "会话无效")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf(cookie.Value))) != 1 {
		s.fail(w, r, 403, "CSRF_DENIED", "请求校验失败")
		return
	}
	var owner string
	err = s.DB.QueryRowContext(r.Context(), "UPDATE admin_sessions SET revoked_at=coalesce(revoked_at,?) WHERE secret_hash=? RETURNING id", time.Now().UTC().UnixMilli(), identity.Digest(cookie.Value)).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.internal(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	if owner != "" && s.Media != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Media.CloseAdmin(cleanup, owner); err != nil {
			s.fail(w, r, 503, "SESSION_CLEANUP_PENDING", "管理会话已撤销，设备资源回收尚未确认完成")
			return
		}
	}
	w.WriteHeader(204)
}

func (s *Server) requireOrigin(w http.ResponseWriter, r *http.Request) bool {
	if sameRequestOrigin(r) {
		return true
	}
	s.fail(w, r, 403, "ORIGIN_DENIED", "请求来源不匹配")
	return false
}

func sameRequestOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") &&
		origin.Host != "" && strings.EqualFold(origin.Host, r.Host) && origin.User == nil &&
		origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}
