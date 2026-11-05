#!/usr/bin/env bash
set -euo pipefail

SECRETS=/etc/loyal-db/replica
WORK=/var/lib/loyal-db-replica
CONF=/etc/postgresql/18/main/conf.d/zz-replica.conf
PUBLICATION=loyal_hz
TS_RETAIN_DAYS=62
CATCHUP_EVENT_MARGIN=10000
ARCHIVE_BACKLOG_MAX=512
export PGSERVICEFILE=$SECRETS/pg_service.conf

DATABASES=(
  "app:loyal_app:loyal_app_owner"
  "yield:loyal_yield_prod:loyal_yield_prod_owner"
  "timescale:kamino_timescale:kamino_timescale_owner"
)
NEON_ACL_ROLES=(loyal_web_stats_reader loyal_vault_demo_observation)
HYPERTABLES=(kamino.reserve_updates loyal_prod.balance_sweep_wallet_ata_observations)

changes=0
changed() { changes=$((changes + 1)); echo "changed: $*"; }
local_sql() { sudo -u postgres psql -XAtq -v ON_ERROR_STOP=1 "$@"; }
neon_ro() { PGOPTIONS='-c default_transaction_read_only=on -c statement_timeout=0' psql -XAtq -v ON_ERROR_STOP=1 "service=neon_$1" "${@:2}"; }
neon_rw() { psql -XAtq -v ON_ERROR_STOP=1 "service=neon_$1" "${@:2}"; }
field() { local IFS=: parts; read -ra parts <<<"$1"; echo "${parts[$2 - 1]}"; }

ensure_conf() {
  local want
  want=$(cat <<'EOF'
max_worker_processes = 16
max_logical_replication_workers = 10
max_sync_workers_per_subscription = 2
max_parallel_apply_workers_per_subscription = 0
max_active_replication_origins = 10
EOF
)
  if [[ "$(cat "$CONF" 2>/dev/null)" != "$want" ]]; then
    printf '%s\n' "$want" >"$CONF"
    chown postgres:postgres "$CONF"
    systemctl restart postgresql@18-main
    changed "$CONF written, postgres restarted"
  fi
}

ensure_service_file() {
  local rendered
  rendered=$(python3 - "$SECRETS" <<'PY'
import sys, urllib.parse as u
for name in ("app", "yield", "timescale"):
    p = u.urlparse(open(f"{sys.argv[1]}/{name}.url").read().strip())
    q = dict(u.parse_qsl(p.query))
    print(f"[neon_{name}]")
    print(f"host={p.hostname.replace('-pooler', '')}")
    print(f"port={p.port or 5432}")
    print(f"dbname={p.path.lstrip('/')}")
    print(f"user={u.unquote(p.username)}")
    print(f"password={u.unquote(p.password)}")
    print("sslmode=require")
    print(f"channel_binding={q.get('channel_binding', 'prefer')}")
    print("application_name=loyal-hz-replica")
    print()
PY
)
  if [[ "$(cat "$PGSERVICEFILE" 2>/dev/null)" != "$rendered" ]]; then
    (umask 077; printf '%s\n' "$rendered" >"$PGSERVICEFILE")
    changed "$PGSERVICEFILE rendered from the Neon URLs"
  fi
}

conninfo() {
  python3 - "$PGSERVICEFILE" "neon_$1" <<'PY'
import sys, configparser
c = configparser.ConfigParser(interpolation=None)
c.read(sys.argv[1])
s = c[sys.argv[2]]
q = lambda v: "'" + v.replace("\\", "\\\\").replace("'", "\\'") + "'"
print(" ".join(f"{k}={q(s[k])}" for k in ("host", "port", "dbname", "user", "password", "sslmode", "channel_binding", "application_name")))
PY
}

sql_literal() { printf "'%s'" "${1//\'/\'\'}"; }

ensure_roles() {
  local role pwfile
  for spec in "${DATABASES[@]}"; do
    role=$(field "$spec" 3)
    pwfile=$SECRETS/$role.password
    if [[ -z $(local_sql -c "select 1 from pg_roles where rolname='$role'") ]]; then
      local_sql -c "CREATE ROLE $role NOLOGIN"
      changed "role $role created"
    fi
    if [[ ! -s $pwfile ]]; then
      (umask 077; openssl rand -base64 33 | tr -d '/+=\n' >"$pwfile")
      printf 'ALTER ROLE %s LOGIN PASSWORD %s;\n' "$role" "$(sql_literal "$(cat "$pwfile")")" | local_sql
      changed "role $role can log in, password in $pwfile"
    fi
  done
  for role in "${NEON_ACL_ROLES[@]}"; do
    if [[ -z $(local_sql -c "select 1 from pg_roles where rolname='$role'") ]]; then
      local_sql -c "CREATE ROLE $role NOLOGIN"
      changed "role $role created NOLOGIN for restored grants"
    fi
  done
}

