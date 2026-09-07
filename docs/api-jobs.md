# Jobs API

All routes require the manager's existing authenticated same-origin session. Every response is a JSON object or array. Errors use `{ "error": "message" }`.

Commands are immutable revisions. A schedule stores the revision identifier selected when it was saved, so editing a snippet can never change a pending scheduled command.

## Snippets

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| `GET` | `/api/v1/jobs/snippets` | | Array of snippets, each with its revisions. |
| `POST` | `/api/v1/jobs/snippets` | `{ "name": "maintenance", "command": "docker ps", "retention": { "enabled": false } }` | `201` snippet and first immutable revision. |
| `GET` | `/api/v1/jobs/snippets/{id}` | | Snippet and revisions. |
| `PUT` | `/api/v1/jobs/snippets/{id}` | `{ "name": "maintenance", "command": "docker ps -a", "retention": { "enabled": true, "maxBytes": 65536, "maxRuns": 10 } }` | New immutable revision and updated snippet. |
| `DELETE` | `/api/v1/jobs/snippets/{id}` | | `204`. Deletion is refused while a schedule references the snippet. |

Output is discarded by default. Retention must be explicitly enabled per revision, is bounded by `maxBytes` and `maxRuns`, and is encrypted through the server vault before persistence. The bounded output runner rejects data beyond the selected byte limit and plaintext output is never stored in a run record or audit event.

## Schedules

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| `GET` | `/api/v1/jobs/schedules` | | Array of schedules. |
| `POST` | `/api/v1/jobs/schedules` | `{ "hostIds": ["host-1", "host-2"], "snippetId": "snippet-1", "revisionId": "revision-1", "cron": "0 3 * * *", "timezone": "America/Toronto", "enabled": true }` | `201` schedule. |
| `GET` | `/api/v1/jobs/schedules/{id}` | | Schedule. |
| `PUT` | `/api/v1/jobs/schedules/{id}` | Same fields as create. | Updated schedule. |
| `DELETE` | `/api/v1/jobs/schedules/{id}` | | `204`. |
| `POST` | `/api/v1/jobs/schedules/preview` | `{ "cron": "0 3 * * *", "timezone": "America/Toronto", "after": "2026-09-07T00:00:00Z", "count": 5 }` | `{ "timezone":"America/Toronto", "times":[...] }`. |

Schedules use five-field cron syntax: minute, hour, day-of-month, month, and day-of-week. Each field accepts `*`, integers, comma lists, ranges, and steps such as `*/15` or `1-5/2`. `timezone` defaults to `America/Toronto` and accepts an IANA timezone name. A schedule runs only on its explicit `hostIds` and submits the stored immutable revision with `AUTOAPPROVED` intent metadata. Legacy `hostId` input remains accepted for one host.

Missed ticks are never replayed after restart. Overlapping occurrences are skipped. The scheduler permits one running scheduled command per host and four running commands total.

## Runs and audit

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| `GET` | `/api/v1/jobs/runs` | Optional `?hostId=&scheduleId=` | Array of runs with per-host outcome. |
| `GET` | `/api/v1/jobs/runs/{id}` | | Run. |
| `POST` | `/api/v1/jobs/runs` | `{ "hostIds": ["host-1", "host-2"], "revisionId": "revision-1", "timeoutSeconds": 600 }` | `202` `{ "runs": [...] }`, one independently durable run and outcome per host. |
| `POST` | `/api/v1/jobs/runs/{id}/cancel` | | `202` cancellation result. |
| `GET` | `/api/v1/jobs/audit` | Optional `?limit=100` | Audit events, newest first. |

The server persists immutable run intent and command revision before opening a connection. The default timeout is 600 seconds and requests may not exceed 600 seconds. A cancellation request reports `cancelled` only when the process termination is confirmed. If confirmation is unavailable, the run is reported as `termination_unknown`; the server never claims a stopped process without evidence. Restart recovery marks unfinished records `unknown` and does not replay them. Audit records include actor, source, host, schedule, revision, timestamps, result, and cancellation evidence and are retained for 90 days.
