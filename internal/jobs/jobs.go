// Package jobs owns durable, unattended command scheduling.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/connection"
	"github.com/Ding-Ding-Projects/container-ssh-manager/internal/core"
)

const (
	defaultTimeout = 10 * time.Minute
	maxRunning     = 4
	timezone       = "America/Toronto"
)

type Retention struct {
	Enabled  bool `json:"enabled"`
	MaxBytes int  `json:"maxBytes,omitempty"`
	MaxRuns  int  `json:"maxRuns,omitempty"`
}

type Revision struct {
	ID        string    `json:"id"`
	SnippetID string    `json:"snippetId"`
	Number    int       `json:"number"`
	Command   string    `json:"command"`
	Retention Retention `json:"retention"`
	CreatedAt time.Time `json:"createdAt"`
}

type Snippet struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Revisions []Revision `json:"revisions"`
}

type Schedule struct {
	ID         string    `json:"id"`
	HostID     string    `json:"hostId"`
	HostIDs    []string  `json:"hostIds"`
	SnippetID  string    `json:"snippetId"`
	RevisionID string    `json:"revisionId"`
	Cron       string    `json:"cron"`
	Timezone   string    `json:"timezone"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type Run struct {
	ID              string     `json:"id"`
	HostID          string     `json:"hostId"`
	HostIDs         []string   `json:"hostIds,omitempty"`
	ScheduleID      string     `json:"scheduleId,omitempty"`
	RevisionID      string     `json:"revisionId"`
	Source          string     `json:"source"`
	Intent          string     `json:"intent"`
	CommandRevision Revision   `json:"commandRevision"`
	TimeoutSeconds  int        `json:"timeoutSeconds"`
	Status          string     `json:"status"`
	ExitCode        *int       `json:"exitCode,omitempty"`
	Error           string     `json:"error,omitempty"`
	StartedAt       time.Time  `json:"startedAt"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	CancelRequested bool       `json:"cancelRequested,omitempty"`
}

type AuditEvent struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	HostID     string    `json:"hostId,omitempty"`
	ScheduleID string    `json:"scheduleId,omitempty"`
	RevisionID string    `json:"revisionId,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	Result     string    `json:"result,omitempty"`
	Metadata   any       `json:"metadata,omitempty"`
}

type RetainedOutput struct {
	RunID      string    `json:"runId"`
	Ciphertext []byte    `json:"ciphertext"`
	MaxBytes   int       `json:"maxBytes"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Manager struct {
	store       *core.Store
	connections *connection.Manager
	vault       *core.Vault
	location    *time.Location
	now         func() time.Time

	mu        sync.Mutex
	running   map[string]context.CancelFunc
	sem       chan struct{}
	run       func(context.Context, string, string) (int, error)
	runOutput func(context.Context, string, string, int) (int, []byte, error)
}

func New(store *core.Store, connections *connection.Manager, vault *core.Vault) *Manager {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.FixedZone(timezone, -5*60*60)
	}
	m := &Manager{store: store, connections: connections, vault: vault, location: loc, now: time.Now, running: make(map[string]context.CancelFunc), sem: make(chan struct{}, maxRunning)}
	m.run = func(ctx context.Context, hostID, command string) (int, error) {
		return connections.Run(ctx, hostID, command)
	}
	m.runOutput = func(ctx context.Context, hostID, command string, limit int) (int, []byte, error) {
		return connections.RunWithOutput(ctx, hostID, command, limit)
	}
	return m
}

