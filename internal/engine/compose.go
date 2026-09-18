package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

const composeKind = "engine-compose"

type composeProject struct {
	ID        string            `json:"id"`
	HostID    string            `json:"hostId"`
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	FileName  string            `json:"fileName,omitempty"`
	Adopted   bool              `json:"adopted"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Revisions []composeRevision `json:"revisions"`
	Pending   *composeWrite     `json:"pending,omitempty"`
}
type composeRevision struct {
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"createdAt"`
	Sealed    string    `json:"sealed,omitempty"`
}
type composeFiles struct {
	Compose     string `json:"compose"`
	Environment string `json:"environment,omitempty"`
}
type composeCreate struct {
	HostID string `json:"hostId"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Adopt  bool   `json:"adopt"`
}
type composeDeploy struct {
	Pull   bool  `json:"pull"`
	Build  bool  `json:"build"`
	Detach *bool `json:"detach"`
}

func (m *Manager) compose(w http.ResponseWriter, r *http.Request, path string) {
	m.composeMu.Lock()
	defer m.composeMu.Unlock()
	if m.store == nil || m.vault == nil {
		core.Error(w, 503, "sealed compose storage is unavailable")
		return
	}
	if path == "projects" {
		if r.Method == http.MethodGet {
			m.listCompose(w)
			return
		}
		if r.Method == http.MethodPost {
			m.createCompose(w, r)
			return
		}
	}
	p := strings.Split(path, "/")
	if len(p) < 2 || p[0] != "projects" || !safeRecordID(p[1]) {
		core.Error(w, 404, "compose route not found")
		return
	}
	project, err := m.loadCompose(p[1])
	if err != nil {
		core.Error(w, 404, "compose project not found")
		return
	}
	if len(p) == 2 && r.Method == http.MethodGet {
		core.JSON(w, 200, publicCompose(project))
		return
	}
	if len(p) == 3 && p[2] == "files" && r.Method == http.MethodGet {
		if len(project.Revisions) == 0 {
			core.Error(w, 404, "no saved compose revision")
			return
		}
		files, err := m.openRevision(project.Revisions[len(project.Revisions)-1])
		if err != nil {
			core.Error(w, 500, "stored compose revision cannot be opened")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		core.JSON(w, 200, files)
		return
	}
	if len(p) == 3 && p[2] == "files" && r.Method == http.MethodPut {
		m.saveFiles(w, r, &project)
		return
	}
	if len(p) == 3 && p[2] == "recover" && r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		if err := m.recoverComposeWrite(ctx, &project); err != nil {
			core.Error(w, 409, err.Error())
			return
		}
		core.JSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(p) == 3 && (p[2] == "validate" || p[2] == "deploy" || p[2] == "stop" || p[2] == "down") && r.Method == http.MethodPost {
		m.runCompose(w, r, &project, p[2])
		return
	}
	if len(p) == 4 && p[2] == "restore" && r.Method == http.MethodPost {
		m.restoreCompose(w, r, &project, p[3])
		return
	}
	core.Error(w, 404, "compose route not found")
}
func (m *Manager) listCompose(w http.ResponseWriter) {
	rows, err := m.store.List(composeKind)
	if err != nil {
		core.Error(w, 500, "cannot list compose projects")
		return
	}
	out := make([]composeProject, 0, len(rows))
	for _, row := range rows {
		var p composeProject
		if json.Unmarshal(row, &p) == nil {
			out = append(out, publicCompose(p))
		}
	}
	core.JSON(w, 200, out)
}
func (m *Manager) createCompose(w http.ResponseWriter, r *http.Request) {
	var in composeCreate
	if core.Decode(r, &in) != nil || in.HostID == "" || !safeName(in.Name) || !safeComposePath(in.Path) {
		core.Error(w, 400, "invalid compose project")
		return
	}
	if in.HostID != "local" && !pathpkg.IsAbs(in.Path) {
		core.Error(w, 400, "remote compose path must be an absolute POSIX path")
		return
	}
	now := time.Now().UTC()
	cleanPath := filepath.Clean(in.Path)
	if in.HostID != "local" {
		cleanPath = pathpkg.Clean(in.Path)
	}
	p := composeProject{ID: core.ID(), HostID: in.HostID, Name: in.Name, Path: cleanPath, Adopted: in.Adopt, CreatedAt: now, UpdatedAt: now}
	if in.Adopt {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		files, fileName, err := m.readComposeProject(ctx, p.HostID, p.Path)
		if err != nil {
			core.Error(w, 400, err.Error())
			return
		}
		plain, err := json.Marshal(files)
		if err != nil {
			core.Error(w, 500, "cannot encode compose revision")
			return
		}
		defer clearBytes(plain)
		sealed, err := m.vault.Seal(plain)
		if err != nil {
			core.Error(w, 500, "cannot seal compose revision")
			return
		}
		p.FileName = fileName
		p.Revisions = []composeRevision{{Number: 1, CreatedAt: now, Sealed: base64.StdEncoding.EncodeToString(sealed)}}
	}
	if m.store.Put(composeKind, p.ID, p) != nil {
		core.Error(w, 500, "cannot save compose project")
		return
	}
	core.JSON(w, 201, publicCompose(p))
}
func (m *Manager) loadCompose(id string) (composeProject, error) {
	var p composeProject
	err := m.store.Get(composeKind, id, &p)
	return p, err
}

func publicCompose(p composeProject) composeProject {
	p.Revisions = append([]composeRevision(nil), p.Revisions...)
	for i := range p.Revisions {
		p.Revisions[i].Sealed = ""
	}
	if p.Pending != nil {
		p.Pending = &composeWrite{State: "recovery_required"}
	}
	return p
}
func (m *Manager) saveFiles(w http.ResponseWriter, r *http.Request, p *composeProject) {
	var f composeFiles
	if core.Decode(r, &f) != nil || strings.TrimSpace(f.Compose) == "" {
		core.Error(w, 400, "compose content is required")
		return
	}
	plain, _ := json.Marshal(f)
	defer clearBytes(plain)
	sealed, err := m.vault.Seal(plain)
	if err != nil {
		core.Error(w, 500, "cannot seal compose revision")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := m.commitComposeRevision(ctx, p, f, base64.StdEncoding.EncodeToString(sealed)); err != nil {
		core.Error(w, 409, err.Error())
		return
	}
	core.JSON(w, 200, map[string]int{"revision": len(p.Revisions)})
}
func (m *Manager) restoreCompose(w http.ResponseWriter, r *http.Request, p *composeProject, n string) {
	var number int
	if _, err := fmt.Sscan(n, &number); err != nil || number < 1 || number > len(p.Revisions) {
		core.Error(w, 400, "invalid revision")
		return
	}
	revision := p.Revisions[number-1]
	files, err := m.openRevision(revision)
	if err != nil {
		core.Error(w, 500, "stored compose revision cannot be opened")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := m.commitComposeRevision(ctx, p, files, revision.Sealed); err != nil {
		core.Error(w, 409, err.Error())
		return
	}
	core.JSON(w, 200, map[string]int{"revision": len(p.Revisions)})
}

func (m *Manager) openRevision(revision composeRevision) (composeFiles, error) {
	var out composeFiles
	b, err := base64.StdEncoding.DecodeString(revision.Sealed)
	if err != nil {
		return out, err
	}
	plain, err := m.vault.Open(b)
	if err != nil {
		return out, err
	}
	defer clearBytes(plain)
	err = json.Unmarshal(plain, &out)
	return out, err
}

func (m *Manager) writeComposeFiles(ctx context.Context, p *composeProject, files composeFiles) error {
	if err := m.fileWriter.WriteComposeFile(ctx, p.HostID, p.Path, projectComposeFile(p), []byte(files.Compose)); err != nil {
		return err
	}
	// Empty input deliberately clears the old environment instead of silently
	// reusing credentials from the previous revision.
	return m.fileWriter.WriteComposeFile(ctx, p.HostID, p.Path, ".env", []byte(files.Environment))
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
func (m *Manager) runCompose(w http.ResponseWriter, r *http.Request, p *composeProject, action string) {
	if p.Pending != nil {
		core.Error(w, 409, "compose file recovery is required before deployment or lifecycle commands")
		return
	}
	args := composeArgsForFile(p.Path, projectComposeFile(p), action, r)
	if args == nil {
		core.Error(w, 400, "invalid compose operation")
		return
	}
	op, ctx, err := m.beginOperation(p.HostID, "compose-"+action, 30*time.Minute)
	if err != nil {
		core.Error(w, 503, "cannot persist compose operation")
		return
	}
	// Preserve the synchronous success shape while recording interruption and
	// restart recovery through the same durable operation endpoint.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	status, err := m.runDockerCommand(ctx, p.HostID, args)
	if err != nil {
		m.finishOperation(op, ctx, "unknown", "compose operation interrupted; outcome is unknown")
		core.JSON(w, 502, map[string]any{"error": "compose operation interrupted; outcome is unknown", "operationId": op.ID})
		return
	}
	if status != 0 {
		m.finishOperation(op, ctx, "failed", "compose command exited unsuccessfully")
		core.JSON(w, 502, map[string]any{"error": "compose operation failed", "operationId": op.ID})
		return
	}
	m.finishOperation(op, ctx, "completed", "")
	core.JSON(w, 200, map[string]any{"ok": true, "operationId": op.ID})
}
func composeCommand(path, action string, r *http.Request) string {
	args := composeArgs(path, action, r)
	if args == nil {
		return ""
	}
	for i := range args {
		args[i] = shellQuote(args[i])
	}
	return "docker " + strings.Join(args, " ")
}
func composeArgs(path, action string, r *http.Request) []string {
	return composeArgsForFile(path, "compose.yaml", action, r)
}
func projectComposeFile(p *composeProject) string {
	if p.FileName == "compose.yml" {
		return "compose.yml"
	}
	return "compose.yaml"
}
func composeArgsForFile(path, fileName, action string, r *http.Request) []string {
	file := filepath.Join(path, fileName)
	if pathpkg.IsAbs(path) {
		file = pathpkg.Join(path, fileName)
	}
	base := []string{"compose", "--project-directory", path, "-f", file}
	switch action {
	case "validate":
		return append(base, "config", "--quiet")
	case "stop":
		return append(base, "stop")
	case "down":
		return append(base, "down")
	case "deploy":
		var d composeDeploy
		if core.Decode(r, &d) != nil {
			return nil
		}
		a := append(base, "up")
		if d.Pull {
			a = append(a, "--pull", "always")
		}
		if d.Build {
			a = append(a, "--build")
		}
		if d.Detach == nil || *d.Detach {
			a = append(a, "--detach")
		}
		if d.Detach != nil && !*d.Detach {
			return nil
		}
		return a
	}
	return nil
}
func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
func safeRecordID(v string) bool {
	return v != "" && v != "." && v != ".." && !strings.ContainsAny(v, "/\\\x00")
}
func safeName(v string) bool {
	return v != "" && len(v) <= 128 && !strings.ContainsAny(v, "/\\\x00\r\n")
}
func safeComposePath(v string) bool {
	if strings.ContainsAny(v, "\x00\r\n") || v == "/" || v == "\\" {
		return false
	}
	return (filepath.IsAbs(v) || pathpkg.IsAbs(v)) && filepath.Clean(v) != filepath.VolumeName(v)+"\\"
}
