#!/bin/sh
set -eu
cd "$(dirname "$0")"
export VERSION="0.1.0-$(git rev-parse --short=12 HEAD)"
export UPDATED_AT="$(git show -s --format=%cI HEAD)"
docker compose build --pull
case "${1:-}" in --run|/run) docker compose up -d;; esac