func (m *Manager) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/jobs/snippets", m.listSnippets)
	mux.HandleFunc("POST /api/v1/jobs/snippets", m.createSnippet)
	mux.HandleFunc("GET /api/v1/jobs/snippets/{id}", m.getSnippet)
	mux.HandleFunc("PUT /api/v1/jobs/snippets/{id}", m.updateSnippet)
	mux.HandleFunc("DELETE /api/v1/jobs/snippets/{id}", m.deleteSnippet)
	mux.HandleFunc("GET /api/v1/jobs/schedules", m.listSchedules)
	mux.HandleFunc("POST /api/v1/jobs/schedules", m.createSchedule)
	mux.HandleFunc("GET /api/v1/jobs/schedules/{id}", m.getSchedule)
	mux.HandleFunc("PUT /api/v1/jobs/schedules/{id}", m.updateSchedule)
	mux.HandleFunc("DELETE /api/v1/jobs/schedules/{id}", m.deleteSchedule)
	mux.HandleFunc("POST /api/v1/jobs/schedules/preview", m.previewSchedule)
	mux.HandleFunc("GET /api/v1/jobs/runs", m.listRuns)
	mux.HandleFunc("POST /api/v1/jobs/runs", m.createRun)
	mux.HandleFunc("GET /api/v1/jobs/runs/{id}", m.getRun)
	mux.HandleFunc("POST /api/v1/jobs/runs/{id}/cancel", m.cancelRun)
	mux.HandleFunc("GET /api/v1/jobs/audit", m.listAudit)
}

// Start recovers incomplete runs and checks schedules once per minute. It deliberately never
// backfills a missed tick, which prevents restart from replaying an unknown command.
func (m *Manager) Start(ctx context.Context) {
	m.recoverUnknown()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		m.runDue(ctx, m.now().In(m.location).Truncate(time.Minute))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) runDue(ctx context.Context, now time.Time) {
	schedules, err := m.schedules()
	if err != nil {
		return
	}
	for _, schedule := range schedules {
		if !schedule.Enabled {
			continue
		}
		loc, err := scheduleLocation(schedule.Timezone)
		if err != nil {
			continue
		}
		spec, err := parseCron(schedule.Cron)
		if err != nil || !spec.Match(now.In(loc)) {
			continue
		}
		for _, hostID := range scheduleHosts(schedule) {
			if m.hostRunning(hostID) {
				m.audit("system", "schedule_skipped_overlap", hostID, schedule.ID, schedule.RevisionID, "skipped", nil)
				continue
			}
			if revision, err := m.revision(schedule.RevisionID); err == nil {
				_, _ = m.submit(ctx, Run{HostID: hostID, ScheduleID: schedule.ID, RevisionID: revision.ID, Source: "schedule", Intent: "AUTOAPPROVED", TimeoutSeconds: int(defaultTimeout.Seconds())})
			}
		}
	}
}

func (m *Manager) submit(parent context.Context, run Run) (Run, error) {
	if strings.TrimSpace(run.HostID) == "" || strings.TrimSpace(run.RevisionID) == "" {
		return Run{}, errors.New("hostId and revisionId are required")
	}
	if run.TimeoutSeconds == 0 {
		run.TimeoutSeconds = int(defaultTimeout.Seconds())
	}
	if run.TimeoutSeconds < 1 || run.TimeoutSeconds > int(defaultTimeout.Seconds()) {
		return Run{}, errors.New("timeoutSeconds must be between 1 and 600")
	}
	revision, err := m.revision(run.RevisionID)
	if err != nil {
		return Run{}, errors.New("revision not found")
	}
	run.CommandRevision = revision
	if m.hostRunning(run.HostID) {
		return Run{}, errors.New("a job is already running on this host")
	}
	run.ID, run.Status, run.StartedAt = core.ID(), "queued", m.now().UTC()
	if run.Source == "" {
		run.Source = "manual"
	}
	if run.Intent == "" {
		run.Intent = "USER_APPROVED"
	}
	if err := m.store.Put("job_run", run.ID, run); err != nil {
		return Run{}, err
	} // durable intent before execution
	m.audit("session", "run_intent_saved", run.HostID, run.ScheduleID, run.RevisionID, "queued", map[string]any{"runId": run.ID, "intent": run.Intent})
	go m.execute(parent, run)
	return run, nil
}

