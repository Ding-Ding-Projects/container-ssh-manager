package engine

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

// recreateContainer follows the safe ordering: inspect, create replacement, remove
// stopped original, then start only when the original had been running.
func (m *Manager) recreateContainer(w http.ResponseWriter, r *http.Request, hostID, id string) {
	if !safeRecordID(id) {
		core.Error(w, 400, "invalid resource id")
		return
	}
	b, status, err := m.engineJSON(r.Context(), hostID, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		core.Error(w, status, "cannot inspect container")
		return
	}
	var inspected struct {
		Name             string          `json:"Name"`
		Config           json.RawMessage `json:"Config"`
		HostConfig       json.RawMessage `json:"HostConfig"`
		NetworkingConfig json.RawMessage `json:"NetworkSettings"`
		State            struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if json.Unmarshal(b, &inspected) != nil || len(inspected.Config) == 0 {
		core.Error(w, 502, "engine returned invalid container inspection")
		return
	}
	name := strings.TrimPrefix(inspected.Name, "/")
	if !safeName(name) {
		name = ""
	}
	create, _ := json.Marshal(map[string]json.RawMessage{"Config": inspected.Config, "HostConfig": inspected.HostConfig, "NetworkingConfig": inspected.NetworkingConfig})
	path := "/containers/create"
	if name != "" {
		path += "?name=" + url.QueryEscape(name+"-recreated")
	}
	created, status, err := m.engineJSON(r.Context(), hostID, http.MethodPost, path, create)
	if err != nil {
		core.Error(w, status, "cannot create replacement container")
		return
	}
	var result struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if json.Unmarshal(created, &result) != nil || result.ID == "" {
		core.Error(w, 502, "engine did not return replacement id")
		return
	}
	if _, status, err = m.engineJSON(r.Context(), hostID, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=false&v=false", nil); err != nil {
		core.Error(w, status, "replacement created but original was retained")
		return
	}
	if inspected.State.Running {
		if _, status, err = m.engineJSON(r.Context(), hostID, http.MethodPost, "/containers/"+url.PathEscape(result.ID)+"/start", nil); err != nil {
			core.Error(w, status, "replacement created but start outcome is unknown")
			return
		}
	}
	core.JSON(w, 201, map[string]any{"id": result.ID, "warnings": result.Warnings})
}
