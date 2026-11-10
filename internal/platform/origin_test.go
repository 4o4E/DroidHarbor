package platform

import (
	"net/http/httptest"
	"testing"
)

func TestOriginIsBoundToRequestHostWithoutTrustingProxyHeaders(t *testing.T) {
	for _, test := range []struct {
		origin, host, forwarded string
		allowed                 bool
	}{
		{"http://pool.internal:8080", "pool.internal:8080", "", true},
		{"https://pool.internal", "pool.internal", "", true},
		{"http://other.internal:8080", "pool.internal:8080", "other.internal:8080", false},
		{"http://pool.internal:8081", "pool.internal:8080", "", false},
		{"null", "pool.internal:8080", "", false},
		{"http://user@pool.internal:8080", "pool.internal:8080", "", false},
	} {
		r := httptest.NewRequest("POST", "http://"+test.host+"/api/v1/admin/sessions", nil)
		r.Header.Set("Origin", test.origin)
		r.Header.Set("X-Forwarded-Host", test.forwarded)
		if actual := sameRequestOrigin(r); actual != test.allowed {
			t.Errorf("origin=%q host=%q forwarded=%q allowed=%v, want %v", test.origin, test.host, test.forwarded, actual, test.allowed)
		}
	}
}
