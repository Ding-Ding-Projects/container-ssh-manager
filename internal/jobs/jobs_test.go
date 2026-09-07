package jobs

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

func TestCronPreviewUsesTorontoLocalTime(t *testing.T) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := parseCron("0 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Match(time.Date(2026, 9, 7, 9, 0, 0, 0, loc)) {
		t.Fatal("Monday 09:00 Toronto must match")
	}
	if spec.Match(time.Date(2026, 9, 7, 8, 59, 0, 0, loc)) {
		t.Fatal("wrong minute matched")
	}
}

func TestCronSupportsListsRangesAndSteps(t *testing.T) {
	spec, err := parseCron("*/15 1-5/2 1,15 1,6 0,6")
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Match(time.Date(2025, 6, 15, 3, 30, 0, 0, time.UTC)) {
		t.Fatal("expected composed expression to match")
	}
	if spec.Match(time.Date(2025, 6, 15, 2, 30, 0, 0, time.UTC)) {
		t.Fatal("step expression matched wrong hour")
	}
}

func TestCronRejectsUnsafeOrAmbiguousFields(t *testing.T) {
	for _, text := range []string{"* * * *", "60 * * * *", "*/0 * * * *", "* * * * 7"} {
		if _, err := parseCron(text); err == nil {
			t.Fatalf("%q unexpectedly parsed", text)
		}
	}
}

