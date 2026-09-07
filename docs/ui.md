# Browser interface

The browser interface is a same-origin TypeScript application using locally bundled Material Web controls and xterm. It renders service records, never sample inventory. Dynamic names, identifiers, command output, and error messages use text nodes rather than HTML interpolation. Each resource path segment and query value is encoded independently.

## Authentication and workspace

The first screen requests public `GET /api/v1/version` and displays the running version plus its recorded `updatedAt` timestamp in the reader's local timezone, including seconds. Missing or invalid provenance is unavailable. Authentication uses `GET /api/v1/session`, a real password form with `POST /api/v1/login`, and `POST /api/v1/logout`. The password is cleared before the request completes and is never persisted.

The shell has responsive host selection and navigation, persistent-in-memory workspace tabs, a Jobs dialog, and a command palette opened from the header or `Ctrl+Shift+P`. Each host-scoped tab retains its original host identifier, including refreshes after an asynchronous action. Its heading repeats the target identity. Narrow screens expose the sidebar through Menu. Closing a terminal tab explicitly confirms termination; closing an editor tab warns about unsaved entries.

## Feature and API inventory

| Surface | Implemented controls | API boundary |
| --- | --- | --- |
| Hosts | Add, edit, delete, select, groups, tags, ordered jump chain, metadata-only JSON import, connection test and explicit verified-key enrollment | `/api/v1/hosts` and per-host `test`, `enroll-host-key` |
| Credentials | Password/private-key create, write-only replacement, metadata listing, delete | `/api/v1/credentials` |
| Containers | List/filter, inspect, create with command/environment/ports/mounts/restart/network/labels and advanced HostConfig, start/stop/restart/recreate/remove, logs, stats, execution with arguments/environment/user/directory | `/api/v1/engine/containers` |
| Images | List/filter, pull, build from a host directory, tag, remove, operation polling and cancellation | `/api/v1/engine/images`, `/api/v1/engine/operations/{id}` |
| Volumes | List from the `Volumes` response envelope, create with driver/options/labels, inspect, remove | `/api/v1/engine/volumes` |
| Networks | List, create with driver/options/labels/internal/attachable, inspect, connect/disconnect a selected real container, remove | `/api/v1/engine/networks` |
| Compose | Create/adopt, read current YAML/environment, edit/save both, inspect revisions, restore, validate, deploy with pull/build/detach options, stop, down with explicit volume/image removal options | `/api/v1/engine/compose/projects` |
| Terminal | xterm input/output, fit/resize, same-session reconnect, explicit new session and close | `/api/v1/hosts/{id}/terminal?session=` WebSocket |
| Files | Browse/filter/parent directory, download, upload confirmation, text editor, hash-checked save, conflict preservation and explicit reload | Per-host `/files` routes |
| Tunnels | Start local/reverse forwarding, list exact host tunnels, refresh, stop | `/api/v1/tunnels` |
| Commands | One-time SSH execution with discarded output; saved command CRUD, immutable revisions, explicitly bounded encrypted-output retention, selection of an actual revision and multiple target hosts | Per-host `/run`, `/api/v1/jobs/snippets`, `/api/v1/jobs/runs` |
| Schedules | Create/edit/delete, enabled state, pinned revision and explicit hosts, timezone, five-field cron, server preview of next occurrences | `/api/v1/jobs/schedules` |
| Jobs | Filter runs by host, inspect durable outcomes, cancellation, retained output retrieval, audit records | `/api/v1/jobs/runs`, `/api/v1/jobs/audit` |

New schedules are disabled by default. Enabling a schedule explicitly selects unattended `AUTOAPPROVED` execution. Output retention is disabled by default. The server's recorded outcome remains authoritative: a cancellation request is not reported as confirmed process termination.

Container and image deletion target the resource with `DELETE`; no invented `/remove` action is used. Destructive resource forms require `REMOVE`, identify the resource, and leave force/volume removal off until explicitly selected. Compose restores and tunnel stops also confirm the exact target.

Terminal identifiers remain in memory. Reconnection attaches to the server's original session and resets the local terminal before the server's bounded replay, avoiding duplicate output. Automatic reconnect is bounded to three attempts; explicit Reattach remains available. Tab disposal closes its socket, observer, timers, and terminal. SSH sessions are not available for the synthetic local-engine host.

## Preferences, privacy, and accessibility

Settings store a validated `manager.preferences.v1` record containing only language, theme, density, and font size. English, Cantonese, and bilingual action/field/navigation labels are provided in `web/src/locales.ts`. Factual server records, paths, commands, identifiers, and output remain unchanged. Some explanatory paragraphs still use English; complete prose localization is an open inventory item. Light, dark, and device theme are supported, with live device-theme changes. Invalid, null, corrupt, or unavailable browser storage falls back safely. A denied write is reported as session-only persistence.

Password and private-key values are write-only. Compose environment values and file editor content remain in the current dialog only and are not put in local storage, logs, or export history. Closing a dialog removes its DOM. Dynamic content does not use `innerHTML`. Uploaded host metadata is bounded to 1 MiB and 100 records, rejects unknown fields, and excludes credentials and host keys. File upload is bounded to 32 MiB in the client as well as server constraints.

Material dialogs have explicit asynchronous Save and Close bindings. Failed saves preserve the editor and show an alert. Buttons disable during their request to prevent accidental duplicate mutations. Notifications use a real ARIA live region with dismissible controls. Forms use labelled Material fields, selectors, and checkboxes; file upload uses the native file picker. Keyboard focus remains visible. CSS supports narrow widths from 320 px and reduced motion, but final viewport and assistive-technology verification requires real browser evidence.

All frontend JavaScript and CSS is bundled locally. No Google Fonts or other network font request is made. The pinned toolchain is Vite 8.2.2, Vitest 5.0.0, happy-dom 20.14.0, and Node 24 type definitions 24.13.3. The declared engine requirements support Node 24. The installed npm audit after updating reported zero vulnerabilities on 2026-09-07; this is a point-in-time dependency audit, not a security certification.

## Verification and remaining universal inventory

Run `npm ci`, `npm run build`, and `npm test` from `web/`. Vitest uses two workers and disables file parallelism. Tests exercise actual component click bindings and request shapes: dialog failure preservation, duplicate submission prevention, hostile text rendering, port/config validation, path encoding, raw upload bodies, volume envelopes, corrupt preferences, container deletion confirmation and route, image tagging, SFTP hash conflicts, immutable revision runs, and disabled-by-default schedules. The DOM tests mock Material registration and transport; they do not prove a rendered browser, real engine, or SSH session.

The expanded shared feature inventory remains incomplete. This frontend does not yet implement the full regex builder, all-message localization and independent playfulness controls, emoji preference, narrator/voice controls, shared School mode and unlock ladder, personal-vocabulary JSON loader, scheduled/external settings, dim-sum catalog, complete element appearance editors and logo customization, tab groups, local history, changelog UI, external-editor handoff, file conversion, broad exports and bulk actions, toy locks/support tickets, extension-download surfaces, shared-link graphic, ADHD modes, Ollama suite manager, offline documentation surface, or per-surface Status Hub integration. It also lacks the complete negative completeness regression and built-capture ledger for those contracts. These are explicit open rows, not satisfied by adjacent screens or declared exempt. The required core management flows above are implemented independently of those gaps.

Real backend integration, browser interaction, narrow/high-scale geometry, terminal reconnect under transport interruption, and capture evidence are owned by the integration verification lane. This document does not claim those checks passed based on source or DOM tests.
