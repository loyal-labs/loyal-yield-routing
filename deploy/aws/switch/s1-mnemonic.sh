#!/usr/bin/env bash
# Switch S1: mnemonic.service and cloudflared-mnemonic.service from the Hetzner box to robinx-main. Runs from the operator's Mac.
set -euo pipefail

OLD_ADDR=${S1_OLD_ADDR:-5.78.147.233}
NEW_ADDR=${S1_NEW_ADDR:-34.229.25.188}
NEW_HOST=${S1_NEW_HOST:-robinx-main}
STORE=/var/lib/mnemonic
DELTA_MAX_MIB=${S1_DELTA_MAX_MIB:-2048}
AWS_PROFILE_NAME=${S1_AWS_PROFILE:-main}
AWS_REGION_NAME=${S1_AWS_REGION:-us-east-1}
CLIENT_CONFIG=${S1_CLIENT_CONFIG:-$HOME/.mnemonic/config.toml}
MNEMONIC_CLI=${S1_MNEMONIC_CLI:-mnemonic}
SHARE_URL=${S1_SHARE_URL:-https://share.askloyal.com/}
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30)

usage() {
  cat >&2 <<EOF
usage: $0 preflight | delta [--reverse] | copy | switch --go | back --go [--sync-back]
preflight  Read-only. Exit 0 only when, on $NEW_HOST, the arm64 binary, both unit files and their slice, the store volume, the
           cloudflared config and credentials and the share port are in place and both units are inactive, the UDP port of the store
           is open in the security group, the store delta is under ${DELTA_MAX_MIB} MiB (S1_DELTA_MAX_MIB), and on $OLD_ADDR both units
           are active and a search answers.
delta      Dry-run rsync old to new (new to old with --reverse) and print the bytes it would move.
copy       One live pass old to new while the old units keep running. Rerun until delta is small.
switch     Stop old cloudflared-mnemonic and mnemonic, final rsync, enable and start mnemonic then cloudflared-mnemonic on $NEW_HOST,
           check that a search answers from it and that the share hostname answers. Needs --go.
back       Stop and disable the new units, start the old ones. --sync-back first copies the new store over the old one (use it
           when the new box took writes). Needs --go.
Both boxes are reached from this machine. The copy runs on the old box and pushes to the new one; the old box reaches the new
one through your forwarded ssh agent (ssh -A), so no key is stored on either box. Allowed: the agent holds a key for root@$OLD_ADDR
and ubuntu@$NEW_HOST.
EOF
  exit 2
}

die() { echo "s1-mnemonic: $*" >&2; exit 1; }
# shellcheck disable=SC2029
old() { ssh "${SSH_OPTS[@]}" "root@$OLD_ADDR" "$@"; }
# shellcheck disable=SC2029
new() { ssh "${SSH_OPTS[@]}" "$NEW_HOST" "$@"; }

FAILS=0
ok() { echo "PASS $1: $2"; }
bad() { echo "FAIL $1: $2"; FAILS=$((FAILS + 1)); }
check() {
  local name=$1 detail
  shift
  if detail=$("$@" 2>&1); then ok "$name" "$detail"; else bad "$name" "${detail:-failed}"; fi
}

new_hostkey() {
  ssh-keygen -F "$NEW_ADDR" -f "$HOME/.ssh/known_hosts" | grep -v '^#' | grep ssh-ed25519 | head -1
}

rsync_run() {
  local dir=$1 live=$2 out=$3 hk src dst flags
  hk=$(new_hostkey)
  [[ -n $hk ]] || die "no ssh-ed25519 host key for $NEW_ADDR in ~/.ssh/known_hosts; ssh $NEW_HOST once first"
  flags="-aH --delete-delay --exclude=/lost+found --stats"
  [[ $live == dry ]] && flags="$flags --dry-run"
  if [[ $dir == forward ]]; then
    src="$STORE/" dst="ubuntu@$NEW_ADDR:$STORE/"
    flags="$flags --chown=mnemonic:mnemonic"
  else
    src="ubuntu@$NEW_ADDR:$STORE/" dst="$STORE/"
  fi
  ssh -A "${SSH_OPTS[@]}" "root@$OLD_ADDR" "ionice -c3 nice -n19 rsync $flags --rsync-path='sudo rsync' \
    -e \"ssh -o BatchMode=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes -o HostKeyAlgorithms=ssh-ed25519 -o 'KnownHostsCommand=/bin/echo $hk'\" \
    $src $dst" >"$out" 2>&1
}

