#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUBCOMMANDS="forward source verifier srcexec catchout passwords probe init subscribe backfill catchup sequences grants schema hash verify preflight fence thaw switch reverse partitions retire writes"
PUB=pgmove
REV_PUB=pgmove_rev
REPL_ROLE=pgmove

usage() {
  cat >&2 <<EOF
usage: $0 <subcommand> <cluster> [flags]
clusters: $(for f in "$HERE"/clusters/*.conf; do basename "$f" .conf; done | paste -sd' ' -)
  forward <c>                  SRC_VIA=forward: the replication password, the passfile and pg-forward@<c> on this box
  source <c> <verifier|same>   SRC_VIA=forward: print, never run, the root script for the source (remote.sh source runs it)
  verifier <c>                 SRC_VIA=forward: stdin holds the source's verifier for the role; prints same or a new one
  srcexec <c> [verifiers]      SRC_VIA=forward, on the operator machine: stdin holds a box step's "need:" lines; prints the root script
  catchout <c> <table> <floor> SRC_VIA=forward: the target's hypertable rows above floor as binary COPY, for a reverse catchup
  passwords <c> [--dry-run]    target roles get the source's SCRAM verifiers (forward: role|verifier lines on stdin); superusers never
  probe <c>                    what feeds the source (neon-fed, static or local) and what blocks a move
  init <c> [--dry-run]         target cluster, settings, roles (NOLOGIN), databases, extensions, schema, hypertables
  subscribe <c> [--dry-run]    freeze marker, replication role, forward, publication, one subscription per database in order
  backfill <c> [--dry-run]     hypertables: the last TS_RETAIN_DAYS days, one day per binary COPY, resumable
  catchup <c> [--reverse]      hypertable rows above the event_id floor
  sequences <c> [--reverse]    setval on every sequence that differs
  grants <c>                   role, membership, setting, owner and ACL rows on both sides
  schema <c>                   pg_dump --schema-only of both sides, object by object
  hash <c> [table] [--age]     md5 per row, summed per 1M-key bucket; differing buckets hashed a second time; hypertables up to the target's max key
  hash <c> --moved             the same, skipping each plain table whose write counters and file nodes on both sides are those the last
                               0-DIFF full hash read before it; needs every other session idle HASH_IDLE_SECS and no prepared transaction
  verify <c> [--reverse]       tables, sequences, hypertables, schema, grants, hash age and the hypertable rows above the hash bound;
                               --reverse reads the target as the side of record, skips the forward hash's age and prints grant
                               rows as drift, not DIFF: a role made on the target in its tenure has no source twin
  preflight <c>                read-only: tables in state r, lag, verify, the reverse, fence and switch dry runs, marker, volume guard
  fence <c> [--reverse]        login roles NOLOGIN on the side being left, sessions ended, slots caught up
  thaw <c> [--dry-run]         before a switch: undo fence; LOGIN back to the source roles fence took it from, the forward on
  thaw <c> --reverse           after a rollback stopped before switch --reverse finished: LOGIN back on the target, the reverse on
  switch <c> [--reverse]       catchup, sequences, subscriptions off, the other direction armed or resumed, login roles LOGIN on the side taking over;
                               --reverse runs verify --reverse and a hash of every hypertable before the source gets LOGIN (--no-verify skips it)
  reverse <c> [--dry-run]      reverse publication on the target, reverse subscription on the source (copy_data=false)
  partitions <c> [--dry-run]   each direction: the publisher's partitions the subscriber lacks, then REFRESH PUBLICATION; drops nothing
  retire <c> [--force]         after REVERSE_HOURS: both directions, their slots, the freeze marker and the forward go
  writes <c> save|diff <tag> src|dst   write counters per table; diff prints DIFF rows and exits 0 once every counter was read
Mutating subcommands print "changed:" lines and end with "<n> change(s)"; --dry-run prints each command instead.
Checking subcommands print DIFF rows and end with "<n> DIFF"; they exit 1 when n > 0. A failed query stops any subcommand with exit 1.
SRC_VIA=forward: a step that needs root on the source prints "need:" lines and exits 3; remote.sh runs them through
PGMOVE_SRC_ADMIN from a script that srcexec builds on the operator machine, then reruns the step.
EOF
  exit 2
}

CMD=${1:-}
CLUSTER=${2:-}
[[ -n $CMD && -n $CLUSTER ]] || usage
case " $SUBCOMMANDS " in *" $CMD "*) ;; *) usage ;; esac
[[ -f $HERE/clusters/$CLUSTER.conf ]] || { echo "pgmove: no cluster $CLUSTER ($HERE/clusters/$CLUSTER.conf)" >&2; usage; }
shift 2
DRY=0 REVERSE=0 FORCE=0 AGE=0 MOVED=0 VERIFY=1
ARGS=()
for a in "$@"; do
  case $a in
    --dry-run) DRY=1 ;;
    --reverse) REVERSE=1 ;;
    --force) FORCE=1 ;;
    --age) AGE=1 ;;
    --moved) MOVED=1 ;;
    --no-verify) VERIFY=0 ;;
    -*) usage ;;
    *) ARGS+=("$a") ;;
  esac
done

PG_MAJOR=18
SRC_SSH='' SRC_SOCKET='' SRC_PORT='' SRC_OSUSER=''
DST_SSH=local DST_CLUSTER=main DST_DATA='' DST_CONF_FROM='' DST_CONF_SKIP=() DST_PORT=5432 DST_SOCKET=/var/run/postgresql DST_OSUSER=postgres
ENCODING=UTF8 LOCALE_PROVIDER='' LOCALE=''
DATABASES=()
HYPERTABLES=() HT_DB='' HT_TIME=observed_at HT_KEY=event_id TS_RETAIN_DAYS=62 CATCHUP_MARGIN=10000
SYNC_WORKERS=2 BINARY=true COPY_SETTINGS=()
SRC_VIA=ssh SRC_SSLMODE=disable SRC_HBA_FROM='' SRC_HBA_LINE='' FWD_TARGET=''
FWD_USER=pgmove-fwd FWD_SSH='' FWD_PORT='' REV_PORT='' REV_HOST=127.0.0.1 FREEZE_MARKER='' FENCE_KEEP=() REVERSE_HOURS=72
# MIGRATE_MARKER: the path the source's own deploy refuses migrations under, or none; preflight wants FREEZE_MARKER there.
# DST_VOLUME: the mount the target's data directory lives on; init makes postgresql@ refuse to start without it.
MIGRATE_MARKER='' DST_VOLUME='' MAX_LAG_BYTES=16777216 MAX_UPSTREAM_LAG_SECS=120 MAX_CATCHUP_ROWS=200000
PROBE_SECS=30 LAG_WAIT_SECS=600 HASH_BUCKET=1000000 HASH_RECHECK_SECS=60 HASH_MAX_AGE_HOURS=24 EXACT_COUNT_BYTES=268435456
# hash --moved: a backend flushes its table counters at exit, or within PGSTAT_IDLE_INTERVAL (10 s) of going idle.
HASH_IDLE_SECS=12 HASH_SETTLE_SECS=30
# shellcheck source=/dev/null
source "$HERE/clusters/$CLUSTER.conf"
for v in SRC_SSH SRC_SOCKET SRC_PORT SRC_OSUSER LOCALE_PROVIDER LOCALE; do
  [[ -n ${!v} ]] || { echo "pgmove: $CLUSTER.conf sets no $v" >&2; exit 2; }
done
((${#DATABASES[@]})) || { echo "pgmove: $CLUSTER.conf lists no DATABASES" >&2; exit 2; }
[[ -n $FWD_SSH ]] || FWD_SSH=$FWD_USER@${SRC_SSH#*@}
# SRC_VIA=ssh: psql and pg_dump run on SRC_SSH over sudo, through the operator's forwarded agent.
# SRC_VIA=forward: nothing from this box gets a shell on the source. FWD_SSH is a forward-only
# user on a hop to FWD_TARGET (remote.sh route makes it), every source query and dump runs here as REPL_ROLE through
# 127.0.0.1:FWD_PORT with SRC_SSLMODE, and what needs a superuser on the source is the script that
# `source` prints and remote.sh source runs from an operator machine.
case $SRC_VIA in
  ssh) ;;
  forward) for v in FWD_SSH FWD_TARGET FWD_PORT; do [[ -n ${!v} ]] || { echo "pgmove: SRC_VIA=forward needs $v in $CLUSTER.conf" >&2; exit 2; }; done ;;
  *) echo "pgmove: SRC_VIA=$SRC_VIA (ssh or forward)" >&2; exit 2 ;;
esac

WORK=/var/lib/pgmove/$CLUSTER
SECRETS=/etc/pgmove/$CLUSTER
FROM=src TO=dst
((REVERSE)) && FROM=dst TO=src

changes=0 diffs=0 DRIFT_ONLY=0 blocks=0 needs=0 DST_DOWN=0 REOPEN=0
# A failed query reads as empty, and an empty answer reads as absent or as equal. q usually runs in
# a subshell, where its exit status is easy to lose, so a failed query stops this whole run: q
# records it here and signals the top shell, whose trap exits 1 before the next command runs.
QFAIL=$(mktemp)
PGMOVE_PID=$$
# One master per run and host: with one path per host, a run's exit closed the master under another
# run's queries, which then exited 255 with no text (round 4, E-2).
SSH_CTL=/run/pgmove/ssh-$$-%C
# The run's masters end with it, not ControlPersist seconds later.
end_masters() {
  local h
  for h in "${SRC_SSH:-local}" "${DST_SSH:-local}"; do [[ $h == local ]] || ssh -o ControlPath="$SSH_CTL" -O exit "$h" >/dev/null 2>&1 || :; done
}
RUN_ID=''
trap 'rc=$?; [[ -z $RUN_ID ]] || echo "$CMD: run $RUN_ID ended rc=$rc"; rm -f "$QFAIL"; end_masters; ((REOPEN == 0)) || reopen_target $rc' EXIT
# PROBE names the reverse verify, so a query that fails there never reads as a difference.
PROBE=0
query_stopped() {
  local probe=''
  ((PROBE == 0)) || probe='probe failed in the reverse verify, so nothing was compared: '
  echo "pgmove: $CMD stopped: ${probe}a query failed, so nothing after it ran: $(paste -sd';' "$QFAIL" 2>/dev/null)" >&2
  exit 1
}
trap query_stopped USR1
trap 'exit 1' USR2
# bash cannot trap a signal its caller ignored, and then a failed query would not stop the run.
[[ $(trap -p USR1) == *stopped* && $(trap -p USR2) == *"exit 1"* ]] || { echo "pgmove: the caller ignores SIGUSR1 or SIGUSR2, so a failed query could not stop this run; run it from a shell that does not ignore them" >&2; exit 2; }

changed() { changes=$((changes + 1)); if ((DRY)); then echo "would change: $*"; else echo "changed: $*"; fi; }
diff_row() {
  if ((DRIFT_ONLY)); then echo "drift $*"; return 0; fi
  diffs=$((diffs + 1))
  # A hash DIFF voids the last pass at once: a run that stops after it never reaches its end.
  if [[ $CMD == hash ]] && ((AGE == 0 && DRY == 0)); then rm -f "$WORK/hash-ok" "$WORK/hash-sig"; fi
  echo "DIFF $*"
}
blocked() { blocks=$((blocks + 1)); echo "blocked: $*"; }
# need <verb> <args>: a root step on the source in SRC_VIA=forward. The box names it; srcexec on the
# operator machine checks each word and builds the SQL, so the box never decides what runs as root there.
need() { needs=$((needs + 1)); if ((DRY)); then echo "would need on the source: $*"; else echo "need: $*"; fi; }
src_by_admin() { [[ $1 == src ]] && via_forward; }
# A die in a command substitution ends the whole run, not the subshell alone.
die() { echo "pgmove: $*" >&2; [[ ${BASHPID:-$$} == "${PGMOVE_PID:-$$}" ]] || kill -USR2 "$PGMOVE_PID" 2>/dev/null; exit 1; }
mask() { sed -E "s/(password=)[^ ']*/\1***/g; s/(PASSWORD +')[^']*'/\1***'/g"; }
lit() { local s=${1//\'/\'\'}; printf "'%s'" "$s"; }
ident() { local s=${1//\"/\"\"}; printf '"%s"' "$s"; }
db_of() { echo "${1%%:*}"; }
shape_of() { echo "${1#*:}"; }
via_forward() { [[ $SRC_VIA == forward ]]; }
host_of() { if [[ $1 == src ]] && ! via_forward; then echo "$SRC_SSH"; else echo "$DST_SSH"; fi; }
other() { if [[ $1 == src ]]; then echo dst; else echo src; fi; }
# A stamp is replaced whole: a run stopped mid-write leaves the old one, never an empty file.
state() {
  if ((DRY)); then return 0; fi
  if ! { mkdir -p "$(dirname "$1")" && printf '%s\n' "$2" >"$1.new" && mv -f "$1.new" "$1"; }; then die "cannot write $1"; fi
}

# Every remote query rides one master connection per host. A new connection per query met the
# source's sshd MaxStartups early drop on a public box and failed 14 of 159 hash queries.
on() {
  if [[ $1 == local ]]; then bash -c "$2"; return; fi
  install -d -m 0700 /run/pgmove 2>/dev/null || true
  ssh -o BatchMode=yes -o ConnectTimeout=15 -o ControlMaster=auto -o ControlPath="$SSH_CTL" -o ControlPersist=120 "$1" "$2" 8>&- 9>&-
}

psql_cmd() {
  local side=$1 mode=$2 db=$3 extra=${4:-} user sock port opts="-c statement_timeout=0" as=''
  if [[ $side == src ]] && via_forward; then
    user=$DST_OSUSER sock=127.0.0.1 port=$FWD_PORT as=" -U $REPL_ROLE"
  elif [[ $side == src ]]; then
    user=$SRC_OSUSER sock=$SRC_SOCKET port=$SRC_PORT
  else
    user=$DST_OSUSER sock=$DST_SOCKET port=$DST_PORT
  fi
  [[ $mode == ro ]] && opts="$opts -c default_transaction_read_only=on"
  printf 'sudo -n -u %q env PGOPTIONS=%q PGSSLMODE=%q PGTZ=UTC PGDATESTYLE=ISO psql -h %q -p %q%s -d %q -XAtq -v ON_ERROR_STOP=1 %s' \
    "$user" "$opts" "$SRC_SSLMODE" "$sock" "$port" "$as" "$db" "$extra"
}

sql() { on "$(host_of "$1")" "$(psql_cmd "$@")"; }

# mutants.sh: PGMOVE_MUTATE_AT=n breaks the n-th query of a run (counted in PGMOVE_MUTATE_COUNT,
# children included), so every check can be shown to stop when its query fails.
mutated() {
  [[ -n ${PGMOVE_MUTATE_AT:-} && -n ${PGMOVE_MUTATE_COUNT:-} ]] || return 1
  local n=0
  until mkdir "$PGMOVE_MUTATE_COUNT.lock" 2>/dev/null; do sleep 0.01; done
  [[ ! -s $PGMOVE_MUTATE_COUNT ]] || n=$(<"$PGMOVE_MUTATE_COUNT")
  n=$((n + 1))
  echo "$n" >"$PGMOVE_MUTATE_COUNT"
  rmdir "$PGMOVE_MUTATE_COUNT.lock"
  [[ $n == "$PGMOVE_MUTATE_AT" ]]
}

query_failed() {
  echo "$1" >>"$QFAIL"
  kill -USR1 "$PGMOVE_PID"
  exit 1
}

# The one way pgmove reads a database. A failed query never returns: the run stops. The only
# absence it answers as empty is init --dry-run's, and only for what init would create: the whole
# target cluster (DST_DOWN) or the target database the call reads.
q() {
  local side=$1 db=$2 text=$3 extra=${4:-}
  local err out rc said try=1
  [[ $side == dst && $DST_DOWN == 1 ]] && return 0
  mutated && text="select pgmove_mutant_breaks_this_query()"
  err=$(mktemp) out=$(mktemp)
  while :; do
    rc=0
    printf '%s\n' "$text" | sql "$side" ro "$db" "$extra" >"$out" 2>"$err" || rc=$?
    if ((rc == 0)); then cat "$out"; rm -f "$err" "$out"; return 0; fi
    # ssh exits 255 only when its connection fails, never for SQL; a hash query only reads, so it
    # may run once more. Its output is held until it succeeds, so a retry never doubles a row.
    said=$(head -c 200 "$err" | tr '\n' ' ')
    ((rc == 255 && try == 1)) && [[ $CMD == hash ]] || break
    echo "pgmove: $side $db: ssh exit 255, the connection failed, not the query (${said:-no text}); this read-only hash query runs once more" >&2
    try=2
  done
  cat "$err" >&2
  if ((DRY)) && [[ $CMD == init && $side == dst ]] && our_db "$db" && grep -qF "database \"$db\" does not exist" "$err"; then
    rm -f "$err" "$out"
    return 0
  fi
  local msg how=''
  if ((rc == 255)); then how=" (ssh: the connection failed$( ((try == 1)) || echo ' twice'))"; fi
  msg="$side $db exit $rc$how: ${said:-no text}"
  rm -f "$err" "$out"
  query_failed "$msg"
}

# pg_dump through the same rule: a failed dump stops the run.
dump() {
  local side=$1 db=$2 flags=$3 cmd
  if [[ $side == src ]]; then cmd=$(src_dump "$db" "$flags"); else cmd=$(printf 'sudo -n -u %q pg_dump -h %q -p %q -d %q %s' "$DST_OSUSER" "$DST_SOCKET" "$DST_PORT" "$db" "$flags"); fi
  mutated && cmd="$cmd --pgmove-mutant-breaks-this-dump"
  on "$(host_of "$side")" "$cmd" || query_failed "$side $db: pg_dump $flags failed"
}

apply() {
  local side=$1 db=$2 text=$3
  shift 3
  changed "$*"
  if ((DRY)); then
    printf '  on %s %s: %s\n' "$(host_of "$side")" "$db" "$(printf '%s' "$text" | mask | tr '\n' ' ')"
    return 0
  fi
  printf '%s\n' "$text" | sql "$side" rw "$db" >/dev/null || query_failed "$side $db: $*: the statement failed"
}

run() {
  local host=$1 cmd=$2
  shift 2
  changed "$*"
  if ((DRY)); then printf '  on %s: %s\n' "$host" "$(printf '%s' "$cmd" | mask)"; return 0; fi
  on "$host" "$cmd"
}

put() {
  local host=$1 path=$2 mode=$3 owner=$4 content=$5
  shift 5
  changed "$*"
  if ((DRY)); then
    printf '  on %s: write %s (%s %s)\n' "$host" "$path" "$mode" "$owner"
    printf '%s\n' "$content" | mask | sed 's/^/    | /'
    return 0
  fi
  printf '%s\n' "$content" | on "$host" "install -d -m 0755 $(printf %q "$(dirname "$path")") && install -m $mode -o $owner -g $owner /dev/stdin $(printf %q "$path")"
}

query_failures() {
  local n
  n=$(grep -c . "$QFAIL" || true)
  ((n == 0)) && return 0
  echo "$CMD: $n quer$( ((n == 1)) && echo y || echo ies) failed, so the result above is incomplete; fix the cause printed above and rerun"
  exit 1
}

finish() {
  query_failures
  if ((DRY)); then echo "$CMD: $changes change(s) in the dry run, nothing ran${1:+; $1}"; else echo "$CMD: $changes change(s)${1:+; $1}"; fi
  ((blocks == 0)) || { echo "$CMD: $blocks blocker(s)"; exit 1; }
  ((needs == 0 || DRY)) || { echo "$CMD: $needs step(s) need root on the source; remote.sh runs them through PGMOVE_SRC_ADMIN, then reruns $CMD"; exit 3; }
}

# Stops a step where what follows must wait for the source steps named so far.
needs_first() { ((needs == 0 || DRY)) || finish "${1:-}"; }

finish_check() {
  query_failures
  echo "$diffs DIFF"
  ((diffs == 0)) || exit 1
}

NS="n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^(_timescaledb|timescaledb_|pg_toast|pg_temp)'"
USER_NS="$NS and not exists (select 1 from pg_depend e where e.classid = 'pg_namespace'::regclass and e.objid = n.oid and e.deptype = 'e')"
NOT_EXT="not exists (select 1 from pg_depend e where e.objid = c.oid and e.classid = 'pg_class'::regclass and e.deptype = 'e')"
NOT_HYPER="not exists (select 1 from pg_extension x where x.extname = 'timescaledb') or not exists (select 1 from timescaledb_information.hypertables h where h.hypertable_schema = n.nspname and h.hypertable_name = c.relname)"

pub_where() {
  local w="c.relkind = 'r' and $NS"
  [[ $1 == plain ]] && w="$w and ($NOT_HYPER)"
  echo "$w"
}

has_ext() { [[ -n $(q "$1" "$2" "select 1 from pg_extension where extname = 'timescaledb'") ]]; }

provider_code() { case $LOCALE_PROVIDER in builtin) echo b ;; libc) echo c ;; icu) echo i ;; *) die "LOCALE_PROVIDER $LOCALE_PROVIDER" ;; esac; }

FACTS_SQL="select format('%s|%s|%s|%s|%s|%s|%s|%s|%s', current_setting('server_version'), current_setting('wal_level'), current_setting('max_slot_wal_keep_size'), (select count(*) from pg_replication_slots), current_setting('max_replication_slots'), pg_encoding_to_char(d.encoding), d.datlocprovider, coalesce(d.datlocale, d.datcollate), pg_size_pretty(pg_database_size(d.datname))) from pg_database d where d.datname = current_database()"

no_identity_sql() {
  echo "select format('%I.%I', n.nspname, c.relname) || case c.relreplident when 'n' then ' has replica identity nothing' else ' has no primary key and default replica identity' end from pg_class c join pg_namespace n on n.oid = c.relnamespace where $(pub_where "$1") and (c.relreplident = 'n' or (c.relreplident = 'd' and not exists (select 1 from pg_index i where i.indrelid = c.oid and i.indisprimary))) order by 1"
}

probe_sample() {
  local i
  echo "select 'a|' || format('%I.%I', n.nspname, c.relname) || '|' || coalesce(s.n_tup_ins + s.n_tup_upd + s.n_tup_del, 0) from pg_class c join pg_namespace n on n.oid = c.relnamespace left join pg_stat_user_tables s on s.relid = c.oid where c.relkind = 'r' and $NS and not exists (select 1 from pg_subscription_rel r where r.srrelid = c.oid);"
  for ((i = 0; i < PROBE_SECS; i++)); do
    echo "select 'w|' || usename from pg_stat_activity where datname = current_database() and backend_type = 'client backend' and backend_xid is not null and pid <> pg_backend_pid();"
    echo "select pg_sleep(1);"
  done
  echo "select 'b|' || format('%I.%I', n.nspname, c.relname) || '|' || coalesce(s.n_tup_ins + s.n_tup_upd + s.n_tup_del, 0) from pg_class c join pg_namespace n on n.oid = c.relnamespace left join pg_stat_user_tables s on s.relid = c.oid where c.relkind = 'r' and $NS and not exists (select 1 from pg_subscription_rel r where r.srrelid = c.oid);"
}

setup_forward() {
  ensure_repl_password
  ensure_pgpass dst "$FWD_PORT"
  ensure_forward
}

cmd_forward() {
  via_forward || die "forward: $CLUSTER.conf sets SRC_VIA=$SRC_VIA; subscribe builds its forward"
  setup_forward
  if ((DRY == 0)) && ! can_connect dst "$FWD_PORT" postgres; then
    echo "forward: 127.0.0.1:$FWD_PORT listens; the source refuses $REPL_ROLE until deploy/aws/db/remote.sh source $CLUSTER runs"
  fi
  finish
}

cmd_verifier() {
  local pw cur
  via_forward || die "verifier: only for SRC_VIA=forward"
  pw=$(repl_password)
  [[ -n $pw ]] || die "verifier: no $SECRETS/repl.password; run forward first"
  IFS= read -r cur || true
  printf '%s\n%s\n' "$pw" "$cur" | scram
}

# shellcheck disable=SC2016
SCRAM_RE='^SCRAM-SHA-256\$4096:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$'

# Every root script for the source starts here: psql as the conf's OS user on its socket.
src_preamble() {
  printf 'osuser=%q sock=%q port=%q\n' "$SRC_OSUSER" "$SRC_SOCKET" "$SRC_PORT"
  cat <<'EOF'
set -euo pipefail
cd /
n=0
changed() { n=$((n + 1)); echo "changed: $*"; }
pq() { local db=$1; shift; sudo -n -u "$osuser" psql -h "$sock" -p "$port" -d "$db" -XAtq -v ON_ERROR_STOP=1 "$@"; }
EOF
}

# The source's root script. It is built from this file and the cluster conf wherever they are
# checked out, so nothing on the target box decides what runs as root on the source.
cmd_source() {
  local v=${ARGS[0]:-} spec db shape dbs=postgres line q
  via_forward || die "source: only for SRC_VIA=forward"
  [[ $v == same || $v =~ $SCRAM_RE ]] || die "source: the argument is not 'same' or a SCRAM-SHA-256 verifier"
  for spec in "${DATABASES[@]}"; do dbs+=",$(db_of "$spec")"; done
  if [[ -n $SRC_HBA_LINE ]]; then line=$SRC_HBA_LINE; else
    [[ -n $SRC_HBA_FROM ]] || die "source: $CLUSTER.conf sets neither SRC_HBA_FROM nor SRC_HBA_LINE"
    line="hostssl $dbs $REPL_ROLE $SRC_HBA_FROM scram-sha-256"
  fi
  src_preamble
  cat <<'EOF'
ts=$(date -u +%Y%m%dT%H%M%SZ)
# A check query that fails stops the script: inside [[ $(...) ]] it would read as "not done yet".
ask() { pq "$1" -c "$2" || { echo "source: $1: this check failed, so nothing after it ran: $2" >&2; exit 1; }; }
ensure() { # ensure <db> <query that answers t once done> <statement> <message>
  local have
  have=$(ask "$1" "$2") || exit 1
  [[ $have == t ]] && return 0
  printf '%s\n' "$3" | pq "$1" >/dev/null
  changed "$4"
}
EOF
  printf 'line=%q role=%q pub=%q marker=%q verifier=%q cluster=%q\n' "$line" "$REPL_ROLE" "$PUB" "$FREEZE_MARKER" "$v" "$CLUSTER"
  # A socket-forward source takes a local line, above its final local reject.
  if [[ $line == local\ * ]]; then echo "anchor='local all all reject'"; else echo "anchor='host all all 0.0.0.0/0 reject'"; fi
  cat <<'EOF'
hba=$(pq postgres -c 'show hba_file')
if ! grep -qxF -- "$line" "$hba"; then
  cp -a "$hba" "$hba.pgmove-$ts"
  awk -v l="$line" -v a="$anchor" '!d && ($1 " " $2 " " $3 " " $4 == a || ($1 ~ /^host/ && "host " $2 " " $3 " " $4 " " $5 == a)) { print l; d = 1 } { print } END { exit !d }' "$hba" >"$hba.new" ||
    { echo "source: $hba has no '$anchor' line to put the $role line above" >&2; exit 1; }
  chown --reference="$hba" "$hba.new" && chmod --reference="$hba" "$hba.new" && mv -f "$hba.new" "$hba"
  [[ $(pq postgres -c 'select count(*) from pg_hba_file_rules where error is not null') == 0 ]] ||
    { cp -a "$hba.pgmove-$ts" "$hba"; echo "source: the new $hba did not parse; the copy is back and nothing was reloaded" >&2; exit 1; }
  pq postgres -c 'select pg_reload_conf()' >/dev/null
  changed "$hba: '$line' above the final reject, reloaded (copy $hba.pgmove-$ts)"
fi
ensure postgres "select exists (select 1 from pg_roles where rolname = '$role')" "CREATE ROLE $role LOGIN REPLICATION NOSUPERUSER;" "role $role LOGIN REPLICATION"
ensure postgres "select rolcanlogin and rolreplication and not rolsuper and not rolcreaterole and not rolcreatedb from pg_roles where rolname = '$role'" \
  "ALTER ROLE $role LOGIN REPLICATION NOSUPERUSER NOCREATEROLE NOCREATEDB;" "role $role LOGIN REPLICATION and nothing more"
for r in pg_read_all_data pg_read_all_settings pg_read_all_stats; do
  ensure postgres "select pg_has_role('$role', '$r', 'MEMBER')" "GRANT $r TO $role;" "$role is a member of $r"
done
# A tablesync COPY of a large table outlives the cluster's statement and idle-transaction timeouts.
for g in statement_timeout idle_in_transaction_session_timeout; do
  ensure postgres "select exists (select 1 from pg_db_role_setting s join pg_roles r on r.oid = s.setrole where r.rolname = '$role' and s.setdatabase = 0 and '$g=0' = any (s.setconfig))" \
    "ALTER ROLE $role SET $g = 0;" "$role $g = 0"
done
if [[ $verifier != same ]]; then
  printf "ALTER ROLE %s PASSWORD '%s';\n" "$role" "$verifier" | pq postgres >/dev/null
  changed "$role password set from the target box's repl.password as a SCRAM verifier"
fi
EOF
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec")
    [[ $shape != upstream=* ]] || die "source: $db copies an upstream subscription; that shape has no source publication"
    if [[ $shape == plain ]]; then
      q="select 'CREATE PUBLICATION $PUB FOR TABLE ' || string_agg(format('%I.%I', n.nspname, c.relname), ', ' order by 1) || ';' from pg_class c join pg_namespace n on n.oid = c.relnamespace where $(pub_where plain)"
    else
      q="select 'CREATE PUBLICATION $PUB FOR ALL TABLES;'"
    fi
    printf 'db=%q shape=%q identity=%q create=%q\n' "$db" "$shape" "$(no_identity_sql "$shape")" "$q"
    cat <<'EOF'
have=$(ask "$db" "select exists (select 1 from pg_publication where pubname = '$pub')") || exit 1
if [[ $have != t ]]; then
  bad=$(pq "$db" -c "$identity")
  [[ -z $bad ]] || { echo "source: $db: $bad; UPDATE and DELETE on it would fail under a publication, so none was made" >&2; exit 1; }
  stmt=$(pq "$db" -c "$create")
  [[ $stmt == "CREATE PUBLICATION $pub FOR "* ]] || { echo "source: $db: no statement for publication $pub" >&2; exit 1; }
  printf '%s\n' "$stmt" | pq "$db" >/dev/null
  changed "$db: publication $pub ($shape: $(pq "$db" -c "select count(*) from pg_publication_tables where pubname = '$pub'") tables)"
fi
EOF
  done
  cat <<'EOF'
if [[ -n $marker && ! -e $marker ]]; then
  install -d -m 0755 "$(dirname "$marker")"
  echo "pgmove $cluster subscribed $(date -u +%FT%TZ)" >"$marker"
  changed "$marker: migrations here are frozen until retire"
fi
echo "source: wal_level=$(pq postgres -c 'show wal_level'), max_slot_wal_keep_size=$(pq postgres -c 'show max_slot_wal_keep_size'), slots $(pq postgres -c 'select count(*) from pg_replication_slots')/$(pq postgres -c 'show max_replication_slots')"
echo "source: $n change(s)"
EOF
}

cmd_probe() {
  local spec db shape facts ver wal keep used slots enc prov loc size subs feed verdict=
  local outside moved writers line name enabled host pubs total notready
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec")
    facts=$(q src "$db" "$FACTS_SQL")
    IFS='|' read -r ver wal keep used slots enc prov loc size <<<"$facts"
    echo "probe: $CLUSTER $db: $ver, wal_level=$wal, slots $used/$slots, max_slot_wal_keep_size=$keep, $enc provider=$prov locale=$loc, $size"
    [[ $enc == "$ENCODING" ]] || diff_row "$db encoding $enc on the source, $ENCODING in $CLUSTER.conf"
    [[ $prov == "$(provider_code)" && $loc == "$LOCALE" ]] || diff_row "$db locale provider $prov $loc on the source, $LOCALE_PROVIDER $LOCALE in $CLUSTER.conf"
    if [[ $shape != upstream=* ]]; then
      [[ $wal == logical ]] || diff_row "$db wal_level=$wal; a publication slot needs logical (owner-gated restart)"
      [[ $keep != -1 ]] || diff_row "$db max_slot_wal_keep_size=-1; a stalled slot could fill the source disk"
      while IFS= read -r line; do
        [[ -n $line ]] && diff_row "$db $line; publication $PUB would make UPDATE and DELETE on it fail"
      done < <(q src "$db" "$(no_identity_sql "$shape")")
    fi
    subs=$(q src "$db" "select s.subname || '|' || case when s.subenabled then 't' else 'f' end || '|' || coalesce(substring(s.subconninfo from 'host=''?([^ '']+)'), '-') || '|' || array_to_string(s.subpublications, ',') || '|' || (select count(*) from pg_subscription_rel r where r.srsubid = s.oid) || '|' || (select count(*) from pg_subscription_rel r where r.srsubid = s.oid and r.srsubstate <> 'r') from pg_subscription s where s.subdbid = (select oid from pg_database where datname = current_database()) order by 1")
    feed=none
    while IFS='|' read -r name enabled host pubs total notready; do
      [[ -n $name ]] || continue
      echo "probe: $CLUSTER $db: subscription $name enabled=$enabled from $host, publications $pubs, $total tables, $notready not in state r"
      if [[ $enabled == t && $notready == 0 && $feed != mixed ]]; then
        case $host in *.neon.tech) [[ $feed == none || $feed == neon ]] && feed=neon || feed=mixed ;; *) [[ $feed == none || $feed == upstream ]] && feed=upstream || feed=mixed ;; esac
      else
        feed=mixed
      fi
      [[ $shape != upstream=* || ",${shape#upstream=}," == *",$name,"* ]] || diff_row "$db subscription $name is not the one $CLUSTER.conf copies (${shape#upstream=})"
    done <<<"$subs"
    [[ $shape != upstream=* || -n $subs ]] || diff_row "$db $CLUSTER.conf copies subscription ${shape#upstream=}, the source has none"
    local sample
    sample=$(probe_sample | sql src ro "$db") || query_failed "src $db: the probe sample failed"
    outside=$(grep -c '^a|' <<<"$sample" || true)
    moved=$(awk -F'|' '$1 == "a" { a[$2] = $3 } $1 == "b" && $3 != a[$2] { n++ } END { print n + 0 }' <<<"$sample")
    writers=$(awk -F'|' '$1 == "w" { u[$2]++ } END { for (k in u) printf "%s%s(%d)", s, k, u[k]; s = "," }' <<<"$sample")
    echo "probe: $CLUSTER $db: $outside tables outside a subscription, $moved of them written in ${PROBE_SECS}s; client sessions holding a write transaction: ${writers:-none}"
    if ((moved > 0)) || [[ -n $writers ]]; then
      line=local
    elif [[ $feed == neon && $outside == 0 ]]; then
      line=neon-fed
    elif [[ $feed == upstream && $outside == 0 ]]; then
      line='upstream-fed'
    elif [[ $feed == none ]]; then
      line=static
    else
      line=mixed
    fi
    echo "probe: $CLUSTER $db: $line"
    if [[ -z $verdict || $verdict == "$line" ]]; then verdict=$line; elif [[ $line == local || $verdict == local ]]; then verdict=local; else verdict=mixed; fi
  done
  echo "feed: $verdict"
  finish_check
}