func (m *Manager) execute(parent context.Context, run Run) {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	default:
		run.Status = "skipped_capacity"
		now := m.now().UTC()
		run.FinishedAt = &now
		_ = m.store.Put("job_run", run.ID, run)
		m.audit("system", "run_skipped_capacity", run.HostID, run.ScheduleID, run.RevisionID, run.Status, map[string]any{"runId": run.ID})
		return
	}
	if m.hostRunning(run.HostID) {
		run.Status = "skipped_overlap"
		now := m.now().UTC()
		run.FinishedAt = &now
		_ = m.store.Put("job_run", run.ID, run)
		return
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(run.TimeoutSeconds)*time.Second)
	m.mu.Lock()
	m.running[run.ID] = cancel
	m.mu.Unlock()
	defer func() { cancel(); m.mu.Lock(); delete(m.running, run.ID); m.mu.Unlock() }()
	run.Status = "running"
	_ = m.store.Put("job_run", run.ID, run)
	_, err := m.revision(run.RevisionID)
	if err == nil {
		var code int
		if run.CommandRevision.Retention.Enabled {
			var output []byte
			code, output, err = m.runOutput(ctx, run.HostID, run.CommandRevision.Command, run.CommandRevision.Retention.MaxBytes)
			if err == nil {
				err = m.saveOutput(run.ID, run.RevisionID, output, run.CommandRevision.Retention)
			}
		} else {
			code, err = m.run(ctx, run.HostID, run.CommandRevision.Command)
		}
		run.ExitCode = &code
	}
	now := m.now().UTC()
	run.FinishedAt = &now
	// Cancellation is persisted by the handler while this goroutine may be
	// blocked in the runner. Reload it so the final outcome never claims a
	// confirmed termination based on a stale in-memory copy.
	var latest Run
	if m.store.Get("job_run", run.ID, &latest) == nil && latest.CancelRequested {
		run.CancelRequested = true
	}
	if run.CancelRequested {
		run.Status = "termination_unknown"
		run.Error = "cancellation requested; remote process termination could not be confirmed"
	} else if errors.Is(err, context.DeadlineExceeded) {
		run.Status = "timed_out"
		run.Error = "timeout reached"
	} else if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
	} else if run.ExitCode != nil && *run.ExitCode != 0 {
		run.Status = "failed"
	} else {
		run.Status = "succeeded"
	}
	_ = m.store.Put("job_run", run.ID, run)
	m.audit("system", "run_finished", run.HostID, run.ScheduleID, run.RevisionID, run.Status, map[string]any{"runId": run.ID})
}

func (m *Manager) hostRunning(host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.running {
		var run Run
		if m.store.Get("job_run", id, &run) == nil && run.HostID == host {
			return true
		}
	}
	return false
}

func (m *Manager) recoverUnknown() {
	runs, err := m.runs()
	if err != nil {
		return
	}
	for _, run := range runs {
		if run.Status == "queued" || run.Status == "running" {
			run.Status = "unknown"
			now := m.now().UTC()
			run.FinishedAt = &now
			run.Error = "server restarted; run was not replayed"
			_ = m.store.Put("job_run", run.ID, run)
			m.audit("system", "run_recovered_unknown", run.HostID, run.ScheduleID, run.RevisionID, run.Status, map[string]any{"runId": run.ID})
		}
	}
}

