#!/usr/bin/env bash
# Copies the ClickStack data dir from the old service box to loyal-main through this machine (the boxes cannot
# reach each other and the Mac has too little disk to stage 4 GB, so it is a tar stream, not rsync).
#   loyal-clickstack-copy.sh pass     copy what is new or changed; deletes nothing; safe with the old service running
#   loyal-clickstack-copy.sh delta    read-only; prints what a pass would copy and what only the target holds
#   loyal-clickstack-copy.sh final    refuses while loyal-clickstack runs on the old box; copies, then removes target-only files
# The tmp/ directory is scratch ClickHouse rewrites and is never copied. Files are compared by type, size, mtime to the
# second, owner ids and mode (what rsync's quick check does). Both boxes are only read, except the target dir.
# Env, all optional: OLD (loyal-workers), NEW (loyal-main), DIR (/srv/loyal-clickstack), CONTAINER (loyal-clickstack).
set -euo pipefail

OLD=${OLD:-loyal-workers} NEW=${NEW:-loyal-main} DIR=${DIR:-/srv/loyal-clickstack} CONTAINER=${CONTAINER:-loyal-clickstack}
mode=${1:-}
case $mode in pass | delta | final) ;; *) echo "usage: $0 pass|delta|final" >&2; exit 2 ;; esac

as_root() {
  local host=$1 c
  shift
  c=$(printf '%q ' "$@")
  ssh -o BatchMode=yes "$host" "if [ \"\$(id -u)\" = 0 ]; then $c; else sudo -n $c; fi"
}

manifest() {
  as_root "$1" bash -c 'cd "$1" && nice -n 19 ionice -c3 find . -mindepth 1 -path ./tmp -prune -o -printf "%P\t%y\t%s\t%T@\t%U:%G\t%m\t%l\n"' _ "$DIR" |
    awk -F'\t' -v OFS='\t' '{ sub(/[.].*/, "", $4); if ($2 == "d") { $3 = "-"; $4 = "-" } print }' | LC_ALL=C sort -t$'\t' -k1,1
}

work=$(mktemp -d)
trap 'rm -r "$work"' EXIT
manifest "$OLD" >"$work/old" &
manifest "$NEW" >"$work/new" &
wait
[[ -s $work/old ]] || { echo "$OLD: empty manifest for $DIR" >&2; exit 1; }

LC_ALL=C awk -F'\t' -v OFS='\t' -v tocopy="$work/copy" -v toremove="$work/remove" '
  FNR == 1 { fileno++ }
  fileno == 1 { oldline[$1] = $0; next }
  { newline[$1] = $0 }
  END {
    for (p in oldline) if (!(p in newline) || newline[p] != oldline[p]) { print p > tocopy }
    for (p in newline) if (!(p in oldline)) print p > toremove
    close(tocopy); close(toremove)
  }' "$work/old" "$work/new"
: >>"$work/copy"; : >>"$work/remove"

copy_n=$(wc -l <"$work/copy" | tr -d ' ')
remove_n=$(wc -l <"$work/remove" | tr -d ' ')
copy_bytes=$(LC_ALL=C awk -F'\t' 'NR == FNR { want[$0] = 1; next } $2 == "f" && ($1 in want) { s += $3 } END { print s + 0 }' "$work/copy" "$work/old")
echo "$(date -u +%FT%TZ) $mode: $OLD has $(wc -l <"$work/old" | tr -d ' ') entries, $NEW has $(wc -l <"$work/new" | tr -d ' '); to copy $copy_n entries ($copy_bytes bytes); only on $NEW $remove_n"

if [[ $mode == delta ]]; then
  echo "to copy, by top-level directory:"
  awk -F/ '{ print $1 }' "$work/copy" | sort | uniq -c | sort -rn | head -12 | sed 's/^/  /'
  echo "only on $NEW, by top-level directory:"
  awk -F/ '{ print $1 }' "$work/remove" | sort | uniq -c | sort -rn | head -12 | sed 's/^/  /'
  exit 0
fi

if [[ $mode == final ]] && as_root "$OLD" docker ps -q --filter "name=^${CONTAINER}$" | grep -q .; then
  echo "refusing: $CONTAINER is running on $OLD; stop it first (the S4 window does)" >&2
  exit 1
fi

if [[ $copy_n -gt 0 ]]; then
  as_root "$NEW" mkdir -p "$DIR"
  as_root "$OLD" bash -c 'cd "$1" && nice -n 19 ionice -c3 tar --create --no-recursion --files-from=- --numeric-owner --format=posix --file=-' _ "$DIR" <"$work/copy" |
    as_root "$NEW" bash -c 'cd "$1" && nice -n 19 ionice -c3 tar --extract --same-owner --preserve-permissions --numeric-owner --file=-' _ "$DIR"
fi

if [[ $mode == final && $remove_n -gt 0 ]]; then
  sort -r "$work/remove" | tr '\n' '\0' | as_root "$NEW" bash -c 'cd "$1" && xargs -0 -r rm -d --' _ "$DIR"
fi
echo "$(date -u +%FT%TZ) $mode: done"
