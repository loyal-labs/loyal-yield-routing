#!/bin/sh
# Merge loyal-app into web/, with its history.
#
# loyal-app stays the writer of everything under web/ until Vercel builds from
# this repo; this script is how its commits arrive here. Every path is moved
# under web/ in every commit before the merge, and filtering is deterministic,
# so a re-run reproduces the commits already merged and brings in only new ones.
#
# Usage: scripts/web/sync.sh [ref]   (default: main)
set -eu

root=$(git rev-parse --show-toplevel)
ref=${1:-main}
url=git@github.com:loyal-labs/loyal-app.git
dir=${LOYAL_SYNC_DIR:-$HOME/.loyal/sync}/loyal-app

rm -rf "$dir"
mkdir -p "$(dirname "$dir")"
git clone -q --single-branch --no-tags --branch "$ref" "$url" "$dir"
sha=$(git -C "$dir" rev-parse HEAD)
git -C "$dir" filter-repo --quiet --force --path-rename :web/
git -C "$root" fetch -q --no-tags "$dir" "+HEAD:refs/import/loyal-app"
if git -C "$root" merge-base --is-ancestor refs/import/loyal-app HEAD; then
	echo "loyal-app $ref@$sha: already merged"
	exit 0
fi
msg="chore(web): sync loyal-app $ref@$sha"
if ! git -C "$root" merge -q --no-edit --allow-unrelated-histories -m "$msg" refs/import/loyal-app; then
	# web/apps/loyal is the one workspace member loyal-app does not have, so
	# web/bun.lock is the one file both sides write: take loyal-app's and
	# let bun add the member back. Any other conflict is for a human.
	conflicts=$(git -C "$root" diff --name-only --diff-filter=U)
	[ "$conflicts" = "web/bun.lock" ] || { echo "sync: unexpected conflicts:" >&2; echo "$conflicts" >&2; exit 1; }
	git -C "$root" checkout --theirs web/bun.lock
	(cd "$root/web" && bun install >/dev/null)
	git -C "$root" add web/bun.lock
	git -C "$root" commit -q --no-edit -m "$msg"
fi
echo "loyal-app $ref@$sha -> $(git -C "$root" rev-parse refs/import/loyal-app)"
