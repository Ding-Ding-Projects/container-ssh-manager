# Engine API

Routes are authenticated same-origin JSON APIs. Responses are direct objects or
arrays; failures use `{ "error": "..." }`. `hostId` selects a stored connection,
never a browser-supplied endpoint. The API follows the [Docker Engine reference](https://docs.docker.com/reference/api/engine/version/v1.45/).

## Containers

| Method | Route | Request |
| --- | --- | --- |
| `GET` | `/api/v1/engine/containers?hostId=&all=` | list Docker summaries |
| `GET` | `/api/v1/engine/containers/{id}?hostId=` | inspect object |
| `POST` | `/api/v1/engine/containers` | `{hostId,name?,config,hostConfig?,networkingConfig?}` |
| `POST` | `/api/v1/engine/containers/{id}/start` | `{hostId}` |
| `POST` | `/api/v1/engine/containers/{id}/stop` | `{hostId,timeoutSeconds?}` |
| `POST` | `/api/v1/engine/containers/{id}/restart` | `{hostId,timeoutSeconds?}` |
| `POST` | `/api/v1/engine/containers/{id}/recreate` | `{hostId}` |
| `DELETE` | `/api/v1/engine/containers/{id}?hostId=&force=&volumes=` | remove |
| `GET` | `/api/v1/engine/containers/{id}/logs?hostId=&stdout=&stderr=&tail=&timestamps=` | decoded text stream |
| `GET` | `/api/v1/engine/containers/{id}/stats?hostId=&stream=` | Docker stats JSON or stream |
| `POST` | `/api/v1/engine/containers/{id}/exec` | `{hostId,cmd,env?,workingDir?,user?,tty?}` |

Container creation flattens `config` into the Docker create payload, translates
`hostConfig` to `HostConfig`, and preserves
`networkingConfig.EndpointsConfig`. Native Docker create fields at the top level
are also accepted. The facade moves the name and stop/restart timeout into the
Docker query parameters. Non-TTY logs and exec output are demultiplexed from
Docker's framed stdout/stderr stream. Exec returns `{id,exitCode,output}`.

Recreate serializes replacements within the service and uses unique temporary
names. It inspects the original, creates the replacement, stops a running original
to release published ports, renames the original to a unique backup name, assigns
the original name to the replacement, and starts it. A configured health check
must become healthy within the two-minute operation deadline. Without a health
check, successful start plus a subsequent running-state inspection is the
readiness criterion, not a promise of application-level health.

Only after readiness succeeds does the service delete the stopped original.
Failure attempts a bounded independent rollback: stop the replacement, restore
names, restart the original when it had been running, verify its state, and remove
the replacement. If recovery cannot be verified, both identities are reported for
inspection. Interrupted requests never claim successful recreation.

Every recreation deletion explicitly uses `v=false`. Named and anonymous volume
mounts are rebound to their existing volumes, and currently published port
assignments are preserved. No recreation route deletes user volumes. Auto-remove,
paused, restarting, inherited `VolumesFrom`, container-shared networking, and explicit static network
assignments are rejected before stopping the original because this replacement
strategy cannot safely preserve their rollback semantics. Pull an image explicitly
before recreation when an updated image is wanted.

Recreate returns `{id,operationId,warnings}` on success. Recovery failures include
`originalId`, `replacementId`, `operationId`, and, where attempted, `rollback`.
A service restart preserves these resource identities in an `unknown` operation;
it does not replay a potentially completed mutation.

## Images and durable operations

| Method | Route | Request |
| --- | --- | --- |
| `GET` | `/api/v1/engine/images?hostId=&all=` | list images |
| `POST` | `/api/v1/engine/images/pull` | `{hostId,reference,platform?,timeoutSeconds?}` |
| `POST` | `/api/v1/engine/images/build` | `{hostId,contextPath,dockerfile?,tags?,buildArgs?,timeoutSeconds?}` |
| `POST` | `/api/v1/engine/images/{id}/tag` | `{hostId,repository,tag?}` |
| `DELETE` | `/api/v1/engine/images/{id}?hostId=&force=&noprune=` | remove image |

Pull and build return HTTP 202 with a persisted operation record before execution.
The timeout defaults to 1,800 seconds and accepts 1 through 3,600 seconds. Poll
`GET /api/v1/engine/operations/{id}` and request cancellation using
`POST /api/v1/engine/operations/{id}/cancel`.
`GET /api/v1/engine/operations?hostId=` lists persisted history, including active
Compose/recreate records and unknown outcomes recovered after a restart.

| State | Meaning |
| --- | --- |
| `running` | durable record created; operation is active |
| `canceling` | cancellation persisted before interrupting execution |
| `completed` | command exited successfully or pull stream ended without a daemon error |
| `failed` | daemon error event, command nonzero exit, or verified rollback |
| `canceled` | local execution interrupted by cancellation; daemon outcome is `unknown` |
| `timed_out` | deadline expired; daemon outcome is `unknown` |
| `unknown` | connection/stream interrupted, persistence recovery, or incomplete rollback |

Records include `createdAt`, `updatedAt`, `deadline`, and `outcome`. They store only
operation metadata and fixed diagnostic messages, not build arguments, environment
content, command output, or daemon error payloads. An HTTP 200 pull response is
not success by itself: the complete JSON stream must be consumed and checked for
`error` and `errorDetail` events. A late completion cannot overwrite a persisted
cancellation. On service startup, interrupted running/canceling records become
`unknown`, and no operation is automatically replayed. Inspect daemon state before
retrying an unknown mutation. Storage recovery errors prevent new operations.

Local build and Compose commands use direct argv execution. Remote commands use
shell-quoted operands over the selected SSH connection. Cancellation terminates
the local command or closes its SSH session; it cannot establish that the daemon
has stopped work. Absolute build context and Dockerfile paths are required.

## Volumes and networks

| Method | Route | Request |
| --- | --- | --- |
| `GET` | `/api/v1/engine/volumes?hostId=` | list volumes |
| `POST` | `/api/v1/engine/volumes` | `{hostId,name?,driver?,driverOpts?,labels?}` |
| `GET` | `/api/v1/engine/volumes/{name}?hostId=` | inspect volume |
| `DELETE` | `/api/v1/engine/volumes/{name}?hostId=&force=` | remove volume |
| `GET` | `/api/v1/engine/networks?hostId=` | list networks |
| `POST` | `/api/v1/engine/networks` | `{hostId,name,driver?,internal?,attachable?,labels?,options?}` |
| `GET` | `/api/v1/engine/networks/{id}?hostId=` | inspect network |
| `POST` | `/api/v1/engine/networks/{id}/connect` | `{hostId,container,endpointConfig?}` |
| `POST` | `/api/v1/engine/networks/{id}/disconnect` | `{hostId,container,force?}` |
| `DELETE` | `/api/v1/engine/networks/{id}?hostId=` | remove network |

## Compose projects

| Method | Route | Request |
| --- | --- | --- |
| `GET` | `/api/v1/engine/compose/projects` | stored metadata records |
| `POST` | `/api/v1/engine/compose/projects` | `{hostId,name,path,adopt?}` |
| `GET` | `/api/v1/engine/compose/projects/{id}` | project/revision metadata without sealed content |
| `GET` | `/api/v1/engine/compose/projects/{id}/files` | latest saved `{compose,environment}` for the authenticated editor |
| `PUT` | `/api/v1/engine/compose/projects/{id}/files` | `{compose,environment?}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/validate` | `{}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/deploy` | `{pull?,build?,detach?}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/stop` | `{}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/down` | `{}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/restore/{revision}` | `{}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/recover` | restore the journaled original file pair after an incomplete write |

Compose paths identify existing absolute host directories; remote paths use POSIX
semantics even when the manager runs on Windows. Revisions retain sealed YAML and
environment data in storage. Metadata responses omit the sealed value, while the
explicit authenticated editor route returns the latest saved plaintext with
`Cache-Control: no-store`. With `adopt:true`, registration reads `compose.yaml`
(falling back to `compose.yml`) and optional `.env` into encrypted revision 1.
Adoption does not write host files or issue daemon commands. Missing, unreadable,
nonregular, or oversized files are rejected. The selected `fileName` is retained
for later save/restore/deploy, so adopting `compose.yml` does not create a competing
`compose.yaml`. Without adoption or a saved revision, `files` returns 404.

Writing an empty environment clears the previous `.env`. Revision restores write
both files back. Deployment is detached; `detach:false` is rejected rather than
holding an unbounded foreground process. Validate/deploy/stop/down preserve the
synchronous `{ok:true,operationId}` response and also persist operation state.
Client disconnection cancels execution. Compose operations have a 30-minute bound;
`down` does not remove volumes or images.

Before a file update, the service persists an encrypted, project-bound write
intent holding the exact existing file pair and intended replacement. Only after
both writes and revision persistence succeed is that intent cleared. Failure
attempts bounded compensation and verifies the original file pair. A pending
intent survives process restart and blocks edits, restores, and lifecycle commands
until `recover` succeeds. Public metadata exposes only `pending.state` as
`recovery_required`, not the sealed intent or file contents. Recovery refuses to
overwrite files that differ from both the recorded original and intended content.
It removes a file only when the journal proves this attempt created it.

The two filesystem renames are not an operating-system-wide transaction. The
journal prevents this service from deploying an unresolved pair; it cannot lock
out an independent host administrator or another Compose process. Remote
replacement requires negotiated atomic SFTP rename support, and servers lacking
that capability reject replacement rather than deleting the original file.

The constructor is `engine.New(store, connections, vault)`. The vault is mandatory
because Compose revisions can contain an environment file. The local Engine
transport must be supported by the connection manager on the running platform.

## Verification

`go test ./internal/engine` covers wire-shape translation, pull-stream errors,
operation cancellation/deadlines/restart recovery, volume-preserving recreate
payloads, adoption, sealed-revision persistence/public projection, second-file
write compensation, persistence-failure recovery, and preservation of independent
host edits. On Linux, set
`CSM_RUN_DOCKER_INTEGRATION=1` and run `go test ./internal/engine -run TestDockerFacadeIntegration`
with a local Docker Unix socket and Docker CLI available. The opt-in integration
fixture owns uniquely labelled disposable resources, uses bounded cleanup, and
never prunes the daemon or removes unrelated workloads.
