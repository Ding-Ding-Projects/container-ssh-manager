package connection

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type sshFixture struct {
	listener net.Listener
	signer   ssh.Signer
	keyPEM   []byte
	active   atomic.Int32
	shells   atomic.Int32
	mu       sync.Mutex
	routes   []string
	sizes    chan [2]uint32
	clients  map[net.Conn]bool
	files    sftp.Handlers
}

func newFixture(t *testing.T) *sshFixture {
	t.Helper()
	_, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := ssh.NewSignerFromKey(key)
	if e != nil {
		t.Fatal(e)
	}
	der, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	f := &sshFixture{listener: l, signer: signer, keyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), sizes: make(chan [2]uint32, 10), clients: map[net.Conn]bool{}, files: sftp.InMemHandler()}
	t.Cleanup(func() {
		_ = l.Close()
		f.mu.Lock()
		for c := range f.clients {
			_ = c.Close()
		}
		f.mu.Unlock()
	})
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			f.mu.Lock()
			f.clients[c] = true
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	return f
}
func (f *sshFixture) serve(raw net.Conn) {
	defer raw.Close()
	defer func() { f.mu.Lock(); delete(f.clients, raw); f.mu.Unlock() }()
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == "tester" && string(p) == "fixture-password" {
			return nil, nil
		}
		return nil, errors.New("authentication refused")
	}, PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == "tester" && bytes.Equal(k.Marshal(), f.signer.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, errors.New("key refused")
	}}
	cfg.AddHostKey(f.signer)
	server, channels, requests, e := ssh.NewServerConn(raw, cfg)
	if e != nil {
		return
	}
	f.active.Add(1)
	defer f.active.Add(-1)
	defer server.Close()
	go f.forwardRequests(server, requests)
	for nc := range channels {
		switch nc.ChannelType() {
		case "session":
			ch, req, e := nc.Accept()
			if e == nil {
				go f.session(ch, req)
			}
		case "direct-tcpip":
			var q struct {
				Host       string
				Port       uint32
				Origin     string
				OriginPort uint32
			}
			if ssh.Unmarshal(nc.ExtraData(), &q) != nil {
				_ = nc.Reject(ssh.ConnectionFailed, "bad request")
				continue
			}
			addr := net.JoinHostPort(q.Host, fmt.Sprint(q.Port))
			f.mu.Lock()
			f.routes = append(f.routes, addr)
			f.mu.Unlock()
			target, e := net.DialTimeout("tcp", addr, time.Second)
			if e != nil {
				_ = nc.Reject(ssh.ConnectionFailed, "unreachable")
				continue
			}
			ch, req, e := nc.Accept()
			if e != nil {
				_ = target.Close()
				continue
			}
			go ssh.DiscardRequests(req)
			go copyChannel(ch, target)
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}
func copyChannel(ch ssh.Channel, c net.Conn) {
	defer ch.Close()
	defer c.Close()
	go func() { _, _ = io.Copy(ch, c); _ = ch.Close(); _ = c.Close() }()
	_, _ = io.Copy(c, ch)
}
func (f *sshFixture) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for r := range reqs {
		switch r.Type {
		case "pty-req":
			_ = r.Reply(true, nil)
		case "window-change":
			var q struct{ Cols, Rows, Width, Height uint32 }
			_ = ssh.Unmarshal(r.Payload, &q)
			f.sizes <- [2]uint32{q.Cols, q.Rows}
			_ = r.Reply(true, nil)
		case "shell":
			f.shells.Add(1)
			_ = r.Reply(true, nil)
			go func() {
				_, _ = io.WriteString(ch, "ready\n")
				buf := make([]byte, 4096)
				for {
					n, e := ch.Read(buf)
					if n > 0 {
						if string(buf[:n]) == "exit\n" {
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{7}))
							_ = ch.Close()
							return
						}
						_, _ = ch.Write(buf[:n])
					}
					if e != nil {
						return
					}
				}
			}()
		case "exec":
			var q struct{ Command string }
			_ = ssh.Unmarshal(r.Payload, &q)
			_ = r.Reply(true, nil)
			if q.Command == "hang" {
				go io.Copy(io.Discard, ch)
				continue
			}
			_, _ = io.WriteString(ch, "stdout\n")
			_, _ = io.WriteString(ch.Stderr(), "stderr\n")
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		case "subsystem":
			var q struct{ Name string }
			_ = ssh.Unmarshal(r.Payload, &q)
			if q.Name != "sftp" {
				_ = r.Reply(false, nil)
				continue
			}
			_ = r.Reply(true, nil)
			s := sftp.NewRequestServer(ch, f.files)
			_ = s.Serve()
			_ = s.Close()
			return
		default:
			_ = r.Reply(false, nil)
		}
	}
}
func (f *sshFixture) forwardRequests(c *ssh.ServerConn, requests <-chan *ssh.Request) {
	listeners := map[string]net.Listener{}
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for r := range requests {
		var q struct {
			Address string
			Port    uint32
		}
		_ = ssh.Unmarshal(r.Payload, &q)
		key := net.JoinHostPort(q.Address, fmt.Sprint(q.Port))
		switch r.Type {
		case "tcpip-forward":
			l, e := net.Listen("tcp", key)
			if e != nil {
				_ = r.Reply(false, nil)
				continue
			}
			port := uint32(l.Addr().(*net.TCPAddr).Port)
			key = net.JoinHostPort(q.Address, fmt.Sprint(port))
			listeners[key] = l
			_ = r.Reply(true, ssh.Marshal(struct{ Port uint32 }{port}))
			go func(l net.Listener, address string, port uint32) {
				for {
					a, e := l.Accept()
					if e != nil {
						return
					}
					origin := a.RemoteAddr().(*net.TCPAddr)
					payload := ssh.Marshal(struct {
						Address    string
						Port       uint32
						Origin     string
						OriginPort uint32
					}{address, port, origin.IP.String(), uint32(origin.Port)})
					ch, req, e := c.OpenChannel("forwarded-tcpip", payload)
					if e != nil {
						_ = a.Close()
						continue
					}
					go ssh.DiscardRequests(req)
					go copyChannel(ch, a)
				}
			}(l, q.Address, port)
		case "cancel-tcpip-forward":
			l, ok := listeners[key]
			if ok {
				_ = l.Close()
				delete(listeners, key)
			}
			_ = r.Reply(ok, nil)
		default:
			_ = r.Reply(false, nil)
		}
	}
}
func fixtureManager(t *testing.T) *Manager {
	t.Helper()
	s, e := core.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	v, e := core.NewVault(make([]byte, 32))
	if e != nil {
		t.Fatal(e)
	}
	m := New(s, v)
	m.dialTimeout = 2 * time.Second
	t.Cleanup(func() { _ = m.Close(); _ = s.Close() })
	return m
}
func addFixtureHost(t *testing.T, m *Manager, f *sshFixture, id, kind string) Host {
	t.Helper()
	secret := []byte("fixture-password")
	if kind == "privateKey" {
		secret = f.keyPEM
	}
	cred, e := m.PutCredential(CredentialMetadata{ID: id + "-credential", Name: id, Kind: kind}, secret)
	if e != nil {
		t.Fatal(e)
	}
	addr := f.listener.Addr().(*net.TCPAddr)
	h := Host{ID: id, Name: id, Address: "127.0.0.1", Port: addr.Port, User: "tester", CredentialID: cred.ID, HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(f.signer.PublicKey())))}
	if e = m.PutHost(h); e != nil {
		t.Fatal(e)
	}
	return h
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not settle")
}
func TestSSHAuthenticationHostKeysAndCancellation(t *testing.T) {
	f := newFixture(t)
	m := fixtureManager(t)
	for _, kind := range []string{"password", "privateKey"} {
		h := addFixtureHost(t, m, f, kind, kind)
		code, out, e := m.RunWithOutput(context.Background(), h.ID, "run", 1000)
		if e != nil || code != 0 || !bytes.Contains(out, []byte("stdout")) || !bytes.Contains(out, []byte("stderr")) {
			t.Fatalf("%s: %d %q %v", kind, code, out, e)
		}
	}
	h := addFixtureHost(t, m, f, "mismatch", "password")
	other := newFixture(t)
	h.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(other.signer.PublicKey())))
	if e := m.store.Put(hostKind, h.ID, h); e != nil {
		t.Fatal(e)
	}
	_, e := m.Dial(context.Background(), h.ID)
	var changed *HostKeyChangedError
	if !errors.As(e, &changed) {
		t.Fatalf("mismatch accepted: %v", e)
	}
	for _, output := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if output {
			_, _, e = m.RunWithOutput(ctx, "password", "hang", 100)
		} else {
			_, e = m.Run(ctx, "password", "hang")
		}
		cancel()
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Fatalf("cancel: %v", e)
		}
	}
	eventually(t, func() bool { return f.active.Load() == 0 })
}
func TestOrderedJumpChainClosesEveryConnection(t *testing.T) {
	a, b, c := newFixture(t), newFixture(t), newFixture(t)
	m := fixtureManager(t)
	addFixtureHost(t, m, a, "a", "password")
	addFixtureHost(t, m, b, "b", "password")
	h := addFixtureHost(t, m, c, "c", "password")
	h.JumpIDs = []string{"a", "b"}
	if e := m.PutHost(h); e != nil {
		t.Fatal(e)
	}
	client, e := m.Dial(context.Background(), "c")
	if e != nil {
		t.Fatal(e)
	}
	a.mu.Lock()
	ar := append([]string(nil), a.routes...)
	a.mu.Unlock()
	b.mu.Lock()
	br := append([]string(nil), b.routes...)
	b.mu.Unlock()
	if len(ar) != 1 || ar[0] != b.listener.Addr().String() || len(br) != 1 || br[0] != c.listener.Addr().String() {
		t.Fatalf("incorrect route %v %v", ar, br)
	}
	_ = client.Close()
	eventually(t, func() bool { return a.active.Load()+b.active.Load()+c.active.Load() == 0 })
	h.JumpIDs = []string{"c"}
	_ = m.PutHost(h)
	if _, e = m.Dial(context.Background(), "c"); e == nil {
		t.Fatal("cycle accepted")
	}
}
func TestSFTPAtomicWritesConflictsAndBounds(t *testing.T) {
	f := newFixture(t)
	m := fixtureManager(t)
	h := addFixtureHost(t, m, f, "files", "password")
	ctx := context.Background()
	if e := m.WriteFile(ctx, h.ID, "/document", []byte("initial")); e != nil {
		t.Fatal(e)
	}
	content, hash, e := m.ReadText(ctx, h.ID, "/document")
	if e != nil || content != "initial" {
		t.Fatalf("read %q %v", content, e)
	}
	if _, e = m.WriteText(ctx, h.ID, "/document", "bad", "stale"); e == nil {
		t.Fatal("stale write accepted")
	}
	next, e := m.WriteText(ctx, h.ID, "/document", "replacement", hash)
	if e != nil {
		t.Fatal(e)
	}
	content, got, e := m.ReadText(ctx, h.ID, "/document")
	if e != nil || content != "replacement" || got != next {
		t.Fatalf("replace %q %v", content, e)
	}
	entries, e := m.ListFiles(ctx, h.ID, "/")
	if e != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %#v %v", entries, e)
	}
	if e = m.WriteFile(ctx, h.ID, "/oversize", bytes.Repeat([]byte("x"), 4<<20+1)); e != nil {
		t.Fatal(e)
	}
	if _, _, e = m.ReadText(ctx, h.ID, "/oversize"); e == nil {
		t.Fatal("truncated text accepted")
	}
	for _, p := range []string{"/a/../b", "\\remote\\file", "/a\\b", "/a//b"} {
		if e = m.WriteFile(ctx, h.ID, p, []byte("x")); e == nil {
			t.Fatalf("unsafe path %q", p)
		}
	}
}
func TestLoopbackTunnelLifecycle(t *testing.T) {
	f := newFixture(t)
	m := fixtureManager(t)
	h := addFixtureHost(t, m, f, "tunnel", "password")
	echo, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	go func() {
		for {
			c, e := echo.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	for _, direction := range []string{"local", "reverse"} {
		t.Run(direction, func(t *testing.T) {
			v, e := m.StartTunnel(context.Background(), Tunnel{HostID: h.ID, Direction: direction, ListenAddress: "127.0.0.1", TargetAddress: "127.0.0.1", TargetPort: echo.Addr().(*net.TCPAddr).Port})
			if e != nil {
				t.Fatal(e)
			}
			address := net.JoinHostPort(v.ListenAddress, fmt.Sprint(v.ListenPort))
			c, e := net.DialTimeout("tcp", address, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			_, _ = c.Write([]byte("hello"))
			b := make([]byte, 5)
			if _, e = io.ReadFull(c, b); e != nil || string(b) != "hello" {
				t.Fatalf("echo %q %v", b, e)
			}
			if e = m.StopTunnel(v.ID); e != nil {
				t.Fatal(e)
			}
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			if _, e = c.Read(b); e == nil {
				t.Fatal("stream remained open")
			}
			if c, e := net.DialTimeout("tcp", address, 100*time.Millisecond); e == nil {
				c.Close()
				t.Fatal("listener remained open")
			}
		})
	}
}
func terminalConnect(t *testing.T, url, origin string) *websocket.Conn {
	t.Helper()
	h := http.Header{"Origin": []string{origin}, "Cookie": []string{"session=fixture"}}
	c, _, e := websocket.DefaultDialer.Dial(url, h)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func terminalRead(t *testing.T, c *websocket.Conn) terminalFrame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var f terminalFrame
	if e := c.ReadJSON(&f); e != nil {
		t.Fatal(e)
	}
	return f
}
func TestTerminalReconnectResizeExitAndTTL(t *testing.T) {
	f := newFixture(t)
	m := fixtureManager(t)
	m.terminals.ttl = 150 * time.Millisecond
	h := addFixtureHost(t, m, f, "terminal", "password")
	addFixtureHost(t, m, f, "other", "password")
	mux := http.NewServeMux()
	m.Register(mux)
	// The integration wrapper exercises the package behind the required root
	// authentication boundary, without depending on another lane's auth code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, e := r.Cookie("session")
		if e != nil || cookie.Value != "fixture" {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/hosts/" + h.ID + "/terminal"
	if c, r, e := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{srv.URL}}); e == nil {
		c.Close()
		t.Fatal("unauthenticated connection accepted")
	} else if r.StatusCode != 401 {
		t.Fatal(r.Status)
	}
	if c, _, e := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{"http://elsewhere"}, "Cookie": []string{"session=fixture"}}); e == nil {
		c.Close()
		t.Fatal("cross-origin connection accepted")
	}
	c := terminalConnect(t, url, srv.URL)
	status := terminalRead(t, c)
	if status.SessionID == "" {
		t.Fatal("missing session id")
	}
	id := status.SessionID
	for {
		v := terminalRead(t, c)
		if strings.Contains(v.Data, "ready") {
			break
		}
	}
	_ = c.WriteJSON(terminalFrame{Type: "resize", Cols: 101, Rows: 37})
	select {
	case size := <-f.sizes:
		if size != [2]uint32{101, 37} {
			t.Fatal(size)
		}
	case <-time.After(time.Second):
		t.Fatal("resize not delivered")
	}
	_ = c.Close()
	eventually(t, func() bool {
		m.terminals.mu.Lock()
		defer m.terminals.mu.Unlock()
		ts := m.terminals.items[id]
		if ts == nil {
			return false
		}
		ts.mu.Lock()
		defer ts.mu.Unlock()
		return ts.ws == nil
	})
	c = terminalConnect(t, url+"?session="+id, srv.URL)
	defer c.Close()
	if v := terminalRead(t, c); v.SessionID != id {
		t.Fatal("session changed")
	}
	for {
		v := terminalRead(t, c)
		if strings.Contains(v.Data, "ready") {
			break
		}
	}
	if f.shells.Load() != 1 {
		t.Fatalf("reconnect created %d shells", f.shells.Load())
	}
	other := terminalConnect(t, strings.Replace(url, "/terminal/terminal", "/other/terminal", 1)+"?session="+id, srv.URL)
	if v := terminalRead(t, other); v.State != "closed" {
		t.Fatal("host mismatch accepted")
	}
	_ = other.Close()
	_ = c.WriteJSON(terminalFrame{Type: "input", Data: "echo after reconnect\n"})
	for {
		v := terminalRead(t, c)
		if strings.Contains(v.Data, "echo after reconnect") {
			break
		}
	}
	_ = c.WriteJSON(terminalFrame{Type: "input", Data: "exit\n"})
	for {
		v := terminalRead(t, c)
		if v.Type == "status" {
			if v.State != "exited" || v.ExitCode == nil || *v.ExitCode != 7 {
				t.Fatalf("exit %#v", v)
			}
			break
		}
	}
	eventually(t, func() bool { m.terminals.mu.Lock(); defer m.terminals.mu.Unlock(); return m.terminals.items[id] == nil })
	if f.active.Load() != 0 {
		t.Fatal("terminal client leaked")
	}
}
func TestTerminalRingBoundAndCredentialUpdate(t *testing.T) {
	ts := &terminalSession{wake: make(chan struct{}, 1)}
	data := bytes.Repeat([]byte("a"), terminalBufferLimit+100)
	_, _ = ts.Write(data)
	_, _ = ts.Write([]byte("tail"))
	if len(ts.ring) != terminalBufferLimit || !bytes.HasSuffix(ts.ring, []byte("tail")) {
		t.Fatal("ring not bounded")
	}
	m := fixtureManager(t)
	f := newFixture(t)
	h := addFixtureHost(t, m, f, "update", "password")
	if _, e := m.UpdateCredential(h.CredentialID, "renamed", nil); e != nil {
		t.Fatal(e)
	}
	if _, e := m.TestHost(context.Background(), h.ID); e != nil {
		t.Fatal(e)
	}
	secret := []byte("wrong")
	if _, e := m.UpdateCredential(h.CredentialID, "renamed", &secret); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Dial(context.Background(), h.ID); e == nil {
		t.Fatal("credential replacement not used")
	}
	other := newFixture(t)
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(other.signer.PublicKey())))
	if _, e := m.EnrollHostKey(h.ID, key); e == nil {
		t.Fatal("enrollment replaced pinned key")
	}
	h.HostKey = key
	if e := m.PutHost(h); e == nil {
		t.Fatal("host update replaced pinned key")
	}
}

