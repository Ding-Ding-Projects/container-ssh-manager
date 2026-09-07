// Package connection provides authenticated SSH-backed host connections.
package connection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"golang.org/x/crypto/ssh"
)

const hostKind = "connection.host"
const credentialKind = "connection.credential"

type Host struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Address      string   `json:"address"`
	Port         int      `json:"port"`
	User         string   `json:"user"`
	CredentialID string   `json:"credentialId"`
	HostKey      string   `json:"hostKey"`
	JumpIDs      []string `json:"jumpIds"`
	Group        string   `json:"group"`
	Tags         []string `json:"tags"`
}

type CredentialMetadata struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}
type credentialRecord struct {
	CredentialMetadata
	Sealed []byte `json:"sealed"`
}

type Manager struct {
	// AllowedOrigin is the validated public origin supplied by the root server.
	AllowedOrigin string
	store         *core.Store
	vault         *core.Vault
	tunnels       *tunnelRegistry
	dialTimeout   time.Duration
	mu            sync.RWMutex
	terminals     *terminalRegistry
	filesMu       sync.Mutex
}

type Runner interface {
	Run(context.Context, string, string) (int, error)
}
type OutputRunner interface {
	RunWithOutput(context.Context, string, string, int) (int, []byte, error)
}
type HostTestResult struct {
	Connected          bool   `json:"connected"`
	HostKey            string `json:"hostKey,omitempty"`
	EnrollmentRequired bool   `json:"enrollmentRequired"`
}
type HostKeyEnrollmentError struct{ Key string }

func (e *HostKeyEnrollmentError) Error() string { return "host key enrollment required" }

type HostKeyChangedError struct{}

func (*HostKeyChangedError) Error() string { return "host key changed" }

func New(store *core.Store, vault *core.Vault) *Manager {
	return &Manager{store: store, vault: vault, tunnels: newTunnelRegistry(), terminals: newTerminalRegistry(), dialTimeout: 20 * time.Second}
}

