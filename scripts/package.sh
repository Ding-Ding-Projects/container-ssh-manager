#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
: "${VERSION:?Set VERSION to the unique release version}"
: "${UPDATED_AT:?Set UPDATED_AT to the recorded build start timestamp}"
test -d web/dist
mkdir -p work/release/bin work/release/deploy work/release/docs
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -p 2 -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.updatedAt=$UPDATED_AT" -o "work/release/bin/manager-linux-$arch" ./cmd/manager
done
cp compose.yaml compose.prebuilt.yaml Dockerfile.prebuilt work/release/
cp deploy/Caddyfile work/release/deploy/
cp docs/deployment.md docs/api-*.md docs/openapi.json work/release/docs/
printf 'VERSION=%s\nUPDATED_AT=%s\n' "$VERSION" "$UPDATED_AT" > work/release/.env.example
printf 'Container SSH Manager %s\nSource: %s\nBuild started: %s\n\nSet VERSION in .env from .env.example. Build with docker compose -f compose.yaml -f compose.prebuilt.yaml build. Complete docs/deployment.md owner and key setup, then start with the same compose files. Both Linux architectures are bundled. No tests are run by the release workflow.\n' "$VERSION" "$(git rev-parse HEAD)" "$UPDATED_AT" > work/release/README.txt
tar -czf "work/container-ssh-manager-$VERSION.tar.gz" -C work/release .
sha256sum "work/container-ssh-manager-$VERSION.tar.gz" > "work/container-ssh-manager-$VERSION.sha256"