pgconf_want() {
  local name value
  cat <<EOF
wal_level = logical
max_worker_processes = 32
max_logical_replication_workers = 16
max_sync_workers_per_subscription = $SYNC_WORKERS
max_parallel_apply_workers_per_subscription = 0
max_replication_slots = 20
max_wal_senders = 20
max_active_replication_origins = 20
EOF
  for name in ${COPY_SETTINGS[@]+"${COPY_SETTINGS[@]}"}; do
    value=$(q src postgres "select current_setting($(lit "$name"), true)")
    [[ -z $value ]] || echo "$name = $(lit "$value")"
  done
}

ensure_cluster() {
  local port extra=
  port=$(on "$DST_SSH" "pg_lsclusters -h 2>/dev/null | awk '\$1 == \"$PG_MAJOR\" && \$2 == \"$DST_CLUSTER\" { print \$3 }'" 2>/dev/null || true)
  [[ $LOCALE_PROVIDER == builtin ]] && extra=" --builtin-locale=$LOCALE"
  local data=''
  [[ -z $DST_DATA ]] || data=" -d $DST_DATA"
  if [[ -z $port ]]; then
    run "$DST_SSH" "pg_createcluster $PG_MAJOR $DST_CLUSTER$data --port $DST_PORT --encoding $ENCODING --locale $LOCALE -- --locale-provider=$LOCALE_PROVIDER$extra && systemctl enable --now postgresql@$PG_MAJOR-$DST_CLUSTER" \
      "cluster $PG_MAJOR/$DST_CLUSTER on port $DST_PORT, $ENCODING, $LOCALE_PROVIDER $LOCALE"
  elif [[ $port != "$DST_PORT" ]]; then
    die "target cluster $PG_MAJOR/$DST_CLUSTER listens on $port, $CLUSTER.conf says $DST_PORT"
  fi
}

# DST_CONF_FROM: a conf.d the target takes as its own, such as the old host's production files
# staged on the box. pgmove's zz-pgmove*.conf and the names in DST_CONF_SKIP are never taken.
# Whatever else sits in conf.d, check_pgconf fails init unless pgmove's settings are the ones live.
ensure_pgconf() {
  local dir=/etc/postgresql/$PG_MAJOR/$DST_CLUSTER/conf.d want have restart=0 f names
  if [[ -n $DST_CONF_FROM ]]; then
    names=$(on "$DST_SSH" "cd $(printf %q "$DST_CONF_FROM") && ls -1 -- *.conf") || die "cannot list $DST_CONF_FROM/*.conf on the box"
    for f in $names; do
      [[ $f == zz-pgmove*.conf || " ${DST_CONF_SKIP[*]-} " == *" $f "* ]] && continue
      want=$(on "$DST_SSH" "cat $(printf %q "$DST_CONF_FROM/$f")") || die "cannot read $DST_CONF_FROM/$f"
      have=$(on "$DST_SSH" "cat $dir/$f" 2>/dev/null || true)
      [[ $have == "$want" ]] && continue
      put "$DST_SSH" "$dir/$f" 0640 postgres "$want" "$dir/$f: taken from $DST_CONF_FROM"
      restart=1
    done
  fi
  want=$(pgconf_want)
  have=$(on "$DST_SSH" "cat $dir/zz-pgmove.conf" 2>/dev/null || true)
  if [[ $have != "$want" ]]; then
    put "$DST_SSH" "$dir/zz-pgmove.conf" 0644 postgres "$want" "$dir/zz-pgmove.conf: logical replication settings, ${#COPY_SETTINGS[@]} copied from the source"
    restart=1
  fi
  if ! on "$DST_SSH" "test -e $dir/zz-pgmove-archive.conf" 2>/dev/null; then
    put "$DST_SSH" "$dir/zz-pgmove-archive.conf" 0644 postgres "$(printf "archive_mode = on\narchive_command = '/bin/true'")" \
      "$dir/zz-pgmove-archive.conf: archive_command=/bin/true until the first pgBackRest full replaces this file"
    restart=1
  fi
  ((restart == 0)) || run "$DST_SSH" "systemctl restart postgresql@$PG_MAJOR-$DST_CLUSTER" "postgresql@$PG_MAJOR-$DST_CLUSTER restarted to load them"
  check_pgconf "$dir"
}

# Every name pgmove's two files set must be live from that file, with no restart pending. A file
# sorting after them, postgresql.auto.conf or a missing library would otherwise win in silence.
check_pgconf() {
  local dir=$1 f n text names='' expect='' rows bad=0 total=0
  ((DRY)) && return 0
  for f in zz-pgmove.conf zz-pgmove-archive.conf; do
    text=$(on "$DST_SSH" "cat $dir/$f") || die "check_pgconf: cannot read $dir/$f"
    while read -r n; do
      names+="${names:+, }$(lit "$n")" expect+="$n=$dir/$f false"$'\n' total=$((total + 1))
    done < <(sed -nE 's/^[[:space:]]*([a-z_.]+)[[:space:]]*=.*/\1/p' <<<"$text")
  done
  ((total)) || die "check_pgconf: read no settings from $dir/zz-pgmove*.conf"
  rows=$(q dst postgres "select name || '=' || coalesce(sourcefile, source) || ' ' || pending_restart from pg_settings where name in ($names) order by 1") || die "check_pgconf: cannot read pg_settings on the target"
  while IFS= read -r n; do [[ -z $n ]] || grep -qxF -- "$n" <<<"$rows" || bad=$((bad + 1)); done <<<"$expect"
  if ((bad)); then
    printf '%s\n' "$rows" | sed 's/^/  live: /' >&2
    die "check_pgconf: $bad of $total settings in $dir/zz-pgmove*.conf are not the live ones; a later conf.d file, ALTER SYSTEM, a missing library or a pending restart wins"
  fi
  echo "pgconf: $total settings live from $dir/zz-pgmove*.conf, none pending a restart"
}

# pg_authid is superuser-only. Through the forward the source reads as REPL_ROLE, so roles come
# from pg_roles on both sides, passwords are neither compared nor copied, and the target roles
# get theirs before a switch.
AUTH=pg_authid PW_ROW="coalesce(md5(rolpassword), '-')" PW_SET="coalesce(quote_literal(rolpassword), 'NULL')"
if via_forward; then AUTH=pg_roles PW_ROW="'unread'" PW_SET="'NULL'"; fi
ROLE_ROWS="select rolname || '|' || rolsuper || '|' || rolinherit || '|' || rolcreaterole || '|' || rolcreatedb || '|' || rolreplication || '|' || rolbypassrls || '|' || rolconnlimit || '|' || coalesce(nullif(rolvaliduntil, 'infinity')::text, '-') || '|' || $PW_ROW from $AUTH where rolname !~ '^pg_' and rolname not in ('postgres', '$REPL_ROLE') order by 1"
ROLE_ALTER="select format('ALTER ROLE %I WITH %s %s %s %s %s %s CONNECTION LIMIT %s VALID UNTIL %s PASSWORD %s;', rolname, case when rolsuper then 'SUPERUSER' else 'NOSUPERUSER' end, case when rolinherit then 'INHERIT' else 'NOINHERIT' end, case when rolcreaterole then 'CREATEROLE' else 'NOCREATEROLE' end, case when rolcreatedb then 'CREATEDB' else 'NOCREATEDB' end, case when rolreplication then 'REPLICATION' else 'NOREPLICATION' end, case when rolbypassrls then 'BYPASSRLS' else 'NOBYPASSRLS' end, rolconnlimit, coalesce(quote_literal(rolvaliduntil::text), '''infinity'''), $PW_SET) from $AUTH where rolname = :'role'"
MEMBER_ROWS="select distinct r.rolname || '|' || m.rolname || '|' || a.admin_option || '|' || a.inherit_option || '|' || a.set_option from pg_auth_members a join pg_roles r on r.oid = a.roleid join pg_roles m on m.oid = a.member where m.rolname not in ('postgres', '$REPL_ROLE') and m.rolname !~ '^pg_' order by 1"
SETTING_ROWS="select coalesce(r.rolname, '-') || '|' || coalesce(d.datname, '-') || '|' || c from pg_db_role_setting s left join pg_roles r on r.oid = s.setrole left join pg_database d on d.oid = s.setdatabase cross join unnest(s.setconfig) c where coalesce(r.rolname, '') not in ('postgres', '$REPL_ROLE') order by 1"

our_db() { local spec; for spec in "${DATABASES[@]}"; do [[ $(db_of "$spec") == "$1" ]] && return 0; done; return 1; }

ensure_roles() {
  local rows have line role create
  rows=$(q src postgres "$ROLE_ROWS")
  have=$(q dst postgres "$ROLE_ROWS")
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    grep -qxF -- "$line" <<<"$have" && continue
    role=${line%%|*}
    create=
    grep -q "^$(printf '%s' "$role" | sed 's/[][\.*^$]/\\&/g')|" <<<"$have" || create="CREATE ROLE $(ident "$role") NOLOGIN;"
    apply dst postgres "$create $(q src postgres "$ROLE_ALTER" "-v role=$(printf %q "$role")")" \
      "role $role ${create:+created NOLOGIN, }attributes and password verifier as on the source"
  done <<<"$rows"
}

ensure_members() {
  local rows have r m adm inh set
  rows=$(q src postgres "$MEMBER_ROWS")
  have=$(q dst postgres "$MEMBER_ROWS")
  while IFS='|' read -r r m adm inh set; do
    [[ -n $r ]] || continue
    grep -qxF -- "$r|$m|$adm|$inh|$set" <<<"$have" && continue
    apply dst postgres "GRANT $(ident "$r") TO $(ident "$m") WITH ADMIN $([[ $adm == true ]] && echo TRUE || echo FALSE), INHERIT $([[ $inh == true ]] && echo TRUE || echo FALSE), SET $([[ $set == true ]] && echo TRUE || echo FALSE);" \
      "$m member of $r (admin $adm, inherit $inh, set $set)"
  done <<<"$rows"
}

ensure_settings() {
  local rows have line role db kv name value target val
  rows=$(q src postgres "$SETTING_ROWS")
  have=$(q dst postgres "$SETTING_ROWS")
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    grep -qxF -- "$line" <<<"$have" && continue
    IFS='|' read -r role db kv <<<"$line"
    [[ $db == - ]] || our_db "$db" || continue
    name=${kv%%=*} value=${kv#*=}
    case $name in search_path | temp_tablespaces | session_preload_libraries | local_preload_libraries) val=$value ;; *) val=$(lit "$value") ;; esac
    if [[ $role == - ]]; then target="DATABASE $(ident "$db")"; elif [[ $db == - ]]; then target="ROLE $(ident "$role")"; else target="ROLE $(ident "$role") IN DATABASE $(ident "$db")"; fi
    apply dst postgres "ALTER $target SET $name TO $val;" "$role in $db: $name as on the source"
  done <<<"$rows"
}

