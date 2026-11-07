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
	"sync"
	"testing"
	"time"

	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
	"droidharbor/internal/storage"
)

func TestCallerCredentialLifecycleAndAuditFailure(t *testing.T) {
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "callers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	adminSecret := identity.Secret()
	var logs synchronizedLogBuffer
	app := &Server{DB: db, Admin: identity.AdminCredential{Version: "test", Hash: hex.EncodeToString(identity.Digest(adminSecret))}, SessionTTL: time.Hour, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	source := &credentialTestSource{links: []leases.Link{
		{DeviceID: "device-a1", LinkID: "link-a1", TransportID: 1, Generation: 1},
		{DeviceID: "device-a2", LinkID: "link-a2", TransportID: 2, Generation: 1},
		{DeviceID: "device-b1", LinkID: "link-b1", TransportID: 3, Generation: 1},
	}}
	app.Leases, err = leases.New(context.Background(), db, source, app.Logger)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Leases.Close()
	server := httptest.NewTLSServer(app.Handler(http.NotFoundHandler()))
	defer server.Close()
	app.Origin = server.URL
	client := server.Client()
	var cookie *http.Cookie
	var csrfToken string
	send := func(method, path, body, token, csrfValue string, admin bool) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if admin || path == "/api/v1/admin/sessions" {
			req.Header.Set("Origin", server.URL)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrfValue)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if admin {
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
		if response.StatusCode >= 400 {
			var failure apiError
			if err = json.Unmarshal(data, &failure); err != nil || failure.Code == "" || failure.RequestID == "" || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("error has no correlated request ID")
			}
		}
		return response, data
	}
	response, data := send("POST", "/api/v1/admin/sessions", `{"token":"`+adminSecret+`"}`, "", "", false)
	if response.StatusCode != 201 {
		t.Fatalf("login status %d", response.StatusCode)
	}
	cookie = response.Cookies()[0]
	var session struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err = json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	csrfToken = session.CSRFToken
	response, _ = send("POST", "/api/v1/callers", `{"name":"denied"}`, "", "wrong", true)
	if response.StatusCode != 403 {
		t.Fatalf("create without CSRF: %d", response.StatusCode)
	}
	create := func(name string) (string, string) {
		t.Helper()
		response, data := send("POST", "/api/v1/callers", `{"name":"`+name+`"}`, "", csrfToken, true)
		if response.StatusCode != 201 {
			t.Fatalf("create caller status %d", response.StatusCode)
		}
		var created struct{ ID, Token string }
		if err := json.Unmarshal(data, &created); err != nil {
			t.Fatal(err)
		}
		return created.ID, created.Token
	}
	callerA, tokenA := create("共享调用方 A")
	callerB, tokenB := create("调用方 B")
	response, data = send("GET", "/api/v1/callers", "", "", "", true)
	if response.StatusCode != 200 || bytes.Contains(data, []byte(tokenA)) || bytes.Contains(data, []byte(tokenB)) || bytes.Contains(data, []byte(`"token"`)) || bytes.Contains(data, []byte(`secret_hash`)) {
		t.Fatal("credential list exposes a secret or is unavailable")
	}
	response, _ = send("GET", "/api/v1/callers", "", tokenA, "", false)
	if response.StatusCode != 401 {
		t.Fatalf("caller gained administrator access: %d", response.StatusCode)
	}
	response, _ = send("GET", "/api/v1/capabilities", "", "invalid-secret-marker", "", true)
	if response.StatusCode != 401 {
		t.Fatal("invalid explicit bearer fell back to administrator cookie")
	}

	// Only the device source is a fixture; task binding and leases are created
	// through the actual HTTP contract, without cookies or browser Origin.
	taskSecrets := []string{identity.Secret(), identity.Secret(), identity.Secret()}
	tokens := []string{tokenA, tokenA, tokenB}
	tasks := []string{"task-a1", "task-a2", "task-b1"}
	deviceIDs := []string{"device-a1", "device-a2", "device-b1"}
	leaseIDs := make([]string, len(tasks))
	for i := range tasks {
		if _, err = db.Exec("INSERT INTO devices(id,display_name,allocation_state,maintenance_requested) VALUES(?,?,'available',0)", deviceIDs[i], deviceIDs[i]); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("INSERT INTO device_tags(device_id,tag) VALUES(?,?)", deviceIDs[i], tasks[i]); err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("POST", server.URL+"/api/v1/leases", strings.NewReader(`{"taskId":"`+tasks[i]+`","taskSecret":"`+taskSecrets[i]+`","requiredTags":["`+tasks[i]+`"]}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+tokens[i])
		request.Header.Set("Idempotency-Key", tasks[i])
		claimResponse, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var lease leaseView
		decodeErr := json.NewDecoder(claimResponse.Body).Decode(&lease)
		closeErr := claimResponse.Body.Close()
		if claimResponse.StatusCode != 200 || decodeErr != nil || closeErr != nil || lease.DeviceID != deviceIDs[i] {
			t.Fatalf("claim failed: status=%d decode=%v close=%v", claimResponse.StatusCode, decodeErr, closeErr)
		}
		leaseIDs[i] = lease.LeaseID
		response, data = send("GET", "/api/v1/capabilities", "", taskSecrets[i], "", false)
		if response.StatusCode != 200 || bytes.Contains(data, []byte(tasks[i])) || bytes.Contains(data, []byte(taskSecrets[i])) {
			t.Fatal("task capability authentication or information boundary failed")
		}
	}
	for _, secret := range []string{tokenA, taskSecrets[1]} {
		response, _ = send("GET", "/api/v1/leases/"+leaseIDs[0], "", secret, "", false)
		if response.StatusCode != 403 {
			t.Fatal("shared caller or sibling task accessed another task lease")
		}
	}
	response, _ = send("POST", "/api/v1/leases/"+leaseIDs[0]+"/renew", `{}`, taskSecrets[0], "", false)
	if response.StatusCode != 200 {
		t.Fatalf("task renewal without browser Origin failed: %d", response.StatusCode)
	}
	response, _ = send("POST", "/api/v1/leases/"+leaseIDs[0]+"/renew", `{"ttl":1}`, taskSecrets[0], "", false)
	if response.StatusCode != 422 {
		t.Fatal("renew accepted a caller-selected TTL")
	}
	response, _ = send("DELETE", "/api/v1/callers/"+callerA, "", "", "wrong", true)
	if response.StatusCode != 403 {
		t.Fatalf("revoke without CSRF: %d", response.StatusCode)
	}
	response, _ = send("GET", "/api/v1/capabilities", "", tokenA, "", false)
	if response.StatusCode != 200 {
		t.Fatal("unauthorized revoke changed caller access")
	}
	for range 2 {
		response, _ = send("DELETE", "/api/v1/callers/"+callerA, "", "", csrfToken, true)
		if response.StatusCode != 204 {
			t.Fatalf("revoke status %d", response.StatusCode)
		}
	}
	for _, secret := range []string{tokenA, taskSecrets[0], taskSecrets[1]} {
		response, _ = send("GET", "/api/v1/capabilities", "", secret, "", false)
		if response.StatusCode != 401 {
			t.Fatal("revoked caller or dependent task retained access")
		}
	}
	for i := range tasks {
		var leaseStatus, deviceStatus string
		if err = db.QueryRow("SELECT l.status,d.allocation_state FROM leases l JOIN devices d ON d.id=l.device_id WHERE l.id=?", leaseIDs[i]).Scan(&leaseStatus, &deviceStatus); err != nil {
			t.Fatal(err)
		}
		if i < 2 && (leaseStatus != "ended" || deviceStatus != "reclaiming") {
			t.Fatal("revocation did not end the lease or made an unclean device available")
		}
		if i == 2 && (leaseStatus != "active" || deviceStatus != "leased") {
			t.Fatal("revocation affected another caller")
		}
	}
	response, _ = send("GET", "/api/v1/capabilities", "", tokenB, "", false)
	if response.StatusCode != 200 {
		t.Fatal("another caller was revoked")
	}

	if _, err = db.Exec("CREATE TRIGGER reject_audit BEFORE INSERT ON operation_records BEGIN SELECT RAISE(ABORT,'sensitive-database-detail'); END"); err != nil {
		t.Fatal(err)
	}
	response, _ = send("POST", "/api/v1/callers", `{"name":"must-not-exist"}`, "", csrfToken, true)
	if response.StatusCode != 503 {
		t.Fatal("audit failure issued a credential")
	}
	var exists bool
	if err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM caller_credentials WHERE name='must-not-exist')").Scan(&exists); err != nil || exists {
		t.Fatal("failed credential creation was committed")
	}
	response, _ = send("DELETE", "/api/v1/callers/"+callerB, "", "", csrfToken, true)
	if response.StatusCode != 503 {
		t.Fatal("failed revocation persistence was reported successful")
	}
	for _, secret := range []string{tokenB, taskSecrets[2]} {
		response, _ = send("GET", "/api/v1/capabilities", "", secret, "", false)
		if response.StatusCode != 401 {
			t.Fatal("persistence failure restored revoked runtime access")
		}
	}
	if _, err = db.Exec("DROP TRIGGER reject_audit"); err != nil {
		t.Fatal(err)
	}
	response, _ = send("DELETE", "/api/v1/callers/"+callerB, "", "", csrfToken, true)
	if response.StatusCode != 204 {
		t.Fatal("explicit retry could not persist revocation")
	}
	var details string
	if err = db.QueryRow("SELECT group_concat(safe_details) FROM operation_records").Scan(&details); err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(taskSecrets, adminSecret, cookie.Value, tokenA, tokenB, "invalid-secret-marker", "sensitive-database-detail") {
		if strings.Contains(logs.String(), secret) || strings.Contains(details, secret) {
			t.Fatal("runtime or operation records contain sensitive content")
		}
	}
}

type credentialTestSource struct{ links []leases.Link }

func (s *credentialTestSource) OnlineLinks() []leases.Link                  { return s.links }
func (*credentialTestSource) Recover(context.Context, string) (bool, error) { return false, nil }

func (s *credentialTestSource) HasLink(want leases.Link) bool {
	for _, link := range s.links {
		if link == want {
			return true
		}
	}
	return false
}

type synchronizedLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}
func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
