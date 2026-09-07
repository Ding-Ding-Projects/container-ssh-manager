package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
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
	Adopted   bool              `json:"adopted"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Revisions []composeRevision `json:"revisions"`
}
type composeRevision struct {
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"createdAt"`
	Sealed    string    `json:"-"`
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
		core.JSON(w, 200, project)
		return
	}
	if len(p) == 3 && p[2] == "files" && r.Method == http.MethodPut {
		m.saveFiles(w, r, &project)
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
			out = append(out, p)
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
	now := time.Now().UTC()
	p := composeProject{ID: core.ID(), HostID: in.HostID, Name: in.Name, Path: filepath.Clean(in.Path), Adopted: in.Adopt, CreatedAt: now, UpdatedAt: now}
	if m.store.Put(composeKind, p.ID, p) != nil {
		core.Error(w, 500, "cannot save compose project")
		return
	}
	core.JSON(w, 201, p)
}
func (m *Manager) loadCompose(id string) (composeProject, error) {
	var p composeProject
	return p, m.store.Get(composeKind, id, &p)
}
func (m *Manager) saveFiles(w http.ResponseWriter, r *http.Request, p *composeProject) {
	var f composeFiles
	if core.Decode(r, &f) != nil || strings.TrimSpace(f.Compose) == "" {
		core.Error(w, 400, "compose content is required")
		return
	}
	plain, _ := json.Marshal(f)
	sealed, err := m.vault.Seal(plain)
	if err != nil {
		core.Error(w, 500, "cannot seal compose revision")
		return
	}
	// Write before recording the revision so a successful response always names a
	// revision that exists on the selected host. The connection manager performs an
	// atomic SFTP rename and rejects unsafe remote paths.
	if err := m.writeComposeFiles(r.Context(), p, f); err != nil {
		core.Error(w, 502, "cannot write compose files")
		return
	}
	p.Revisions = append(p.Revisions, composeRevision{Number: len(p.Revisions) + 1, CreatedAt: time.Now().UTC(), Sealed: base64.StdEncoding.EncodeToString(sealed)})
	p.UpdatedAt = time.Now().UTC()
	if m.store.Put(composeKind, p.ID, p) != nil {
		core.Error(w, 500, "cannot save compose revision")
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
	if err := m.writeComposeFiles(r.Context(), p, files); err != nil {
		core.Error(w, 502, "cannot restore compose files")
		return
	}
	revision.Number = len(p.Revisions) + 1
	revision.CreatedAt = time.Now().UTC()
	p.Revisions = append(p.Revisions, revision)
	p.UpdatedAt = time.Now().UTC()
	if m.store.Put(composeKind, p.ID, p) != nil {
		core.Error(w, 500, "cannot restore compose revision")
		return
	}
	core.JSON(w, 200, map[string]int{"revision": revision.Number})
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
	return out, json.Unmarshal(plain, &out)
}

func (m *Manager) writeComposeFiles(ctx context.Context, p *composeProject, files composeFiles) error {
	if err := m.connections.WriteFile(ctx, p.HostID, filepath.Join(p.Path, "compose.yaml"), []byte(files.Compose)); err != nil {
		return err
	}
	if files.Environment != "" {
		return m.connections.WriteFile(ctx, p.HostID, filepath.Join(p.Path, ".env"), []byte(files.Environment))
	}
	return nil
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
func (m *Manager) runCompose(w http.ResponseWriter, r *http.Request, p *composeProject, action string) {
	cmd := composeCommand(p.Path, action, r)
	if cmd == "" {
		core.Error(w, 400, "invalid compose operation")
		return
	}
	status, err := m.connections.Run(context.Background(), p.HostID, cmd)
	if err != nil {
		core.Error(w, 502, "compose operation interrupted; outcome is unknown")
		return
	}
	if status != 0 {
		core.Error(w, 502, "compose operation failed")
		return
	}
	core.JSON(w, 200, map[string]bool{"ok": true})
}
func composeCommand(path, action string, r *http.Request) string {
	base := "docker compose --project-directory " + shellQuote(path) + " -f " + shellQuote(filepath.Join(path, "compose.yaml")) + " "
	switch action {
	case "validate":
		return base + "config --quiet"
	case "stop":
		return base + "stop"
	case "down":
		return base + "down"
	case "deploy":
		var d composeDeploy
		if core.Decode(r, &d) != nil {
			return ""
		}
		a := "up"
		if d.Pull {
			a += " --pull always"
		}
		if d.Build {
			a += " --build"
		}
		if d.Detach == nil || *d.Detach {
			a += " --detach"
		}
		return base + a
	}
	return ""
}
func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }
func safeRecordID(v string) bool { return v != "" && !strings.ContainsAny(v, "/\\\x00") }
func safeName(v string) bool {
	return v != "" && len(v) <= 128 && !strings.ContainsAny(v, "/\\\x00\r\n")
}
func safeComposePath(v string) bool {
	return filepath.IsAbs(v) && v != filepath.VolumeName(v)+"\\" && !strings.ContainsAny(v, "\x00\r\n")
}
