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
	Store    *core.Store
	Origin   string
	Secure   bool
	mu       sync.Mutex
	attempts map[string]bucket
}

func New(s *core.Store, origin string) *Auth {
	return &Auth{Store: s, Origin: origin, Secure: true, attempts: map[string]bucket{}}
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
func (a *Auth) allowed(r *http.Request) bool {
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, v := range a.attempts {
		if now.After(v.Until) {
			delete(a.attempts, k)
		}
	}
	b := a.attempts[ip]
	if b.Until.IsZero() {
		b.Until = now.Add(15 * time.Minute)
	}
	b.Count++
	a.attempts[ip] = b
	return b.Count <= 10
}
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
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
		core.Error(w, 401, "Sign-in failed")
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
		_ = a.Store.Delete("session", key(c.Value))
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
		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/api/v1/" && r.URL.Path != "/api/v1/health" && r.URL.Path != "/api/v1/login" && !a.valid(r) {
			core.Error(w, 401, "Sign in required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
