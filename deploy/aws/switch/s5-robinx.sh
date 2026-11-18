#!/usr/bin/env bash
# Switch S5: robinX production (robinx-prod 5.161.126.23) to robinx-main. Runs from the operator's Mac.
# Only `preflight` exists; the switch runs from deploy/aws/db (pgmove.sh, cutover.sh) by the steps in the S5 runbook.
set -uo pipefail

DIR=$(cd "$(dirname "$0")/.." && pwd)
DB=$DIR/db
OLD=${S5_OLD:-root@5.161.126.23}
OLD_ADDR=${OLD#*@}
NEW=${S5_NEW:-robinx-main}
NEW_ADDR=${S5_NEW_ADDR:-34.229.25.188}
WEN=${WEN:-$HOME/loyal/wen}
AGENT=${PGMOVE_AGENT:-/tmp/pgmove-agent.sock}
FREEZE_HOURS=${S5_FREEZE_HOURS:-120}
WAL_MARGIN_GB=${S5_WAL_MARGIN_GB:-64}
KEYLESS="'invite_deposits','tracked_wallets_dueat_bak_20260925'"
SETTINGS="'max_connections','superuser_reserved_connections','shared_buffers','effective_cache_size','work_mem',
'maintenance_work_mem','autovacuum_work_mem','autovacuum_max_workers','max_parallel_workers',
'max_parallel_workers_per_gather','max_parallel_maintenance_workers','effective_io_concurrency','maintenance_io_concurrency',
'temp_file_limit','idle_in_transaction_session_timeout','wal_compression','wal_buffers','max_wal_size','min_wal_size',
'checkpoint_timeout','track_io_timing','track_wal_io_timing','pg_stat_statements.track','shared_preload_libraries'"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30)

usage() {
  cat >&2 <<EOF
usage: $0 preflight
preflight  Read-only. Runs every check even after one fails, prints one line per check as ok, BLOCKER or PARKED and ends
           "<n> ok, <m> blocker, <k> parked". Exit 0 only when no check is a BLOCKER. PARKED is an owner call or a known gap
           that does not stop the subscribe. Env: S5_BUILD (the arm64 build tree, else the binaries are compared by release
           sha), S5_FREEZE_HOURS (${FREEZE_HOURS}), PGMOVE_AGENT (${AGENT}), WEN (${WEN}).
EOF
  exit 2
}

# shellcheck disable=SC2029
old() { ssh "${SSH_OPTS[@]}" "$OLD" "$@"; }
# shellcheck disable=SC2029
new() { ssh "${SSH_OPTS[@]}" "$NEW" "$@"; }
srcq() { old "cd /tmp && sudo -u postgres psql -h /run/robinx-postgres -d robinx -XAtq -v ON_ERROR_STOP=1" <<<"$1"; }
dstq() { new "cd /tmp && sudo -n -u postgres psql -p 5432 -d ${2:-robinx} -XAtq -v ON_ERROR_STOP=1" <<<"$1"; }
pgmove() { local sub=$1; shift; PGMOVE_AGENT=$AGENT bash "$DB/remote.sh" run "$sub" robinx "$@"; }
conf() { bash -c 'source "$1"; eval "echo \"\${$2[*]}\""' _ "$DB/clusters/robinx.conf" "$1"; }
gb() { awk -v b="$1" 'BEGIN { printf "%.0f GiB", b / 1073741824 }'; }

OK=0 BLOCK=0 PARK=0
# check <name> <fn>: fn prints its detail; exit 0 is ok, 3 is PARKED, anything else is a BLOCKER.
check() {
  local name=$1 detail rc
  detail=$("$2" 2>&1)
  rc=$?
  detail=$(tr '\n' ' ' <<<"${detail:-no output}" | sed 's/ *$//')
  case $rc in
    0) OK=$((OK + 1)); echo "ok $name: $detail" ;;
    3) PARK=$((PARK + 1)); echo "PARKED $name: $detail" ;;
    *) BLOCK=$((BLOCK + 1)); echo "BLOCKER $name: $detail" ;;
  esac
}

