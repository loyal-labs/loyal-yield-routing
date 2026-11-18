#!/usr/bin/env bash
# Stages on loyal-main everything loyal-workers and loyal-db-primary run for Loyal: the arm64 release laid out as
# scripts/deploy-workers.sh lays it out, the unit files, drop-ins, timers and config trees, and every sealed
# credential re-sealed for loyal-main's host key. Enables, starts and activates nothing; runs daemon-reload only.
# Run from the Mac: deploy/aws/loyal-main-config.sh [stage|proof <dir>]. Idempotent; a rerun prints 0 change(s).
# Contents cross one boundary: an old host's read or `systemd-creds decrypt` piped straight into loyal-main's
# writer (deploy/aws/box-copy.py recv), which prints paths and rewrite counts, never contents.
# The old hosts are only read. Env: RELEASE_SHA, ARTIFACTS (gh run download of its Production rollout run),
# LAUNCHER (arm64 loyal-env-launcher built from the reviewed source), NEW (default loyal-main).
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$DIR/../.." && pwd)
# shellcheck source=/dev/null
source "$REPO/deploy/hetzner/db/remote.sh"

NEW=${NEW:-loyal-main}
RELEASE_SHA=${RELEASE_SHA:-249031cd970f23791e354daf294c66a640871f4f}
ARTIFACTS=${ARTIFACTS:?ARTIFACTS is the gh run download directory of RELEASE_SHA}
LAUNCHER=${LAUNCHER:?LAUNCHER is the arm64 loyal-env-launcher}
STAGED=/opt/loyal-db-staged
# family binary artifact-dir
FAMILIES=(
  "backyard loyal-engine workers" "earn-max loyal-engine workers" "retail loyal-engine workers"
  "observer loyal-observer workers" "telegram loyal-telegram workers" "web loyal-web web-arm64"
  "admin loyal-admin admin-arm64" "stats loyal-stats stats-arm64"
)
SERVICE_SOURCE=(go web crates migrations src Cargo.toml Cargo.lock bun.lock package.json Dockerfile.light-workers
  rust-toolchain.toml tools deploy/hetzner/activate.sh deploy/hetzner/activate-realtime.sh
  deploy/hetzner/retain-web-assets.sh deploy/hetzner/systemd deploy/hetzner/monitoring)

die() { echo "loyal-main-config: $*" >&2; exit 1; }
old() { ssh -o BatchMode=yes loyal-workers "$@"; }
new() { ssh -o BatchMode=yes "$NEW" "$@"; }
b64() { base64 | tr -d '\n'; }
j64() { python3 -I -c 'import base64, json, sys; print(base64.b64encode(json.dumps(json.load(sys.stdin)).encode()).decode())'; }
CODE=$(b64 <"$DIR/box-copy.py")
# shellcheck disable=SC2016 # expanded on the remote host
py() { printf 'python3 -I -c "$(echo %s | base64 -d)"' "$CODE"; }
total=0

recv() { # recv <params json>: frames on stdin into loyal-main's writer
  local out
  out=$(new "sudo $(py) recv $(j64 <<<"$1")") || die "loyal-main writer failed"
  grep -v '^recv: ' <<<"$out" || true
  total=$((total + $(sed -n 's/^recv: \([0-9]*\) change(s)$/\1/p' <<<"$out")))
  grep -q '^changed: /etc/systemd/system/' <<<"$out" && reload=1 || true
}

