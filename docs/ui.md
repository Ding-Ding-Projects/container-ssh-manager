# Browser UI

The browser interface is a same-origin TypeScript and Vite application that uses Material Web components and xterm. It only renders records returned by the service. When an endpoint has no data or is unavailable, the view shows an honest empty or error state rather than sample containers, hosts, or activity.

## Authentication and provenance

On startup the UI calls `GET /api/v1/session`. An unauthenticated browser is directed to `/login`; login submits `{ "password": "…" }` to `POST /api/v1/login`, and logout calls `POST /api/v1/logout`. The front screen fetches `GET /api/v1/version` and displays its version and recorded updated time. Missing provenance is shown as `Unavailable`.

## Navigation and targets

The responsive shell has a host sidebar, tabbed work area, command palette (`Ctrl+Shift+F`), and job drawer. The sidebar provides Overview, Hosts, Containers, Compose, Images, Volumes, Networks, Files, Tunnels, Commands, Schedules, and Settings. A selected host is repeated in each host-scoped heading as `name · user@address`, so an operation always has an obvious target.

Host creation accepts connection data and a password or private key in a password control. The secret is submitted only to create the credential and is never read back, displayed, or retained by the UI. Host removal asks the user to type the host name. Command execution labels the target host and states that output is not retained. File browsing uses the documented SFTP API, including server-side conflict handling for later text editing. The xterm session uses the same-origin WebSocket terminal route.

Tunnels are explicitly created for the selected host, with direction, listener, and destination fields. The UI explains the loopback-only local listener behavior. Schedules explain that they are auto-approved by default and avoid inventing a scheduler form before the jobs API exists.

## Preferences and accessibility

Settings provide English, Traditional Chinese, and bilingual presentation; light, dark, and system theme; separate English and Cantonese playfulness controls; dialog/message emoji preference; speech narration; and a local-only personal-vocabulary JSON picker. Preferences are browser-local. The vocabulary picker enforces a 256 KiB cap, accepts only JSON objects, never uploads its content, and has a clear action.

The shell works from a 320 px viewport, maintains keyboard-accessible native and Material controls, has labelled icon buttons, preserves visible focus from Material components, and collapses the sidebar on small displays. The command palette is keyboard invoked and routes to each application view.

## API coverage and planned surfaces

Implemented connections integration uses `GET/POST/DELETE /api/v1/hosts`, `POST /api/v1/credentials`, `POST /api/v1/hosts/{id}/run`, WebSocket `/api/v1/hosts/{id}/terminal`, `GET /api/v1/hosts/{id}/files`, and `POST /api/v1/tunnels`.

The Containers, Compose, Images, Volumes, Networks, and schedule editor views intentionally wait for their owning API routes. They remain available in navigation and show truthful no-data states. The UI does not yet implement a full regex workbench, complete appearance editing, element locks, local history, external settings sources, or the broader universal settings inventory. Those requirements need backing APIs and durable storage beyond this frontend lane; they are documented here rather than represented as nonfunctional controls.
