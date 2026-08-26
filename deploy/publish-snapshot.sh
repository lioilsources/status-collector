#!/usr/bin/env bash
# Force-push the collector's snapshot JSON to the repo's `data` branch.
#
# The status page is hosted on GitHub Pages precisely so it survives this NAS
# being down — which only works if the data lives off the NAS too. The page
# reads these files whenever the live API cannot be reached.
#
# The branch is kept at exactly one commit (commit --amend + push --force):
# this runs every 15 minutes, so accumulating history would add ~35k commits a
# year of data nobody reads. SQLite on the NAS is the real history.
set -euo pipefail

SNAPSHOT_DIR=${SNAPSHOT_DIR:-/var/lib/ol1n-status/snapshot}
WORK_DIR=${WORK_DIR:-/var/lib/ol1n-status/publish}
REPO=${REPO:-git@github.com:lioilsources/status-collector.git}
BRANCH=${BRANCH:-data}
SSH_KEY=${SSH_KEY:-/var/lib/ol1n-status/deploy_key}
GIT_AUTHOR=${GIT_AUTHOR:-ol1n-status collector}
GIT_EMAIL=${GIT_EMAIL:-ol1n-status@users.noreply.github.com}

if [ ! -d "$SNAPSHOT_DIR" ]; then
    echo "snapshot dir $SNAPSHOT_DIR does not exist — is the collector running with -snapshot-dir?" >&2
    exit 1
fi
if ! compgen -G "$SNAPSHOT_DIR/*.json" > /dev/null; then
    echo "no JSON in $SNAPSHOT_DIR yet — nothing to publish" >&2
    exit 0
fi
if [ ! -f "$SSH_KEY" ]; then
    echo "deploy key $SSH_KEY missing; see README (Deploy keys, write access)" >&2
    exit 1
fi

export GIT_SSH_COMMAND="ssh -i $SSH_KEY -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

if [ ! -d "$WORK_DIR/.git" ]; then
    rm -rf "$WORK_DIR"
    mkdir -p "$WORK_DIR"
    git init -q "$WORK_DIR"
    git -C "$WORK_DIR" remote add origin "$REPO"
    git -C "$WORK_DIR" checkout -q --orphan "$BRANCH"
fi

git -C "$WORK_DIR" config user.name  "$GIT_AUTHOR"
git -C "$WORK_DIR" config user.email "$GIT_EMAIL"

cp "$SNAPSHOT_DIR"/*.json "$WORK_DIR/"
git -C "$WORK_DIR" add -A

if git -C "$WORK_DIR" diff --cached --quiet; then
    echo "snapshot unchanged, nothing to push"
    exit 0
fi

MSG="snapshot $(date -u +%Y-%m-%dT%H:%M:%SZ)"
if git -C "$WORK_DIR" rev-parse -q --verify HEAD >/dev/null 2>&1; then
    git -C "$WORK_DIR" commit -q --amend -m "$MSG"
else
    git -C "$WORK_DIR" commit -q -m "$MSG"
fi

git -C "$WORK_DIR" push -q --force origin "$BRANCH"
echo "published $MSG to $BRANCH"
