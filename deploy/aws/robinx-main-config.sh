#!/usr/bin/env bash
# Stages on robinx-main everything robinx-prod and the mnemonic box run, beside production: the arm64 release of the
# commit robinx-prod's live slot runs, laid out as wen's deploy/activate-release.sh lays it out (no activation, no
# migration), the wallet indexer, ffmpeg with libx264, the pinned monitoring binaries, every robinx unit with its
# drop-ins, the helper scripts, the config trees, the sealed credentials re-sealed for this host, the two slices.
# Enables, starts and activates nothing; runs daemon-reload only.
# Run from the Mac: deploy/aws/robinx-main-config.sh [stage|check|proof <dir>]. Idempotent; a rerun prints 0 change(s).
# check is read-only: what a stage rerun would change, as "differs:" lines and "check: <n> difference(s)", then the
# secrets count. Without BUILD it compares the two binaries by the release sha and presence instead of by content.
# Contents cross one boundary: an old host's read or `systemd-creds decrypt` piped straight into robinx-main's writer
# (deploy/aws/box-copy.py recv), which prints paths and rewrite counts, never contents. The old hosts are only read.
# Env: BUILD (a tree holding bin/robinx-linux and bin/wallet-indexer built for arm64 at RELEASE_SHA, see below),
# WEN (a wen checkout, for the commit's alerts.yml and deploy/systemd/robinx-slot and sudoers-robinx; default
# ~/loyal/wen), RELEASE_SHA (default: what robinx-prod's /opt/robinx/current says, which preflight checks),
# OLD, MNEMONIC_OLD, NEW (ssh destinations; default root@5.161.126.23, root@5.78.147.233, robinx-main).
# Build: git archive RELEASE_SHA into an empty dir, `bun install --frozen-lockfile`, then there
# ROBINX_GOARCH=arm64 ROBINX_RELEASE=<sha> sh deploy/build-release.sh and
# (cd server && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o ../bin/wallet-indexer ./cmd/wallet-indexer).
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
OLD=${OLD:-root@5.161.126.23}
MNEMONIC_OLD=${MNEMONIC_OLD:-root@5.78.147.233}
NEW=${NEW:-robinx-main}
WEN=${WEN:-$HOME/loyal/wen}
BUILD=${BUILD:-}
CLOUDFLARED_VERSION=2026.8.3
CLOUDFLARED_SHA256=2c0d991ed6061e55d2e0148b794d86f594ddb473c1db43cf0647278b62e326ab # arm64 deb, fetched once from the release page

die() { echo "robinx-main-config: $*" >&2; exit 1; }
old() { ssh -o BatchMode=yes "$OLD" "$@"; }
mnem() { ssh -o BatchMode=yes "$MNEMONIC_OLD" "$@"; }
new() { ssh -o BatchMode=yes "$NEW" "$@"; }
b64() { base64 | tr -d '\n'; }
j64() { python3 -I -c 'import base64, json, sys; print(base64.b64encode(json.dumps(json.load(sys.stdin)).encode()).decode())'; }
CODE=$(b64 <"$DIR/box-copy.py")
# shellcheck disable=SC2016 # expanded on the remote host
py() { printf 'python3 -I -c "$(echo %s | base64 -d)"' "$CODE"; }
total=0
reload=0

recv() { # recv <params json>: frames on stdin into robinx-main's writer
  local out
  out=$(new "sudo $(py) recv $(j64 <<<"$1")") || die "robinx-main writer failed"
  grep -v '^recv: ' <<<"$out" || true
  total=$((total + $(sed -n 's/^recv: \([0-9]*\) change(s)$/\1/p' <<<"$out")))
  if grep -q '^changed: /etc/systemd/system/' <<<"$out"; then reload=1; fi
}

# Pinned tools from wen's deploy/monitoring/install.sh at RELEASE_SHA: "<org> <name> <version> <arm64 sha256>" lines.
tool_pins() {
  local f v s
  f=$(git -C "$WEN" show "$RELEASE_SHA:deploy/monitoring/install.sh")
  for t in prometheus:prometheus:PROMETHEUS alertmanager:prometheus:ALERTMANAGER node_exporter:prometheus:NODE_EXPORTER \
    postgres_exporter:prometheus-community:POSTGRES_EXPORTER; do
    IFS=: read -r name org key <<<"$t"
    v=$(sed -n "s/^${key}_VERSION=//p" <<<"$f")
    s=$(sed -n "s/^${key}_SHA256_ARM64=//p" <<<"$f")
    [[ -n $v && $s =~ ^[0-9a-f]{64}$ ]] || die "install.sh at $RELEASE_SHA does not pin $name"
    echo "$org $name $v $s"
  done
}

