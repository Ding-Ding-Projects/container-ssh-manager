package connection

import (
	"context"
	"errors"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (m *Manager) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/hosts", m.hostsHandler)
	mux.HandleFunc("/api/v1/hosts/", m.hostHandler)
	mux.HandleFunc("/api/v1/credentials", m.credentialsHandler)
	mux.HandleFunc("/api/v1/credentials/", m.credentialHandler)
	mux.HandleFunc("/api/v1/tunnels", m.tunnelsHandler)
	mux.HandleFunc("/api/v1/tunnels/", m.tunnelHandler)
}
func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method != allowed {
		core.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}
func idAfter(p, base string) string { return strings.Trim(strings.TrimPrefix(p, base), "/") }
func (m *Manager) hostsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		v, e := m.Hosts()
		respond(w, e, v)
	case http.MethodPost:
		var h Host
		e := core.Decode(r, &h)
		if e == nil {
			e = m.PutHost(h)
		}
		if e != nil {
			core.Error(w, 400, e.Error())
			return
		}
		core.JSON(w, 201, h)
	default:
		core.Error(w, 405, "method not allowed")
	}
}
func (m *Manager) hostHandler(w http.ResponseWriter, r *http.Request) {
	p := idAfter(r.URL.Path, "/api/v1/hosts/")
	parts := strings.Split(p, "/")
	if len(parts) == 0 || parts[0] == "" {
		core.Error(w, 404, "not found")
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h, e := m.Host(id)
			respond(w, e, h)
		case http.MethodPut:
			var h Host
			e := core.Decode(r, &h)
			if e == nil {
				h.ID = id
				e = m.PutHost(h)
			}
			if e != nil {
				core.Error(w, 400, e.Error())
				return
			}
			core.JSON(w, 200, h)
		case http.MethodDelete:
			respond(w, m.DeleteHost(id), nil)
		default:
			core.Error(w, 405, "method not allowed")
		}
		return
	}
	switch strings.Join(parts[1:], "/") {
	case "test":
		if !method(w, r, "POST") {
			return
		}
		v, err := m.TestHost(r.Context(), id)
		if err != nil {
			hostError(w, err)
			return
		}
		core.JSON(w, http.StatusOK, v)
	case "enroll-host-key":
		if !method(w, r, "POST") {
			return
		}
		var q struct {
			HostKey string `json:"hostKey"`
		}
		if err := core.Decode(r, &q); err != nil {
			core.Error(w, 400, err.Error())
			return
		}
		h, err := m.EnrollHostKey(id, q.HostKey)
		if err != nil {
			hostError(w, err)
			return
		}
		core.JSON(w, http.StatusOK, h)
	case "run":
		if !method(w, r, "POST") {
			return
		}
		var q struct {
			Command string `json:"command"`
		}
		if e := core.Decode(r, &q); e != nil {
			core.Error(w, 400, e.Error())
			return
		}
		code, e := m.Run(r.Context(), id, q.Command)
		if e != nil {
			core.Error(w, 502, e.Error())
			return
		}
		core.JSON(w, 200, map[string]int{"exitCode": code})
	case "terminal":
		m.terminalHandler(w, r, id)
	case "files", "files/content", "files/download":
		m.filesHandler(w, r, id)
	default:
		core.Error(w, 404, "not found")
	}
}
func (m *Manager) credentialsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		v, e := m.Credentials()
		respond(w, e, v)
		return
	}
	if !method(w, r, "POST") {
		return
	}
	var q struct{ Name, Kind, Secret string }
	if e := core.Decode(r, &q); e != nil {
		core.Error(w, 400, e.Error())
		return
	}
	v, e := m.PutCredential(CredentialMetadata{Name: q.Name, Kind: q.Kind}, []byte(q.Secret))
	if e != nil {
		core.Error(w, 400, e.Error())
		return
	}
	core.JSON(w, 201, v)
}
func (m *Manager) credentialHandler(w http.ResponseWriter, r *http.Request) {
	id := idAfter(r.URL.Path, "/api/v1/credentials/")
	if id == "" {
		core.Error(w, 404, "not found")
		return
	}
	if !method(w, r, "DELETE") {
		return
	}
	respond(w, m.DeleteCredential(id), nil)
}
func (m *Manager) filesHandler(w http.ResponseWriter, r *http.Request, id string) {
	p := r.URL.Query().Get("path")
	switch r.Method {
	case "GET":
		if strings.HasSuffix(r.URL.Path, "/download") {
			w.Header().Set("Content-Disposition", "attachment")
			if e := m.Download(r.Context(), id, p, w); e != nil {
				core.Error(w, 502, e.Error())
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/content") {
			c, h, e := m.ReadText(r.Context(), id, p)
			if e != nil {
				core.Error(w, 502, e.Error())
				return
			}
			core.JSON(w, 200, map[string]string{"content": c, "hash": h})
			return
		}
		v, e := m.ListFiles(r.Context(), id, p)
		respond(w, e, v)
	case "PUT":
		var q struct{ Content, Hash string }
		if e := core.Decode(r, &q); e != nil {
			core.Error(w, 400, e.Error())
			return
		}
		h, e := m.WriteText(r.Context(), id, p, q.Content, q.Hash)
		if e != nil {
			status := 502
			if strings.Contains(e.Error(), "conflict") {
				status = 409
			}
			core.Error(w, status, e.Error())
			return
		}
		core.JSON(w, 200, map[string]string{"hash": h})
	case "POST":
		v, e := m.Upload(r.Context(), id, p, io.LimitReader(r.Body, 128<<20))
		respond(w, e, v)
	default:
		core.Error(w, 405, "method not allowed")
	}
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
}
func (m *Manager) terminalHandler(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != "GET" || !sameOrigin(r) {
		core.Error(w, 403, "WebSocket origin refused")
		return
	}
	u := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return sameOrigin(r) }, ReadBufferSize: 4096, WriteBufferSize: 4096}
	ws, e := u.Upgrade(w, r, nil)
	if e != nil {
		return
	}
	if e = m.ServeTerminal(context.Background(), id, ws); e != nil {
		_ = ws.WriteJSON(terminalFrame{Type: "status", State: "closed", Message: e.Error()})
	}
}
func (m *Manager) tunnelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		core.JSON(w, 200, m.Tunnels())
		return
	}
	if !method(w, r, "POST") {
		return
	}
	var t Tunnel
	if e := core.Decode(r, &t); e != nil {
		core.Error(w, 400, e.Error())
		return
	}
	v, e := m.StartTunnel(r.Context(), t)
	respond(w, e, v)
}
func (m *Manager) tunnelHandler(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "DELETE") {
		return
	}
	respond(w, m.StopTunnel(idAfter(r.URL.Path, "/api/v1/tunnels/")), nil)
}
func respond(w http.ResponseWriter, e error, v any) {
	if e != nil {
		if errors.Is(e, io.EOF) {
			core.Error(w, 404, "not found")
		} else {
			core.Error(w, 502, e.Error())
		}
		return
	}
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	core.JSON(w, 200, v)
}
func hostError(w http.ResponseWriter, err error) {
	var changed *HostKeyChangedError
	if errors.As(err, &changed) {
		core.Error(w, http.StatusConflict, "host key changed")
		return
	}
	core.Error(w, http.StatusBadGateway, err.Error())
}
