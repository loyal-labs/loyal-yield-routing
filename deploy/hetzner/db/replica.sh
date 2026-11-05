#!/usr/bin/env bash
set -euo pipefail

SECRETS=/etc/loyal-db/replica
WORK=/var/lib/loyal-db-replica
CONF=/etc/postgresql/18/main/conf.d/zz-replica.conf
PUBLICATION=loyal_hz
TS_RETAIN_DAYS=${TS_RETAIN_DAYS:-62}
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
neon_ro() { PGOPTIONS="-c default_transaction_read_only=on -c statement_timeout=${NEON_STATEMENT_TIMEOUT:-0}" psql -XAtq -v ON_ERROR_STOP=1 "service=neon_$1" "${@:2}"; }
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
      awk -F'|' '{ exact = $2 !~ /^~/ && $3 !~ /^~/; flag = ($3 ~ /^syncing/ || $4 != $5 || (exact && $2 != $3)) ? "  DIFF" : ""; printf "%-70s %16s %16s %s | %s%s\n", $1, $2, $3, $4, $5, flag }'
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

cmd_metrics() {
  local out=/var/lib/prometheus/node-exporter/loyal_db_replica.prom
  local spec src sub sql row active lag enabled workers last
  {
    echo '# TYPE loyal_db_subscription_enabled gauge'
    echo '# TYPE loyal_db_subscription_worker_up gauge'
    echo '# TYPE loyal_db_subscription_last_msg_receipt_timestamp_seconds gauge'
    for spec in "${DATABASES[@]}"; do
      sub=loyal_hz_$(field "$spec" 1)
      [[ -n $(local_sql -d postgres -c "select 1 from pg_subscription where subname='$sub'") ]] || continue
      sql="select coalesce((select subenabled::int from pg_subscription where subname='$sub'), 0), (select count(*) from pg_stat_subscription where subname='$sub' and worker_type='apply' and pid is not null), coalesce((select extract(epoch from max(last_msg_receipt_time))::bigint::text from pg_stat_subscription where subname='$sub' and worker_type='apply'), '')"
      IFS='|' read -r enabled workers last < <(local_sql -d postgres -c "$sql")
      echo "loyal_db_subscription_enabled{subscription=\"$sub\"} $enabled"
      echo "loyal_db_subscription_worker_up{subscription=\"$sub\"} $((workers > 0 ? 1 : 0))"
      [[ -z $last ]] || echo "loyal_db_subscription_last_msg_receipt_timestamp_seconds{subscription=\"$sub\"} $last"
    done
    echo '# TYPE loyal_db_neon_probe_success gauge'
    echo '# TYPE loyal_db_neon_slot_active gauge'
    echo '# TYPE loyal_db_neon_slot_lag_bytes gauge'
    for spec in "${DATABASES[@]}"; do
      src=$(field "$spec" 1) sub=loyal_hz_$(field "$spec" 1)
      [[ -n $(local_sql -d postgres -c "select 1 from pg_subscription where subname='$sub'") ]] || continue
      if row=$(PGCONNECT_TIMEOUT=10 NEON_STATEMENT_TIMEOUT=20000 neon_ro "$src" -c "select coalesce(bool_or(active), false)::int || ' ' || coalesce(max(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn))::bigint, 0) from pg_replication_slots where slot_name='$sub'" 2>/dev/null); then
        read -r active lag <<<"$row"
        echo "loyal_db_neon_probe_success{source=\"$src\"} 1"
        echo "loyal_db_neon_slot_active{slot=\"$sub\"} $active"
        echo "loyal_db_neon_slot_lag_bytes{slot=\"$sub\"} $lag"
      else
        echo "loyal_db_neon_probe_success{source=\"$src\"} 0"
      fi
    done
    echo '# TYPE loyal_db_pg_wal_bytes gauge'
    echo "loyal_db_pg_wal_bytes $(local_sql -d postgres -c 'select coalesce(sum(size), 0) from pg_ls_waldir()')"
    echo '# TYPE loyal_db_replica_metrics_last_success_timestamp_seconds gauge'
    echo "loyal_db_replica_metrics_last_success_timestamp_seconds $(date +%s)"
  } >"$out.tmp"
  chmod 0644 "$out.tmp"
  mv -f "$out.tmp" "$out"
}

