#!/usr/bin/env bash
# deploy.sh — build and deploy ol1n-status-collector na NAS
# Spouštěj z rootu repozitáře: ./deploy/deploy.sh
set -euo pipefail

BINARY=/usr/local/bin/status-collector
DATA_DIR=/var/lib/ol1n-status
WEB_DIR=$DATA_DIR/web
SERVICE=ol1n-status

echo "==> Building status-collector…"
CGO_ENABLED=1 go build -ldflags="-s -w" -o /tmp/status-collector ./cmd/status-collector

echo "==> Installing binary…"
sudo install -m 0755 /tmp/status-collector "$BINARY"

echo "==> Creating data directory…"
sudo mkdir -p "$DATA_DIR" "$WEB_DIR"
sudo chown -R ol1n:ol1n "$DATA_DIR"

echo "==> Deploying web assets…"
sudo cp web/index.html "$WEB_DIR/index.html"
sudo chown ol1n:ol1n "$WEB_DIR/index.html"

echo "==> Installing systemd service…"
sudo cp deploy/ol1n-status.service /etc/systemd/system/

echo "==> Restarting service…"
sudo systemctl daemon-reload
sudo systemctl enable --now "$SERVICE"
sudo systemctl restart "$SERVICE"

echo "==> Status:"
sudo systemctl status "$SERVICE" --no-pager -l

echo ""
echo "✓ API dostupné na http://127.0.0.1:8765/api/status"
echo "  Přidej Caddyfile.snippet do /etc/caddy/Caddyfile a reload Caddy."
