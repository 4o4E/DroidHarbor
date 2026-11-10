package platform

import (
	"bytes"
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

func TestSessionAuthorizationLifecycle(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	secret := identity.Secret()
	var logs synchronizedLogBuffer
	app := &Server{DB: db, Admin: identity.AdminCredential{Version: "first", Hash: hex.EncodeToString(identity.Digest(secret))}, SessionTTL: time.Hour, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	app.Leases, err = leases.New(context.Background(), db, &credentialTestSource{}, app.Logger)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Leases.Close()
	server := httptest.NewTLSServer(app.Handler(http.NotFoundHandler()))
	defer server.Close()
	app.Origin = server.URL
	client := server.Client()
	send := func(method, path, body, origin, csrfToken string, cookie *http.Cookie) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("X-CSRF-Token", csrfToken)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err = response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return response, data
	}
	sessionPath := "/api/v1/admin/sessions"
	loginBody := `{"token":"` + secret + `"}`
	response, _ := send("POST", sessionPath, loginBody, "https://other.invalid", "", nil)
	if response.StatusCode != 403 {
		t.Fatalf("cross origin login %d", response.StatusCode)
	}
	response, _ = send("POST", sessionPath, `{"token":"bad"}`, server.URL, "", nil)
	if response.StatusCode != 401 {
		t.Fatalf("invalid token %d", response.StatusCode)
	}
	response, data := send("POST", sessionPath, loginBody, server.URL, "", nil)
	if response.StatusCode != 201 {
		t.Fatalf("login %d %s", response.StatusCode, data)
	}
	cookie := response.Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie")
	}
	var result struct {
		CSRF string `json:"csrfToken"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	current := sessionPath + "/current"
	response, _ = send("DELETE", current, "", server.URL, "wrong", cookie)
	if response.StatusCode != 403 {
		t.Fatalf("missing csrf %d", response.StatusCode)
	}
	response, _ = send("GET", current, "", "", "", cookie)
	if response.StatusCode != 200 {
		t.Fatalf("valid session %d", response.StatusCode)
	}
	response, _ = send("DELETE", current, "", server.URL, result.CSRF, cookie)
	if response.StatusCode != 204 {
		t.Fatalf("logout %d", response.StatusCode)
	}
	response, _ = send("GET", current, "", "", "", cookie)
	if response.StatusCode != 401 {
		t.Fatalf("revoked cookie %d", response.StatusCode)
	}
	response, _ = send("DELETE", current, "", server.URL, result.CSRF, cookie)
	if response.StatusCode != 204 {
		t.Fatalf("repeat logout %d", response.StatusCode)
	}
	response, _ = send("POST", sessionPath, loginBody, server.URL, "", nil)
	cookie = response.Cookies()[0]
	// 模拟部署换用新凭证版本；旧会话不能跟随新管理员凭证继续使用。
	if _, err = db.Exec("UPDATE admin_sessions SET admin_credential_version='old' WHERE secret_hash=?", identity.Digest(cookie.Value)); err != nil {
		t.Fatal(err)
	}
	response, _ = send("GET", current, "", "", "", cookie)
	if response.StatusCode != 401 {
		t.Fatalf("old credential version %d", response.StatusCode)
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), cookie.Value) {
		t.Fatal("secret leaked into logs")
	}
}

func TestPanicAbortsRequestWithoutLoggingSecretValue(t *testing.T) {
	var output bytes.Buffer
	app := &Server{Logger: slog.New(slog.NewJSONHandler(&output, nil))}
	server := httptest.NewUnstartedServer(app.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret-request-payload-marker")
	})))
	// Capture the standard HTTP logger too: it must not repeat the panic value.
	server.Config.ErrorLog = slog.NewLogLogger(app.Logger.Handler(), slog.LevelError)
	server.Start()
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/fault?secret-query-marker")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("panic was converted to an HTTP success")
	}
	// The abort is limited to the failed request, not the whole server.
	response, err = server.Client().Get(server.URL + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("server did not survive request panic: %d", response.StatusCode)
	}
	logs := output.String()
	for _, secret := range []string{"secret-request-payload-marker", "secret-query-marker"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("panic diagnosis leaked %q", secret)
		}
	}
	var record struct {
		Message   string `json:"msg"`
		RequestID string `json:"requestId"`
		Stack     string `json:"stack"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.Message != "request_panicked" || record.RequestID == "" || !strings.Contains(record.Stack, "TestPanicAbortsRequestWithoutLoggingSecretValue") {
		t.Fatalf("panic lost diagnostic context: %s", logs)
	}
}