cmd_retain() {
  local table dropped
  for table in "${HYPERTABLES[@]}"; do
    if [[ ${1:-} == --dry-run ]]; then
      local_sql -d kamino_timescale -F' ' -c "select 'would drop ' || c || ' ' || pg_size_pretty(pg_total_relation_size(c)) from show_chunks('$table', older_than => interval '$TS_RETAIN_DAYS days') c order by c::text"
    else
      dropped=$(local_sql -d kamino_timescale -c "select drop_chunks('$table', older_than => interval '$TS_RETAIN_DAYS days')")
      echo "retain: $table dropped $(grep -c . <<<"$dropped") chunk(s) older than $TS_RETAIN_DAYS days"
      [[ -z $dropped ]] || echo "$dropped"
    fi
  done
}

HBA=/etc/postgresql/18/main/pg_hba.conf
PGB_USERS=/etc/pgbouncer/loyal-users.txt
CUTOVER=$WORK/cutover
CLIENT_NETS=(10.92.0.0/16 127.0.0.1/32)
USER_NS="n.nspname not in ('pg_catalog','information_schema') and n.nspname !~ '^(_timescaledb|timescaledb_|pg_toast|pg_temp)' and not exists (select 1 from pg_depend e where e.classid = 'pg_namespace'::regclass and e.objid = n.oid and e.deptype = 'e')"
NOT_EXT="not exists (select 1 from pg_depend e where e.objid = c.oid and e.classid = 'pg_class'::regclass and e.deptype = 'e')"
WRITES_SQL="select '@epoch|' || extract(epoch from pg_postmaster_start_time())::bigint || '/' || coalesce(extract(epoch from stats_reset)::bigint, 0) from pg_stat_database where datname = current_database() union all select format('%I.%I', schemaname, relname) || '|' || (n_tup_ins + n_tup_upd + n_tup_del) from pg_stat_user_tables where schemaname not in ('_timescaledb_catalog', '_timescaledb_config')"
DUMP_FLAGS=(--schema-only --no-owner --no-privileges --no-publications --no-subscriptions --exclude-schema='_timescaledb*' --exclude-schema='timescaledb_*')
ROLE_SETTINGS="select name || '=' || setting from pg_settings where name in ('TimeZone','search_path','DateStyle','IntervalStyle','lock_timeout','idle_in_transaction_session_timeout','idle_session_timeout','transaction_timeout','default_transaction_isolation','client_encoding','standard_conforming_strings','statement_timeout') order by 1"

indent() { sed 's/^/  /'; }
local_dump() { sudo -u postgres pg_dump "$@"; }

owner_sql() {
  local db=$1 role=$2
  PGPASSWORD=$(cat "$SECRETS/$role.password") psql -XAtq -v ON_ERROR_STOP=1 "host=127.0.0.1 port=${PGB_PORT:-5432} dbname=$db user=$role sslmode=require application_name=loyal-cutover" "${@:3}"
}

