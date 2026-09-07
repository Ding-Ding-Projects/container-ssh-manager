package connection

import (
	"context"
	"errors"
	"fmt"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"io"
	"net"
	"sync"
	"time"
)

type Tunnel struct {
	ID            string `json:"id"`
	HostID        string `json:"hostId"`
	Direction     string `json:"direction"`
	ListenAddress string `json:"listenAddress"`
	ListenPort    int    `json:"listenPort"`
	TargetAddress string `json:"targetAddress"`
	TargetPort    int    `json:"targetPort"`
	State         string `json:"state"`
}
type activeTunnel struct {
	Tunnel
	close func() error
}
type tunnelRegistry struct {
	mu    sync.Mutex
	slots chan struct{}
	items map[string]activeTunnel
}

func newTunnelRegistry() *tunnelRegistry {
	return &tunnelRegistry{items: map[string]activeTunnel{}, slots: make(chan struct{}, 32)}
}
func (m *Manager) Tunnels() []Tunnel {
	m.tunnels.mu.Lock()
	defer m.tunnels.mu.Unlock()
	out := make([]Tunnel, 0, len(m.tunnels.items))
	for _, v := range m.tunnels.items {
		out = append(out, v.Tunnel)
	}
	return out
}
func (m *Manager) StartTunnel(ctx context.Context, t Tunnel) (Tunnel, error) {
	// The server owns identities, avoiding replacement of a live registry entry.
	t.ID = core.ID()
	select {
	case m.tunnels.slots <- struct{}{}:
	default:
		return t, errors.New("tunnel limit reached")
	}
	owned := false
	defer func() {
		if !owned {
			<-m.tunnels.slots
		}
	}()
	if t.Direction != "local" && t.Direction != "reverse" {
		return t, errors.New("invalid tunnel direction")
	}
	if t.ListenPort < 0 || t.ListenPort > 65535 || t.TargetPort < 1 || t.TargetPort > 65535 {
		return t, errors.New("invalid tunnel port")
	}
	if t.ListenAddress != "127.0.0.1" && t.ListenAddress != "::1" {
		return t, errors.New("tunnel must bind an explicit loopback address")
	}
	if t.TargetAddress == "" {
		return t, errors.New("target address required")
	}
	c, e := m.Dial(ctx, t.HostID)
	if e != nil {
		return t, e
	}
	setupCtx, cancel := context.WithTimeout(ctx, m.dialTimeout)
	defer cancel()
	stop := context.AfterFunc(setupCtx, func() { _ = c.Close() })
	defer stop()
	var l net.Listener
	address := net.JoinHostPort(t.ListenAddress, fmt.Sprint(t.ListenPort))
	if t.Direction == "local" {
		l, e = net.Listen("tcp", address)
	} else {
		l, e = c.Listen("tcp", address)
	}
	if e != nil {
		_ = c.Close()
		return t, e
	}
	t.ListenPort = l.Addr().(*net.TCPAddr).Port
	t.State = "running"
	var mu sync.Mutex
	active := map[net.Conn]bool{}
	closed := false
	pairs := 0
	var once sync.Once
	closeAll := func() error {
		once.Do(func() {
			mu.Lock()
			closed = true
			<-m.tunnels.slots
			for conn := range active {
				_ = conn.Close()
			}
			mu.Unlock()
			_ = l.Close()
			_ = c.Close()
		})
		return nil
	}
	owned = true
	m.addTunnel(t, closeAll)
	go func() {
		_ = c.Wait()
		_ = closeAll()
		m.tunnels.mu.Lock()
		v, ok := m.tunnels.items[t.ID]
		if ok {
			v.State = "closed"
			m.tunnels.items[t.ID] = v
		}
		m.tunnels.mu.Unlock()
	}()
	go func() {
		for {
			a, e := l.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			if closed || pairs >= 64 {
				mu.Unlock()
				_ = a.Close()
				continue
			}
			active[a] = true
			pairs++
			mu.Unlock()
			go func(a net.Conn) {
				defer func() { mu.Lock(); delete(active, a); pairs--; mu.Unlock(); _ = a.Close() }()
				target := net.JoinHostPort(t.TargetAddress, fmt.Sprint(t.TargetPort))
				dialCtx, cancel := context.WithTimeout(context.Background(), m.dialTimeout)
				defer cancel()
				var b net.Conn
				var err error
				if t.Direction == "local" {
					b, err = c.DialContext(dialCtx, "tcp", target)
				} else {
					b, err = (&net.Dialer{Timeout: 20 * time.Second}).DialContext(dialCtx, "tcp", target)
				}
				if err != nil {
					return
				}
				mu.Lock()
				if closed {
					mu.Unlock()
					_ = b.Close()
					return
				}
				active[b] = true
				mu.Unlock()
				defer func() { mu.Lock(); delete(active, b); mu.Unlock() }()
				proxy(a, b)
			}(a)
		}
	}()
	return t, nil
}
func proxy(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(a, b); _ = a.Close(); _ = b.Close(); close(done) }()
	_, _ = io.Copy(b, a)
	_ = a.Close()
	_ = b.Close()
	<-done
}
func (m *Manager) addTunnel(t Tunnel, close func() error) {
	m.tunnels.mu.Lock()
	defer m.tunnels.mu.Unlock()
	if len(m.tunnels.items) >= 64 {
		for id, v := range m.tunnels.items {
			if v.State == "closed" {
				delete(m.tunnels.items, id)
				break
			}
		}
	}
	m.tunnels.items[t.ID] = activeTunnel{Tunnel: t, close: close}
}
func (m *Manager) StopTunnel(id string) error {
	m.tunnels.mu.Lock()
	a, ok := m.tunnels.items[id]
	if ok {
		delete(m.tunnels.items, id)
	}
	m.tunnels.mu.Unlock()
	if !ok {
		return errors.New("tunnel not found")
	}
	return a.close()
}
