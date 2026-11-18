#!/usr/bin/env bash
# Switch S1: mnemonic.service and cloudflared-mnemonic.service from the Hetzner box to robinx-main. Runs from the operator's Mac.
set -euo pipefail

OLD_ADDR=${S1_OLD_ADDR:-5.78.147.233}
NEW_ADDR=${S1_NEW_ADDR:-34.229.25.188}
NEW_HOST=${S1_NEW_HOST:-robinx-main}
STORE=/var/lib/mnemonic
DELTA_MAX_MIB=${S1_DELTA_MAX_MIB:-512}
AWS_PROFILE_NAME=${S1_AWS_PROFILE:-main}
AWS_REGION_NAME=${S1_AWS_REGION:-us-east-1}
CLIENT_CONFIG=${S1_CLIENT_CONFIG:-$HOME/.mnemonic/config.toml}
MNEMONIC_CLI=${S1_MNEMONIC_CLI:-mnemonic}
SHARE_URL=${S1_SHARE_URL:-https://share.askloyal.com/}
VANISH_OK=0
ITEMIZE=0
COPY_MIB_S=${S1_COPY_MIB_S:-26}
LIST_MAX=${S1_LIST_MAX:-200}
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30)

usage() {
  cat >&2 <<EOF
usage: $0 preflight | delta [--reverse] | copy | switch --go | back --go [--sync-back] [--risk-two-stores] | back --sync-back --dry-run
preflight  Read-only. Exit 0 only when, on $NEW_HOST, the arm64 binary, both unit files and their slice, the store volume, the
           cloudflared config and credentials and the share port are in place and both units are inactive, the UDP port of the store
           is open in the security group, the new store has room for the delta, and on $OLD_ADDR both units are active and a search
           answers. A store delta over ${DELTA_MAX_MIB} MiB (S1_DELTA_MAX_MIB) is a note, not a failure: switch copies it first.
delta      Dry-run rsync old to new (new to old with --reverse) and print the bytes it would move.
copy       One live pass old to new while the old units keep running. Rerun until delta is small.
switch     Catch-up copy with the old units running, re-measure the delta (stop if still over ${DELTA_MAX_MIB} MiB), stop and disable old
           cloudflared-mnemonic and mnemonic, final rsync, enable and start mnemonic then cloudflared-mnemonic on $NEW_HOST, check
           that a search answers from it and that the share hostname answers. Needs --go.
back       Stop and disable the new units, enable and start the old ones. If $NEW_HOST does not answer, the old side starts only
           when the AWS API shows the new instance stopped or terminated (S1_NEW_INSTANCE_ID names it, else it is found by
           $NEW_ADDR); a new store that answers a search always refuses. With no proof, back refuses, because the new store may
           still be serving and two stores must never run; --risk-two-stores starts the old side anyway, for an operator who knows
           the new box is down or accepts the risk. --sync-back first copies the new store over the old one (use it when the new
           box took writes) and fails when $NEW_HOST cannot be reached or an old unit is running. Needs --go.
back --sync-back --dry-run
           Prints what --sync-back would copy back and changes nothing; refuses when an old unit is running. No --go needed.
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
  ((ITEMIZE)) && flags="$flags -i"
  if [[ $dir == forward ]]; then
    src="$STORE/" dst="ubuntu@$NEW_ADDR:$STORE/"
    flags="$flags --chown=mnemonic:mnemonic"
  else
    src="ubuntu@$NEW_ADDR:$STORE/" dst="$STORE/"
  fi
  local rc=0
  ssh -A "${SSH_OPTS[@]}" "root@$OLD_ADDR" "ionice -c3 nice -n19 rsync $flags --rsync-path='sudo rsync' \
    -e \"ssh -o BatchMode=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes -o HostKeyAlgorithms=ssh-ed25519 -o 'KnownHostsCommand=/bin/echo $hk'\" \
    $src $dst" >"$out" 2>&1 || rc=$?
  if ((rc == 24 && VANISH_OK)); then return 0; fi
  return "$rc"
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
  VANISH_OK=1
  [[ ${1:-} == --reverse ]] && dir=reverse
  t0=$(date +%s)
  b=$(delta_bytes "$dir")
  echo "delta $dir: $b bytes ($((b / 1048576)) MiB), scan took $(($(date +%s) - t0)) s"
}