live_release() {
  [[ $(new uname -m) == aarch64 ]] || die "$NEW is not arm64"
  live=$(old 'readlink /opt/robinx/current' | sed -n 's|^/opt/robinx/releases/robinx-\([0-9a-f]\{40\}\)$|\1|p')
  [[ $live =~ ^[0-9a-f]{40}$ ]] || die "robinx-prod has no live release under /opt/robinx/current"
  RELEASE_SHA=${RELEASE_SHA:-$live}
  [[ $RELEASE_SHA == "$live" ]] || die "RELEASE_SHA $RELEASE_SHA is not the live release $live"
  [[ $(old "cat /opt/robinx/releases/robinx-$RELEASE_SHA/REVISION") == "$RELEASE_SHA" ]] || die "live REVISION does not say $RELEASE_SHA"
  git -C "$WEN" cat-file -e "$RELEASE_SHA^{commit}" 2>/dev/null || git -C "$WEN" fetch -q origin
}

build_ok() {
  [[ -n $BUILD ]] || die "BUILD is the tree with bin/robinx-linux and bin/wallet-indexer for linux/arm64"
  for b in robinx-linux wallet-indexer; do
    [[ $(file -b "$BUILD/bin/$b") == *"ARM aarch64"* ]] || die "$BUILD/bin/$b is not linux/arm64"
  done
  [[ $(strings -a "$BUILD/bin/robinx-linux" | grep -c "$RELEASE_SHA") -ge 1 ]] || die "bin/robinx-linux does not carry $RELEASE_SHA"
}

preflight() {
  live_release
  build_ok
  alerts_sha=$(git -C "$WEN" show "$RELEASE_SHA:deploy/monitoring/alerts.yml" | shasum -a 256 | cut -d' ' -f1)
  [[ $(old "sha256sum </opt/robinx/releases/robinx-$RELEASE_SHA/alerts.yml" | cut -d' ' -f1) == "$alerts_sha" ]] ||
    die "the live release's alerts.yml is not the commit's"
  # The root-side helper and its sudoers come from wen main (they carry the migration-freeze refusal).
  git -C "$WEN" fetch -q origin main
  slot_ref=$(git -C "$WEN" rev-parse --short origin/main)
  git -C "$WEN" show origin/main:deploy/systemd/robinx-slot | grep 'FREEZE_MARKER=/etc/pgmove/freeze' >/dev/null || die "wen main's robinx-slot has no freeze refusal"
  git -C "$WEN" show origin/main:deploy/systemd/sudoers-robinx | new "sudo visudo -cf /dev/stdin" >/dev/null || die "sudoers-robinx does not parse on $NEW"
  addrs
}

addrs() {
  # shellcheck disable=SC2016 # expanded on robinx-main
  new_ip=$(new 't=$(curl -fsS -m 5 -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 60" http://169.254.169.254/latest/api/token) && curl -fsS -m 5 -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/public-ipv4')
  mnem_ip=$(mnem "grep -o 'quic_addr *= *\"[0-9.]*:' /etc/mnemonic/server.toml" | grep -o '[0-9.]*[0-9]')
  [[ $new_ip =~ ^[0-9.]+$ && $mnem_ip =~ ^[0-9.]+$ ]] || die "public addresses not found (new '$new_ip', mnemonic '$mnem_ip')"
}

