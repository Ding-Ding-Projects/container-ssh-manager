#!/bin/sh
set -eu
cd "$(dirname "$0")"
export VERSION="0.1.0-$(git rev-parse --short=12 HEAD)"
export UPDATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
mkdir -p work
printf '{"version":"%s","sourceRevision":"%s","buildStartedAt":"%s","status":"building"}\n' "$VERSION" "$(git rev-parse HEAD)" "$UPDATED_AT" > work/build-receipt.json
docker compose build --pull
printf '{"version":"%s","sourceRevision":"%s","buildStartedAt":"%s","status":"built"}\n' "$VERSION" "$(git rev-parse HEAD)" "$UPDATED_AT" > work/build-receipt.json
case "${1:-}" in --run|/run) docker compose up -d;; esac
