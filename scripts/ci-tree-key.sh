#!/usr/bin/env bash
# Prints the artifact name a green Workers v2 verification uploads for a commit.
# It hashes the git objects at that commit's push paths in the workflow, so
# commits that leave those inputs alone share one name.
set -euo pipefail
commit=${1:?usage: scripts/ci-tree-key.sh <commit>}
cd "$(git rev-parse --show-toplevel)"
paths=$(git show "$commit:.github/workflows/workers-v2-verify.yml" |
  sed -n '/^  push:$/,/^  pull_request:$/s|^      - \(.*\)$|\1|p' | sed 's|/\*\*$||')
if [[ -z $paths ]]; then
  echo "no push paths in .github/workflows/workers-v2-verify.yml at $commit" >&2
  exit 1
fi
# shellcheck disable=SC2086
echo "workers-v2-verified-$(git ls-tree "$commit" -- $paths | git hash-object --stdin)"