ensure_database() {
  local db=$1 owner=$2
  if [[ -z $(local_sql -c "select 1 from pg_database where datname='$db'") ]]; then
    local_sql -c "CREATE DATABASE $db OWNER $owner"
    changed "database $db created"
  fi
  if [[ -z $(local_sql -c "select 1 from pg_db_role_setting s join pg_database d on d.oid=s.setdatabase join pg_roles r on r.oid=s.setrole where d.datname='$db' and r.rolname='postgres' and 'statement_timeout=0' = any(s.setconfig)") ]]; then
    local_sql -c "ALTER ROLE postgres IN DATABASE $db SET statement_timeout = 0" -c "ALTER ROLE postgres IN DATABASE $db SET idle_in_transaction_session_timeout = 0"
    changed "postgres in $db: no statement or idle timeout for replication workers"
  fi
}

user_relations() {
  local_sql -d "$1" -c "select count(*) from pg_class c join pg_namespace n on n.oid=c.relnamespace where c.relkind in ('r','p','v','m','S') and n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^(_timescaledb|timescaledb_|pg_toast)'"
}

ensure_schema() {
  local src=$1 db=$2 owner=$3 dump=$WORK/schema-$1.dump list=$WORK/schema-$1.list
  if [[ $src == timescale && -z $(local_sql -d "$db" -c "select 1 from pg_extension where extname='timescaledb'") ]]; then
    local_sql -d "$db" -c "CREATE EXTENSION timescaledb VERSION '$(neon_ro timescale -c "select extversion from pg_extension where extname='timescaledb'")'"
    changed "$db: timescaledb extension created"
  fi
  [[ $(user_relations "$db") == 0 ]] || return 0
  mkdir -p "$WORK"
  PGOPTIONS='-c default_transaction_read_only=on' pg_dump "service=neon_$src" --schema-only -Fc \
    --exclude-schema='_timescaledb*' --exclude-schema='timescaledb_*' -f "$dump"
  pg_restore -l "$dump" | grep -v -E ' EXTENSION - | COMMENT - EXTENSION |DEFAULT ACL .* cloud_admin$' >"$list"
  pg_restore -L "$list" --no-owner --role="$owner" -f - "$dump" | local_sql --single-transaction -d "$db" >/dev/null
  changed "$db: schema restored from Neon $src ($(grep -c . "$list") TOC entries, owner $owner)"
  if [[ $src == timescale ]]; then
    neon_ro timescale -F' ' -c "select format('%I.%I', hypertable_schema, hypertable_name), column_name, time_interval from timescaledb_information.dimensions order by 1" |
      while read -r table column interval; do
        local_sql -d "$db" -c "SET ROLE $owner" -c "SELECT create_hypertable('$table', by_range('$column', interval '$interval'))" >/dev/null
        changed "$db: $table is a hypertable on $column every $interval"
      done
  fi
}

publication_tables() {
  neon_ro "$1" -c "select string_agg(format('%I.%I', n.nspname, c.relname), ', ' order by 1) from pg_class c join pg_namespace n on n.oid=c.relnamespace where c.relkind='r' and n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^(_timescaledb|timescaledb_)' and not exists (select 1 from timescaledb_information.hypertables h where h.hypertable_schema=n.nspname and h.hypertable_name=c.relname)"
}

ensure_publication() {
  local src=$1
  [[ -n $(neon_ro "$src" -c "select 1 from pg_publication where pubname='$PUBLICATION'") ]] && return 0
  if [[ $src == timescale ]]; then
    neon_rw "$src" -c "CREATE PUBLICATION $PUBLICATION FOR TABLE $(publication_tables "$src")"
  else
    neon_rw "$src" -c "CREATE PUBLICATION $PUBLICATION FOR ALL TABLES"
  fi
  changed "Neon $src: publication $PUBLICATION created"
}

ensure_subscription() {
  local src=$1 db=$2 sub=loyal_hz_$1
  [[ -n $(local_sql -d "$db" -c "select 1 from pg_subscription where subname='$sub'") ]] && return 0
  printf "CREATE SUBSCRIPTION %s CONNECTION %s PUBLICATION %s WITH (binary = true, streaming = on, disable_on_error = false);\n" \
    "$sub" "$(sql_literal "$(conninfo "$src")")" "$PUBLICATION" | local_sql -d "$db"
  changed "$db: subscription $sub created (slot $sub on Neon $src)"
}

cmd_setup() {
  ensure_conf
  ensure_service_file
  ensure_roles
  for spec in "${DATABASES[@]}"; do
    local src db owner
    src=$(field "$spec" 1) db=$(field "$spec" 2) owner=$(field "$spec" 3)
    ensure_database "$db" "$owner"
    ensure_schema "$src" "$db" "$owner"
    ensure_publication "$src"
    ensure_subscription "$src" "$db"
  done
  echo "setup: $changes change(s)"
}

