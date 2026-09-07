package connection

import (
	"context"
	"errors"
	"fmt"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"sync"
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
	items map[string]activeTunnel
}

func newTunnelRegistry() *tunnelRegistry { return &tunnelRegistry{items: map[string]activeTunnel{}} }
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
	if t.ID == "" {
		t.ID = core.ID()
	}
	if t.Direction != "local" && t.Direction != "reverse" {
		return t, errors.New("invalid tunnel direction")
	}
	if t.ListenPort < 1 || t.ListenPort > 65535 || t.TargetPort < 1 || t.TargetPort > 65535 {
		return t, errors.New("invalid tunnel port")
	}
	if t.Direction == "local" && t.ListenAddress != "127.0.0.1" && t.ListenAddress != "::1" && t.ListenAddress != "localhost" {
		return t, errors.New("local tunnel must bind loopback")
	}
	c, e := m.Dial(ctx, t.HostID)
	if e != nil {
		return t, e
	}
	if t.Direction == "local" {
		l, e := net.Listen("tcp", net.JoinHostPort(t.ListenAddress, fmt.Sprint(t.ListenPort)))
		if e != nil {
			_ = c.Close()
			return t, e
		}
		t.State = "running"
		m.addTunnel(t, func() error { _ = l.Close(); return c.Close() })
		go func() {
			for {
				a, e := l.Accept()
				if e != nil {
					return
				}
				go func() {
					b, e := c.Dial("tcp", net.JoinHostPort(t.TargetAddress, fmt.Sprint(t.TargetPort)))
					if e != nil {
						_ = a.Close()
						return
					}
					proxy(a, b)
				}()
			}
		}()
		return t, nil
	}
	l, e := c.Listen("tcp", net.JoinHostPort(t.ListenAddress, fmt.Sprint(t.ListenPort)))
	if e != nil {
		_ = c.Close()
		return t, e
	}
	t.State = "running"
	m.addTunnel(t, func() error { _ = l.Close(); return c.Close() })
	go func() {
		for {
			a, e := l.Accept()
			if e != nil {
				return
			}
			go func() {
				b, e := net.Dial("tcp", net.JoinHostPort(t.TargetAddress, fmt.Sprint(t.TargetPort)))
				if e != nil {
					_ = a.Close()
					return
				}
				proxy(a, b)
			}()
		}
	}()
	return t, nil
}
func proxy(a, b net.Conn) { defer a.Close(); defer b.Close(); go io.Copy(a, b); io.Copy(b, a) }
func (m *Manager) addTunnel(t Tunnel, close func() error) {
	m.tunnels.mu.Lock()
	defer m.tunnels.mu.Unlock()
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

var _ *ssh.Client
