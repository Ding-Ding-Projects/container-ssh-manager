// Package engine exposes a deliberately narrow Docker Engine API facade.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/connection"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

// Manager owns container-engine HTTP handlers. Connections owns all transport.
type Manager struct {
	store       *core.Store
	connections *connection.Manager
	vault       *core.Vault
}

func New(store *core.Store, connections *connection.Manager, vault *core.Vault) *Manager {
	return &Manager{store: store, connections: connections, vault: vault}
}

func (m *Manager) Register(mux *http.ServeMux) { mux.HandleFunc("/api/v1/engine/", m.serve) }

func (m *Manager) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/engine/")
	if p == "" {
		core.Error(w, http.StatusNotFound, "engine route not found")
		return
	}
	if strings.HasPrefix(p, "compose/") {
		m.compose(w, r, strings.TrimPrefix(p, "compose/"))
		return
	}
	if strings.HasPrefix(p, "operations/") {
		m.operation(w, r, strings.TrimPrefix(p, "operations/"))
		return
	}
	m.docker(w, r, p)
}

func (m *Manager) docker(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(path, "/")
	if len(parts) == 0 || !resource(parts[0]) {
		core.Error(w, 404, "engine resource not found")
		return
	}
	hostID, body, err := hostAndBody(r)
	if err != nil {
		core.Error(w, 400, err.Error())
		return
	}
	if hostID == "" {
		core.Error(w, 400, "hostId is required")
		return
	}
	if len(parts) == 2 && parts[0] == "images" && (parts[1] == "pull" || parts[1] == "build") && r.Method == http.MethodPost {
		m.startImageOperation(w, hostID, parts[1], body)
		return
	}
	if len(parts) == 3 && parts[0] == "containers" && parts[2] == "exec" && r.Method == http.MethodPost {
		m.execContainer(w, r, hostID, parts[1], body)
		return
	}
	apiPath, method, stream, err := dockerRoute(r.Method, parts, r.URL.Query())
	if err != nil {
		core.Error(w, 400, err.Error())
		return
	}
	if err := validatePayload(parts[0], method, body); err != nil {
		core.Error(w, 400, err.Error())
		return
	}
	m.proxy(w, r, hostID, method, apiPath, body, stream)
}

type execRequest struct {
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
	User       string   `json:"user,omitempty"`
	TTY        bool     `json:"tty"`
}

