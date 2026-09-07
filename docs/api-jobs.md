# Jobs API

All routes require the manager's existing authenticated same-origin session. Every response is a JSON object or array. Errors use `{ "error": "message" }`. Manual acceptance returns `400` for invalid fields or timeout, `404` for an absent revision, `409` for host or queue conflicts, and `503` for unavailable persistence, vault, recovery, or a stopped manager.

Commands are immutable revisions. A schedule stores the revision identifier selected when it was saved, so editing a snippet can never change a pending scheduled command.

## Snippets

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| `GET` | `/api/v1/jobs/snippets` | | Array of snippets, each with its revisions. |
| `POST` | `/api/v1/jobs/snippets` | `{ "name": "maintenance", "command": "docker ps", "retention": { "enabled": false } }` | `201` snippet and first immutable revision. |
| `GET` | `/api/v1/jobs/snippets/{id}` | | Snippet and revisions. |
| `PUT` | `/api/v1/jobs/snippets/{id}` | `{ "name": "maintenance", "command": "docker ps -a", "retention": { "enabled": true, "maxBytes": 65536, "maxRuns": 10 } }` | New immutable revision and updated snippet. |
| `DELETE` | `/api/v1/jobs/snippets/{id}` | | `204`. Deletion is refused while a schedule references the snippet. |

Output is discarded by default. Retention must be explicitly enabled per revision, is bounded by `maxBytes` (1 through 1,048,576) and `maxRuns` (1 through 1,000), and is encrypted through the server vault before persistence. The encrypted envelope binds the output to its run ID, revision ID, and byte limit, preventing a valid ciphertext from being reassigned to another run. The bounded output runner rejects data beyond the selected byte limit and plaintext output is never stored in a run record or audit event. Transport error text is replaced with a generic outcome because it can contain remote output. Concurrent retention writes are serialized, and encrypted insertion plus pruning commit in one transaction, so the per-revision record bound applies after every completed write. Retention-enabled commands are rejected before acceptance if the vault is unavailable.

## Schedules

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| `GET` | `/api/v1/jobs/schedules` | | Array of schedules. |
| `POST` | `/api/v1/jobs/schedules` | `{ "hostIds": ["host-1", "host-2"], "snippetId": "snippet-1", "revisionId": "revision-1", "cron": "0 3 * * *", "timezone": "America/Toronto", "enabled": true }` | `201` schedule. |
| `GET` | `/api/v1/jobs/schedules/{id}` | | Schedule. |
| `PUT` | `/api/v1/jobs/schedules/{id}` | Same fields as create. | Updated schedule. |
| `DELETE` | `/api/v1/jobs/schedules/{id}` | | `204`. |
| `POST` | `/api/v1/jobs/schedules/preview` | `{ "cron": "0 3 * * *", "timezone": "America/Toronto", "after": "2026-09-07T00:00:00Z", "count": 5 }` | `{ "timezone":"America/Toronto", "times":[...] }`. |

Schedules use five-field cron syntax: minute, hour, day-of-month, month, and day-of-week, parsed by the pinned `robfig/cron/v3` dependency. Each field accepts `*`, integers, comma lists, ranges, and steps such as `*/15`, `1-5/2`, or `5/15` (minutes 5, 20, 35, and 50). When both day-of-month and day-of-week are restricted, either may match. `timezone` defaults to `America/Toronto` and accepts an IANA timezone name. A schedule runs only on its explicit `hostIds` and submits the stored immutable revision with `AUTOAPPROVED` intent metadata. Legacy `hostId` input remains accepted for one host. Preview checks request cancellation and returns `400` when no occurrence exists within the parser's bounded five-year search horizon, including impossible dates such as February 31.

Missed ticks are never replayed after restart. Startup skips the current minute and waits for the next minute boundary; delayed wakes examine only the current minute. A persisted schedule-configuration/host watermark claims each matching occurrence in the same transaction as acceptance, or in the same transaction as its skipped-occurrence audit when its host or queue is occupied. Rechecking a minute or restarting cannot replay that claim. Repeated local wall-clock minutes during the daylight-saving fall-back hour are skipped, and clock rollback cannot replay earlier claims. Editing the schedule records a new configuration identity, so switching to an earlier timezone does not suppress valid future occurrences. Each schedule/host has one stable watermark row; edits and deletion retire obsolete claims in the same transaction as the schedule mutation, including claims for removed hosts.