preflight() {
  [[ $(new uname -m) == aarch64 ]] || die "$NEW is not arm64"
  running=$(old "sed -n 's|^ExecStart=/opt/loyal/releases/[a-z-]*/\([0-9a-f]\{40\}\)/.*|\1|p' /etc/systemd/system/loyal-*.service.d/40-verified-release.conf" | sort -u)
  [[ $running =~ ^[0-9a-f]{40}$ ]] || die "loyal-workers families run more than one release: $running"
  for c in "$running" "$RELEASE_SHA"; do
    git -C "$REPO" cat-file -e "$c^{commit}" 2>/dev/null || git -C "$REPO" fetch -q origin main
  done
  git -C "$REPO" diff --quiet "$running" "$RELEASE_SHA" -- "${SERVICE_SOURCE[@]}" ||
    die "service source differs between the running $running and $RELEASE_SHA; stage the running commit instead"
  for d in workers web-arm64 admin-arm64 stats-arm64 realtime-arm64; do
    (cd "$ARTIFACTS/$d/arm64" && shasum -a 256 -c --quiet ./*-SHA256SUMS) || die "$d artifacts fail their checksums"
  done
  launcher_sha=$(shasum -a 256 "$LAUNCHER" | cut -d' ' -f1)
  old_launcher=$(old "ls /opt/loyal-hetzner" | sed -n 's/^env-launcher-\([0-9a-f]\{64\}\)$/\1/p')
  [[ $old_launcher =~ ^[0-9a-f]{64}$ ]] || die "loyal-workers has no single /opt/loyal-hetzner/env-launcher-<sha256>"
  old_ip=$(old "ip -4 -o addr show scope global" | awk '{ sub("/.*", "", $4); print $4 }' |
    grep -v -E '^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)')
  # shellcheck disable=SC2016 # expanded on loyal-main
  new_ip=$(new 't=$(curl -fsS -m 5 -X PUT -H "X-aws-ec2-metadata-token-ttl-seconds: 60" http://169.254.169.254/latest/api/token) && curl -fsS -m 5 -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/public-ipv4')
  [[ $old_ip =~ ^[0-9.]+$ && $new_ip =~ ^[0-9.]+$ ]] || die "public addresses not found (old '$old_ip', new '$new_ip')"
  # The release drop-ins are generated, not copied: prove the old ones are exactly that format for the running sha.
  local fam bin _ want have
  for f in "${FAMILIES[@]}"; do
    read -r fam bin _ <<<"$f"
    want=$(printf '[Service]\nExecStart=\nExecStart=/opt/loyal/releases/%s/%s/%s\n' "$fam" "$running" "$bin" | shasum -a 256 | cut -d' ' -f1)
    have=$(old "sha256sum </etc/systemd/system/loyal-$fam.service.d/40-verified-release.conf" | cut -d' ' -f1)
    [[ $want == "$have" ]] || die "loyal-$fam 40-verified-release.conf is not the activate.sh format"
  done
}

release_spec() {
  local fam bin art dir e=() first=1
  # shellcheck disable=SC2059 # the drop-in text carries printf escapes
  dropin() { printf '{"k":"file","dst":"/etc/systemd/system/%s/40-verified-release.conf","mode":"0644","own":"root:root","data":"%s"}' "$1" "$(printf "$2" | b64)"; }
  ent() { printf '{"k":"file","src":"%s","dst":"%s","mode":"%s","own":"root:root"}' "$1" "$2" "$3"; }
  for f in "${FAMILIES[@]}"; do
    read -r fam bin art <<<"$f"
    dir=/opt/loyal/releases/$fam/$RELEASE_SHA
    e+=("{\"k\":\"dir\",\"dst\":\"/opt/loyal/releases/$fam\",\"mode\":\"0755\",\"own\":\"root:root\"}"
      "{\"k\":\"dir\",\"dst\":\"$dir\",\"mode\":\"0755\",\"own\":\"root:root\"}"
      "$(ent "$ARTIFACTS/$art/arm64/$bin" "$dir/$bin" 0555)"
      "{\"k\":\"file\",\"dst\":\"$dir/activate.sh\",\"mode\":\"0555\",\"own\":\"root:root\",\"data\":\"$(git -C "$REPO" show "$RELEASE_SHA:deploy/hetzner/activate.sh" | b64)\"}"
      "$(dropin "loyal-$fam.service.d" "[Service]\nExecStart=\nExecStart=$dir/$bin\n")")
    if [[ $fam == web ]]; then
      e+=("{\"k\":\"tree\",\"src\":\"$ARTIFACTS/web-arm64/arm64/web-assets\",\"dst\":\"$dir/assets\",\"dmode\":\"0755\",\"fmode\":\"0644\",\"own\":\"root:root\"}"
        "{\"k\":\"file\",\"dst\":\"$dir/retain-web-assets.sh\",\"mode\":\"0644\",\"own\":\"root:root\",\"data\":\"$(git -C "$REPO" show "$RELEASE_SHA:deploy/hetzner/retain-web-assets.sh" | b64)\"}"
        "$(dropin "loyal-web-cron@.service.d" "[Service]\nExecStart=\nExecStart=$dir/$bin cron %%i\n")")
    fi
  done
  dir=/opt/loyal/releases/realtime/$RELEASE_SHA
  e+=("{\"k\":\"dir\",\"dst\":\"/opt/loyal/releases/realtime\",\"mode\":\"0755\",\"own\":\"root:root\"}"
    "{\"k\":\"dir\",\"dst\":\"$dir\",\"mode\":\"0755\",\"own\":\"root:root\"}"
    "$(ent "$ARTIFACTS/realtime-arm64/arm64/loyal-yield-realtime" "$dir/loyal-yield-realtime" 0555)"
    "$(ent "$ARTIFACTS/realtime-arm64/arm64/loyal-yield-realtime" /opt/loyal/bin/loyal-yield-realtime 0555)"
    "$(dropin loyal-realtime.service.d "[Service]\nEnvironment=LOYAL_IMAGE_VERSION=sha-$RELEASE_SHA\n")"
    "$(ent "$LAUNCHER" /opt/loyal/bin/loyal-env-launcher 0555)"
    "$(ent "$LAUNCHER" "/opt/loyal-hetzner/env-launcher-$launcher_sha" 0755)")
  printf '{"have":"%s","entries":[' "$1"
  for x in "${e[@]}"; do ((first)) || printf ','; first=0; printf '%s' "$x"; done
  printf ']}'
}

stage_release() {
  local spec have
  have=$(mktemp)
  # shellcheck disable=SC2064
  trap "rm -f '$have'" RETURN
  spec=$(release_spec /dev/null)
  python3 -I -c "import json,sys; [print(e['dst']) for e in json.loads(sys.argv[1])['entries'] if e['k'] == 'file']" "$spec" |
    new "sudo $(py) have" >"$have"
  find "$ARTIFACTS/web-arm64/arm64/web-assets" -type f | sed "s|^$ARTIFACTS/web-arm64/arm64/web-assets|/opt/loyal/releases/web/$RELEASE_SHA/assets|" |
    new "sudo $(py) have" >>"$have"
  spec=$(release_spec "$have")
  recv '{}' < <(python3 -I -c "$(cat "$DIR/box-copy.py")" local "$(j64 <<<"$spec")")
}

stage_workers() {
  local spec params ids cs_new relay_new cs_old relay_old image_args='' cs_rules='' relay_rules=''
  # The two docker images are built natively on loyal-main (deploy/aws/loyal-images.sh), which records their ids; the
  # amd64 ids in the old units would not run here.
  ids=$(new "cat /opt/loyal/ops/arm64-images 2>/dev/null || true")
  cs_new=$(sed -n 's/^clickstack=\(sha256:[0-9a-f]\{64\}\)$/\1/p' <<<"$ids")
  relay_new=$(sed -n 's/^relay=\(sha256:[0-9a-f]\{64\}\)$/\1/p' <<<"$ids")
  if [[ -n $cs_new && -n $relay_new ]]; then
    cs_old=$(old "grep -o -E ' sha256:[0-9a-f]{64} --service' /etc/systemd/system/loyal-clickstack.service" | awk '{ print $1 }')
    relay_old=$(old "grep -o -E ' sha256:[0-9a-f]{64} --service' /etc/systemd/system/loyal-clickstack-relay.service" | awk '{ print $1 }')
    [[ $cs_old =~ ^sha256:[0-9a-f]{64}$ && $relay_old =~ ^sha256:[0-9a-f]{64}$ ]] || die "old clickstack units name no single image id"
    image_args=", \"old_cs_image\": \"$cs_old\", \"new_cs_image\": \"$cs_new\", \"old_relay_image\": \"$relay_old\", \"new_relay_image\": \"$relay_new\""
    cs_rules=', "sub:cs_image"' relay_rules=', "sub:relay_image"'
  else
    echo "warning: /opt/loyal/ops/arm64-images missing on $NEW; the clickstack units keep the amd64 image ids (run deploy/aws/loyal-images.sh build, then stage again)" >&2
  fi
  spec=$(cat <<EOF
{"skip": "/40-verified-release\\\\.conf$|/loyal-ci-wireguard\\\\.cred$",
 "entries": [
  {"k": "glob", "src": "/etc/systemd/system/loyal-*"},
  {"src": "/etc/systemd/system/cloudflared.service"},
  {"src": "/etc/systemd/system/otelcol-contrib.service.d"},
  {"src": "/etc/systemd/system/prometheus-alertmanager.service.d"},
  {"src": "/etc/prometheus/prometheus.yml"}, {"src": "/etc/prometheus/alertmanager.yml"},
  {"src": "/etc/prometheus/loyal.rules.yml"}, {"src": "/etc/prometheus/loyal.rules.yml.sha"},
  {"src": "/etc/otelcol-contrib/loyal.yaml"}, {"src": "/etc/otelcol-contrib/config.yaml"},
  {"src": "/etc/otelcol-contrib/otelcol-contrib.conf"},
  {"src": "/etc/cloudflared/config.yml"},
  {"src": "/etc/caddy/Caddyfile"},
  {"src": "/etc/loyal"}, {"src": "/etc/loyal-clickstack"},
  {"k": "creds"},
  {"k": "seal", "src": "/etc/cloudflared/loyal-workers.json", "name": "loyal-workers.json",
   "dst": "/etc/credstore.encrypted/cloudflared/loyal-workers.json"}
 ]}
EOF
)
  params=$(cat <<EOF
{"args": {"old_pubip": "$old_ip", "new_pubip": "$new_ip",
          "old_launcher": "/opt/loyal-hetzner/env-launcher-$old_launcher",
          "new_launcher": "/opt/loyal-hetzner/env-launcher-$launcher_sha"$image_args},
 "cred_rules": ["db"],
 "rules": {"/etc/prometheus/prometheus.yml": ["promdb"],
           "/etc/caddy/Caddyfile": ["pubip"],
           "/etc/systemd/system/loyal-admin.service.d/60-env.conf": ["pubip"],
           "/etc/systemd/system/loyal-clickstack.service": ["pubip", "launcher"$cs_rules],
           "/etc/systemd/system/loyal-clickstack-relay.service": ["launcher"$relay_rules]}}
EOF
)
  recv "$params" < <(old "sudo $(py) send $(j64 <<<"$spec")")
}

stage_db() {
  local spec
  spec=$(cat <<EOF
{"entries": [
  {"k": "glob", "src": "/etc/systemd/system/loyal-*"},
  {"src": "/etc/systemd/system/postgresql@18-main.service.d", "dst": "$STAGED/etc/systemd/system/postgresql@18-main.service.d"},
  {"src": "/etc/postgresql/18/main", "dst": "$STAGED/etc/postgresql/18/main"},
  {"src": "/etc/pgbouncer/loyal.ini"}, {"src": "/etc/pgbouncer/loyal-users.txt"},
  {"src": "/etc/pgbackrest/pgbackrest.conf"}, {"src": "/etc/pgbackrest.conf"},
  {"src": "/etc/loyal-db/replica"}, {"src": "/etc/loyal-db/tls"},
  {"src": "/etc/loyal-observer-monitor"}, {"src": "/etc/loyal-squads-watch"},
  {"src": "/opt/loyal-observer-monitor"}, {"src": "/opt/loyal-squads-watch-recovery-v2"},
  {"k": "glob", "src": "/usr/local/sbin/loyal-db-*"},
  {"k": "creds"}
 ]}
EOF
)
  recv "{\"rules\": {\"/etc/pgbouncer/loyal.ini\": [\"listen\"], \"$STAGED/etc/postgresql/18/main/conf.d/loyal.conf\": [\"listen\"]}, \"cred_rules\": [\"db\"]}" < <(box "$(py) send $(j64 <<<"$spec")")
}

stage() {
  reload=0
  preflight
  echo "release $RELEASE_SHA (loyal-workers runs $running; service source identical)"
  new "sudo install -d -m 0700 -o root -g root $STAGED"
  stage_workers
  stage_db
  stage_release
  ((reload == 0)) || new "sudo systemctl daemon-reload"
  [[ -z $(new "systemctl list-unit-files 'loyal-*' --state=enabled --no-legend") ]] || die "a loyal-* unit is enabled on $NEW"
  echo "$NEW: $total change(s)"
}

proof() { # proof <dir>: the evidence files, names and exit codes only
  local p=$1 units u
  mkdir -p "$p"
  new "sudo $(py) creds e30=" >"$p/creds-new.txt" || true
  old "sudo $(py) creds $(j64 <<<'{"skip": "loyal-ci-wireguard\\.cred$"}')" >"$p/creds-old-workers.txt" || true
  box "$(py) creds e30=" >"$p/creds-old-db.txt" || true
  units=$(new "cd /etc/systemd/system && ls -d loyal-*.service loyal-*.timer cloudflared.service" |
    sed -E 's/@\.service$/@probe.service/')
  for u in $units otelcol-contrib.service prometheus-alertmanager.service; do
    new "out=\$(sudo systemd-analyze verify $u 2>&1); echo \"$u rc=\$? \$(tr '\n' ' ' <<<\"\$out\")\""
  done >"$p/verify.txt"
  new "echo '# enabled loyal-* unit files:'; systemctl list-unit-files 'loyal-*' --state=enabled --no-legend;
    echo '# active loyal-* units:'; systemctl list-units 'loyal-*' --all --state=active --no-legend;
    for u in cloudflared otelcol-contrib prometheus prometheus-alertmanager prometheus-node-exporter caddy docker pgbouncer postgresql; do
      echo \"\$u \$(systemctl is-enabled \$u 2>&1) \$(systemctl is-active \$u 2>&1)\"; done
    echo '# listening:'; sudo ss -lntu" >"$p/enabled.txt"
  new "for b in /opt/loyal/releases/*/$RELEASE_SHA/loyal-* /opt/loyal/bin/loyal-env-launcher /opt/loyal/bin/loyal-yield-realtime /opt/loyal-hetzner/env-launcher-*; do
      for a in --version --help; do
        out=\$(sudo unshare -n setpriv --reuid=nobody --regid=nogroup --clear-groups timeout 5 \$b \$a 2>&1); rc=\$?
        echo \"\$b \$a rc=\$rc \$(head -1 <<<\"\$out\" | cut -c1-120)\"; done; done" >"$p/binaries.txt"
}

case ${1:-stage} in
  stage) stage ;;
  proof) proof "${2:?proof <dir>}" ;;
  *) echo "usage: $0 [stage|proof <dir>]" >&2; exit 2 ;;
esac
