package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

const operationKind = "engine-operation"

type operation struct {
	ID        string    `json:"id"`
	HostID    string    `json:"hostId"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Error     string    `json:"error,omitempty"`
}
type imagePull struct {
	Reference string `json:"reference"`
	Platform  string `json:"platform,omitempty"`
}
type imageBuild struct {
	ContextPath string            `json:"contextPath"`
	Dockerfile  string            `json:"dockerfile,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	BuildArgs   map[string]string `json:"buildArgs,omitempty"`
}

func (m *Manager) startImageOperation(w http.ResponseWriter, hostID, kind string, body []byte) {
	if m.store == nil {
		core.Error(w, 503, "operation storage is unavailable")
		return
	}
	var endpoint string
	switch kind {
	case "pull":
		var in imagePull
		if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.Reference) == "" {
			core.Error(w, 400, "image reference is required")
			return
		}
		endpoint = "/images/create?fromImage=" + url.QueryEscape(in.Reference)
		if in.Platform != "" {
			endpoint += "&platform=" + url.QueryEscape(in.Platform)
		}
	case "build":
		var in imageBuild
		if json.Unmarshal(body, &in) != nil || !safeComposePath(in.ContextPath) || (in.Dockerfile != "" && !safeComposePath(in.Dockerfile)) {
			core.Error(w, 400, "absolute build contextPath is required")
			return
		}
		for _, tag := range in.Tags {
			if !safeBuildToken(tag) {
				core.Error(w, 400, "invalid image tag")
				return
			}
		}
		for key := range in.BuildArgs {
			if !safeBuildKey(key) {
				core.Error(w, 400, "invalid build argument name")
				return
			}
		}
		now := time.Now().UTC()
		op := operation{ID: core.ID(), HostID: hostID, Kind: kind, State: "running", CreatedAt: now, UpdatedAt: now}
		if m.store.Put(operationKind, op.ID, op) != nil {
			core.Error(w, 500, "cannot save operation")
			return
		}
		go m.runBuildOperation(op, in)
		core.JSON(w, http.StatusAccepted, op)
		return
	default:
		core.Error(w, 400, "unsupported image operation")
		return
	}
	now := time.Now().UTC()
	op := operation{ID: core.ID(), HostID: hostID, Kind: kind, State: "running", CreatedAt: now, UpdatedAt: now}
	if m.store.Put(operationKind, op.ID, op) != nil {
		core.Error(w, 500, "cannot save operation")
		return
	}
	go m.runImageOperation(op, endpoint)
	core.JSON(w, http.StatusAccepted, op)
}
func (m *Manager) runBuildOperation(op operation, in imageBuild) {
	args := []string{"build"}
	if in.Dockerfile != "" {
		args = append(args, "--file", in.Dockerfile)
	}
	for _, tag := range in.Tags {
		args = append(args, "--tag", tag)
	}
	for key, value := range in.BuildArgs {
		args = append(args, "--build-arg", key+"="+value)
	}
	args = append(args, in.ContextPath)
	var err error
	if op.HostID == "local" {
		err = exec.CommandContext(context.Background(), "docker", args...).Run()
	} else {
		quoted := make([]string, len(args))
		for i, v := range args {
			quoted[i] = shellQuote(v)
		}
		status, runErr := m.connections.Run(context.Background(), op.HostID, "docker "+strings.Join(quoted, " "))
		if runErr != nil {
			err = runErr
		} else if status != 0 {
			err = fmt.Errorf("docker build exited %d", status)
		}
	}
	if err != nil {
		op.State = "unknown"
		op.Error = "operation stream interrupted or failed; daemon outcome is unknown"
	} else {
		op.State = "completed"
	}
	op.UpdatedAt = time.Now().UTC()
	_ = m.store.Put(operationKind, op.ID, op)
}
func safeBuildToken(v string) bool {
	return v != "" && len(v) <= 255 && !strings.ContainsAny(v, " \t\r\n\x00;&|`$<>")
}
func safeBuildKey(v string) bool {
	if v == "" {
		return false
	}
	for i, c := range v {
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
func (m *Manager) runImageOperation(op operation, endpoint string) {
	var err error
	if strings.HasPrefix(endpoint, "build:") {
		path := strings.TrimPrefix(endpoint, "build:")
		if op.HostID == "local" {
			err = exec.CommandContext(context.Background(), "docker", "build", path).Run()
		} else {
			status, runErr := m.connections.Run(context.Background(), op.HostID, "docker build "+shellQuote(path))
			if runErr != nil {
				err = runErr
			} else if status != 0 {
				err = fmt.Errorf("docker build exited %d", status)
			}
		}
	} else {
		_, _, err = m.engineJSON(context.Background(), op.HostID, http.MethodPost, endpoint, nil)
	}
	if err != nil {
		op.State = "unknown"
		op.Error = "operation stream interrupted or failed; daemon outcome is unknown"
	} else {
		op.State = "completed"
	}
	op.UpdatedAt = time.Now().UTC()
	_ = m.store.Put(operationKind, op.ID, op)
}
func (m *Manager) operationHandler(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(path, "/")
	if len(parts) < 1 || !safeRecordID(parts[0]) {
		core.Error(w, 404, "operation not found")
		return
	}
	var op operation
	if m.store == nil || m.store.Get(operationKind, parts[0], &op) != nil {
		core.Error(w, 404, "operation not found")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		core.JSON(w, 200, op)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		if op.State == "running" {
			op.State = "unknown"
			op.Error = "cancel requested; daemon outcome is unknown"
			op.UpdatedAt = time.Now().UTC()
			_ = m.store.Put(operationKind, op.ID, op)
		}
		core.JSON(w, 202, op)
		return
	}
	core.Error(w, 404, "operation route not found")
}
