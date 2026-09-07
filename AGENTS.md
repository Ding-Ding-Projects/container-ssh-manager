# Project implementation guidance

Container SSH Manager is a single-owner, private-network Linux container service. Public source must contain no real credentials, private host inventory, or user-specific deployment data.

## Build and checks

- Build frontend assets before compiling the Go command: `npm ci --prefix web`, `npm run build --prefix web`, then `go test ./...` and `go build ./cmd/manager`.
- Use `GOMAXPROCS=2 go test -p 1 ./...` on resource-constrained hosts. Tests must not depend on a user's saved SSH hosts or credentials.
- Compose builds supply Node and Go inside builder stages. `build.sh` and `build.bat` are the supported build entry points.
- Container integration fixtures require explicit opt-in and exact fixture labels. Never prune shared engines or remove unrelated resources.

## Ownership and architecture

- `internal/core`: SQLite records, encrypted vault, JSON boundaries.
- `internal/auth`: single owner, session cookies, origin and trusted-proxy boundaries.
- `internal/connection`: verified SSH, files, terminals, tunnels and engine transports.
- `internal/engine`: controlled container and Compose administration.
- `internal/jobs`: immutable accepted command intent, schedules and execution lifecycle.
- `web`: production TypeScript interface; `web/assets.go` embeds only generated production output.

Keep documented request shapes compatible across backend and frontend. A successful compilation does not establish working browser controls, remote execution or deployment. Preserve explicit unknown outcomes after interrupted remote mutations.