cmd_copy() {
  local out t0
  VANISH_OK=1
  out=$(mktemp)
  t0=$(date +%s)
  rsync_run forward live "$out" || { cat "$out"; rm -f "$out"; die "copy failed; no unit was stopped"; }
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

# The live store drifts after every copy, so size is a note; only a new box that cannot take the copy (no answer, no room) fails.
store_delta() {
  local b mib free err
  err=$(mktemp)
  if ! b=$(delta_bytes forward 2>"$err"); then
    bad store-delta "$(cat "$err")"
    rm -f "$err"
    return
  fi
  mib=$((b / 1048576))
  free=$(new "df -B1 --output=avail $STORE | tail -n1 | tr -d ' '" 2>&1) || free=
  if [[ ! $free =~ ^[0-9]+$ ]]; then
    bad store-delta "cannot read the free space of $STORE on $NEW_HOST: ${free:-no answer}"
  elif ((free < b + 1073741824)); then
    bad store-delta "$mib MiB to move but $((free / 1048576)) MiB free on $NEW_HOST:$STORE (1024 MiB kept spare)"
  elif ((mib <= DELTA_MAX_MIB)); then
    ok store-delta "$mib MiB to move, limit $DELTA_MAX_MIB MiB ($(cat "$err"))"
  else
    ok store-delta "note: $mib MiB behind the live store, over the $DELTA_MAX_MIB MiB limit; switch --go copies it first with the old units running (about $((mib / COPY_MIB_S + 1)) s at $COPY_MIB_S MiB/s, no extra outage) and stops if it is still over the limit afterwards ($(cat "$err"))"
  fi
  rm -f "$err"
}

cmd_preflight() {
  VANISH_OK=1
  new_static
  new_idle
  old_live
  store_delta
  if ((FAILS)); then echo "preflight: $FAILS check(s) failed"; return 1; fi
  echo "preflight: all checks passed"
}

need_go() { [[ ${1:-} == --go ]] || die "$2 changes both boxes; rerun with --go"; }

cmd_switch() {
  need_go "${1:-}" switch
  local t0 out b mib
  new_static
  new_idle
  ((FAILS == 0)) || die "new box is not ready ($FAILS failed); no unit was touched"
  echo "catch-up copy, old units still running"
  cmd_copy
  VANISH_OK=1
  b=$(delta_bytes forward) || die "could not re-measure the store delta; no unit was touched"
  VANISH_OK=0
  mib=$((b / 1048576))
  ((mib <= DELTA_MAX_MIB)) || die "store still $mib MiB behind after the catch-up, over the $DELTA_MAX_MIB MiB limit (S1_DELTA_MAX_MIB); no unit was touched. Run copy again, or raise the limit"
  echo "store delta after catch-up: $mib MiB, limit $DELTA_MAX_MIB MiB"
  t0=$(date +%s)
  echo "stop and disable old (share links down from here)"
  old 'systemctl disable --now cloudflared-mnemonic && systemctl disable --now mnemonic'
  echo "final rsync"
  out=$(mktemp)
  if ! rsync_run forward live "$out"; then
    cat "$out"
    die "final rsync failed; old units stay stopped and disabled. Rerun switch --go to retry, or back --go to restore the old box"
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

old_running() { # prints the old units that are running; status 2 when the old box does not answer
  local st
  # shellcheck disable=SC2016
  st=$(old 'for u in mnemonic cloudflared-mnemonic; do echo "$u $(systemctl is-active $u)"; done') || return 2
  awk '$2 == "active" || $2 == "activating" || $2 == "reloading" {print $1}' <<<"$st"
}

old_must_be_idle() { # old_must_be_idle <what refuses>
  local rc=0 running
  running=$(old_running) || rc=$?
  ((rc == 0)) || die "$1: the old box does not answer, so it cannot be shown that its units are stopped"
  [[ -z $running ]] || die "$1: old unit(s) running on $OLD_ADDR: ${running//$'\n'/ }; copying a store over a live one would corrupt it. Stop them first"
}

sync_back_dry() {
  local out n total
  old_must_be_idle "sync-back dry run refused"
  new true 2>/dev/null || die "sync-back dry run: $NEW_HOST does not answer; nothing to copy back from"
  out=$(mktemp)
  VANISH_OK=1
  ITEMIZE=1
  rsync_run reverse dry "$out" || { cat "$out"; rm -f "$out"; die "sync-back dry run failed"; }
  echo "would copy back from $NEW_HOST:$STORE/ over $OLD_ADDR:$STORE/ (rsync -aH --delete-delay, nothing written):"
  total=$(grep -cE '^([<>ch.][a-zA-Z.+?]{10}|\*deleting) ' "$out" || true)
  { grep -E '^([<>ch.][a-zA-Z.+?]{10}|\*deleting) ' "$out" || true; } | head -n "$LIST_MAX" | sed 's/^/  /' || true
  ((total <= LIST_MAX)) || echo "  ... $((total - LIST_MAX)) more (S1_LIST_MAX=$LIST_MAX)"
  n=$(stat_of 'Total transferred file size' "$out")
  echo "dry run: $total path(s) listed, files $(stat_of 'Number of regular files transferred' "$out"), deleted $(stat_of 'Number of deleted files' "$out"), bytes $n ($((n / 1048576)) MiB). Nothing was copied or stopped."
  rm -f "$out"
}

new_proven_down() { # the AWS API says the new instance is stopped or terminated; says why not otherwise
  local st ids=${S1_NEW_INSTANCE_ID:-}
  command -v aws >/dev/null || { echo "aws CLI not found"; return 1; }
  if [[ -n $ids ]]; then
    st=$(aws --profile "$AWS_PROFILE_NAME" --region "$AWS_REGION_NAME" ec2 describe-instances --instance-ids "$ids" \
      --query 'Reservations[].Instances[].State.Name' --output text 2>&1) || { echo "aws: $st"; return 1; }
  else
    st=$(aws --profile "$AWS_PROFILE_NAME" --region "$AWS_REGION_NAME" ec2 describe-instances --filters "Name=ip-address,Values=$NEW_ADDR" \
      --query 'Reservations[].Instances[].State.Name' --output text 2>&1) || { echo "aws: $st"; return 1; }
  fi
  case $st in
    stopped | terminated) echo "AWS reports the new instance $st"; return 0 ;;
    "") echo "AWS shows no instance for $NEW_ADDR (a stopped instance without an elastic IP has none): set S1_NEW_INSTANCE_ID"; return 1 ;;
    *) echo "AWS reports the new instance $st"; return 1 ;;
  esac
}

