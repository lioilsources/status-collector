#!/usr/bin/env bash
# Install the collector, its publish script and the systemd units.
#
# Works from two layouts: a release tarball (binary + systemd/ beside this
# script) and a repo checkout (deploy/ holds the units, binary passed as $1).
set -euo pipefail

BINARY_SRC=${1:-}
BINARY=/usr/local/bin/status-collector
PUBLISH=/usr/local/bin/ol1n-status-publish
DATA_DIR=/var/lib/ol1n-status
SNAPSHOT_DIR=$DATA_DIR/snapshot
SERVICE=ol1n-status
USER=${OL1N_USER:-ol1n}

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Units live in systemd/ in a tarball and next to this script in the repo.
if [ -d "$here/systemd" ]; then
    UNIT_DIR="$here/systemd"
else
    UNIT_DIR="$here"
fi

if [ -z "$BINARY_SRC" ]; then
    if [ -x "$here/status-collector" ]; then
        BINARY_SRC="$here/status-collector"
    else
        echo "usage: $0 <path-to-status-collector>   (or run it from a release tarball)" >&2
        exit 1
    fi
fi

if ! id -u "$USER" >/dev/null 2>&1; then
    echo "service user '$USER' does not exist; create it or set OL1N_USER" >&2
    exit 1
fi

echo "==> installing binaries"
sudo install -m 0755 "$BINARY_SRC" "$BINARY"
sudo install -m 0755 "$here/publish-snapshot.sh" "$PUBLISH"

echo "==> data directories"
sudo mkdir -p "$DATA_DIR" "$SNAPSHOT_DIR"
sudo chown -R "$USER:$USER" "$DATA_DIR"

echo "==> systemd units (running as $USER)"
for unit in ol1n-status.service ol1n-status-snapshot.service ol1n-status-snapshot.timer; do
    # The units ship with the default user baked in; rewrite it on the way in so
    # OL1N_USER actually takes effect. Without this the data directory would be
    # chowned to one user while systemd ran the service as another.
    sed -e "s/^User=.*/User=$USER/" -e "s/^Group=.*/Group=$USER/" "$UNIT_DIR/$unit" \
        | sudo tee "/etc/systemd/system/$unit" > /dev/null
done
sudo systemctl daemon-reload
sudo systemctl enable --now "$SERVICE"
sudo systemctl restart "$SERVICE"
sudo systemctl enable --now ol1n-status-snapshot.timer

echo "==> status"
"$BINARY" -h 2>&1 | head -1 || true
sudo systemctl status "$SERVICE" --no-pager -l | head -12
