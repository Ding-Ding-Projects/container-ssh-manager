package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
)

type composeWrite struct {
	State  string `json:"state"`
	Sealed string `json:"sealed,omitempty"`
}

type composeFileWriter interface {
	WriteComposeFile(context.Context, string, string, string, []byte) error
}
type composeWriteIntent struct {
	ProjectID string          `json:"projectId"`
	FileName  string          `json:"fileName"`
	Before    composeSnapshot `json:"before"`
	After     composeFiles    `json:"after"`
}

// Persist the encrypted before/after pair before touching host files. A pending
// intent blocks lifecycle commands across process restarts until it is resolved.
func (m *Manager) commitComposeRevision(ctx context.Context, p *composeProject, files composeFiles, sealedRevision string) error {
	if p.Pending != nil {
		return fmt.Errorf("compose file recovery is required before editing or restoring")
	}
	before, _, err := m.readComposeSnapshot(ctx, p.HostID, p.Path, projectComposeFile(p), true)
	if err != nil {
		return fmt.Errorf("cannot snapshot existing compose files before writing")
	}
	intent := composeWriteIntent{ProjectID: p.ID, FileName: projectComposeFile(p), Before: before, After: files}
	plain, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("cannot encode compose write intent")
	}
	defer clearBytes(plain)
	sealed, err := m.vault.Seal(plain)
	if err != nil {
		return fmt.Errorf("cannot seal compose write intent")
	}
	p.Pending = &composeWrite{State: "pending", Sealed: base64.StdEncoding.EncodeToString(sealed)}
	if err = m.store.Put(composeKind, p.ID, p); err != nil {
		p.Pending = nil
		return fmt.Errorf("cannot persist compose write intent; host files unchanged")
	}
	if err = m.writeComposeFiles(ctx, p, files); err == nil {
		completed := *p
		completed.Revisions = append(append([]composeRevision(nil), p.Revisions...), composeRevision{Number: len(p.Revisions) + 1, CreatedAt: time.Now().UTC(), Sealed: sealedRevision})
		completed.UpdatedAt = time.Now().UTC()
		completed.Pending = nil
		if err = m.store.Put(composeKind, p.ID, completed); err == nil {
			*p = completed
			return nil
		}
	}
	recovery, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = m.recoverComposeWrite(recovery, p); err != nil {
		return fmt.Errorf("compose write incomplete; encrypted recovery intent retained; recover files before deployment")
	}
	return fmt.Errorf("compose write failed; original files restored and no revision committed")
}

func (m *Manager) recoverComposeWrite(ctx context.Context, p *composeProject) error {
	if p.Pending == nil {
		return nil
	}
	sealed, err := base64.StdEncoding.DecodeString(p.Pending.Sealed)
	if err != nil {
		return fmt.Errorf("invalid compose recovery record")
	}
	plain, err := m.vault.Open(sealed)
	if err != nil {
		return fmt.Errorf("cannot open compose recovery record")
	}
	defer clearBytes(plain)
	var intent composeWriteIntent
	if json.Unmarshal(plain, &intent) != nil || intent.ProjectID != p.ID || intent.FileName != projectComposeFile(p) {
		return fmt.Errorf("compose recovery identity does not match project")
	}
	current, _, err := m.readComposeSnapshot(ctx, p.HostID, p.Path, intent.FileName, true)
	if err != nil {
		return fmt.Errorf("cannot inspect current compose files for recovery")
	}
	compatible := func(exists bool, value string, beforeExists bool, before, after string) bool {
		return exists == beforeExists && value == before || exists && value == after
	}
	if !compatible(current.ComposeExists, current.Files.Compose, intent.Before.ComposeExists, intent.Before.Files.Compose, intent.After.Compose) || !compatible(current.EnvironmentExists, current.Files.Environment, intent.Before.EnvironmentExists, intent.Before.Files.Environment, intent.After.Environment) {
		return fmt.Errorf("host files changed independently; manual reconciliation is required")
	}
	restore := func(name string, exists bool, value string, currentExists bool, currentValue string) error {
		if exists == currentExists && value == currentValue {
			return nil
		}
		if exists {
			return m.fileWriter.WriteComposeFile(ctx, p.HostID, p.Path, name, []byte(value))
		}
		return m.removeComposeFile(ctx, p.HostID, p.Path, name)
	}
	if err = restore(intent.FileName, intent.Before.ComposeExists, intent.Before.Files.Compose, current.ComposeExists, current.Files.Compose); err != nil {
		return fmt.Errorf("cannot restore original compose file")
	}
	if err = restore(".env", intent.Before.EnvironmentExists, intent.Before.Files.Environment, current.EnvironmentExists, current.Files.Environment); err != nil {
		return fmt.Errorf("cannot restore original environment file")
	}
	verified, _, err := m.readComposeSnapshot(ctx, p.HostID, p.Path, intent.FileName, true)
	if err != nil || verified != intent.Before {
		return fmt.Errorf("compose recovery could not be verified")
	}
	recovered := *p
	recovered.Pending = nil
	recovered.UpdatedAt = time.Now().UTC()
	if err = m.store.Put(composeKind, p.ID, recovered); err != nil {
		return fmt.Errorf("files restored but durable recovery update failed")
	}
	*p = recovered
	return nil
}

// This is used only for a journal-proven file created by this write attempt.
func (m *Manager) removeComposeFile(ctx context.Context, hostID, dir, name string) error {
	if name != "compose.yaml" && name != "compose.yml" && name != ".env" {
		return fmt.Errorf("invalid compose filename")
	}
	if hostID == "local" {
		err := os.Remove(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	client, err := m.connections.Dial(ctx, hostID)
	if err != nil {
		return err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	files, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer files.Close()
	err = files.Remove(path.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