func (m *Manager) snippets() ([]Snippet, error) {
	raws, e := m.store.List("job_snippet")
	if e != nil {
		return nil, e
	}
	out := make([]Snippet, 0, len(raws))
	for _, raw := range raws {
		var v Snippet
		if json.Unmarshal(raw, &v) == nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (m *Manager) schedules() ([]Schedule, error) {
	raws, e := m.store.List("job_schedule")
	if e != nil {
		return nil, e
	}
	out := make([]Schedule, 0, len(raws))
	for _, raw := range raws {
		var v Schedule
		if json.Unmarshal(raw, &v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}
func (m *Manager) runs() ([]Run, error) {
	raws, e := m.store.List("job_run")
	if e != nil {
		return nil, e
	}
	out := make([]Run, 0, len(raws))
	for _, raw := range raws {
		var v Run
		if json.Unmarshal(raw, &v) == nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}
func (m *Manager) revision(id string) (Revision, error) {
	snippets, e := m.snippets()
	if e != nil {
		return Revision{}, e
	}
	for _, s := range snippets {
		for _, r := range s.Revisions {
			if r.ID == id {
				return r, nil
			}
		}
	}
	return Revision{}, errors.New("revision not found")
}
func (m *Manager) audit(actor, action, host, schedule, revision, result string, metadata any) {
	event := AuditEvent{ID: core.ID(), At: m.now().UTC(), Actor: actor, Action: action, HostID: host, ScheduleID: schedule, RevisionID: revision, Result: result, Metadata: metadata}
	_ = m.store.Put("job_audit", event.ID, event)
	// Audit metadata is intentionally retained for 90 days, then deleted rather
	// than merely hidden by the list endpoint.
	raws, err := m.store.List("job_audit")
	if err != nil {
		return
	}
	cutoff := m.now().Add(-90 * 24 * time.Hour)
	for _, raw := range raws {
		var old AuditEvent
		if json.Unmarshal(raw, &old) == nil && old.At.Before(cutoff) {
			_ = m.store.Delete("job_audit", old.ID)
		}
	}
}

func decode(r *http.Request, v any) error                    { defer r.Body.Close(); return core.Decode(r, v) }
func write(w http.ResponseWriter, status int, v any)         { core.JSON(w, status, v) }
func fail(w http.ResponseWriter, status int, message string) { core.Error(w, status, message) }

func (m *Manager) listSnippets(w http.ResponseWriter, r *http.Request) {
	v, e := m.snippets()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	write(w, 200, v)
}
func (m *Manager) getSnippet(w http.ResponseWriter, r *http.Request) {
	var v Snippet
	if e := m.store.Get("job_snippet", r.PathValue("id"), &v); e != nil {
		fail(w, 404, "snippet not found")
		return
	}
	write(w, 200, v)
}
func validRetention(v Retention) error {
	if !v.Enabled {
		return nil
	}
	if v.MaxBytes < 1 || v.MaxBytes > 1<<20 || v.MaxRuns < 1 || v.MaxRuns > 1000 {
		return errors.New("retention maxBytes must be 1..1048576 and maxRuns must be 1..1000")
	}
	return nil
}

func (m *Manager) saveOutput(runID, revisionID string, plaintext []byte, retention Retention) error {
	if len(plaintext) > retention.MaxBytes {
		return errors.New("command output exceeds retention limit")
	}
	if m.vault == nil {
		return errors.New("output retention vault unavailable")
	}
	ciphertext, err := m.vault.Seal(plaintext)
	if err != nil {
		return err
	}
	if err = m.store.Put("job_output", runID, RetainedOutput{RunID: runID, Ciphertext: ciphertext, MaxBytes: retention.MaxBytes, CreatedAt: m.now().UTC()}); err != nil {
		return err
	}
	runs, err := m.runs()
	if err != nil {
		return err
	}
	retained := make([]Run, 0, retention.MaxRuns)
	for _, run := range runs {
		if run.RevisionID == revisionID {
			var record RetainedOutput
			if m.store.Get("job_output", run.ID, &record) == nil {
				retained = append(retained, run)
			}
		}
	}
	for index := retention.MaxRuns; index < len(retained); index++ {
		_ = m.store.Delete("job_output", retained[index].ID)
	}
	return nil
}
func (m *Manager) createSnippet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name, Command string
		Retention     Retention `json:"retention"`
	}
	if e := decode(r, &in); e != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Command) == "" {
		fail(w, 400, "name and command are required")
		return
	}
	if e := validRetention(in.Retention); e != nil {
		fail(w, 409, e.Error())
		return
	}
	now := m.now().UTC()
	s := Snippet{ID: core.ID(), Name: in.Name, CreatedAt: now, UpdatedAt: now}
	s.Revisions = []Revision{{ID: core.ID(), SnippetID: s.ID, Number: 1, Command: in.Command, Retention: in.Retention, CreatedAt: now}}
	if e := m.store.Put("job_snippet", s.ID, s); e != nil {
		fail(w, 500, e.Error())
		return
	}
	m.audit("session", "snippet_created", "", "", s.Revisions[0].ID, "created", map[string]any{"snippetId": s.ID})
	write(w, 201, s)
}
func (m *Manager) updateSnippet(w http.ResponseWriter, r *http.Request) {
	var s Snippet
	if e := m.store.Get("job_snippet", r.PathValue("id"), &s); e != nil {
		fail(w, 404, "snippet not found")
		return
	}
	var in struct {
		Name, Command string
		Retention     Retention `json:"retention"`
	}
	if e := decode(r, &in); e != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Command) == "" {
		fail(w, 400, "name and command are required")
		return
	}
	if e := validRetention(in.Retention); e != nil {
		fail(w, 409, e.Error())
		return
	}
	now := m.now().UTC()
	s.Name = in.Name
	s.UpdatedAt = now
	s.Revisions = append(s.Revisions, Revision{ID: core.ID(), SnippetID: s.ID, Number: len(s.Revisions) + 1, Command: in.Command, Retention: in.Retention, CreatedAt: now})
	if e := m.store.Put("job_snippet", s.ID, s); e != nil {
		fail(w, 500, e.Error())
		return
	}
	m.audit("session", "snippet_revised", "", "", s.Revisions[len(s.Revisions)-1].ID, "created", map[string]any{"snippetId": s.ID})
	write(w, 200, s)
}
func (m *Manager) deleteSnippet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ss, _ := m.schedules()
	for _, s := range ss {
		if s.SnippetID == id {
			fail(w, 409, "snippet is referenced by a schedule")
			return
		}
	}
	if e := m.store.Delete("job_snippet", id); e != nil {
		fail(w, 404, "snippet not found")
		return
	}
	m.audit("session", "snippet_deleted", "", "", "", "deleted", map[string]any{"snippetId": id})
	w.WriteHeader(204)
}