c_0348() {
  local ri mig
  ri=$(srcq "select string_agg(relname || '=' || relreplident::text, ' ' order by relname) from pg_class where relname in ($KEYLESS) and relnamespace = 'public'::regnamespace") || return 1
  mig=$(srcq "select applied_at from schema_history where name = '0348_replica_identity_full_keyless'") || return 1
  echo "replica identity $ri; 0348 applied ${mig:-never}"
  [[ $ri == "invite_deposits=f tracked_wallets_dueat_bak_20260925=f" && -n $mig ]]
}

c_source_clean() {
  local n
  n=$(srcq "select (select count(*) from pg_publication) || ' publication(s), ' || (select count(*) from pg_replication_slots) || ' slot(s)'") || return 1
  echo "robinx-prod: $n"
  [[ $n == "0 publication(s), 0 slot(s)" ]] || { echo "(subscribe has run)"; return 3; }
}

c_subscribe_dry() {
  local out rc
  out=$(pgmove subscribe --dry-run 2>&1)
  rc=$?
  grep '^blocked:' <<<"$out"
  echo "exit $rc, $(grep -E 'change\(s\)' <<<"$out" | tail -1)"
  ((rc == 0)) && ! grep -q '^blocked:' <<<"$out" && schema_first
}

# A dry run against a target with no tables cannot meet the conflicts the real run would.
TARGET_TABLES=''
schema_first() { [[ $TARGET_TABLES != 0 ]] || { echo "(robinx-main has no tables yet: rerun once target-schema is ok)"; return 3; }; }

c_partitions_dry() {
  local out rc
  out=$(pgmove partitions --dry-run 2>&1)
  rc=$?
  echo "exit $rc, $(grep -E 'change\(s\)' <<<"$out" | tail -1)"
  ((rc == 0)) && schema_first
}

c_init_rerun() {
  local out rc
  out=$(pgmove init --dry-run 2>&1)
  rc=$?
  echo "exit $rc, $(grep -E 'change\(s\)|^blocked:' <<<"$out" | tail -1)"
  ((rc == 0)) && grep -q '^init: 0 change(s)' <<<"$out"
}

c_schema() {
  local q a b
  q="select count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace where c.relkind in ('r','p')
     and n.nspname not in ('pg_catalog','information_schema') and n.nspname not like 'pg_toast%'"
  a=$(srcq "$q") || return 1
  b=$(dstq "$q") || { echo "robinx-main has no database robinx"; return 1; }
  echo "tables: robinx-prod $a, robinx-main $b"
  [[ $a == "$b" ]]
}

c_cluster() {
  local line mnt avail src dst need
  line=$(new "sudo -n pg_lsclusters --no-header" | awk '$1 == 18 && $2 == "production"')
  mnt=$(new "sudo -n findmnt -n -o TARGET -T /srv/robinx-data/production")
  avail=$(new "df -B1 --output=avail /srv/robinx-data | tail -1" | tr -d ' ')
  src=$(srcq "select pg_database_size('robinx')") || return 1
  dst=$(dstq "select pg_database_size('robinx')" 2>/dev/null || echo 0)
  need=$((src + WAL_MARGIN_GB * 1073741824))
  echo "${line:-no cluster 18 production}; mount $mnt; free $(gb "$avail") + robinX here $(gb "$dst") against robinx-prod $(gb "$src") + ${WAL_MARGIN_GB} GiB WAL"
  [[ $line =~ ^18\ +production\ +5432\ +online\ +postgres\ +/srv/robinx-data/production\  && $mnt == /srv/robinx-data && $((avail + dst)) -ge $need ]]
}

