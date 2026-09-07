// Package jobs owns durable, unattended command scheduling.
package jobs

import (
	"context"
	"database/sql"
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
	"github.com/robfig/cron/v3"
)

const (
	defaultTimeout = 10 * time.Minute
	maxRunning     = 4
	maxAccepted    = 128
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
	Source     string    `json:"source,omitempty"`
	Intent     string    `json:"intent,omitempty"`
	Action     string    `json:"action"`
	HostID     string    `json:"hostId,omitempty"`
	ScheduleID string    `json:"scheduleId,omitempty"`
	RevisionID string    `json:"revisionId,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	Result     string    `json:"result,omitempty"`
	Metadata   any       `json:"metadata,omitempty"`
}

type outputEnvelope struct {
	RunID      string `json:"runId"`
	RevisionID string `json:"revisionId"`
	MaxBytes   int    `json:"maxBytes"`
	Output     []byte `json:"output"`
}

type acceptanceError struct {
	status  int
	message string
}

func (e *acceptanceError) Error() string      { return e.message }
func reject(status int, message string) error { return &acceptanceError{status, message} }

var errRevisionNotFound = errors.New("revision not found")

type scheduleClaim struct {
	key        string
	occurrence occurrence
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

	mu            sync.Mutex
	outputMu      sync.Mutex
	ctx           context.Context
	stop          context.CancelFunc
	initialized   bool
	initErr       error
	startupMinute time.Time
	active        map[string]*execution
	hosts         map[string]string
	queue         []*execution
	workers       int
	wg            sync.WaitGroup
	run           func(context.Context, string, string) (int, error)
	runOutput     func(context.Context, string, string, int) (int, []byte, error)
}

type execution struct {
	run    Run
	ctx    context.Context
	cancel context.CancelFunc
}

type occurrence struct {
	Minute        time.Time `json:"minute"`
	WallMinute    string    `json:"wallMinute"`
	Configuration string    `json:"configuration"`
}

func New(store *core.Store, connections *connection.Manager, vault *core.Vault) *Manager {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.FixedZone(timezone, -5*60*60)
	}
	ctx, stop := context.WithCancel(context.Background())
	m := &Manager{store: store, connections: connections, vault: vault, location: loc, now: time.Now, ctx: ctx, stop: stop, active: make(map[string]*execution), hosts: make(map[string]string)}
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
	mux.HandleFunc("GET /api/v1/jobs/runs/{id}/output", m.getOutput)
	mux.HandleFunc("POST /api/v1/jobs/runs/{id}/cancel", m.cancelRun)
	mux.HandleFunc("GET /api/v1/jobs/audit", m.listAudit)
}

// Start observes future minute boundaries only. Recovery runs once before any acceptance,
// including an HTTP request arriving before the scheduler goroutine starts.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.initializeLocked()
	err := m.initErr
	m.mu.Unlock()
	if err != nil {
		return
	}
	defer m.Close()
	for {
		now := m.now()
		timer := time.NewTimer(now.Truncate(time.Minute).Add(time.Minute).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.runDue(m.ctx, m.now().UTC().Truncate(time.Minute))
		}
	}
}

// Close stops accepting commands. Queued commands have never contacted a host;
// active SSH commands remain termination_unknown unless termination is proven.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop()
	for _, e := range m.active {
		e.cancel()
		if e.run.Status == "queued" {
			e.run.Status = "cancelled"
			now := m.now().UTC()
			e.run.FinishedAt = &now
			e.run.Error = "server stopped before execution"
			if err := m.persistRunEvent(e.run, "system", "run_stopped_before_execution", e.run.Status); err != nil {
				m.initErr = err
				continue
			}
			delete(m.active, e.run.ID)
			delete(m.hosts, e.run.HostID)
		}
	}
	m.queue = nil
}

func (m *Manager) initializeLocked() {
	if m.initialized {
		return
	}
	m.initialized = true
	m.startupMinute = m.now().UTC().Truncate(time.Minute)
	m.initErr = m.recoverUnknown()
}

func (m *Manager) runDue(_ context.Context, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initializeLocked()
	now = now.UTC().Truncate(time.Minute)
	// A late wake checks only its current minute, never a backlog or the minute
	// in which this manager started. UTC ordering also tolerates clock rollback.
	if m.initErr != nil || m.ctx.Err() != nil || !now.After(m.startupMinute) || !now.Equal(m.now().UTC().Truncate(time.Minute)) {
		return
	}
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
		for _, host := range scheduleHosts(schedule) {
			// One stable row per schedule/host carries the configuration identity.
			// Local wall time deliberately skips the repeated DST fall-back hour.
			key := schedule.ID + ":" + host
			configuration := schedule.Timezone + ":" + schedule.Cron + ":" + schedule.UpdatedAt.UTC().Format(time.RFC3339Nano)
			wall := now.In(loc).Format("2006-01-02T15:04")
			var last occurrence
			err := m.store.Get("job_occurrence", key, &last)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err == nil && last.Configuration == configuration && (!now.After(last.Minute) || wall <= last.WallMinute) {
				continue
			}
			claim := scheduleClaim{key: key, occurrence: occurrence{Minute: now, WallMinute: wall, Configuration: configuration}}
			_, err = m.acceptLocked([]string{host}, schedule.RevisionID, int(defaultTimeout.Seconds()), schedule.ID, claim)
			if err != nil {
				event := AuditEvent{ID: core.ID(), At: m.now().UTC(), Actor: "system", Source: "schedule", Intent: "AUTOAPPROVED", Action: "schedule_skipped", HostID: host, ScheduleID: schedule.ID, RevisionID: schedule.RevisionID, Result: "skipped"}
				if err := m.persistSkip(claim, event); err != nil {
					m.initErr = err
					return
				}
			}
		}
	}
	m.dispatchLocked()
}

// submit is also used by scheduler tests; request cancellation must never own a
// durable accepted command. Only manager shutdown and explicit cancellation do.
func (m *Manager) submit(_ context.Context, run Run) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initializeLocked()
	runs, err := m.acceptLocked([]string{run.HostID}, run.RevisionID, run.TimeoutSeconds, run.ScheduleID)
	if err != nil {
		return Run{}, err
	}
	m.dispatchLocked()
	return runs[0], nil
}

// acceptLocked persists the entire multi-host batch and its audit events in one
// transaction before publishing reservations or allowing any connection to open.
func (m *Manager) acceptLocked(hosts []string, revisionID string, timeout int, scheduleID string, claims ...scheduleClaim) ([]Run, error) {
	if m.initErr != nil {
		return nil, reject(503, "job persistence unavailable; restart required")
	}
	if m.ctx.Err() != nil {
		return nil, reject(503, "job manager is stopped")
	}
	if timeout == 0 {
		timeout = int(defaultTimeout.Seconds())
	}
	if timeout < 1 || timeout > int(defaultTimeout.Seconds()) {
		return nil, reject(400, "timeoutSeconds must be between 1 and 600")
	}
	hosts = uniqueHosts(hosts)
	if len(hosts) == 0 || strings.TrimSpace(revisionID) == "" {
		return nil, reject(400, "hostIds and revisionId are required")
	}
	if len(m.active)+len(hosts) > maxAccepted {
		return nil, reject(409, "job queue is full (128 accepted commands)")
	}
	for _, host := range hosts {
		if m.hosts[host] != "" {
			return nil, reject(409, fmt.Sprintf("host %s already has an accepted job; no commands accepted", host))
		}
	}
	revision, err := m.revision(revisionID)
	if err != nil {
		if errors.Is(err, errRevisionNotFound) {
			return nil, reject(404, "revision not found")
		}
		return nil, reject(503, "revision storage unavailable")
	}
	if err = validRetention(revision.Retention); err != nil {
		return nil, reject(503, "stored output retention is invalid")
	}
	if revision.Retention.Enabled && m.vault == nil {
		return nil, reject(503, "output retention vault unavailable")
	}
	runs := make([]Run, 0, len(hosts))
	tx, err := m.store.DB.Begin()
	if err != nil {
		return nil, reject(503, "cannot persist command intent")
	}
	defer tx.Rollback()
	for _, claim := range claims {
		if err := upsertTransaction(tx, "job_occurrence", claim.key, claim.occurrence); err != nil {
			return nil, reject(503, "cannot persist schedule occurrence")
		}
	}
	for _, host := range hosts {
		run := Run{ID: core.ID(), HostID: host, RevisionID: revisionID, CommandRevision: revision, ScheduleID: scheduleID, TimeoutSeconds: timeout, Status: "queued", StartedAt: m.now().UTC(), Source: "manual", Intent: "USER_APPROVED"}
		actor := "session"
		if scheduleID != "" {
			run.Source, run.Intent, actor = "schedule", "AUTOAPPROVED", "system"
		}
		if err = putTransaction(tx, "job_run", run.ID, run); err != nil {
			return nil, reject(503, "cannot persist command intent")
		}
		event := AuditEvent{ID: core.ID(), At: m.now().UTC(), Actor: actor, Source: run.Source, Intent: run.Intent, Action: "run_intent_saved", HostID: host, ScheduleID: scheduleID, RevisionID: revisionID, RunID: run.ID, Result: "queued"}
		if err = putTransaction(tx, "job_audit", event.ID, event); err != nil {
			return nil, reject(503, "cannot persist command audit")
		}
		runs = append(runs, run)
	}
	if err = tx.Commit(); err != nil {
		return nil, reject(503, "cannot persist command intent")
	}
	for _, run := range runs {
		ctx, cancel := context.WithCancel(m.ctx)
		e := &execution{run: run, ctx: ctx, cancel: cancel}
		m.active[run.ID] = e
		m.hosts[run.HostID] = run.ID
		m.queue = append(m.queue, e)
	}
	return runs, nil
}

func putTransaction(tx *sql.Tx, kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO records(kind,id,data,updated_at) VALUES(?,?,?,?)`, kind, id, string(data), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func upsertTransaction(tx *sql.Tx, kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO records(kind,id,data,updated_at) VALUES(?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at`, kind, id, string(data), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func pruneAuditTransaction(tx *sql.Tx, cutoff time.Time) error {
	_, err := tx.Exec(`DELETE FROM records WHERE kind='job_audit' AND json_extract(data,'$.at') < ?`, cutoff.UTC().Format(time.RFC3339Nano))
	return err
}

func (m *Manager) persistRunEvent(run Run, actor, action, result string) error {
	tx, err := m.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	event := AuditEvent{ID: core.ID(), At: m.now().UTC(), Actor: actor, Source: run.Source, Intent: run.Intent, Action: action, HostID: run.HostID, ScheduleID: run.ScheduleID, RevisionID: run.RevisionID, RunID: run.ID, Result: result, Metadata: map[string]any{"cancelRequested": run.CancelRequested}}
	if err := upsertTransaction(tx, "job_run", run.ID, run); err != nil {
		return err
	}
	if err := putTransaction(tx, "job_audit", event.ID, event); err != nil {
		return err
	}
	if err := pruneAuditTransaction(tx, m.now().Add(-90*24*time.Hour)); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) persistSkip(claim scheduleClaim, event AuditEvent) error {
	tx, err := m.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertTransaction(tx, "job_occurrence", claim.key, claim.occurrence); err != nil {
		return err
	}
	if err := putTransaction(tx, "job_audit", event.ID, event); err != nil {
		return err
	}
	if err := pruneAuditTransaction(tx, m.now().Add(-90*24*time.Hour)); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) dispatchLocked() {
	if m.ctx.Err() != nil || m.initErr != nil {
		return
	}
	for m.workers < maxRunning && len(m.queue) > 0 {
		e := m.queue[0]
		if _, exists := m.active[e.run.ID]; !exists {
			m.queue = m.queue[1:]
			continue
		}
		e.run.Status = "running"
		if err := m.store.Put("job_run", e.run.ID, e.run); err != nil {
			e.run.Status = "queued"
			return
		}
		m.queue = m.queue[1:]
		m.workers++
		m.wg.Add(1)
		go m.execute(e)
	}
}

func (m *Manager) execute(e *execution) {
	defer m.wg.Done()
	// Snapshot is immutable after acceptance. No lookup of a mutable snippet is
	// permitted on this path, including after deletion or another revision edit.
	m.mu.Lock()
	run := e.run
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(e.ctx, time.Duration(run.TimeoutSeconds)*time.Second)
	defer cancel()
	var err error
	var code int
	launched := ctx.Err() == nil
	if launched {
		if run.CommandRevision.Retention.Enabled {
			var output []byte
			code, output, err = m.runOutput(ctx, run.HostID, run.CommandRevision.Command, run.CommandRevision.Retention.MaxBytes)
			if err == nil {
				err = m.saveOutput(run.ID, run.RevisionID, output, run.CommandRevision.Retention)
			}
			clear(output)
		} else {
			code, err = m.run(ctx, run.HostID, run.CommandRevision.Command)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run.CancelRequested = e.run.CancelRequested
	now := m.now().UTC()
	run.FinishedAt = &now
	if !launched {
		run.Status = "cancelled"
		run.Error = "cancelled before execution"
	} else if run.CancelRequested || errors.Is(ctx.Err(), context.Canceled) {
		run.Status = "termination_unknown"
		run.Error = "cancellation requested; remote process termination could not be confirmed"
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		run.Status = "termination_unknown"
		run.Error = "timeout reached; remote process termination could not be confirmed"
	} else if err != nil {
		run.Status = "failed"
		// Transport errors can echo commands or remote output. Never persist them.
		run.Error = "command execution or output retention failed"
	} else {
		run.ExitCode = &code
		run.Status = "succeeded"
		if code != 0 {
			run.Status = "failed"
		}
	}
	if err := m.persistRunEvent(run, "system", "run_finished", run.Status); err != nil {
		m.initErr = err
		// Keep the reservation when the final result cannot be made durable.
		// A restart will recover this persisted running row as unknown.
		e.run = run
		e.cancel()
		m.workers--
		return
	}
	e.run = run
	delete(m.active, run.ID)
	delete(m.hosts, run.HostID)
	e.cancel()
	m.workers--
	m.dispatchLocked()
}

func (m *Manager) recoverUnknown() error {
	runs, err := m.runs()
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == "queued" || run.Status == "running" {
			run.Status = "unknown"
			now := m.now().UTC()
			run.FinishedAt = &now
			run.Error = "server restarted; run was not replayed"
			if err := m.persistRunEvent(run, "system", "run_recovered_unknown", run.Status); err != nil {
				return err
			}
		}
	}
	return nil
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
	return Revision{}, errRevisionNotFound
}
func (m *Manager) audit(actor, action, host, schedule, revision, result string, metadata any) {
	event := AuditEvent{ID: core.ID(), At: m.now().UTC(), Actor: actor, Action: action, HostID: host, ScheduleID: schedule, RevisionID: revision, Result: result, Metadata: metadata}
	if fields, ok := metadata.(map[string]any); ok {
		event.RunID, _ = fields["runId"].(string)
	}
	if event.RunID != "" || strings.HasPrefix(action, "schedule_skipped") {
		event.Source, event.Intent = "manual", "USER_APPROVED"
		if schedule != "" {
			event.Source, event.Intent = "schedule", "AUTOAPPROVED"
		}
	}
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
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	if err := validRetention(retention); err != nil || !retention.Enabled {
		return errors.New("invalid output retention")
	}
	if len(plaintext) > retention.MaxBytes {
		return errors.New("command output exceeds retention limit")
	}
	if m.vault == nil {
		return errors.New("output retention vault unavailable")
	}
	envelope, err := json.Marshal(outputEnvelope{RunID: runID, RevisionID: revisionID, MaxBytes: retention.MaxBytes, Output: plaintext})
	if err != nil {
		return err
	}
	defer clear(envelope)
	ciphertext, err := m.vault.Seal(envelope)
	if err != nil {
		return err
	}
	runs, err := m.runs()
	if err != nil {
		return err
	}
	retained := make([]Run, 0, retention.MaxRuns)
	for _, run := range runs {
		if run.RevisionID == revisionID {
			if run.ID == runID {
				retained = append(retained, run)
				continue
			}
			var record RetainedOutput
			err := m.store.Get("job_output", run.ID, &record)
			if err == nil {
				retained = append(retained, run)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	tx, err := m.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := putTransaction(tx, "job_output", runID, RetainedOutput{RunID: runID, Ciphertext: ciphertext, MaxBytes: retention.MaxBytes, CreatedAt: m.now().UTC()}); err != nil {
		return err
	}
	for index := retention.MaxRuns; index < len(retained); index++ {
		if _, err := tx.Exec(`DELETE FROM records WHERE kind='job_output' AND id=?`, retained[index].ID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	id := r.PathValue("id")
	var old Schedule
	if e = m.store.Get("job_schedule", id, &old); e != nil {
		fail(w, 404, "schedule not found")
		return
	}
	s.ID = id
	s.CreatedAt = old.CreatedAt
	s.UpdatedAt = m.now().UTC()
	if e = m.changeSchedule(s, false); e != nil {
		fail(w, 500, e.Error())
		return
	}
	m.audit("session", "schedule_updated", s.HostID, s.ID, s.RevisionID, "updated", nil)
	write(w, 200, s)
}
func (m *Manager) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := r.PathValue("id")
	var schedule Schedule
	if e := m.store.Get("job_schedule", id, &schedule); e != nil {
		fail(w, 404, "schedule not found")
		return
	}
	if e := m.changeSchedule(schedule, true); e != nil {
		fail(w, 503, "schedule storage unavailable")
		return
	}
	m.audit("session", "schedule_deleted", "", id, "", "deleted", nil)
	w.WriteHeader(204)
}

// Replace/delete the schedule and retire all its prior host/configuration claims
// atomically. The scheduler mutex prevents a tick from restoring an obsolete row.
func (m *Manager) changeSchedule(schedule Schedule, remove bool) error {
	tx, err := m.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if remove {
		if _, err := tx.Exec(`DELETE FROM records WHERE kind='job_schedule' AND id=?`, schedule.ID); err != nil {
			return err
		}
	} else if err := upsertTransaction(tx, "job_schedule", schedule.ID, schedule); err != nil {
		return err
	}
	prefix := schedule.ID + ":"
	if _, err := tx.Exec(`DELETE FROM records WHERE kind='job_occurrence' AND substr(id,1,length(?))=?`, prefix, prefix); err != nil {
		return err
	}
	return tx.Commit()
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
	cursor := in.After.In(loc)
	for len(times) < in.Count {
		if r.Context().Err() != nil {
			fail(w, 408, "preview cancelled")
			return
		}
		cursor = spec.Next(cursor)
		if cursor.IsZero() {
			fail(w, 400, "cron has no occurrence within the five-year search horizon")
			return
		}
		times = append(times, cursor)
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

// Run requests intentionally omit server-owned audit and execution fields.
type runRequest struct {
	HostID         string   `json:"hostId"`
	HostIDs        []string `json:"hostIds"`
	RevisionID     string   `json:"revisionId"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}

func (m *Manager) createRun(w http.ResponseWriter, r *http.Request) {
	var in runRequest
	if err := decode(r, &in); err != nil {
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
	m.mu.Lock()
	m.initializeLocked()
	runs, err := m.acceptLocked(hosts, in.RevisionID, in.TimeoutSeconds, "")
	if err == nil {
		m.dispatchLocked()
	}
	m.mu.Unlock()
	if err != nil {
		status := 500
		var rejected *acceptanceError
		if errors.As(err, &rejected) {
			status = rejected.status
		}
		fail(w, status, err.Error())
		return
	}
	write(w, 202, map[string]any{"runs": runs})
}

// Register is mounted inside the same authenticated, same-origin boundary as all
// other jobs routes. Plaintext is available only through this deliberate read.
func (m *Manager) getOutput(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var run Run
	if m.store.Get("job_run", r.PathValue("id"), &run) != nil {
		fail(w, 404, "run not found")
		return
	}
	if !run.CommandRevision.Retention.Enabled {
		fail(w, 404, "output was not retained")
		return
	}
	m.outputMu.Lock()
	defer m.outputMu.Unlock()
	var record RetainedOutput
	if m.store.Get("job_output", run.ID, &record) != nil {
		fail(w, 404, "output was not retained or has expired")
		return
	}
	if m.vault == nil {
		fail(w, 503, "output vault unavailable")
		return
	}
	if record.RunID != run.ID || record.MaxBytes != run.CommandRevision.Retention.MaxBytes || record.MaxBytes < 1 || record.MaxBytes > 1<<20 || len(record.Ciphertext) > record.MaxBytes*2+512 {
		fail(w, 500, "invalid retained output")
		return
	}
	plaintext, err := m.vault.Open(record.Ciphertext)
	if err != nil {
		fail(w, 500, "retained output unavailable")
		return
	}
	defer clear(plaintext)
	var envelope outputEnvelope
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		fail(w, 500, "invalid retained output")
		return
	}
	defer clear(envelope.Output)
	if envelope.RunID != run.ID || envelope.RevisionID != run.RevisionID || envelope.MaxBytes != record.MaxBytes || len(envelope.Output) > record.MaxBytes {
		fail(w, 500, "invalid retained output identity or bound")
		return
	}
	write(w, 200, map[string]any{"runId": run.ID, "output": string(envelope.Output)})
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
	m.mu.Lock()
	defer m.mu.Unlock()
	var run Run
	if err := m.store.Get("job_run", r.PathValue("id"), &run); err != nil {
		fail(w, 404, "run not found")
		return
	}
	e, ok := m.active[run.ID]
	if !ok || (e.run.Status != "queued" && e.run.Status != "running") {
		fail(w, 409, "run is not active")
		return
	}
	run = e.run
	run.CancelRequested = true
	result := "termination_unknown"
	if run.Status == "queued" {
		// This reservation never reached a worker, so cancellation is proven.
		run.Status, result = "cancelled", "cancelled"
		now := m.now().UTC()
		run.FinishedAt = &now
	}
	if err := m.persistRunEvent(run, "session", "run_cancel_requested", result); err != nil {
		fail(w, 500, "cannot persist cancellation")
		return
	}
	e.run = run
	e.cancel()
	if result == "cancelled" {
		delete(m.active, run.ID)
		delete(m.hosts, run.HostID)
		// Physically remove cancelled queue entries so repeated submit/cancel
		// requests cannot grow the in-memory queue beyond its durable bound.
		for i, candidate := range m.queue {
			if candidate == e {
				m.queue = append(m.queue[:i], m.queue[i+1:]...)
				break
			}
		}
	}
	write(w, 202, map[string]string{"status": result})
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

// The pinned cron parser handles standard day-of-month/day-of-week semantics,
// single-value steps, and a bounded Next search for impossible dates.
type cronSpec struct{ cron.Schedule }

func parseCron(text string) (cronSpec, error) {
	schedule, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(text)
	if err != nil {
		return cronSpec{}, errors.New("invalid five-field cron expression")
	}
	return cronSpec{schedule}, nil
}
func (c cronSpec) Match(t time.Time) bool {
	t = t.Truncate(time.Minute)
	return c.Next(t.Add(-time.Minute)).Equal(t)
}