func (m *Manager) listSchedules(w http.ResponseWriter, r *http.Request) {
	v, e := m.schedules()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	write(w, 200, v)
}
func (m *Manager) getSchedule(w http.ResponseWriter, r *http.Request) {
	var v Schedule
	if e := m.store.Get("job_schedule", r.PathValue("id"), &v); e != nil {
		fail(w, 404, "schedule not found")
		return
	}
	write(w, 200, v)
}
func (m *Manager) scheduleInput(r *http.Request) (Schedule, error) {
	var in Schedule
	if e := decode(r, &in); e != nil {
		return Schedule{}, errors.New("invalid JSON")
	}
	if len(in.HostIDs) == 0 && in.HostID != "" {
		in.HostIDs = []string{in.HostID}
	}
	in.HostIDs = uniqueHosts(in.HostIDs)
	if len(in.HostIDs) == 0 || strings.TrimSpace(in.SnippetID) == "" || strings.TrimSpace(in.RevisionID) == "" {
		return Schedule{}, errors.New("hostIds, snippetId, and revisionId are required")
	}
	in.HostID = in.HostIDs[0]
	if in.Timezone == "" {
		in.Timezone = timezone
	}
	if _, err := scheduleLocation(in.Timezone); err != nil {
		return Schedule{}, err
	}
	rev, e := m.revision(in.RevisionID)
	if e != nil || rev.SnippetID != in.SnippetID {
		return Schedule{}, errors.New("revision must belong to snippet")
	}
	if _, e = parseCron(in.Cron); e != nil {
		return Schedule{}, e
	}
	return in, nil
}
func (m *Manager) createSchedule(w http.ResponseWriter, r *http.Request) {
	s, e := m.scheduleInput(r)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	now := m.now().UTC()
	s.ID = core.ID()
	s.CreatedAt = now
	s.UpdatedAt = now
	if e = m.store.Put("job_schedule", s.ID, s); e != nil {
		fail(w, 500, e.Error())
		return
	}
	m.audit("session", "schedule_created", s.HostID, s.ID, s.RevisionID, "created", nil)
	write(w, 201, s)
}
func (m *Manager) updateSchedule(w http.ResponseWriter, r *http.Request) {
	s, e := m.scheduleInput(r)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	id := r.PathValue("id")
	var old Schedule
	if e = m.store.Get("job_schedule", id, &old); e != nil {
		fail(w, 404, "schedule not found")
		return
	}
	s.ID = id
	s.CreatedAt = old.CreatedAt
	s.UpdatedAt = m.now().UTC()
	if e = m.store.Put("job_schedule", id, s); e != nil {
		fail(w, 500, e.Error())
		return
	}
	m.audit("session", "schedule_updated", s.HostID, s.ID, s.RevisionID, "updated", nil)
	write(w, 200, s)
}
func (m *Manager) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := m.store.Delete("job_schedule", id); e != nil {
		fail(w, 404, "schedule not found")
		return
	}
	m.audit("session", "schedule_deleted", "", id, "", "deleted", nil)
	w.WriteHeader(204)
}
func (m *Manager) previewSchedule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Cron     string    `json:"cron"`
		After    time.Time `json:"after"`
		Count    int       `json:"count"`
		Timezone string    `json:"timezone"`
	}
	if e := decode(r, &in); e != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	spec, e := parseCron(in.Cron)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	if in.Timezone == "" {
		in.Timezone = timezone
	}
	loc, err := scheduleLocation(in.Timezone)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if in.Count == 0 {
		in.Count = 5
	}
	if in.Count < 1 || in.Count > 20 {
		fail(w, 400, "count must be 1..20")
		return
	}
	if in.After.IsZero() {
		in.After = m.now()
	}
	times := make([]time.Time, 0, in.Count)
	cursor := in.After.In(loc).Truncate(time.Minute)
	for len(times) < in.Count {
		cursor = cursor.Add(time.Minute)
		if spec.Match(cursor) {
			times = append(times, cursor)
		}
	}
	write(w, 200, map[string]any{"timezone": in.Timezone, "times": times})
}