ensure_database() {
  local db=$1 owner loc
  owner=$(q src "$db" "select pg_get_userbyid(datdba) from pg_database where datname = current_database()")
  loc="LOCALE $(lit "$LOCALE")"
  [[ $LOCALE_PROVIDER == builtin ]] && loc="$loc BUILTIN_LOCALE $(lit "$LOCALE")"
  if [[ -z $(q dst postgres "select 1 from pg_database where datname = $(lit "$db")") ]]; then
    apply dst postgres "CREATE DATABASE $(ident "$db") OWNER $(ident "$owner") TEMPLATE template0 ENCODING $(lit "$ENCODING") LOCALE_PROVIDER $LOCALE_PROVIDER $loc;" \
      "database $db owned by $owner, $LOCALE_PROVIDER $LOCALE"
  fi
  if [[ -z $(q dst postgres "select 1 from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase join pg_roles r on r.oid = s.setrole where d.datname = $(lit "$db") and r.rolname = 'postgres' and 'statement_timeout=0' = any(s.setconfig)") ]]; then
    apply dst postgres "ALTER ROLE postgres IN DATABASE $(ident "$db") SET statement_timeout = 0; ALTER ROLE postgres IN DATABASE $(ident "$db") SET idle_in_transaction_session_timeout = 0;" \
      "postgres in $db: no statement or idle timeout for the replication workers"
  fi
}

ensure_extensions() {
  local db=$1 have name ver nsp
  have=$(q dst "$db" "select extname from pg_extension")
  while IFS='|' read -r name ver nsp; do
    [[ -n $name ]] || continue
    grep -qxF -- "$name" <<<"$have" && continue
    [[ $nsp == public ]] || { blocked "$db: extension $name lives in schema $nsp; only public is created before the schema restore"; continue; }
    apply dst "$db" "CREATE EXTENSION IF NOT EXISTS $(ident "$name") WITH SCHEMA public VERSION $(lit "$ver");" "$db: extension $name $ver"
  done < <(q src "$db" "select e.extname || '|' || e.extversion || '|' || n.nspname from pg_extension e join pg_namespace n on n.oid = e.extnamespace where e.extname <> 'plpgsql' order by 1")
}

# Only what the restore would collide with: an extension's own views (pg_stat_statements, made just
# before by ensure_extensions) are left out of the dump, so they are not counted.
USER_OBJECTS="select (select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind in ('r','p','v','m','S') and $NS and $NOT_EXT) + (select count(*) from pg_proc p join pg_namespace n on n.oid = p.pronamespace where $USER_NS and not exists (select 1 from pg_depend e where e.classid = 'pg_proc'::regclass and e.objid = p.oid and e.deptype = 'e'))"

src_dump() {
  if via_forward; then
    printf 'sudo -n -u %q env PGSSLMODE=%q pg_dump -h 127.0.0.1 -p %q -U %q -d %q %s' "$DST_OSUSER" "$SRC_SSLMODE" "$FWD_PORT" "$REPL_ROLE" "$1" "$2"
  else
    printf 'sudo -n -u %q pg_dump -h %q -p %q -d %q %s' "$SRC_OSUSER" "$SRC_SOCKET" "$SRC_PORT" "$1" "$2"
  fi
}

ensure_schema() {
  local db=$1 n dump=$WORK/schema-$1.dump list=$WORK/schema-$1.list
  n=$(q dst "$db" "$USER_OBJECTS")
  [[ -z $n || $n == 0 ]] || return 0
  [[ $(q src "$db" "$USER_OBJECTS") != 0 ]] || return 0
  local fetch restore
  fetch="$(src_dump "$db" "--schema-only -Fc --no-publications --no-subscriptions --exclude-schema='_timescaledb*' --exclude-schema='timescaledb_*'")"
  restore="/usr/lib/postgresql/$PG_MAJOR/bin/pg_restore -l $dump | grep -v -E ' EXTENSION - | COMMENT - EXTENSION ' >$list && /usr/lib/postgresql/$PG_MAJOR/bin/pg_restore -L $list -f - $dump | $(psql_cmd dst rw "$db" --single-transaction) >/dev/null"
  changed "$db: schema restored from the source with owners and grants (pg_dump --schema-only)"
  if ((DRY)); then
    printf '  on %s: %s, stdout to %s on this box\n' "$(host_of src)" "$fetch" "$dump"
    printf '  on %s: %s\n' "$DST_SSH" "$restore"
    return 0
  fi
  mkdir -p "$WORK"
  on "$(host_of src)" "$fetch" >"$dump"
  on "$DST_SSH" "$restore"
}

# pg_dump leaves out a NOT NULL constraint's name when it looks like the default one, and the
# restore then picks a free name in the schema. Where another table already holds that name
# (trades_* copied from wallet_swaps), the target gets wallet_swaps_id_not_null1, and a later
# migration that names the constraint fails there. Rename the target's to the source's.
NN_NAMES="select format('%I.%I', n.nspname, c.relname) || '|' || a.attname || '|' || k.conname from pg_constraint k join pg_class c on c.oid = k.conrelid join pg_namespace n on n.oid = c.relnamespace join pg_attribute a on a.attrelid = k.conrelid and a.attnum = k.conkey[1] where k.contype = 'n' and $NS"

ensure_constraint_names() {
  local db=$1 table col name sql='' n=0
  local -A want=()
  while IFS='|' read -r table col name; do [[ -n $table ]] && want[$table|$col]=$name; done < <(q src "$db" "$NN_NAMES")
  while IFS='|' read -r table col name; do
    [[ -n $table && -n ${want[$table|$col]:-} && ${want[$table|$col]} != "$name" ]] || continue
    sql+="ALTER TABLE $table RENAME CONSTRAINT $(ident "$name") TO $(ident "${want[$table|$col]}");"$'\n'
    n=$((n + 1))
  done < <(q dst "$db" "$NN_NAMES")
  ((n == 0)) || apply dst "$db" "$sql" "dst $db: $n NOT NULL constraint(s) renamed to the source's names"
}

ensure_hypertables() {
  local db=$1 have table column interval
  has_ext src "$db" || return 0
  have=''
  ! has_ext dst "$db" || have=$(q dst "$db" "select format('%I.%I', hypertable_schema, hypertable_name) from timescaledb_information.hypertables")
  while read -r table column interval; do
    [[ -n $table ]] || continue
    grep -qxF -- "$table" <<<"$have" && continue
    apply dst "$db" "SELECT create_hypertable($(lit "$table"), by_range($(lit "$column"), interval $(lit "$interval")));" "$db: $table is a hypertable on $column every $interval"
  done < <(q src "$db" "select format('%I.%I', hypertable_schema, hypertable_name) || ' ' || column_name || ' ' || time_interval from timescaledb_information.dimensions order by 1")
}

cmd_init() {
  local spec db
  ensure_cluster
  ensure_pgconf
  ensure_roles
  ensure_members
  for spec in "${DATABASES[@]}"; do ensure_database "$(db_of "$spec")"; done
  ensure_settings
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    ensure_extensions "$db"
    ensure_schema "$db"
    ensure_database_acl "$db"
    ensure_constraint_names "$db"
    ensure_hypertables "$db"
    ensure_timescale "$db"
  done
  ensure_volume_guard
  finish
}

# CREATE DATABASE leaves PUBLIC its CONNECT and TEMPORARY; a source that revoked them, or granted
# CONNECT to named roles, gets the same rows here. The owner's own rows are left as they are.
DB_ACL_ROWS="select case when a.grantee = 0 then 'PUBLIC' else format('%I', pg_get_userbyid(a.grantee)) end || '|' || a.privilege_type || '|' || case when a.is_grantable then 't' else 'f' end from pg_database d, aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a where d.datname = current_database() and a.grantee <> d.datdba"

ensure_database_acl() {
  local db=$1 want have who priv opt sql=''
  want=$(q src "$db" "$DB_ACL_ROWS" | sort)
  have=$(q dst "$db" "$DB_ACL_ROWS" | sort)
  [[ $want == "$have" ]] && return 0
  while IFS='|' read -r who priv opt; do [[ -z $who ]] || sql+="REVOKE ALL ON DATABASE $(printf '"%s"' "${db//\"/\"\"}") FROM $who CASCADE;"$'\n'; done <<<"$have"
  while IFS='|' read -r who priv opt; do [[ -z $who ]] || sql+="GRANT $priv ON DATABASE $(printf '"%s"' "${db//\"/\"\"}") TO $who$([[ $opt != t ]] || echo ' WITH GRANT OPTION');"$'\n'; done <<<"$want"
  apply dst "$db" "$sql" "$db: database privileges as on the source ($(grep -c . <<<"$want") row(s))"
}

scram() {
  python3 -I -c '
import base64, hashlib, hmac, os, sys
pw = sys.stdin.readline().rstrip("\n").encode()
cur = sys.stdin.readline().strip()
b64 = lambda x: base64.b64encode(x).decode()
def verifier(salt, it):
    salted = hashlib.pbkdf2_hmac("sha256", pw, salt, it)
    ck = hmac.new(salted, b"Client Key", "sha256").digest()
    sk = hmac.new(salted, b"Server Key", "sha256").digest()
    return "SCRAM-SHA-256$%d:%s$%s:%s" % (it, b64(salt), b64(hashlib.sha256(ck).digest()), b64(sk))
if cur.startswith("SCRAM-SHA-256$"):
    it, salt = cur.split("$")[1].split(":")
    if verifier(base64.b64decode(salt), int(it)) == cur:
        print("same")
        sys.exit()
print(verifier(os.urandom(16), 4096))'
}

repl_password() {
  if ((DRY)) || ! on "$DST_SSH" "test -s $SECRETS/repl.password" 2>/dev/null; then echo; return 0; fi
  on "$DST_SSH" "cat $SECRETS/repl.password"
}

ensure_repl_password() {
  on "$DST_SSH" "test -s $SECRETS/repl.password" 2>/dev/null && ((DRY == 0)) && return 0
  run "$DST_SSH" "install -d -m 0700 $SECRETS && (umask 077; openssl rand -base64 33 | tr -d '/+=\n' >$SECRETS/repl.password)" \
    "$SECRETS/repl.password generated (root only)"
}

ensure_repl_role() {
  local side=$1 attrs cur res pw
  if [[ $side == src ]] && via_forward; then
    [[ $(q src postgres "select rolreplication and not rolsuper and pg_has_role(current_user, 'pg_read_all_data', 'MEMBER') and pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER') and pg_has_role(current_user, 'pg_read_all_settings', 'MEMBER') from pg_roles where rolname = current_user") == t ]] ||
      blocked "src: $REPL_ROLE lacks REPLICATION or a pg_read_all_* role; run deploy/aws/db/remote.sh source $CLUSTER from an operator machine"
    return 0
  fi
  attrs=$(q "$side" postgres "select rolcanlogin and rolreplication and not rolsuper from pg_roles where rolname = '$REPL_ROLE'")
  if [[ -z $attrs ]]; then
    apply "$side" postgres "CREATE ROLE $REPL_ROLE LOGIN REPLICATION;" "$side: role $REPL_ROLE LOGIN REPLICATION"
  elif [[ $attrs != t ]]; then
    apply "$side" postgres "ALTER ROLE $REPL_ROLE LOGIN REPLICATION NOSUPERUSER;" "$side: role $REPL_ROLE LOGIN REPLICATION"
  fi
  [[ -n $(q "$side" postgres "select 1 from pg_auth_members a join pg_roles r on r.oid = a.roleid join pg_roles m on m.oid = a.member where r.rolname = 'pg_read_all_data' and m.rolname = '$REPL_ROLE'") ]] ||
    apply "$side" postgres "GRANT pg_read_all_data TO $REPL_ROLE;" "$side: $REPL_ROLE reads every table for the initial copy"
  pw=$(repl_password)
  if [[ -z $pw ]]; then
    changed "$side: $REPL_ROLE password set from $SECRETS/repl.password as a SCRAM verifier (the password itself is never sent)"
    ((DRY)) && printf "  on %s postgres: ALTER ROLE %s PASSWORD 'SCRAM-SHA-256\$4096:***';\n" "$(host_of "$side")" "$REPL_ROLE"
    return 0
  fi
  cur=$(q "$side" postgres "select coalesce(rolpassword, '') from pg_authid where rolname = '$REPL_ROLE'")
  res=$(printf '%s\n%s\n' "$pw" "$cur" | scram)
  [[ $res == same ]] || apply "$side" postgres "ALTER ROLE $REPL_ROLE PASSWORD $(lit "$res");" "$side: $REPL_ROLE password set from $SECRETS/repl.password as a SCRAM verifier"
}

# The source cluster's server runs as an OS user that may have no home (robinx-test has
# /nonexistent and ProtectHome=true), so its reverse subscription names this file in passfile=.
src_passfile() { echo "$SECRETS/pgpass"; }

ensure_pgpass() {
  local side=$1 port=$2 host user pw line f
  host=$(host_of "$side")
  if [[ $side == src ]]; then user=$SRC_OSUSER f=$(src_passfile); else user=$DST_OSUSER f="\$(getent passwd $user | cut -d: -f6)/.pgpass"; fi
  pw=$(repl_password)
  line="127.0.0.1:$port:*:$REPL_ROLE:$pw"
  if [[ -n $pw ]] && printf '%s\n' "$line" | on "$host" "test -f $f && grep -qxF -f /dev/stdin $f" 2>/dev/null; then return 0; fi
  changed "$side: $user's passfile holds $REPL_ROLE for 127.0.0.1:$port"
  if ((DRY)); then printf '  on %s: replace the 127.0.0.1:%s line of %s with 127.0.0.1:%s:*:%s:***\n' "$host" "$port" "$f" "$port" "$REPL_ROLE"; return 0; fi
  printf '%s\n' "$line" | on "$host" "f=$f; $([[ $side == src ]] && echo "install -d -m 0755 $SECRETS;") umask 077; { grep -v '^127\\.0\\.0\\.1:$port:' \$f 2>/dev/null || true; cat; } >\$f.new && chown $user: \$f.new && mv -f \$f.new \$f"
}

# One ssh session from the target carries both directions: -L reaches the source socket, and
# once the reverse is armed -R binds 127.0.0.1:REV_PORT on the source box to the target socket,
# so the source never holds a key for the target.
ensure_forward() {
  local env unit have restart=0
  have=$(on "$DST_SSH" "cat $SECRETS/forward.env" 2>/dev/null || true)
  # FWD_SOCKET is the -L destination: the source socket on SRC_SSH, or host:port behind FWD_SSH.
  if via_forward; then
    env=$(printf 'FWD_PORT=%s\nFWD_SOCKET=%s\nFWD_SSH=%s' "$FWD_PORT" "$FWD_TARGET" "$FWD_SSH")
  else
    env=$(printf 'FWD_PORT=%s\nFWD_SOCKET=%s\nFWD_SSH=%s' "$FWD_PORT" "$SRC_SOCKET/.s.PGSQL.$SRC_PORT" "$FWD_SSH")
  fi
  # SRC_VIA=forward: the -R binds on the hop behind FWD_SSH at REV_HOST, where the source reaches it;
  # the hop's sshd allows that one listener (remote.sh route) and nothing else.
  if [[ ${1:-} == rev || $have == *FWD_REV=* ]]; then
    env+=$(printf '\nFWD_REV="-R %s:%s:%s"' "$REV_HOST" "$REV_PORT" "$DST_SOCKET/.s.PGSQL.$DST_PORT")
  fi
  unit=$(cat "$HERE/pg-forward@.service")
  if [[ $(on "$DST_SSH" "cat /etc/systemd/system/pg-forward@.service" 2>/dev/null || true) != "$unit" ]]; then
    put "$DST_SSH" /etc/systemd/system/pg-forward@.service 0644 root "$unit" "pg-forward@.service installed"
    run "$DST_SSH" "systemctl daemon-reload" "systemd reloaded"
    restart=1
  fi
  if [[ $have != "$env" ]]; then
    put "$DST_SSH" "$SECRETS/forward.env" 0600 root "$env" "$SECRETS/forward.env: 127.0.0.1:$FWD_PORT to $(sed -n 's/^FWD_SOCKET=//p' <<<"$env") behind $FWD_SSH$([[ $env == *FWD_REV=* ]] && echo ", and $FWD_SSH 127.0.0.1:$REV_PORT back to this box")"
    restart=1
  fi
  if ((restart)); then
    run "$DST_SSH" "systemctl enable pg-forward@$CLUSTER && systemctl restart pg-forward@$CLUSTER" "pg-forward@$CLUSTER (re)started"
  elif ! on "$DST_SSH" "systemctl is-active --quiet pg-forward@$CLUSTER" 2>/dev/null; then
    run "$DST_SSH" "systemctl enable --now pg-forward@$CLUSTER" "pg-forward@$CLUSTER started"
  fi
  forward_wait "$DST_SSH" "$FWD_PORT"
  # Forward mode: the box has no shell on the hop; srcexec's revsub connects through it before it subscribes.
  [[ $env != *FWD_REV=* ]] || via_forward || forward_wait "$SRC_SSH" "$REV_PORT"
}

forward_wait() {
  ((DRY)) && return 0
  on "$1" "for i in \$(seq 30); do (exec 3<>/dev/tcp/127.0.0.1/$2) 2>/dev/null && exit 0; sleep 1; done; exit 1" ||
    die "pg-forward@$CLUSTER: nothing listens on $1 127.0.0.1:$2 after 30 s"
}

can_connect() {
  local side=$1 port=$2 db=$3 user ssl=disable
  local pass=''
  if [[ $side == src ]]; then user=$SRC_OSUSER pass=" passfile=$(src_passfile)"; else user=$DST_OSUSER ssl=$SRC_SSLMODE; fi
  ((DRY)) && return 0
  on "$(host_of "$side")" "sudo -n -u $user psql $(printf %q "host=127.0.0.1 port=$port dbname=$db user=$REPL_ROLE sslmode=$ssl connect_timeout=10$pass") -XAtqc 'select 1'" >/dev/null 2>&1
}

# remote_has <host> <path>: 0 present, 1 absent; a host that does not answer stops the run.
remote_has() {
  local a
  a=$(on "$1" "if test -e $(printf %q "$2"); then echo present; else echo absent; fi") || die "cannot read $2 on $1"
  case $a in present) return 0 ;; absent) return 1 ;; *) die "reading $2 on $1 answered '$a'" ;; esac
}

put_marker() {
  [[ -n $FREEZE_MARKER ]] || return 0
  remote_has "$1" "$FREEZE_MARKER" && return 0
  run "$1" "install -d -m 0755 $(dirname "$FREEZE_MARKER") && echo \"pgmove $CLUSTER $2 \$(date -u +%FT%TZ)\" >$FREEZE_MARKER" \
    "$1:$FREEZE_MARKER: migrations there refuse until retire"
}

ensure_marker() {
  via_forward && return 0 # the source script writes it
  put_marker "$SRC_SSH" subscribed
}

sub_name() { if [[ $1 == rev ]]; then echo "pgmove_rev_$2"; else echo "pgmove_$2"; fi; }

ready_count() {
  q "$1" "$2" "select count(*) filter (where r.srsubstate <> 'r') || '|' || count(*) from pg_subscription_rel r join pg_subscription s on s.oid = r.srsubid where s.subname = $(lit "$3")"
}

ensure_publication() {
  local side=$1 db=$2 shape=$3 name=$4 list
  [[ -n $(q "$side" "$db" "select 1 from pg_publication where pubname = '$name'") ]] && return 0
  if [[ $side == src ]] && via_forward; then
    blocked "src $db: no publication $name; run deploy/aws/db/remote.sh source $CLUSTER from an operator machine"
    return 0
  fi
  if [[ $shape == plain ]]; then
    list=$(q "$side" "$db" "select string_agg(format('%I.%I', n.nspname, c.relname), ', ' order by 1) from pg_class c join pg_namespace n on n.oid = c.relnamespace where $(pub_where plain)")
    [[ -n $list ]] || { blocked "$side $db: no plain tables to publish"; return 0; }
    apply "$side" "$db" "CREATE PUBLICATION $name FOR TABLE $list;" "$side $db: publication $name for $(tr ',' '\n' <<<"$list" | grep -c .) plain tables (hypertables ride backfill and catchup)"
  else
    apply "$side" "$db" "CREATE PUBLICATION $name FOR ALL TABLES;" "$side $db: publication $name for all tables"
  fi
}

identity_blockers() {
  local side=$1 db=$2 shape=$3 line
  while IFS= read -r line; do
    [[ -n $line ]] && blocked "$side $db: $line; it needs REPLICA IDENTITY FULL before publication $PUB exists"
  done < <(q "$side" "$db" "$(no_identity_sql "$shape")")
}

# The source reads the upstream password from a passfile on its own disk. The target gets that one
# line in its postgres user's ~/.pgpass, and the conninfo it subscribes with loses passfile=.
ensure_upstream_pass() {
  local conn=$1 pf host user line f
  pf=$(sed -nE 's/(^|.* )passfile=([^ ]+).*/\2/p' <<<"$conn")
  [[ -n $pf ]] || return 0
  host=$(sed -nE 's/(^|.* )host=([^ ]+).*/\2/p' <<<"$conn")
  user=$(sed -nE 's/(^|.* )user=([^ ]+).*/\2/p' <<<"$conn")
  line=$(on "$SRC_SSH" "awk -F: -v h=$(printf %q "$host") -v u=$(printf %q "$user") '(\$1 == h || \$1 == \"*\") && \$4 == u' $(printf %q "$pf")") || die "reading $pf on $SRC_SSH failed"
  [[ -n $line ]] || die "$pf on $SRC_SSH holds no line for $user at $host"
  f="\$(getent passwd $DST_OSUSER | cut -d: -f6)/.pgpass"
  ((DRY)) || ! printf '%s\n' "$line" | on "$DST_SSH" "IFS= read -r l; grep -qxF \"\$l\" $f" 2>/dev/null || return 0
  changed "dst: $DST_OSUSER's ~/.pgpass holds the upstream password line for $user"
  ((DRY)) || printf '%s\n' "$line" | on "$DST_SSH" "IFS= read -r l; f=$f; umask 077; { cat \$f 2>/dev/null || true; printf '%s\n' \"\$l\"; } >\$f.new && chown $DST_OSUSER: \$f.new && mv -f \$f.new \$f"
}

