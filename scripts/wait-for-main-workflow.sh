#!/usr/bin/env bash
set -euo pipefail

command -v gh >/dev/null 2>&1 || {
  echo "gh CLI is required" >&2
  exit 127
}

repo="${GH_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
run_id="$({
  gh run list \
    --repo "$repo" \
    --branch main \
    --limit 1 \
    --json databaseId \
    --jq '.[0].databaseId'
})"

if [[ -z "$run_id" ]]; then
  echo "No GitHub Actions runs found for main in $repo" >&2
  exit 1
fi

echo "Waiting for the latest main run: https://github.com/$repo/actions/runs/$run_id"
gh run watch "$run_id" --repo "$repo" --compact --exit-status
echo "Latest main run passed."