archive_backlog() { find /srv/postgresql/18/main/pg_wal/archive_status -name '*.ready' | wc -l; }

wait_for_archiver() {
  while (( $(archive_backlog) > ARCHIVE_BACKLOG_MAX )); do
    echo "backfill: waiting, $(archive_backlog) WAL segments not yet archived"
    sleep 300
  done
}

ts_state() { echo "$WORK/backfill/${1//./_}"; }

ts_cutoff() {
  local file=$WORK/backfill/cutoff
  if [[ ! -s $file ]]; then
    mkdir -p "$WORK/backfill"
    neon_ro timescale -c "select (date_trunc('day', now() at time zone 'UTC') - interval '$TS_RETAIN_DAYS days')::date" >"$file"
  fi
  cat "$file"
}

cmd_backfill() {
  local cutoff table state day next start_event
  cutoff=$(ts_cutoff)
  for table in "${HYPERTABLES[@]}"; do
    state=$(ts_state "$table")
    mkdir -p "$state"
    if [[ ! -s $state/event_floor ]]; then
      neon_ro timescale -c "select coalesce(max(event_id), 0) from $table" >"$state/event_floor"
    fi
    day=$cutoff
    while [[ $day < $(date -u -d tomorrow +%F) ]]; do
      next=$(date -u -d "$day + 1 day" +%F)
      if [[ ! -e $state/$day.done ]]; then
        wait_for_archiver
        local_sql -d kamino_timescale -c "DELETE FROM $table WHERE observed_at >= '$day' AND observed_at < '$next'" >/dev/null
        neon_ro timescale -c "\\copy (select * from $table where observed_at >= '$day' and observed_at < '$next') to stdout with (format binary)" |
          local_sql -d kamino_timescale --single-transaction -c "SET session_replication_role = replica" -c "\\copy $table from stdin with (format binary)" >/dev/null
        touch "$state/$day.done"
        echo "backfill: $table $day copied"
      fi
      day=$next
    done
  done
  echo "backfill: done from $cutoff"
}

cmd_catchup() {
  local cutoff table state floor ceiling inserted
  cutoff=$(ts_cutoff)
  for table in "${HYPERTABLES[@]}"; do
    state=$(ts_state "$table")
    [[ -s $state/event_floor ]] || { echo "catchup: run backfill first" >&2; exit 1; }
    floor=$(( $(cat "$state/event_floor") - CATCHUP_EVENT_MARGIN ))
    ceiling=$(neon_ro timescale -c "select coalesce(max(event_id), 0) from $table")
    inserted=$(neon_ro timescale -c "\\copy (select * from $table where event_id > $floor and observed_at >= '$cutoff') to stdout with (format binary)" |
      local_sql -d kamino_timescale --single-transaction \
        -c "SET session_replication_role = replica" \
        -c "CREATE TEMP TABLE catchup_rows (LIKE $table) ON COMMIT DROP" \
        -c "\\copy catchup_rows from stdin with (format binary)" \
        -c "WITH moved AS (INSERT INTO $table SELECT c.* FROM catchup_rows c WHERE NOT EXISTS (SELECT 1 FROM $table t WHERE t.event_id = c.event_id AND t.observed_at = c.observed_at) RETURNING 1) SELECT count(*) FROM moved" | tail -1)
    echo "$ceiling" >"$state/event_floor"
    echo "catchup: $table +$inserted rows above event_id $floor; next floor $ceiling"
  done
}

