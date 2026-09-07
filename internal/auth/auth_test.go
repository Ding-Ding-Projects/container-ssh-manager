package auth

import (
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyIdentityAndAttemptIsolation(t *testing.T) {
	a := New(nil, "https://manager.test")
	_, a.TrustedProxy, _ = net.ParseCIDR("172.30.245.3/32")
	r := httptest.NewRequest("POST", "/api/v1/login", nil)
	r.RemoteAddr = "172.30.245.3:9000"
	r.Header.Set("X-Manager-Client-IP", "192.0.2.1")
	for i := 0; i < 10; i++ {
		a.attempt(r, false)
	}
	if a.allowed(r) {
		t.Fatal("failed client not throttled")
	}
	r.Header.Set("X-Manager-Client-IP", "192.0.2.2")
	if !a.allowed(r) {
		t.Fatal("other client throttled")
	}
	for i := 0; i < 20; i++ {
		a.attempt(r, true)
	}
	if !a.allowed(r) {
		t.Fatal("success consumed attempt budget")
	}
	r.RemoteAddr = "192.0.2.3:1000"
	if a.clientIP(r) != "192.0.2.3" {
		t.Fatal("untrusted forwarded identity accepted")
	}
}

func TestOwnerAndOriginBoundaries(t *testing.T) {
	s, e := core.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = Bootstrap(s, []byte("fixture-password-123")); e != nil {
		t.Fatal(e)
	}
	if e = Bootstrap(s, []byte("overwrite-attempt-123")); e == nil {
		t.Fatal("owner replaced")
	}
	a := New(s, "https://manager.test")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login", a.Login)
	mux.HandleFunc("GET /api/v1/hosts", func(w http.ResponseWriter, r *http.Request) { core.JSON(w, 200, []string{}) })
	h := a.Wrap(mux)
	for _, tc := range []struct {
		origin string
		status int
	}{{"https://evil.test", 403}, {"", 403}, {"https://manager.test", 200}} {
		r := httptest.NewRequest("POST", "/api/v1/login", strings.NewReader(`{"password":"fixture-password-123"}`))
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("origin %q status %d", tc.origin, w.Code)
		}
		if w.Code == 200 {
			c := w.Result().Cookies()[0]
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatal("insecure cookie")
			}
			get := httptest.NewRequest("GET", "/api/v1/hosts", nil)
			get.AddCookie(c)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, get)
			if out.Code != 200 {
				t.Fatal("session failed")
			}
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/hosts", nil))
	if w.Code != 401 {
		t.Fatal("unauthenticated host access")
	}
}
