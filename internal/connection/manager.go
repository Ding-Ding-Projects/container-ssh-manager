// Package connection provides authenticated SSH-backed host connections.
package connection

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	store       *core.Store
	vault       *core.Vault
	tunnels     *tunnelRegistry
	dialTimeout time.Duration
	mu          sync.RWMutex
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
	return &Manager{store: store, vault: vault, tunnels: newTunnelRegistry(), dialTimeout: 20 * time.Second}
}

func (m *Manager) PutHost(h Host) error {
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
func (m *Manager) dialHost(ctx context.Context, h Host, seen map[string]bool) (*ssh.Client, error) {
	if seen[h.ID] {
		return nil, errors.New("jump host cycle")
	}
	seen[h.ID] = true
	defer delete(seen, h.ID)
	cfg, e := m.clientConfig(h)
	if e != nil {
		return nil, e
	}
	addr := net.JoinHostPort(h.Address, fmt.Sprint(h.Port))
	var conn net.Conn
	if len(h.JumpIDs) == 0 {
		d := net.Dialer{Timeout: m.dialTimeout}
		conn, e = d.DialContext(ctx, "tcp", addr)
	} else {
		// JumpIDs are ordered from the network edge toward the destination.
		var jump *ssh.Client
		for _, jumpID := range h.JumpIDs {
			j, e2 := m.Host(jumpID)
			if e2 != nil {
				return nil, e2
			}
			jcfg, e2 := m.clientConfig(j)
			if e2 != nil {
				return nil, e2
			}
			jaddr := net.JoinHostPort(j.Address, fmt.Sprint(j.Port))
			var jc net.Conn
			if jump == nil {
				jc, e2 = (&net.Dialer{Timeout: m.dialTimeout}).DialContext(ctx, "tcp", jaddr)
			} else {
				jc, e2 = jump.Dial("tcp", jaddr)
			}
			if e2 != nil {
				if jump != nil {
					_ = jump.Close()
				}
				return nil, e2
			}
			cc, ch, req, e2 := ssh.NewClientConn(jc, jaddr, jcfg)
			if e2 != nil {
				_ = jc.Close()
				if jump != nil {
					_ = jump.Close()
				}
				return nil, e2
			}
			jump = ssh.NewClient(cc, ch, req)
		}
		conn, e = jump.Dial("tcp", addr)
		if e != nil {
			_ = jump.Close()
		}
	}
	if e != nil {
		return nil, e
	}
	c, ch, req, e := ssh.NewClientConn(conn, addr, cfg)
	if e != nil {
		_ = conn.Close()
		return nil, e
	}
	return ssh.NewClient(c, ch, req), nil
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
	w        io.Writer
	n        int
	exceeded bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		w.exceeded = true
		p = p[:w.n]
	}
	n, e := w.w.Write(p)
	w.n -= n
	return len(p), e
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
	s, e := c.NewSession()
	if e != nil {
		return -1, e
	}
	defer s.Close()
	s.Stdout = io.Discard
	s.Stderr = io.Discard
	e = s.Run(command)
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
func safeRemotePath(path string) error {
	if path == "" || !strings.HasPrefix(filepath.ToSlash(path), "/") || strings.Contains(path, "\x00") {
		return errors.New("absolute non-NUL path required")
	}
	return nil
}
