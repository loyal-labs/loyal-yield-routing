#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SUBCOMMANDS="probe init subscribe backfill catchup sequences grants schema hash verify fence switch reverse retire writes"
PUB=pgmove
REV_PUB=pgmove_rev
REPL_ROLE=pgmove

usage() {
  cat >&2 <<EOF
usage: $0 <subcommand> <cluster> [flags]
clusters: $(for f in "$HERE"/clusters/*.conf; do basename "$f" .conf; done | paste -sd' ' -)
  probe <c>                    what feeds the source (neon-fed, static or local) and what blocks a move
  init <c> [--dry-run]         target cluster, settings, roles (NOLOGIN), databases, extensions, schema, hypertables
  subscribe <c> [--dry-run]    freeze marker, replication role, forward, publication, one subscription per database in order
  backfill <c> [--dry-run]     hypertables: the last TS_RETAIN_DAYS days, one day per binary COPY, resumable
  catchup <c> [--reverse]      hypertable rows above the event_id floor
  sequences <c> [--reverse]    setval on every sequence that differs
  grants <c>                   role, membership, setting, owner and ACL rows on both sides
  schema <c>                   pg_dump --schema-only of both sides, object by object
  hash <c> [table] [--age]     md5 per row, summed per 1M-key bucket; differing buckets hashed a second time
  verify <c> [--reverse]       tables, sequences, hypertables, schema, grants and hash age
  fence <c> [--reverse]        login roles NOLOGIN on the side being left, sessions ended, slots caught up
  switch <c> [--reverse]       catchup, sequences, subscriptions off, the other direction armed or resumed, login roles LOGIN on the side taking over
  reverse <c> [--dry-run]      reverse publication on the target, reverse subscription on the source (copy_data=false)
  retire <c> [--force]         after REVERSE_HOURS: both directions, their slots, the freeze marker and the forward go
  writes <c> save|diff <tag> src|dst   write counters per table
Mutating subcommands print "changed:" lines and end with "<n> change(s)"; --dry-run prints each command instead.
Checking subcommands print DIFF rows and end with "<n> DIFF"; they exit 1 when n > 0.
EOF
  exit 2
}

CMD=${1:-}
CLUSTER=${2:-}
[[ -n $CMD && -n $CLUSTER ]] || usage
case " $SUBCOMMANDS " in *" $CMD "*) ;; *) usage ;; esac
[[ -f $HERE/clusters/$CLUSTER.conf ]] || { echo "pgmove: no cluster $CLUSTER ($HERE/clusters/$CLUSTER.conf)" >&2; usage; }
shift 2
DRY=0 REVERSE=0 FORCE=0 AGE=0
ARGS=()
for a in "$@"; do
  case $a in
    --dry-run) DRY=1 ;;
    --reverse) REVERSE=1 ;;
    --force) FORCE=1 ;;
    --age) AGE=1 ;;
    -*) usage ;;
    *) ARGS+=("$a") ;;
  esac
done

PG_MAJOR=18
SRC_SSH='' SRC_SOCKET='' SRC_PORT='' SRC_OSUSER=''
DST_SSH=local DST_CLUSTER=main DST_DATA='' DST_PORT=5432 DST_SOCKET=/var/run/postgresql DST_OSUSER=postgres
ENCODING=UTF8 LOCALE_PROVIDER='' LOCALE=''
DATABASES=()
HYPERTABLES=() HT_DB='' HT_TIME=observed_at HT_KEY=event_id TS_RETAIN_DAYS=62 CATCHUP_MARGIN=10000
SYNC_WORKERS=2 BINARY=true COPY_SETTINGS=()
FWD_USER=pgmove-fwd FWD_SSH='' FWD_PORT='' REV_PORT='' FREEZE_MARKER='' FENCE_KEEP=() REVERSE_HOURS=72
PROBE_SECS=30 LAG_WAIT_SECS=600 HASH_BUCKET=1000000 HASH_RECHECK_SECS=60 HASH_MAX_AGE_HOURS=24 EXACT_COUNT_BYTES=268435456
# shellcheck source=/dev/null
source "$HERE/clusters/$CLUSTER.conf"
for v in SRC_SSH SRC_SOCKET SRC_PORT SRC_OSUSER LOCALE_PROVIDER LOCALE; do
  [[ -n ${!v} ]] || { echo "pgmove: $CLUSTER.conf sets no $v" >&2; exit 2; }
done
((${#DATABASES[@]})) || { echo "pgmove: $CLUSTER.conf lists no DATABASES" >&2; exit 2; }
[[ -n $FWD_SSH ]] || FWD_SSH=$FWD_USER@${SRC_SSH#*@}

WORK=/var/lib/pgmove/$CLUSTER
SECRETS=/etc/pgmove/$CLUSTER
FROM=src TO=dst
((REVERSE)) && FROM=dst TO=src

changes=0 diffs=0 blocks=0 DST_DOWN=0
# A failed query reads as empty, and an empty answer reads as absent or as equal. q runs in
# subshells, so it records each failure here and finish and finish_check refuse to pass.
QFAIL=$(mktemp)
trap 'rm -f "$QFAIL"' EXIT

changed() { changes=$((changes + 1)); if ((DRY)); then echo "would change: $*"; else echo "changed: $*"; fi; }
diff_row() { diffs=$((diffs + 1)); echo "DIFF $*"; }
blocked() { blocks=$((blocks + 1)); echo "blocked: $*"; }
die() { echo "pgmove: $*" >&2; exit 1; }
mask() { sed -E "s/(password=)[^ ']*/\1***/g; s/(PASSWORD +')[^']*'/\1***'/g"; }
lit() { local s=${1//\'/\'\'}; printf "'%s'" "$s"; }
ident() { local s=${1//\"/\"\"}; printf '"%s"' "$s"; }
db_of() { echo "${1%%:*}"; }
shape_of() { echo "${1#*:}"; }
host_of() { if [[ $1 == src ]]; then echo "$SRC_SSH"; else echo "$DST_SSH"; fi; }
other() { if [[ $1 == src ]]; then echo dst; else echo src; fi; }
state() { if ((DRY)); then return 0; fi; mkdir -p "$(dirname "$1")"; printf '%s\n' "$2" >"$1"; }

# Every remote query rides one master connection per host. A new connection per query met the
# source's sshd MaxStartups early drop on a public box and failed 14 of 159 hash queries.
on() {
  if [[ $1 == local ]]; then bash -c "$2"; return; fi
  install -d -m 0700 /run/pgmove 2>/dev/null || true
  ssh -o BatchMode=yes -o ConnectTimeout=15 -o ControlMaster=auto -o ControlPath=/run/pgmove/ssh-%C -o ControlPersist=120 "$1" "$2"
}

psql_cmd() {
  local side=$1 mode=$2 db=$3 extra=${4:-} user sock port opts="-c statement_timeout=0"
  if [[ $side == src ]]; then user=$SRC_OSUSER sock=$SRC_SOCKET port=$SRC_PORT; else user=$DST_OSUSER sock=$DST_SOCKET port=$DST_PORT; fi
  [[ $mode == ro ]] && opts="$opts -c default_transaction_read_only=on"
  printf 'sudo -n -u %q env PGOPTIONS=%q PGTZ=UTC PGDATESTYLE=ISO psql -h %q -p %q -d %q -XAtq -v ON_ERROR_STOP=1 %s' \
    "$user" "$opts" "$sock" "$port" "$db" "$extra"
}

sql() { on "$(host_of "$1")" "$(psql_cmd "$@")"; }

q() {
  local side=$1 db=$2 text=$3 extra=${4:-}
  [[ $side == dst && $DST_DOWN == 1 ]] && return 0
  printf '%s\n' "$text" | sql "$side" ro "$db" "$extra" && return 0
  local rc=$?
  # A dry run reads databases and objects that do not exist yet; those failures mean absent.
  ((DRY)) || echo "$side $db exit $rc" >>"$QFAIL"
  return $rc
}

apply() {
  local side=$1 db=$2 text=$3
  shift 3
  changed "$*"
  if ((DRY)); then
    printf '  on %s %s: %s\n' "$(host_of "$side")" "$db" "$(printf '%s' "$text" | mask | tr '\n' ' ')"
    return 0
  fi
  printf '%s\n' "$text" | sql "$side" rw "$db" >/dev/null
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
}

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
    sample=$(probe_sample | sql src ro "$db")
    outside=$(grep -c '^a|' <<<"$sample" || true)
    moved=$(awk -F'|' '$1 == "a" { a[$2] = $3 } $1 == "b" && $3 != a[$2] { n++ } END { print n + 0 }' <<<"$sample")
    writers=$(awk -F'|' '$1 == "w" { u[$2]++ } END { for (k in u) printf "%s%s(%d)", s, k, u[k]; s = "," }' <<<"$sample")
    echo "probe: $CLUSTER $db: $outside tables outside a subscription, $moved of them written in ${PROBE_SECS}s; client sessions holding a write transaction: ${writers:-none}"
    if ((moved > 0)) || [[ -n $writers ]]; then
      line=local
    elif [[ $feed == neon && $outside == 0 ]]; then
      line=neon-fed
    elif [[ $feed == upstream && $outside == 0 ]]; then
      line=upstream-fed
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

ensure_pgconf() {
  local dir=/etc/postgresql/$PG_MAJOR/$DST_CLUSTER/conf.d want have restart=0
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
}

ROLE_ROWS="select rolname || '|' || rolsuper || '|' || rolinherit || '|' || rolcreaterole || '|' || rolcreatedb || '|' || rolreplication || '|' || rolbypassrls || '|' || rolconnlimit || '|' || coalesce(nullif(rolvaliduntil, 'infinity')::text, '-') || '|' || coalesce(md5(rolpassword), '-') from pg_authid where rolname !~ '^pg_' and rolname not in ('postgres', '$REPL_ROLE') order by 1"
ROLE_ALTER="select format('ALTER ROLE %I WITH %s %s %s %s %s %s CONNECTION LIMIT %s VALID UNTIL %s PASSWORD %s;', rolname, case when rolsuper then 'SUPERUSER' else 'NOSUPERUSER' end, case when rolinherit then 'INHERIT' else 'NOINHERIT' end, case when rolcreaterole then 'CREATEROLE' else 'NOCREATEROLE' end, case when rolcreatedb then 'CREATEDB' else 'NOCREATEDB' end, case when rolreplication then 'REPLICATION' else 'NOREPLICATION' end, case when rolbypassrls then 'BYPASSRLS' else 'NOBYPASSRLS' end, rolconnlimit, coalesce(quote_literal(rolvaliduntil::text), '''infinity'''), coalesce(quote_literal(rolpassword), 'NULL')) from pg_authid where rolname = :'role'"
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

USER_OBJECTS="select (select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind in ('r','p','v','m','S') and $NS) + (select count(*) from pg_proc p join pg_namespace n on n.oid = p.pronamespace where $USER_NS and not exists (select 1 from pg_depend e where e.classid = 'pg_proc'::regclass and e.objid = p.oid and e.deptype = 'e'))"

src_dump() {
  printf 'sudo -n -u %q pg_dump -h %q -p %q -d %q %s' "$SRC_OSUSER" "$SRC_SOCKET" "$SRC_PORT" "$1" "$2"
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
    printf '  on %s: %s, stdout to %s on this box\n' "$SRC_SSH" "$fetch" "$dump"
    printf '  on %s: %s\n' "$DST_SSH" "$restore"
    return 0
  fi
  mkdir -p "$WORK"
  on "$SRC_SSH" "$fetch" >"$dump"
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
  done < <(q src "$db" "select format('%I.%I', hypertable_schema, hypertable_name) || ' ' || column_name || ' ' || time_interval from timescaledb_information.dimensions order by 1" -F' ')
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
    ensure_constraint_names "$db"
    ensure_hypertables "$db"
  done
  finish
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
  env=$(printf 'FWD_PORT=%s\nFWD_SOCKET=%s\nFWD_SSH=%s' "$FWD_PORT" "$SRC_SOCKET/.s.PGSQL.$SRC_PORT" "$FWD_SSH")
  if [[ ${1:-} == rev || $have == *FWD_REV=* ]]; then env+=$(printf '\nFWD_REV="-R 127.0.0.1:%s:%s"' "$REV_PORT" "$DST_SOCKET/.s.PGSQL.$DST_PORT"); fi
  unit=$(cat "$HERE/pg-forward@.service")
  if [[ $(on "$DST_SSH" "cat /etc/systemd/system/pg-forward@.service" 2>/dev/null || true) != "$unit" ]]; then
    put "$DST_SSH" /etc/systemd/system/pg-forward@.service 0644 root "$unit" "pg-forward@.service installed"
    run "$DST_SSH" "systemctl daemon-reload" "systemd reloaded"
    restart=1
  fi
  if [[ $have != "$env" ]]; then
    put "$DST_SSH" "$SECRETS/forward.env" 0600 root "$env" "$SECRETS/forward.env: 127.0.0.1:$FWD_PORT to $FWD_SSH:$SRC_SOCKET$([[ $env == *FWD_REV=* ]] && echo ", and $FWD_SSH 127.0.0.1:$REV_PORT back to this box")"
    restart=1
  fi
  if ((restart)); then
    run "$DST_SSH" "systemctl enable pg-forward@$CLUSTER && systemctl restart pg-forward@$CLUSTER" "pg-forward@$CLUSTER (re)started"
  elif ! on "$DST_SSH" "systemctl is-active --quiet pg-forward@$CLUSTER" 2>/dev/null; then
    run "$DST_SSH" "systemctl enable --now pg-forward@$CLUSTER" "pg-forward@$CLUSTER started"
  fi
  forward_wait "$DST_SSH" "$FWD_PORT"
  [[ $env != *FWD_REV=* ]] || forward_wait "$SRC_SSH" "$REV_PORT"
}

forward_wait() {
  ((DRY)) && return 0
  on "$1" "for i in \$(seq 30); do (exec 3<>/dev/tcp/127.0.0.1/$2) 2>/dev/null && exit 0; sleep 1; done; exit 1" ||
    die "pg-forward@$CLUSTER: nothing listens on $1 127.0.0.1:$2 after 30 s"
}

can_connect() {
  local side=$1 port=$2 db=$3 user
  local pass=''
  if [[ $side == src ]]; then user=$SRC_OSUSER pass=" passfile=$(src_passfile)"; else user=$DST_OSUSER; fi
  ((DRY)) && return 0
  on "$(host_of "$side")" "sudo -n -u $user psql $(printf %q "host=127.0.0.1 port=$port dbname=$db user=$REPL_ROLE sslmode=disable connect_timeout=10$pass") -XAtqc 'select 1'" >/dev/null 2>&1
}

ensure_marker() {
  [[ -n $FREEZE_MARKER ]] || return 0
  on "$SRC_SSH" "test -e $FREEZE_MARKER" 2>/dev/null && return 0
  run "$SRC_SSH" "install -d -m 0755 $(dirname "$FREEZE_MARKER") && echo \"pgmove $CLUSTER subscribed \$(date -u +%FT%TZ)\" >$FREEZE_MARKER" \
    "$SRC_SSH:$FREEZE_MARKER: migrations on the source refuse until retire"
}

sub_name() { if [[ $1 == rev ]]; then echo "pgmove_rev_$2"; else echo "pgmove_$2"; fi; }

ready_count() {
  q "$1" "$2" "select count(*) filter (where r.srsubstate <> 'r') || '|' || count(*) from pg_subscription_rel r join pg_subscription s on s.oid = r.srsubid where s.subname = $(lit "$3")"
}

ensure_publication() {
  local side=$1 db=$2 shape=$3 name=$4 list
  [[ -n $(q "$side" "$db" "select 1 from pg_publication where pubname = '$name'") ]] && return 0
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
  if [[ -n $FWD_PORT ]]; then
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
      conn=$(sed -E 's/(^| )passfile=[^ ]+//' <<<"$conn")
      apply dst "$db" "CREATE SUBSCRIPTION $sub CONNECTION $(lit "$conn") PUBLICATION $pubs WITH (binary = $BINARY, streaming = on, copy_data = true, disable_on_error = false);" \
        "dst $db: subscription $sub to the publisher of ${shape#upstream=} ($pubs), copy_data=true"
      continue
    fi
    conn="host=127.0.0.1 port=$FWD_PORT dbname=$db user=$REPL_ROLE application_name=$sub sslmode=disable"
    if ! can_connect dst "$FWD_PORT" "$db"; then
      blocked "the source refuses $REPL_ROLE through the forward; its hba needs 'local $db $REPL_ROLE scram-sha-256' above the peer lines, then a reload"
      break
    fi
    ensure_publication src "$db" "$shape" "$PUB"
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
      printf 'DELETE FROM %s WHERE %s >= %s AND %s < %s::date + 1;\n' "$table" "$HT_TIME" "$(lit "$day")" "$HT_TIME" "$(lit "$day")" | sql dst rw "$HT_DB" >/dev/null
      printf '\\copy (select * from %s where %s >= %s and %s < %s::date + 1) to stdout with (format binary)\n' "$table" "$HT_TIME" "$(lit "$day")" "$HT_TIME" "$(lit "$day")" | sql src ro "$HT_DB" |
        on "$DST_SSH" "$(psql_cmd dst rw "$HT_DB" "--single-transaction -c 'SET session_replication_role = replica' -c $(printf %q "\\copy $table from stdin with (format binary)")")" >/dev/null
      touch "$st/$day.done"
    done < <(q src "$HT_DB" "select d::date from generate_series($(lit "$cutoff")::date, (now() at time zone 'UTC')::date, interval '1 day') d")
  done
  finish "backfill from $cutoff"
}

floor_file() { if ((REVERSE)); then echo "$(ht_state "$1")/event_floor.rev"; else echo "$(ht_state "$1")/event_floor"; fi; }

catchup_tables() {
  local table f floor ceiling inserted cutoff
  ((${#HYPERTABLES[@]})) || return 0
  cutoff=$(ts_cutoff)
  for table in "${HYPERTABLES[@]}"; do
    f=$(floor_file "$table")
    [[ -s $f ]] || die "catchup: no floor $f (forward: run backfill; reverse: run switch)"
    floor=$(($(cat "$f") - CATCHUP_MARGIN))
    ceiling=$(q "$FROM" "$HT_DB" "select coalesce(max($HT_KEY), 0) from $table")
    if ((DRY)); then changed "$table rows above $HT_KEY $floor, $FROM to $TO"; continue; fi
    inserted=$(printf '\\copy (select * from %s where %s > %s and %s >= %s) to stdout with (format binary)\n' "$table" "$HT_KEY" "$floor" "$HT_TIME" "$(lit "$cutoff")" | sql "$FROM" ro "$HT_DB" |
      on "$(host_of "$TO")" "$(psql_cmd "$TO" rw "$HT_DB" "--single-transaction -c 'SET session_replication_role = replica' -c $(printf %q "CREATE TEMP TABLE catchup_rows (LIKE $table) ON COMMIT DROP") -c $(printf %q '\copy catchup_rows from stdin with (format binary)') -c $(printf %q "WITH moved AS (INSERT INTO $table SELECT c.* FROM catchup_rows c WHERE NOT EXISTS (SELECT 1 FROM $table t WHERE t.$HT_KEY = c.$HT_KEY AND t.$HT_TIME = c.$HT_TIME) RETURNING 1) SELECT count(*) FROM moved")")" | tail -1)
    state "$f" "$ceiling"
    ((inserted == 0)) || changed "$table +$inserted rows above $HT_KEY $floor, $FROM to $TO"
  done
}

cmd_catchup() {
  catchup_tables
  finish
}

SEQ_ROWS="select format('%I.%I', schemaname, sequencename) || '|' || coalesce(last_value::text, '-') from pg_sequences where schemaname not in ('pg_catalog','information_schema') and schemaname !~ '^(_timescaledb|timescaledb_)' order by 1"

sync_sequences() {
  local spec db have name value
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    have=$(q "$TO" "$db" "$SEQ_ROWS")
    while IFS='|' read -r name value; do
      [[ -n $name && $value != - ]] || continue
      grep -qxF -- "$name|$value" <<<"$have" && continue
      apply "$TO" "$db" "SELECT setval($(lit "$name"), $value, true);" "$TO $db: $name at $value as on $FROM"
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

check_grants() {
  local spec db start=$diffs ours
  compare_rows "role" "$(q src postgres "$ROLE_ROWS")" "$(q dst postgres "$ROLE_ROWS")"
  compare_rows "membership" "$(q src postgres "$MEMBER_ROWS")" "$(q dst postgres "$MEMBER_ROWS")"
  ours=$(for spec in "${DATABASES[@]}"; do db_of "$spec"; done | paste -sd'|' -)
  compare_rows "setting" "$(q src postgres "$SETTING_ROWS" | grep -E "^[^|]*\|(-|$ours)\|" || true)" "$(q dst postgres "$SETTING_ROWS" | grep -E "^[^|]*\|(-|$ours)\|" || true)"
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
bare = lambda t: re.sub(r"[()\s]", "", t)
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
union all select 'job ' || proc_name || ' ' || coalesce(format('%I.%I', hypertable_schema, hypertable_name), '-') || ' every ' || schedule_interval || ' ' || coalesce(config::text, '') from timescaledb_information.jobs where job_id >= 1000"

check_schema() {
  local spec db start=$diffs flags line tmp
  flags="--schema-only --no-owner --no-privileges --no-publications --no-subscriptions --exclude-schema='_timescaledb*' --exclude-schema='timescaledb_*'"
  tmp=$(mktemp -d)
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    on "$SRC_SSH" "$(src_dump "$db" "$flags")" >"$tmp/src.sql"
    on "$DST_SSH" "$(printf 'sudo -n -u %q pg_dump -h %q -p %q -d %q %s' "$DST_OSUSER" "$DST_SOCKET" "$DST_PORT" "$db" "$flags")" >"$tmp/dst.sql"
    while IFS= read -r line; do
      case $line in $'\t'*) echo "  ${line#$'\t'}" ;; ?*) diff_row "$db schema $line" ;; esac
    done < <(schema_diff "$tmp/src.sql" "$tmp/dst.sql")
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

hash_table() {
  local db=$1 table=$2 pk=$3 type=$4 nb=$5 bexpr where='' key tmp b n bad s d lo sc ss dc ds
  key=${pk%%,*}
  if is_hypertable "$table"; then
    bexpr="(t.$HT_KEY / $HASH_BUCKET)" where="where t.$HT_TIME >= $(lit "$(ts_cutoff)")"
  elif [[ -z $pk ]]; then
    bexpr=0::int
  elif [[ $type == smallint || $type == integer || $type == bigint ]]; then
    bexpr="(t.$key / $HASH_BUCKET)"
  else
    bexpr="(abs(hashtext(t.$key::text)::bigint) % $nb)"
  fi
  # The planner reads (key / bucket) as one group per row and sorts the whole table, which spilled
  # past the source's temp_file_limit on 17.9M rows; the real buckets fit a hash table in memory.
  sel() { echo "set enable_sort = off; select $bexpr || '|' || count(*) || '|' || sum(('x' || left(md5(t::text), 16))::bit(64)::bigint::numeric) from $table t $1 group by $bexpr order by 1"; }
  tmp=$(mktemp -d)
  q src "$db" "$(sel "$where")" >"$tmp/s" & s=$!
  q dst "$db" "$(sel "$where")" >"$tmp/d" & d=$!
  hash_wait "$db" "$table" $s $d
  bad=$(join -t'|' -a1 -a2 -e- -o 0,1.2,1.3,2.2,2.3 <(sort -t'|' -k1,1 "$tmp/s") <(sort -t'|' -k1,1 "$tmp/d") | awk -F'|' '$2 != $4 || $3 != $5 { print $1 }' | paste -sd, -)
  n=$(wc -l <"$tmp/s" | tr -d ' ')
  if [[ -n $bad ]]; then
    sleep "$HASH_RECHECK_SECS"
    if [[ $bexpr == "(t.$key / $HASH_BUCKET)" || $bexpr == "(t.$HT_KEY / $HASH_BUCKET)" ]]; then
      local col=${bexpr#(t.} ranges=
      col=${col%% *}
      for lo in ${bad//,/ }; do ranges="$ranges${ranges:+ or }(t.$col >= $((lo * HASH_BUCKET)) and t.$col < $(((lo + 1) * HASH_BUCKET)))"; done
      where="${where:-where true} and ($ranges)"
    else
      where="${where:-where true} and $bexpr in ($bad)"
    fi
    q src "$db" "$(sel "$where")" >"$tmp/s" & s=$!
    q dst "$db" "$(sel "$where")" >"$tmp/d" & d=$!
    hash_wait "$db" "$table" $s $d
    while IFS='|' read -r b sc ss dc ds; do
      [[ $sc == "$dc" && $ss == "$ds" ]] || diff_row "$db $table bucket $b: src $sc rows, dst $dc rows, sums differ twice $HASH_RECHECK_SECS s apart"
    done < <(join -t'|' -a1 -a2 -e- -o 0,1.2,1.3,2.2,2.3 <(sort -t'|' -k1,1 "$tmp/s") <(sort -t'|' -k1,1 "$tmp/d"))
  fi
  rm -rf "$tmp"
  echo "hash: $db $table $n bucket(s)${bad:+, first pass differed in bucket(s) $bad}"
}

hash_age() {
  local f=$WORK/hash-ok age
  if [[ ! -s $f ]]; then diff_row "hash: no 0-DIFF full hash recorded in $f"; return 0; fi
  age=$(( ($(date +%s) - $(cat "$f")) / 3600 ))
  if ((age >= HASH_MAX_AGE_HOURS)); then diff_row "hash: the last 0-DIFF full hash is ${age} h old (limit $HASH_MAX_AGE_HOURS h)"; else echo "hash: last 0-DIFF full hash ${age} h ago"; fi
}

cmd_hash() {
  local only=${ARGS[0]:-} spec db table pk type nb
  if ((AGE)); then hash_age; finish_check; return 0; fi
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    while IFS='|' read -r table pk type nb; do
      [[ -n $table && ( -z $only || $only == "$table" ) ]] || continue
      hash_table "$db" "$table" "$pk" "$type" "$nb"
    done < <(hash_meta "$db")
  done
  [[ -n $only || $diffs != 0 ]] || state "$WORK/hash-ok" "$(date +%s)"
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

check_tables() {
  local spec db shape start rows
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec") start=$diffs
    rows=$(join -t'|' -a1 -a2 -e- -o 0,1.2,2.2,1.3,2.3 \
      <(q "$FROM" "$db" "$VERIFY_TABLES" | sort -t'|' -k1,1) <(q "$TO" "$db" "$VERIFY_TABLES" | sort -t'|' -k1,1))
    while IFS='|' read -r table a b pa pb; do
      [[ -n $table ]] || continue
      if is_hypertable "$table"; then continue; fi
      if [[ $b == syncing:* || $a == - || $b == - || $pa != "$pb" ]] || { [[ $a != "~"* && $b != "~"* && $a != "$b" ]]; }; then
        diff_row "$db $table: $FROM rows $a max key $pa, $TO rows $b max key $pb"
      fi
    done <<<"$rows"
    compare_rows "$db sequence" "$(q "$FROM" "$db" "$SEQ_ROWS")" "$(q "$TO" "$db" "$SEQ_ROWS")"
    echo "verify: $db $(grep -c . <<<"$rows") tables and the sequences: $((diffs - start)) DIFF"
  done
  local table qh a b
  for table in ${HYPERTABLES[@]+"${HYPERTABLES[@]}"}; do
    qh="select coalesce(max($HT_KEY)::text, '-') || ' ' || count(*) from $table where $HT_TIME >= date_trunc('day', now()) - interval '1 day'"
    a=$(q "$FROM" "$HT_DB" "$qh") b=$(q "$TO" "$HT_DB" "$qh")
    if [[ $a == "$b" ]]; then echo "verify: $table max $HT_KEY and rows since yesterday $a on both"; else diff_row "$table max $HT_KEY and rows since yesterday: $FROM $a, $TO $b"; fi
  done
}

cmd_verify() {
  check_tables
  check_schema
  check_grants
  hash_age
  finish_check
}

LOGIN_ROLES="select rolname from pg_roles where rolcanlogin and rolname not in ('postgres', '$REPL_ROLE') and rolname !~ '^pg_' order by 1"

login_roles() {
  local f=$WORK/login-roles
  if [[ ! -s $f ]]; then
    state "$f" "$(q src postgres "$LOGIN_ROLES")"
    ((DRY == 0)) || q src postgres "$LOGIN_ROLES"
  fi
  [[ ! -s $f ]] || cat "$f"
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

cmd_fence() {
  local role spec db n slot
  upstream_blockers fencing
  ((blocks == 0)) || finish
  for role in $(login_roles); do
    kept "$role" && continue
    [[ $(q "$FROM" postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")") == f ]] ||
      apply "$FROM" postgres "SET lock_timeout = '2s'; ALTER ROLE $(ident "$role") NOLOGIN;" "$FROM: $role NOLOGIN"
    n=$(q "$FROM" postgres "select count(*) from pg_stat_activity where usename = $(lit "$role")")
    [[ -z $n || $n == 0 ]] || apply "$FROM" postgres "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $(lit "$role");" "$FROM: $n session(s) of $role ended"
  done
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    if ((REVERSE)); then slot=$(sub_name rev "$db"); else slot=$(sub_name fwd "$db"); fi
    slot_wait "$FROM" "$slot" "$db"
  done
  finish
}

sub_set() {
  local side=$1 db=$2 sub=$3 want=$4 have
  have=$(q "$side" "$db" "select subenabled from pg_subscription where subname = '$sub'")
  [[ -n $have && $have != "$want" ]] || return 0
  if [[ $want == t ]]; then apply "$side" "$db" "ALTER SUBSCRIPTION $sub ENABLE;" "$side $db: $sub enabled"; else apply "$side" "$db" "ALTER SUBSCRIPTION $sub DISABLE;" "$side $db: $sub disabled"; fi
}

# A resumed direction must skip the rows the other one applied while it was paused, or it sends
# them back and stops on their keys.
sub_resume() {
  local side=$1 db=$2 sub=$3
  [[ -n $(q "$side" "$db" "select 1 from pg_subscription where subname = '$sub'") ]] || return 0
  [[ $(q "$side" "$db" "select suborigin from pg_subscription where subname = '$sub'") == none ]] ||
    apply "$side" "$db" "ALTER SUBSCRIPTION $sub SET (origin = none);" "$side $db: $sub skips rows the other direction applied"
  sub_set "$side" "$db" "$sub" t
}

cmd_switch() {
  local role spec db table
  upstream_blockers "the switch"
  for role in $(login_roles); do
    kept "$role" && continue
    [[ $(q "$FROM" postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")") != t ]] || blocked "$FROM: $role can still log in; run fence$( ((REVERSE)) && echo ' --reverse') first"
  done
  ((blocks == 0)) || finish
  catchup_tables
  sync_sequences
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec")
    if ((REVERSE)); then
      sub_set src "$db" "$(sub_name rev "$db")" f
      sub_resume dst "$db" "$(sub_name fwd "$db")"
    else
      sub_set dst "$db" "$(sub_name fwd "$db")" f
    fi
  done
  if ((REVERSE == 0)); then
    for table in ${HYPERTABLES[@]+"${HYPERTABLES[@]}"}; do
      [[ -s $(ht_state "$table")/event_floor.rev ]] || state "$(ht_state "$table")/event_floor.rev" "$(q dst "$HT_DB" "select coalesce(max($HT_KEY), 0) from $table")"
    done
    arm_reverse
  fi
  for role in $(login_roles); do
    [[ $(q "$TO" postgres "select rolcanlogin from pg_roles where rolname = $(lit "$role")") == t ]] || apply "$TO" postgres "ALTER ROLE $(ident "$role") LOGIN;" "$TO: $role LOGIN"
  done
  if ((REVERSE)); then [[ -s $WORK/reversed ]] || state "$WORK/reversed" "$(date +%s)"; else [[ -s $WORK/switched ]] || state "$WORK/switched" "$(date +%s)"; fi
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
  ensure_pgpass src "$REV_PORT"
  ensure_forward rev
  for spec in "${DATABASES[@]}"; do
    db=$(db_of "$spec") shape=$(shape_of "$spec") sub=$(sub_name rev "$db")
    identity_blockers dst "$db" "$shape"
    ensure_publication dst "$db" "$shape" "$REV_PUB"
    if [[ -n $(q src "$db" "select 1 from pg_subscription where subname = '$sub'") ]]; then
      sub_resume src "$db" "$sub"
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
}

cmd_reverse() {
  arm_reverse
  finish
}

drop_sub() {
  local side=$1 db=$2 sub=$3 slot_side=$4
  if [[ -n $(q "$side" "$db" "select 1 from pg_subscription where subname = '$sub'") ]]; then
    apply "$side" "$db" "ALTER SUBSCRIPTION $sub DISABLE; ALTER SUBSCRIPTION $sub SET (slot_name = NONE); DROP SUBSCRIPTION $sub;" "$side $db: subscription $sub dropped"
  fi
  [[ -z $(q "$slot_side" "$db" "select 1 from pg_replication_slots where slot_name = '$sub'") ]] ||
    apply "$slot_side" "$db" "SELECT pg_drop_replication_slot('$sub');" "$slot_side: slot $sub dropped"
}

drop_pub() {
  [[ -z $(q "$1" "$2" "select 1 from pg_publication where pubname = '$3'") ]] || apply "$1" "$2" "DROP PUBLICATION $3;" "$1 $2: publication $3 dropped"
}

cmd_retire() {
  local stamp=$WORK/switched age spec db
  [[ -s $WORK/reversed ]] && stamp=$WORK/reversed
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
    drop_pub dst "$db" "$REV_PUB"
    drop_sub dst "$db" "$(sub_name fwd "$db")" src
    drop_pub src "$db" "$PUB"
  done
  if [[ -n $FREEZE_MARKER ]] && on "$SRC_SSH" "test -e $FREEZE_MARKER" 2>/dev/null; then
    run "$SRC_SSH" "rm -f $FREEZE_MARKER" "$SRC_SSH:$FREEZE_MARKER removed; migrations run again"
  fi
  if [[ -n $FWD_PORT ]] && on "$DST_SSH" "systemctl is-enabled --quiet pg-forward@$CLUSTER" 2>/dev/null; then
    run "$DST_SSH" "systemctl disable --now pg-forward@$CLUSTER" "pg-forward@$CLUSTER stopped and disabled"
  fi
  finish
}

WRITES_SQL="select '@epoch|' || extract(epoch from pg_postmaster_start_time())::bigint || '/' || coalesce(extract(epoch from stats_reset)::bigint, 0) from pg_stat_database where datname = current_database() union all select format('%I.%I', schemaname, relname) || '|' || (n_tup_ins + n_tup_upd + n_tup_del) from pg_stat_user_tables where schemaname not in ('_timescaledb_catalog', '_timescaledb_config')"

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
      if [[ $table == @reset ]]; then diff_row "$side $db: counters reset since $tag (restart or stats reset); writes cannot be counted"; continue; fi
      diff_row "$side $db $table $d since $tag"
      ((d < 0)) || total=$((total + d))
    done <<<"$out"
  done
  [[ $action == save ]] && return 0
  echo "writes: $side total $total since $tag"
  finish_check
}

if ((DRY)) && ! echo 'select 1' | sql dst ro postgres >/dev/null 2>&1; then
  DST_DOWN=1
  echo "dry run: the target ($DST_SSH, $DST_SOCKET:$DST_PORT) does not answer from $(hostname -s); its state reads as absent"
fi
"cmd_$CMD"