func TestScheduleTimezoneAndHostsAreDeterministic(t *testing.T) {
	loc, err := scheduleLocation("America/New_York")
	if err != nil || loc.String() != "America/New_York" {
		t.Fatalf("location = %v, %v", loc, err)
	}
	if _, err := scheduleLocation("not/a-timezone"); err == nil {
		t.Fatal("invalid timezone accepted")
	}
	hosts := scheduleHosts(Schedule{HostIDs: []string{"host-1", "host-1", " host-2 "}})
	if len(hosts) != 2 || hosts[0] != "host-1" || hosts[1] != "host-2" {
		t.Fatalf("hosts = %#v", hosts)
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	store, err := core.NewStore(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{store: store, location: loc, now: time.Now, running: make(map[string]context.CancelFunc), sem: make(chan struct{}, maxRunning)}
}

func addRevision(t *testing.T, m *Manager) Revision {
	t.Helper()
	now := time.Now().UTC()
	revision := Revision{ID: "revision-1", SnippetID: "snippet-1", Number: 1, Command: "true", CreatedAt: now}
	if err := m.store.Put("job_snippet", "snippet-1", Snippet{ID: "snippet-1", Name: "test", CreatedAt: now, UpdatedAt: now, Revisions: []Revision{revision}}); err != nil {
		t.Fatal(err)
	}
	return revision
}

func waitRun(t *testing.T, m *Manager, id string, want ...string) Run {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var run Run
		if m.store.Get("job_run", id, &run) == nil {
			for _, status := range want {
				if run.Status == status {
					return run
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run %s did not reach one of %v", id, want)
	return Run{}
}

func TestRestartMarksIncompleteRunsUnknownWithoutReplay(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	run := Run{ID: "run-1", HostID: "host-1", RevisionID: "revision-1", Status: "running", StartedAt: time.Now()}
	if err := m.store.Put("job_run", run.ID, run); err != nil {
		t.Fatal(err)
	}
	m.recoverUnknown()
	var got Run
	if err := m.store.Get("job_run", run.ID, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "unknown" || got.FinishedAt == nil {
		t.Fatalf("restart result = %#v", got)
	}
}

func TestGlobalConcurrencyIsFourAndExcessIsSkipped(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	started := make(chan struct{}, maxRunning)
	release := make(chan struct{})
	m.run = func(context.Context, string, string) (int, error) { started <- struct{}{}; <-release; return 0, nil }
	runs := make([]Run, 0, 5)
	for i := 0; i < 5; i++ {
		run, err := m.submit(context.Background(), Run{HostID: string(rune('a' + i)), RevisionID: "revision-1"})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	for i := 0; i < maxRunning; i++ {
		<-started
	}
	deadline := time.Now().Add(2 * time.Second)
	skipped := 0
	for time.Now().Before(deadline) {
		skipped = 0
		for _, run := range runs {
			var persisted Run
			if m.store.Get("job_run", run.ID, &persisted) == nil && persisted.Status == "skipped_capacity" {
				skipped++
			}
		}
		if skipped == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if skipped != 1 {
		t.Fatalf("skipped capacity count = %d, want 1", skipped)
	}
	close(release)
	for _, run := range runs {
		waitRun(t, m, run.ID, "succeeded", "skipped_capacity")
	}
}

func TestHostOverlapIsRejectedAfterTheFirstRunStarts(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	started := make(chan struct{})
	release := make(chan struct{})
	m.run = func(context.Context, string, string) (int, error) { close(started); <-release; return 0, nil }
	first, err := m.submit(context.Background(), Run{HostID: "host-1", RevisionID: "revision-1", Source: "schedule", Intent: "AUTOAPPROVED"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := m.submit(context.Background(), Run{HostID: "host-1", RevisionID: "revision-1", Source: "schedule", Intent: "AUTOAPPROVED"}); err == nil {
		t.Fatal("overlapping host run was accepted")
	}
	var persisted Run
	if err := m.store.Get("job_run", first.ID, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Intent != "AUTOAPPROVED" || persisted.CommandRevision.Command != "true" {
		t.Fatalf("durable intent missing immutable revision: %#v", persisted)
	}
	close(release)
	waitRun(t, m, first.ID, "succeeded")
}

func TestCancelNeverClaimsRemoteTerminationWithoutConfirmation(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	started := make(chan struct{})
	m.run = func(ctx context.Context, _, _ string) (int, error) { close(started); <-ctx.Done(); return 0, ctx.Err() }
	run, err := m.submit(context.Background(), Run{HostID: "host-1", RevisionID: "revision-1"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	request := newRequest(t, "POST", "/api/v1/jobs/runs/"+run.ID+"/cancel")
	request.SetPathValue("id", run.ID)
	m.cancelRun(newResponse(), request)
	got := waitRun(t, m, run.ID, "termination_unknown")
	if got.Status != "termination_unknown" {
		t.Fatal(got.Status)
	}
}

func TestExplicitOutputRetentionEncryptsAndBoundsTheRecord(t *testing.T) {
	m := newTestManager(t)
	vault, err := core.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m.vault = vault
	revision := addRevision(t, m)
	revision.Retention = Retention{Enabled: true, MaxBytes: 32, MaxRuns: 1}
	var snippet Snippet
	if err := m.store.Get("job_snippet", revision.SnippetID, &snippet); err != nil {
		t.Fatal(err)
	}
	snippet.Revisions[0] = revision
	if err := m.store.Put("job_snippet", snippet.ID, snippet); err != nil {
		t.Fatal(err)
	}
	m.runOutput = func(context.Context, string, string, int) (int, []byte, error) {
		return 0, []byte("private output"), nil
	}
	run, err := m.submit(context.Background(), Run{HostID: "host-1", RevisionID: revision.ID})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, m, run.ID, "succeeded")
	var record RetainedOutput
	if err := m.store.Get("job_output", run.ID, &record); err != nil {
		t.Fatal(err)
	}
	if string(record.Ciphertext) == "private output" {
		t.Fatal("plaintext output was stored")
	}
	plain, err := vault.Open(record.Ciphertext)
	if err != nil || string(plain) != "private output" {
		t.Fatalf("encrypted output = %q, %v", plain, err)
	}
}

func newRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type response struct{ header http.Header }

func newResponse() *response                    { return &response{header: make(http.Header)} }
func (r *response) Header() http.Header         { return r.header }
func (r *response) Write(b []byte) (int, error) { return len(b), nil }
func (r *response) WriteHeader(int)             {}
