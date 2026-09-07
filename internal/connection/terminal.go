package connection

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

const terminalBufferLimit = 256 << 10
const terminalSessionLimit = 64

type terminalFrame struct {
	Type      string `json:"type"`
	Data      string `json:"data,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
	State     string `json:"state,omitempty"`
	Message   string `json:"message,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Offset    uint64 `json:"offset,omitempty"`
	ExitCode  *int   `json:"exitCode,omitempty"`
}
type TerminalID struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type terminalRegistry struct {
	mu    sync.Mutex
	items map[string]*terminalSession
	ttl   time.Duration
}

func newTerminalRegistry() *terminalRegistry {
	return &terminalRegistry{items: map[string]*terminalSession{}, ttl: 5 * time.Minute}
}

type terminalSession struct {
	mu                sync.Mutex
	control           chan struct{}
	id, hostID, state string
	client            *ssh.Client
	session           *ssh.Session
	input             io.WriteCloser
	ring              []byte
	end               uint64
	exitCode          *int
	ws                *websocket.Conn
	wake              chan struct{}
	timer             *time.Timer
	registry          *terminalRegistry
}

func (t *terminalSession) notify() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}
func (t *terminalSession) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.end += uint64(len(p))
	if len(p) >= terminalBufferLimit {
		t.ring = append(t.ring[:0], p[len(p)-terminalBufferLimit:]...)
	} else {
		extra := len(t.ring) + len(p) - terminalBufferLimit
		if extra > 0 {
			copy(t.ring, t.ring[extra:])
			t.ring = t.ring[:len(t.ring)-extra]
		}
		t.ring = append(t.ring, p...)
	}
	t.notify()
	return len(p), nil
}
func (t *terminalSession) finish(state string, code *int) {
	t.mu.Lock()
	if t.state == "connected" {
		t.state = state
		t.exitCode = code
	}
	t.notify()
	t.mu.Unlock()
	_ = t.client.Close()
}
func (r *terminalRegistry) expire(t *terminalSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ws != nil {
		return
	}
	delete(r.items, t.id)
	t.state = "closed"
	clear(t.ring)
	t.ring = nil
	_ = t.client.Close()
}
func (m *Manager) terminal(ctx context.Context, hostID, id string) (*terminalSession, error) {
	r := m.terminals
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" {
		t, ok := r.items[id]
		if !ok || t.hostID != hostID {
			return nil, errors.New("terminal session not found for host")
		}
		return t, nil
	}
	if len(r.items) >= terminalSessionLimit {
		return nil, errors.New("terminal session limit reached")
	}
	c, e := m.Dial(ctx, hostID)
	if e != nil {
		return nil, e
	}
	setupCtx, cancel := context.WithTimeout(ctx, m.dialTimeout)
	defer cancel()
	stop := context.AfterFunc(setupCtx, func() { _ = c.Close() })
	defer stop()
	s, e := c.NewSession()
	if e != nil {
		_ = c.Close()
		return nil, e
	}
	t := &terminalSession{id: core.ID(), hostID: hostID, state: "connected", client: c, session: s, wake: make(chan struct{}, 1), registry: r, control: make(chan struct{}, 1)}
	t.input, e = s.StdinPipe()
	if e == nil {
		e = s.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{})
	}
	s.Stdout = t
	s.Stderr = t
	if e == nil {
		e = s.Shell()
	}
	if e != nil {
		_ = c.Close()
		return nil, e
	}
	r.items[t.id] = t
	t.timer = time.AfterFunc(r.ttl, func() { r.expire(t) })
	go func() {
		e := s.Wait()
		code := 0
		state := "exited"
		if e != nil {
			var exit *ssh.ExitError
			if errors.As(e, &exit) {
				code = exit.ExitStatus()
			} else {
				code = -1
				state = "closed"
			}
		}
		t.finish(state, &code)
	}()
	return t, nil
}
func (m *Manager) ServeTerminal(ctx context.Context, hostID string, ws *websocket.Conn) error {
	return m.ServeTerminalSession(ctx, hostID, "", ws)
}
func (m *Manager) ServeTerminalSession(ctx context.Context, hostID, id string, ws *websocket.Conn) error {
	defer ws.Close()
	t, e := m.terminal(ctx, hostID, id)
	if e != nil {
		_ = ws.WriteJSON(terminalFrame{Type: "status", State: "closed", Message: e.Error()})
		return e
	}
	r := m.terminals
	// Registry then session is the shared lock order for attach, detach and expiry.
	r.mu.Lock()
	t.mu.Lock()
	if r.items[t.id] != t {
		t.mu.Unlock()
		r.mu.Unlock()
		return errors.New("terminal session expired")
	}
	old := t.ws
	wake := make(chan struct{}, 1)
	t.wake = wake
	t.ws = ws
	if t.timer != nil {
		t.timer.Stop()
	}
	t.mu.Unlock()
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	defer func() {
		r.mu.Lock()
		t.mu.Lock()
		if t.ws == ws {
			t.ws = nil
			t.timer = time.AfterFunc(r.ttl, func() { r.expire(t) })
		}
		t.mu.Unlock()
		r.mu.Unlock()
	}()
	ws.SetReadLimit(64 << 10)
	frames := make(chan terminalFrame, 8)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			var f terminalFrame
			if e := ws.ReadJSON(&f); e != nil {
				readErr <- e
				return
			}
			select {
			case frames <- f:
			case <-done:
				return
			}
		}
	}()
	send := func(f terminalFrame) error {
		_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteJSON(f)
	}
	t.mu.Lock()
	initialState := t.state
	initialCode := t.exitCode
	t.mu.Unlock()
	if e = send(terminalFrame{Type: "status", State: initialState, SessionID: t.id, Message: t.id, ExitCode: initialCode}); e != nil {
		return e
	}
	var cursor uint64
	flush := func() (bool, error) {
		t.mu.Lock()
		start := t.end - uint64(len(t.ring))
		if cursor < start {
			cursor = start
		}
		state := t.state
		chunk := t.ring[cursor-start:]
		if cursor == start && start > 0 {
			for len(chunk) > 0 && !utf8.RuneStart(chunk[0]) {
				chunk = chunk[1:]
				cursor++
			}
		}
		n := len(chunk)
		if state == "connected" {
			for k := len(chunk) - 1; k >= 0 && k >= len(chunk)-utf8.UTFMax; k-- {
				if utf8.RuneStart(chunk[k]) {
					if !utf8.FullRune(chunk[k:]) {
						n = k
					}
					break
				}
			}
		}
		data := string(chunk[:n])
		cursor += uint64(n)
		code := t.exitCode
		t.mu.Unlock()
		if data != "" {
			if e := send(terminalFrame{Type: "output", Data: data, Offset: cursor}); e != nil {
				return true, e
			}
		}
		if state != "connected" {
			return true, send(terminalFrame{Type: "status", State: state, SessionID: t.id, ExitCode: code})
		}
		return false, nil
	}
	if done, e := flush(); done {
		return e
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-readErr:
			return e
		case <-wake:
			if done, e := flush(); done {
				return e
			}
		case f := <-frames:
			t.mu.Lock()
			attached := t.ws == ws
			state := t.state
			t.mu.Unlock()
			if !attached {
				return errors.New("terminal attached elsewhere")
			}
			if state != "connected" {
				if _, e := flush(); e != nil {
					return e
				}
				return nil
			}
			switch f.Type {
			case "input":
				e = t.command(func() error { _, err := io.WriteString(t.input, f.Data); return err })
			case "resize":
				if f.Cols < 1 || f.Cols > 1000 || f.Rows < 1 || f.Rows > 1000 {
					return errors.New("invalid terminal dimensions")
				}
				e = t.command(func() error { return t.session.WindowChange(f.Rows, f.Cols) })
			case "reconnect":
				e = send(terminalFrame{Type: "status", State: "connected", SessionID: t.id, Message: t.id})
			case "close":
				t.finish("closed", nil)
				_, e = flush()
				return e
			default:
				return errors.New("unknown terminal frame")
			}
			if e != nil {
				return e
			}
		}
	}
}
func decodeTerminal(b []byte) (terminalFrame, error) {
	var f terminalFrame
	e := json.Unmarshal(b, &f)
	return f, e
}

// Close releases all volatile SSH resources during server shutdown.
func (m *Manager) Close() error {
	r := m.terminals
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, t := range r.items {
		t.mu.Lock()
		if t.timer != nil {
			t.timer.Stop()
		}
		if t.ws != nil {
			_ = t.ws.Close()
		}
		t.state = "closed"
		clear(t.ring)
		t.ring = nil
		_ = t.client.Close()
		t.mu.Unlock()
		delete(r.items, id)
	}
	for _, t := range m.Tunnels() {
		_ = m.StopTunnel(t.ID)
	}
	return nil
}

// A blocked peer cannot accumulate input writers through repeated reconnects.
func (t *terminalSession) command(fn func() error) error {
	select {
	case t.control <- struct{}{}:
	default:
		return errors.New("terminal input busy")
	}
	done := make(chan error, 1)
	go func() { defer func() { <-t.control }(); done <- fn() }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case e := <-done:
		return e
	case <-timer.C:
		t.finish("closed", nil)
		return errors.New("terminal input timed out")
	}
}