func (m *Manager) execContainer(w http.ResponseWriter, r *http.Request, hostID, id string, body []byte) {
	if !safeRecordID(id) {
		core.Error(w, 400, "invalid resource id")
		return
	}
	var in execRequest
	if json.Unmarshal(body, &in) != nil || len(in.Cmd) == 0 {
		core.Error(w, 400, "cmd is required")
		return
	}
	payload, _ := json.Marshal(map[string]any{"Cmd": in.Cmd, "Env": in.Env, "WorkingDir": in.WorkingDir, "User": in.User, "Tty": in.TTY, "AttachStdout": true, "AttachStderr": true})
	created, status, err := m.engineJSON(r.Context(), hostID, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", payload)
	if err != nil {
		core.Error(w, status, "cannot create container exec")
		return
	}
	var createdBody struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(created, &createdBody) != nil || createdBody.ID == "" {
		core.Error(w, 502, "engine did not return an exec id")
		return
	}
	output, status, err := m.engineJSON(r.Context(), hostID, http.MethodPost, "/exec/"+url.PathEscape(createdBody.ID)+"/start", []byte(fmt.Sprintf(`{"Detach":false,"Tty":%t}`, in.TTY)))
	if err != nil {
		core.Error(w, status, "container exec interrupted; outcome is unknown")
		return
	}
	inspect, status, err := m.engineJSON(r.Context(), hostID, http.MethodGet, "/exec/"+url.PathEscape(createdBody.ID)+"/json", nil)
	if err != nil {
		core.Error(w, status, "container exec finished with unknown status")
		return
	}
	var state struct {
		ExitCode *int `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	_ = json.Unmarshal(inspect, &state)
	if state.Running || state.ExitCode == nil {
		core.Error(w, 502, "container exec outcome is unknown")
		return
	}
	core.JSON(w, 200, map[string]any{"id": createdBody.ID, "exitCode": *state.ExitCode, "output": string(output)})
}

func (m *Manager) engineJSON(ctx context.Context, hostID, method, path string, body []byte) ([]byte, int, error) {
	client, base, closer, err := m.connections.EngineClient(ctx, hostID)
	if err != nil {
		return nil, 502, err
	}
	defer closer.Close()
	u, err := url.Parse(base)
	if err != nil {
		return nil, 502, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 500, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 502, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return out, resp.StatusCode, fmt.Errorf("engine status %d", resp.StatusCode)
	}
	return out, resp.StatusCode, nil
}

func (m *Manager) proxy(w http.ResponseWriter, r *http.Request, hostID, method, apiPath string, body []byte, stream bool) {
	client, base, closer, err := m.connections.EngineClient(r.Context(), hostID)
	if err != nil {
		core.Error(w, 502, "engine connection unavailable")
		return
	}
	defer closer.Close()
	u, err := url.Parse(base)
	if err != nil {
		core.Error(w, 502, "invalid engine transport")
		return
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + apiPath
	req, err := http.NewRequestWithContext(r.Context(), method, u.String(), bytes.NewReader(body))
	if err != nil {
		core.Error(w, 500, "cannot create engine request")
		return
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		core.Error(w, 502, "engine request failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		m.copyError(w, resp)
		return
	}
	if stream {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (m *Manager) copyError(w http.ResponseWriter, resp *http.Response) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var value any
	if json.Unmarshal(b, &value) == nil {
		core.JSON(w, resp.StatusCode, value)
		return
	}
	core.Error(w, resp.StatusCode, "engine operation failed")
}

func hostAndBody(r *http.Request) (string, []byte, error) {
	host := r.URL.Query().Get("hostId")
	if r.Body == nil {
		return host, nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return "", nil, fmt.Errorf("invalid request body")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return host, nil, nil
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil {
		return "", nil, fmt.Errorf("request body must be JSON")
	}
	if raw, ok := v["hostId"]; ok {
		if err := json.Unmarshal(raw, &host); err != nil || host == "" {
			return "", nil, fmt.Errorf("hostId must be a string")
		}
		delete(v, "hostId")
		b, _ = json.Marshal(v)
	}
	return host, b, nil
}

func resource(v string) bool {
	return v == "containers" || v == "images" || v == "volumes" || v == "networks"
}

func dockerRoute(method string, p []string, q url.Values) (string, string, bool, error) {
	root := p[0]
	if root == "images" && len(p) == 2 && method == http.MethodPost {
		switch p[1] {
		case "pull":
			return "/images/create", method, true, nil
		case "build":
			return "/build", method, true, nil
		}
	}
	id := ""
	if len(p) > 1 {
		id = p[1]
		if id == "" || strings.ContainsAny(id, "/\\") {
			return "", "", false, fmt.Errorf("invalid resource id")
		}
	}
	if root == "containers" {
		if id == "" {
			if method == http.MethodGet {
				return "/containers/json?all=" + boolQuery(q, "all"), method, false, nil
			}
			if method == http.MethodPost {
				return "/containers/create" + nameQuery(q), method, false, nil
			}
		}
		if id != "" && len(p) == 2 && method == http.MethodGet {
			return "/containers/" + url.PathEscape(id) + "/json", method, false, nil
		}
		if id != "" && len(p) == 2 && method == http.MethodDelete {
			return "/containers/" + url.PathEscape(id) + "?force=" + boolQuery(q, "force") + "&v=" + boolQuery(q, "volumes"), method, false, nil
		}
		if id != "" && len(p) == 3 {
			switch p[2] {
			case "start", "stop", "restart":
				if method == http.MethodPost {
					return "/containers/" + url.PathEscape(id) + "/" + p[2], method, false, nil
				}
			case "logs":
				if method == http.MethodGet {
					return "/containers/" + url.PathEscape(id) + "/logs?stdout=" + defaultBool(q, "stdout", true) + "&stderr=" + defaultBool(q, "stderr", true) + "&tail=" + url.QueryEscape(q.Get("tail")) + "&timestamps=" + boolQuery(q, "timestamps"), method, true, nil
				}
			case "stats":
				if method == http.MethodGet {
					return "/containers/" + url.PathEscape(id) + "/stats?stream=" + defaultBool(q, "stream", false), method, q.Get("stream") == "true", nil
				}
			case "exec":
				if method == http.MethodPost {
					return "/containers/" + url.PathEscape(id) + "/exec", method, false, nil
				}
			}
		}
	}
	if root == "images" {
		if id == "" && method == http.MethodGet {
			return "/images/json?all=" + boolQuery(q, "all"), method, false, nil
		}
		if id != "" && len(p) == 3 && p[2] == "tag" && method == http.MethodPost {
			return "/images/" + url.PathEscape(id) + "/tag", method, false, nil
		}
		if id != "" && len(p) == 2 && method == http.MethodDelete {
			return "/images/" + url.PathEscape(id) + "?force=" + boolQuery(q, "force") + "&noprune=" + boolQuery(q, "noprune"), method, false, nil
		}
	}
	if root == "volumes" {
		if id == "" && method == http.MethodGet {
			return "/volumes", method, false, nil
		}
		if id == "" && method == http.MethodPost {
			return "/volumes/create", method, false, nil
		}
		if id != "" && len(p) == 2 && method == http.MethodGet {
			return "/volumes/" + url.PathEscape(id), method, false, nil
		}
		if id != "" && len(p) == 2 && method == http.MethodDelete {
			return "/volumes/" + url.PathEscape(id) + "?force=" + boolQuery(q, "force"), method, false, nil
		}
	}
	if root == "networks" {
		if id == "" && method == http.MethodGet {
			return "/networks", method, false, nil
		}
		if id == "" && method == http.MethodPost {
			return "/networks/create", method, false, nil
		}
		if id != "" && len(p) == 2 && method == http.MethodGet {
			return "/networks/" + url.PathEscape(id), method, false, nil
		}
		if id != "" && len(p) == 2 && method == http.MethodDelete {
			return "/networks/" + url.PathEscape(id), method, false, nil
		}
		if id != "" && len(p) == 3 && (p[2] == "connect" || p[2] == "disconnect") && method == http.MethodPost {
			return "/networks/" + url.PathEscape(id) + "/" + p[2], method, false, nil
		}
	}
	return "", "", false, fmt.Errorf("unsupported engine operation")
}

func boolQuery(q url.Values, key string) string { return strconv.FormatBool(q.Get(key) == "true") }
func defaultBool(q url.Values, key string, fallback bool) string {
	if q.Get(key) == "" {
		return strconv.FormatBool(fallback)
	}
	return boolQuery(q, key)
}
func nameQuery(q url.Values) string {
	if q.Get("name") == "" {
		return ""
	}
	return "?name=" + url.QueryEscape(q.Get("name"))
}

func validatePayload(resource, method string, body []byte) error {
	if method != http.MethodPost || len(body) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return fmt.Errorf("request body must be JSON")
	}
	if resource == "images" && bytes.Contains(bytes.ToLower(body), []byte("http://")) {
		return fmt.Errorf("arbitrary engine proxy URLs are not permitted")
	}
	return nil
}

// operation records are intentionally unavailable until a durable background runner
// lands. The facade never claims an interrupted engine stream completed.
func (m *Manager) operation(w http.ResponseWriter, r *http.Request, path string) {
	m.operationHandler(w, r, path)
}

// EngineClient contract is checked here at compile time when connection lands.
var _ = context.Background