# The host side of everything that is not a copy: packages, pinned tools, accounts. Prints "changed:" lines.
prepare() {
  local pins out
  pins=$(tool_pins)
  out=$(new "sudo CF_VER=$CLOUDFLARED_VERSION CF_SHA=$CLOUDFLARED_SHA256 PINS='$pins' bash -s" <<'REMOTE'
set -euo pipefail
n=0
chg() { echo "changed: $*"; n=$((n + 1)); }
if ! dpkg-query -W ffmpeg >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l apt-get install -y -qq ffmpeg >/dev/null 2>&1
  chg "apt ffmpeg $(dpkg-query -W -f '${Version}' ffmpeg)"
fi
encoders=$(ffmpeg -hide_banner -encoders 2>/dev/null)
grep -q libx264 <<<"$encoders" || { echo "ffmpeg has no libx264" >&2; exit 1; }
if [ "$(dpkg-query -W -f '${Version}' cloudflared 2>/dev/null || true)" != "$CF_VER" ]; then
  t=$(mktemp -d)
  curl -fsSL --retry 3 -o "$t/cf.deb" "https://github.com/cloudflare/cloudflared/releases/download/$CF_VER/cloudflared-linux-arm64.deb"
  echo "$CF_SHA  $t/cf.deb" | sha256sum -c --quiet - || { rm -rf "$t"; echo "cloudflared deb checksum mismatch" >&2; exit 1; }
  dpkg -i "$t/cf.deb" >/dev/null
  rm -rf "$t"
  chg "dpkg cloudflared $CF_VER"
fi
[ "$(readlink /usr/local/bin/cloudflared 2>/dev/null)" = /usr/bin/cloudflared ] || { ln -sfn /usr/bin/cloudflared /usr/local/bin/cloudflared; chg "link /usr/local/bin/cloudflared"; }
while read -r org name ver sum; do
  dir=/opt/$name-$ver
  if [ ! -x "$dir/$name" ]; then
    tarball=$name-$ver.linux-arm64.tar.gz
    t=$(mktemp -d)
    curl -fsSL --retry 3 -o "$t/$tarball" "https://github.com/$org/$name/releases/download/v$ver/$tarball"
    echo "$sum  $t/$tarball" | sha256sum -c --quiet - || { rm -rf "$t"; echo "$tarball checksum mismatch" >&2; exit 1; }
    tar -xzf "$t/$tarball" -C "$t"
    rm -rf "$dir.new"
    mv "$t/$name-$ver.linux-arm64" "$dir.new"
    chown -R root:root "$dir.new"
    mv "$dir.new" "$dir"
    rm -rf "$t"
    chg "tool $name $ver"
  fi
  [ "$(readlink /opt/$name 2>/dev/null)" = "$dir" ] || { ln -sfn "$dir" "/opt/$name"; chg "link /opt/$name"; }
done <<<"$PINS"
sys() { id "$1" >/dev/null 2>&1 || { useradd --system --no-create-home --shell /usr/sbin/nologin "$@"; chg "user $1"; }; }
sys prometheus; sys alertmanager; sys node_exporter; sys postgres_exporter; sys cloudflared
id mnemonic >/dev/null 2>&1 || { useradd --system --home-dir /var/lib/mnemonic --no-create-home --shell /usr/sbin/nologin mnemonic; chg "user mnemonic"; }
id deploy >/dev/null 2>&1 || { useradd --create-home --shell /bin/bash --groups users deploy; chg "user deploy"; }
echo "prepare: $n change(s)"
REMOTE
  ) || die "prepare failed on $NEW"
  grep -v '^prepare: ' <<<"$out" || true
  total=$((total + $(sed -n 's/^prepare: \([0-9]*\) change(s)$/\1/p' <<<"$out")))
}