func (m *Manager) PutHost(h Host) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h.ID == "local" {
		return errors.New("local host is reserved")
	}
	var existing Host
	err := m.store.Get(hostKind, h.ID, &existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && h.HostKey != existing.HostKey {
		return errors.New("host key must remain unchanged")
	}
	if h.ID == "" {
		h.ID = core.ID()
	}
	if h.Name == "" || h.Address == "" || h.User == "" || h.CredentialID == "" {
		return errors.New("name, address, user and credentialId are required")
	}
	if h.Port == 0 {
		h.Port = 22
	}
	if h.Port < 1 || h.Port > 65535 {
		return errors.New("invalid port")
	}
	if strings.ContainsAny(h.Address, "\r\n") {
		return errors.New("invalid address")
	}
	return m.store.Put(hostKind, h.ID, h)
}
func (m *Manager) Host(id string) (Host, error) {
	if id == "local" {
		return Host{ID: "local", Name: "Local engine", Address: "local", Group: "local"}, nil
	}
	var h Host
	err := m.store.Get(hostKind, id, &h)
	return h, err
}
func (m *Manager) Hosts() ([]Host, error) {
	raw, e := m.store.List(hostKind)
	if e != nil {
		return nil, e
	}
	out := make([]Host, 1, len(raw)+1)
	out[0] = Host{ID: "local", Name: "Local engine", Address: "local", Group: "local"}
	for _, v := range raw {
		var h Host
		if e = json.Unmarshal(v, &h); e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	return out, nil
}
func (m *Manager) DeleteHost(id string) error { return m.store.Delete(hostKind, id) }

// EnrollHostKey is the only path that changes a saved server key.
func (m *Manager) EnrollHostKey(id, key string) (Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, err := m.Host(id)
	if err != nil {
		return Host{}, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return Host{}, errors.New("host key required")
	}
	if _, _, _, _, err = ssh.ParseAuthorizedKey([]byte(key)); err != nil {
		return Host{}, errors.New("invalid host key")
	}
	if h.HostKey != "" && hostKeyCallback(h.HostKey)("", nil, mustPublicKey(key)) != nil {
		return Host{}, &HostKeyChangedError{}
	}
	h.HostKey = key
	err = m.store.Put(hostKind, id, h)
	return h, err
}

func (m *Manager) PutCredential(meta CredentialMetadata, secret []byte) (CredentialMetadata, error) {
	if meta.ID == "" {
		meta.ID = core.ID()
	}
	if meta.Name == "" || (meta.Kind != "password" && meta.Kind != "privateKey") {
		return CredentialMetadata{}, errors.New("credential name and supported kind are required")
	}
	if len(secret) == 0 {
		return CredentialMetadata{}, errors.New("credential secret is required")
	}
	sealed, e := m.vault.Seal(secret)
	if e != nil {
		return CredentialMetadata{}, e
	}
	e = m.store.Put(credentialKind, meta.ID, credentialRecord{CredentialMetadata: meta, Sealed: sealed})
	return meta, e
}
func (m *Manager) Credentials() ([]CredentialMetadata, error) {
	raw, e := m.store.List(credentialKind)
	if e != nil {
		return nil, e
	}
	out := make([]CredentialMetadata, 0, len(raw))
	for _, v := range raw {
		var r credentialRecord
		if e = json.Unmarshal(v, &r); e != nil {
			return nil, e
		}
		out = append(out, r.CredentialMetadata)
	}
	return out, nil
}
func (m *Manager) DeleteCredential(id string) error { return m.store.Delete(credentialKind, id) }
func (m *Manager) credential(id string) (credentialRecord, []byte, error) {
	var r credentialRecord
	if e := m.store.Get(credentialKind, id, &r); e != nil {
		return r, nil, e
	}
	plain, e := m.vault.Open(r.Sealed)
	return r, plain, e
}

func (m *Manager) Dial(ctx context.Context, hostID string) (*ssh.Client, error) {
	h, e := m.Host(hostID)
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	return m.dialHost(ctx, h, seen)
}

// TestHost connects using its configured credential and reports an unpinned key
// for explicit enrollment. It never persists a discovered key automatically.
func (m *Manager) TestHost(ctx context.Context, hostID string) (HostTestResult, error) {
	c, err := m.Dial(ctx, hostID)
	if err != nil {
		var enrollment *HostKeyEnrollmentError
		if errors.As(err, &enrollment) {
			return HostTestResult{HostKey: enrollment.Key, EnrollmentRequired: true}, nil
		}
		return HostTestResult{}, err
	}
	defer c.Close()
	return HostTestResult{Connected: true}, nil
}

// ownedConn closes every jump connection when the destination is closed.
type ownedConn struct {
	net.Conn
	parents []*ssh.Client
	once    sync.Once
}

func (c *ownedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		for i := len(c.parents) - 1; i >= 0; i-- {
			_ = c.parents[i].Close()
		}
	})
	return err
}
func (m *Manager) dialHost(ctx context.Context, h Host, seen map[string]bool) (*ssh.Client, error) {
	var chain []Host
	var expand func(Host) error
	expand = func(v Host) error {
		if seen[v.ID] {
			return errors.New("jump host cycle or duplicate")
		}
		seen[v.ID] = true
		if len(seen) > 16 {
			return errors.New("jump chain exceeds 16 hosts")
		}
		for _, id := range v.JumpIDs {
			j, e := m.Host(id)
			if e != nil {
				return e
			}
			if e = expand(j); e != nil {
				return e
			}
		}
		chain = append(chain, v)
		return nil
	}
	if err := expand(h); err != nil {
		return nil, err
	}
	var clients []*ssh.Client
	success := false
	defer func() {
		if !success {
			for i := len(clients) - 1; i >= 0; i-- {
				_ = clients[i].Close()
			}
		}
	}()
	for _, host := range chain {
		cfg, err := m.clientConfig(host)
		if err != nil {
			return nil, err
		}
		addr := net.JoinHostPort(host.Address, fmt.Sprint(host.Port))
		dialCtx, cancel := context.WithTimeout(ctx, m.dialTimeout)
		var conn net.Conn
		if len(clients) == 0 {
			conn, err = (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
		} else {
			conn, err = clients[len(clients)-1].DialContext(dialCtx, "tcp", addr)
		}
		if err != nil {
			cancel()
			return nil, err
		}
		// SSH channel connections do not implement deadlines. Cancellation closes
		// the transport instead, bounding both direct and jump handshakes.
		stop := context.AfterFunc(dialCtx, func() { _ = conn.Close() })
		cc, ch, req, err := ssh.NewClientConn(&ownedConn{Conn: conn, parents: append([]*ssh.Client(nil), clients...)}, addr, cfg)
		stopped := stop()
		cancelled := dialCtx.Err()
		cancel()
		if err != nil || !stopped || cancelled != nil {
			_ = conn.Close()
			if err == nil {
				err = context.DeadlineExceeded
			}
			return nil, err
		}
		clients = append(clients, ssh.NewClient(cc, ch, req))
	}
	success = true
	return clients[len(clients)-1], nil
}
func mustPublicKey(s string) ssh.PublicKey {
	k, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(s))
	return k
}