cmd_verify() {
  local src db
  local q_tables
  q_tables=$(cat <<'EOF'
with t as (
  select c.oid, n.nspname, c.relname, c.reltuples::bigint as est, pg_relation_size(c.oid) as bytes,
    (select r.srsubstate::text from pg_subscription_rel r where r.srrelid=c.oid and r.srsubstate <> 'r') as sync,
    (select a.attname from pg_index i join pg_attribute a on a.attrelid=i.indrelid and a.attnum=i.indkey[0] where i.indrelid=c.oid and i.indisprimary) as pk
  from pg_class c join pg_namespace n on n.oid=c.relnamespace
  where c.relkind in ('r','p') and n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^(_timescaledb|timescaledb_)'
)
select format('%I.%I', nspname, relname),
  case when sync is not null then 'syncing:' || sync when bytes < 268435456 then (xpath('/table/row/v/text()', query_to_xml(format('select count(*) as v from %I.%I', nspname, relname), false, false, '')))[1]::text when est >= 0 then '~' || est else '~' || pg_size_pretty(bytes) end,
  case when sync is not null then '-' else coalesce((xpath('/table/row/v/text()', query_to_xml(format('select %I::text as v from %I.%I order by %I desc limit 1', pk, nspname, relname, pk), false, false, '')))[1]::text, '-') end
from t where pk is not null order by 1
EOF
)
  echo "verify-replica $(date -u +%FT%TZ)"
  echo "box: $(df -h --output=avail /srv/postgresql | tail -1 | tr -d ' ') free on /srv/postgresql, pg_wal $(du -sh /srv/postgresql/18/main/pg_wal | cut -f1), $(archive_backlog) segments waiting for pgbackrest"
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1) db=$(field "$spec" 2)
    echo
    echo "== $db (Neon $src)"
    printf '%-70s %16s %16s %s\n' table neon_rows hz_rows "max_pk neon | hz"
    join -t'|' -a1 -a2 -e '-' -o 0,1.2,2.2,1.3,2.3 \
      <(neon_ro "$src" -c "$q_tables" | sort -t'|' -k1,1) \
      <(local_sql -d "$db" -c "$q_tables" | sort -t'|' -k1,1) |
      awk -F'|' '{ flag = ($4 == $5 || $3 ~ /^syncing/) ? "" : "  DIFF"; printf "%-70s %16s %16s %s | %s%s\n", $1, $2, $3, $4, $5, flag }'
    echo "-- subscription (Hetzner)"
    local_sql -d "$db" -F' ' -c "select 'tables by state: ' || string_agg(st::text || '=' || n, ' ' order by st) from (select srsubstate as st, count(*) n from pg_subscription_rel r join pg_subscription s on s.oid=r.srsubid where s.subname='loyal_hz_$src' group by 1) x" \
      -c "select 'worker ' || coalesce(relid::regclass::text, 'apply') || ' received ' || coalesce(received_lsn::text, '-') || ' last_msg ' || coalesce(round(extract(epoch from now() - last_msg_receipt_time))::text, '-') || 's ago' from pg_stat_subscription where subname='loyal_hz_$src'" \
      -c "select 'copy in progress ' || relid::regclass || ' ' || pg_size_pretty(bytes_processed) || ' written' from pg_stat_progress_copy where datname=current_database()"
    echo "-- slot (Neon)"
    neon_ro "$src" -c "select format('slot %s active=%s wal_status=%s lag_bytes=%s lag_pretty=%s', slot_name, active, wal_status, pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn), pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn))) from pg_replication_slots where slot_name='loyal_hz_$src'" \
      -c "select format('walsender %s state=%s write_lag=%s flush_lag=%s replay_lag=%s', application_name, state, coalesce(write_lag::text,'-'), coalesce(flush_lag::text,'-'), coalesce(replay_lag::text,'-')) from pg_stat_replication where application_name like 'loyal_hz_$src%'"
  done
  echo
  echo "== kamino_timescale hypertables (cutoff $(ts_cutoff 2>/dev/null || echo unset))"
  for table in "${HYPERTABLES[@]}"; do
    local q="select 'max_event_id=' || coalesce(max(event_id)::text,'-') || ' max_observed_at=' || coalesce(max(observed_at)::text,'-') || ' rows_since_yesterday=' || count(*) from $table where observed_at >= date_trunc('day', now()) - interval '1 day'"
    echo "$table neon: $(neon_ro timescale -c "$q")"
    echo "$table hz:   $(local_sql -d kamino_timescale -c "$q")  days_done=$(ls "$(ts_state "$table")" 2>/dev/null | grep -c '\.done$' || true)"
  done
}

cmd_sequences() {
  local src db name value applied
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1) db=$(field "$spec" 2) applied=0
    while IFS='|' read -r name value; do
      local_sql -d "$db" -c "SELECT setval('$name', $value, true)" >/dev/null
      applied=$((applied + 1))
    done < <(neon_ro "$src" -c "select format('%I.%I', schemaname, sequencename), last_value from pg_sequences where last_value is not null and schemaname not in ('pg_catalog','information_schema') and schemaname !~ '^(_timescaledb|timescaledb_)'")
    echo "sequences: $db $applied set from Neon $src"
  done
}

cmd_check() {
  local src bad=0 row
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1)
    row=$(neon_ro "$src" -c "select coalesce(bool_and(active)::text, 'missing') || ' ' || coalesce(max(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn))::bigint, 0) from pg_replication_slots where slot_name='loyal_hz_$src'")
    read -r active lag <<<"$row"
    if [[ $active != true || $lag -gt 5368709120 ]]; then
      echo "ALERT slot loyal_hz_$src active=$active lag_bytes=$lag"
      bad=1
    fi
  done
  exit $bad
}

case ${1:-} in
  setup) cmd_setup ;;
  backfill) cmd_backfill ;;
  catchup) cmd_catchup ;;
  verify) cmd_verify ;;
  sequences) cmd_sequences ;;
  check) cmd_check ;;
  *) echo "usage: $0 setup|backfill|catchup|sequences|verify|check" >&2; exit 2 ;;
esac
