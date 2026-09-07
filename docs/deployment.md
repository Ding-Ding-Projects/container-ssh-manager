# Deployment and recovery

This product targets Linux Docker hosts on a private network. It is not a Windows desktop installer. The portable stack contains the Go service and Caddy HTTPS proxy; no source-hosted preview can replace the service's local socket or SSH transports.

## First installation

1. Clone the repository and run `sh build.sh` (or `build.bat` on a machine with Docker Desktop configured for Linux containers). Docker builds supply the pinned Node, Go and CLI toolchains; the host does not need them separately.
2. Create a private `.env` file with `BIND_ADDRESS` set to the selected LAN address, `MANAGER_HOST` set to the same address or a private DNS name, and `HTTPS_PORT=8443`. Defaults bind only loopback. Do not open a router port or public tunnel.
3. Create the vault key with `mkdir -p secrets && chmod 700 secrets` followed by `docker compose --profile setup run --rm init`. Set `VERSION` to the version printed by the build, either exported or saved in `.env`. The setup service mounts the secrets directory writable once; the running service mounts only the completed key read-only.
4. Create the owner using the bootstrap command below. Keep the password out of command arguments, shell history and environment variables.
5. Run `docker compose up -d`. Read `docker compose ps` and the health result before using the URL.
6. Trust the project-specific Caddy local CA on your own client through the operating system's normal certificate-import flow. This private CA is not a public identity guarantee. Do not disable TLS verification globally.

Interactive owner bootstrap on a Linux host, using a hidden prompt and standard input:

```sh
python3 - <<'PY'
import getpass, subprocess
password = getpass.getpass('New owner password: ')
confirmation = getpass.getpass('Repeat owner password: ')
if password != confirmation:
    raise SystemExit('Passwords differ')
subprocess.run(['docker','compose','run','--rm','-T','manager','bootstrap'], input=(password+'\n').encode(), check=True)
PY
```

Bootstrap refuses to replace an existing owner. The data directory and vault key must remain protected. The mounted engine socket grants broad host control; authentication to this service therefore represents administrative authority. The stack is not privileged and mounts no unrelated host directories. Local Compose projects live under `/projects`; host bind-mount paths inside managed Compose files refer to the engine host, not the manager container.

## Backup and restore

`manager backup DESTINATION` produces a consistent SQLite snapshot and refuses an existing destination. With the running stack: `docker compose exec manager manager backup /data/backup-YYYYMMDD.db`, then copy that exact file to protected backup storage. It contains encrypted saved credentials and may contain sensitive command definitions. It is not a workload or volume backup.

Preserve `secrets/vault.key` separately. A database backup without its original key cannot recover encrypted credentials or Compose revisions. Preserve Caddy state separately when keeping the same private CA is desired.

Restore only into a stopped, isolated stack: retain the existing database, place the chosen snapshot at `/data/manager.db`, mount the original vault key, then start and verify. Never replace a running database or copy stale WAL files over it. Pending work becomes unknown and is not replayed. Re-enable any operation only after reviewing its last outcome.

## Updates and rollback

Build or load the exact new image tag, back up the database, and use `docker compose up -d`. Inspect health, version provenance, and a read-only host listing. Retain the previous image and pre-update database. Rollback means stopping this project and restoring the compatible previous image/database pair, never pruning the host. Automatic desktop-style restart installation does not apply to this container service; updates use explicit Compose replacement.

## Runtime boundaries

Schedules execute without a browser after explicit creation/enabling. A network interruption can leave a remote command running; unknown means unknown, not cancelled. Terminal sessions cannot survive service restart. Default command output and terminal transcripts are not persisted. Application data requires a separate workload-aware backup plan.