cmd_subscribe() {
  local spec db shape sub prev='' counts wal keep conn
  if via_forward; then
    setup_forward
    ((DRY)) || can_connect dst "$FWD_PORT" postgres || { blocked "the source refuses $REPL_ROLE through the forward; run deploy/aws/db/remote.sh source $CLUSTER from an operator machine"; finish; }
  fi
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec")
    [[ $shape == upstream=* ]] && continue
    IFS='|' read -r wal keep <<<"$(q src "$db" "select current_setting('wal_level') || '|' || current_setting('max_slot_wal_keep_size')")"
    [[ $wal == logical ]] || blocked "source wal_level=$wal; a slot needs logical (owner-gated restart)"
    [[ $keep != -1 ]] || blocked "source max_slot_wal_keep_size=-1; set it to half the free space and reload first"
    identity_blockers src "$db" "$shape"
  done
  ((blocks == 0 || DRY)) || finish
  ensure_marker
  if via_forward; then
    ensure_repl_role src
  elif [[ -n $FWD_PORT ]]; then
    ensure_repl_password
    ensure_repl_role src
    ensure_pgpass dst "$FWD_PORT"
    ensure_forward
  fi
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec") sub=$(sub_name fwd "$db")
    if [[ -n $prev ]] && ((DRY == 0)); then
      IFS='|' read -r counts _ <<<"$(ready_count dst "$prev" "$(sub_name fwd "$prev")")"
      if [[ $counts != 0 ]]; then
        finish "$db waits until every table of $prev is in state r ($counts not yet); rerun then"
        return 0
      fi
    fi
    prev=$db
    [[ -n $(q dst "$db" "select 1 from pg_subscription where subname = '$sub'") ]] && continue
    if [[ $shape == upstream=* ]]; then
      conn=$(q src "$db" "select subconninfo from pg_subscription where subname = $(lit "${shape#upstream=}")")
      local pubs
      pubs=$(q src "$db" "select array_to_string(subpublications, ', ') from pg_subscription where subname = $(lit "${shape#upstream=}")")
      [[ -n $conn ]] || { blocked "$db: the source has no subscription ${shape#upstream=} to copy"; break; }
      ensure_upstream_pass "$conn"
      conn=$(sed -E "s/(^| )passfile=[^ ]+//; s/(^| )application_name=[^ ]+/\\1application_name=$sub/" <<<"$conn")
      apply dst "$db" "CREATE SUBSCRIPTION $sub CONNECTION $(lit "$conn") PUBLICATION $pubs WITH (binary = $BINARY, streaming = on, copy_data = true, disable_on_error = false);" \
        "dst $db: subscription $sub to the publisher of ${shape#upstream=} ($pubs), copy_data=true"
      continue
    fi
    conn="host=127.0.0.1 port=$FWD_PORT dbname=$db user=$REPL_ROLE application_name=$sub sslmode=$SRC_SSLMODE"
    if ! can_connect dst "$FWD_PORT" "$db"; then
      if via_forward; then
        blocked "the source refuses $REPL_ROLE for $db through the forward; run deploy/aws/db/remote.sh source $CLUSTER from an operator machine"
      else
        blocked "the source refuses $REPL_ROLE through the forward; its hba needs 'local $db $REPL_ROLE scram-sha-256' above the peer lines, then a reload"
      fi
      break
    fi
    ensure_publication src "$db" "$shape" "$PUB"
    sync_partitions src dst "$db" "$sub" "$PUB"
    apply dst "$db" "CREATE SUBSCRIPTION $sub CONNECTION $(lit "$conn") PUBLICATION $PUB WITH (binary = $BINARY, streaming = on, copy_data = true, origin = none, disable_on_error = false);" \
      "dst $db: subscription $sub (slot $sub on the source), $SYNC_WORKERS sync workers"
  done
  ((DRY == 0)) || echo "dry run: in a real run each database subscribes only once every table of the one before is in state r"
  finish
}

ts_cutoff() {
  local f=$WORK/backfill/cutoff
  if [[ -s $f ]]; then cat "$f"; return 0; fi
  local c
  c=$(q src "$HT_DB" "select (date_trunc('day', now() at time zone 'UTC') - interval '$TS_RETAIN_DAYS days')::date")
  state "$f" "$c"
  echo "$c"
}

ht_state() { echo "$WORK/backfill/${1//./_}"; }

cmd_backfill() {
  ((${#HYPERTABLES[@]})) || { echo "backfill: $CLUSTER has no hypertables"; finish; return 0; }
  local cutoff table st day
  cutoff=$(ts_cutoff)
  for table in "${HYPERTABLES[@]}"; do
    st=$(ht_state "$table")
    [[ -s $st/event_floor ]] || state "$st/event_floor" "$(q src "$HT_DB" "select coalesce(max($HT_KEY), 0) from $table")"
    while read -r day; do
      [[ -e $st/$day.done ]] && continue
      changed "$table $day copied by binary COPY"
      if ((DRY)); then echo "  delete $day on dst, then \\copy (select * from $table where $HT_TIME in [$day, +1 day)) src to dst"; continue; fi
      printf 'DELETE FROM %s WHERE %s >= %s AND %s < %s::date + 1;\n' "$table" "$HT_TIME" "$(lit "$day")" "$HT_TIME" "$(lit "$day")" | sql dst rw "$HT_DB" >/dev/null || query_failed "dst: clearing $table $day failed"
      printf '\\copy (select * from %s where %s >= %s and %s < %s::date + 1) to stdout with (format binary)\n' "$table" "$HT_TIME" "$(lit "$day")" "$HT_TIME" "$(lit "$day")" | sql src ro "$HT_DB" |
        on "$DST_SSH" "$(psql_cmd dst rw "$HT_DB" "--single-transaction -c 'SET session_replication_role = replica' -c $(printf %q "\\copy $table from stdin with (format binary)")")" >/dev/null || query_failed "copying $table $day failed"
      touch "$st/$day.done"
    done < <(q src "$HT_DB" "select d::date from generate_series($(lit "$cutoff")::date, (now() at time zone 'UTC')::date, interval '1 day') d")
  done
  finish "backfill from $cutoff"
}

floor_file() { if ((REVERSE)); then echo "$(ht_state "$1")/event_floor.rev"; else echo "$(ht_state "$1")/event_floor"; fi; }

# Rows above a floor, by the predicates catchup copies with: count|sum of row hashes.
catch_sum() { q "$1" "$HT_DB" "select count(*) || '|' || coalesce($ROW_SUM, 0) from $2 t where t.$HT_KEY > $3 and t.$HT_TIME >= $(lit "$4")"; }

catchup_tables() {
  local table f floor ceiling inserted cutoff a b
  ((${#HYPERTABLES[@]})) || return 0
  cutoff=$(ts_cutoff)
  for table in "${HYPERTABLES[@]}"; do
    f=$(floor_file "$table")
    [[ -s $f ]] || die "catchup: no floor $f (forward: run backfill; reverse: run switch)"
    floor=$(($(cat "$f") - CATCHUP_MARGIN))
    ceiling=$(q "$FROM" "$HT_DB" "select coalesce(max($HT_KEY), 0) from $table")
    [[ $ceiling =~ ^[0-9]+$ ]] || die "catchup: $FROM $table answered no max($HT_KEY)"
    if src_by_admin "$TO"; then
      # Done once both sides hold the same rows above the floor; the insert skips rows already there.
      if [[ $(catch_sum src "$table" "$floor" "$cutoff") == "$(catch_sum "$FROM" "$table" "$floor" "$cutoff")" ]]; then state "$f" "$ceiling"; else need catchup "$table" "$floor"; fi
      continue
    fi
    if ((DRY)); then changed "$table rows above $HT_KEY $floor, $FROM to $TO"; continue; fi
    inserted=$(printf '\\copy (select * from %s where %s > %s and %s >= %s) to stdout with (format binary)\n' "$table" "$HT_KEY" "$floor" "$HT_TIME" "$(lit "$cutoff")" | sql "$FROM" ro "$HT_DB" |
      on "$(host_of "$TO")" "$(psql_cmd "$TO" rw "$HT_DB" "--single-transaction -c 'SET session_replication_role = replica' -c $(printf %q "CREATE TEMP TABLE catchup_rows (LIKE $table) ON COMMIT DROP") -c $(printf %q '\copy catchup_rows from stdin with (format binary)') -c $(printf %q "WITH moved AS (INSERT INTO $table SELECT c.* FROM catchup_rows c WHERE NOT EXISTS (SELECT 1 FROM $table t WHERE t.$HT_KEY = c.$HT_KEY AND t.$HT_TIME = c.$HT_TIME) RETURNING 1) SELECT count(*) FROM moved")")" | tail -1) || query_failed "catchup: copying $table above $floor, $FROM to $TO failed"
    [[ $inserted =~ ^[0-9]+$ ]] || query_failed "catchup: $TO answered '$inserted', not a row count"
    ((inserted == 0)) || changed "$table +$inserted rows above $HT_KEY $floor, $FROM to $TO"
    a=$(catch_sum "$FROM" "$table" "$floor" "$cutoff") b=$(catch_sum "$TO" "$table" "$floor" "$cutoff")
    if [[ $a != "$b" ]]; then blocked "catchup: $table above $HT_KEY $floor: $FROM ${a%%|*} rows, $TO ${b%%|*} rows, sums differ; the floor stays"; continue; fi
    state "$f" "$ceiling"
  done
}

cmd_catchup() {
  catchup_tables
  finish
}

# name|next|last_value|is_called|increment_by, read from the sequence itself: pg_sequences shows no
# last_value for a sequence whose next value is its start, as after ALTER SEQUENCE RESTART.
SEQ_ROWS="select format('%I.%I', schemaname, sequencename) || '|' || (xpath('/table/row/v/text()', query_to_xml(format('select (case when is_called then last_value::numeric + %s else last_value end) || ''|'' || last_value || ''|'' || is_called as v from %I.%I', increment_by, schemaname, sequencename), false, false, '')))[1]::text || '|' || increment_by from pg_sequences where schemaname not in ('pg_catalog','information_schema') and schemaname !~ '^(_timescaledb|timescaledb_)' order by 1"

# A sequence only moves forward: a rerun after TO took ids never hands them out again.
sync_sequences() {
  local spec db have name next last called inc tnext
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    have=$(q "$TO" "$db" "$SEQ_ROWS")
    while IFS='|' read -r name next last called inc; do
      [[ -n $name ]] || continue
      tnext=$(awk -F'|' -v n="$name" '$1 == n { print $2 }' <<<"$have")
      [[ -n $tnext ]] || { blocked "$TO $db: no sequence $name, which $FROM has"; continue; }
      if ((inc > 0)); then ((next > tnext)) || continue; else ((next < tnext)) || continue; fi
      if src_by_admin "$TO"; then need setval "$db" "$name" "$last" "$called"; continue; fi
      apply "$TO" "$db" "SELECT setval($(lit "$name"), $last, $called);" "$TO $db: $name next $next as on $FROM (was $tnext)"
    done < <(q "$FROM" "$db" "$SEQ_ROWS")
  done
}

cmd_sequences() {
  sync_sequences
  finish
}

compare_rows() {
  local label=$1 a=$2 b=$3 line
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    case $line in
      $'\t'*) diff_row "$label only on dst: ${line#$'\t'}" ;;
      *) diff_row "$label only on src: $line" ;;
    esac
  done < <(comm -3 <(sort -u <<<"$a") <(sort -u <<<"$b"))
}

acl_sql() {
  cat <<EOF
with o as (
  select 'relation' as kind, format('%I.%I', n.nspname, c.relname) as name, c.relowner as owner,
    coalesce(c.relacl, acldefault((case when c.relkind = 'S' then 's' else 'r' end)::"char", c.relowner)) as acl
  from pg_class c join pg_namespace n on n.oid = c.relnamespace
  where c.relkind in ('r','p','v','m','f','S') and $USER_NS and $NOT_EXT
  union all
  select 'schema', format('%I', n.nspname), n.nspowner, coalesce(n.nspacl, acldefault('n', n.nspowner)) from pg_namespace n where $USER_NS
  union all
  select 'function', format('%I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)), p.proowner, coalesce(p.proacl, acldefault('f', p.proowner))
  from pg_proc p join pg_namespace n on n.oid = p.pronamespace
  where $USER_NS and not exists (select 1 from pg_depend e where e.classid = 'pg_proc'::regclass and e.objid = p.oid and e.deptype = 'e')
  union all
  select 'database', format('%I', d.datname), d.datdba, coalesce(d.datacl, acldefault('d', d.datdba)) from pg_database d where d.datname = current_database()
)
select kind || ' ' || name || ' owner ' || pg_get_userbyid(owner) from o
union all
select o.kind || ' ' || o.name || ' ' || case when a.grantee = 0 then 'PUBLIC' else pg_get_userbyid(a.grantee) end || ' ' || a.privilege_type || case when a.is_grantable then ' WITH GRANT OPTION' else '' end from o, aclexplode(o.acl) a
union all
select 'default-acl ' || pg_get_userbyid(d.defaclrole) || ' ' || coalesce(n.nspname, '*') || ' ' || d.defaclobjtype::text || ' ' || case when a.grantee = 0 then 'PUBLIC' else pg_get_userbyid(a.grantee) end || ' ' || a.privilege_type
from pg_default_acl d left join pg_namespace n on n.oid = d.defaclnamespace, aclexplode(d.defaclacl) a
EOF
}

# Settings rows for every database (-) or one of ours; awk, as grep's no-match exit would end the run.
ours_only() { awk -F'|' -v ours="|$1|" '$2 == "-" || index(ours, "|" $2 "|")'; }

check_grants() {
  local spec db start=$diffs ours
  compare_rows "role" "$(q src postgres "$ROLE_ROWS")" "$(q dst postgres "$ROLE_ROWS")"
  compare_rows "membership" "$(q src postgres "$MEMBER_ROWS")" "$(q dst postgres "$MEMBER_ROWS")"
  ours=$(for spec in "${DATABASES[@]}"; do db_of "$spec"; done | paste -sd'|' -)
  compare_rows "setting" "$(q src postgres "$SETTING_ROWS" | ours_only "$ours")" "$(q dst postgres "$SETTING_ROWS" | ours_only "$ours")"
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    compare_rows "$db acl" "$(q src "$db" "$(acl_sql)")" "$(q dst "$db" "$(acl_sql)")"
  done
  echo "grants: roles, memberships, settings and the owner and ACL rows of ${#DATABASES[@]} database(s): $((diffs - start)) DIFF"
}

cmd_grants() {
  check_grants
  finish_check
}

schema_diff() {
  python3 -I - "$1" "$2" <<'PY'
import re, sys
def blocks(path):
    out, key, body = {}, "preamble", []
    def flush():
        text = "\n".join(l for l in body if l.strip() and not l.startswith("--"))
        if text:
            out.setdefault(key, []).append(text)
    for line in open(path):
        line = line.rstrip("\n")
        if line.startswith(("\\restrict", "\\unrestrict", "SET ", "SELECT pg_catalog.set_config")):
            continue
        m = re.match(r"-- Name: (.*); Type: (.*); Schema: (.*); Owner: .*", line)
        if m:
            flush()
            key, body = f"{m.group(2)} {m.group(3)}.{m.group(1)}", []
        else:
            body.append(line)
    flush()
    return {k: "\n".join(sorted(v)) for k, v in out.items() if not k.startswith(("EXTENSION ", "COMMENT -.EXTENSION "))}
a, b = blocks(sys.argv[1]), blocks(sys.argv[2])
# Only whitespace outside literals, quoted names and dollar-quoted bodies is ignored: parentheses carry precedence.
lit = re.compile(r"'(?:[^']|'')*'|\"(?:[^\"]|\"\")*\"|\$([A-Za-z_0-9]*)\$.*?\$\1\$", re.S)
def bare(t):
    out, pos = [], 0
    for m in lit.finditer(t):
        out += [re.sub(r"\s", "", t[pos:m.start()]), m.group(0)]
        pos = m.end()
    return "".join(out) + re.sub(r"\s", "", t[pos:])
for k in sorted(a.keys() | b.keys()):
    x, y = a.get(k), b.get(k)
    if x == y or (x is not None and y is not None and bare(x) == bare(y)):
        continue
    print(f"{k}: {'only on src' if y is None else 'only on dst' if x is None else 'definition differs'}")
    for side, t in (("<", x), (">", y)):
        for l in (t or "").splitlines():
            print(f"\t{side} {l}")
PY
}

TS_SETTINGS="select 'hypertable ' || format('%I.%I', hypertable_schema, hypertable_name) || ' dims=' || num_dimensions || ' compression=' || compression_enabled from timescaledb_information.hypertables
union all select 'dimension ' || format('%I.%I', hypertable_schema, hypertable_name) || ' ' || column_name || ' ' || coalesce(time_interval::text, integer_interval::text) from timescaledb_information.dimensions
union all select 'compression ' || hypertable::text || ' segmentby=' || coalesce(segmentby, '-') || ' orderby=' || coalesce(orderby, '-') from timescaledb_information.hypertable_compression_settings
union all select 'job ' || proc_name || ' ' || coalesce(format('%I.%I', hypertable_schema, hypertable_name), '-') || ' every ' || schedule_interval || ' ' || coalesce((config - 'hypertable_id' - 'mat_hypertable_id')::text, '') from timescaledb_information.jobs where job_id >= 1000
union all select 'cagg ' || format('%I.%I', view_schema, view_name) || ' materialized_only=' || materialized_only from timescaledb_information.continuous_aggregates"

check_schema() {
  local spec db start=$diffs flags line tmp
  flags="--schema-only --no-owner --no-privileges --no-publications --no-subscriptions --exclude-schema='_timescaledb*' --exclude-schema='timescaledb_*'"
  tmp=$(mktemp -d)
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    dump src "$db" "$flags" >"$tmp/src.sql"
    dump dst "$db" "$flags" >"$tmp/dst.sql"
    schema_diff "$tmp/src.sql" "$tmp/dst.sql" >"$tmp/diff" || die "schema: $db: the dump comparator failed, so nothing was compared"
    while IFS= read -r line; do
      case $line in $'\t'*) echo "  ${line#$'\t'}" ;; ?*) diff_row "$db schema $line" ;; esac
    done <"$tmp/diff"
    compare_rows "$db database" "$(q src "$db" "$FACTS_SQL" | cut -d'|' -f6-8)" "$(q dst "$db" "$FACTS_SQL" | cut -d'|' -f6-8)"
    compare_rows "$db extension" "$(q src "$db" "select extname || ' ' || extversion from pg_extension")" "$(q dst "$db" "select extname || ' ' || extversion from pg_extension")"
    if has_ext src "$db"; then compare_rows "$db timescale" "$(q src "$db" "$TS_SETTINGS")" "$(q dst "$db" "$TS_SETTINGS")"; fi
  done
  rm -rf "$tmp"
  echo "schema: ${#DATABASES[@]} database(s) dumped on both sides: $((diffs - start)) DIFF"
}

cmd_schema() {
  check_schema
  finish_check
}

is_hypertable() { local t; for t in ${HYPERTABLES[@]+"${HYPERTABLES[@]}"}; do [[ $t == "$1" ]] && return 0; done; return 1; }

hash_meta() {
  q "$FROM" "$1" "select format('%I.%I', n.nspname, c.relname) || '|' || coalesce((select string_agg(quote_ident(a.attname), ',' order by k.o) from pg_index i cross join unnest(i.indkey) with ordinality k(att, o) join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.att where i.indrelid = c.oid and i.indisprimary), '') || '|' || coalesce((select format_type(a.atttypid, null) from pg_index i join pg_attribute a on a.attrelid = i.indrelid and a.attnum = i.indkey[0] where i.indrelid = c.oid and i.indisprimary), '') || '|' || greatest(1, ceil(greatest(c.reltuples, 0) / $HASH_BUCKET))::bigint from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind = 'r' and $NS order by 1"
}

# Both sides must answer: an empty side would compare as zero buckets and pass.
hash_wait() {
  local rs=0 rd=0
  wait "$3" || rs=$?
  wait "$4" || rd=$?
  ((rs == 0 && rd == 0)) || die "hash: $1 $2: the $( ((rs)) && echo source)$( ((rs && rd)) && echo ' and ')$( ((rd)) && echo target) query failed (exit $rs/$rd); nothing was compared"
}

ROW_SUM="sum(('x' || left(md5(t::text), 16))::bit(64)::bigint::numeric)"
HASH_BOUNDS=''

# Catchup, not the subscription, moves a hypertable, so the source runs ahead of the target. hash
# compares the rows up to the target's max key; verify compares the rest exactly under the fence.
ht_bound() {
  local db=$1 table=$2 f max floor
  f=$(ht_state "$table")/event_floor
  floor=$(cat "$f" 2>/dev/null || true)
  [[ $floor =~ ^[0-9]+$ ]] || { diff_row "$db $table: no backfill floor in $f, so nothing bounds the hash; run backfill and catchup"; return 1; }
  max=$(q dst "$db" "select max($HT_KEY) from $table") || die "hash: $db $table: the target's max($HT_KEY) query failed; nothing was compared"
  if [[ ! $max =~ ^[0-9]+$ ]] || ((max < floor)); then
    diff_row "$db $table: the target's max $HT_KEY ${max:-NULL} is below the backfill floor $floor; run catchup"
    return 1
  fi
  HT_BOUND=$max
}

hash_table() {
  local db=$1 table=$2 pk=$3 type=$4 nb=$5 since=${6:-} bexpr where='' key tmp b n bad s d lo sc ss dc ds bound=''
  key=${pk%%,*}
  if is_hypertable "$table"; then
    ht_bound "$db" "$table" || return 0
    bound=$HT_BOUND
    HASH_BOUNDS+="$table $bound"$'\n'
    [[ -n $since ]] || since=$(ts_cutoff)
    bexpr="(t.$HT_KEY / $HASH_BUCKET)" where="where t.$HT_TIME >= $(lit "$since") and t.$HT_KEY <= $bound"
  elif [[ -z $pk ]]; then
    bexpr=0::int
  elif [[ $type == smallint || $type == integer || $type == bigint ]]; then
    bexpr="(t.$key / $HASH_BUCKET)"
  else
    bexpr="(abs(hashtext(t.$key::text)::bigint) % $nb)"
  fi
  # The planner reads (key / bucket) as one group per row and sorts the whole table, which spilled
  # past the source's temp_file_limit on 17.9M rows; the real buckets fit a hash table in memory.
  sel() { echo "set enable_sort = off; select $bexpr || '|' || count(*) || '|' || $ROW_SUM from $table t $1 group by $bexpr order by 1"; }
  tmp=$(mktemp -d)
  q src "$db" "$(sel "$where")" >"$tmp/s" & s=$!
  q dst "$db" "$(sel "$where")" >"$tmp/d" & d=$!
  hash_wait "$db" "$table" $s $d
  bad=$(join -t'|' -a1 -a2 -e- -o 0,1.2,1.3,2.2,2.3 <(sort -t'|' -k1,1 "$tmp/s") <(sort -t'|' -k1,1 "$tmp/d") | awk -F'|' '($2 "") != ($4 "") || ($3 "") != ($5 "") { print $1 }' | paste -sd, -)
  n=$(wc -l <"$tmp/s" | tr -d ' ')
  [[ -z $bound ]] || bound=", $HT_KEY <= $bound: src $(awk -F'|' '{ r += $2 } END { print r + 0 }' "$tmp/s") rows, dst $(awk -F'|' '{ r += $2 } END { print r + 0 }' "$tmp/d") rows"
  if [[ -n $bad ]]; then
    sleep "$HASH_RECHECK_SECS"
    # The recheck reads the buckets the first pass grouped: key / bucket truncates toward zero, so
    # bucket b holds keys from (b-1)*bucket to (b+1)*bucket, exclusive; the range lets an index help.
    if [[ $bexpr == "(t.$key / $HASH_BUCKET)" || $bexpr == "(t.$HT_KEY / $HASH_BUCKET)" ]]; then
      local col=${bexpr#(t.} ranges=
      col=${col%% *}
      # At the ends of bigint the bound would wrap in bash; there the bucket has no bound on that side.
      local edge=$((9223372036854775807 / HASH_BUCKET)) r
      for lo in ${bad//,/ }; do
        r=true
        ((lo - 1 < -edge)) || r="$r and t.$col > $(((lo - 1) * HASH_BUCKET))"
        ((lo + 1 > edge)) || r="$r and t.$col < $(((lo + 1) * HASH_BUCKET))"
        ranges="$ranges${ranges:+ or }($r)"
      done
      where="${where:-where true} and ($ranges)"
    fi
    where="${where:-where true} and $bexpr in ($bad)"
    q src "$db" "$(sel "$where")" >"$tmp/s" & s=$!
    q dst "$db" "$(sel "$where")" >"$tmp/d" & d=$!
    hash_wait "$db" "$table" $s $d
    while IFS='|' read -r b sc ss dc ds; do
      [[ $sc == "$dc" && $ss == "$ds" ]] || diff_row "$db $table bucket $b: src $sc rows, dst $dc rows, sums differ twice $HASH_RECHECK_SECS s apart"
    done < <(join -t'|' -a1 -a2 -e- -o 0,1.2,1.3,2.2,2.3 <(sort -t'|' -k1,1 "$tmp/s") <(sort -t'|' -k1,1 "$tmp/d"))
  fi
  rm -rf "$tmp"
  echo "hash: $db $table $n bucket(s)$bound${bad:+, first pass differed in bucket(s) $bad}"
}

# Under the fence: each hypertable's rows above the last full hash's bound, less the margin catchup
# rereads for ids committed out of order, hashed exactly on both sides.
check_tail() {
  local table bound lo tq tmp s d a b
  for table in ${HYPERTABLES[@]+"${HYPERTABLES[@]}"}; do
    bound=$(awk -v t="$table" '$1 == t { print $2 }' "$WORK/hash-bounds" 2>/dev/null || true)
    if [[ ! $bound =~ ^[0-9]+$ ]]; then diff_row "$table: no bound in $WORK/hash-bounds from a 0-DIFF full hash; run hash $CLUSTER"; continue; fi
    lo=$((bound - CATCHUP_MARGIN))
    tq="select count(*) || '|' || coalesce($ROW_SUM, 0) from $table t where t.$HT_KEY > $lo and t.$HT_TIME >= $(lit "$(ts_cutoff)")"
    tmp=$(mktemp -d)
    q src "$HT_DB" "$tq" >"$tmp/s" & s=$!
    q dst "$HT_DB" "$tq" >"$tmp/d" & d=$!
    hash_wait "$HT_DB" "$table" $s $d
    a=$(cat "$tmp/s") b=$(cat "$tmp/d")
    rm -rf "$tmp"
    if [[ -n $a && $a == "$b" ]]; then echo "verify: $table ${a%%|*} row(s) above $HT_KEY $lo (hash bound $bound less $CATCHUP_MARGIN) hash equal on both"
    else diff_row "$table rows above $HT_KEY $lo (hash bound $bound less $CATCHUP_MARGIN): src ${a%%|*} rows, dst ${b%%|*} rows, sums ${a#*|} and ${b#*|}"; fi
  done
}

hash_age() {
  local f=$WORK/hash-ok age
  if [[ ! -s $f ]]; then diff_row "hash: no 0-DIFF full hash recorded in $f"; return 0; fi
  if [[ -z $(cut -s -d' ' -f2 "$f") ]]; then diff_row "hash: $f is in the one-field form from before the fence field, which no switch takes; run hash $CLUSTER"; return 0; fi
  age=$(( ($(date +%s) - $(awk '{ print $1 }' "$f")) / 3600 ))
  if ((age >= HASH_MAX_AGE_HOURS)); then diff_row "hash: the last 0-DIFF full hash is ${age} h old (limit $HASH_MAX_AGE_HOURS h)"; else echo "hash: last 0-DIFF full hash ${age} h ago"; fi
}

# One line per plain table: its and its descendants' n_tup_ins+upd+del and oid:filenode (TRUNCATE and
# rewrites take a new file node). The # line changes on a restart, a stats reset (pg_stat_reset_single_table_counters
# stamps the database's) or the reset a crash makes (pg_stat_wal's).
HASH_SIG="with recursive d(root, rel) as (select c.oid, c.oid from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind = 'r' and $NS union all select d.root, i.inhrelid from d join pg_inherits i on i.inhparent = d.rel)
select '#|' || pg_postmaster_start_time() || '|' || coalesce((select stats_reset::text from pg_stat_database where datname = current_database()), '-') || '|' || coalesce((select stats_reset::text from pg_stat_wal), '-') || '|' || current_setting('track_counts')
union all (select format('%I.%I', n.nspname, c.relname) || '|' || sum(coalesce(s.n_tup_ins, 0) + coalesce(s.n_tup_upd, 0) + coalesce(s.n_tup_del, 0)) || '|' || string_agg(r.oid || ':' || coalesce(pg_relation_filenode(r.oid)::text, '-'), ',' order by r.oid)
  from d join pg_class c on c.oid = d.root join pg_namespace n on n.oid = c.relnamespace join pg_class r on r.oid = d.rel left join pg_stat_all_tables s on s.relid = d.rel group by n.nspname, c.relname order by n.nspname, c.relname)"
# A session that is not idle may hold committed writes its counters do not show yet; a prepared
# transaction commits rows no session holds. Replication workers and the Timescale scheduler write no user table here.
HASH_BUSY="select (select count(*) from pg_stat_activity where datname = current_database() and pid <> pg_backend_pid()
  and backend_type not in ('walsender', 'autovacuum worker', 'logical replication apply worker', 'logical replication tablesync worker', 'logical replication parallel worker', 'TimescaleDB Background Worker Scheduler')
  and not (state is not distinct from 'idle' and state_change < now() - make_interval(secs => $HASH_IDLE_SECS))) + (select count(*) from pg_prepared_xacts where database = current_database())"

# sig_line <prefix> [file]: the one signature line that starts with the prefix.
sig_line() { awk -v p="$1" 'index($0, p) == 1' "${2:--}"; }

# hash_lock: an exclusive flock on fd 9, held until this run's last copy of fd 9 closes; no ssh master gets one.
hash_lock() { perl -MFcntl=:flock -e 'open(my $f, ">&=", 9) or exit 2; flock($f, LOCK_EX | LOCK_NB) or exit 1'; }

# hash_quiet <db>: 0 once no session on either side may hold writes its counters do not show yet.
hash_quiet() {
  local db=$1 end busy
  end=$(($(date +%s) + HASH_SETTLE_SECS))
  while :; do
    busy=$(($(q src "$db" "$HASH_BUSY") + $(q dst "$db" "$HASH_BUSY")))
    ((busy)) || break
    if (($(date +%s) >= end)); then
      echo "hash: $db: $busy session(s) not idle for $HASH_IDLE_SECS s, or prepared transaction(s), after $HASH_SETTLE_SECS s; every table hashed"
      return 1
    fi
    sleep 2
  done
}

# hash_same_run <db>: 0 if neither side restarted, reset its statistics or stopped counting since the full hash.
hash_same_run() {
  local db=$1 side now
  [[ -s $WORK/hash-sig ]] || { echo "hash: $db: no $WORK/hash-sig from a 0-DIFF full hash; every table hashed"; return 1; }
  for side in src dst; do
    now=$(sig_line "$side $db #|" <<<"$HASH_NOW")
    if [[ $now != *'|on' || $now != "$(sig_line "$side $db #|" "$WORK/hash-sig")" ]]; then
      echo "hash: $db: the $side restarted, reset its statistics or does not track counts since the last full hash; every table hashed"
      return 1
    fi
  done
}

cmd_hash() {
  local only=${ARGS[0]:-} spec db table pk type nb start skip side a b
  # The fence this hash began under, if one had finished: switch takes only a hash whose fence is
  # still the current one, never a time compared to the fence's.
  start="$(date +%s) $(cat "$WORK/fence-ok" 2>/dev/null || echo -)"
  # Two runs' lines in one log stay apart: this line and the EXIT trap's end line carry the same id.
  RUN_ID="$(hostname -s)-$$-$(date -u +%Y%m%dT%H%M%SZ)"
  echo "hash: run $RUN_ID started $(date -u +%FT%TZ): $CLUSTER${only:+ $only}$( ((MOVED)) && echo ' --moved')$( ((AGE)) && echo ' --age')"
  if ((AGE)); then hash_age; finish_check; return 0; fi
  [[ -z $only ]] || ((MOVED == 0)) || die "hash: --moved hashes every table it cannot skip; it takes no table"
  if ((DRY == 0)); then
    # One hash at a time per cluster: an older run ending after a newer one would put its pass over
    # the newer run's, or its stale pass over the newer run's DIFF.
    mkdir -p "$WORK" || die "cannot make $WORK"
    exec 9>>"$WORK/hash.lock" || die "cannot open $WORK/hash.lock"
    hash_lock || die "hash: run $(cat "$WORK/hash-run" 2>/dev/null || echo '?') holds $WORK/hash.lock; wait for it to end, or stop it, then rerun"
    state "$WORK/hash-run" "$RUN_ID pid $$"
    # A full run's pass is the only thing that may stand after it: one that stops for any reason leaves none.
    [[ -n $only ]] || rm -f "$WORK/hash-ok"
  fi
  HASH_NOW=''
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    skip=0
    if ((MOVED)) && hash_quiet "$db"; then skip=1; fi
    # Read before any row of this db is: a write the hash does not see moves a counter after this.
    for side in src dst; do
      HASH_NOW+=$(q "$side" "$db" "$HASH_SIG" | sed "s/^/$side $db /")$'\n'
    done
    if ((skip)) && ! hash_same_run "$db"; then skip=0; fi
    while IFS='|' read -r table pk type nb; do
      [[ -n $table && ( -z $only || $only == "$table" ) ]] || continue
      if ((skip)) && ! is_hypertable "$table"; then
        a=$(sig_line "src $db $table|" <<<"$HASH_NOW") b=$(sig_line "dst $db $table|" <<<"$HASH_NOW")
        if [[ -n $a && -n $b && $a == "$(sig_line "src $db $table|" "$WORK/hash-sig")" && $b == "$(sig_line "dst $db $table|" "$WORK/hash-sig")" ]]; then
          echo "hash: $db $table unchanged on both sides since the last full hash (write counters and file nodes); not reread"
          continue
        fi
      fi
      hash_table "$db" "$table" "$pk" "$type" "$nb"
    done < <(hash_meta "$db")
  done
  if [[ -z $only && $diffs == 0 ]]; then
    query_failures
    state "$WORK/hash-bounds" "$HASH_BOUNDS"
    state "$WORK/hash-sig" "$HASH_NOW"
    state "$WORK/hash-ok" "$start"
  fi
  finish_check
}

VERIFY_TABLES="with t as (
  select c.oid, n.nspname, c.relname, c.reltuples::bigint as est, pg_relation_size(c.oid) as bytes,
    (select r.srsubstate::text from pg_subscription_rel r where r.srrelid = c.oid and r.srsubstate <> 'r') as sync,
    (select a.attname from pg_index i join pg_attribute a on a.attrelid = i.indrelid and a.attnum = i.indkey[0] where i.indrelid = c.oid and i.indisprimary) as pk
  from pg_class c join pg_namespace n on n.oid = c.relnamespace
  where c.relkind = 'r' and $NS
)
select format('%I.%I', nspname, relname),
  case when sync is not null then 'syncing:' || sync when bytes < $EXACT_COUNT_BYTES then (xpath('/table/row/v/text()', query_to_xml(format('select count(*) as v from %I.%I', nspname, relname), false, false, '')))[1]::text when est >= 0 then '~' || est else '~' || pg_size_pretty(bytes) end,
  case when sync is not null or pk is null then '-' else coalesce((xpath('/table/row/v/text()', query_to_xml(format('select %I::text as v from %I.%I order by %I desc limit 1', pk, nspname, relname, pk), false, false, '')))[1]::text, '-') end
from t order by 1"

# at_least <source value> <target value>: equal, or both integers with the target not below.
at_least() { [[ $1 == "$2" ]] || { [[ $1 =~ ^-?[0-9]+$ && $2 =~ ^-?[0-9]+$ ]] && (($2 >= $1)); }; }

slot_seen() {
  local deadline=$((SECONDS + 60)) v
  while v=$(q src "$2" "select coalesce(bool_and(confirmed_flush_lsn >= '$3'), false) from pg_replication_slots where slot_name = '$1'") || return 1; [[ $v != t ]]; do
    ((SECONDS < deadline)) || return 1
    sleep 1
  done
}

check_tables() {
  local spec db shape start rows name
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec") start=$diffs
    local live=0 srows lsn
    # Before the fence the source keeps writing. preflight reads the source, waits for the slot to
    # confirm an LSN past that read, then reads the target: the target must hold at least what the
    # source showed. Anything else compares exactly.
    [[ $CMD == preflight && $shape != upstream=* && $FROM == src ]] && live=1
    srows=$(q "$FROM" "$db" "$VERIFY_TABLES" | sort -t'|' -k1,1)
    if ((live)); then
      lsn=$(q src "$db" "select pg_current_wal_lsn()")
      slot_seen "$(sub_name fwd "$db")" "$db" "$lsn" || diff_row "$db: slot $(sub_name fwd "$db") did not confirm $lsn in 60 s, so the target read below may be behind"
    fi
    rows=$(join -t'|' -a1 -a2 -e- -o 0,1.2,2.2,1.3,2.3 <(awk 'NF' <<<"$srows") <(q "$TO" "$db" "$VERIFY_TABLES" | sort -t'|' -k1,1))
    while IFS='|' read -r table a b pa pb; do
      [[ -n $table ]] || continue
      if is_hypertable "$table"; then continue; fi
      if ((live)) && [[ $b != syncing:* && $a != - && $b != - ]] && at_least "$pa" "$pb" && { [[ $a == "~"* || $b == "~"* ]] || at_least "$a" "$b"; }; then continue; fi
      if [[ $b == syncing:* || $a == - || $b == - || $pa != "$pb" ]] || { [[ $a != "~"* && $b != "~"* && $a != "$b" ]]; }; then
        diff_row "$db $table: $FROM rows $a max key $pa, $TO rows $b max key $pb"
      fi
    done <<<"$rows"
    if [[ $CMD == preflight ]]; then
      # The source keeps taking ids until the fence; switch's setval copies the values, so before it
      # only a sequence missing on one side, or a target ahead of the source, is a fault.
      local sseq dseq
      sseq=$(q "$FROM" "$db" "$SEQ_ROWS" | cut -d'|' -f1,2,5) dseq=$(q "$TO" "$db" "$SEQ_ROWS" | cut -d'|' -f1,2)
      compare_rows "$db sequence" "$(cut -d'|' -f1 <<<"$sseq")" "$(cut -d'|' -f1 <<<"$dseq")"
      # A descending sequence (negative increment) is ahead when its next value is lower.
      while IFS='|' read -r name a inc b; do
        [[ $a =~ ^-?[0-9]+$ && $b =~ ^-?[0-9]+$ && $inc =~ ^-?[0-9]+$ ]] && (((b - a) * inc > 0)) && diff_row "$db sequence $name: $TO next $b is ahead of $FROM next $a"
      done < <(join -t'|' <(sort -t'|' -k1,1 <<<"$sseq") <(sort -t'|' -k1,1 <<<"$dseq"))
      echo "preflight: $db: $(join -t'|' <(sort -t'|' -k1,1 <<<"$sseq") <(sort -t'|' -k1,1 <<<"$dseq") | awk -F'|' '$2 != $4 { n++ } END { print n + 0 }') sequence(s) behind the source, which switch's setval closes"
    else
      compare_rows "$db sequence" "$(q "$FROM" "$db" "$SEQ_ROWS" | cut -d'|' -f1,2)" "$(q "$TO" "$db" "$SEQ_ROWS" | cut -d'|' -f1,2)"
    fi
    echo "verify: $db $(grep -c . <<<"$rows") tables and the sequences: $((diffs - start)) DIFF"
  done
  local table qh a b top pending
  for table in ${HYPERTABLES[@]+"${HYPERTABLES[@]}"}; do
    if [[ $CMD == preflight && $FROM == src ]]; then
      # What fence's catchup will copy, by its own predicates: source rows above the target's top key.
      top=$(q "$TO" "$HT_DB" "select coalesce(max($HT_KEY), 0) from $table")
      pending=$(q src "$HT_DB" "select count(*) from $table where $HT_KEY > $top and $HT_TIME >= $(lit "$(ts_cutoff)")")
      if ((pending > MAX_CATCHUP_ROWS)); then diff_row "$table: $pending source row(s) above the target's top $HT_KEY $top wait for catchup (limit $MAX_CATCHUP_ROWS)"; continue; fi
    fi
    qh="select coalesce(max($HT_KEY)::text, '-') || ' ' || count(*) from $table where $HT_TIME >= date_trunc('day', now()) - interval '1 day'"
    a=$(q "$FROM" "$HT_DB" "$qh") b=$(q "$TO" "$HT_DB" "$qh")
    if [[ $a == "$b" ]]; then echo "verify: $table max $HT_KEY and rows since yesterday $a on both"
    elif [[ $CMD == preflight && $FROM == src ]] && at_least "${b% *}" "${a% *}" && at_least "${b#* }" "${a#* }"; then
      # Hypertables move by catchup, not the subscription: fence's catchup copies the rows above the target's max.
      echo "preflight: $table: $pending source row(s) above the target's top $HT_KEY $top; fence's catchup copies them (limit $MAX_CATCHUP_ROWS)"
    else diff_row "$table max $HT_KEY and rows since yesterday: $FROM $a, $TO $b"; fi
  done
}

# The reverse catchup carries only hypertable rows above the switch's floor, and the tail check reads
# only rows above the last hash bound: an UPDATE or DELETE below it on the target during its tenure
# would pass both. So the reverse hashes every hypertable row a retention job on either side cannot
# have dropped yet (newer than TS_RETAIN_DAYS less a day, and not before the backfill cutoff).
hash_hypertables() {
  local table since
  for table in ${HYPERTABLES[@]+"${HYPERTABLES[@]}"}; do
    since=$(q dst "$HT_DB" "select greatest($(lit "$(ts_cutoff)")::timestamptz, date_trunc('day', now()) - interval '$((TS_RETAIN_DAYS - 1)) days')")
    [[ -n $since ]] || die "hash: $table: the target answered no lower time bound"
    hash_table "$HT_DB" "$table" "$HT_KEY" bigint 1 "$since"
  done
}

verify_checks() {
  check_tables
  check_schema
  DRIFT_ONLY=$REVERSE check_grants
  if ((REVERSE)); then hash_hypertables; else hash_age; fi
  check_tail
}

cmd_verify() {
  verify_checks
  finish_check
}

LOGIN_ROLES="select rolname from pg_roles where rolcanlogin and rolname not in ('postgres', '$REPL_ROLE') and rolname !~ '^pg_' order by 1"

# The roles fence takes LOGIN from, first read on the source while they still log in. fence adds
# any role that can log in on its side now, so a role made since is fenced too.
login_roles() {
  local f=$WORK/login-roles roles='' live
  [[ ! -s $f ]] || roles=$(awk 'NF' "$f" | sort -u)
  if [[ -z $roles ]]; then
    roles=$(q src postgres "$LOGIN_ROLES")
    [[ -n $roles ]] || die "no role but postgres and $REPL_ROLE can log in on src, so fence would end no session; refusing an empty role list"
  fi
  if [[ $CMD == fence ]]; then
    live=$(q "$FROM" postgres "$LOGIN_ROLES")
    roles=$(printf '%s\n%s\n' "$roles" "$live" | grep . | sort -u)
  fi
  [[ -s $f && $(awk 'NF' "$f" | sort -u) == "$roles" ]] || state "$f" "$roles"
  printf '%s\n' "$roles"
}

sessions() { q "$1" postgres "select count(*) from pg_stat_activity where usename = $(lit "$2")"; }

# Run once the role is NOLOGIN, so no session counted here can be replaced by a new one.
end_sessions() {
  local side=$1 role=$2 n deadline=$((SECONDS + LAG_WAIT_SECS))
  n=$(sessions "$side" "$role")
  if ((DRY)); then ((n == 0)) || changed "$side: $n session(s) of $role ended"; return 0; fi
  until ((n == 0)); do
    ((SECONDS < deadline)) || die "fence: $n session(s) of $role still on $side $LAG_WAIT_SECS s after NOLOGIN"
    apply "$side" postgres "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $(lit "$role");" "$side: $n session(s) of $role ended"
    sleep 0.5
    n=$(sessions "$side" "$role")
  done
}

fence_ok() { if ((REVERSE)); then echo "$WORK/fence-ok.rev"; else echo "$WORK/fence-ok"; fi; }
fenced_file() { if ((REVERSE)); then echo "$WORK/fenced.rev"; else echo "$WORK/fenced"; fi; }
# One id per continuous fence of a side: made by the first fence, kept by its reruns, gone once that
# side may log in again. fence-ok, hash-ok and drained-<sub> name it.
fence_id() { if ((REVERSE)); then echo "$WORK/fence-id.rev"; else echo "$WORK/fence-id"; fi; }
# unfence <rev|fwd>: that side's fence ends, so nothing recorded under it holds any more.
unfence() {
  local x='' spec
  [[ $1 == rev ]] && x=.rev
  ((DRY)) && return 0
  for spec in "${DATABASES[@]}"; do rm -f "$WORK/drained-$(sub_name "$1" "$(db_of "$spec")")"; done
  rm -f "$WORK/fence-ok$x" "$WORK/fence-id$x"
}

# Every change on FROM has reached TO: the slot of each database confirmed FROM's current LSN. A
# subscription already off counts only if a switch waited on it and turned it off under this fence.
from_slots() {
  local spec db sub
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    if ((REVERSE)); then sub=$(sub_name rev "$db"); else sub=$(sub_name fwd "$db"); fi
    if [[ $(q "$TO" "$db" "select subenabled from pg_subscription where subname = $(lit "$sub")") == f ]]; then
      if [[ -s $(fence_id) && $(cat "$WORK/drained-$sub" 2>/dev/null) == "$(cat "$(fence_id)")" ]]; then
        echo "$CMD: $TO $db: $sub was drained and turned off under fence $(cat "$(fence_id)")"
      else
        blocked "$TO $db: $sub is disabled and no switch drained it under this fence, so rows on $FROM may not have reached $TO; enable it, let it catch up, and rerun"
      fi
      continue
    fi
    slot_wait "$FROM" "$sub" "$db"
  done
}

# Before the reverse direction carries the last rows back, it carries every table the target
# publishes, each in state r. A table sync a refresh just started is waited on, up to LAG_WAIT_SECS.
reverse_ready() {
  local spec db sub counts total missing deadline=$((SECONDS + LAG_WAIT_SECS)) left
  # A blocker already known fails the step now: waiting for the subscription would only lengthen the
  # outage before the same verdict.
  ((blocks == 0)) || return 0
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") sub=$(sub_name rev "$db")
    [[ -n $(q src "$db" "select 1 from pg_subscription where subname = $(lit "$sub")") ]] || { blocked "src $db: no reverse subscription $sub"; continue; }
    while :; do
      IFS='|' read -r counts total <<<"$(ready_count src "$db" "$sub")"
      missing=$(only_in "$(q dst "$db" "$(pub_tables_sql "$REV_PUB")")" "$(q src "$db" "$(sub_tables_sql "$sub")")" | awk 'NF { n++ } END { print n + 0 }')
      [[ $counts == 0 && $missing == 0 ]] && break
      left=$((deadline - SECONDS))
      if ((DRY || left <= 0)); then
        blocked "src $db: $counts of $total tables of $sub not in state r, $missing table(s) of $REV_PUB on dst not in it$( ((DRY)) || echo " after $LAG_WAIT_SECS s")"
        break
      fi
      echo "$CMD: src $db: $counts of $total tables of $sub not in state r, $missing not in it; waiting (${left} s left)"
      sleep 2
    done
  done
}

kept() {
  local k
  for k in ${FENCE_KEEP[@]+"${FENCE_KEEP[@]}"}; do [[ $k == "$1" ]] && return 0; done
  [[ $(q "$FROM" postgres "select rolsuper from pg_roles where rolname = $(lit "$1")") == t ]]
}

upstream_blockers() {
  local spec
  for spec in "${DATABASES[@]}"; do
    [[ $(shape_of "$spec") != upstream=* ]] || blocked "$(db_of "$spec") is fed by its upstream publisher; $1 happens there, not here"
  done
}

slot_wait() {
  local side=$1 slot=$2 db=$3 target deadline=$((SECONDS + LAG_WAIT_SECS))
  ((DRY)) && { echo "  wait until slot $slot on $side confirms the current LSN"; return 0; }
  target=$(q "$side" "$db" "select pg_current_wal_lsn()")
  until [[ $(q "$side" "$db" "select coalesce(bool_and(confirmed_flush_lsn >= '$target'), false) from pg_replication_slots where slot_name = '$slot'") == t ]]; do
    ((SECONDS < deadline)) || die "slot $slot on $side has not confirmed $target in $LAG_WAIT_SECS s"
    sleep 1
  done
  echo "$CMD: slot $slot on $side confirmed past $target"
}

is_super() { [[ $(q "$1" postgres "select rolsuper from pg_roles where rolname = $(lit "$2")") == t ]]; }

# A kept role logs in through the fence, so it must not be able to write an application table: not
# by a table or column grant, not through any role it may SET ROLE to (INHERIT FALSE included), not
# through a SECURITY DEFINER function whose owner can write. fence and switch both run this, so a
# grant made after the fence still blocks the switch.
keep_writers() {
  local k spec db w l
  for k in ${FENCE_KEEP[@]+"${FENCE_KEEP[@]}"}; do
    [[ -n $(q "$FROM" postgres "select 1 from pg_roles where rolname = $(lit "$k")") ]] || continue
    if is_super "$FROM" "$k"; then blocked "$FROM: $k is in FENCE_KEEP and is a superuser; FENCE_KEEP holds read-only roles only"; continue; fi
    l=$(lit "$k")
    for spec in "${DATABASES[@]}"; do
      db=$(db_of "$spec")
      w=$(q "$FROM" "$db" "with r as (select oid, rolname, rolsuper from pg_roles where pg_has_role($l, oid, 'MEMBER')),
        t as (select c.oid, format('%I.%I', n.nspname, c.relname) as name from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind in ('r', 'p') and $NS),
        w as (select 'as superuser ' || r.rolname as how from r where r.rolsuper
          union all select t.name || case when r.rolname = $l then '' else ' as ' || r.rolname end from r cross join t
            where has_table_privilege(r.oid, t.oid, 'INSERT, UPDATE, DELETE, TRUNCATE') or has_any_column_privilege(r.oid, t.oid, 'INSERT, UPDATE')
          union all select 'through SECURITY DEFINER ' || p.oid::regprocedure::text || ' owned by ' || pg_get_userbyid(p.proowner) from pg_proc p join pg_namespace n on n.oid = p.pronamespace
            where p.prosecdef and $NS and not exists (select 1 from pg_depend e where e.classid = 'pg_proc'::regclass and e.objid = p.oid and e.deptype = 'e')
              and exists (select 1 from r where has_function_privilege(r.oid, p.oid, 'EXECUTE'))
              and exists (select 1 from t where has_table_privilege(p.proowner, t.oid, 'INSERT, UPDATE, DELETE, TRUNCATE') or has_any_column_privilege(p.proowner, t.oid, 'INSERT, UPDATE')))
        select count(*) || ' way(s), first ' || coalesce(min(how), '-') from w")
      [[ $w == 0\ * ]] || blocked "$FROM $db: $k is in FENCE_KEEP and can write: $w; FENCE_KEEP holds read-only roles only, so revoke its writes or let fence take its LOGIN"
    done
  done
}

# The Timescale scheduler runs jobs as their owner, through any fence; scheduled jobs on a side
# whose timescaledb.max_background_workers is above 0 write there.
ts_running() {
  local side=$1 spec db n
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    has_ext "$side" "$db" || continue
    n=$(q "$side" "$db" "select case when coalesce(nullif(current_setting('timescaledb.max_background_workers', true), ''), '0')::int > 0 then (select count(*) from timescaledb_information.jobs where job_id >= 1000 and scheduled) else 0 end")
    ((n == 0)) || echo "$db $n"
  done
}

# REOPEN is set once fence --reverse may have taken LOGIN on the target, the primary since the switch:
# a run that stops after that gives it back, so a rollback that cannot finish leaves the target writable.
login_back() { printf 'ALTER ROLE %s LOGIN;\n' "$(ident "$1")" | sql dst rw postgres >/dev/null || return 1; }
reopen_target() {
  local rc=$1 f=$WORK/fenced.rev role n=0 bad=0
  ((REOPEN && rc != 0)) && [[ ${BASHPID:-$$} == "$PGMOVE_PID" && -s $f ]] || return 0
  while IFS= read -r role; do
    [[ -z $role ]] && continue
    if login_back "$role"; then n=$((n + 1)); else bad=1; fi
  done <"$f"
  if ((bad)); then
    echo "pgmove: fence --reverse stopped after taking LOGIN on the target and could not give all of it back; the target is fenced: run thaw $CLUSTER --reverse" >&2
    return 0
  fi
  unfence rev
  rm -f "$f"
  echo "pgmove: fence --reverse stopped after taking LOGIN on the target; LOGIN given back to $n role(s) there, so the target takes writes again" >&2
}

cmd_fence() {
  local role can roles fenced spec ts
  fenced=$(fenced_file)
  upstream_blockers fencing
  ((blocks == 0)) || finish
  keep_writers
  if ((REVERSE)); then
    # Every step that can fail on the source runs while the target still takes writes. The pass after
    # the NOLOGIN below catches a partition made in between.
    for spec in "${DATABASES[@]}"; do sync_partitions dst src "$(db_of "$spec")" "$(sub_name rev "$(db_of "$spec")")" "$REV_PUB"; done
    needs_first "the target keeps its LOGIN until the source has its partitions"
    reverse_ready
    ((blocks == 0)) || finish "the target keeps its LOGIN"
    # Its jobs run as their owner through the fence; thaw --reverse and the next switch schedule them again.
    for spec in "${DATABASES[@]}"; do unschedule_timescale "$(db_of "$spec")"; done
  else
    ts=$(ts_running src)
    [[ -z $ts ]] || blocked "src: scheduled Timescale jobs run there and write through the fence (database and jobs: $ts); unschedule them, or set timescaledb.max_background_workers = 0 there"
  fi
  ((blocks == 0)) || finish
  ((DRY)) || rm -f "$(fence_ok)"
  # A rerun after switch --reverse passed its stamp fences a target that is no longer the primary: that one stays fenced.
  if ((DRY == 0 && REVERSE)) && target_primary; then REOPEN=1; fi
  ((DRY)) || [[ -s $(fence_id) ]] || state "$(fence_id)" "$(date +%s)-$$"
  roles=$(login_roles)
  for role in $roles; do
    kept "$role" && continue
    can=$(q "$FROM" postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")")
    [[ -n $can ]] || { echo "fence: $role is not on $FROM"; continue; }
    if src_by_admin "$FROM"; then
      [[ $can == f && $(sessions "$FROM" "$role") == 0 ]] || need fence "$role"
      continue
    fi
    if [[ $can != f ]]; then
      # The journal first: a run killed after the ALTER commits still lets thaw give the LOGIN back.
      ((DRY)) || { [[ -e $fenced ]] && grep -qxF -- "$role" "$fenced"; } || echo "$role" >>"$fenced"
      apply "$FROM" postgres "SET lock_timeout = '2s'; ALTER ROLE $(ident "$role") NOLOGIN;" "$FROM: $role NOLOGIN"
    fi
    end_sessions "$FROM" "$role"
  done
  needs_first "the slots are waited on once the source roles are fenced"
  if ((REVERSE)); then
    for spec in "${DATABASES[@]}"; do sync_partitions dst src "$(db_of "$spec")" "$(sub_name rev "$(db_of "$spec")")" "$REV_PUB"; done
    needs_first "the slots are waited on once the source has the target's partitions"
    reverse_ready
  fi
  ((blocks == 0)) || finish
  from_slots
  query_failures
  ((blocks || DRY)) || state "$(fence_ok)" "$(cat "$(fence_id)")"
  finish
}

# The way back from a fence that no switch followed. Nothing on the target has been anyone's
# primary, so the source takes over again as it was; after a switch only the reverse path is safe.
# The target may hold writes the source lacks: a switch reached its first LOGIN grant on the target
# and no switch --reverse has reached its first on the source since. Each restamps its file.
target_primary() { [[ -s $WORK/switched ]] && ! { [[ -s $WORK/reversed ]] && (($(cat "$WORK/reversed") > $(cat "$WORK/switched"))); }; }
# switched and reversed order the switches: each is written above the other, even in one second.
switch_stamp() { local t o; t=$(date +%s) o=$(cat "$WORK/$2" 2>/dev/null || echo 0); ((t > o)) || t=$((o + 1)); state "$WORK/$1" "$t"; }

# The way back from a fence --reverse that no switch --reverse finished: the target stays the
# primary. No source role has LOGIN yet, since switch --reverse grants it only after its stamp.
thaw_reverse() {
  local role roles spec db fenced=$WORK/fenced.rev
  target_primary || die "thaw --reverse: no switch since the last switch --reverse, so the source is the primary and the target has nothing to take back"
  if [[ -s $fenced ]]; then
    roles=$(awk 'NF' "$fenced" | sort -u)
    for role in $roles; do
      kept "$role" && continue
      is_super dst "$role" && continue
      [[ $(q dst postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")") == f ]] || continue
      apply dst postgres "ALTER ROLE $(ident "$role") LOGIN;" "dst: $role LOGIN again"
    done
  fi
  # A switch --reverse that stopped part way may have turned the reverse off and the forward on.
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    sub_set dst "$db" "$(sub_name fwd "$db")" f
    slot_ok dst "$db" "$(sub_name rev "$db")" "the reverse subscription" warn && sub_resume src "$db" "$(sub_name rev "$db")"
    schedule_timescale "$db"
  done
  query_failures
  ((DRY || blocks || needs)) || { unfence rev; rm -f "$fenced"; }
  finish "the target takes writes again"
}

cmd_thaw() {
  local role roles spec db fenced=$WORK/fenced
  if ((REVERSE)); then thaw_reverse; return 0; fi
  target_primary && die "thaw: $WORK/switched is newer than any switch --reverse, so the target may hold writes the source lacks; roll back with fence --reverse and switch --reverse"
  unfence fwd
  roles=$(login_roles)
  # Nobody logs in on the target again, and no session of a run stopped part way survives there.
  for role in $roles; do
    kept "$role" && continue
    is_super dst "$role" && continue
    [[ $(q dst postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")") != t ]] ||
      apply dst postgres "SET lock_timeout = '2s'; ALTER ROLE $(ident "$role") NOLOGIN;" "dst: $role NOLOGIN again"
    end_sessions dst "$role"
  done
  for spec in "${DATABASES[@]}"; do unschedule_timescale "$(db_of "$spec")"; done
  for spec in "${DATABASES[@]}"; do sub_set dst "$(db_of "$spec")" "$(sub_name fwd "$(db_of "$spec")")" t; done
  for role in $roles; do
    kept "$role" && continue
    [[ $(q src postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")") == f ]] || continue
    # SRC_VIA=forward: srcexec grants only a role listed in the source's own fenced file.
    if src_by_admin src; then need login "$role"; continue; fi
    if [[ ! -e $fenced ]] || ! grep -qxF -- "$role" "$fenced"; then echo "thaw: $role is NOLOGIN on src but fence did not make it so; left as it is"; continue; fi
    apply src postgres "ALTER ROLE $(ident "$role") LOGIN;" "src: $role LOGIN again"
  done
  needs_first "the target marker goes once the source roles log in again"
  if [[ -n $FREEZE_MARKER ]] && remote_has "$DST_SSH" "$FREEZE_MARKER"; then run "$DST_SSH" "rm -f $FREEZE_MARKER" "$DST_SSH:$FREEZE_MARKER removed"; fi
  query_failures
  ((DRY || blocks)) || rm -f "$fenced"
  finish
}

sub_set() {
  local side=$1 db=$2 sub=$3 want=$4 have
  have=$(q "$side" "$db" "select subenabled from pg_subscription where subname = '$sub'")
  [[ -n $have && $have != "$want" ]] || return 0
  if src_by_admin "$side"; then
    if [[ $want == t ]]; then need enable "$db"; else need disable "$db"; fi
    return 0
  fi
  if [[ $want == t ]]; then apply "$side" "$db" "ALTER SUBSCRIPTION $sub ENABLE;" "$side $db: $sub enabled"; else apply "$side" "$db" "ALTER SUBSCRIPTION $sub DISABLE;" "$side $db: $sub disabled"; fi
}

# A resumed direction must skip the rows the other one applied while it was paused, or it sends
# them back and stops on their keys.
sub_resume() {
  local side=$1 db=$2 sub=$3 origin
  [[ -n $(q "$side" "$db" "select 1 from pg_subscription where subname = '$sub'") ]] || return 0
  origin=$(q "$side" "$db" "select suborigin from pg_subscription where subname = '$sub'")
  if src_by_admin "$side"; then
    # srcexec's enable sets origin = none and enables, both idempotent.
    [[ $origin == none && $(q src "$db" "select subenabled from pg_subscription where subname = '$sub'") == t ]] || need enable "$db"
    return 0
  fi
  [[ $origin == none ]] || apply "$side" "$db" "ALTER SUBSCRIPTION $sub SET (origin = none);" "$side $db: $sub skips rows the other direction applied"
  sub_set "$side" "$db" "$sub" t
}

# A slot a subscription can resume from: present, not lost, with a confirmed position. Resuming
# onto a missing one would let a new slot start past commits nobody carried.
# slot_ok <side> <db> <slot> <what> [warn]: warn prints the reason and leaves the run unblocked.
slot_ok() {
  local side=$1 db=$2 slot=$3 st why=''
  st=$(q "$side" "$db" "select coalesce(wal_status, '-') || '|' || coalesce(confirmed_flush_lsn::text, '-') from pg_replication_slots where slot_name = $(lit "$slot")")
  if [[ -z $st ]]; then why="$side $db: no slot $slot; $4 has nothing to resume from"
  elif [[ $st == lost\|* || $st == *\|- ]]; then why="$side $db: slot $slot is $st (wal_status|confirmed_flush_lsn); $4 cannot resume from it"; fi
  [[ -n $why ]] || return 0
  if [[ ${5:-} == warn ]]; then echo "$CMD: WARNING $why; it stays off, and another switch needs a new one (init, subscribe)"; else blocked "$why"; fi
  return 1
}

cmd_switch() {
  local role spec db table roles n fence sub can
  fence=fence
  ((REVERSE == 0)) || fence='fence --reverse'
  upstream_blockers "the switch"
  roles=$(login_roles)
  for role in $roles; do
    kept "$role" && continue
    n=$(sessions "$FROM" "$role")
    if [[ $(q "$FROM" postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")") != t && $n == 0 ]]; then continue; fi
    if ((DRY)); then echo "dry run: $FROM $role can still log in or holds $n session(s); $fence ends that before a real switch"; else blocked "$FROM: $role can still log in or holds $n session(s); run $fence first"; fi
  done
  if ((DRY == 0)) && [[ ! -s $(fence_ok) ]]; then blocked "no $(fence_ok): $fence has not finished since it last started; run $fence"; fi
  # Under the fence the source is still, so one full hash begun after it covers every row; the
  # live hash before it and verify's tail cannot see an update to an old row.
  if ((DRY == 0 && REVERSE == 0)) && [[ -s $(fence_ok) ]] && [[ $(cut -s -d' ' -f2- "$WORK/hash-ok" 2>/dev/null) != "$(cat "$(fence_ok)")" ]]; then
    blocked "no 0-DIFF full hash begun after fence $(cat "$(fence_ok)") finished; run hash $CLUSTER"
  fi
  # The target's roles were made without passwords; passwords copies the source's verifiers.
  if via_forward && ((REVERSE == 0)) && [[ ! -s $WORK/passwords-ok ]]; then
    if ((DRY)); then echo "dry run: no $WORK/passwords-ok yet; remote.sh run switch runs passwords first"; else blocked "no $WORK/passwords-ok; run deploy/aws/db/remote.sh run passwords $CLUSTER"; fi
  fi
  if ((DRY == 0)) && [[ ! -s $(fence_id) ]]; then blocked "no $(fence_id): run $fence"; fi
  keep_writers
  ((blocks == 0)) || finish
  catchup_tables
  sync_sequences
  needs_first "the subscriptions change once the source holds the target's last rows and sequence values"
  if ((REVERSE)); then
    for spec in "${DATABASES[@]}"; do sync_partitions dst src "$(db_of "$spec")" "$(sub_name rev "$(db_of "$spec")")" "$REV_PUB"; done
    needs_first "the reverse is checked once the source has the target's partitions"
    reverse_ready
  fi
  ((blocks == 0)) || finish
  # Every row FROM took before the fence is on TO before its subscription stops.
  from_slots
  ((blocks == 0)) || finish
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    if ((REVERSE)); then sub=$(sub_name rev "$db"); else sub=$(sub_name fwd "$db"); fi
    ((DRY)) || state "$WORK/drained-$sub" "$(cat "$(fence_id)")"
    if ((REVERSE)); then sub_set src "$db" "$sub" f; else sub_set dst "$db" "$sub" f; fi
  done
  if ((REVERSE)); then
    # One direction at a time: the forward resumes once the source's root steps turned the reverse off.
    needs_first "the forward resumes once the reverse is off"
    for spec in "${DATABASES[@]}"; do
      db=$(db_of "$spec")
      # A lost forward slot costs the next attempt a new forward, not this rollback its LOGIN.
      if ! slot_ok src "$db" "$(sub_name fwd "$db")" "the forward subscription" warn; then continue; fi
      sub_resume dst "$db" "$(sub_name fwd "$db")"
      sync_partitions src dst "$db" "$(sub_name fwd "$db")" "$PUB"
    done
  fi
  if ((REVERSE == 0)); then
    for table in ${HYPERTABLES[@]+"${HYPERTABLES[@]}"}; do
      [[ -s $(ht_state "$table")/event_floor.rev ]] || state "$(ht_state "$table")/event_floor.rev" "$(q dst "$HT_DB" "select coalesce(max($HT_KEY), 0) from $table")"
    done
    for spec in "${DATABASES[@]}"; do schedule_timescale "$(db_of "$spec")"; done
    arm_reverse
    # The target runs no migration while the source can still take its writes back.
    put_marker "$DST_SSH" switched
  fi
  # No role logs in on TO before every step above has run, the source's root steps included.
  needs_first "roles on $TO get LOGIN once the steps above have run"
  query_failures
  ((blocks == 0)) || finish
  # The target keeps the data until the source has proved equal: a refusal here leaves no role with
  # LOGIN on the source, so thaw --reverse reopens the target in the same rollback.
  if ((REVERSE && VERIFY && DRY == 0)) && target_primary; then
    PROBE=1
    verify_checks
    query_failures
    PROBE=0
    ((diffs == 0)) || { echo "$diffs DIFF"; die "switch --reverse: the source differs from the target in the $diffs DIFF line(s) above; no source role got LOGIN, so the target stays the primary; thaw $CLUSTER --reverse gives it back its LOGIN (cutover.sh rollback runs it)"; }
    echo "switch: verify --reverse 0 DIFF; the source gets LOGIN"
  fi
  # From the first grant on, TO may take writes: recovery treats it as primary, FROM's fence is over.
  # A rerun keeps the stamp of the switch it finishes: cutover.sh binds its later stamps to that value.
  if ((DRY == 0)); then
    if ((REVERSE)); then unfence fwd; target_primary && switch_stamp reversed switched; else unfence rev; target_primary || switch_stamp switched reversed; fi
  fi
  # A superuser on either side keeps the LOGIN it has there: the switch grants nothing a superuser holds.
  for role in $roles; do
    if is_super src "$role" || is_super dst "$role"; then echo "$CMD: $role is a superuser; its LOGIN on $TO stays as it is"; continue; fi
    can=$(q "$TO" postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")")
    # A role made on FROM since the last switch has nothing on TO to give LOGIN back to.
    [[ -n $can ]] || { echo "$CMD: $role is not on $TO; it was made on $FROM, so it gets no LOGIN here"; continue; }
    [[ $can == t ]] && continue
    if ((REVERSE)) && ! src_by_admin "$TO" && ! { [[ -e $WORK/fenced ]] && grep -qxF -- "$role" "$WORK/fenced"; }; then
      echo "$CMD: $role is NOLOGIN on $TO but fence did not make it so; left as it is"
      continue
    fi
    if src_by_admin "$TO"; then need login "$role"; else apply "$TO" postgres "ALTER ROLE $(ident "$role") LOGIN;" "$TO: $role LOGIN"; fi
  done
  needs_first
  query_failures
  ((blocks == 0)) || finish
  finish
}

ensure_dst_hba() {
  local hba=/etc/postgresql/$PG_MAJOR/$DST_CLUSTER/pg_hba.conf line
  line="local all $REPL_ROLE scram-sha-256"
  [[ $(on "$DST_SSH" "head -1 $hba" 2>/dev/null || true) == "$line" ]] && return 0
  run "$DST_SSH" "sed -i '1i $line' $hba && $(psql_cmd dst rw postgres "-c 'select pg_reload_conf()'") >/dev/null" "$hba: '$line' first, reloaded"
}

arm_reverse() {
  local spec db shape sub conn
  upstream_blockers "reverse replication"
  [[ -n $REV_PORT && -n $FWD_PORT ]] || blocked "$CLUSTER.conf sets no REV_PORT or FWD_PORT; pg-forward@$CLUSTER binds REV_PORT on the source box back to the target"
  ((blocks == 0 || DRY)) || finish
  ensure_repl_password
  ensure_repl_role dst
  ensure_dst_hba
  via_forward || ensure_pgpass src "$REV_PORT"
  ensure_forward rev
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec")
    identity_blockers dst "$db" "$shape"
    ensure_publication dst "$db" "$shape" "$REV_PUB"
    # CREATE SUBSCRIPTION needs every published table on the subscriber, copy_data=false or not.
    sync_partitions dst src "$db" "$(sub_name rev "$db")" "$REV_PUB"
  done
  needs_first "the reverse subscriptions are made once the source has the target's partitions"
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") sub=$(sub_name rev "$db")
    if [[ -n $(q src "$db" "select 1 from pg_subscription where subname = '$sub'") ]]; then
      slot_ok dst "$db" "$sub" "the reverse subscription" || continue
      sub_resume src "$db" "$sub"
      continue
    fi
    if via_forward; then
      # srcexec writes the passfile, connects through REV_HOST:REV_PORT as SRC_OSUSER, then subscribes.
      need revsub "$db"
      continue
    fi
    if ! can_connect src "$REV_PORT" "$db"; then
      blocked "the source box does not reach the target as $REPL_ROLE on 127.0.0.1:$REV_PORT through pg-forward@$CLUSTER"
      continue
    fi
    conn="host=127.0.0.1 port=$REV_PORT dbname=$db user=$REPL_ROLE passfile=$(src_passfile) application_name=$sub sslmode=disable"
    apply src "$db" "CREATE SUBSCRIPTION $sub CONNECTION $(lit "$conn") PUBLICATION $REV_PUB WITH (copy_data = false, binary = $BINARY, streaming = on, origin = none, disable_on_error = false);" \
      "src $db: reverse subscription $sub (slot $sub on the target), copy_data=false"
  done
  for spec in "${DATABASES[@]}"; do sync_partitions dst src "$(db_of "$spec")" "$(sub_name rev "$(db_of "$spec")")" "$REV_PUB"; done
}

cmd_reverse() {
  arm_reverse
  finish
}

# Logical replication carries no DDL. The app makes trades_YYYYMMDD days ahead on whichever side
# takes writes, and the publisher's FOR ALL TABLES publication lists it at once. The subscriber
# stops on a row for a table it lacks, and skips the rows of a table it has but does not carry.
# leaf|parent|relkind|owner|bound|partition key, parents before their partitions.
LEAVES_SQL="select format('%I.%I', n.nspname, c.relname) || '|' || format('%I.%I', pn.nspname, p.relname) || '|' || c.relkind::text || '|' || pg_get_userbyid(c.relowner) || '|' || pg_get_expr(c.relpartbound, c.oid) || '|' || case when c.relkind = 'p' then pg_get_partkeydef(c.oid) else '-' end from pg_class c join pg_namespace n on n.oid = c.relnamespace join pg_inherits i on i.inhrelid = c.oid join pg_class p on p.oid = i.inhparent join pg_namespace pn on pn.oid = p.relnamespace where c.relispartition and p.relkind = 'p' and $NS order by (select count(*) from pg_partition_ancestors(c.oid)), 1"
# leaf_grants <side> <db> <leaf>: role:PRIVILEGE[*] for each grant on the leaf beyond its owner's, * for WITH GRANT OPTION.
leaf_grants() { q "$1" "$2" "select case when a.grantee = 0 then 'PUBLIC' else pg_get_userbyid(a.grantee) end || ':' || a.privilege_type || case when a.is_grantable then '*' else '' end from pg_class c, aclexplode(c.relacl) a where c.oid = to_regclass($(lit "$3")) and a.grantee <> c.relowner order by 1"; }
# grant_sql <leaf> <role:PRIV[*],...>: the GRANT statements; srcexec checked each word first.
grant_sql() {
  local g r p o=''
  for g in ${2//,/ }; do
    [[ $g != - ]] || continue
    r=${g%%:*} p=${g#*:} o=''
    [[ $p != *\* ]] || { p=${p%\*}; o=' WITH GRANT OPTION'; }
    [[ $r == PUBLIC ]] || r="\"$r\""
    printf 'GRANT %s ON %s TO %s%s; ' "$p" "$1" "$r" "$o"
  done
}
pub_tables_sql() { echo "select format('%I.%I', schemaname, tablename) from pg_publication_tables where pubname = $(lit "$1") order by 1"; }
sub_tables_sql() { echo "select format('%I.%I', n.nspname, c.relname) from pg_subscription_rel r join pg_subscription s on s.oid = r.srsubid join pg_class c on c.oid = r.srrelid join pg_namespace n on n.oid = c.relnamespace where s.subname = $(lit "$1") order by 1"; }
only_in() { comm -23 <(grep . <<<"$1" | sort -u) <(grep . <<<"$2" | sort -u); }

# sync_partitions <publisher side> <subscriber side> <db> <subscription> <publication>
sync_partitions() {
  local from=$1 to=$2 db=$3 sub=$4 pub=$5 theirs ours leaf parent kind owner bound key have x p2 k2 b2 pown by g grants made=0 owners='' missing t e copy='' rows=0 empty=0 b0=$blocks
  theirs=$(q "$from" "$db" "$LEAVES_SQL") || die "partitions: $from $db: reading partitions failed"
  ours=$(q "$to" "$db" "$LEAVES_SQL") || die "partitions: $to $db: reading partitions failed"
  while IFS='|' read -r leaf parent kind owner bound key; do
    [[ -n $leaf ]] || continue
    have=$(awk -F'|' -v l="$leaf" '$1 == l' <<<"$ours")
    if [[ -n $have ]]; then
      IFS='|' read -r x p2 k2 x b2 x <<<"$have"
      [[ $p2 == "$parent" && $k2 == "$kind" && $b2 == "$bound" ]] ||
        blocked "$to $db: $leaf is a partition of $p2 $b2 (relkind $k2) here and of $parent $bound (relkind $kind) on $from, so the rows $from sends it would not land; reconcile it by hand"
      continue
    fi
    if [[ $kind != r && $kind != p ]]; then blocked "$to $db: $leaf has relkind $kind; partitions makes tables and partitioned tables only"; continue; fi
    # Parents come first, so a parent made above is in owners before the catalog has it (dry run, root steps).
    pown=$(awk -F'|' -v l="$parent" '$1 == l { print $2 }' <<<"$owners")
    [[ -n $pown ]] || pown=$(q "$to" "$db" "select pg_get_userbyid(relowner) from pg_class where oid = to_regclass($(lit "$parent")) and relkind = 'p'")
    [[ -n $pown ]] || { blocked "$to $db: $leaf belongs under $parent, which is not a partitioned table here"; continue; }
    # A role made on the publisher after the switch has nothing here to own the table.
    if [[ -z $(q "$to" postgres "select 1 from pg_roles where rolname = $(lit "$owner")") ]]; then
      echo "partitions: $to $db: $owner, the owner of $leaf on $from, is not a role here; $leaf is made as $pown, the owner of $parent here"
      owner=$pown
    fi
    owners+="$leaf|$owner"$'\n'
    made=$((made + 1))
    by=''
    [[ $kind == r ]] || by=" PARTITION BY $key"
    # Default ACLs grant what they grant; a grant made on the leaf itself on the publisher is made here too.
    grants=''
    for g in $(leaf_grants "$from" "$db" "$leaf"); do
      if [[ ${g%%:*} == PUBLIC || -n $(q "$to" postgres "select 1 from pg_roles where rolname = $(lit "${g%%:*}")") ]]; then grants+="${grants:+,}$g"
      else echo "partitions: $to $db: ${g%%:*} holds ${g#*:} on $leaf on $from and is not a role here; not granted"; fi
    done
    if src_by_admin "$to"; then need partition "$db" "$leaf" "$parent" "$owner" "${key// /}" "${grants:--}" "$bound"; continue; fi
    # As the app does (wen server/internal/store/trades_hot.go), as the publisher's owner so its default ACLs apply.
    apply "$to" "$db" "BEGIN; SET LOCAL lock_timeout = '10s'; SET LOCAL ROLE $(ident "$owner"); CREATE TABLE $leaf (LIKE $parent INCLUDING ALL)$by; ALTER TABLE $parent ATTACH PARTITION $leaf $bound; $(grant_sql "$leaf" "${grants:--}")COMMIT;" \
      "$to $db: partition $leaf of $parent $bound${by:+,$by}, owner $owner${grants:+, grants $grants}, as on $from"
  done <<<"$theirs"
  t=$(q "$to" "$db" "select count(*) from pg_subscription where subname = $(lit "$sub")") || die "partitions: $to $db: reading $sub failed"
  [[ $t == 1 ]] || return 0
  # The refresh runs once the source's root steps above made the tables, at the rerun.
  ((made && DRY == 0)) && src_by_admin "$to" && return 0
  t=$(q "$from" "$db" "$(pub_tables_sql "$pub")") || die "partitions: $from $db: reading publication $pub failed"
  e=$(q "$to" "$db" "$(sub_tables_sql "$sub")") || die "partitions: $to $db: reading pg_subscription_rel of $sub failed"
  missing=$(only_in "$t" "$e")
  [[ -n $missing ]] || return 0
  e=$(q "$to" "$db" "select subenabled from pg_subscription where subname = $(lit "$sub")") || die "partitions: $to $db: reading $sub failed"
  [[ $e == t ]] || { echo "partitions: $to $db: $sub is disabled; its $(grep -c . <<<"$missing") new table(s) are refreshed when switch or reverse enables it"; return 0; }
  # A table the subscriber holds rows of came the other way (the source's own, before the switch):
  # copying it again would collide. An empty one takes the publisher's rows by a table sync.
  while IFS= read -r t; do
    e=$(q "$to" "$db" "select to_regclass($(lit "$t")) is not null") || die "partitions: $to $db: looking up $t failed"
    if [[ $e != t ]]; then
      if ((DRY)); then empty=$((empty + 1)); continue; fi
      blocked "$to $db: $t is in publication $pub on $from but absent here"
      continue
    fi
    e=$(q "$to" "$db" "select exists (select 1 from only $t)") || die "partitions: $to $db: reading $t failed"
    if [[ $e != t ]]; then empty=$((empty + 1)); continue; fi
    # copy_data=false is right only when the subscriber already holds exactly the publisher's rows.
    rows=$((rows + 1))
    [[ $(q "$from" "$db" "select count(*) || '|' || coalesce($ROW_SUM, 0) from only $t t") == "$(q "$to" "$db" "select count(*) || '|' || coalesce($ROW_SUM, 0) from only $t t")" ]] ||
      blocked "$to $db: $t holds rows that differ from $from's; REFRESH with copy_data=false would leave them behind, so reconcile it by hand under the fence"
  done <<<"$missing"
  ((blocks == b0)) || return 0
  if ((rows && empty)); then blocked "$to $db: $sub lacks $rows table(s) holding rows and $empty empty one(s); one REFRESH takes one copy_data, so this needs a hand decision: $(paste -sd' ' - <<<"$missing")"; return 0; fi
  if ((rows)); then copy=false; else copy=true; fi
  if src_by_admin "$to"; then need refresh "$db" "$copy"; return 0; fi
  apply "$to" "$db" "ALTER SUBSCRIPTION $sub REFRESH PUBLICATION WITH (copy_data = $copy);" "$to $db: $sub refreshed for $(paste -sd' ' - <<<"$missing") (copy_data=$copy)"
}

cmd_partitions() {
  local spec db n
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    if [[ $(shape_of "$spec") == upstream=* ]]; then echo "partitions: $db copies an upstream publisher, which this box does not read"; continue; fi
    sync_partitions src dst "$db" "$(sub_name fwd "$db")" "$PUB"
    n=$(q src "$db" "select count(*) from pg_subscription where subname = $(lit "$(sub_name rev "$db")")") || die "partitions: src $db: reading the reverse subscription failed"
    [[ $n == 0 ]] || sync_partitions dst src "$db" "$(sub_name rev "$db")" "$REV_PUB"
  done
  finish
}

# check_partitions <publisher side> <subscriber side> <db> <subscription> <publication>
check_partitions() {
  local from=$1 to=$2 db=$3 sub=$4 pub=$5 a b t n=0
  a=$(q "$from" "$db" "$LEAVES_SQL" | cut -d'|' -f1)
  b=$(q "$to" "$db" "$LEAVES_SQL" | cut -d'|' -f1)
  while IFS= read -r t; do [[ -n $t ]] && diff_row "$db partition $t: on $from, not on $to; run partitions $CLUSTER" && n=$((n + 1)); done < <(only_in "$a" "$b")
  a=$(q "$from" "$db" "$(pub_tables_sql "$pub")")
  b=$(q "$to" "$db" "$(sub_tables_sql "$sub")")
  if [[ -z $a && $(q "$from" "$db" "select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind = 'r' and $NS") != 0 ]]; then
    diff_row "$db: publication $pub on $from lists no tables, but $from has tables"; n=$((n + 1))
  fi
  while IFS= read -r t; do [[ -n $t ]] && diff_row "$db $t: in publication $pub on $from, not carried by $sub on $to (pg_subscription_rel); run partitions $CLUSTER" && n=$((n + 1)); done < <(only_in "$a" "$b")
  echo "preflight: $db $from to $to: $(awk 'NF { n++ } END { print n + 0 }' <<<"$a") published table(s), $n partition or subscription DIFF"
}

drop_sub() {
  local side=$1 db=$2 sub=$3 slot_side=$4
  if [[ -n $(q "$side" "$db" "select 1 from pg_subscription where subname = '$sub'") ]]; then
    if src_by_admin "$side"; then
      # The slot on the other side stays active until the source drops its subscription; the rerun drops it.
      need dropsub "$db"
      return 0
    fi
    apply "$side" "$db" "ALTER SUBSCRIPTION $sub DISABLE; ALTER SUBSCRIPTION $sub SET (slot_name = NONE); DROP SUBSCRIPTION $sub;" "$side $db: subscription $sub dropped"
  fi
  [[ -n $(q "$slot_side" "$db" "select 1 from pg_replication_slots where slot_name = '$sub'") ]] || return 0
  if src_by_admin "$slot_side"; then need dropslot "$db"; return 0; fi
  apply "$slot_side" "$db" "SELECT pg_drop_replication_slot('$sub');" "$slot_side: slot $sub dropped"
}

drop_pub() {
  [[ -n $(q "$1" "$2" "select 1 from pg_publication where pubname = '$3'") ]] || return 0
  if src_by_admin "$1"; then need droppub "$2"; return 0; fi
  apply "$1" "$2" "DROP PUBLICATION $3;" "$1 $2: publication $3 dropped"
}

cmd_retire() {
  local stamp=$WORK/switched age spec db
  [[ -s $WORK/reversed ]] && (($(cat "$WORK/reversed") > $(cat "$WORK/switched" 2>/dev/null || echo 0))) && stamp=$WORK/reversed
  if [[ -s $stamp ]]; then
    age=$(($(date +%s) - $(cat "$stamp")))
    ((age >= REVERSE_HOURS * 3600 || FORCE)) || die "retire: $((age / 3600)) h since $(basename "$stamp"), the window is $REVERSE_HOURS h (--force overrides)"
  else
    ((FORCE)) || die "retire: no switch recorded in $WORK (--force retires anyway)"
  fi
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    if [[ $(shape_of "$spec") == upstream=* ]]; then
      echo "retire: $db stays subscribed to its upstream publisher"
      continue
    fi
    drop_sub src "$db" "$(sub_name rev "$db")" dst
    # The reverse publication goes once the source's subscription to it is gone.
    [[ -n $(q src "$db" "select 1 from pg_subscription where subname = '$(sub_name rev "$db")'") ]] || drop_pub dst "$db" "$REV_PUB"
    drop_sub dst "$db" "$(sub_name fwd "$db")" src
    drop_pub src "$db" "$PUB"
  done
  # SRC_VIA=forward: remote.sh removes the source's marker through PGMOVE_SRC_ADMIN once retire exits 0.
  if [[ -n $FREEZE_MARKER ]] && ! via_forward && remote_has "$SRC_SSH" "$FREEZE_MARKER"; then
    run "$SRC_SSH" "rm -f $FREEZE_MARKER" "$SRC_SSH:$FREEZE_MARKER removed; migrations run again"
  fi
  if [[ -n $FREEZE_MARKER ]] && ((needs == 0)) && remote_has "$DST_SSH" "$FREEZE_MARKER"; then
    run "$DST_SSH" "rm -f $FREEZE_MARKER" "$DST_SSH:$FREEZE_MARKER removed; migrations run again"
  fi
  if ((needs == 0)) && [[ -n $FWD_PORT ]] && on "$DST_SSH" "systemctl is-enabled --quiet pg-forward@$CLUSTER" 2>/dev/null; then
    run "$DST_SSH" "systemctl disable --now pg-forward@$CLUSTER" "pg-forward@$CLUSTER stopped and disabled"
  fi
  finish
}

# The source's root steps for SRC_VIA=forward, built on the operator machine from the "need:" lines
# a box step printed. Each word is checked against this checkout's conf before it goes into SQL.
cmd_srcexec() {
  local verb a b c line db fenced=$SECRETS/fenced role
  via_forward || die "srcexec: only for SRC_VIA=forward"
  ok_db() { our_db "$1" || die "srcexec: $1 is not a database of $CLUSTER.conf"; }
  ok_role() {
    [[ $1 =~ ^[A-Za-z0-9_.-]+$ && $1 != postgres && $1 != "$REPL_ROLE" && $1 != pg_* ]] || die "srcexec: role '$1' is not one pgmove fences"
    local k; for k in ${FENCE_KEEP[@]+"${FENCE_KEEP[@]}"}; do [[ $k != "$1" ]] || die "srcexec: $1 is in FENCE_KEEP"; done
  }
  src_preamble
  printf 'fenced=%q secrets=%q\n' "$fenced" "$SECRETS"
  cat <<'EOS'
role_is() { pq postgres -c "select $2 from pg_roles where rolname = '$1'"; }
EOS
  if [[ ${ARGS[0]:-} == verifiers ]]; then
    # Rows for passwords on the box: every role that is not a superuser and has a verifier.
    printf 'pq postgres -c %q\n' "select rolname || '|' || rolpassword from pg_authid where rolpassword is not null and not rolsuper and rolname !~ '^pg_' and rolname not in ('postgres', '$REPL_ROLE') order by 1"
    return 0
  fi
  if [[ ${ARGS[0]:-} == marker ]]; then
    printf 'm=%q; if [[ -z $m ]]; then echo "marker: none set"; elif [[ -e $m ]]; then echo "marker: $m present"; else echo "marker: $m absent"; exit 1; fi\n' "$FREEZE_MARKER"
    return 0
  fi
  while IFS=' ' read -r line verb a b c; do
    [[ $line == need: ]] || continue
    case $verb in
      fence)
        ok_role "$a"
        printf 'r=%q\n' "$a"
        cat <<'EOS'
[[ $(role_is "$r" rolsuper) == f ]] || { echo "srcexec: $r is a superuser or absent on the source; not fenced" >&2; exit 1; }
[[ -d $secrets ]] || install -d -m 0755 "$secrets"; touch "$fenced"; chmod 0600 "$fenced"
grep -qxF -- "$r" "$fenced" || echo "$r" >>"$fenced"
if [[ $(role_is "$r" rolcanlogin) == t ]]; then pq postgres -c "SET lock_timeout = '2s'; ALTER ROLE \"$r\" NOLOGIN;" >/dev/null; changed "src: $r NOLOGIN (listed in $fenced)"; fi
for i in $(seq 40); do
  k=$(pq postgres -c "select count(pg_terminate_backend(pid)) from pg_stat_activity where usename = '$r'")
  [[ $k == 0 ]] && break
  changed "src: $k session(s) of $r ended"
  sleep 0.5
done
[[ $k == 0 ]] || { echo "srcexec: $r still holds $k session(s) after NOLOGIN" >&2; exit 1; }
EOS
        ;;
      login)
        ok_role "$a"
        printf 'r=%q\n' "$a"
        cat <<'EOS'
grep -qxF -- "$r" "$fenced" 2>/dev/null || { echo "srcexec: $r is not in $fenced, so fence did not take its LOGIN; not granted" >&2; exit 1; }
[[ $(role_is "$r" rolsuper) == f ]] || { echo "srcexec: $r is a superuser; not granted" >&2; exit 1; }
if [[ $(role_is "$r" rolcanlogin) != t ]]; then pq postgres -c "ALTER ROLE \"$r\" LOGIN;" >/dev/null; changed "src: $r LOGIN"; fi
grep -vxF -- "$r" "$fenced" >"$fenced.new" || true; mv -f "$fenced.new" "$fenced"
EOS
        ;;
      setval)
        ok_db "$a"
        local v called
        read -r v called <<<"$c"
        # is_called as sync_sequences reads it: SEQ_ROWS concatenates the boolean, which spells it true or false.
        [[ $b =~ ^[A-Za-z0-9_.\"]+$ && $v =~ ^-?[0-9]+$ && ($called == true || $called == false) ]] || die "srcexec: setval $b $c has an unexpected shape"
        printf 'pq %q -c %q >/dev/null; changed %q\n' "$a" "SELECT setval($(lit "$b")::regclass, $v, $called)" "src $a: $b at $v (is_called $called) as on the target"
        ;;
      revsub)
        ok_db "$a"
        [[ $REV_HOST =~ ^[0-9.]+$ && $REV_PORT =~ ^[0-9]+$ ]] || die "srcexec: REV_HOST or REV_PORT has an unexpected shape"
        [[ -n ${PGMOVE_REV_PW:-} ]] || die "srcexec: revsub needs PGMOVE_REV_PW (remote.sh reads it from the box)"
        printf 'db=%q sub=%q pf=%q line=%q conn=%q create=%q\n' "$a" "$(sub_name rev "$a")" "$SECRETS/pgpass" "$REV_HOST:$REV_PORT:*:$REPL_ROLE:$PGMOVE_REV_PW" \
          "host=$REV_HOST port=$REV_PORT dbname=$a user=$REPL_ROLE passfile=$SECRETS/pgpass application_name=$(sub_name rev "$a") sslmode=disable" \
          "PUBLICATION $REV_PUB WITH (copy_data = false, binary = $BINARY, streaming = on, origin = none, disable_on_error = false)"
        cat <<'EOS'
[[ -d $secrets ]] || install -d -m 0755 "$secrets"
if ! grep -qxF -- "$line" "$pf" 2>/dev/null; then
  { grep -vF -- "${line%:*}:" "$pf" 2>/dev/null || true; printf '%s\n' "$line"; } >"$pf.new"; chown "$osuser": "$pf.new"; chmod 0600 "$pf.new"; mv -f "$pf.new" "$pf"
  changed "src: $pf holds the reverse line"
fi
sudo -n -u "$osuser" psql "$conn connect_timeout=10" -XAtqc 'select 1' >/dev/null || { echo "srcexec: the source does not reach the target through ${conn%% dbname*}; nothing subscribed" >&2; exit 1; }
if [[ $(pq "$db" -c "select count(*) from pg_subscription where subname = '$sub'") == 0 ]]; then
  pq "$db" -c "CREATE SUBSCRIPTION $sub CONNECTION '$conn' $create;" >/dev/null
  changed "src $db: reverse subscription $sub, copy_data=false"
fi
EOS
        ;;
      enable | disable | dropsub)
        ok_db "$a"
        printf 'db=%q sub=%q verb=%q\n' "$a" "$(sub_name rev "$a")" "$verb"
        cat <<'EOS'
have=$(pq "$db" -c "select subenabled from pg_subscription where subname = '$sub'")
case $verb in
  enable) [[ -n $have ]] || { echo "srcexec: no $sub in $db" >&2; exit 1; }
    pq "$db" -c "ALTER SUBSCRIPTION $sub SET (origin = none); ALTER SUBSCRIPTION $sub ENABLE;" >/dev/null; changed "src $db: $sub origin=none, enabled" ;;
  disable) [[ $have != t ]] || { pq "$db" -c "ALTER SUBSCRIPTION $sub DISABLE;" >/dev/null; changed "src $db: $sub disabled"; } ;;
  dropsub) [[ -z $have ]] || { pq "$db" -c "ALTER SUBSCRIPTION $sub DISABLE;" -c "ALTER SUBSCRIPTION $sub SET (slot_name = NONE);" -c "DROP SUBSCRIPTION $sub;" >/dev/null; changed "src $db: $sub dropped"; } ;;
esac
EOS
        ;;
      dropslot | droppub)
        ok_db "$a"
        printf 'db=%q slot=%q pub=%q verb=%q\n' "$a" "$(sub_name fwd "$a")" "$PUB" "$verb"
        cat <<'EOS'
if [[ $verb == dropslot ]]; then
  act=$(pq "$db" -c "select active from pg_replication_slots where slot_name = '$slot'")
  [[ $act != t ]] || { echo "srcexec: slot $slot is active; drop the target's subscription first" >&2; exit 1; }
  [[ -z $act ]] || { pq "$db" -c "select pg_drop_replication_slot('$slot')" >/dev/null; changed "src: slot $slot dropped"; }
else
  [[ $(pq "$db" -c "select count(*) from pg_publication where pubname = '$pub'") == 0 ]] || { pq "$db" -c "DROP PUBLICATION $pub;" >/dev/null; changed "src $db: publication $pub dropped"; }
fi
EOS
        ;;
      partition)
        ok_db "$a"
        local p o key g gs by='' bound id_re='^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$' bound_re="^FOR VALUES FROM \('[-0-9: +.]+'\) TO \('[-0-9: +.]+'\)$"
        # key: the partition key of a partitioned leaf with its spaces taken out, or - for a plain one.
        read -r p o key g bound <<<"$c"
        gs='(PUBLIC|[a-z_][a-z0-9_]*):(SELECT|INSERT|UPDATE|DELETE|TRUNCATE|REFERENCES|TRIGGER|MAINTAIN)\*?'
        [[ $b =~ $id_re && $p =~ $id_re && $o =~ ^[A-Za-z0-9_]+$ && $bound =~ $bound_re && $key =~ ^(-|(RANGE|LIST)\([a-z_][a-z0-9_]*(,[a-z_][a-z0-9_]*)*\))$ && $g =~ ^(-|$gs(,$gs)*)$ ]] || die "srcexec: partition $b of $p ($o, $key, $g, $bound) has an unexpected shape"
        [[ $key == - ]] || by=" PARTITION BY $key"
        printf 'db=%q leaf=%q parent=%q owner=%q bound=%q by=%q grants=%q\n' "$a" "$b" "$p" "$o" "$bound" "$by" "$(grant_sql "$b" "$g")"
        cat <<'EOS'
[[ $(pq "$db" -c "select relkind from pg_class where oid = to_regclass('$parent')") == p ]] || { echo "srcexec: $parent is not a partitioned table in $db" >&2; exit 1; }
if [[ $(pq "$db" -c "select to_regclass('$leaf') is null") == t ]]; then
  pq "$db" -c "BEGIN" -c "SET LOCAL lock_timeout = '10s'" -c "SET LOCAL ROLE \"$owner\"" -c "CREATE TABLE $leaf (LIKE $parent INCLUDING ALL)$by" -c "ALTER TABLE $parent ATTACH PARTITION $leaf $bound" -c "${grants:-select 1}" -c "COMMIT" >/dev/null
  changed "src $db: partition $leaf of $parent $bound$by, owner $owner${grants:+ with its grants}, as on the target"
fi
EOS
        ;;
      refresh)
        ok_db "$a"
        [[ $b == true || $b == false ]] || die "srcexec: refresh copy_data '$b' is not true or false"
        printf 'db=%q sub=%q copy=%q\n' "$a" "$(sub_name rev "$a")" "$b"
        cat <<'EOS'
[[ $(pq "$db" -c "select subenabled from pg_subscription where subname = '$sub'") == t ]] || { echo "srcexec: $sub in $db is absent or disabled; REFRESH needs it enabled" >&2; exit 1; }
pq "$db" -c "ALTER SUBSCRIPTION $sub REFRESH PUBLICATION WITH (copy_data = $copy)" >/dev/null
changed "src $db: $sub refreshed (copy_data=$copy)"
EOS
        ;;
      rmmarker)
        printf 'm=%q\n' "$FREEZE_MARKER"
        echo '[[ -z $m || ! -e $m ]] || { rm -f "$m"; changed "src: $m removed; migrations run again"; }'
        ;;
      catchup) ;; # remote.sh pipes catchout from the box into catchin
      catchin)
        is_hypertable "$a" && [[ $b =~ ^[0-9]+$ ]] || die "srcexec: catchin $a $b is not a hypertable of $CLUSTER.conf and a floor"
        printf 'pq %q --single-transaction -c %q -c %q -c %q -c %q | tail -1\n' "$HT_DB" "SET session_replication_role = replica" "CREATE TEMP TABLE catchup_rows (LIKE $a) ON COMMIT DROP" '\copy catchup_rows from stdin with (format binary)' \
          "WITH moved AS (INSERT INTO $a SELECT c.* FROM catchup_rows c WHERE NOT EXISTS (SELECT 1 FROM $a t WHERE t.$HT_KEY = c.$HT_KEY AND t.$HT_TIME = c.$HT_TIME) RETURNING 1) SELECT count(*) FROM moved"
        return 0
        ;;
      *) die "srcexec: unknown need '$verb'" ;;
    esac
  done
  echo 'echo "source: $n change(s)"'
}

