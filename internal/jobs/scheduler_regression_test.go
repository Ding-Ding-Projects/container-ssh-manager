package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func frozenClock(m *Manager, initial time.Time) func(time.Time) {
	var clock atomic.Int64
	clock.Store(initial.UnixNano())
	m.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	return func(at time.Time) { clock.Store(at.UnixNano()) }
}

func initialize(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initializeLocked()
	if m.initErr != nil {
		t.Fatal(m.initErr)
	}
}

func storeSchedule(t *testing.T, m *Manager, expression string) {
	t.Helper()
	schedule := Schedule{ID: "schedule-1", HostIDs: []string{"a"}, SnippetID: "snippet-1", RevisionID: "revision-1", Cron: expression, Timezone: timezone, Enabled: true}
	if err := m.store.Put("job_schedule", schedule.ID, schedule); err != nil {
		t.Fatal(err)
	}
}

func runCount(t *testing.T, m *Manager, want int) []Run {
	t.Helper()
	runs, err := m.runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != want {
		t.Fatalf("run count %d, want %d", len(runs), want)
	}
	return runs
}

func TestSchedulerPersistsOccurrenceAndSkipsDuplicateTicksAndClockRollback(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "* * * * *")
	tick := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	set := frozenClock(m, tick.Add(-time.Minute))
	initialize(t, m)
	m.run = func(context.Context, string, string) (int, error) { return 0, nil }
	set(tick)
	m.runDue(context.Background(), tick)
	first := runCount(t, m, 1)[0]
	waitRun(t, m, first.ID, "succeeded")
	if first.Source != "schedule" || first.Intent != "AUTOAPPROVED" || first.CommandRevision.Command != "true" {
		t.Fatal("scheduled immutable intent missing")
	}
	m.runDue(context.Background(), tick)
	runCount(t, m, 1)
	set(tick.Add(-time.Minute))
	m.runDue(context.Background(), tick.Add(-time.Minute))
	runCount(t, m, 1)
	m.Close()
	m.wg.Wait()
	// Simulate a replacement manager with a startup watermark earlier than the
	// saved claim. This isolates the persisted deduplication boundary from Start.
	replacement := New(m.store, nil, nil)
	t.Cleanup(func() { replacement.Close(); replacement.wg.Wait() })
	setReplacement := frozenClock(replacement, tick.Add(-time.Minute))
	initialize(t, replacement)
	replacement.run = func(context.Context, string, string) (int, error) { return 0, nil }
	setReplacement(tick)
	replacement.runDue(context.Background(), tick)
	runCount(t, replacement, 1)
	setReplacement(tick.Add(time.Minute))
	replacement.runDue(context.Background(), tick.Add(time.Minute))
	second := runCount(t, replacement, 2)[0]
	waitRun(t, replacement, second.ID, "succeeded")
}

func TestSchedulerSkipsRepeatedFallBackWallMinute(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "30 1 * * *")
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	set := frozenClock(m, first.Add(-time.Minute))
	initialize(t, m)
	m.run = func(context.Context, string, string) (int, error) { return 0, nil }
	set(first)
	m.runDue(context.Background(), first)
	waitRun(t, m, runCount(t, m, 1)[0].ID, "succeeded")
	second := first.Add(time.Hour)
	set(second)
	m.runDue(context.Background(), second)
	runCount(t, m, 1)
	nextDay := second.Add(24 * time.Hour)
	set(nextDay)
	m.runDue(context.Background(), nextDay)
	waitRun(t, m, runCount(t, m, 2)[0].ID, "succeeded")
}

func TestSchedulerStartupAndMissedTicksNeverExecute(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "* * * * *")
	tick := time.Date(2026, 9, 7, 13, 0, 30, 0, time.UTC)
	set := frozenClock(m, tick)
	m.run = func(context.Context, string, string) (int, error) {
		t.Error("startup or missed occurrence executed")
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Start(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		ready := m.initialized
		m.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("scheduler did not initialize")
		}
		time.Sleep(time.Millisecond)
	}
	m.runDue(context.Background(), tick.Truncate(time.Minute))
	runCount(t, m, 0)
	set(tick.Add(10 * time.Minute))
	m.runDue(context.Background(), tick.Add(5*time.Minute))
	runCount(t, m, 0)
	cancel()
	<-done
}

func TestSchedulerOverlapIsClaimedAndNeverReplayed(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	storeSchedule(t, m, "* * * * *")
	tick := time.Date(2026, 9, 7, 13, 0, 0, 0, time.UTC)
	set := frozenClock(m, tick.Add(-time.Minute))
	initialize(t, m)
	release := make(chan struct{})
	m.run = func(ctx context.Context, _, _ string) (int, error) {
		select {
		case <-release:
			return 0, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	server := jobsHTTP(t, m)
	run := postRuns(t, server, "a")[0]
	set(tick)
	m.runDue(context.Background(), tick)
	runCount(t, m, 1)
	close(release)
	waitRun(t, m, run.ID, "succeeded")
	m.runDue(context.Background(), tick)
	runCount(t, m, 1)
}

func TestHTTPImmediateCancellationAlwaysFindsAcceptedReservation(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	m.run = func(ctx context.Context, _, _ string) (int, error) { <-ctx.Done(); return 0, ctx.Err() }
	server := jobsHTTP(t, m)
	runs := postRuns(t, server, "a", "b", "c", "d", "e", "f")
	for _, run := range runs {
		status, _ := requestJSON(t, server, "POST", "/api/v1/jobs/runs/"+run.ID+"/cancel", nil)
		if status != 202 {
			t.Fatalf("accepted run cancellation returned %d", status)
		}
		waitRun(t, m, run.ID, "cancelled", "termination_unknown")
	}
}

func TestHTTPTimeoutDoesNotClaimConfirmedTermination(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	m.run = func(ctx context.Context, _, _ string) (int, error) { <-ctx.Done(); return 0, ctx.Err() }
	server := jobsHTTP(t, m)
	status, body := requestJSON(t, server, "POST", "/api/v1/jobs/runs", runRequest{HostID: "a", RevisionID: "revision-1", TimeoutSeconds: 1})
	var response struct {
		Runs []Run `json:"runs"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if status != 202 || len(response.Runs) != 1 {
		t.Fatalf("POST %d: %s", status, body)
	}
	run := waitRun(t, m, response.Runs[0].ID, "termination_unknown")
	if run.ExitCode != nil || run.Error != "timeout reached; remote process termination could not be confirmed" {
		t.Fatalf("timeout evidence %#v", run)
	}
}

func TestExecutionErrorNeverPersistsUnretainedOutput(t *testing.T) {
	m := newTestManager(t)
	addRevision(t, m)
	m.run = func(context.Context, string, string) (int, error) { return 0, errors.New("fixture private output") }
	server := jobsHTTP(t, m)
	run := postRuns(t, server, "a")[0]
	finished := waitRun(t, m, run.ID, "failed")
	if finished.Error != "command execution or output retention failed" {
		t.Fatal("runner error was exposed")
	}
	var count int
	if err := m.store.DB.QueryRow(`SELECT count(*) FROM records WHERE data LIKE '%fixture private output%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unretained output persisted")
	}
}

func TestRestartMarksQueuedIntentUnknownWithoutExecuting(t *testing.T) {
	m := newTestManager(t)
	if err := m.store.Put("job_run", "queued-before-restart", Run{ID: "queued-before-restart", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	initialize(t, m)
	waitRun(t, m, "queued-before-restart", "unknown")
	if len(m.queue) != 0 || len(m.active) != 0 {
		t.Fatal("recovery replayed queued work")
	}
}