func (m *Manager) listRuns(w http.ResponseWriter, r *http.Request) {
	v, e := m.runs()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	host, schedule := r.URL.Query().Get("hostId"), r.URL.Query().Get("scheduleId")
	out := v[:0]
	for _, run := range v {
		if (host == "" || run.HostID == host) && (schedule == "" || run.ScheduleID == schedule) {
			out = append(out, run)
		}
	}
	write(w, 200, out)
}
func (m *Manager) getRun(w http.ResponseWriter, r *http.Request) {
	var v Run
	if e := m.store.Get("job_run", r.PathValue("id"), &v); e != nil {
		fail(w, 404, "run not found")
		return
	}
	write(w, 200, v)
}
func (m *Manager) createRun(w http.ResponseWriter, r *http.Request) {
	var in Run
	if e := decode(r, &in); e != nil {
		fail(w, 400, "invalid JSON")
		return
	}
	if len(in.HostIDs) == 0 && in.HostID != "" {
		in.HostIDs = []string{in.HostID}
	}
	hosts := uniqueHosts(in.HostIDs)
	if len(hosts) == 0 {
		fail(w, 400, "hostIds are required")
		return
	}
	runs := make([]Run, 0, len(hosts))
	for _, hostID := range hosts {
		candidate := in
		candidate.HostID = hostID
		candidate.HostIDs = nil
		run, e := m.submit(r.Context(), candidate)
		if e != nil {
			fail(w, 409, e.Error())
			return
		}
		runs = append(runs, run)
	}
	write(w, 202, map[string]any{"runs": runs})
}

