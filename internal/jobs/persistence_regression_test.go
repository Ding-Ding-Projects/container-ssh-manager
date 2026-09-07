package jobs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestHTTPScheduleChangesRetireObsoleteWatermarks(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "* * * * *")
	tick := time.Date(2026, 9, 7, 18, 0, 0, 0, time.UTC)
	set := frozenClock(m, tick.Add(-time.Minute))
	initialize(t, m)
	m.run = func(context.Context, string, string) (int, error) { return 0, nil }
	server := jobsHTTP(t, m)
	if err := m.store.Put("job_occurrence", "schedule-1-other:a", occurrence{Minute: tick}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		at := tick.Add(time.Duration(i) * time.Minute)
		set(at)
		status, body := requestJSON(t, server, "PUT", "/api/v1/jobs/schedules/schedule-1", Schedule{HostIDs: []string{fmt.Sprintf("host-%d", i)}, SnippetID: "snippet-1", RevisionID: "revision-1", Cron: "* * * * *", Timezone: timezone, Enabled: true})
		if status != 200 {
			t.Fatalf("update %d: %s", status, body)
		}
		m.runDue(context.Background(), at)
		waitRun(t, m, runCount(t, m, i+1)[0].ID, "succeeded")
		claims, err := m.store.List("job_occurrence")
		if err != nil {
			t.Fatal(err)
		}
		if len(claims) != 2 {
			t.Fatalf("obsolete watermark accumulated: %d", len(claims))
		}
	}
	status, _ := requestJSON(t, server, "DELETE", "/api/v1/jobs/schedules/schedule-1", nil)
	if status != 204 {
		t.Fatal(status)
	}
	claims, err := m.store.List("job_occurrence")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatal("deletion retained obsolete claims or removed another schedule's claim")
	}
	var other occurrence
	if err := m.store.Get("job_occurrence", "schedule-1-other:a", &other); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPScheduleClaimRemovalFailureRollsBackScheduleMutation(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "* * * * *")
	if err := m.store.Put("job_occurrence", "schedule-1:a", occurrence{Configuration: "old"}); err != nil {
		t.Fatal(err)
	}
	_, err := m.store.DB.Exec(`CREATE TRIGGER reject_claim_removal BEFORE DELETE ON records WHEN OLD.kind='job_occurrence' BEGIN SELECT RAISE(ABORT,'fixture claim retirement failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	server := jobsHTTP(t, m)
	status, _ := requestJSON(t, server, "PUT", "/api/v1/jobs/schedules/schedule-1", Schedule{HostIDs: []string{"a"}, SnippetID: "snippet-1", RevisionID: "revision-1", Cron: "* * * * *", Timezone: "Pacific/Honolulu", Enabled: true})
	if status != 500 {
		t.Fatal(status)
	}
	var schedule Schedule
	if err := m.store.Get("job_schedule", "schedule-1", &schedule); err != nil {
		t.Fatal(err)
	}
	if schedule.Timezone != timezone {
		t.Fatal("schedule edit survived failed claim retirement")
	}
	status, _ = requestJSON(t, server, "DELETE", "/api/v1/jobs/schedules/schedule-1", nil)
	if status != 503 {
		t.Fatal(status)
	}
	if err := m.store.Get("job_schedule", "schedule-1", &schedule); err != nil {
		t.Fatal("schedule deletion survived failed claim retirement")
	}
	claims, err := m.store.List("job_occurrence")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatal("original claim lost on rollback")
	}
}

func TestHTTPRetainedCiphertextCannotBeReassignedToAnotherRun(t *testing.T) {
	m := outputManager(t, 32, 10)
	m.runOutput = func(_ context.Context, host, _ string, _ int) (int, []byte, error) {
		return 0, []byte("private output for " + host), nil
	}
	server := jobsHTTP(t, m)
	first := postRuns(t, server, "a")[0]
	waitRun(t, m, first.ID, "succeeded")
	second := postRuns(t, server, "b")[0]
	waitRun(t, m, second.ID, "succeeded")
	var from, to RetainedOutput
	if err := m.store.Get("job_output", first.ID, &from); err != nil {
		t.Fatal(err)
	}
	if err := m.store.Get("job_output", second.ID, &to); err != nil {
		t.Fatal(err)
	}
	to.Ciphertext = from.Ciphertext
	if err := m.store.Put("job_output", second.ID, to); err != nil {
		t.Fatal(err)
	}
	status, _ := requestJSON(t, server, "GET", "/api/v1/jobs/runs/"+second.ID+"/output", nil)
	if status != 500 {
		t.Fatal("valid ciphertext was accepted under the wrong run identity")
	}
}

func TestHTTPAcceptanceStatusDistinguishesValidationMissingAndUnavailable(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	server := jobsHTTP(t, m)
	for _, timeout := range []int{-1, 601} {
		status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostID: "a", RevisionID: "revision-1", TimeoutSeconds: timeout})
		if status != 400 {
			t.Fatalf("invalid timeout %d status %d", timeout, status)
		}
	}
	status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostID: "a", RevisionID: "missing"})
	if status != 404 {
		t.Fatalf("missing revision status %d", status)
	}
	m.Close()
	status, _ = requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostID: "a", RevisionID: "revision-1"})
	if status != 503 {
		t.Fatalf("stopped manager status %d", status)
	}
}

func waitDegraded(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		degraded := m.initErr != nil
		m.mu.Unlock()
		if degraded {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("manager did not stop dispatching after persistence failure")
}

func TestTerminalAuditFailureRollsBackOutcomeAndStopsDispatch(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	started := make(chan string, 5)
	releaseFirst := make(chan struct{})
	m.run = func(ctx context.Context, host, _ string) (int, error) {
		started <- host
		if host == "a" {
			select {
			case <-releaseFirst:
				return 0, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	server := jobsHTTP(t, m)
	runs := postRuns(t, server, "a", "b", "c", "d", "queued")
	for i := 0; i < 4; i++ {
		<-started
	}
	_, err := m.store.DB.Exec(`CREATE TRIGGER reject_finish_audit BEFORE INSERT ON records WHEN NEW.kind='job_audit' AND json_extract(NEW.data,'$.action')='run_finished' BEGIN SELECT RAISE(ABORT,'fixture outcome audit failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	waitDegraded(t, m)
	var durable Run
	if err := m.store.Get("job_run", runs[0].ID, &durable); err != nil {
		t.Fatal(err)
	}
	if durable.Status != "running" || durable.FinishedAt != nil {
		t.Fatal("outcome survived rollback without audit")
	}
	waitRun(t, m, runs[4].ID, "queued")
	status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostID: "new", RevisionID: "revision-1"})
	if status != 503 {
		t.Fatalf("degraded manager accepted new work: %d", status)
	}
	select {
	case host := <-started:
		t.Fatalf("new connection opened after persistence failure: %s", host)
	default:
	}
}