cmd_access() {
  local spec db role net line verifier hba=0 users=0
  for spec in "${DATABASES[@]}"; do
    db=$(field "$spec" 2) role=$(field "$spec" 3)
    if [[ $(local_sql -c "select rolcanlogin and coalesce(rolpassword like 'SCRAM-SHA-256\$%', false) from pg_authid where rolname='$role'") != t ]]; then
      echo "access: $role cannot log in with a SCRAM password; run setup" >&2
      exit 1
    fi
    for net in "${CLIENT_NETS[@]}"; do
      line="hostssl $db $role $net scram-sha-256"
      grep -qxF "$line" "$HBA" && continue
      grep -qE '^host +all +all +0\.0\.0\.0/0 +reject' "$HBA" || { echo "access: no catch-all reject line in $HBA" >&2; exit 1; }
      awk -v l="$line" '!done && /^host +all +all +0\.0\.0\.0\/0 +reject/ { print l; done = 1 } { print }' "$HBA" >"$HBA.new"
      cat "$HBA.new" >"$HBA" && rm -f "$HBA.new"
      hba=1
      changed "$HBA: $line"
    done
    verifier=$(local_sql -c "select rolpassword from pg_authid where rolname='$role'")
    line="\"$role\" \"$verifier\""
    if ! grep -qxF "$line" "$PGB_USERS"; then
      { grep -v "^\"$role\" " "$PGB_USERS" || true; echo "$line"; } >"$PGB_USERS.new"
      cat "$PGB_USERS.new" >"$PGB_USERS" && rm -f "$PGB_USERS.new"
      users=1
      changed "$PGB_USERS: $role SCRAM verifier"
    fi
  done
  if ((hba)); then
    [[ $(local_sql -c "select count(*) from pg_hba_file_rules where error is not null") == 0 ]] || { echo "access: pg_hba.conf has errors, not reloaded" >&2; exit 1; }
    local_sql -c "select pg_reload_conf()" >/dev/null
  fi
  ((users)) && systemctl reload loyal-pgbouncer
  echo "access: $changes change(s)"
}

