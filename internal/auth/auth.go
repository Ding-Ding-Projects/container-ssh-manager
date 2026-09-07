package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"golang.org/x/crypto/bcrypt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Owner struct {
	Hash []byte `json:"hash"`
}
type session struct {
	Expires time.Time `json:"expires"`
}
type bucket struct {
	Count int
	Until time.Time
}
type Auth struct {
	Store        *core.Store
	Origin       string
	Secure       bool
	mu           sync.Mutex
	attempts     map[string]bucket
	TrustedProxy *net.IPNet
	logins       chan struct{}
}

func New(s *core.Store, origin string) *Auth {
	return &Auth{Store: s, Origin: origin, Secure: true, attempts: map[string]bucket{}, logins: make(chan struct{}, 4)}
}
func Bootstrap(s *core.Store, password []byte) error {
	h, e := bcrypt.GenerateFromPassword(password, 12)
	if e != nil {
		return e
	}
	b, e := json.Marshal(Owner{h})
	if e != nil {
		return e
	}
	_, e = s.DB.Exec(`INSERT INTO records(kind,id,data,updated_at) VALUES('owner','owner',?,?)`, string(b), time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
func key(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func (a *Auth) clientIP(r *http.Request) string {
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	if a.TrustedProxy != nil && a.TrustedProxy.Contains(net.ParseIP(ip)) {
		if client := net.ParseIP(r.Header.Get("X-Manager-Client-IP")); client != nil {
			return client.String()
		}
	}
	return ip
}
func (a *Auth) allowed(r *http.Request) bool {
	ip := a.clientIP(r)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, v := range a.attempts {
		if now.After(v.Until) {
			delete(a.attempts, k)
		}
	}
	b := a.attempts[ip]
	return b.Count < 10
}
func (a *Auth) attempt(r *http.Request, success bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ip := a.clientIP(r)
	if success {
		delete(a.attempts, ip)
		return
	}
	b := a.attempts[ip]
	if b.Until.IsZero() {
		b.Until = time.Now().Add(15 * time.Minute)
	}
	b.Count++
	a.attempts[ip] = b
}
func (a *Auth) prune() error {
	_, e := a.Store.DB.Exec(`DELETE FROM records WHERE kind='session' AND (json_extract(data,'$.expires') < ? OR id IN (SELECT id FROM records WHERE kind='session' ORDER BY updated_at DESC LIMIT -1 OFFSET 15))`, time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	select {
	case a.logins <- struct{}{}:
		defer func() { <-a.logins }()
	default:
		core.Error(w, 429, "Sign-in busy; try again shortly")
		return
	}
	if !a.allowed(r) {
		core.Error(w, 429, "Too many attempts; try again later")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if core.Decode(r, &in) != nil || len(in.Password) > 72 {
		core.Error(w, 400, "Invalid sign-in request")
		return
	}
	var owner Owner
	if a.Store.Get("owner", "owner", &owner) != nil || bcrypt.CompareHashAndPassword(owner.Hash, []byte(in.Password)) != nil {
		a.attempt(r, false)
		core.Error(w, 401, "Sign-in failed")
		return
	}
	a.attempt(r, true)
	if a.prune() != nil {
		core.Error(w, 500, "Session storage unavailable")
		return
	}
	token := core.ID() + core.ID()
	expiry := time.Now().Add(12 * time.Hour)
	if a.Store.Put("session", key(token), session{expiry}) != nil {
		core.Error(w, 500, "Session unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "manager_session", Value: token, Path: "/", HttpOnly: true, Secure: a.Secure, SameSite: http.SameSiteStrictMode, Expires: expiry})
	core.JSON(w, 200, map[string]bool{"authenticated": true})
}
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("manager_session"); e == nil {
		if a.Store.Delete("session", key(c.Value)) != nil {
			core.Error(w, 500, "Session revocation failed; retry logout")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "manager_session", Path: "/", MaxAge: -1, Secure: a.Secure, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	core.JSON(w, 200, map[string]bool{"authenticated": false})
}
func (a *Auth) valid(r *http.Request) bool {
	c, e := r.Cookie("manager_session")
	if e != nil || len(c.Value) != 64 {
		return false
	}
	var s session
	return a.Store.Get("session", key(c.Value), &s) == nil && time.Now().Before(s.Expires)
}
func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data: blob:; font-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'")
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			parsed, e := url.Parse(origin)
			if e != nil || parsed.Host == "" || subtle.ConstantTimeCompare([]byte(origin), []byte(a.Origin)) != 1 {
				core.Error(w, 403, "Request origin rejected")
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/v1/health" && r.URL.Path != "/api/v1/version" && r.URL.Path != "/api/v1/login" && !a.valid(r) {
			core.Error(w, 401, "Sign in required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