cmd_back() {
  local sync=0 dry=0 go=0 risk=0 arg out new_up=1 why
  for arg in "$@"; do
    case $arg in
      --go) go=1 ;;
      --sync-back) sync=1 ;;
      --dry-run) dry=1 ;;
      --risk-two-stores) risk=1 ;;
      *) die "back: unknown argument $arg" ;;
    esac
  done
  if ((dry)); then
    ((sync)) || die "back --dry-run previews --sync-back; add --sync-back"
    sync_back_dry
    return
  fi
  ((go)) || die "back changes both boxes; rerun with --go"
  ((sync == 0)) || old_must_be_idle "sync-back refused, nothing was changed"
  if new true 2>/dev/null; then
    echo "stop and disable new"
    # shellcheck disable=SC2016
    new 'sudo -n systemctl disable --now cloudflared-mnemonic; a=$?; sudo -n systemctl disable --now mnemonic; b=$?; [ $a -eq 0 ] && [ $b -eq 0 ]' ||
      die "could not stop the new units; the old units were not started, so two stores cannot run"
  else
    new_up=0
    ((sync == 0)) || die "$NEW_HOST does not answer, so --sync-back cannot be honoured; nothing was changed. Rerun without --sync-back to start the old side on its own store"
    if search_check "$NEW_ADDR" >/dev/null 2>&1; then
      die "$NEW_HOST does not answer ssh but the store at $NEW_ADDR answers a search: it is serving. The old side was not started, so two stores cannot run. Stop the new units, then rerun"
    fi
    if why=$(new_proven_down); then
      echo "$NEW_HOST does not answer ssh; $why. Starting the old side"
    elif ((risk)); then
      echo "RISK ACCEPTED (--risk-two-stores): $NEW_HOST does not answer ssh and nothing proves it is down ($why). Starting the old side; if the new store is up this is two stores, and writes it took are not on the old store"
    else
      die "$NEW_HOST does not answer ssh and nothing proves its store is down ($why). The old side was not started, so two stores cannot run. Stop the new instance and rerun, or rerun with --risk-two-stores to start the old side anyway"
    fi
  fi
  if ((sync)); then
    echo "copy new store over old"
    out=$(mktemp)
    rsync_run reverse live "$out" || { cat "$out"; die "reverse rsync failed; old units not started"; }
    grep -E 'Number of regular files transferred|Total transferred file size' "$out"
    rm -f "$out"
  fi
  echo "start old"
  old 'systemctl enable --now mnemonic && systemctl enable --now cloudflared-mnemonic && systemctl is-active mnemonic cloudflared-mnemonic'
  check old-search search_check "$OLD_ADDR" || die "old box does not answer a search"
  if ((new_up)); then echo "back done"; else echo "back done on the old side; $NEW_HOST was not reached, check its units there"; fi
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