func TestRecoveryAuditFailurePreservesOriginalIntentAndRefusesAcceptance(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	if err := m.store.Put("job_run", "pending", Run{ID: "pending", HostID: "a", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	_, err := m.store.DB.Exec(`CREATE TRIGGER reject_recovery_audit BEFORE INSERT ON records WHEN NEW.kind='job_audit' AND json_extract(NEW.data,'$.action')='run_recovered_unknown' BEGIN SELECT RAISE(ABORT,'fixture recovery audit failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	server := jobsHTTP(t, m)
	status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostID: "b", RevisionID: "revision-1"})
	if status != 503 {
		t.Fatal(status)
	}
	waitRun(t, m, "pending", "queued")
	runCount(t, m, 1)
}

func TestSkippedAuditFailureRollsBackOccurrenceClaim(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "* * * * *")
	tick := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	set := frozenClock(m, tick.Add(-time.Minute))
	initialize(t, m)
	m.run = func(ctx context.Context, _, _ string) (int, error) { <-ctx.Done(); return 0, ctx.Err() }
	server := jobsHTTP(t, m)
	postRuns(t, server, "a")
	_, err := m.store.DB.Exec(`CREATE TRIGGER reject_skip_audit BEFORE INSERT ON records WHEN NEW.kind='job_audit' AND json_extract(NEW.data,'$.action')='schedule_skipped' BEGIN SELECT RAISE(ABORT,'fixture skip audit failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	set(tick)
	m.runDue(context.Background(), tick)
	waitDegraded(t, m)
	claims, err := m.store.List("job_occurrence")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatal("skip claim survived without its audit evidence")
	}
	runCount(t, m, 1)
}

func TestHTTPScheduleTimezoneEditDoesNotSuppressNewOccurrences(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "* * * * *")
	tick := time.Date(2026, 9, 7, 18, 0, 0, 0, time.UTC)
	set := frozenClock(m, tick.Add(-time.Minute))
	initialize(t, m)
	m.run = func(context.Context, string, string) (int, error) { return 0, nil }
	set(tick)
	m.runDue(context.Background(), tick)
	waitRun(t, m, runCount(t, m, 1)[0].ID, "succeeded")
	next := tick.Add(time.Minute)
	set(next)
	server := jobsHTTP(t, m)
	status, body := requestJSON(t, server, "PUT", "/api/v1/jobs/schedules/schedule-1", Schedule{HostIDs: []string{"a"}, SnippetID: "snippet-1", RevisionID: "revision-1", Cron: "* * * * *", Timezone: "Pacific/Honolulu", Enabled: true})
	if status != 200 {
		t.Fatalf("schedule edit %d: %s", status, body)
	}
	m.runDue(context.Background(), next)
	waitRun(t, m, runCount(t, m, 2)[0].ID, "succeeded")
}
