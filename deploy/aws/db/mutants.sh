#!/usr/bin/env bash
# Breaks each query of a check in turn and asserts the check stops: a non-zero exit with pgmove's
# "a query failed" line, never a DIFF count or an ok. For scratch pairs: every check runs once per query.
# Usage: mutants.sh <cluster> '<check>'...
#   a check is a pgmove subcommand with its arguments ('verify', 'writes diff freeze src') or
#   'cutover <step>'; PGMOVE_WORK, PGMOVE_DEST and the rest of the pair's environment pass through.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
c=${1:?usage: mutants.sh <cluster> '<check>'...}
shift
count=$(mktemp)
out=$(mktemp)
trap 'rm -f "$count" "$out"; rmdir "$count.lock" 2>/dev/null || true' EXIT

check() {
  if [[ $1 == cutover ]]; then
    shift
    "$HERE/cutover.sh" "$c" "$@"
  else
    local sub=$1
    shift
    "$HERE/pgmove.sh" "$sub" "$c" "$@"
  fi
}

total=0 closed=0 empty=0
for spec in "$@"; do
  read -ra a <<<"$spec"
  : >"$count"
  rc=0
  PGMOVE_MUTATE_AT=0 PGMOVE_MUTATE_COUNT=$count check "${a[@]}" >"$out" 2>&1 || rc=$?
  k=$(cat "$count")
  if [[ -z $k || $k == 0 ]]; then
    echo "mutants: $spec ran no query (exit $rc): $(tail -1 "$out")"
    empty=$((empty + 1))
    continue
  fi
  open=0
  for ((n = 1; n <= k; n++)); do
    : >"$count"
    rc=0
    PGMOVE_MUTATE_AT=$n PGMOVE_MUTATE_COUNT=$count check "${a[@]}" >"$out" 2>&1 || rc=$?
    total=$((total + 1))
    if ((rc != 0)) && grep -q 'stopped: a query failed' "$out"; then
      closed=$((closed + 1))
    else
      open=$((open + 1))
      echo "mutants: OPEN $spec, query $n of $k broken: exit $rc, last line: $(tail -1 "$out")"
    fi
  done
  echo "mutants: $spec: $k queries broken one at a time, $((k - open)) stopped the check"
done
echo "mutants: $total checks, $closed fail closed$( ((empty == 0)) || echo ", $empty ran no query")"
((total == closed && empty == 0))
