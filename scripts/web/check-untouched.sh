#!/bin/sh
# Fail when this branch edits loyal-app's files under web/.
#
# loyal-app writes web/ (scripts/web/sync.sh merges it here); our own code
# there is web/apps/loyal only. Sync merges bring loyal-app's commits in on
# their second parent, so walking first parents skips them.
#
# Usage: scripts/web/check-untouched.sh [base]   (default: origin/main)
set -eu

base=${1:-origin/main}
touched=$(git log --first-parent --no-merges --format= --name-only "$(git merge-base "$base" HEAD)..HEAD" \
	-- web ':(exclude)web/apps/loyal' | sort -u)
if [ -n "$touched" ]; then
	echo "these loyal-app files changed here; change them in loyal-app and run scripts/web/sync.sh:" >&2
	echo "$touched" >&2
	exit 1
fi
