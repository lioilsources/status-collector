#!/usr/bin/env bash
# Build and install the collector on the NAS.
#
# The frontend is NOT deployed from here any more — pushing to main deploys
# web/ to GitHub Pages via .github/workflows/pages.yml.
set -euo pipefail

BINARY=/usr/local/bin/status-collector
PUBLISH=/usr/local/bin/ol1n-status-publish
DATA_DIR=/var/lib/ol1n-status
SNAPSHOT_DIR=$DATA_DIR/snapshot
SERVICE=ol1n-status

echo "==> building"
CGO_ENABLED=1 go build -ldflags="-s -w" -o /tmp/status-collector ./cmd/status-collector

echo "==> installing binaries"
sudo install -m 0755 /tmp/status-collector "$BINARY"
sudo install -m 0755 deploy/publish-snapshot.sh "$PUBLISH"

echo "==> data directories"
sudo mkdir -p "$DATA_DIR" "$SNAPSHOT_DIR"
sudo chown -R ol1n:ol1n "$DATA_DIR"

echo "==> systemd units"
sudo cp deploy/ol1n-status.service /etc/systemd/system/
sudo cp deploy/ol1n-status-snapshot.service /etc/systemd/system/
sudo cp deploy/ol1n-status-snapshot.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now "$SERVICE"
sudo systemctl restart "$SERVICE"
sudo systemctl enable --now ol1n-status-snapshot.timer

echo "==> status"
sudo systemctl status "$SERVICE" --no-pager -l | head -20
systemctl list-timers ol1n-status-snapshot.timer --no-pager | head -3