priv_sql() {
  cat <<EOF
select kind || '|' || name || '|' || priv || '|' || ok from (
  select 'table' as kind, format('%I.%I', n.nspname, c.relname) as name, p as priv, has_table_privilege($1, c.oid, p)::text as ok
  from pg_class c join pg_namespace n on n.oid = c.relnamespace cross join unnest(array['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p
  where c.relkind in ('r','p','v','m','f') and $USER_NS and $NOT_EXT
  union all
  select 'sequence', format('%I.%I', n.nspname, c.relname), p, has_sequence_privilege($1, c.oid, p)::text
  from pg_class c join pg_namespace n on n.oid = c.relnamespace cross join unnest(array['USAGE','SELECT','UPDATE']) p
  where c.relkind = 'S' and $USER_NS and $NOT_EXT
  union all
  select 'schema', format('%I', n.nspname), p, has_schema_privilege($1, n.oid, p)::text
  from pg_namespace n cross join unnest(array['USAGE','CREATE']) p where $USER_NS
  union all
  select 'function', format('%I.%I(%s)', n.nspname, f.proname, pg_get_function_identity_arguments(f.oid)), 'EXECUTE', has_function_privilege($1, f.oid, 'EXECUTE')::text
  from pg_proc f join pg_namespace n on n.oid = f.pronamespace
  where $USER_NS and not exists (select 1 from pg_depend d where d.classid = 'pg_proc'::regclass and d.objid = f.oid and d.deptype = 'e')
  union all
  select 'database', '-', p, has_database_privilege($1, current_database(), p)::text from unnest(array['CONNECT','CREATE','TEMPORARY']) p
) x order by 1
EOF
}

cmd_grants() {
  local spec src db role gaps=0 out kind name priv
  mkdir -p "$CUTOVER"
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1) db=$(field "$spec" 2) role=$(field "$spec" 3)
    neon_ro "$src" -c "$(priv_sql current_user)" >"$CUTOVER/grants-neon-$src"
    local_sql -d "$db" -c "$(priv_sql "'$role'")" >"$CUTOVER/grants-hz-$src"
    out=$(awk -F'|' 'NR == FNR { n[$1 "|" $2 "|" $3] = $4; next } { h[$1 "|" $2 "|" $3] = $4 }
      END {
        for (k in n) if (!(k in h)) print "missing-on-hz|" k; else if (n[k] == "true" && h[k] != "true") print "gap|" k; else if (n[k] != h[k]) print "extra-on-hz|" k
        for (k in h) if (!(k in n)) print "missing-on-neon|" k
      }' "$CUTOVER/grants-neon-$src" "$CUTOVER/grants-hz-$src" | sort)
    echo "grants: $db as $role vs Neon $src as current_user: $(wc -l <"$CUTOVER/grants-neon-$src") checks, $(grep -c '^gap|' <<<"$out" || true) gap(s), $(grep -c '^extra-on-hz|' <<<"$out" || true) extra on Hetzner, $(grep -c '^missing' <<<"$out" || true) object(s) on one side only"
    [[ -z $out ]] || indent <<<"$out"
    while IFS='|' read -r _ kind name priv; do
      if [[ ${1:-} == --fix ]]; then
        local_sql -d "$db" -c "GRANT $priv ON ${kind^^} $name TO $role"
        changed "$db: GRANT $priv ON ${kind^^} $name TO $role"
      else
        gaps=$((gaps + 1))
      fi
    done < <(grep '^gap|' <<<"$out" || true)
    out=$(grep -vxF -f <(owner_sql "$db" "$role" -c "$ROLE_SETTINGS") <(PGOPTIONS='-c default_transaction_read_only=on' psql -XAtq "service=neon_$src" -c "$ROLE_SETTINGS") || true)
    echo "settings: $db as $role: $(grep -c . <<<"$out" || true) differ from Neon $src as current_user"
    while IFS='=' read -r name value; do
      [[ -n $name ]] || continue
      if [[ ${1:-} == --fix ]]; then
        printf 'ALTER ROLE %s IN DATABASE %s SET %s = %s;\n' "$role" "$db" "$name" "$(sql_literal "$value")" | local_sql
        changed "$db: ALTER ROLE $role SET $name = '$value' (the Neon value)"
      else
        echo "  $name: Neon '$value'"
        gaps=$((gaps + 1))
      fi
    done <<<"$out"
  done
  ((gaps == 0)) || { echo "grants: $gaps gap(s); rerun with --fix" >&2; exit 1; }
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
neon, hz = blocks(sys.argv[1]), blocks(sys.argv[2])
bare = lambda t: re.sub(r"[()\s]", "", t)
drift = 0
print(f"  {len(neon)} objects on Neon, {len(hz)} on Hetzner (extensions compared by name and version)")
for k in sorted(neon.keys() | hz.keys()):
    a, b = neon.get(k), hz.get(k)
    if a == b:
        continue
    if a is not None and b is not None and bare(a) == bare(b):
        print(f"  deparse-only: {k} (same tokens, redundant parentheses differ)")
        continue
    drift = 1
    print(f"  DRIFT {k}: {'only on Neon' if b is None else 'only on Hetzner' if a is None else 'definition differs'}")
    for side, t in (("<", a), (">", b)):
        for l in (t or "").splitlines():
            print(f"    {side} {l}")
sys.exit(drift)
PY
}

EXT_SQL="select extname || ' ' || extversion from pg_extension order by 1"

TS_SETTINGS=$(cat <<'EOF'
select 'hypertable ' || format('%I.%I', hypertable_schema, hypertable_name) || ' dims=' || num_dimensions || ' compression=' || compression_enabled from timescaledb_information.hypertables
union all select 'dimension ' || format('%I.%I', hypertable_schema, hypertable_name) || ' ' || column_name || ' ' || coalesce(time_interval::text, integer_interval::text) from timescaledb_information.dimensions
union all select 'compression ' || hypertable::text || ' segmentby=' || coalesce(segmentby, '-') || ' orderby=' || coalesce(orderby, '-') from timescaledb_information.hypertable_compression_settings
union all select 'job ' || proc_name || ' ' || coalesce(format('%I.%I', hypertable_schema, hypertable_name), '-') || ' every ' || schedule_interval || ' ' || coalesce(config::text, '') from timescaledb_information.jobs where job_id >= 1000
order by 1
EOF
)

cmd_schema() {
  local spec src db drift=0 d
  mkdir -p "$CUTOVER"
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1) db=$(field "$spec" 2)
    PGOPTIONS='-c default_transaction_read_only=on' pg_dump "service=neon_$src" "${DUMP_FLAGS[@]}" -f "$CUTOVER/schema-neon-$src.sql"
    local_dump -d "$db" "${DUMP_FLAGS[@]}" >"$CUTOVER/schema-hz-$src.sql"
    echo "schema: $db vs Neon $src"
    schema_diff "$CUTOVER/schema-neon-$src.sql" "$CUTOVER/schema-hz-$src.sql" || drift=1
    echo "  extensions: Neon $(neon_ro "$src" -c "$EXT_SQL" | paste -sd, -) | Hetzner $(local_sql -d "$db" -c "$EXT_SQL" | paste -sd, -)"
  done
  d=$(diff <(neon_ro timescale -c "$TS_SETTINGS") <(local_sql -d kamino_timescale -c "$TS_SETTINGS") || true)
  echo "timescale settings: $(grep -c '^[<>]' <<<"$d" || true) differing line(s) (< Neon, > Hetzner)"
  local_sql -d kamino_timescale -c "$TS_SETTINGS" | indent
  [[ -z $d ]] || { indent <<<"$d"; drift=1; }
  exit $drift
}

cmd_pgbouncer_test() {
  local spec db role
  for spec in "${DATABASES[@]}"; do
    db=$(field "$spec" 2) role=$(field "$spec" 3)
    PGB_PORT=6432 owner_sql "$db" "$role" -f - <<'SQL' | tr '\n' ' '
select 'pgbouncer: ' || current_user || ' ' || current_database() || ' via 6432';
select $1::int * 10 as v \parse cutover_probe
\bind_named cutover_probe 1 \g
\bind_named cutover_probe 2 \g
\close_prepared cutover_probe
SQL
    echo "(named prepared statement reused across transactions)"
  done
}

cmd_lag_wait() {
  local spec src db target deadline=$((SECONDS + ${1:-600}))
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1) db=$(field "$spec" 2)
    if [[ $(local_sql -d "$db" -c "select subenabled from pg_subscription where subname = 'loyal_hz_$src'") == f ]]; then
      echo "lag-wait: subscription loyal_hz_$src is disabled, so an earlier switch already passed lag-wait; skipped"
      continue
    fi
    target=$(neon_ro "$src" -c "select pg_current_wal_lsn()")
    until [[ $(neon_ro "$src" -c "select coalesce(bool_and(confirmed_flush_lsn >= '$target'), false) from pg_replication_slots where slot_name = 'loyal_hz_$src'") == t ]]; do
      ((SECONDS < deadline)) || { echo "lag-wait: slot loyal_hz_$src has not confirmed $target" >&2; exit 1; }
      sleep 1
    done
    echo "lag-wait: slot loyal_hz_$src confirmed past $target"
  done
}