# Rows of a target hypertable above a floor, for a reverse catchup into a SRC_VIA=forward source.
cmd_catchout() {
  local table=${ARGS[0]:-} floor=${ARGS[1]:-}
  is_hypertable "$table" && [[ $floor =~ ^[0-9]+$ ]] || die "catchout: $table $floor is not a hypertable of $CLUSTER.conf and a floor"
  printf '\\copy (select * from %s where %s > %s and %s >= %s) to stdout with (format binary)\n' "$table" "$HT_KEY" "$floor" "$HT_TIME" "$(lit "$(ts_cutoff)")" | sql dst ro "$HT_DB"
}

# Target roles get the source's SCRAM verifiers, so the switch grants LOGIN with no password reset.
# Superusers are never touched: a target superuser keeps what it has, a source superuser is not read.
cmd_passwords() {
  local rows role ver super have same=0 total=0
  if via_forward; then
    rows=$(cat)
  else
    rows=$(q src postgres "select rolname || '|' || rolpassword from pg_authid where rolpassword is not null and not rolsuper and rolname !~ '^pg_' and rolname not in ('postgres', '$REPL_ROLE') order by 1")
  fi
  [[ -n $rows ]] || die "passwords: read no role|verifier rows from the source; an empty answer is not 'no passwords'"
  while IFS='|' read -r role ver; do
    [[ -n $role ]] || continue
    total=$((total + 1))
    [[ $ver =~ $SCRAM_RE ]] || { blocked "$role: the source's verifier is not SCRAM-SHA-256; not copied"; continue; }
    super=$(q dst postgres "select rolsuper from pg_roles where rolname = $(lit "$role")")
    [[ -n $super ]] || { blocked "$role is not on the target; run init"; continue; }
    [[ $super == f ]] || { echo "passwords: $role is a superuser on the target; left as it is"; continue; }
    have=$(q dst postgres "select coalesce(rolpassword, '') from pg_authid where rolname = $(lit "$role")")
    if [[ $have == "$ver" ]]; then same=$((same + 1)); continue; fi
    apply dst postgres "ALTER ROLE $(ident "$role") PASSWORD $(lit "$ver");" "dst: $role password verifier as on the source"
  done <<<"$rows"
  ((DRY || blocks)) || state "$WORK/passwords-ok" "$(date +%s)"
  finish "$same of $total verifier(s) already equal"
}