func uniqueHosts(hosts []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host != "" && !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out
}
func scheduleHosts(schedule Schedule) []string {
	if len(schedule.HostIDs) > 0 {
		return uniqueHosts(schedule.HostIDs)
	}
	return uniqueHosts([]string{schedule.HostID})
}
func scheduleLocation(name string) (*time.Location, error) {
	if name == "" {
		name = timezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errors.New("invalid timezone")
	}
	return loc, nil
}
func (m *Manager) cancelRun(w http.ResponseWriter, r *http.Request) {
	var run Run
	if e := m.store.Get("job_run", r.PathValue("id"), &run); e != nil {
		fail(w, 404, "run not found")
		return
	}
	m.mu.Lock()
	cancel, ok := m.running[run.ID]
	m.mu.Unlock()
	if !ok {
		fail(w, 409, "run is not active")
		return
	}
	run.CancelRequested = true
	_ = m.store.Put("job_run", run.ID, run)
	cancel()
	m.audit("session", "run_cancel_requested", run.HostID, run.ScheduleID, run.RevisionID, "termination_unknown", map[string]any{"runId": run.ID})
	write(w, 202, map[string]string{"status": "termination_unknown"})
}
func (m *Manager) listAudit(w http.ResponseWriter, r *http.Request) {
	raws, e := m.store.List("job_audit")
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	out := make([]AuditEvent, 0, len(raws))
	for _, raw := range raws {
		var event AuditEvent
		if json.Unmarshal(raw, &event) == nil && event.At.After(m.now().Add(-90*24*time.Hour)) {
			out = append(out, event)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	limit := 100
	if text := r.URL.Query().Get("limit"); text != "" {
		if value, err := strconv.Atoi(text); err == nil && value > 0 && value <= 1000 {
			limit = value
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	write(w, 200, out)
}

type cronSpec struct{ fields [5]map[int]bool }

func parseCron(text string) (cronSpec, error) {
	parts := strings.Fields(text)
	if len(parts) != 5 {
		return cronSpec{}, errors.New("cron must have five fields")
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	var spec cronSpec
	for i, part := range parts {
		values, e := cronField(part, bounds[i][0], bounds[i][1])
		if e != nil {
			return cronSpec{}, fmt.Errorf("cron field %d: %w", i+1, e)
		}
		spec.fields[i] = values
	}
	return spec, nil
}
func cronField(text string, min, max int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, segment := range strings.Split(text, ",") {
		base, stepText, hasStep := strings.Cut(segment, "/")
		step := 1
		if hasStep {
			value, e := strconv.Atoi(stepText)
			if e != nil || value < 1 {
				return nil, errors.New("invalid step")
			}
			step = value
		}
		start, end := min, max
		if base != "*" {
			if strings.Contains(base, "-") {
				parts := strings.Split(base, "-")
				if len(parts) != 2 {
					return nil, errors.New("invalid range")
				}
				a, e := strconv.Atoi(parts[0])
				if e != nil {
					return nil, errors.New("invalid range")
				}
				b, e := strconv.Atoi(parts[1])
				if e != nil {
					return nil, errors.New("invalid range")
				}
				start, end = a, b
			} else {
				value, e := strconv.Atoi(base)
				if e != nil {
					return nil, errors.New("invalid value")
				}
				start, end = value, value
			}
		}
		if start < min || end > max || start > end {
			return nil, errors.New("value out of range")
		}
		for value := start; value <= end; value += step {
			out[value] = true
		}
	}
	if len(out) == 0 {
		return nil, errors.New("empty field")
	}
	return out, nil
}
func (c cronSpec) Match(t time.Time) bool {
	return c.fields[0][t.Minute()] && c.fields[1][t.Hour()] && c.fields[2][t.Day()] && c.fields[3][int(t.Month())] && c.fields[4][int(t.Weekday())]
}
