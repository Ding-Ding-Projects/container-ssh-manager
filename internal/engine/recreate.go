package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

type containerInspection struct {
	ID              string                     `json:"Id"`
	Name            string                     `json:"Name"`
	Config          map[string]json.RawMessage `json:"Config"`
	HostConfig      map[string]json.RawMessage `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]map[string]json.RawMessage `json:"Networks"`
		Ports    map[string]json.RawMessage            `json:"Ports"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type, Name, Destination string
		RW                      bool
	} `json:"Mounts"`
	State struct {
		Running, Paused, Restarting bool
		Health                      *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

// recreatePayload rejects cases that cannot preserve rollback safety. In
// particular stopping an auto-remove container would destroy the original.
func recreatePayload(in containerInspection) ([]byte, error) {
	if in.Config == nil || in.HostConfig == nil || in.ID == "" {
		return nil, fmt.Errorf("invalid inspection")
	}
	var auto bool
	_ = json.Unmarshal(in.HostConfig["AutoRemove"], &auto)
	var mode string
	_ = json.Unmarshal(in.HostConfig["NetworkMode"], &mode)
	var volumesFrom []string
	_ = json.Unmarshal(in.HostConfig["VolumesFrom"], &volumesFrom)
	if len(volumesFrom) > 0 {
		return nil, fmt.Errorf("inherited volumes require manual recreation")
	}
	if auto || in.State.Paused || in.State.Restarting || strings.HasPrefix(mode, "container:") {
		return nil, fmt.Errorf("auto-remove, paused, restarting, and shared-network containers require manual recreation")
	}
	endpoints := map[string]any{}
	for name, endpoint := range in.NetworkSettings.Networks {
		var ipam map[string]json.RawMessage
		_ = json.Unmarshal(endpoint["IPAMConfig"], &ipam)
		if len(ipam) > 0 {
			return nil, fmt.Errorf("static network assignments require manual recreation")
		}
		clean := map[string]json.RawMessage{}
		// Do not reuse daemon-generated endpoint IDs, MACs, or dynamic addresses.
		for _, key := range []string{"Aliases", "Links", "DriverOpts", "GwPriority"} {
			if v, ok := endpoint[key]; ok {
				clean[key] = v
			}
		}
		if name == "bridge" || name == "host" || name == "none" {
			delete(clean, "Aliases")
		}
		endpoints[name] = clean
	}
	// Docker's image VOLUME declarations otherwise allocate new anonymous volumes.
	// Rebind every inspected volume that is not already configured explicitly.
	var binds []string
	_ = json.Unmarshal(in.HostConfig["Binds"], &binds)
	var mounts []map[string]any
	_ = json.Unmarshal(in.HostConfig["Mounts"], &mounts)
	for _, mount := range in.Mounts {
		if mount.Type != "volume" || mount.Name == "" {
			continue
		}
		found := false
		for _, b := range binds {
			if strings.Contains(b, ":"+mount.Destination+":") || strings.HasSuffix(b, ":"+mount.Destination) {
				found = true
			}
		}
		for _, b := range mounts {
			if b["Target"] == mount.Destination {
				found = true
				if b["Source"] == "" || b["Source"] == nil {
					b["Source"] = mount.Name
				}
			}
		}
		if !found {
			mounts = append(mounts, map[string]any{"Type": "volume", "Source": mount.Name, "Target": mount.Destination, "ReadOnly": !mount.RW})
		}
	}
	if len(mounts) > 0 {
		in.HostConfig["Mounts"], _ = json.Marshal(mounts)
	}
	if in.State.Running && len(in.NetworkSettings.Ports) > 0 {
		bindings := map[string]json.RawMessage{}
		for port, values := range in.NetworkSettings.Ports {
			if string(values) != "null" {
				bindings[port] = values
			}
		}
		if len(bindings) > 0 {
			in.HostConfig["PortBindings"], _ = json.Marshal(bindings)
		}
	}
	in.Config["HostConfig"], _ = json.Marshal(in.HostConfig)
	in.Config["NetworkingConfig"], _ = json.Marshal(map[string]any{"EndpointsConfig": endpoints})
	return json.Marshal(in.Config)
}

func (m *Manager) inspectContainer(ctx context.Context, host, id string) (containerInspection, error) {
	var out containerInspection
	b, _, err := m.engineJSON(ctx, host, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}
func (m *Manager) containerAction(ctx context.Context, host, id, action string) error {
	_, _, err := m.engineJSON(ctx, host, http.MethodPost, "/containers/"+url.PathEscape(id)+"/"+action, nil)
	return err
}
func (m *Manager) removeContainer(ctx context.Context, host, id string, force bool) error {
	_, _, err := m.engineJSON(ctx, host, http.MethodDelete, fmt.Sprintf("/containers/%s?force=%t&v=false", url.PathEscape(id), force), nil)
	return err
}

// All originals and volumes survive until the replacement has the original name
// and has started (and passed its configured health check, when present).
func (m *Manager) recreateContainer(w http.ResponseWriter, r *http.Request, hostID, id string) {
	if !safeRecordID(id) {
		core.Error(w, 400, "invalid resource id")
		return
	}
	m.recreateMu.Lock()
	defer m.recreateMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	old, err := m.inspectContainer(ctx, hostID, id)
	if err != nil {
		core.Error(w, 502, "cannot inspect original container")
		return
	}
	payload, err := recreatePayload(old)
	if err != nil {
		core.Error(w, 409, err.Error())
		return
	}
	name := strings.TrimPrefix(old.Name, "/")
	if !safeName(name) {
		core.Error(w, 502, "original container name is invalid")
		return
	}
	suffix := core.ID()
	temporary := "csm-replacement-" + suffix
	backup := "csm-original-" + suffix
	// Persist only resource IDs, never inspection/environment content.
	op, opctx, err := m.beginOperation(hostID, "recreate", 2*time.Minute)
	if err != nil {
		core.Error(w, 503, "cannot persist recreation")
		return
	}
	stopCancel := context.AfterFunc(opctx, cancel)
	defer stopCancel()
	op.OriginalID = old.ID
	finalState, finalMessage := "unknown", "recreation interrupted; inspect original and replacement before retrying"
	defer func() { m.finishOperation(op, opctx, finalState, finalMessage) }()
	if err = m.saveOperationIdentity(op); err != nil {
		core.Error(w, 500, "cannot persist original identity")
		return
	}
	created, status, err := m.engineJSON(ctx, hostID, http.MethodPost, "/containers/create?name="+url.QueryEscape(temporary), payload)
	if err != nil {
		core.JSON(w, status, map[string]any{"error": "replacement creation failed; original retained", "operationId": op.ID})
		return
	}
	var result struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if json.Unmarshal(created, &result) != nil || !safeRecordID(result.ID) {
		core.Error(w, 502, "replacement identity unknown; original retained")
		return
	}
	op.ReplacementID = result.ID
	if err = m.saveOperationIdentity(op); err != nil {
		core.Error(w, 500, "replacement created; original retained because operation persistence failed")
		return
	}
	stopped, renamed, replacementNamed := false, false, false
	rollback := func(reason string) {
		recovery, recoveryCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer recoveryCancel()
		recovered := true
		if err := m.containerAction(recovery, hostID, result.ID, "stop?t=10"); err != nil {
			recovered = false
		}
		if replacementNamed {
			if err := m.containerAction(recovery, hostID, result.ID, "rename?name="+url.QueryEscape(temporary)); err != nil {
				recovered = false
			}
		}
		if renamed {
			if err := m.containerAction(recovery, hostID, old.ID, "rename?name="+url.QueryEscape(name)); err != nil {
				recovered = false
			}
		}
		if stopped && old.State.Running {
			if err := m.containerAction(recovery, hostID, old.ID, "start"); err != nil {
				recovered = false
			}
		}
		if recovered {
			current, e := m.inspectContainer(recovery, hostID, old.ID)
			if e != nil || current.Name != "/"+name || current.State.Running != old.State.Running {
				recovered = false
			}
		}
		if recovered {
			if err := m.removeContainer(recovery, hostID, result.ID, false); err != nil {
				recovered = false
			}
		}
		if recovered {
			finalState = "failed"
			finalMessage = reason + "; original restored; volumes retained"
		} else {
			finalMessage = reason + "; recovery incomplete; original retained; inspect both resource identities"
		}
		core.JSON(w, 502, map[string]any{"error": finalMessage, "operationId": op.ID, "originalId": old.ID, "replacementId": result.ID, "rollback": recovered})
	}
	if old.State.Running {
		// Set before sending: a lost response may still mean Docker stopped it.
		stopped = true
		if err = m.containerAction(ctx, hostID, old.ID, "stop?t=10"); err != nil {
			rollback("original stop outcome uncertain")
			return
		}
	}
	renamed = true
	if err = m.containerAction(ctx, hostID, old.ID, "rename?name="+url.QueryEscape(backup)); err != nil {
		rollback("original rename outcome uncertain")
		return
	}
	replacementNamed = true
	if err = m.containerAction(ctx, hostID, result.ID, "rename?name="+url.QueryEscape(name)); err != nil {
		rollback("replacement rename outcome uncertain")
		return
	}
	if old.State.Running {
		if err = m.containerAction(ctx, hostID, result.ID, "start"); err != nil {
			rollback("replacement start failed")
			return
		}
		if err = m.waitContainerReady(ctx, hostID, result.ID); err != nil {
			rollback("replacement did not become ready")
			return
		}
	}
	if err = m.removeContainer(ctx, hostID, old.ID, false); err != nil {
		finalMessage = "replacement ready; original removal outcome is unknown; inspect backup identity"
		core.JSON(w, 502, map[string]any{"error": finalMessage, "operationId": op.ID, "originalId": old.ID, "replacementId": result.ID})
		return
	}
	finalState = "completed"
	finalMessage = ""
	core.JSON(w, 201, map[string]any{"id": result.ID, "operationId": op.ID, "warnings": result.Warnings})
}
func (m *Manager) waitContainerReady(ctx context.Context, host, id string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	// Wait one scheduling interval even without HEALTHCHECK, catching immediate exits.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		current, err := m.inspectContainer(ctx, host, id)
		if err != nil {
			return err
		}
		if !current.State.Running || current.State.Restarting {
			return fmt.Errorf("replacement is not running")
		}
		if current.State.Health == nil || current.State.Health.Status == "healthy" {
			return nil
		}
		if current.State.Health.Status == "unhealthy" {
			return fmt.Errorf("replacement is unhealthy")
		}
	}
}