# TimescaleDB policies are rows in its catalog, which neither pg_dump --schema-only nor a
# subscription carries. init copies compression settings and the policy jobs, unscheduled; the
# switch schedules the target's where the source's run.
TS_JOBS="select j.job_id || '|' || j.proc_name || '|' || format('%I.%I', j.hypertable_schema, j.hypertable_name) || '|' || j.schedule_interval || '|' || case when j.scheduled then 't' else 'f' end || '|' || coalesce((j.config - 'hypertable_id' - 'mat_hypertable_id')::text, '{}') from timescaledb_information.jobs j where j.job_id >= 1000 order by 1"

ensure_timescale() {
  local db=$1 have line id proc table every sched config add seg ord t c
  has_ext src "$db" || return 0
  while IFS='|' read -r t seg ord; do
    [[ -n $t ]] || continue
    [[ $(q dst "$db" "select compression_enabled from timescaledb_information.hypertables where format('%I.%I', hypertable_schema, hypertable_name) = $(lit "$t")") == t ]] && continue
    apply dst "$db" "ALTER TABLE $t SET (timescaledb.compress$([[ $seg == - ]] || echo ", timescaledb.compress_segmentby = $(lit "$seg")")$([[ $ord == - ]] || echo ", timescaledb.compress_orderby = $(lit "$ord")"));" "$db: $t compression as on the source"
  done < <(q src "$db" "select format('%I.%I', h.hypertable_schema, h.hypertable_name) || '|' || coalesce(s.segmentby, '-') || '|' || coalesce(s.orderby, '-') from timescaledb_information.hypertables h join timescaledb_information.hypertable_compression_settings s on s.hypertable = format('%I.%I', h.hypertable_schema, h.hypertable_name)::regclass where h.compression_enabled")
  while IFS= read -r c; do [[ -z $c ]] || blocked "$db: continuous aggregate $c; pgmove does not build those, make it by hand"; done < <(q src "$db" "select format('%I.%I', view_schema, view_name) from timescaledb_information.continuous_aggregates")
  have=$(q dst "$db" "$TS_JOBS" | cut -d'|' -f2,3)
  while IFS='|' read -r id proc table every sched config; do
    [[ -n $id ]] || continue
    grep -qxF -- "$proc|$table" <<<"$have" && continue
    case $proc in
      policy_compression) add="add_compression_policy($(lit "$table"), compress_after => ($(lit "$config")::jsonb ->> 'compress_after')::interval" ;;
      policy_retention) add="add_retention_policy($(lit "$table"), drop_after => ($(lit "$config")::jsonb ->> 'drop_after')::interval" ;;
      *) blocked "$db: job $id $proc on $table; pgmove copies compression and retention policies only"; continue ;;
    esac
    apply dst "$db" "SELECT alter_job(${add}, schedule_interval => $(lit "$every")::interval), scheduled => false);" "$db: $proc on $table every $every, unscheduled until the switch"
  done < <(q src "$db" "$TS_JOBS")
}

