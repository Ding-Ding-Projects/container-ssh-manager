package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
	"golang.org/x/crypto/ssh"
)

const operationKind = "engine-operation"

type operation struct {
	ID            string    `json:"id"`
	HostID        string    `json:"hostId"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	Outcome       string    `json:"outcome,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Deadline      time.Time `json:"deadline"`
	Error         string    `json:"error,omitempty"`
	OriginalID    string    `json:"originalId,omitempty"`
	ReplacementID string    `json:"replacementId,omitempty"`
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

// A restart never replays mutations. A lost stream has an unknown outcome, even
// when the daemon may have finished it while this process was unavailable.
func (m *Manager) recoverOperations() error {
	if m.store == nil {
		return nil
	}
	rows, err := m.store.List(operationKind)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var op operation
		if json.Unmarshal(row, &op) != nil {
			return fmt.Errorf("invalid operation record")
		}
		if op.State == "running" || op.State == "canceling" {
			op.State = "unknown"
			op.Outcome = "unknown"
			op.Error = "service restarted; inspect daemon state before retrying"
			op.UpdatedAt = time.Now().UTC()
			if err = m.store.Put(operationKind, op.ID, op); err != nil {
				return err
			}
		}
	}
	return nil
}

func operationTimeout(body []byte) (time.Duration, error) {
	var in struct {
		TimeoutSeconds *int `json:"timeoutSeconds"`
	}
	if json.Unmarshal(body, &in) != nil {
		return 0, fmt.Errorf("invalid operation options")
	}
	seconds := 1800
	if in.TimeoutSeconds != nil {
		seconds = *in.TimeoutSeconds
	}
	if seconds < 1 || seconds > 3600 {
		return 0, fmt.Errorf("timeoutSeconds must be between 1 and 3600")
	}
	return time.Duration(seconds) * time.Second, nil
}

func (m *Manager) beginOperation(hostID, kind string, timeout time.Duration) (operation, context.Context, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if m.store == nil || m.recoveryErr != nil {
		return operation{}, nil, fmt.Errorf("operation storage unavailable")
	}
	now := time.Now().UTC()
	op := operation{ID: core.ID(), HostID: hostID, Kind: kind, State: "running", CreatedAt: now, UpdatedAt: now, Deadline: now.Add(timeout)}
	if err := m.store.Put(operationKind, op.ID, op); err != nil {
		return operation{}, nil, err
	}
	ctx, cancel := context.WithDeadline(context.Background(), op.Deadline)
	if m.active == nil {
		m.active = make(map[string]context.CancelFunc)
	}
	m.active[op.ID] = cancel
	return op, ctx, nil
}

func (m *Manager) saveOperationIdentity(op operation) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	var current operation
	if err := m.store.Get(operationKind, op.ID, &current); err != nil {
		return err
	}
	current.OriginalID = op.OriginalID
	current.ReplacementID = op.ReplacementID
	current.UpdatedAt = time.Now().UTC()
	return m.store.Put(operationKind, op.ID, current)
}

func (m *Manager) finishOperation(op operation, ctx context.Context, state, message string) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	// Cancellation wins over a completion racing with its durable request.
	var current operation
	if err := m.store.Get(operationKind, op.ID, &current); err != nil {
		m.recoveryErr = err
	} else if current.State == "canceling" {
		state = "canceled"
		message = "cancel requested; daemon outcome is unknown"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		state = "timed_out"
		message = "operation timed out; daemon outcome is unknown"
	} else if ctx.Err() != nil && state != "canceled" {
		state = "canceled"
		message = "operation canceled; daemon outcome is unknown"
	}
	op.State = state
	op.Error = message
	op.UpdatedAt = time.Now().UTC()
	if state == "unknown" || state == "timed_out" || state == "canceled" {
		op.Outcome = "unknown"
	} else {
		op.Outcome = state
	}
	if err := m.store.Put(operationKind, op.ID, op); err != nil {
		m.recoveryErr = err
	}
	if cancel := m.active[op.ID]; cancel != nil {
		cancel()
	}
	delete(m.active, op.ID)
}

func (m *Manager) startImageOperation(w http.ResponseWriter, hostID, kind string, body []byte) {
	timeout, err := operationTimeout(body)
	if err != nil {
		core.Error(w, 400, err.Error())
		return
	}
	var endpoint string
	var build imageBuild
	switch kind {
	case "pull":
		var in imagePull
		if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.Reference) == "" || strings.ContainsAny(in.Reference, "\x00\r\n") {
			core.Error(w, 400, "image reference is required")
			return
		}
		endpoint = "/images/create?fromImage=" + url.QueryEscape(in.Reference)
		if in.Platform != "" {
			endpoint += "&platform=" + url.QueryEscape(in.Platform)
		}
	case "build":
		if json.Unmarshal(body, &build) != nil || !safeComposePath(build.ContextPath) || (build.Dockerfile != "" && !safeComposePath(build.Dockerfile)) {
			core.Error(w, 400, "absolute build contextPath and Dockerfile paths are required")
			return
		}
		for _, tag := range build.Tags {
			if !safeBuildToken(tag) {
				core.Error(w, 400, "invalid image tag")
				return
			}
		}
		for key := range build.BuildArgs {
			if !safeBuildKey(key) {
				core.Error(w, 400, "invalid build argument name")
				return
			}
		}
	default:
		core.Error(w, 400, "unsupported image operation")
		return
	}
	op, ctx, err := m.beginOperation(hostID, kind, timeout)
	if err != nil {
		core.Error(w, 503, "cannot persist operation")
		return
	}
	if kind == "build" {
		go m.runBuildOperation(ctx, op, build)
	} else {
		go m.runImageOperation(ctx, op, endpoint)
	}
	core.JSON(w, http.StatusAccepted, op)
}

