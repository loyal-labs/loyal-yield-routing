#!/usr/bin/env bash
# Run as root on the target box, no agent needed: it reads only the target and the upstream the
# target already subscribes to (conninfo from the target's own catalog, never printed).
#   parsers-copy.sh progress    tables by state, bytes on the target, the upstream's slots and their lag
#   parsers-copy.sh abandon     disable and drop the pgmove_* subscriptions; each drops its own slots upstream
set -euo pipefail

PORT=5433
DBS=(fomo pump)
sub() { echo "pgmove_$1"; }
pg() { local db=$1; shift; sudo -n -u postgres psql -h /var/run/postgresql -p $PORT -d "$db" -XAtq -v ON_ERROR_STOP=1 "$@"; }

upstream() {
  local conn
  conn=$(pg "$1" -c "select subconninfo from pg_subscription where subname = '$(sub "$1")'")
  [[ -n $conn ]] || { echo "no subscription $(sub "$1") on the target" >&2; return 1; }
  shift
  sudo -n -u postgres /usr/lib/postgresql/18/bin/psql "$conn" -XAtq -v ON_ERROR_STOP=1 "$@"
}

upstream_slots() {
  upstream "$1" -F ' ' -c "select slot_name, active, coalesce(pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)), '-') as behind, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) as retained from pg_replication_slots where slot_type = 'logical' order by slot_name"
}

progress() {
  local db
  for db in "${DBS[@]}"; do
    if [[ -z $(pg "$db" -c "select 1 from pg_subscription where subname = '$(sub "$db")'") ]]; then
      echo "$db: not subscribed yet (rerun pgmove subscribe parsers once the database before it is all r)"
      continue
    fi
    echo "== $db"
    pg "$db" -F ' ' -c "select 'tables', srsubstate, count(*) from pg_subscription_rel r join pg_subscription s on s.oid = r.srsubid where s.subname = '$(sub "$db")' group by srsubstate order by srsubstate"
    pg "$db" -F ' ' -c "select 'copying', relid::regclass, pg_size_pretty(bytes_processed) as copied, tuples_processed from pg_stat_progress_copy order by bytes_processed desc"
    echo "target $(pg "$db" -c "select pg_size_pretty(pg_database_size(current_database()))"), upstream $(upstream "$db" -c "select pg_size_pretty(pg_database_size(current_database()))")"
    echo "apply: $(pg "$db" -F ' ' -c "select count(*) filter (where relid is null) || ' apply worker, ' || count(*) filter (where relid is not null) || ' sync workers' from pg_stat_subscription where subname = '$(sub "$db")'")"
    echo "upstream slots (name active behind retained):"
    upstream_slots "$db" | sed 's/^/  /'
  done
}

abandon() {
  local db conn=''
  conn=$(pg "${DBS[0]}" -c "select subconninfo from pg_subscription where subname = '$(sub "${DBS[0]}")'")
  for db in "${DBS[@]}"; do
    if [[ -n $(pg "$db" -c "select 1 from pg_subscription where subname = '$(sub "$db")'") ]]; then
      pg "$db" -c "ALTER SUBSCRIPTION $(sub "$db") DISABLE"
      pg "$db" -c "DROP SUBSCRIPTION $(sub "$db")"
      echo "dropped $(sub "$db") on the target and its slots upstream"
    fi
  done
  echo "upstream slots left (the two feeding the old box stay):"
  if [[ -n $conn ]]; then
    sudo -n -u postgres /usr/lib/postgresql/18/bin/psql "$conn" -XAtq -F ' ' -c "select slot_name, active from pg_replication_slots where slot_type = 'logical' order by slot_name" | sed 's/^/  /'
  else
    echo "  (no subscription was left to read the upstream through)"
  fi
}

case ${1:-} in
  progress) progress ;;
  abandon) abandon ;;
  *) echo "usage: $0 progress | abandon" >&2; exit 2 ;;
esac