stat_of() { grep -m1 "^$1" "$2" | sed -E 's/[^:]*: *([0-9,]+).*/\1/' | tr -d ,; }

delta_bytes() {
  local dir=$1 out
  out=$(mktemp)
  if ! rsync_run "$dir" dry "$out"; then
    cat "$out" >&2
    rm -f "$out"
    return 1
  fi
  echo "files $(stat_of 'Number of regular files transferred' "$out"), deleted $(stat_of 'Number of deleted files' "$out"), bytes $(stat_of 'Total transferred file size' "$out")" >&2
  stat_of 'Total transferred file size' "$out"
  rm -f "$out"
}

cmd_delta() {
  local dir=forward t0 b
  [[ ${1:-} == --reverse ]] && dir=reverse
  t0=$(date +%s)
  b=$(delta_bytes "$dir")
  echo "delta $dir: $b bytes ($((b / 1048576)) MiB), scan took $(($(date +%s) - t0)) s"
}

cmd_copy() {
  local out t0
  out=$(mktemp)
  t0=$(date +%s)
  rsync_run forward live "$out" || { cat "$out"; rm -f "$out"; die "copy failed"; }
  grep -E 'Number of regular files transferred|Total transferred file size|Total bytes sent|Number of deleted' "$out"
  echo "copy took $(($(date +%s) - t0)) s"
  rm -f "$out"
}

search_check() {
  local addr=$1 cfg
  cfg=$(mktemp)
  awk '/^\[\[sources\]\]/{exit} {print}' "$CLIENT_CONFIG" | sed -E "s#^server *=.*#server = \"quic://$addr:8741\"#" >"$cfg"
  local rc=0
  python3 -I - "$MNEMONIC_CLI" "$cfg" <<'PY' || rc=$?
import json, subprocess, sys, threading
cli, cfg = sys.argv[1], sys.argv[2]
p = subprocess.Popen([cli, "mcp", "--config", cfg], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
threading.Timer(40, p.kill).start()
def send(m):
    p.stdin.write(json.dumps(m) + "\n")
    p.stdin.flush()
send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "s1", "version": "1"}}})
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
send({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "search_sessions", "arguments": {"query": "mnemonic", "limit": 1}}})
for line in p.stdout:
    m = json.loads(line)
    if m.get("id") == 2:
        text = "".join(c.get("text", "") for c in m.get("result", {}).get("content", []))
        head = text.split("\n")[0]
        p.kill()
        if head.startswith("Found ") and not head.startswith("Found 0 "):
            print(head)
            sys.exit(0)
        print(head or json.dumps(m)[:200])
        sys.exit(1)
print("no answer")
sys.exit(1)
PY
  rm -f "$cfg"
  return $rc
}

