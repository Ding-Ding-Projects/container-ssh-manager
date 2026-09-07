# Connections API

All routes require the root session cookie and return JSON. Error responses are
`{"error":"message"}`. Host key changes are never accepted implicitly.

## Hosts

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| GET | `/api/v1/hosts` | | `Host[]` |
| POST | `/api/v1/hosts` | `HostInput` | `Host` |
| GET | `/api/v1/hosts/{id}` | | `Host` |
| PUT | `/api/v1/hosts/{id}` | `HostInput` | `Host` |
| DELETE | `/api/v1/hosts/{id}` | | `204` |
| POST | `/api/v1/hosts/{id}/test` | | `{connected,hostKey}` |
| POST | `/api/v1/hosts/{id}/enroll-host-key` | `{hostKey}` | `Host` |

`Host` contains `id`, `name`, `address`, `port`, `user`, `credentialId`,
`hostKey`, `jumpIds`, `group`, and `tags`. `HostInput` omits `id` and accepts
the other fields. `test` returns a discovered host key only for an unpinned
host. A pinned key mismatch returns `409` and cannot be enrolled through test.

## Credentials

| Method | Route | Request | Response |
| --- | --- | --- | --- |
| GET | `/api/v1/credentials` | | `CredentialMetadata[]` |
| POST | `/api/v1/credentials` | `{name,kind,secret}` | `CredentialMetadata` |
| PUT | `/api/v1/credentials/{id}` | `{name,secret?}` | `CredentialMetadata` |
| DELETE | `/api/v1/credentials/{id}` | | `204` |

Kinds are `password` and `privateKey`. Responses never include `secret` or
sealed data.

## Command and engine bridge

`POST /api/v1/hosts/{id}/run` accepts `{command}` and returns `{exitCode}`.
Command output is discarded. `GET /api/v1/hosts/{id}/engine` is reserved for
the container lane and returns `404` to browser callers.

## Terminals

`GET /api/v1/hosts/{id}/terminal` upgrades to WebSocket. The WebSocket origin
must be same-origin, even though the HTTP route is authenticated. Client frames:
`{"type":"input","data":"..."}`, `{"type":"resize","cols":80,"rows":24}`,
and `{"type":"reconnect"}`. Server frames are `{"type":"output","data":"..."}`
and `{"type":"status","state":"connected|reconnecting|closed","message":"..."}`.
Output is bounded; slow clients receive the newest buffered data only.

## SFTP

| Method | Route | Request/response |
| --- | --- | --- |
| GET | `/api/v1/hosts/{id}/files?path=/` | `FileEntry[]` |
| GET | `/api/v1/hosts/{id}/files/content?path=` | `{content,hash}` |
| PUT | `/api/v1/hosts/{id}/files/content?path=` | `{content,hash}` returns `{hash}` |
| POST | `/api/v1/hosts/{id}/files/upload?path=` | raw request body returns `FileEntry` |
| GET | `/api/v1/hosts/{id}/files/download?path=` | streamed attachment |

Text writes require the current SHA-256 `hash`; a mismatch returns `409`.

## Tunnels

| Method | Route | Request | Response |
| --- | --- | --- |
| GET | `/api/v1/tunnels` | | `Tunnel[]` |
| POST | `/api/v1/tunnels` | `{hostId,direction,listenAddress,listenPort,targetAddress,targetPort}` | `Tunnel` |
| DELETE | `/api/v1/tunnels/{id}` | | `204` |

Directions are `local` and `reverse`. Local listeners bind loopback only.
Tunnel records report lifecycle state and never expose credentials.