# The switch schedules a target job where the source's same job is scheduled.
# thaw: no target job runs while the source is primary again; a switch that stopped part way may have scheduled them.
unschedule_timescale() {
  local db=$1 id proc table every sched config
  has_ext dst "$db" || return 0
  while IFS='|' read -r id proc table every sched config; do
    [[ -n $id && $sched == t ]] || continue
    apply dst "$db" "SELECT alter_job($id, scheduled => false);" "$db: job $id $proc on $table unscheduled again"
  done < <(q dst "$db" "$TS_JOBS")
}

schedule_timescale() {
  local db=$1 id proc table every sched config want
  has_ext src "$db" || return 0
  want=$(q src "$db" "$TS_JOBS" | awk -F'|' '$5 == "t" { print $2 "|" $3 }')
  while IFS='|' read -r id proc table every sched config; do
    [[ -n $id && $sched == f ]] && grep -qxF -- "$proc|$table" <<<"$want" || continue
    apply dst "$db" "SELECT alter_job($id, scheduled => true);" "$db: job $id $proc on $table scheduled"
  done < <(q dst "$db" "$TS_JOBS")
}

volume_dropin() {
  printf '[Unit]\nRequiresMountsFor=%s\n[Service]\nExecStartPre=/usr/bin/mountpoint -q %s\n' "$DST_VOLUME" "$DST_VOLUME"
}

