package platform

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"droidharbor/internal/events"
	"droidharbor/internal/identity"
)

type callerView struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"createdAt"`
	RevokedAt *string `json:"revokedAt"`
}

func (s *Server) createCaller(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		s.fail(w, r, 422, "INVALID_REQUEST", "调用凭证名称无效")
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		s.fail(w, r, 422, "INVALID_REQUEST", "请求包含尾随内容")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || utf8.RuneCountInString(body.Name) > 120 {
		s.fail(w, r, 422, "INVALID_REQUEST", "调用凭证名称须为 1 至 120 个字符")
		return
	}
	id, secret := identity.Secret(), "dhc_"+identity.Secret()
	now := time.Now().UTC()
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "INSERT INTO caller_credentials(id,name,secret_hash,created_at) VALUES(?,?,?,?)", id, body.Name, identity.Digest(secret), now.UnixMilli()); err != nil {
		s.internal(w, r, err)
		return
	}
	// 凭证与操作结果在同一事务内提交，记录不包含名称、秘密或请求正文。
	if _, err = tx.ExecContext(r.Context(), "INSERT INTO operation_records(id,actor_kind,actor_id,kind,status,created_at,finished_at,result_code,safe_details) VALUES(?,'admin',?,'caller.create','succeeded',?,?,'CREATED',?)", identity.Secret(), adminID, now.UnixMilli(), now.UnixMilli(), `{"callerId":"`+id+`"}`); err != nil {
		s.internal(w, r, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, r, err)
		return
	}
	if s.Events != nil {
		if _, err := s.Events.Publish(events.Change{EntityType: "caller", EntityID: id}); err != nil && !errors.Is(err, events.ErrClosed) {
			s.Logger.Error("state_notification_failed", "entityType", "caller")
		}
	}
	s.Logger.Info("caller_created", "requestId", r.Context().Value(requestKey{}), "callerId", id, "actorId", adminID)
	s.json(w, 201, struct {
		callerView
		Token string `json:"token"`
	}{callerView: callerView{ID: id, Name: body.Name, CreatedAt: now.Format(time.RFC3339Nano)}, Token: secret})
}

func (s *Server) listCallers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, false); !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), "SELECT id,name,created_at,revoked_at FROM caller_credentials ORDER BY created_at,id")
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer rows.Close()
	callers := []callerView{}
	for rows.Next() {
		var caller callerView
		var created int64
		var revoked sql.NullInt64
		if err = rows.Scan(&caller.ID, &caller.Name, &created, &revoked); err != nil {
			s.internal(w, r, err)
			return
		}
		caller.CreatedAt = time.UnixMilli(created).UTC().Format(time.RFC3339Nano)
		if revoked.Valid {
			value := time.UnixMilli(revoked.Int64).UTC().Format(time.RFC3339Nano)
			caller.RevokedAt = &value
		}
		callers = append(callers, caller)
	}
	if err = rows.Err(); err != nil {
		s.internal(w, r, err)
		return
	}
	s.json(w, 200, map[string]any{"callers": callers})
}

func (s *Server) revokeCaller(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.Leases.RevokeCaller(r.Context(), id, adminID); err != nil {
		s.domainError(w, r, err)
		return
	}
	s.Logger.Info("caller_revoked", "requestId", r.Context().Value(requestKey{}), "callerId", id, "actorId", adminID)
	w.WriteHeader(204)
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.principal(w, r, false); !ok {
		return
	}
	// 能力表示当前进程可提供的协议，不替代具体手机/网络的兼容验收。
	limits := make(map[string]any)
	if s.Transfers != nil {
		limits["fileTransfer"] = s.Transfers.Limits()
	}
	s.json(w, 200, map[string]any{
		"apiMajor": 1,
		"platform": map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH},
		"features": map[string]bool{
			"adminSessions": true, "callerCredentials": true,
			"deviceDiscovery": true, "leaseClaim": true, "remoteAdb": s.Connections != nil,
			"webRTC": s.Media != nil, "fileTransfers": s.Transfers != nil,
			"appium": false, "uiautomator2": false,
		},
		"limits": limits,
	})
}