func TestConfiguredTerminalOriginAndCredentialHTTPUpdate(t *testing.T) {
	m := fixtureManager(t)
	m.AllowedOrigin = "https://public.example:8443"
	r := httptest.NewRequest("GET", "http://public.example:8443/terminal", nil)
	r.Header.Set("Origin", m.AllowedOrigin)
	if !m.terminalOrigin(r) {
		t.Fatal("configured proxy origin rejected")
	}
	r.Header.Set("Origin", "http://public.example:8443")
	r.Header.Set("X-Forwarded-Proto", "https")
	if m.terminalOrigin(r) {
		t.Fatal("forwarded-header origin accepted")
	}
	meta, e := m.PutCredential(CredentialMetadata{Name: "before", Kind: "password"}, []byte("fixture"))
	if e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	m.Register(mux)
	r = httptest.NewRequest("PUT", "/api/v1/credentials/"+meta.ID, strings.NewReader(`{"name":"after"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "sealed") || strings.Contains(w.Body.String(), "fixture") {
		t.Fatalf("update response %d %s", w.Code, w.Body.String())
	}
	record, secret, e := m.credential(meta.ID)
	defer clear(secret)
	if e != nil || record.Name != "after" || string(secret) != "fixture" {
		t.Fatal("metadata update lost secret")
	}
}
func TestSFTPConcurrentConflictAndFailedUploadPreservesFile(t *testing.T) {
	f := newFixture(t)
	m := fixtureManager(t)
	h := addFixtureHost(t, m, f, "concurrent", "password")
	ctx := context.Background()
	if e := m.WriteFile(ctx, h.ID, "/shared", []byte("base")); e != nil {
		t.Fatal(e)
	}
	results := make(chan error, 2)
	for _, value := range []string{"first", "second"} {
		go func(v string) { _, e := m.WriteText(ctx, h.ID, "/shared", v, HashText([]byte("base"))); results <- e }(value)
	}
	successes := 0
	for i := 0; i < 2; i++ {
		if e := <-results; e == nil {
			successes++
		} else if !strings.Contains(e.Error(), "conflict") {
			t.Fatal(e)
		}
	}
	if successes != 1 {
		t.Fatalf("%d concurrent writers succeeded", successes)
	}
	before, hash, e := m.ReadText(ctx, h.ID, "/shared")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Upload(ctx, h.ID, "/shared", io.MultiReader(strings.NewReader("partial"), failingReader{})); e == nil {
		t.Fatal("upload read error ignored")
	}
	after, got, e := m.ReadText(ctx, h.ID, "/shared")
	if e != nil || before != after || hash != got {
		t.Fatal("failed upload changed destination")
	}
	if e = m.withSFTP(ctx, h.ID, func(s *sftp.Client) error {
		return atomicRemoteWrite(s, "/shared", strings.NewReader("too much data"), 4)
	}); e == nil {
		t.Fatal("write limit ignored")
	}
	after, got, e = m.ReadText(ctx, h.ID, "/shared")
	if e != nil || before != after || hash != got {
		t.Fatal("oversized upload changed destination")
	}
	files, e := m.ListFiles(ctx, h.ID, "/")
	if e != nil || len(files) != 1 {
		t.Fatalf("temporary files remain %#v %v", files, e)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }
