package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

func jobsHTTP(t *testing.T, m *Manager) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	m.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func requestJSON(t *testing.T, server *httptest.Server, method, path string, value any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func postRuns(t *testing.T, server *httptest.Server, hosts ...string) []Run {
	t.Helper()
	status, body := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostIDs: hosts, RevisionID: "revision-1"})
	if status != 202 {
		t.Fatalf("POST status %d: %s", status, body)
	}
	var response struct {
		Runs []Run `json:"runs"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Runs) != len(hosts) {
		t.Fatalf("accepted %d runs for %d hosts", len(response.Runs), len(hosts))
	}
	return response.Runs
}

func TestHTTPAcceptedRunSurvivesHandlerReturn(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	release := make(chan struct{})
	started := make(chan context.Context, 1)
	m.run = func(ctx context.Context, _, _ string) (int, error) {
		started <- ctx
		select {
		case <-release:
			return 0, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	server := jobsHTTP(t, m)
	runs := postRuns(t, server, "a")
	if runs[0].Source != "manual" || runs[0].Intent != "USER_APPROVED" {
		t.Fatal("manual run did not receive server-owned provenance")
	}
	ctx := <-started
	// A second HTTP exchange proves the POST handler and its request lifecycle
	// ended while execution is still waiting for this explicit release.
	status, _ := requestJSON(t, server, "GET", "/api/v1/jobs/runs/"+runs[0].ID, nil)
	if status != 200 {
		t.Fatal(status)
	}
	select {
	case <-ctx.Done():
		t.Fatal("HTTP handler cancelled accepted work")
	default:
	}
	close(release)
	waitRun(t, m, runs[0].ID, "succeeded")
	status, body := requestJSON(t, server, "GET", "/api/v1/jobs/audit", nil)
	var events []AuditEvent
	if status != 200 || json.Unmarshal(body, &events) != nil {
		t.Fatal("cannot read audit")
	}
	found := false
	for _, event := range events {
		if event.Action == "run_intent_saved" && event.RunID == runs[0].ID {
			found = event.Source == "manual" && event.Intent == "USER_APPROVED" && event.Actor == "session"
		}
	}
	if !found {
		t.Fatal("transaction did not retain server-owned acceptance provenance")
	}
}

func TestHTTPConcurrentHostAcceptanceIsExclusive(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	m.run = func(ctx context.Context, _, _ string) (int, error) { <-ctx.Done(); return 0, ctx.Err() }
	server := jobsHTTP(t, m)
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make(chan int, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostID: "same-host", RevisionID: "revision-1"})
			statuses <- status
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	accepted := 0
	for status := range statuses {
		if status == 202 {
			accepted++
		} else if status != 409 {
			t.Fatal(status)
		}
	}
	if accepted != 1 {
		t.Fatalf("same host accepted %d simultaneous jobs", accepted)
	}
	runs, err := m.runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("persisted %d intents", len(runs))
	}
}

func TestHTTPBatchConflictAcceptsNothing(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	m.run = func(ctx context.Context, _, _ string) (int, error) { <-ctx.Done(); return 0, ctx.Err() }
	server := jobsHTTP(t, m)
	postRuns(t, server, "occupied")
	status, body := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostIDs: []string{"free", "occupied"}, RevisionID: "revision-1"})
	if status != 409 {
		t.Fatalf("status %d: %s", status, body)
	}
	runs, err := m.runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].HostID != "occupied" {
		t.Fatalf("partial batch persisted: %#v", runs)
	}
	// The free host remains usable after the rejected transaction.
	postRuns(t, server, "free")
}

func TestHTTPBatchPersistenceFailureRollsBackAllIntents(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	m.run = func(context.Context, string, string) (int, error) {
		t.Error("rolled-back intent executed")
		return 0, nil
	}
	_, err := m.store.DB.Exec(`CREATE TRIGGER reject_second BEFORE INSERT ON records WHEN NEW.kind='job_run' AND json_extract(NEW.data,'$.hostId')='second' BEGIN SELECT RAISE(ABORT,'fixture write failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	server := jobsHTTP(t, m)
	status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostIDs: []string{"first", "second"}, RevisionID: "revision-1"})
	if status != 503 {
		t.Fatal(status)
	}
	runs, err := m.runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatal("partial intent survived rollback")
	}
}

func TestHTTPQueuedCancellationAndImmutableRevision(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	started := make(chan string, 6)
	release := make(chan struct{})
	m.run = func(ctx context.Context, host, command string) (int, error) {
		if command != "true" {
			t.Errorf("snapshot changed to %q", command)
		}
		started <- host
		select {
		case <-release:
			return 0, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	server := jobsHTTP(t, m)
	initial := postRuns(t, server, "a", "b", "c", "d")
	for range initial {
		<-started
	}
	queued := postRuns(t, server, "cancel-me", "keep-me")
	status, body := requestJSON(t, server, "POST", "/api/v1/jobs/runs/"+queued[0].ID+"/cancel", nil)
	if status != 202 || !bytes.Contains(body, []byte(`"cancelled"`)) {
		t.Fatalf("cancel %d: %s", status, body)
	}
	waitRun(t, m, queued[0].ID, "cancelled")
	status, _ = requestJSON(t, server, "DELETE", "/api/v1/jobs/snippets/snippet-1", nil)
	if status != 204 {
		t.Fatal(status)
	}
	close(release)
	for _, run := range initial {
		waitRun(t, m, run.ID, "succeeded")
	}
	waitRun(t, m, queued[1].ID, "succeeded")
	if host := <-started; host != "keep-me" {
		t.Fatalf("cancelled queued job executed: %q", host)
	}
	select {
	case host := <-started:
		t.Fatalf("unexpected execution: %q", host)
	default:
	}
}

func TestHTTPRejectsSpoofedServerFields(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	server := jobsHTTP(t, m)
	for _, field := range []string{"source", "intent", "scheduleId", "status", "commandRevision", "cancelRequested", "id", "startedAt"} {
		status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", map[string]any{"hostId": "a", "revisionId": "revision-1", field: "forged"})
		if status != 400 {
			t.Fatalf("field %s accepted with status %d", field, status)
		}
	}
	runs, err := m.runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatal("spoofed request persisted")
	}
}

func TestHTTPQueueBoundRejectsWholeBatch(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	m.run = func(ctx context.Context, _, _ string) (int, error) { <-ctx.Done(); return 0, ctx.Err() }
	server := jobsHTTP(t, m)
	hosts := make([]string, maxAccepted)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("host-%d", i)
	}
	postRuns(t, server, hosts...)
	status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostIDs: []string{"extra1", "extra2"}, RevisionID: "revision-1"})
	if status != 409 {
		t.Fatal(status)
	}
	runs, err := m.runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != maxAccepted {
		t.Fatalf("accepted count %d", len(runs))
	}
}

func TestHTTPOutputRetrievalAndRetentionBound(t *testing.T) {
	m := newTestManager(t)
	vault, err := core.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m.vault = vault
	revision := addRevision(t, m)
	revision.Retention = Retention{Enabled: true, MaxBytes: 32, MaxRuns: 1}
	if err := m.store.Put("job_snippet", "snippet-1", Snippet{ID: "snippet-1", Revisions: []Revision{revision}}); err != nil {
		t.Fatal(err)
	}
	m.runOutput = func(context.Context, string, string, int) (int, []byte, error) {
		return 0, []byte("retained private bytes"), nil
	}
	server := jobsHTTP(t, m)
	first := postRuns(t, server, "a")[0]
	waitRun(t, m, first.ID, "succeeded")
	second := postRuns(t, server, "a")[0]
	waitRun(t, m, second.ID, "succeeded")
	status, _ := requestJSON(t, server, "GET", "/api/v1/jobs/runs/"+first.ID+"/output", nil)
	if status != 404 {
		t.Fatalf("expired output status %d", status)
	}
	resp, err := server.Client().Get(server.URL + "/api/v1/jobs/runs/" + second.ID + "/output")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || !bytes.Contains(body, []byte("retained private bytes")) {
		t.Fatalf("output retrieval %d: %s", resp.StatusCode, body)
	}
	var record RetainedOutput
	if err := m.store.Get("job_output", second.ID, &record); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(record.Ciphertext, []byte("retained private bytes")) {
		t.Fatal("plaintext persisted")
	}
	status, body = requestJSON(t, server, "GET", "/api/v1/jobs/runs", nil)
	if status != 200 || bytes.Contains(body, []byte("retained private bytes")) {
		t.Fatal("ordinary metadata exposed output")
	}
	record.Ciphertext[0] ^= 1
	if err := m.store.Put("job_output", second.ID, record); err != nil {
		t.Fatal(err)
	}
	status, body = requestJSON(t, server, "GET", "/api/v1/jobs/runs/"+second.ID+"/output", nil)
	if status != 500 || bytes.Contains(body, []byte("retained private bytes")) {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestHTTPImpossibleCronPreviewIsBounded(t *testing.T) {
	m := newTestManager(t)
	server := jobsHTTP(t, m)
	start := time.Now()
	status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/schedules/preview", map[string]any{"cron": "0 0 31 2 *", "after": "2026-09-07T00:00:00Z"})
	if status != 400 {
		t.Fatal(status)
	}
	if time.Since(start) > time.Second {
		t.Fatal("impossible preview did not return promptly")
	}
	status, body := requestJSON(t, server, "POST", "/api/v1/jobs/schedules/preview", map[string]any{"cron": "5/15 * * * *", "after": "2026-09-07T00:00:00Z", "timezone": "UTC", "count": 4})
	var result struct {
		Times []time.Time `json:"times"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if status != 200 || len(result.Times) != 4 {
		t.Fatalf("preview %d: %s", status, body)
	}
	for i, at := range result.Times {
		if at.Minute() != 5+15*i {
			t.Fatalf("step occurrence %d = %v", i, at)
		}
	}
}

func outputManager(t *testing.T, maxBytes, maxRuns int) *Manager {
	t.Helper()
	m := newTestManager(t)
	vault, err := core.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m.vault = vault
	revision := addRevision(t, m)
	revision.Retention = Retention{Enabled: true, MaxBytes: maxBytes, MaxRuns: maxRuns}
	if err := m.store.Put("job_snippet", "snippet-1", Snippet{ID: "snippet-1", Revisions: []Revision{revision}}); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHTTPConcurrentOutputRetentionCannotExceedRecordBound(t *testing.T) {
	m := outputManager(t, 32, 2)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	m.runOutput = func(ctx context.Context, _, _ string, _ int) (int, []byte, error) {
		started <- struct{}{}
		select {
		case <-release:
			return 0, []byte("private retained bytes"), nil
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	server := jobsHTTP(t, m)
	runs := postRuns(t, server, "a", "b", "c", "d")
	for range runs {
		<-started
	}
	close(release)
	for _, run := range runs {
		waitRun(t, m, run.ID, "succeeded")
	}
	records, err := m.store.List("job_output")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("retained %d records, want 2", len(records))
	}
}

func TestHTTPRetentionPruneFailureRollsBackNewOutput(t *testing.T) {
	m := outputManager(t, 32, 1)
	m.runOutput = func(context.Context, string, string, int) (int, []byte, error) {
		return 0, []byte("private retained bytes"), nil
	}
	server := jobsHTTP(t, m)
	first := postRuns(t, server, "a")[0]
	waitRun(t, m, first.ID, "succeeded")
	_, err := m.store.DB.Exec(`CREATE TRIGGER reject_output_prune BEFORE DELETE ON records WHEN OLD.kind='job_output' BEGIN SELECT RAISE(ABORT,'fixture prune failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	second := postRuns(t, server, "a")[0]
	waitRun(t, m, second.ID, "failed")
	records, err := m.store.List("job_output")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatal("failed prune exceeded retention bound")
	}
	status, _ := requestJSON(t, server, "GET", "/api/v1/jobs/runs/"+second.ID+"/output", nil)
	if status != 404 {
		t.Fatal("new output survived failed retention transaction")
	}
	status, _ = requestJSON(t, server, "GET", "/api/v1/jobs/runs/"+first.ID+"/output", nil)
	if status != 200 {
		t.Fatal("previous output was lost on rollback")
	}
}

func TestHTTPRetainedOutputOverByteLimitIsNeverStored(t *testing.T) {
	m := outputManager(t, 8, 1)
	m.runOutput = func(context.Context, string, string, int) (int, []byte, error) {
		return 0, []byte("oversized output"), nil
	}
	server := jobsHTTP(t, m)
	run := postRuns(t, server, "a")[0]
	waitRun(t, m, run.ID, "failed")
	status, _ := requestJSON(t, server, "GET", "/api/v1/jobs/runs/"+run.ID+"/output", nil)
	if status != 404 {
		t.Fatal("oversized output retrievable")
	}
	records, err := m.store.List("job_output")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatal("oversized output persisted")
	}
}