func (m *Manager) runBuildOperation(ctx context.Context, op operation, in imageBuild) {
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
	status, err := m.runDockerCommand(ctx, op.HostID, args)
	state, message := "completed", ""
	if err != nil {
		state = "unknown"
		message = "build connection interrupted; daemon outcome is unknown"
	} else if status != 0 {
		state = "failed"
		message = "build command exited unsuccessfully"
	}
	m.finishOperation(op, ctx, state, message)
}

// Remote cancellation closes the session transport. That cannot prove the remote
// daemon stopped its work, so cancellation remains an unknown outcome.
func (m *Manager) runDockerCommand(ctx context.Context, hostID string, args []string) (int, error) {
	if hostID == "local" {
		cmd := exec.CommandContext(ctx, "docker", args...)
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return exit.ExitCode(), nil
		}
		if err != nil {
			return -1, err
		}
		return 0, nil
	}
	client, err := m.connections.Dial(ctx, hostID)
	if err != nil {
		return -1, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		return -1, err
	}
	defer session.Close()
	session.Stdout = io.Discard
	session.Stderr = io.Discard
	quoted := make([]string, len(args))
	for i, v := range args {
		quoted[i] = shellQuote(v)
	}
	err = session.Run("docker " + strings.Join(quoted, " "))
	var exit *ssh.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return exit.ExitStatus(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func safeBuildToken(v string) bool {
	return v != "" && len(v) <= 255 && !strings.HasPrefix(v, "-") && !strings.ContainsAny(v, " \t\r\n\x00;&|`$<>")
}
func safeBuildKey(v string) bool {
	if v == "" {
		return false
	}
	for i, c := range v {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// decodePullStream reads to EOF and checks JSON error events. A 200 response is
// only stream setup, not evidence that the image was pulled successfully.
func decodePullStream(reader io.Reader) error {
	dec := json.NewDecoder(reader)
	seen := false
	for {
		var event struct {
			Error       string          `json:"error"`
			ErrorDetail json.RawMessage `json:"errorDetail"`
			Status      string          `json:"status"`
			Aux         json.RawMessage `json:"aux"`
		}
		err := dec.Decode(&event)
		if errors.Is(err, io.EOF) {
			if !seen {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		if event.Error != "" || len(event.ErrorDetail) > 0 && string(event.ErrorDetail) != "null" && string(event.ErrorDetail) != "{}" {
			return errDaemonOperation
		}
		if event.Status != "" || len(event.Aux) > 0 {
			seen = true
		}
	}
}

var errDaemonOperation = errors.New("daemon reported operation failure")

func (m *Manager) runImageOperation(ctx context.Context, op operation, endpoint string) {
	state, message := "completed", ""
	err := m.pullImage(ctx, op.HostID, endpoint)
	if errors.Is(err, errDaemonOperation) {
		state = "failed"
		message = "daemon rejected image pull"
	} else if err != nil {
		state = "unknown"
		message = "image stream interrupted; daemon outcome is unknown"
	}
	m.finishOperation(op, ctx, state, message)
}
func (m *Manager) pullImage(ctx context.Context, hostID, endpoint string) error {
	client, base, closer, err := m.connections.EngineClient(ctx, hostID)
	if err != nil {
		return err
	}
	defer closer.Close()
	defer client.CloseIdleConnections()
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	rel, _ := url.Parse(endpoint)
	u.Path = strings.TrimSuffix(u.Path, "/") + rel.Path
	u.RawQuery = rel.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return errDaemonOperation
	}
	return decodePullStream(resp.Body)
}

func (m *Manager) operationHandler(w http.ResponseWriter, r *http.Request, path string) {
	if path == "" && r.Method == http.MethodGet {
		m.operationMu.Lock()
		defer m.operationMu.Unlock()
		if m.store == nil || m.recoveryErr != nil {
			core.Error(w, 503, "operation storage unavailable")
			return
		}
		rows, err := m.store.List(operationKind)
		if err != nil {
			core.Error(w, 500, "cannot list operations")
			return
		}
		out := []operation{}
		for _, row := range rows {
			var op operation
			if json.Unmarshal(row, &op) != nil {
				core.Error(w, 500, "invalid stored operation")
				return
			}
			if host := r.URL.Query().Get("hostId"); host != "" && op.HostID != host {
				continue
			}
			out = append(out, op)
		}
		core.JSON(w, 200, out)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) < 1 || !safeRecordID(parts[0]) {
		core.Error(w, 404, "operation not found")
		return
	}
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if m.recoveryErr != nil {
		core.Error(w, 503, "operation persistence unavailable; inspect daemon before retrying")
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
			op.State = "canceling"
			op.Outcome = "unknown"
			op.Error = "cancel requested; daemon outcome is unknown"
			op.UpdatedAt = time.Now().UTC()
			if m.store.Put(operationKind, op.ID, op) != nil {
				core.Error(w, 500, "cannot persist cancellation")
				return
			}
			if cancel := m.active[op.ID]; cancel != nil {
				cancel()
			}
		}
		core.JSON(w, 202, op)
		return
	}
	core.Error(w, 404, "operation route not found")
}