cmd_writes() {
  local action=$1 tag=$2 only=${3:-} spec src db file unknown=0
  mkdir -p "$CUTOVER"
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1) db=$(field "$spec" 2)
    for side in neon hz; do
      [[ -z $only || $only == "$side" ]] || continue
      file=$CUTOVER/writes-$tag-$side-$src
      if [[ $side == neon ]]; then neon_ro "$src" -c "$WRITES_SQL"; else local_sql -d "$db" -c "$WRITES_SQL"; fi >"$file.now"
      grep -q '^@epoch|' "$file.now" || { echo "writes: $side $src returned no counters" >&2; exit 1; }
      if [[ $action == save ]]; then
        mv -f "$file.now" "$file"
        echo "writes: saved $tag $side $src ($(($(wc -l <"$file") - 1)) tables)"
        continue
      fi
      [[ -s $file ]] || { echo "writes: no snapshot $file" >&2; exit 1; }
      awk -F'|' -v l="$side $src" -v tag="$tag" 'NR == FNR { was[$1] = $2; next }
        $1 == "@epoch" { r += ($2 != was[$1]); next }
        { d = $2 - was[$1] } d != 0 { printf "writes: %s %s %+d\n", l, $1, d; t += (d < 0 ? 0 : d); r += (d < 0) }
        END { printf "writes: %s total %d since %s%s\n", l, t, tag, (r ? ", UNKNOWN: counters reset (restart or stats reset), writes since " tag " cannot be counted" : ""); exit (r > 0) }' \
        "$file" "$file.now" || unknown=1
      rm -f "$file.now"
    done
  done
  exit $unknown
}

