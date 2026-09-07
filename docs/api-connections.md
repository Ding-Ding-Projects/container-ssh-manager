# Connections API

All routes require the root session cookie and return JSON. Error responses are
`{"error":"message"}`. Host key changes are never accepted implicitly. Host updates preserve the saved
key exactly; enrollment cannot replace an already pinned different key.

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
Command output is discarded. Context cancellation closes the SSH transport and
returns the context error. This does not prove termination of a remote process
that ignores disconnect or deliberately detaches; callers must report unknown
remote termination when cancellation interrupts execution. `GET /api/v1/hosts/{id}/engine` is reserved for
the container lane and returns `404` to browser callers.

## Terminals

`GET /api/v1/hosts/{id}/terminal` upgrades to WebSocket behind the root
session authentication middleware. The root server supplies its validated public origin through
`Manager.AllowedOrigin`. The Origin header must match that complete origin and
the request host must match its authority. Forwarded headers never supply trust.
When no public origin is configured, the direct request scheme and host are used. A new connection creates one SSH shell and returns
`{"type":"status","state":"connected","sessionId":"...","message":"..."}`.
The message also contains the session ID for compatibility.

Reconnect with `?session=<sessionId>` on the same host route. The backend owns
the SSH client, session, stdin, and a 256 KiB output ring in memory. Reattachment
replaces the previous socket, replays the retained output, and never starts a
second shell. Reconnecting clients should reset their visible terminal before
replay. Unknown, expired, or other-host IDs are refused. The registry permits 64
sessions and expires detached sessions after five minutes. Server restart clears
all sessions; no terminal transcript is persisted. Server shutdown must call
`Manager.Close()` to release terminal and tunnel resources.

Client frames: `{"type":"input","data":"..."}`,
`{"type":"resize","cols":80,"rows":24}`, `{"type":"reconnect"}`, and
`{"type":"close"}`. Resize dimensions range from 1 to 1000; input frames are
limited to 64 KiB. The reconnect frame on an already attached socket only repeats
the connection status. Socket loss detaches; the close frame ends the SSH shell.

Output frames have `type: "output"`, `data`, and cumulative byte `offset`.
Slow readers receive the newest retained bytes when the ring overwrites older
output. Writes to sockets have a ten-second deadline. Natural shell termination
sends `state: "exited"` with the actual `exitCode`; transport loss or explicit
close sends `state: "closed"`. Retained exit output expires after detachment.

## SFTP

| Method | Route | Request/response |
| --- | --- | --- |
| GET | `/api/v1/hosts/{id}/files?path=/` | `FileEntry[]` |
| GET | `/api/v1/hosts/{id}/files/content?path=` | `{content,hash}` |
| PUT | `/api/v1/hosts/{id}/files/content?path=` | `{content,hash}` returns `{hash}` |
| POST | `/api/v1/hosts/{id}/files/upload?path=` | raw request body returns `FileEntry` |
| GET | `/api/v1/hosts/{id}/files/download?path=` | streamed attachment |

Text writes require the current SHA-256 `hash`; a mismatch returns `409`.
Manager-originated text edits serialize the read/compare/replace operation.
Independent remote editors can still change a file between comparison and rename;
SFTP does not provide a portable conditional atomic replacement primitive.
Text reads/writes reject files above 4 MiB. Uploads reject bytes above 128 MiB,
and downloads reject a stat size above 128 MiB before streaming. Uploads and text
writes stage a unique exclusive temporary file, then use the server's POSIX
atomic rename extension. Existing files are preserved when that extension is
unavailable. Temporary files are removed on errors. Only canonical absolute POSIX
paths are accepted, with no backslashes, dot traversal, or NUL characters.

## Tunnels

| Method | Route | Request | Response |
| --- | --- | --- |
| GET | `/api/v1/tunnels` | | `Tunnel[]` |
| POST | `/api/v1/tunnels` | `{hostId,direction,listenAddress,listenPort,targetAddress,targetPort}` | `Tunnel` |
| DELETE | `/api/v1/tunnels/{id}` | | `204` |

Directions are `local` and `reverse`. Both listener directions require explicit
`127.0.0.1` or `::1`. Port zero requests an available port, returned in the record.
The server assigns IDs and limits active tunnels to 32, with at most 64 concurrent
forwarded connection pairs per tunnel. Closing a tunnel closes its listener,
forwarded connections, and SSH jump chain.
Tunnel records report lifecycle state and never expose credentials.
