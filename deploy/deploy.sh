#!/usr/bin/env bash
# Build from source on this machine, then install.
#
# Needs Go here — but not a C compiler, and not on the NAS: SQLite is pure Go,
# so `make build-linux` on any machine produces a binary the NAS can run.
# Easiest of all is a release; see README, "Instalace z release".
#
# The frontend is NOT deployed from here: pushing to main deploys web/ to
# GitHub Pages via .github/workflows/pages.yml.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)

echo "==> building"
cd "$repo"
CGO_ENABLED=0 go build -ldflags="-s -w" -o /tmp/status-collector ./cmd/status-collector

exec "$here/install.sh" /tmp/status-collector