The manager allows four executing commands and up to 128 accepted commands in total. Excess accepted work waits in a persisted FIFO queue rather than being discarded when all four execution slots are occupied. Both scheduled and manual commands reserve their host atomically at acceptance, including time spent queued, so only one command can be accepted for a host at once. Scheduled overlap is skipped with an audit event. Manual overlap rejects the entire requested batch with `409` and the conflicting host; a queue-capacity rejection likewise accepts nothing.

## Runs and audit

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| `GET` | `/api/v1/jobs/runs` | Optional `?hostId=&scheduleId=` | Array of runs with per-host outcome. |
| `GET` | `/api/v1/jobs/runs/{id}` | | Run. |
| `GET` | `/api/v1/jobs/runs/{id}/output` | | `200` `{ "runId": "...", "output": "..." }` for explicitly retained output; `404` if disabled or expired. |
| `POST` | `/api/v1/jobs/runs` | `{ "hostIds": ["host-1", "host-2"], "revisionId": "revision-1", "timeoutSeconds": 600 }` | `202` `{ "runs": [...] }`, one durable run and outcome per host, accepted atomically as one batch. |
| `POST` | `/api/v1/jobs/runs/{id}/cancel` | | `202` cancellation result. |
| `GET` | `/api/v1/jobs/audit` | Optional `?limit=100` | Audit events, newest first. |

The server persists the complete batch's immutable run intent, command snapshots, and acceptance audit events in one transaction before opening any connection. If one host conflicts or a write fails, no command in that batch starts and no partial run ID is hidden from the caller. Execution reads only the accepted snapshot, so deleting or editing the originating snippet cannot change an accepted command. `source`, `intent`, `scheduleId`, status, timestamps, and snapshots are server-owned fields and are rejected in manual-run requests. Manual commands always carry `manual` and `USER_APPROVED`; schedules always carry `schedule` and `AUTOAPPROVED`.

Accepted work belongs to the manager lifecycle, not the HTTP request. Returning `202`, closing a browser, or cancelling that HTTP request does not cancel a run. The default timeout is 600 seconds and requests may not exceed 600 seconds; the timeout begins when a worker takes the command, excluding queued time. Explicit cancellation is available immediately after acceptance. Queued cancellation reports `cancelled` because no worker has opened a connection, removes the queued entry, and releases the host reservation. An executing cancellation reports `termination_unknown` until evidence proves otherwise. A timeout likewise reports `termination_unknown` with an explicit timeout reason, never a claim that the remote process stopped. Restart recovery marks unfinished queued and running records `unknown` and does not execute them again. The queue is durable for intent and recovery evidence, not automatic replay after restart.

The output endpoint is protected by the same authenticated, same-origin manager boundary as other jobs routes and returns `Cache-Control: no-store`. It decrypts only a matching run's explicitly retained record, verifies its configured bound, and refuses malformed or tampered ciphertext. Ordinary run and audit reads never return retained output. Audit records include actor, source, host, schedule, revision, timestamps, result, and cancellation evidence and are retained for 90 days.

Terminal outcomes, explicit cancellations, restart recovery, and skipped occurrences commit together with their audit evidence. If terminal or recovery persistence fails, the manager stops accepting and dispatching additional commands and responds with `503` to new run requests. Queued intent and the last durable running state remain intact for investigation and restart recovery; an unavailable database never produces a fabricated successful completion. Running commands may still finish, and each result must pass the same durable outcome transaction.

## Verification

Run `go test -race ./internal/jobs`. The focused suite drives registered handlers through real HTTP servers, including handler return while a command continues, concurrent same-host requests, atomic multi-host conflicts and injected database failure, four-worker queueing, the 128-command acceptance bound, queued and immediate cancellation, immutable snapshots after snippet deletion, forged server metadata rejection, encrypted output expiry/retrieval/tamper rejection, timeout evidence, impossible preview dates, single-value cron steps, persisted occurrence deduplication, restart recovery, clock rollback, daylight-saving repetition, and skipped overlap. SSH transport cancellation and authentication middleware require the corresponding connection/server integration suites; context-aware test runners alone do not establish those boundaries.
