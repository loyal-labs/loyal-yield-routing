#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
B=/Users/user/loyal/migration-plans/neon-to-hetzner-2026-09-29
YIELD_ENV=/Users/user/loyal/loyal-yield-routing/.env.1password
APP_ENV=/Users/user/loyal/loyal-apps/.env.1password
PROOF=/Users/user/loyal/artifacts/orchestrate/loyal-hetzner/proof/hz-db1

box() {
  ssh -S none -o ControlMaster=no -o BatchMode=yes -o StrictHostKeyChecking=yes \
    -o UserKnownHostsFile="$B/deployment/known-hosts" -o HostKeyAlias=178.156.223.43 \
    -o IdentitiesOnly=yes -i "$B/deployment/admin-public-key.pub" -J loyal-workers root@10.92.1.1 "$@"
}
export -f box
export B

install() {
  box 'f=/usr/local/sbin/loyal-db-replica; install -m 0755 /dev/stdin $f.new && mv -f $f.new $f' <"$HERE/replica.sh"
}

case ${1:-} in
  urls)
    box 'umask 077; mkdir -p /etc/loyal-db/replica'
    op run --env-file="$YIELD_ENV" -- bash -c '
      printf "%s\n" "$NEON_DATABASE_URL" | box "umask 077; cat > /etc/loyal-db/replica/yield.url"
      printf "%s\n" "$TIMESCALEDB_URL" | box "umask 077; cat > /etc/loyal-db/replica/timescale.url"'
    op run --env-file="$APP_ENV" -- bash -c '
      printf "%s\n" "$DATABASE_URL" | box "umask 077; cat > /etc/loyal-db/replica/app.url"'
    ;;
  install) install ;;
  setup | catchup | sequences | verify | check)
    install
    box "loyal-db-replica $1"
    ;;
  backfill)
    install
    box 'systemctl is-active --quiet loyal-ts-backfill || systemd-run --unit=loyal-ts-backfill --nice=10 --property=StandardOutput=append:/var/lib/loyal-db-replica/backfill.log --property=StandardError=append:/var/lib/loyal-db-replica/backfill.log /usr/local/sbin/loyal-db-replica backfill; systemctl status --no-pager loyal-ts-backfill | head -5'
    ;;
  neon-cu)
    key=$(op read 'op://Engineering/f4adqjr3dxnqofikunbwaajumy/credential')
    curl -fsS -H "Authorization: Bearer $key" \
      "https://console.neon.tech/api/v2/consumption_history/v2/projects?org_id=org-noisy-cake-62775570&from=$(date -u -v-${2:-6}H +%Y-%m-%dT%H:00:00Z)&to=$(date -u -v+1H +%Y-%m-%dT%H:00:00Z)&granularity=hourly&metrics=compute_unit_seconds&project_ids=purple-wave-56227231,curly-glade-19640752" |
      python3 -c 'import json,sys
for p in json.load(sys.stdin)["projects"]:
    for per in p["periods"]:
        for c in per["consumption"]:
            v = c["metrics"][0]["value"]
            print(p["project_id"], c["timeframe_start"], "avg_cu=%.2f" % (v / 3600))'
    ;;
  *) echo "usage: $0 urls|install|setup|backfill|catchup|sequences|verify|check|neon-cu [hours]" >&2; exit 2 ;;
esac