c_slot_cap() {
  local r cap rate hours h rh
  r=$(srcq "select (select setting::bigint * 1024 * 1024 from pg_settings where name = 'max_slot_wal_keep_size'),
    (select (wal_bytes / greatest(extract(epoch from now() - stats_reset), 1) * 3600)::bigint from pg_stat_wal),
    (select round(extract(epoch from now() - stats_reset) / 3600, 1) from pg_stat_wal)") || return 1
  IFS='|' read -r cap rate hours <<<"$r"
  ((cap > 0 && rate > 0)) || { echo "cap $cap bytes, WAL $rate bytes/h"; return 1; }
  h=$((cap / rate)) rh=$(conf REVERSE_HOURS)
  rh=${rh:-72}
  echo "cap $(gb "$cap") at $(gb $((rate * 24)))/day (measured over ${hours} h) holds ${h} h of WAL; freeze ${FREEZE_HOURS} h, reverse window ${rh} h"
  ((h >= FREEZE_HOURS)) && return 0
  ((h >= rh)) && return 3
  return 1
}

c_settings() {
  local q a b d
  q="select name || '=' || setting || coalesce(unit, '') from pg_settings where name in ($SETTINGS) order by name"
  a=$(srcq "$q") || return 1
  b=$(dstq "$q" postgres) || return 1
  d=$(join -t= <(sort <<<"$a") <(sort <<<"$b") | awk -F= '$2 != $3 { printf "%s %s/%s ", $1, $2, $3 }')
  echo "${d:-every listed setting equal}${d:+(robinx-prod/robinx-main)}"
  [[ -z $d ]]
}

c_backup() {
  local ts unit
  ts=$(old "grep -h 'robinx_pgbackrest_last_backup_timestamp_seconds{stanza=\"robinx\",type=\"full\"}' /var/lib/node_exporter/textfile/robinx_pgbackrest.prom" | awk '{ print $2 }')
  unit=$(old "systemctl show robinx-backup@full.service -p Result --value")
  [[ $ts =~ ^[0-9]+$ ]] || { echo "no robinX full in the textfile metric"; return 1; }
  echo "newest good full $(date -u -r "$ts" +%FT%TZ), $(( ($(date +%s) - ts) / 86400 )) days old; last full run result $unit"
  return 3
}

# One read-only run of robinx-main-config.sh check feeds two lines.
CONFIG_OUT=''
c_config() {
  local s l
  grep -E '^(differs|release|check|robinx-main-config):' <<<"$CONFIG_OUT"
  read -r s l < <(sed -n 's/^release: robinx-main stages \([0-9a-f]*\), robinx-prod runs \([0-9a-f]*\).*/\1 \2/p' <<<"$CONFIG_OUT")
  grep -q '^check: 0 difference(s)$' <<<"$CONFIG_OUT" && [[ ${#s} == 40 && $s == "$l" ]]
}
c_secrets() {
  local a b
  grep -E '^(secrets:|  not matching)' <<<"$CONFIG_OUT" | sed 's/^secrets: //' || echo "no secrets line"
  read -r a b < <(sed -n 's/^secrets: \([0-9]*\) of \([0-9]*\) .*/\1 \2/p' <<<"$CONFIG_OUT")
  [[ -n $a && $a == "$b" ]] && ! grep -q 'not matching' <<<"$CONFIG_OUT"
}

c_units_robinx() {
  local pat="'robinx*' 'cloudflared.service' 'prometheus*' 'alertmanager*' 'node_exporter*' 'postgres_exporter*'" e a
  e=$(new "systemctl list-unit-files $pat --state=enabled --no-legend" | awk '$1 !~ /[.]slice$/ { print $1 }' | paste -sd' ' -)
  a=$(new "systemctl list-units $pat --all --state=active,activating,reloading --no-legend --plain" | awk '$1 !~ /[.]slice$/ { print $1 }' | paste -sd' ' -)
  echo "enabled: ${e:-none}; active: ${a:-none} (robinx.slice is active: it holds the Postgres clusters)"
  [[ -z $e && -z $a ]]
}

c_units_mnemonic() {
  local pat="'mnemonic*' 'cloudflared-mnemonic*'" e a
  e=$(new "systemctl list-unit-files $pat --state=enabled --no-legend" | awk '{ print $1 }' | paste -sd' ' -)
  a=$(new "systemctl list-units $pat --all --state=active,activating,reloading --no-legend --plain" | awk '{ print $1 }' | paste -sd' ' -)
  echo "enabled: ${e:-none}; active: ${a:-none}"
  [[ -z $e && -z $a ]] && return 0
  echo "(S1 has switched mnemonic here; S1 owns these units)"
  return 3
}

c_login() {
  local r d
  r=$(ssh "${SSH_OPTS[@]}" "root@$NEW_ADDR" true 2>&1 && echo accepted || echo refused)
  d=$(ssh "${SSH_OPTS[@]}" "deploy@$NEW_ADDR" true 2>&1 && echo accepted || echo refused)
  echo "root@$NEW_ADDR ${r##*$'\n'}, deploy@$NEW_ADDR ${d##*$'\n'} from this Mac; robinx-prod's root and deploy authorized_keys are copied at the switch (owner, Nov 18)"
  return 3
}

c_recovery() {
  local a b
  a=$(old "stat -c '%a %U:%G' /srv/robinx-data/recovery" 2>&1)
  b=$(new "sudo -n stat -c '%a %U:%G' /srv/robinx-data/recovery" 2>&1)
  echo "/srv/robinx-data/recovery: robinx-prod $a, robinx-main $b"
  [[ $a == "$b" && $b == "700 postgres:postgres" ]]
}

c_repoint() {
  local out n
  out=$(cd "$WEN" && bash deploy/aws/repoint.sh check 2>&1) || { tail -1 <<<"$out"; return 1; }
  n=$(sed -n 's/^\([0-9]*\) pinned$/\1/p' <<<"$out")
  echo "wen $(git -C "$WEN" rev-parse --short HEAD): ${n:-?} old-address pins listed, none applied"
  [[ $n =~ ^[0-9]+$ ]] && ((n > 0))
}

c_alias() {
  local h
  h=$(ssh -G wen-prod 2>/dev/null | awk '$1 == "hostname" { print $2 }')
  echo "wen-prod resolves to $h (switch edits it to $NEW_ADDR)"
  [[ $h == "$OLD_ADDR" || $h == "$NEW_ADDR" ]]
}

c_cloudflared() {
  local a b s
  a=$(old "dpkg-query -W -f '\${Version}' cloudflared")
  b=$(new "dpkg-query -W -f '\${Version}' cloudflared")
  s=$(new "echo \$(systemctl is-enabled cloudflared.service 2>&1) \$(systemctl is-active cloudflared.service 2>&1)")
  echo "robinx-prod $a, robinx-main $b, robinx-main cloudflared.service $s"
  [[ -n $a && $a == "$b" && $s == "disabled inactive" ]]
}

c_slots() {
  local stop svc live
  stop=$(conf CUT_STOP) svc=$(conf CUT_SERVICES)
  live=$(old "readlink /opt/robinx/current" | sed 's|.*robinx-||' | cut -c1-9)
  echo "CUT_STOP=($stop) CUT_SERVICES=($svc); robinx-prod runs $live"
  [[ " $stop " == *" robinx@8080 "* && " $stop " == *" robinx@8081 "* && $svc == *robinx@LIVE_SLOT* ]]
}

preflight() {
  CONFIG_OUT=$(BUILD=${S5_BUILD:-} WEN=$WEN bash "$DIR/robinx-main-config.sh" check 2>&1)
  TARGET_TABLES=$(dstq "select count(*) from pg_class where relkind in ('r','p') and relnamespace = 'public'::regnamespace" 2>/dev/null || echo 0)
  check 0348-on-production c_0348
  check source-untouched c_source_clean
  check subscribe-dry-run c_subscribe_dry
  check target-cluster c_cluster
  check target-schema c_schema
  check target-settings c_settings
  check init-rerun c_init_rerun
  check partitions-dry-run c_partitions_dry
  check slot-cap c_slot_cap
  check backup c_backup
  check config-rerun c_config
  check secrets c_secrets
  check units-robinx c_units_robinx
  check units-mnemonic c_units_mnemonic
  check login c_login
  check recovery-dir c_recovery
  check repoint c_repoint
  check wen-prod-alias c_alias
  check cloudflared c_cloudflared
  check app-slots c_slots
  echo "$OK ok, $BLOCK blocker, $PARK parked"
  ((BLOCK == 0))
}

case ${1:-} in
  preflight) preflight ;;
  *) usage ;;
esac
