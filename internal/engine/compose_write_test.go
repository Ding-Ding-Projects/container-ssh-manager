package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/connection"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

type composeWriterFunc func(context.Context, string, string, string, []byte) error

func (f composeWriterFunc) WriteComposeFile(ctx context.Context, host, dir, name string, data []byte) error {
	return f(ctx, host, dir, name, data)
}

func TestComposeWriteFailureRestoresBothFiles(t *testing.T) {
	m, p, original := composeWriteFixture(t)
	writer := m.fileWriter
	failed := false
	m.fileWriter = composeWriterFunc(func(ctx context.Context, host, dir, name string, data []byte) error {
		if name == ".env" && !failed {
			failed = true
			return errors.New("injected second write failure")
		}
		return writer.WriteComposeFile(ctx, host, dir, name, data)
	})
	next := composeFiles{Compose: "services: {changed: {image: busybox}}\n", Environment: "REPLACEMENT=value\n"}
	if err := m.commitComposeRevision(context.Background(), &p, next, sealFixtureRevision(t, m, next)); err == nil {
		t.Fatal("second-file failure reported success")
	}
	assertComposeSnapshot(t, m, &p, original)
	stored, err := m.loadCompose(p.ID)
	if err != nil || stored.Pending != nil || len(stored.Revisions) != 1 {
		t.Fatalf("rollback record: %+v %v", stored, err)
	}
}

func TestComposeFinalPersistenceFailureBlocksDeploymentAcrossRestart(t *testing.T) {
	m, p, original := composeWriteFixture(t)
	_, err := m.store.DB.Exec(`CREATE TRIGGER fail_compose_commit BEFORE UPDATE OF data ON records WHEN NEW.kind='engine-compose' AND json_extract(NEW.data,'$.pending') IS NULL BEGIN SELECT RAISE(FAIL,'injected final persistence failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	next := composeFiles{Compose: "services: {changed: {image: busybox}}\n", Environment: "REPLACEMENT=value\n"}
	if err = m.commitComposeRevision(context.Background(), &p, next, sealFixtureRevision(t, m, next)); err == nil {
		t.Fatal("failed final persistence reported success")
	}
	assertComposeSnapshot(t, m, &p, original)
	restarted := New(m.store, m.connections, m.vault)
	stored, err := restarted.loadCompose(p.ID)
	if err != nil || stored.Pending == nil || len(stored.Revisions) != 1 {
		t.Fatal("pending intent was lost")
	}
	public, _ := json.Marshal(publicCompose(stored))
	if bytes.Contains(public, []byte("sealed")) || bytes.Contains(public, []byte("REPLACEMENT")) {
		t.Fatal("pending metadata disclosed content")
	}
	response := httptest.NewRecorder()
	restarted.runCompose(response, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{}`)), &stored, "deploy")
	if response.Code != http.StatusConflict {
		t.Fatalf("deployment not blocked: %d", response.Code)
	}
	if _, err = m.store.DB.Exec(`DROP TRIGGER fail_compose_commit`); err != nil {
		t.Fatal(err)
	}
	if err = restarted.recoverComposeWrite(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Pending != nil {
		t.Fatal("recovery did not clear intent")
	}
	assertComposeSnapshot(t, restarted, &stored, original)
}

func TestComposeRecoveryPreservesIndependentHostEdits(t *testing.T) {
	m, p, _ := composeWriteFixture(t)
	_, err := m.store.DB.Exec(`CREATE TRIGGER fail_compose_commit BEFORE UPDATE OF data ON records WHEN NEW.kind='engine-compose' AND json_extract(NEW.data,'$.pending') IS NULL BEGIN SELECT RAISE(FAIL,'injected final persistence failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	next := composeFiles{Compose: "services: {}\n# new\n", Environment: "NEW=value\n"}
	_ = m.commitComposeRevision(context.Background(), &p, next, sealFixtureRevision(t, m, next))
	if err = os.WriteFile(filepath.Join(p.Path, ".env"), []byte("INDEPENDENT=value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.recoverComposeWrite(context.Background(), &p); err == nil {
		t.Fatal("independent edits were accepted for automatic replacement")
	}
	got, _ := os.ReadFile(filepath.Join(p.Path, ".env"))
	if string(got) != "INDEPENDENT=value\n" {
		t.Fatal("independent edit destroyed")
	}
}

func composeWriteFixture(t *testing.T) (*Manager, composeProject, composeSnapshot) {
	t.Helper()
	store, err := core.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	vault, _ := core.NewVault(bytes.Repeat([]byte{3}, 32))
	m := New(store, connection.New(store, vault), vault)
	dir := t.TempDir()
	files := composeFiles{Compose: "services: {}\n", Environment: "INITIAL=value\n"}
	if err = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(files.Compose), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ".env"), []byte(files.Environment), 0600); err != nil {
		t.Fatal(err)
	}
	p := composeProject{ID: core.ID(), HostID: "local", Name: "fixture", Path: dir, Revisions: []composeRevision{{Number: 1, Sealed: sealFixtureRevision(t, m, files)}}}
	if err = store.Put(composeKind, p.ID, p); err != nil {
		t.Fatal(err)
	}
	return m, p, composeSnapshot{Files: files, ComposeExists: true, EnvironmentExists: true}
}
func sealFixtureRevision(t *testing.T, m *Manager, files composeFiles) string {
	t.Helper()
	plain, _ := json.Marshal(files)
	sealed, err := m.vault.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sealed)
}
func assertComposeSnapshot(t *testing.T, m *Manager, p *composeProject, want composeSnapshot) {
	t.Helper()
	got, _, err := m.readComposeSnapshot(context.Background(), p.HostID, p.Path, projectComposeFile(p), true)
	if err != nil || got != want {
		t.Fatalf("host file pair not restored: %v", err)
	}
}
