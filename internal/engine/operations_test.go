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
	"strings"
	"testing"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

func TestPullStreamErrorAndTruncation(t *testing.T) {
	for _, input := range []string{`{"error":"credential must not reach stored error"}`, `{"status":"pulling"}` + "\n" + `{"errorDetail":{"message":"failed"}}`, `{"status":`, ""} {
		if decodePullStream(strings.NewReader(input)) == nil {
			t.Fatalf("invalid pull stream accepted: %q", input)
		}
	}
	if err := decodePullStream(strings.NewReader("{\"status\":\"Pulling\"}\n{\"status\":\"Downloaded\"}\n")); err != nil {
		t.Fatal(err)
	}
}

func TestOperationCancelTimeoutAndRestart(t *testing.T) {
	store, err := core.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := New(store, nil, nil)
	op, ctx, err := m.beginOperation("local", "build", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	m.operationHandler(response, httptest.NewRequest(http.MethodPost, "/", nil), op.ID+"/cancel")
	if response.Code != 202 || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("cancel not delivered: %s", response.Body)
	}
	m.finishOperation(op, ctx, "completed", "")
	var got operation
	if err = store.Get(operationKind, op.ID, &got); err != nil || got.State != "canceled" || got.Outcome != "unknown" {
		t.Fatalf("late success overwrote cancellation: %+v %v", got, err)
	}
	timed, deadline, err := m.beginOperation("local", "pull", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	<-deadline.Done()
	m.finishOperation(timed, deadline, "completed", "")
	_ = store.Get(operationKind, timed.ID, &got)
	if got.State != "timed_out" || got.Outcome != "unknown" {
		t.Fatalf("timeout reported as success: %+v", got)
	}
	pending, pendingctx, err := m.beginOperation("local", "recreate", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer m.active[pending.ID]()
	restarted := New(store, nil, nil)
	if restarted.recoveryErr != nil {
		t.Fatal(restarted.recoveryErr)
	}
	_ = store.Get(operationKind, pending.ID, &got)
	if got.State != "unknown" || got.Outcome != "unknown" || pendingctx.Err() != nil {
		t.Fatalf("restart recovery: %+v", got)
	}
	listed := httptest.NewRecorder()
	restarted.operationHandler(listed, httptest.NewRequest(http.MethodGet, "/?hostId=local", nil), "")
	var records []operation
	if listed.Code != 200 || json.Unmarshal(listed.Body.Bytes(), &records) != nil || len(records) != 3 {
		t.Fatalf("restart operation history unavailable: %s", listed.Body)
	}
}

func TestPayloadTranslationMatchesEngineAPI(t *testing.T) {
	path, body, err := translatePayload([]string{"containers"}, "POST", "/containers/create", []byte(`{"name":"demo","config":{"Image":"busybox","Env":["A=B"]},"hostConfig":{"PortBindings":{}},"networkingConfig":{"EndpointsConfig":{"test":{}}}}`))
	if err != nil || path != "/containers/create?name=demo" {
		t.Fatalf("%s %v", path, err)
	}
	var values map[string]json.RawMessage
	_ = json.Unmarshal(body, &values)
	if values["Image"] == nil || values["Config"] != nil || values["config"] != nil || values["HostConfig"] == nil || values["NetworkingConfig"] == nil {
		t.Fatalf("incorrect create shape: %s", body)
	}
	path, _, err = translatePayload([]string{"images", "sha256:abc", "tag"}, "POST", "/images/sha256:abc/tag", []byte(`{"repository":"fixture","tag":"one"}`))
	if err != nil || path != "/images/sha256:abc/tag?repo=fixture&tag=one" {
		t.Fatalf("tag route: %s %v", path, err)
	}
	path, body, err = translatePayload([]string{"containers", "abc", "stop"}, "POST", "/containers/abc/stop", []byte(`{"timeoutSeconds":17}`))
	if err != nil || path != "/containers/abc/stop?t=17" || len(body) != 0 {
		t.Fatalf("stop route: %s %v", path, err)
	}
}

func TestRecreatePayloadPreservesAnonymousVolumesAndRejectsAutoRemove(t *testing.T) {
	var in containerInspection
	if err := json.Unmarshal([]byte(`{"Id":"abc","Config":{"Image":"busybox","Volumes":{"/data":{}}},"HostConfig":{"AutoRemove":false},"Mounts":[{"Type":"volume","Name":"generated-volume","Destination":"/data","RW":true}],"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.1.0.2","EndpointID":"old","Aliases":null}}}}`), &in); err != nil {
		t.Fatal(err)
	}
	body, err := recreatePayload(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"Config"`)) || !bytes.Contains(body, []byte(`"Source":"generated-volume"`)) || bytes.Contains(body, []byte(`"IPAddress"`)) || !bytes.Contains(body, []byte(`"EndpointsConfig"`)) {
		t.Fatalf("unsafe recreation: %s", body)
	}
	in.HostConfig["AutoRemove"] = json.RawMessage("true")
	if _, err = recreatePayload(in); err == nil {
		t.Fatal("auto-remove would destroy original")
	}
	in.HostConfig["AutoRemove"] = json.RawMessage("false")
	in.HostConfig["VolumesFrom"] = json.RawMessage(`["source:rw"]`)
	if _, err = recreatePayload(in); err == nil {
		t.Fatal("mixed inherited and independent anonymous volumes accepted")
	}
}

func TestComposeRevisionPersistsButPublicMetadataOmitsSealedContent(t *testing.T) {
	store, err := core.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, _ := core.NewVault(bytes.Repeat([]byte{1}, 32))
	m := New(store, nil, vault)
	sealed, _ := vault.Seal([]byte(`{"compose":"services: {}","environment":"VALUE=test"}`))
	// Save the exact model through Store to catch json:"-" silently losing content.
	encoded := base64.StdEncoding.EncodeToString(sealed)
	p := composeProject{ID: "test", Revisions: []composeRevision{{Number: 1, Sealed: encoded}}}
	if err = store.Put(composeKind, p.ID, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := m.loadCompose(p.ID)
	if err != nil || loaded.Revisions[0].Sealed != encoded {
		t.Fatalf("revision lost: %+v %v", loaded, err)
	}
	public, _ := json.Marshal(publicCompose(loaded))
	if bytes.Contains(public, []byte(encoded)) || bytes.Contains(public, []byte("VALUE=test")) || loaded.Revisions[0].Sealed != encoded {
		t.Fatal("public projection leaked or mutated stored revision")
	}
	editor := httptest.NewRecorder()
	m.compose(editor, httptest.NewRequest(http.MethodGet, "/", nil), "projects/test/files")
	var files composeFiles
	if editor.Code != 200 || json.Unmarshal(editor.Body.Bytes(), &files) != nil || files.Environment != "VALUE=test" || editor.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("explicit editor content unavailable: %s", editor.Body)
	}
}

func TestDockerOutputDemultiplexesAndRejectsTruncatedFrame(t *testing.T) {
	frame := append([]byte{1, 0, 0, 0, 0, 0, 0, 3}, []byte("out")...)
	frame = append(frame, append([]byte{2, 0, 0, 0, 0, 0, 0, 3}, []byte("err")...)...)
	var decoded bytes.Buffer
	if err := copyDockerOutput(&decoded, bytes.NewReader(frame)); err != nil || decoded.String() != "outerr" {
		t.Fatalf("decode: %q %v", decoded.String(), err)
	}
	if copyDockerOutput(&bytes.Buffer{}, bytes.NewReader(frame[:len(frame)-1])) == nil {
		t.Fatal("truncated frame accepted")
	}
}

func TestComposeAdoptReadsWithoutMutationAndSealsInitialRevision(t *testing.T) {
	dir := t.TempDir()
	yaml := "services: {}\n"
	environment := "PRIVATE_FIXTURE=value\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(environment), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := core.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, _ := core.NewVault(bytes.Repeat([]byte{2}, 32))
	m := New(store, nil, vault)
	body, _ := json.Marshal(map[string]any{"hostId": "local", "name": "adopted", "path": dir, "adopt": true})
	w := httptest.NewRecorder()
	m.compose(w, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)), "projects")
	var created composeProject
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.FileName != "compose.yml" || len(created.Revisions) != 1 || created.Revisions[0].Sealed != "" {
		t.Fatalf("adoption metadata: %s", w.Body)
	}
	stored, err := m.loadCompose(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := m.openRevision(stored.Revisions[0])
	if err != nil || plain.Compose != yaml || plain.Environment != environment {
		t.Fatal("initial encrypted revision lost content")
	}
	for name, want := range map[string]string{"compose.yml": yaml, ".env": environment} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatal("adoption modified source files")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); !os.IsNotExist(err) {
		t.Fatal("adoption created a new compose file")
	}
	if _, _, err := m.readComposeProject(context.Background(), "local", t.TempDir()); err == nil {
		t.Fatal("missing compose file accepted")
	}
}