new_static() {
  check binary new "
    f=\$(file -b /usr/local/bin/mnemonic)
    case \$f in *'ELF 64-bit'*aarch64*) ;; *) echo \"\$f\"; exit 1;; esac
    /usr/local/bin/mnemonic 2>&1 | grep -q '^usage: mnemonic' || { echo 'does not run'; exit 1; }
    echo \"aarch64, runs, sha256 \$(sha256sum /usr/local/bin/mnemonic | cut -c1-16)\""
  check units-verify new "sudo -n systemd-analyze verify mnemonic.service cloudflared-mnemonic.service 2>&1 && echo 'verify rc=0'"
  check slice new "
    q=\$(systemctl show mnemonic.slice -p CPUQuotaPerSecUSec --value); m=\$(systemctl show mnemonic.slice -p MemoryMax --value)
    [ \"\$q\" = 2s ] && [ \"\$m\" = 8589934592 ] || { echo \"slice cpu \$q mem \$m\"; exit 1; }
    for u in mnemonic cloudflared-mnemonic; do s=\$(systemctl show \$u -p Slice --value); [ \"\$s\" = mnemonic.slice ] || { echo \"\$u in \$s\"; exit 1; }; done
    echo 'mnemonic.slice 2 cores 8 GiB, both units in it'"
  check store-mount new "
    t=\$(findmnt -no FSTYPE,SOURCE $STORE) || { echo 'not a mount point'; exit 1; }
    grep -q '^UUID=.* $STORE ' /etc/fstab || { echo 'no fstab line'; exit 1; }
    echo \"\$t, \$(df -h --output=avail $STORE | tail -1 | tr -d ' ') free\""
  check config new "
    sudo -n test -s /etc/mnemonic/server.toml || { echo 'no server.toml'; exit 1; }
    sudo -n grep -q '^data_dir *= *\"$STORE\"' /etc/mnemonic/server.toml || { echo 'data_dir is not $STORE'; exit 1; }
    sudo -n grep -q '^quic_addr *= *\"$NEW_ADDR:8741\"' /etc/mnemonic/server.toml || { echo 'quic_addr is not $NEW_ADDR'; exit 1; }
    echo 'server.toml present, data_dir and quic_addr as expected'"
  check cloudflared new "
    sudo -n test -s /etc/cloudflared/mnemonic.yml && sudo -n test -s /etc/cloudflared/mnemonic-share.json || { echo 'config or credentials missing'; exit 1; }
    /usr/local/bin/cloudflared --version | head -1"
  check credentials-match creds_match
  check udp-8741 sg_udp
}

creds_match() {
  local a b
  a=$(old 'sha256sum /etc/cloudflared/mnemonic-share.json' | cut -d' ' -f1)
  b=$(new 'sudo -n sha256sum /etc/cloudflared/mnemonic-share.json' | cut -d' ' -f1)
  [[ -n $a && $a == "$b" ]] || { echo "tunnel credentials differ between the boxes"; return 1; }
  echo "same tunnel credentials as the old box"
}

sg_udp() {
  command -v aws >/dev/null || { echo "aws CLI not found"; return 1; }
  command -v jq >/dev/null || { echo "jq not found"; return 1; }
  local sgs n
  sgs=$(aws --profile "$AWS_PROFILE_NAME" --region "$AWS_REGION_NAME" ec2 describe-instances --filters "Name=ip-address,Values=$NEW_ADDR" \
    --query 'Reservations[].Instances[].SecurityGroups[].GroupId' --output text 2>&1) || { echo "aws: $sgs"; return 1; }
  [[ -n $sgs ]] || { echo "no instance with $NEW_ADDR in profile $AWS_PROFILE_NAME"; return 1; }
  # shellcheck disable=SC2086
  n=$(aws --profile "$AWS_PROFILE_NAME" --region "$AWS_REGION_NAME" ec2 describe-security-groups --group-ids $sgs --output json |
    jq '[.SecurityGroups[].IpPermissions[] | select(.IpProtocol == "-1" or (.IpProtocol == "udp" and .FromPort <= 8741 and .ToPort >= 8741))] | length')
  if ((n > 0)); then echo "UDP 8741 allowed in $sgs"; else echo "UDP 8741 is not open in $sgs; clients and share links reach the store on it"; return 1; fi
}

new_idle() {
  check new-units-inactive new "
    for u in mnemonic cloudflared-mnemonic; do s=\$(systemctl is-active \$u || true); [ \"\$s\" = inactive ] || { echo \"\$u is \$s\"; exit 1; }; done
    echo 'both inactive'"
}

old_live() {
  check old-units-active old "
    for u in mnemonic cloudflared-mnemonic; do s=\$(systemctl is-active \$u || true); [ \"\$s\" = active ] || { echo \"\$u is \$s\"; exit 1; }; done
    echo 'both active'"
  check old-search search_check "$OLD_ADDR"
}

cmd_preflight() {
  new_static
  new_idle
  old_live
  local b
  if b=$(delta_bytes forward 2>/tmp/s1-delta.$$); then
    if ((b / 1048576 <= DELTA_MAX_MIB)); then ok store-delta "$((b / 1048576)) MiB to move, limit $DELTA_MAX_MIB MiB ($(cat /tmp/s1-delta.$$))"; else bad store-delta "$((b / 1048576)) MiB to move, over the $DELTA_MAX_MIB MiB limit; run copy again"; fi
  else
    bad store-delta "$(cat /tmp/s1-delta.$$)"
  fi
  rm -f /tmp/s1-delta.$$
  if ((FAILS)); then echo "preflight: $FAILS check(s) failed"; return 1; fi
  echo "preflight: all checks passed"
}

need_go() { [[ ${1:-} == --go ]] || die "$2 changes both boxes; rerun with --go"; }

cmd_switch() {
  need_go "${1:-}" switch
  local t0 out
  new_static
  new_idle
  ((FAILS == 0)) || die "new box is not ready ($FAILS failed); no unit was touched"
  t0=$(date +%s)
  echo "stop old (share links down from here)"
  old 'systemctl stop cloudflared-mnemonic && systemctl stop mnemonic'
  echo "final rsync"
  out=$(mktemp)
  if ! rsync_run forward live "$out"; then
    cat "$out"
    die "final rsync failed; old units stay stopped. Rerun switch --go to retry, or back --go to restore the old box"
  fi
  grep -E 'Number of regular files transferred|Total transferred file size|Total bytes sent' "$out"
  rm -f "$out"
  echo "start new"
  # shellcheck disable=SC2016
  new 'sudo -n systemctl enable --now mnemonic && for i in $(seq 1 60); do ss -ltn | grep -q ":8742 " && exit 0; sleep 1; done; echo "mnemonic is not listening on 8742"; exit 1' ||
    die "new mnemonic did not come up; run back --go"
  new 'sudo -n systemctl enable --now cloudflared-mnemonic' || die "new cloudflared did not start; run back --go"
  echo "pause $(($(date +%s) - t0)) s (old stop to new start)"
  switch_check
}

switch_check() {
  local fail=0 code
  check new-search search_check "$NEW_ADDR"
  ((FAILS == 0)) || fail=1
  new 'systemctl is-active mnemonic cloudflared-mnemonic && systemctl show mnemonic cloudflared-mnemonic -p Slice --value' || fail=1
  new 'sudo -n journalctl -u cloudflared-mnemonic --since "-2min" --no-pager | grep -c "Registered tunnel connection"' || fail=1
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$SHARE_URL" || true)
  echo "share hostname answers HTTP $code"
  [[ $code =~ ^[2-4][0-9][0-9]$ ]] || fail=1
  if ((fail)); then die "check after switch failed; run back --go"; fi
  echo "switch done: a search answers from $NEW_ADDR and the share hostname answers"
}

cmd_back() {
  need_go "${1:-}" back
  local sync=0 out
  [[ ${2:-} == --sync-back ]] && sync=1
  echo "stop new"
  new 'sudo -n systemctl disable --now cloudflared-mnemonic && sudo -n systemctl disable --now mnemonic'
  if ((sync)); then
    echo "copy new store over old"
    out=$(mktemp)
    rsync_run reverse live "$out" || { cat "$out"; die "reverse rsync failed; old units not started"; }
    grep -E 'Number of regular files transferred|Total transferred file size' "$out"
    rm -f "$out"
  fi
  echo "start old"
  old 'systemctl start mnemonic && systemctl start cloudflared-mnemonic && systemctl is-active mnemonic cloudflared-mnemonic'
  check old-search search_check "$OLD_ADDR" || die "old box does not answer a search"
  echo "back done"
}

[[ $# -ge 1 ]] || usage
sub=$1
shift
case $sub in
  preflight) cmd_preflight ;;
  delta) cmd_delta "$@" ;;
  copy) cmd_copy ;;
  switch) cmd_switch "$@" ;;
  back) cmd_back "$@" ;;
  *) usage ;;
esac
