#!/usr/bin/env bash
# Compares an old host with its AWS replacement and prints only what differs; read-only on both hosts.
# usage: host-diff.sh <old-alias> <new-alias>
# Compared: installed package versions (arch suffix ignored), systemd unit files in /etc/systemd/system by name and
# sha256 (the hash is taken on the host, contents are never read here), and the names, never contents, of everything
# under the /etc trees the services read, drop-in files included.
# Each host gets one `bash -s` over ssh with the collector on stdin: nothing is written there, not even in /tmp.
# Output lines: "pkg|unit|etc <name>: <how it differs>", then "<n> difference(s)". Exit 0 when n is 0, else 1.
# Env, all optional: HOST_DIFF_PKGS (package-name regex), HOST_DIFF_UNITS (unit-name regex),
# HOST_DIFF_TREES (space-separated /etc paths), HOST_DIFF_EXCLUDE (name regex for backup clutter),
# HOST_DIFF_SSH_OPTS (extra ssh options, e.g. "-F /path/config").
set -euo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 <old-alias> <new-alias>" >&2; exit 2; }
OLD=$1 NEW=$2

PKGS=${HOST_DIFF_PKGS:-'^(postgresql.*|libpq5|pgbouncer|pgbackrest|timescaledb.*|python3-psycopg2|docker.*|containerd|runc|prometheus.*|caddy|cloudflared|otelcol.*|ssl-cert|jq|curl|git|rsync|nftables|iptables|wireguard-tools|sqlite3|fio|openssl|wget|python3|python3[.]12|bash|ca-certificates|systemd|libc6|zstd)$'}
UNITS=${HOST_DIFF_UNITS:-'^(loyal-|cloudflared|otelcol|prometheus|postgresql|pgbouncer|caddy|pg-forward|wg-quick@loyal-ci)'}
TREES=${HOST_DIFF_TREES:-'/etc/loyal /etc/loyal-db /etc/loyal-observer-monitor /etc/loyal-squads-watch /etc/credstore.encrypted /etc/cloudflared /etc/caddy /etc/otelcol-contrib /etc/prometheus /etc/pgbouncer /etc/postgresql /etc/postgresql-common/createcluster.conf /etc/pgbackrest /etc/pgbackrest.conf /etc/wireguard /etc/nftables.conf /etc/default/prometheus /etc/default/prometheus-alertmanager /etc/default/prometheus-node-exporter /etc/apt/sources.list.d'}
EXCLUDE=${HOST_DIFF_EXCLUDE:-'(\.(bak|prev[0-9]*|slow)(-|$)|\.pre-|\.dpkg-|[.-]before$|\.before\.)'}

collector() {
  printf 'PKGS=%q UNITS=%q TREES=%q EXCLUDE=%q\n' "$PKGS" "$UNITS" "$TREES" "$EXCLUDE"
  cat <<'EOF'
dpkg-query -W -f '${binary:Package}\t${Version}\n' |
  awk -F'\t' -v re="$PKGS" '{ sub(/:[a-z0-9]+$/, "", $1); if ($1 ~ re) print "pkg\t" $1 "\t" $2 }'
for f in /etc/systemd/system/*; do
  [ -f "$f" ] && [ ! -L "$f" ] || continue
  b=${f##*/}
  case $b in *.service | *.timer | *.socket | *.path | *.mount | *.target) ;; *) continue ;; esac
  printf '%s\n' "$b" | grep -Eq "$UNITS" || continue
  printf 'unit\t%s\t%s\n' "$b" "$(sha256sum "$f" | cut -d' ' -f1)"
done
{
  for t in $TREES; do [ -e "$t" ] && find "$t" -printf '%p\n'; done
  find /etc/systemd/system -mindepth 2 -maxdepth 2 -path '*.d/*' -printf '%P\n' | while IFS= read -r p; do
    printf '%s\n' "${p%%/*}" | grep -Eq "$UNITS" && printf '/etc/systemd/system/%s\n' "$p"
  done
} | grep -Ev "$EXCLUDE" | sort -u | awk '{ print "etc\t" $0 "\t-" }'
true
EOF
}

collect() {
  # shellcheck disable=SC2086
  collector | ssh -o BatchMode=yes ${HOST_DIFF_SSH_OPTS:-} "$1" 'if [ "$(id -u)" = 0 ]; then exec bash -s; else exec sudo -n bash -s; fi'
}

old=$(collect "$OLD")
new=$(collect "$NEW")

lines=$(awk -F'\t' -v OFS='\t' '
  FNR == 1 { fileno++ }
  { key = $1 "\t" $2; seen[key] = 1; if (fileno == 1) a[key] = $3; else b[key] = $3 }
  END {
    for (k in seen) {
      split(k, p, "\t")
      inA = (k in a); inB = (k in b)
      if (inA && inB) {
        if (a[k] == b[k]) continue
        if (p[1] == "unit") print p[1] " " p[2] ": content differs"
        else print p[1] " " p[2] ": " a[k] " -> " b[k]
      } else if (inA) {
        print p[1] " " p[2] ": only on old" (p[1] == "etc" ? "" : " (" a[k] ")")
      } else {
        print p[1] " " p[2] ": only on new" (p[1] == "etc" ? "" : " (" b[k] ")")
      }
    }
  }' <(printf '%s\n' "$old") <(printf '%s\n' "$new") | LC_ALL=C sort)

n=0
if [[ -n $lines ]]; then
  printf '%s\n' "$lines"
  n=$(printf '%s\n' "$lines" | wc -l | tr -d ' ')
fi
echo "$n difference(s)"
((n == 0))
