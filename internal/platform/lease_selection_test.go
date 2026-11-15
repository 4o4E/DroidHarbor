package platform

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
	"droidharbor/internal/storage"
)

func TestExplicitNullSelectorCannotClaimAnArbitraryPhone(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	admin := identity.Secret()
	app := &Server{DB: db, Admin: identity.AdminCredential{Version: "selector", Hash: hex.EncodeToString(identity.Digest(admin))}, SessionTTL: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app.Leases, err = leases.New(context.Background(), db, &credentialTestSource{links: []leases.Link{{DeviceID: "phone", LinkID: "link", TransportID: 1, Generation: 1}}}, app.Logger)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Leases.Close()
	if _, err = db.Exec(`INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES('phone','phone','available',0)`); err != nil {
		t.Fatal(err)
	}
	handler := app.Handler(http.NotFoundHandler())
	send := func(path, body, token, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "http://localhost"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		} else {
			r.Header.Set("Origin", "http://localhost")
		}
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", identity.Secret())
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	login := send("/api/v1/admin/sessions", `{"token":"`+admin+`"}`, "", "", nil)
	if login.Code != 201 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	var session struct{ CSRFToken string }
	if err = json.Unmarshal(login.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	created := send("/api/v1/callers", `{"name":"selector"}`, "", session.CSRFToken, login.Result().Cookies()[0])
	if created.Code != 201 {
		t.Fatalf("caller: %d %s", created.Code, created.Body.String())
	}
	var caller struct{ Token string }
	if err = json.Unmarshal(created.Body.Bytes(), &caller); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{`"deviceId":null`, `"deviceName":null`, `"deviceId":""`, `"deviceName":42`, `"deviceId":null,"deviceName":"phone"`} {
		body := `{"taskId":"invalid","taskSecret":"` + identity.Secret() + `",` + selector + `}`
		w := send("/api/v1/leases", body, caller.Token, "", nil)
		if w.Code != 422 {
			t.Fatalf("invalid selector claimed a phone: %s status=%d body=%s", selector, w.Code, w.Body.String())
		}
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM leases`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid selectors allocated leases: count=%d err=%v", count, err)
	}
	w := send("/api/v1/leases", `{"taskId":"valid","taskSecret":"`+identity.Secret()+`","deviceName":"phone"}`, caller.Token, "", nil)
	var claimed leaseView
	if err = json.Unmarshal(w.Body.Bytes(), &claimed); err != nil || w.Code != 200 || claimed.DeviceID != "phone" {
		t.Fatalf("valid selection unavailable: status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
}
