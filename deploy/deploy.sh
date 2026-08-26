#!/usr/bin/env bash
# Build from source on this machine, then install.
#
# Needs Go and gcc here. If you would rather not have a toolchain on the NAS,
# use a release instead — see README, "Instalace z release".
#
# The frontend is NOT deployed from here: pushing to main deploys web/ to
# GitHub Pages via .github/workflows/pages.yml.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)

echo "==> building"
cd "$repo"
CGO_ENABLED=1 go build -ldflags="-s -w" -o /tmp/status-collector ./cmd/status-collector

exec "$here/install.sh" /tmp/status-collector