# Everything that is built or generated here: the release, the helper, the slices and their assignments.
local_spec() { # local_spec <have file>
  local rel=/opt/robinx/releases/robinx-$RELEASE_SHA e=() first=1
  ent() { printf '{"k":"file","src":"%s","dst":"%s","mode":"%s","own":"%s"}' "$1" "$2" "$3" "$4"; }
  gen() { printf '{"k":"file","dst":"%s","mode":"%s","own":"%s","data":"%s"}' "$1" "$2" "$3" "$(printf '%s' "$4" | b64)"; }
  dir() { printf '{"k":"dir","dst":"%s","mode":"%s","own":"%s"}' "$1" "$2" "$3"; }
  link() { printf '{"k":"link","dst":"%s","target":"%s","own":"%s"}' "$1" "$2" "$3"; }
  e+=("$(dir /opt/robinx 0755 deploy:deploy)" "$(dir /opt/robinx/releases 0755 deploy:deploy)" "$(dir "$rel" 0775 deploy:deploy)"
    "$(dir /opt/robinx/slots 0755 deploy:deploy)" "$(dir /opt/robinx/assets 0755 deploy:deploy)"
    "$(dir /opt/robinx/incoming 0755 deploy:deploy)" "$(dir /opt/robinx/deploy 0755 deploy:deploy)"
    "$(gen /opt/robinx/deploy.lock 0664 deploy:deploy '')"
    "$(dir /srv/robinx-data/recovery 0700 postgres:postgres)"
    "{\"k\":\"file\",\"dst\":\"$rel/alerts.yml\",\"mode\":\"0644\",\"own\":\"deploy:deploy\",\"data\":\"$(git -C "$WEN" show "$RELEASE_SHA:deploy/monitoring/alerts.yml" | b64)\"}"
    "$(gen "$rel/REVISION" 0664 deploy:deploy "$RELEASE_SHA
")"
    "$(link /opt/robinx/current "$rel" root:root)" "$(link /opt/robinx/slots/8080 "$rel" deploy:deploy)"
    "{\"k\":\"file\",\"dst\":\"/usr/local/bin/robinx-slot\",\"mode\":\"0755\",\"own\":\"root:root\",\"data\":\"$(git -C "$WEN" show origin/main:deploy/systemd/robinx-slot | b64)\"}"
    "{\"k\":\"file\",\"dst\":\"/etc/sudoers.d/robinx\",\"mode\":\"0440\",\"own\":\"root:root\",\"data\":\"$(git -C "$WEN" show origin/main:deploy/systemd/sudoers-robinx | b64)\"}"
    "$(gen /etc/systemd/system/robinx.slice 0644 root:root '[Unit]
Description=robinX: the app slots, their PostgreSQL clusters and the wallet indexer (CPU and IO priority)

[Slice]
CPUWeight=1000
IOWeight=1000
')"
    "$(gen /etc/systemd/system/mnemonic.slice 0644 root:root '[Unit]
Description=mnemonic: capped so it cannot starve robinX

[Slice]
CPUQuota=200%
MemoryMax=8G
')")
  [[ -z $BUILD ]] || e+=("$(ent "$BUILD/bin/robinx-linux" "$rel/robinx" 0755 deploy:deploy)"
    "$(ent "$BUILD/bin/wallet-indexer" /usr/local/bin/wallet-indexer 0755 root:root)")
  local u
  for u in robinx@.service robinx-wallet-indexer.service postgresql@18-production.service postgresql@18-parsers.service postgresql@18-test.service; do
    e+=("$(dir "/etc/systemd/system/$u.d" 0755 root:root)"
      "$(gen "/etc/systemd/system/$u.d/10-slice.conf" 0644 root:root '[Service]
Slice=robinx.slice
')")
  done
  for u in mnemonic.service cloudflared-mnemonic.service; do
    e+=("$(dir "/etc/systemd/system/$u.d" 0755 root:root)"
      "$(gen "/etc/systemd/system/$u.d/10-slice.conf" 0644 root:root '[Service]
Slice=mnemonic.slice
')")
  done
  printf '{"have":"%s","entries":[' "$1"
  for x in "${e[@]}"; do ((first)) || printf ','; first=0; printf '%s' "$x"; done
  printf ']}'
}

stage_local() {
  local spec have
  have=$(mktemp)
  # shellcheck disable=SC2064
  trap "rm -f '$have'" RETURN
  spec=$(local_spec /dev/null)
  python3 -I -c "import json,sys; [print(e['dst']) for e in json.loads(sys.argv[1])['entries'] if e['k'] == 'file']" "$spec" |
    new "sudo $(py) have" >"$have"
  spec=$(local_spec "$have")
  recv '{}' < <(python3 -I -c "$(cat "$DIR/box-copy.py")" local "$(j64 <<<"$spec")")
}

robinx_spec() {
  cat <<EOF
{"skip": "\\\\.retired-",
 "entries": [
  {"k": "dir", "dst": "/etc/robinx", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/etc/cloudflared", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/etc/prometheus", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/etc/alertmanager", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/etc/postgres_exporter", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/var/lib/prometheus", "mode": "0750", "own": "prometheus:prometheus"},
  {"k": "dir", "dst": "/var/lib/alertmanager", "mode": "0750", "own": "alertmanager:alertmanager"},
  {"k": "dir", "dst": "/var/lib/node_exporter", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/var/lib/node_exporter/textfile", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/usr/local/libexec", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/usr/local/libexec/robinx-monitoring", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/usr/local/libexec/robinx-postgres", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/usr/local/libexec/robinx-postgres/rehearsal", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/usr/local/libexec/robinx-parsers", "mode": "0755", "own": "root:root"},
  {"k": "dir", "dst": "/home/deploy/.ssh", "mode": "0700", "own": "deploy:deploy"},
  {"src": "/home/deploy/.ssh/authorized_keys"},
  {"src": "/etc/systemd/system/robinx@.service"},
  {"src": "/etc/systemd/system/robinx-alert.service"},
  {"src": "/etc/systemd/system/robinx-wallet-indexer.service"},
  {"src": "/etc/systemd/system/robinx-textfile.service"}, {"src": "/etc/systemd/system/robinx-textfile.timer"},
  {"src": "/etc/systemd/system/robinx-trim-data.service"}, {"src": "/etc/systemd/system/robinx-trim-data.timer"},
  {"src": "/etc/systemd/system/cloudflared.service"},
  {"src": "/etc/systemd/system/prometheus.service"}, {"src": "/etc/systemd/system/alertmanager.service"},
  {"src": "/etc/systemd/system/node_exporter.service"}, {"src": "/etc/systemd/system/postgres_exporter@.service"},
  {"src": "/usr/local/libexec/robinx-monitoring/robinx-textfile.sh"},
  {"src": "/usr/local/libexec/robinx-postgres/mount-guard.sh"}, {"src": "/usr/local/libexec/robinx-postgres/trim-data.sh"},
  {"src": "/usr/local/libexec/robinx-postgres/rehearsal/lockwatch.sh"},
  {"src": "/usr/local/libexec/robinx-parsers/cap-tune.sh"}, {"src": "/usr/local/libexec/robinx-parsers/prod-gate.sh"},
  {"src": "/opt/robinx/deploy/notify.sh", "dst": "/opt/robinx/deploy/notify.sh"},
  {"src": "/etc/robinx/env"},
  {"src": "/etc/cloudflared/config.yml"}, {"src": "/etc/cloudflared/robinx-prod.json"},
  {"src": "/etc/prometheus/prometheus.yml"},
  {"k": "link", "dst": "/etc/prometheus/alerts.yml", "target": "/opt/robinx/current/alerts.yml", "own": "root:root"},
  {"src": "/etc/alertmanager/alertmanager.yml"}, {"src": "/etc/alertmanager/telegram.tmpl"},
  {"src": "/etc/alertmanager/telegram_token"},
  {"src": "/etc/postgres_exporter/queries.yml"}, {"src": "/etc/postgres_exporter/production.env"},
  {"src": "/etc/postgres_exporter/parsers.env"},
  {"k": "creds", "only": "^(robinx-app-db|robinx-migration-age|robinx-pg-verify-(production|parsers))\$",
   "ids": {"robinx-pg-verify-production": "pg-verify", "robinx-pg-verify-parsers": "pg-verify"}}
 ]}
EOF
}
ROBINX_PARAMS='{"args": {"old_pg": "robinx-postgres.service", "new_pg": "postgresql@18-production.service"},
 "rules": {"/etc/systemd/system/robinx@.service": ["sub:pg"]}}'

stage_robinx() { recv "$ROBINX_PARAMS" < <(old "$(py) send $(robinx_spec | j64)"); }

mnemonic_spec() {
  cat <<EOF
{"entries": [
  {"k": "dir", "dst": "/etc/mnemonic", "mode": "0750", "own": "root:mnemonic"},
  {"src": "/etc/systemd/system/mnemonic.service"}, {"src": "/etc/systemd/system/cloudflared-mnemonic.service"},
  {"src": "/etc/mnemonic/server.toml"},
  {"src": "/etc/cloudflared/config.yml", "dst": "/etc/cloudflared/mnemonic.yml"},
  {"src": "/etc/cloudflared/mnemonic-share.json"}
 ]}
EOF
}

mnemonic_params() {
  echo "{\"args\": {\"old_quic\": \"$mnem_ip:8741\", \"new_quic\": \"$new_ip:8741\",
          \"old_cfg\": \"--config /etc/cloudflared/config.yml\", \"new_cfg\": \"--config /etc/cloudflared/mnemonic.yml\"},
 \"rules\": {\"/etc/mnemonic/server.toml\": [\"sub:quic\"], \"/etc/systemd/system/cloudflared-mnemonic.service\": [\"sub:cfg\"]}}"
}

stage_mnemonic() { recv "$(mnemonic_params)" < <(mnem "$(py) send $(mnemonic_spec | j64)"); }

stage() {
  preflight
  echo "release $RELEASE_SHA (robinx-prod's live slot; robinx-slot and sudoers from wen main $slot_ref)"
  prepare
  stage_local
  stage_robinx
  stage_mnemonic
  ((reload == 0)) || new "sudo systemctl daemon-reload"
  [[ -z $(new "systemctl list-unit-files 'robinx*' 'mnemonic*' 'cloudflared*' 'prometheus*' 'alertmanager*' 'node_exporter*' 'postgres_exporter*' --state=enabled --no-legend") ]] ||
    die "a robinx, mnemonic or monitoring unit is enabled on $NEW"
  echo "$NEW: $total change(s)"
}

# Secret names with the sha256 of each value on both sides; a line that differs is a secret that did not cross.
SUMS_ROBINX='{"env": ["/etc/robinx/env"], "files": ["/etc/cloudflared/robinx-prod.json", "/etc/alertmanager/telegram_token"],
 "creds": {"only": "^(robinx-app-db|robinx-migration-age|robinx-pg-verify-(production|parsers))$",
           "ids": {"robinx-pg-verify-production": "pg-verify", "robinx-pg-verify-parsers": "pg-verify"}}}'
SUMS_MNEMONIC='{"files": ["/etc/cloudflared/mnemonic-share.json"], "lines": [["/etc/mnemonic/server.toml", "^key[ ]*="]]}'

classify() { # classify <tsv> <host-diff output>...: every difference line gets the first matching class and owner; prints the rest
  awk -F'\t' '
    FNR == NR { if ($0 !~ /^#/ && NF == 4) { n++; cls[n] = $1; own[n] = $2; rx[n] = $3 } next }
    $0 !~ /^(pkg|unit|etc) / { next }
    { hit = 0; for (i = 1; i <= n; i++) if ($0 ~ rx[i]) { cnt[cls[i] "\t" own[i]]++; hit = 1; break }
      if (!hit) { print "unclassified: " $0; un++ } total++ }
    END { for (k in cnt) { split(k, a, "\t"); printf "%-16s %-28s %d\n", a[1], a[2], cnt[k] }
          printf "%d difference line(s), %d unclassified\n", total, un + 0 }' "$1" "${@:2}" | LC_ALL=C sort
}

host_diffs() { # host_diffs <dir>: host-diff.sh after staging, robinx-prod and the mnemonic box against robinx-main, classified
  local p=$1 sha=$RELEASE_SHA
  export HOST_DIFF_PKGS='^(postgresql.*|libpq5|pgbackrest|ffmpeg|libx264.*|cloudflared|jq|curl|git|rsync|nftables|iptables|openssl|python3|python3[.]12|bash|ca-certificates|systemd|libc6|zstd|xfsprogs|sqlite3|fio|wireguard-tools)$'
  export HOST_DIFF_UNITS='^(robinx|mnemonic|cloudflared|prometheus|alertmanager|node_exporter|postgres_exporter|postgresql|pg-forward|captune|lockwatch)'
  export HOST_DIFF_TREES="/etc/robinx /etc/robinx-postgres /etc/robinx-parsers /etc/credstore.encrypted /etc/cloudflared /etc/prometheus /etc/alertmanager /etc/postgres_exporter /etc/mnemonic /etc/sudoers.d/robinx /etc/postgresql /etc/pgbackrest.conf /usr/local/bin /usr/local/libexec /opt/robinx/current /opt/robinx/slots /opt/robinx/deploy /opt/robinx/deploy.lock /opt/robinx/releases/robinx-$sha /opt/prometheus /opt/alertmanager /opt/node_exporter /opt/postgres_exporter /home/deploy/.ssh"
  HOST_DIFF_SSH_OPTS=-oBatchMode=yes "$DIR/host-diff.sh" "$OLD" "$NEW" >"$p/host-diff-after-robinx-prod.txt" || true
  HOST_DIFF_PKGS='^cloudflared$' HOST_DIFF_UNITS='^(mnemonic|cloudflared)' HOST_DIFF_TREES='/etc/mnemonic /etc/cloudflared' \
    HOST_DIFF_SSH_OPTS=-oBatchMode=yes "$DIR/host-diff.sh" "$MNEMONIC_OLD" "$NEW" >"$p/host-diff-after-mnemonic.txt" || true
  { echo "# class, owning unit, count; rules in boxes/robinx-main.host-diff.tsv"
    classify "$DIR/boxes/robinx-main.host-diff.tsv" "$p/host-diff-after-robinx-prod.txt" "$p/host-diff-after-mnemonic.txt"; } >"$p/classification.txt"
}

secrets() { # secrets <dir>: secret names and sha256s, old against new, into <dir>/secrets-{old,new}.txt and secrets.txt
  local p=$1 a b
  old "$(py) sums $(j64 <<<"$SUMS_ROBINX")" >"$p/secrets-old.txt"
  new "sudo $(py) sums $(j64 <<<"$SUMS_ROBINX")" >"$p/secrets-new.txt" || true
  mnem "$(py) sums $(j64 <<<"$SUMS_MNEMONIC")" >>"$p/secrets-old.txt"
  new "sudo $(py) sums $(j64 <<<"$SUMS_MNEMONIC")" >>"$p/secrets-new.txt" || true
  a=$(wc -l <"$p/secrets-old.txt" | tr -d ' ')
  # shellcheck disable=SC2126 # a count of zero must not fail the pipeline
  b=$(comm -12 <(LC_ALL=C sort "$p/secrets-old.txt") <(LC_ALL=C sort "$p/secrets-new.txt") | grep -v -E ' (MISSING|UNDECRYPTABLE)$' | wc -l | tr -d ' ')
  echo "$b of $a secret names present with matching sha256" >"$p/secrets.txt"
  comm -23 <(LC_ALL=C sort "$p/secrets-old.txt") <(LC_ALL=C sort "$p/secrets-new.txt") | cut -d' ' -f1 | sed 's/^/  not matching: /' >>"$p/secrets.txt"
}

proof() { # proof <dir>: the evidence files, names, hashes and exit codes only
  local p=$1 units u
  mkdir -p "$p"
  RELEASE_SHA=${RELEASE_SHA:-$(old 'readlink /opt/robinx/current' | sed -n 's|^/opt/robinx/releases/robinx-\([0-9a-f]\{40\}\)$|\1|p')}
  secrets "$p"
  # unit checks
  units=$(new "cd /etc/systemd/system && ls -d robinx*.service robinx*.timer robinx*.slice mnemonic*.service mnemonic.slice cloudflared*.service cloudflared-mnemonic.service prometheus.service alertmanager.service node_exporter.service postgres_exporter@.service 2>/dev/null | sort -u" |
    sed -E 's/^robinx@\.service$/robinx@8080.service/; s/^postgres_exporter@\.service$/postgres_exporter@production.service/')
  for u in $units; do
    new "out=\$(sudo systemd-analyze verify $u 2>&1); echo \"$u rc=\$? \$(tr '\n' ' ' <<<\"\$out\")\""
  done >"$p/verify.txt"
  new "echo '# slices as loaded:'; for s in robinx.slice mnemonic.slice; do systemctl show \$s -p CPUWeight -p IOWeight -p CPUQuotaPerSecUSec -p MemoryMax --no-pager | tr '\n' ' '; echo; done
    for u in robinx@8080 robinx-wallet-indexer postgresql@18-production postgresql@18-parsers postgresql@18-test mnemonic cloudflared-mnemonic; do echo \"\$u slice=\$(systemctl show \$u -p Slice --value) active=\$(systemctl is-active \$u)\"; done" >"$p/slices.txt"
  # (c) nothing enabled, nothing listening that was not
  new "echo '# enabled robinx*|mnemonic*|monitoring unit files:'
    systemctl list-unit-files 'robinx*' 'mnemonic*' 'cloudflared*' 'prometheus*' 'alertmanager*' 'node_exporter*' 'postgres_exporter*' --state=enabled --no-legend
    echo '# active (robinx*|mnemonic*|monitoring):'
    systemctl list-units 'robinx*' 'mnemonic*' 'cloudflared*' 'prometheus*' 'alertmanager*' 'node_exporter*' 'postgres_exporter*' --all --state=active --no-legend
    echo '# listening:'; sudo ss -lnt" >"$p/enabled.txt"
  new "echo '# ffmpeg:'; ffmpeg -version | sed -n 1p; ffmpeg -hide_banner -encoders 2>/dev/null | grep libx264
    for b in /opt/robinx/releases/robinx-$RELEASE_SHA/robinx /usr/local/bin/wallet-indexer /usr/bin/cloudflared /opt/prometheus/prometheus /opt/prometheus/promtool /opt/alertmanager/alertmanager /opt/node_exporter/node_exporter /opt/postgres_exporter/postgres_exporter; do
      case \$b in *wallet-indexer) a=--help ;; *cloudflared) a=--version ;; *robinx) a=--help ;; *) a=--version ;; esac
      out=\$(sudo unshare -n setpriv --reuid=nobody --regid=nogroup --clear-groups timeout 10 \$b \$a 2>&1); rc=\$?
      echo \"\$b \$a rc=\$rc \$(head -1 <<<\"\$out\" | cut -c1-110)\"; done
    echo '# release layout:'; sudo stat -c '%A %U:%G %n' /opt/robinx /opt/robinx/current /opt/robinx/slots/8080 /opt/robinx/releases/robinx-$RELEASE_SHA/*; sudo cat /opt/robinx/releases/robinx-$RELEASE_SHA/REVISION
    sudo sha256sum /opt/robinx/releases/robinx-$RELEASE_SHA/robinx /opt/robinx/releases/robinx-$RELEASE_SHA/alerts.yml /usr/local/bin/wallet-indexer /usr/local/bin/robinx-slot
    echo '# authorized_keys (type comment):'; sudo awk '{print \$1, (NF > 2 ? \$3 : \"(no comment)\")}' /home/deploy/.ssh/authorized_keys" >"$p/binaries.txt" 2>&1 || true
  host_diffs "$p"
}

# One line per frame a sender emits, as recv would leave it: "file sha mode owner dst", "dir mode owner dst",
# "link target owner dst". Credentials are left to secrets. Runs box-copy.py's own rewrite.
MANIFEST='import hashlib, json, sys
src = open(sys.argv[1]).read()
exec(src[:src.index("\nmode = sys.argv")])
params = json.loads(sys.argv[2])
inp = sys.stdin.buffer
while True:
    line = inp.readline()
    line or die("the sender stopped before its end frame")
    h = json.loads(line)
    if h["k"] == "end":
        break
    data = inp.read(h["size"])
    if h["k"] == "dir":
        print("dir", h["mode"], h["own"], h["dst"])
    elif h["k"] == "link":
        print("link", data.decode(), h["own"], h["dst"])
    elif h["k"] == "file":
        data, _ = rewrite(h["dst"], data, params.get("rules", {}).get(h["dst"], []), params.get("args", {}))
        print("file", hashlib.sha256(data).hexdigest(), h["mode"], h["own"], h["dst"])'
# The same lines for what robinx-main holds now, for "kind dst" lines on stdin.
STATE='import grp, hashlib, os, pwd, stat, sys
def own(st):
    return pwd.getpwuid(st.st_uid).pw_name + ":" + grp.getgrgid(st.st_gid).gr_name
for line in sys.stdin:
    k, dst = line.split()
    if k == "dir" and os.path.isdir(dst) and not os.path.islink(dst):
        st = os.lstat(dst)
        print("dir", "%04o" % stat.S_IMODE(st.st_mode), own(st), dst)
    elif k == "link" and os.path.islink(dst):
        print("link", os.readlink(dst), own(os.lstat(dst)), dst)
    elif k == "file" and os.path.isfile(dst) and not os.path.islink(dst):
        st = os.lstat(dst)
        print("file", hashlib.sha256(open(dst, "rb").read()).hexdigest(), "%04o" % stat.S_IMODE(st.st_mode), own(st), dst)'

manifest() { python3 -I -c "$MANIFEST" "$DIR/box-copy.py" "$1"; }

prepare_check() { # what prepare would change, as "differs:" lines
  local pins
  pins=$(tool_pins)
  # shellcheck disable=SC2016 # expanded on robinx-main
  new "CF_VER=$CLOUDFLARED_VERSION PINS='$pins' bash -s" <<'REMOTE'
{ dpkg-query -W ffmpeg >/dev/null 2>&1 && ffmpeg -hide_banner -encoders 2>/dev/null | grep libx264 >/dev/null; } || echo "differs: ffmpeg with libx264"
[ "$(dpkg-query -W -f '${Version}' cloudflared 2>/dev/null)" = "$CF_VER" ] || echo "differs: cloudflared $CF_VER"
[ "$(readlink /usr/local/bin/cloudflared)" = /usr/bin/cloudflared ] || echo "differs: link /usr/local/bin/cloudflared"
while read -r _ name ver _; do
  [ -x "/opt/$name-$ver/$name" ] || echo "differs: tool $name $ver"
  [ "$(readlink "/opt/$name")" = "/opt/$name-$ver" ] || echo "differs: link /opt/$name"
done <<<"$PINS"
for u in prometheus alertmanager node_exporter postgres_exporter cloudflared mnemonic deploy; do id "$u" >/dev/null 2>&1 || echo "differs: user $u"; done
REMOTE
}

check() {
  local want got n rel staged t
  live_release
  git -C "$WEN" fetch -q origin main
  addrs
  rel=/opt/robinx/releases/robinx-$RELEASE_SHA
  want=$({ python3 -I -c "$(cat "$DIR/box-copy.py")" local "$(local_spec /dev/null | j64)" | manifest '{}'
    old "$(py) send $(robinx_spec | j64)" | manifest "$ROBINX_PARAMS"
    mnem "$(py) send $(mnemonic_spec | j64)" | manifest "$(mnemonic_params)"; } | LC_ALL=C sort -u)
  got=$(awk '{ print $1, $NF }' <<<"$want" | new "sudo python3 -I -c \"\$(echo $(b64 <<<"$STATE") | base64 -d)\"" | LC_ALL=C sort -u)
  t=$(mktemp -d)
  { comm -23 <(echo "$want") <(echo "$got") | awk '{ print "differs: " $1 " " $NF }'
    prepare_check
    if [[ -z $BUILD ]]; then
      new "sudo grep -c -a $RELEASE_SHA $rel/robinx >/dev/null" || echo "differs: $rel/robinx does not carry $RELEASE_SHA"
      new "test -x /usr/local/bin/wallet-indexer" || echo "differs: /usr/local/bin/wallet-indexer missing"
    fi; } >"$t/differs"
  cat "$t/differs"
  n=$(grep -c '^differs: ' "$t/differs" || true)
  staged=$(new "cat /opt/robinx/current/REVISION 2>/dev/null" || true)
  echo "release: robinx-main stages ${staged:-nothing}, robinx-prod runs $RELEASE_SHA$([[ -n $BUILD ]] || echo '; binaries by release sha and presence, BUILD unset')"
  echo "check: $n difference(s)"
  secrets "$t"
  echo "secrets: $(head -1 "$t/secrets.txt")"
  tail -n +2 "$t/secrets.txt"
  [[ $n == 0 && $staged == "$RELEASE_SHA" ]] && ! grep -q 'not matching' "$t/secrets.txt"
}

case ${1:-stage} in
  stage) stage ;;
  check) check ;;
  proof) proof "${2:?proof <dir>}" ;;
  *) echo "usage: $0 [stage|check|proof <dir>]" >&2; exit 2 ;;
esac
