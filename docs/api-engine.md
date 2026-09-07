# Engine API

All routes are authenticated same-origin JSON APIs. Responses are direct objects or
arrays and errors are `{ "error": "..." }`. `hostId` selects a stored connection,
never a browser-supplied endpoint.

## Containers

| Method | Route | Request |
| --- | --- | --- |
| `GET` | `/api/v1/engine/containers?hostId=&all=` | list Docker summaries |
| `GET` | `/api/v1/engine/containers/{id}?hostId=` | inspect object |
| `POST` | `/api/v1/engine/containers` | `{hostId,name?,config,hostConfig?,networkingConfig?}` |
| `POST` | `/api/v1/engine/containers/{id}/start` | `{hostId}` |
| `POST` | `/api/v1/engine/containers/{id}/stop` | `{hostId,timeoutSeconds?}` |
| `POST` | `/api/v1/engine/containers/{id}/restart` | `{hostId,timeoutSeconds?}` |
| `POST` | `/api/v1/engine/containers/{id}/recreate` | `{hostId,pull?}` |
| `DELETE` | `/api/v1/engine/containers/{id}?hostId=&force=&volumes=` | remove |
| `GET` | `/api/v1/engine/containers/{id}/logs?hostId=&stdout=&stderr=&tail=&timestamps=` | `text/plain` stream |
| `GET` | `/api/v1/engine/containers/{id}/stats?hostId=&stream=` | Docker stats JSON or stream |
| `POST` | `/api/v1/engine/containers/{id}/exec` | `{hostId,cmd,env?,workingDir?,user?,tty?}` |

Recreate uses inspect data, creates the replacement before deleting the stopped
original, then starts the replacement only if the original was running.

## Images

| Method | Route | Request |
| --- | --- | --- |
| `GET` | `/api/v1/engine/images?hostId=&all=` | list images |
| `POST` | `/api/v1/engine/images/pull` | `{hostId,reference,platform?}` |
| `POST` | `/api/v1/engine/images/build` | `{hostId,contextPath,dockerfile?,tags?,buildArgs?}` |
| `POST` | `/api/v1/engine/images/{id}/tag` | `{hostId,repository,tag?}` |
| `DELETE` | `/api/v1/engine/images/{id}?hostId=&force=&noprune=` | remove image |

Pull and build return durable operation records. Poll
`GET /api/v1/engine/operations/{id}` and cancel with
`POST /api/v1/engine/operations/{id}/cancel`. Interruption while a daemon may still
change state is reported as `unknown`, never success.

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
| `GET` | `/api/v1/engine/compose/projects` | stored records |
| `POST` | `/api/v1/engine/compose/projects` | `{hostId,name,path,adopt?}` |
| `GET` | `/api/v1/engine/compose/projects/{id}` | project and revisions |
| `PUT` | `/api/v1/engine/compose/projects/{id}/files` | `{compose,environment?}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/validate` | `{}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/deploy` | `{pull?,build?,detach?}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/stop` | `{}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/down` | `{volumes?,images?}` |
| `POST` | `/api/v1/engine/compose/projects/{id}/restore/{revision}` | `{}` |

Compose paths are validated host-side absolute paths. The engine shell-quotes each
path as one operand and does not accept a free-form command. Revisions are sealed
before persistence because environment files may contain secrets. Plaintext
environment data never appears in logs or operation output.

The integration constructor is `engine.New(store, connections, vault)`. The vault
is mandatory because Compose revisions can include an environment file.