// UpdateCredential preserves encrypted material when the secret is omitted.
func (m *Manager) UpdateCredential(id, name string, secret *[]byte) (CredentialMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var r credentialRecord
	if e := m.store.Get(credentialKind, id, &r); e != nil {
		return CredentialMetadata{}, e
	}
	if strings.TrimSpace(name) == "" {
		return CredentialMetadata{}, errors.New("credential name required")
	}
	r.Name = name
	if secret != nil {
		if len(*secret) == 0 {
			return CredentialMetadata{}, errors.New("credential secret required")
		}
		sealed, e := m.vault.Seal(*secret)
		if e != nil {
			return CredentialMetadata{}, e
		}
		r.Sealed = sealed
	}
	return r.CredentialMetadata, m.store.Put(credentialKind, id, r)
}
func (m *Manager) clientConfig(h Host) (*ssh.ClientConfig, error) {
	r, secret, e := m.credential(h.CredentialID)
	if e != nil {
		return nil, e
	}
	defer clear(secret)
	var auth ssh.AuthMethod
	switch r.Kind {
	case "password":
		auth = ssh.Password(string(secret))
	case "privateKey":
		signer, e := ssh.ParsePrivateKey(secret)
		if e != nil {
			return nil, e
		}
		auth = ssh.PublicKeys(signer)
	default:
		return nil, errors.New("unsupported credential")
	}
	return &ssh.ClientConfig{User: h.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: hostKeyCallback(h.HostKey), Timeout: m.dialTimeout}, nil
}
func hostKeyCallback(pinned string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		actual := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if pinned == "" {
			return &HostKeyEnrollmentError{Key: actual}
		}
		want, _, _, _, e := ssh.ParseAuthorizedKey([]byte(pinned))
		if e != nil {
			return fmt.Errorf("invalid stored host key: %w", e)
		}
		if string(want.Marshal()) != string(key.Marshal()) {
			return &HostKeyChangedError{}
		}
		return nil
	}
}

func (m *Manager) RunWithOutput(ctx context.Context, hostID, command string, limit int) (int, []byte, error) {
	if limit < 1 || limit > 4<<20 {
		return -1, nil, errors.New("output limit must be between 1 and 4 MiB")
	}
	c, err := m.Dial(ctx, hostID)
	if err != nil {
		return -1, nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	s, err := c.NewSession()
	if err != nil {
		return -1, nil, err
	}
	defer s.Close()
	var out strings.Builder
	writer := &limitedWriter{w: &out, n: limit}
	s.Stdout = writer
	s.Stderr = writer
	err = s.Run(command)
	if ctx.Err() != nil {
		return -1, []byte(out.String()), ctx.Err()
	}
	if writer.exceeded {
		return -1, []byte(out.String()), errors.New("command output exceeds limit")
	}
	if err == nil {
		return 0, []byte(out.String()), nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return exit.ExitStatus(), []byte(out.String()), nil
	}
	return -1, []byte(out.String()), err
}

type limitedWriter struct {
	mu       sync.Mutex
	w        io.Writer
	n        int
	exceeded bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(p)
	if len(p) > w.n {
		w.exceeded = true
		p = p[:w.n]
	}
	n, e := w.w.Write(p)
	w.n -= n
	return original, e
}
func clear(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (m *Manager) Run(ctx context.Context, hostID, command string) (int, error) {
	if strings.TrimSpace(command) == "" {
		return -1, errors.New("command required")
	}
	c, e := m.Dial(ctx, hostID)
	if e != nil {
		return -1, e
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	s, e := c.NewSession()
	if e != nil {
		return -1, e
	}
	defer s.Close()
	s.Stdout = io.Discard
	s.Stderr = io.Discard
	e = s.Run(command)
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if e == nil {
		return 0, nil
	}
	var x *ssh.ExitError
	if errors.As(e, &x) {
		return x.ExitStatus(), nil
	}
	return -1, e
}

func (m *Manager) EngineClient(ctx context.Context, hostID string) (*http.Client, string, io.Closer, error) {
	if hostID == "local" {
		path := os.Getenv("DOCKER_SOCKET")
		if path == "" {
			path = "/var/run/docker.sock"
		}
		tr := &http.Transport{DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}
		return &http.Client{Transport: tr}, "http://docker", io.NopCloser(strings.NewReader("")), nil
	}
	c, e := m.Dial(ctx, hostID)
	if e != nil {
		return nil, "", nil, e
	}
	tr := &http.Transport{DialContext: func(_ context.Context, _ string, _ string) (net.Conn, error) {
		return c.Dial("unix", "/var/run/docker.sock")
	}}
	return &http.Client{Transport: tr}, "http://docker", c, nil
}

func HashText(b []byte) string {
	s := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(s[:])
}
func safeRemotePath(p string) error {
	if strings.Contains(p, "\\") || path.Clean(p) != p {
		return errors.New("canonical POSIX path required")
	}
	path := p
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "\x00") {
		return errors.New("absolute non-NUL path required")
	}
	return nil
}
