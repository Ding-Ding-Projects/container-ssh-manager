//go:build linux

package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/connection"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

const dockerIntegrationEnv = "CSM_RUN_DOCKER_INTEGRATION"
const dockerIntegrationLabel = "container-ssh-manager.integration"

type fixtureCall func(method, path string, payload any, want int, into any)

// TestDockerFacadeIntegration exercises the real Engine API only when explicitly
// enabled. Every mutable fixture resource carries a unique label and cleanup uses
// that label, never a global prune operation.
func TestDockerFacadeIntegration(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run against a local Docker daemon", dockerIntegrationEnv)
	}
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("Docker Unix socket is unavailable: %v", err)
	}
	preflight, preflightCancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := exec.CommandContext(preflight, "docker", "version", "--format", "{{.Server.Version}}").Run()
	preflightCancel()
	if err != nil {
		t.Fatalf("Docker daemon is unavailable: %v", err)
	}
	composePreflight, composeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = exec.CommandContext(composePreflight, "docker", "compose", "version").Run()
	composeCancel()
	if err != nil {
		t.Fatalf("Docker Compose CLI plugin is unavailable: %v", err)
	}

	id := integrationID(t)
	label := dockerIntegrationLabel + "=" + id
	t.Cleanup(func() { cleanupDockerFixture(t, label) })

	store, err := core.NewStore(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	vault, err := core.NewVault(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(store, connection.New(store, vault), vault).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	call := func(method, path string, payload any, want int, into any) {
		t.Helper()
		var body io.Reader
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, srv.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, resp.StatusCode, want, b)
		}
		if into != nil && len(b) != 0 {
			if text, ok := into.(*string); ok {
				*text = string(b)
				return
			}
			if err := json.Unmarshal(b, into); err != nil {
				t.Fatalf("decode %s %s: %v: %s", method, path, err, b)
			}
		}
	}

	var pull operation
	call(http.MethodPost, "/api/v1/engine/images/pull", map[string]any{"hostId": "local", "reference": "busybox:1.36"}, http.StatusAccepted, &pull)
	deadline := time.Now().Add(2 * time.Minute)
	for pull.State == "running" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		call(http.MethodGet, "/api/v1/engine/operations/"+pull.ID+"?hostId=local", nil, http.StatusOK, &pull)
	}
	if pull.State != "completed" {
		t.Fatalf("image pull did not complete: state=%q error=%q", pull.State, pull.Error)
	}
	tag := "csm-integration:" + id
	call(http.MethodPost, "/api/v1/engine/images/busybox:1.36/tag", map[string]any{"hostId": "local", "repository": "csm-integration", "tag": id}, http.StatusCreated, nil)
	t.Cleanup(func() { cleanupImageFixture(t, tag) })
	var missing operation
	call(http.MethodPost, "/api/v1/engine/images/pull", map[string]any{"hostId": "local", "reference": "busybox:missing-csm-integration-" + id}, http.StatusAccepted, &missing)
	deadline = time.Now().Add(2 * time.Minute)
	for missing.State == "running" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		call(http.MethodGet, "/api/v1/engine/operations/"+missing.ID+"?hostId=local", nil, http.StatusOK, &missing)
	}
	if missing.State != "failed" {
		t.Fatalf("missing image pull did not fail: state=%q error=%q", missing.State, missing.Error)
	}
	buildDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte("FROM scratch\nLABEL "+dockerIntegrationLabel+"="+id+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	buildTag := "csm-integration-build:" + id
	var build operation
	call(http.MethodPost, "/api/v1/engine/images/build", map[string]any{"hostId": "local", "contextPath": buildDir, "tags": []string{buildTag}}, http.StatusAccepted, &build)
	deadline = time.Now().Add(2 * time.Minute)
	for build.State == "running" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		call(http.MethodGet, "/api/v1/engine/operations/"+build.ID+"?hostId=local", nil, http.StatusOK, &build)
	}
	if build.State != "completed" {
		t.Fatalf("scratch image build did not complete: state=%q error=%q", build.State, build.Error)
	}
	t.Cleanup(func() { cleanupImageFixture(t, buildTag) })

	name := "csm-integration-" + id
	volumeName := "csm-integration-" + id
	var volume struct {
		Name string `json:"Name"`
	}
	call(http.MethodPost, "/api/v1/engine/volumes", map[string]any{"hostId": "local", "Name": volumeName, "Labels": map[string]string{dockerIntegrationLabel: id}}, http.StatusCreated, &volume)
	if volume.Name == "" {
		t.Fatal("volume create returned no name")
	}
	networkName := "csm-integration-" + id
	var network struct {
		ID string `json:"Id"`
	}
	call(http.MethodPost, "/api/v1/engine/networks", map[string]any{"hostId": "local", "Name": networkName, "Labels": map[string]string{dockerIntegrationLabel: id}}, http.StatusCreated, &network)
	if network.ID == "" {
		t.Fatal("network create returned no id")
	}
	create := map[string]any{"hostId": "local", "name": name, "config": map[string]any{"Image": "busybox:1.36", "Cmd": []string{"sh", "-c", "echo fixture-ready; sleep 300"}, "Labels": map[string]string{dockerIntegrationLabel: id}, "ExposedPorts": map[string]any{"80/tcp": map[string]any{}}}, "hostConfig": map[string]any{"Mounts": []map[string]any{{"Type": "volume", "Source": volume.Name, "Target": "/fixture"}}, "PortBindings": map[string]any{"80/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "0"}}}}, "networkingConfig": map[string]any{"EndpointsConfig": map[string]any{networkName: map[string]any{}}}}
	var created struct {
		ID string `json:"Id"`
	}
	call(http.MethodPost, "/api/v1/engine/containers", create, http.StatusCreated, &created)
	if created.ID == "" {
		t.Fatal("container create returned no id")
	}
	call(http.MethodPost, "/api/v1/engine/containers/"+created.ID+"/start?hostId=local", nil, http.StatusNoContent, nil)
	expectedPort := containerPort(t, call, created.ID)

	var list []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	call(http.MethodGet, "/api/v1/engine/containers?hostId=local&all=true", nil, http.StatusOK, &list)
	if !containsLabelledContainer(list, created.ID, id) {
		t.Fatal("created fixture was not returned by container list")
	}
	var logs string
	call(http.MethodGet, "/api/v1/engine/containers/"+created.ID+"/logs?hostId=local&tail=20", nil, http.StatusOK, &logs)
	if !strings.Contains(logs, "fixture-ready") {
		t.Fatalf("container logs did not contain fixture output: %q", logs)
	}
	var executed struct {
		ExitCode int    `json:"exitCode"`
		Output   string `json:"output"`
	}
	call(http.MethodPost, "/api/v1/engine/containers/"+created.ID+"/exec", map[string]any{"hostId": "local", "cmd": []string{"sh", "-c", "printf exec-ready"}}, http.StatusOK, &executed)
	if executed.ExitCode != 0 || !strings.Contains(executed.Output, "exec-ready") {
		t.Fatalf("unexpected exec result: %#v", executed)
	}
	call(http.MethodPost, "/api/v1/engine/containers/"+created.ID+"/exec", map[string]any{"hostId": "local", "cmd": []string{"sh", "-c", "printf preserved >/fixture/value"}}, http.StatusOK, nil)

	var recreated struct {
		ID string `json:"id"`
	}
	call(http.MethodPost, "/api/v1/engine/containers/"+created.ID+"/recreate?hostId=local", nil, http.StatusCreated, &recreated)
	if recreated.ID == "" {
		t.Fatal("first recreate returned no id")
	}
	assertRecreatedState(t, call, created.ID, recreated.ID, name, networkName, expectedPort)
	call(http.MethodPost, "/api/v1/engine/containers/"+recreated.ID+"/exec", map[string]any{"hostId": "local", "cmd": []string{"sh", "-c", "cat /fixture/value"}}, http.StatusOK, &executed)
	if executed.ExitCode != 0 || !strings.Contains(executed.Output, "preserved") {
		t.Fatalf("named volume data was not preserved: %#v", executed)
	}
	var recreatedAgain struct {
		ID string `json:"id"`
	}
	call(http.MethodPost, "/api/v1/engine/containers/"+recreated.ID+"/recreate?hostId=local", nil, http.StatusCreated, &recreatedAgain)
	if recreatedAgain.ID == "" {
		t.Fatal("second recreate returned no id")
	}
	assertRecreatedState(t, call, recreated.ID, recreatedAgain.ID, name, networkName, expectedPort)
	call(http.MethodPost, "/api/v1/engine/containers/"+recreatedAgain.ID+"/stop?hostId=local", nil, http.StatusNoContent, nil)
	call(http.MethodDelete, "/api/v1/engine/containers/"+recreatedAgain.ID+"?hostId=local", nil, http.StatusNoContent, nil)
	call(http.MethodGet, "/api/v1/engine/volumes?hostId=local", nil, http.StatusOK, nil)
	call(http.MethodDelete, "/api/v1/engine/volumes/"+volume.Name+"?hostId=local", nil, http.StatusNoContent, nil)
	call(http.MethodGet, "/api/v1/engine/networks?hostId=local", nil, http.StatusOK, nil)
	call(http.MethodDelete, "/api/v1/engine/networks/"+network.ID+"?hostId=local", nil, http.StatusNoContent, nil)

	failingName := "csm-rollback-" + id
	var failing struct {
		ID string `json:"Id"`
	}
	call(http.MethodPost, "/api/v1/engine/containers", map[string]any{"hostId": "local", "name": failingName, "config": map[string]any{"Image": "busybox:1.36", "Cmd": []string{"sh", "-c", "sleep 30"}, "Labels": map[string]string{dockerIntegrationLabel: id}, "Healthcheck": map[string]any{"Test": []string{"CMD-SHELL", "exit 1"}, "Interval": 1000000000, "Retries": 1}}}, http.StatusCreated, &failing)
	call(http.MethodPost, "/api/v1/engine/containers/"+failing.ID+"/start?hostId=local", nil, http.StatusNoContent, nil)
	var rollback map[string]any
	call(http.MethodPost, "/api/v1/engine/containers/"+failing.ID+"/recreate?hostId=local", nil, http.StatusBadGateway, &rollback)
	if rollback["rollback"] != true {
		t.Fatalf("failed replacement did not report rollback: %#v", rollback)
	}
	var restored struct {
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	call(http.MethodGet, "/api/v1/engine/containers/"+failing.ID+"?hostId=local", nil, http.StatusOK, &restored)
	if restored.Name != "/"+failingName || !restored.State.Running {
		t.Fatalf("rollback did not restore running original: %#v", restored)
	}

	composeDir := t.TempDir()
	composeName := "csm-compose-" + id
	var project composeProject
	call(http.MethodPost, "/api/v1/engine/compose/projects", map[string]any{"hostId": "local", "name": composeName, "path": composeDir}, http.StatusCreated, &project)
	call(http.MethodPut, "/api/v1/engine/compose/projects/"+project.ID+"/files", map[string]any{"compose": "name: " + composeName + "\nservices:\n  fixture:\n    image: busybox:1.36\n    command: [\"sh\", \"-c\", \"sleep 300\"]\n    labels:\n      " + dockerIntegrationLabel + ": \"" + id + "\"\nnetworks:\n  default:\n    labels:\n      " + dockerIntegrationLabel + ": \"" + id + "\"\n", "environment": "FIXTURE=1\n"}, http.StatusOK, nil)
	call(http.MethodPost, "/api/v1/engine/compose/projects/"+project.ID+"/validate", nil, http.StatusOK, nil)
	call(http.MethodPost, "/api/v1/engine/compose/projects/"+project.ID+"/deploy", map[string]any{"detach": true}, http.StatusOK, nil)
	call(http.MethodPost, "/api/v1/engine/compose/projects/"+project.ID+"/stop", nil, http.StatusOK, nil)
	call(http.MethodPost, "/api/v1/engine/compose/projects/"+project.ID+"/down", nil, http.StatusOK, nil)
	call(http.MethodPost, "/api/v1/engine/compose/projects/"+project.ID+"/restore/1", nil, http.StatusOK, nil)
	if _, err := os.Stat(filepath.Join(composeDir, "compose.yaml")); err != nil {
		t.Fatalf("compose file was not written locally: %v", err)
	}
	adoptDir := t.TempDir()
	adoptCompose := []byte("services:\n  adopted:\n    image: busybox:1.36\n")
	adoptEnv := []byte("ADOPTED=1\n")
	if err := os.WriteFile(filepath.Join(adoptDir, "compose.yml"), adoptCompose, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adoptDir, ".env"), adoptEnv, 0600); err != nil {
		t.Fatal(err)
	}
	var adopted composeProject
	call(http.MethodPost, "/api/v1/engine/compose/projects", map[string]any{"hostId": "local", "name": "csm-adopt-" + id, "path": adoptDir, "adopt": true}, http.StatusCreated, &adopted)
	var adoptedFiles composeFiles
	call(http.MethodGet, "/api/v1/engine/compose/projects/"+adopted.ID+"/files", nil, http.StatusOK, &adoptedFiles)
	if adoptedFiles.Compose != string(adoptCompose) || adoptedFiles.Environment != string(adoptEnv) {
		t.Fatalf("adopted files differ: %#v", adoptedFiles)
	}
	gotCompose, err := os.ReadFile(filepath.Join(adoptDir, "compose.yml"))
	if err != nil || !bytes.Equal(gotCompose, adoptCompose) {
		t.Fatalf("adoption changed compose.yml: %v", err)
	}
	gotEnv, err := os.ReadFile(filepath.Join(adoptDir, ".env"))
	if err != nil || !bytes.Equal(gotEnv, adoptEnv) {
		t.Fatalf("adoption changed .env: %v", err)
	}
}

func integrationID(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func containerPort(t *testing.T, call fixtureCall, id string) string {
	t.Helper()
	var inspected struct {
		NetworkSettings struct {
			Ports map[string][]struct{ HostPort string } `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	call(http.MethodGet, "/api/v1/engine/containers/"+id+"?hostId=local", nil, http.StatusOK, &inspected)
	ports := inspected.NetworkSettings.Ports["80/tcp"]
	if len(ports) != 1 || ports[0].HostPort == "" || ports[0].HostPort == "0" {
		t.Fatalf("initial loopback port missing: %#v", ports)
	}
	return ports[0].HostPort
}

func assertRecreatedState(t *testing.T, call fixtureCall, oldID, newID, name, network, expectedPort string) {
	t.Helper()
	call(http.MethodGet, "/api/v1/engine/containers/"+oldID+"?hostId=local", nil, http.StatusNotFound, nil)
	var inspected struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage                     `json:"Networks"`
			Ports    map[string][]struct{ HostIP, HostPort string } `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	call(http.MethodGet, "/api/v1/engine/containers/"+newID+"?hostId=local", nil, http.StatusOK, &inspected)
	if inspected.ID != newID || inspected.Name != "/"+name || !inspected.State.Running {
		t.Fatalf("replacement did not retain running original identity: %#v", inspected)
	}
	if _, ok := inspected.NetworkSettings.Networks[network]; !ok {
		t.Fatalf("replacement is not attached to dynamic network %q", network)
	}
	ports := inspected.NetworkSettings.Ports["80/tcp"]
	if len(ports) != 1 || ports[0].HostIP != "127.0.0.1" || ports[0].HostPort != expectedPort {
		t.Fatalf("replacement did not retain loopback-only ephemeral port: %#v", ports)
	}
}

func containsLabelledContainer(items []struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"Labels"`
}, id, value string) bool {
	for _, item := range items {
		if item.ID == id && item.Labels[dockerIntegrationLabel] == value {
			return true
		}
	}
	return false
}

func cleanupImageFixture(t *testing.T, tag string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "rm", tag).Run(); err != nil {
		t.Errorf("fixture image cleanup %q: %v", tag, err)
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run(); err == nil {
		t.Errorf("fixture image cleanup verification retained %q", tag)
	}
}

func cleanupDockerFixture(t *testing.T, label string) {
	t.Helper()
	cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, args := range [][]string{{"ps", "-aq", "--filter", "label=" + label}, {"volume", "ls", "-q", "--filter", "label=" + label}, {"network", "ls", "-q", "--filter", "label=" + label}} {
		item, itemCancel := context.WithTimeout(cleanup, 15*time.Second)
		out, err := exec.CommandContext(item, "docker", args...).Output()
		itemCancel()
		if err != nil {
			t.Errorf("fixture cleanup discovery %q: %v", args, err)
			continue
		}
		for _, resource := range strings.Fields(string(out)) {
			var remove []string
			switch args[0] {
			case "ps":
				remove = []string{"rm", "-f", resource}
			case "volume":
				remove = []string{"volume", "rm", "-f", resource}
			case "network":
				remove = []string{"network", "rm", resource}
			}
			item, itemCancel := context.WithTimeout(cleanup, 15*time.Second)
			if err := exec.CommandContext(item, "docker", remove...).Run(); err != nil {
				t.Errorf("fixture cleanup %q: %v", remove, err)
			}
			itemCancel()
		}
	}
	for _, args := range [][]string{{"ps", "-aq", "--filter", "label=" + label}, {"volume", "ls", "-q", "--filter", "label=" + label}, {"network", "ls", "-q", "--filter", "label=" + label}} {
		item, itemCancel := context.WithTimeout(cleanup, 15*time.Second)
		out, err := exec.CommandContext(item, "docker", args...).Output()
		itemCancel()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("fixture cleanup verification %q: output=%q err=%v", args, out, err)
		}
	}
}