cmd_sessions() {
  local spec src
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1)
    neon_ro "$src" -F' ' -c "select 'sessions: Neon $src', coalesce(nullif(application_name, ''), '-'), coalesce(host(client_addr), 'local'), state, count(*) from pg_stat_activity where backend_type = 'client backend' and datname = current_database() and coalesce(application_name, '') <> 'loyal-hz-replica' and pid <> pg_backend_pid() group by 2, 3, 4 order by 5 desc"
  done
}

cmd_subscriptions() {
  local spec src db
  if [[ $1 == enabled ]]; then
    for spec in "${DATABASES[@]}"; do
      src=$(field "$spec" 1) db=$(field "$spec" 2)
      [[ $(local_sql -d "$db" -c "select subenabled from pg_subscription where subname = 'loyal_hz_$src'") == t ]] || { echo "subscription loyal_hz_$src: not enabled" >&2; exit 1; }
    done
    echo "subscriptions: all enabled"
    return 0
  fi
  for spec in "${DATABASES[@]}"; do
    src=$(field "$spec" 1) db=$(field "$spec" 2)
    [[ -n $(local_sql -d "$db" -c "select 1 from pg_subscription where subname = 'loyal_hz_$src'") ]] || { echo "subscription loyal_hz_$src: absent"; continue; }
    case $1 in
      disable | enable) local_sql -d "$db" -c "ALTER SUBSCRIPTION loyal_hz_$src ${1^^}" ;;
      drop) local_sql -d "$db" -c "ALTER SUBSCRIPTION loyal_hz_$src DISABLE" -c "ALTER SUBSCRIPTION loyal_hz_$src SET (slot_name = NONE)" -c "DROP SUBSCRIPTION loyal_hz_$src" ;;
    esac
    echo "subscription loyal_hz_$src: $1"
  done
  if [[ $1 == drop ]]; then
    for spec in "${DATABASES[@]}"; do
      src=$(field "$spec" 1)
      neon_rw "$src" -c "select 'Neon slot ' || slot_name || ' dropped' from pg_replication_slots, pg_drop_replication_slot(slot_name) where slot_name = 'loyal_hz_$src'"
    done
  fi
}

case ${1:-} in
  access) cmd_access ;;
  grants) cmd_grants "${2:-}" ;;
  schema) cmd_schema ;;
  pgbouncer-test) cmd_pgbouncer_test ;;
  lag-wait) cmd_lag_wait "${2:-600}" ;;
  writes) cmd_writes "$2" "$3" "${4:-}" ;;
  sessions) cmd_sessions ;;
  subscriptions) cmd_subscriptions "$2" ;;
  setup) cmd_setup ;;
  backfill) cmd_backfill ;;
  catchup) cmd_catchup ;;
  verify) cmd_verify ;;
  sequences) cmd_sequences ;;
  check) cmd_check ;;
  metrics) cmd_metrics ;;
  retain) cmd_retain "${2:-}" ;;
  *) echo "usage: $0 setup|backfill|catchup|sequences|verify|check|metrics|retain [--dry-run]|access|grants [--fix]|schema|pgbouncer-test|lag-wait [secs]|writes save|diff <tag> [neon|hz]|sessions|subscriptions enabled|disable|enable|drop" >&2; exit 2 ;;
esac
