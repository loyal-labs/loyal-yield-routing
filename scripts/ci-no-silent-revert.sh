#!/usr/bin/env bash
# Fails a pull request that sets a file to content the base branch already had at an
# older commit while the base tip has moved on, the signature of an old tree squashed over main.
# Usage: scripts/ci-no-silent-revert.sh <base-ref> <head-ref>   (PR_BODY env carries `revert-ok: <glob> <reason>` lines)
set -euo pipefail
base=${1:?usage: scripts/ci-no-silent-revert.sh <base-ref> <head-ref>}
head=${2:?usage: scripts/ci-no-silent-revert.sh <base-ref> <head-ref>}
cd "$(git rev-parse --show-toplevel)"

mb=$(git merge-base "$base" "$head")
globs=() reasons=()
while IFS= read -r line; do
  line=${line%$'\r'}
  if [[ $line =~ ^revert-ok:[[:space:]]+([^[:space:]]+)[[:space:]]+(.+)$ ]]; then
    globs+=("${BASH_REMATCH[1]}")
    reasons+=("${BASH_REMATCH[2]}")
  fi
done <<<"${PR_BODY:-}"

short() { git log -1 --format='%h %s' "$1"; }
failed=0
allowed=()

while IFS= read -r -d '' path; do
  head_blob=$(git rev-parse -q --verify "$head:$path" 2>/dev/null) || continue
  tip_blob=$(git rev-parse -q --verify "$base:$path" 2>/dev/null) || continue
  [[ $head_blob != "$tip_blob" ]] || continue
  undone=()
  restored=
  while read -r commit; do
    blob=$(git rev-parse -q --verify "$commit:$path" 2>/dev/null) || blob=
    if [[ $blob == "$head_blob" ]]; then
      restored=$commit
      break
    fi
    undone+=("$commit")
  done < <(git log --format=%H "$base" -- "$path")
  [[ -n $restored && ${#undone[@]} -gt 0 ]] || continue

  ok=
  for i in "${!globs[@]}"; do
    # shellcheck disable=SC2053
    if [[ $path == ${globs[$i]} ]]; then ok="${globs[$i]} ${reasons[$i]}"; break; fi
  done
  if [[ -n $ok ]]; then
    allowed+=("$path  allowed by revert-ok: $ok")
    continue
  fi

  failed=1
  echo "SILENT REVERT: $path"
  echo "  the pull request sets it to the content it had at: $(short "$restored")"
  echo "  newer commits on $base that this undoes:"
  for c in "${undone[@]}"; do echo "    $(short "$c")"; done
done < <(git diff --name-only --no-renames -z "$mb" "$head")

for line in "${allowed[@]+"${allowed[@]}"}"; do echo "revert-ok used: $line"; done

if ((failed)); then
  echo
  echo "Fix: merge origin/main into the branch and take main's version of these files; never squash or rebase an old tree over main."
  echo "A deliberate revert passes with a line in the PR body: revert-ok: <path or glob> <reason>"
  exit 1
fi
echo "no silent revert against $base"
