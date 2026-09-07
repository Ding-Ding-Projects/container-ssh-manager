package connection

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"io"
	"sync"
)

type terminalFrame struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	State   string `json:"state,omitempty"`
	Message string `json:"message,omitempty"`
}

// ServeTerminal keeps WebSocket writes serialized and bounds output to 256KiB.
func (m *Manager) ServeTerminal(ctx context.Context, hostID string, ws *websocket.Conn) error {
	defer ws.Close()
	c, e := m.Dial(ctx, hostID)
	if e != nil {
		return e
	}
	defer c.Close()
	s, e := c.NewSession()
	if e != nil {
		return e
	}
	defer s.Close()
	if e = s.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); e != nil {
		return e
	}
	in, e := s.StdinPipe()
	if e != nil {
		return e
	}
	out, e := s.StdoutPipe()
	if e != nil {
		return e
	}
	var mu sync.Mutex
	send := func(v terminalFrame) error { mu.Lock(); defer mu.Unlock(); return ws.WriteJSON(v) }
	if e = send(terminalFrame{Type: "status", State: "connected"}); e != nil {
		return e
	}
	if e = s.Shell(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, e := out.Read(buf)
			if n > 0 {
				_ = send(terminalFrame{Type: "output", Data: string(buf[:n])})
			}
			if e != nil {
				if e == io.EOF {
					done <- nil
				} else {
					done <- e
				}
				return
			}
		}
	}()
	for {
		select {
		case e := <-done:
			return e
		default:
			var f terminalFrame
			if e := ws.ReadJSON(&f); e != nil {
				return e
			}
			switch f.Type {
			case "input":
				if _, e := io.WriteString(in, f.Data); e != nil {
					return e
				}
			case "resize":
				if f.Cols > 0 && f.Rows > 0 {
					if e := s.WindowChange(f.Rows, f.Cols); e != nil {
						return e
					}
				}
			case "reconnect":
				if e := send(terminalFrame{Type: "status", State: "connected"}); e != nil {
					return e
				}
			}
		}
	}
}
func decodeTerminal(b []byte) (terminalFrame, error) {
	var f terminalFrame
	return f, json.Unmarshal(b, &f)
}