# Postgres must never start on an empty directory where the volume should be mounted: initdb'd or
# not, it would take writes the volume never sees.
ensure_volume_guard() {
  local f=/etc/systemd/system/postgresql@$PG_MAJOR-$DST_CLUSTER.service.d/pgmove-volume.conf
  [[ -n $DST_VOLUME ]] || { blocked "$CLUSTER.conf sets no DST_VOLUME; the target's volume guard has no mount to require"; return 0; }
  if [[ $(on "$DST_SSH" "cat $f" 2>/dev/null || true) != "$(volume_dropin)" ]]; then
    put "$DST_SSH" "$f" 0644 root "$(volume_dropin)" "$f: postgresql@$PG_MAJOR-$DST_CLUSTER requires $DST_VOLUME mounted"
  else
    # The file can be in place with systemd never reloaded, as when an earlier init stopped here.
    local req
    req=$(on "$DST_SSH" "systemctl show -p RequiresMountsFor --value postgresql@$PG_MAJOR-$DST_CLUSTER") || die "volume: systemd on $DST_SSH did not answer"
    [[ " $req " != *" $DST_VOLUME "* ]] || return 0
  fi
  run "$DST_SSH" "systemctl daemon-reload" "systemd reloaded (no restart: the guard applies at the next start)"
}

check_volume_guard() {
  local f=/etc/systemd/system/postgresql@$PG_MAJOR-$DST_CLUSTER.service.d/pgmove-volume.conf data mnt req
  [[ -n $DST_VOLUME ]] || { diff_row "volume: $CLUSTER.conf sets no DST_VOLUME"; return 0; }
  data=$(q dst postgres "show data_directory")
  [[ -n $data ]] || { diff_row "volume: the target answered no data_directory"; return 0; }
  mnt=$(on "$DST_SSH" "findmnt -rno TARGET -T $(printf %q "$data")" 2>/dev/null || true)
  [[ $mnt == "$DST_VOLUME" ]] || diff_row "volume: $data is on '${mnt:-?}', not $DST_VOLUME"
  [[ $(on "$DST_SSH" "cat $f" 2>/dev/null || true) == "$(volume_dropin)" ]] || { diff_row "volume: no $f; run init $CLUSTER (writes it and reloads systemd, no restart)"; return 0; }
  req=$(on "$DST_SSH" "systemctl show -p RequiresMountsFor --value postgresql@$PG_MAJOR-$DST_CLUSTER" 2>/dev/null || true)
  [[ " $req " == *" $DST_VOLUME "* ]] || { diff_row "volume: systemd has not loaded $f (RequiresMountsFor='$req')"; return 0; }
  echo "volume: $data on $DST_VOLUME; postgresql@$PG_MAJOR-$DST_CLUSTER will not start without it"
}

# Read-only. Exits 0 only when the switch could start now: every check prints DIFF or passes.
cmd_preflight() {
  local spec db shape sub counts total lag age s out rc rev upstream=0
  echo "preflight: $CLUSTER versions: source $(q src postgres 'show server_version'), target $(q dst postgres 'show server_version')"
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec") sub=$(sub_name fwd "$db")
    IFS='|' read -r counts total <<<"$(ready_count dst "$db" "$sub")"
    if [[ -z $total || $total == 0 ]] && [[ $(q src "$db" "select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind = 'r' and $NS") == 0 ]]; then
      echo "preflight: $db: the source has no tables, so $sub has none to copy"
    elif [[ -z $total || $total == 0 ]]; then diff_row "$db: the target has no subscription $sub with tables"
    elif [[ $counts != 0 ]]; then diff_row "$db: $counts of $total tables not in state r (the copy is still running)"
    else echo "preflight: $db: all $total tables in state r"; fi
    if [[ $shape == upstream=* ]]; then
      upstream=1
      # The publisher is upstream of the source, so its LSN is not readable here: the lag is the
      # age of the last end position the target confirmed.
      age=$(q dst "$db" "select coalesce(extract(epoch from now() - max(latest_end_time))::bigint, -1) from pg_stat_subscription where subname = '$sub'")
      if [[ ! $age =~ ^[0-9]+$ ]] || ((age > MAX_UPSTREAM_LAG_SECS)); then diff_row "$db: $sub last confirmed ${age:-?} s ago (limit $MAX_UPSTREAM_LAG_SECS s)"; else echo "preflight: $db: $sub confirmed $age s ago"; fi
      continue
    fi
    lag=$(q src "$db" "select case when wal_status = 'lost' then 'lost' else pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)::bigint::text end from pg_replication_slots where slot_name = '$sub'")
    if [[ ! $lag =~ ^-?[0-9]+$ ]]; then diff_row "$db: no slot $sub on the source"
    elif ((lag >= MAX_LAG_BYTES)); then diff_row "$db: slot $sub is $lag bytes behind (limit $MAX_LAG_BYTES)"
    else echo "preflight: $db: slot $sub $lag bytes behind"; fi
    # A reverse slot left inactive keeps every byte of the target's WAL since it stopped, and the
    # source's first reverse row waits for all of it to be decoded (about 2 min for 4 GB on test).
    s=$(sub_name rev "$db")
    lag=$(q dst "$db" "select case when wal_status = 'lost' then 'lost' else pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)::bigint::text end from pg_replication_slots where slot_name = '$s'")
    rev=$(q src "$db" "select subenabled from pg_subscription where subname = $(lit "$s")")
    if [[ -z $lag && -z $rev ]]; then echo "preflight: $db: no reverse subscription or slot $s yet; reverse makes them at the current LSN"
    elif [[ -z $rev ]]; then diff_row "$db: reverse slot $s on the target has no subscription on the source and holds the target's WAL; run reverse $CLUSTER"
    elif [[ -z $lag ]]; then diff_row "$db: reverse subscription $s on the source has no slot on the target; drop the subscription, then run reverse $CLUSTER"
    elif [[ $rev != t ]]; then diff_row "$db: reverse subscription $s on the source is disabled, so its slot holds the target's WAL; run reverse $CLUSTER"
    elif [[ ! $lag =~ ^-?[0-9]+$ ]] || ((lag >= MAX_LAG_BYTES)); then diff_row "$db: reverse slot $s on the target is ${lag:-?} bytes behind (limit $MAX_LAG_BYTES); run reverse $CLUSTER to arm it, then wait"
    else echo "preflight: $db: reverse slot $s $lag bytes behind"; fi
  done
  check_tables
  check_schema
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    [[ $(shape_of "$spec") == upstream=* ]] && { echo "preflight: $db: the publisher is upstream of the source; its partitions are not read here"; continue; }
    check_partitions src dst "$db" "$(sub_name fwd "$db")" "$PUB"
    s=$(sub_name rev "$db")
    if [[ $(q src "$db" "select count(*) from pg_subscription where subname = $(lit "$s")") == 0 ]]; then echo "preflight: $db: no reverse subscription $s on the source, so no reverse partitions to compare"
    else check_partitions dst src "$db" "$s" "$REV_PUB"; fi
  done
  check_grants
  hash_age
  if ((upstream)); then
    echo "preflight: $CLUSTER is fed by its upstream publisher; fence, switch and reverse do not apply"
  else
    for s in reverse fence switch; do
      rc=0
      out=$("$0" "$s" "$CLUSTER" --dry-run 2>&1) || rc=$?
      sed "s/^/  $s: /" <<<"$out"
      if ((rc)); then diff_row "$s --dry-run exits $rc"; else echo "preflight: $s --dry-run exits 0"; fi
    done
  fi
  if ((upstream)); then echo "preflight: marker: no source publication, so no freeze marker"
  elif [[ -z $FREEZE_MARKER ]]; then diff_row "marker: $CLUSTER.conf sets no FREEZE_MARKER"
  elif [[ $MIGRATE_MARKER == none ]]; then echo "preflight: marker: nothing on the source refuses migrations under a marker (MIGRATE_MARKER=none); $FREEZE_MARKER is the record only"
  elif [[ $FREEZE_MARKER != "$MIGRATE_MARKER" ]]; then diff_row "marker: FREEZE_MARKER $FREEZE_MARKER is not $MIGRATE_MARKER, the path the source's deploy refuses migrations under"
  else echo "preflight: marker: $FREEZE_MARKER is the path the source's deploy refuses under"; fi
  if ((upstream)); then :
  elif [[ -n $FREEZE_MARKER ]] && ! via_forward; then
    if on "$SRC_SSH" "test -e $FREEZE_MARKER"; then echo "preflight: marker: $FREEZE_MARKER present on the source"; else diff_row "marker: $FREEZE_MARKER absent on the source"; fi
  elif via_forward; then
    echo "preflight: marker presence and the verifiers are checked by remote.sh through PGMOVE_SRC_ADMIN"
  fi
  check_volume_guard
  finish_check
}

WRITES_SQL="select '@epoch|' || extract(epoch from pg_postmaster_start_time())::bigint || '/' || coalesce(extract(epoch from stats_reset)::bigint, 0) from pg_stat_database where datname = current_database() union all select format('%I.%I', schemaname, relname) || '|' || (n_tup_ins + n_tup_upd + n_tup_del) from pg_stat_user_tables where schemaname !~ '^(_timescaledb|timescaledb_)' or (schemaname = '_timescaledb_internal' and relname ~ '^_hyper_')"

cmd_writes() {
  local action=${ARGS[0]:-} tag=${ARGS[1]:-} side=${ARGS[2]:-} spec db file now total=0 out
  [[ ($action == save || $action == diff) && -n $tag && ($side == src || $side == dst) ]] || usage
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") file=$WORK/writes/$tag-$side-$db
    now=$(q "$side" "$db" "$WRITES_SQL")
    grep -q '^@epoch|' <<<"$now" || die "writes: $side $db returned no counters"
    if [[ $action == save ]]; then
      state "$file" "$now"
      echo "writes: saved $tag $side $db ($(($(grep -c . <<<"$now") - 1)) tables)"
      continue
    fi
    [[ -s $file ]] || die "writes: no snapshot $file"
    out=$(awk -F'|' 'NR == FNR { was[$1] = $2; next }
      $1 == "@epoch" { if ($2 != was[$1]) print "@reset"; next }
      { d = $2 - was[$1] } d != 0 { print $1 "|" d }' "$file" - <<<"$now")
    while IFS='|' read -r table d; do
      [[ -n $table ]] || continue
      if [[ $table == @reset ]]; then echo "DIFF $side $db: counters reset since $tag (restart or stats reset); writes cannot be counted"; continue; fi
      echo "DIFF $side $db $table $d since $tag"
      ((d < 0)) || total=$((total + d))
    done <<<"$out"
  done
  [[ $action == save ]] && return 0
  query_failures
  # A report, not a check: exit 0 says every counter was read; cutover.sh judges the rows.
  echo "writes: $side total $total since $tag"
}

# Only init makes the target cluster, so only init --dry-run reads a silent target as absent.
if ((DRY)) && [[ $CMD == init ]] && ! echo 'select 1' | sql dst ro postgres >/dev/null 2>&1; then
  DST_DOWN=1
  echo "dry run: the target ($DST_SSH, $DST_SOCKET:$DST_PORT) does not answer from $(hostname -s); init would make it, so its state reads as absent"
fi
# remote.sh install holds this lock exclusively while a copy lands, so no run of any copy reads a
# half-written script or a stamp another copy wrote mid-install. ssh masters get no copy of it;
# srcexec runs on the operator machine.
if [[ $CMD != srcexec ]] && command -v flock >/dev/null; then
  mkdir -p "${WORK%/*}" || die "cannot make ${WORK%/*}"
  exec 8>>"${WORK%/*}/.run.lock" || die "cannot open ${WORK%/*}/.run.lock"
  flock -s -w 120 8 || die "remote.sh install has held ${WORK%/*}/.run.lock for 120 s; rerun once it ends"
fi
"cmd_$CMD"
