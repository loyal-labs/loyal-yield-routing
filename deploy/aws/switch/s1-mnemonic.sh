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
OWNER_FILE=/var/lib/s1-owner
EXIT_CLIENT=3
CLIENT_FIX_RC=0
MNEMONIC_CLI=${S1_MNEMONIC_CLI:-mnemonic}
SHARE_URL=${S1_SHARE_URL:-https://share.askloyal.com/}
VANISH_OK=0
ITEMIZE=0
COPY_MIB_S=${S1_COPY_MIB_S:-26}
LIST_MAX=${S1_LIST_MAX:-200}
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30)
DAEMON_LABEL=${S1_DAEMON_LABEL:-com.mnemonic.push}
PUSH_LOG=${S1_PUSH_LOG:-$HOME/.mnemonic/push.log}
DAEMON_WAIT=${S1_DAEMON_WAIT:-30}

usage() {
  cat >&2 <<EOF
usage: $0 preflight | delta [--reverse] | copy | switch --go | resume --go | back --go [--sync-back] [--accept-stale-store] [--risk-two-stores] | back --sync-back --dry-run
preflight  Read-only. Exit 0 only when, on $NEW_HOST, the arm64 binary, both unit files and their slice, the store volume, the
           cloudflared config and credentials and the share port are in place and both units are inactive, the UDP port of the store
           is open in the security group, the new store has room for the delta, and on $OLD_ADDR both units are active and a search
           answers. A store delta over ${DELTA_MAX_MIB} MiB (S1_DELTA_MAX_MIB) is a note, not a failure: switch copies it first.
delta      Dry-run rsync old to new (new to old with --reverse) and print the bytes it would move.
copy       One live pass old to new while the old units keep running. Rerun until delta is small.
switch     Catch-up copy with the old units running, re-measure the delta (stop if still over ${DELTA_MAX_MIB} MiB), stop and disable old
           cloudflared-mnemonic and mnemonic, final rsync, write the owner marker on $OLD_ADDR, then start (never enable) mnemonic and
           cloudflared-mnemonic on $NEW_HOST, check that a search answers from it and that the share hostname answers, then point
           the client config at $NEW_ADDR and restart the resident push daemon ($DAEMON_LABEL, launchctl kickstart -k, because it reads
           its config once) until it runs with no push error (a failure there prints the one-line fix and exits $EXIT_CLIENT; the store
           has moved). Open 'mnemonic mcp' sessions keep the old address until they are restarted. Needs --go.
resume     After a reboot of $NEW_HOST, or a switch killed after the marker: start its units again and finish the client handoff.
           Only when the owner marker says AWS owns the store and both old units are stopped AND disabled.  Needs --go. Never rerun switch for this: it would overwrite the new store with the old one.
back       Stop and disable the new units, enable and start the old ones, point the client config back at $OLD_ADDR. If $NEW_HOST
           does not answer, the old side starts only when the AWS API shows the new instance stopped or terminated
           (S1_NEW_INSTANCE_ID names it, else it is found by $NEW_ADDR); a new store that answers a search always refuses. With
           no proof, back refuses, because the new store may still be serving and two stores must never run; --risk-two-stores
           starts the old side anyway. The new units are never enabled at boot, so powering the new box on later cannot start a
           second store. When the owner marker says AWS may have taken writes, back measures what the old store lacks (dry run
           of the copy back) and refuses unless it is zero, or --sync-back copies it, or --accept-stale-store leaves it behind;
           a new box that cannot be reached cannot be measured, so that case needs --accept-stale-store. A measure that fails or cannot be read
           refuses too, before anything is stopped. --sync-back fails when
           $NEW_HOST cannot be reached or an old unit is running. Needs --go.
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

owner_state() { # prints the owner marker of the old box ("aws <epoch>" or nothing); fails when the old box cannot be read
  old "if [ -e $OWNER_FILE ]; then cat $OWNER_FILE; fi"
}

client_config_state() { # prints old, new or a note; fails when the client config does not name the store being moved
  local line
  [[ -f $CLIENT_CONFIG ]] || { echo "no client config at $CLIENT_CONFIG (S1_CLIENT_CONFIG)"; return 1; }
  line=$(grep -m1 '^[[:space:]]*server[[:space:]]*=' "$CLIENT_CONFIG" || true)
  case $line in
    *"quic://$OLD_ADDR:8741"*) echo "$CLIENT_CONFIG names the old store $OLD_ADDR; switch --go points it at $NEW_ADDR" ;;
    *"quic://$NEW_ADDR:8741"*) echo "note: $CLIENT_CONFIG already names the new store $NEW_ADDR" ;;
    *) echo "$CLIENT_CONFIG server line is neither $OLD_ADDR nor $NEW_ADDR: ${line:-no server line}"; return 1 ;;
  esac
}

point_client() { # point_client ADDR: rewrite the one server line, keep the first copy as .pre-s1
  local addr=$1 tmp
  [[ -f $CLIENT_CONFIG ]] || return 1
  [[ -e $CLIENT_CONFIG.pre-s1 ]] || cp -p "$CLIENT_CONFIG" "$CLIENT_CONFIG.pre-s1" || return 1
  tmp=$(mktemp) || return 1
  awk -v s="server = \"quic://$addr:8741\"" '!d && /^[ \t]*server[ \t]*=/ {print s; d=1; next} {print}' "$CLIENT_CONFIG" >"$tmp" || { rm -f "$tmp"; return 1; }
  grep -q "^server = \"quic://$addr:8741\"" "$tmp" || { rm -f "$tmp"; return 1; }
  cat "$tmp" >"$CLIENT_CONFIG" || { rm -f "$tmp"; return 1; }
  rm -f "$tmp"
}

client_fix_line() { echo "sed -i.bak -E 's#^[[:space:]]*server[[:space:]]*=.*#server = \"quic://$1:8741\"#' '$CLIENT_CONFIG'"; }

repoint_client() { # repoint_client ADDR: never fails the caller's switch; sets CLIENT_FIX_RC
  local addr=$1
  if ! point_client "$addr"; then
    echo "client config NOT changed: could not write $CLIENT_CONFIG. Fix by hand: $(client_fix_line "$addr")"
    CLIENT_FIX_RC=$EXIT_CLIENT
    return
  fi
  if [[ $addr == "$NEW_ADDR" ]]; then
    if ! search_check --config; then
      echo "client config now names $addr but a search through it does not answer. Check it, or restore: $(client_fix_line "$OLD_ADDR")"
      CLIENT_FIX_RC=$EXIT_CLIENT
      return
    fi
  fi
  echo "client config $CLIENT_CONFIG now names $addr (first copy $CLIENT_CONFIG.pre-s1)"
  if ! restart_daemon "$addr"; then
    echo "the resident push daemon $DAEMON_LABEL did not prove it reaches $addr: ingestion may be stopped. Check $PUSH_LOG, then: launchctl kickstart -k gui/$(id -u)/$DAEMON_LABEL"
    CLIENT_FIX_RC=$EXIT_CLIENT
    return
  fi
  echo "open 'mnemonic mcp' sessions (editors, agents) keep the old address: restart them"
}

ERR_LINE_RE='^[a-z]+:'

# push_log_errors LOG OFFSET: prints the first error line after OFFSET bytes and fails when the log cannot be read. The chunk is read
# into a variable first: a reader that stops early closes its pipe, and pipefail would turn that into "no error".
push_log_errors() {
  local chunk
  chunk=$(tail -c +"$(($2 + 1))" "$1" 2>/dev/null) || return 1
  grep -m1 -E "$ERR_LINE_RE" <<<"$chunk" || true
}

daemon_domain() { echo "gui/$(id -u)/$DAEMON_LABEL"; }

daemon_print() { launchctl print "$(daemon_domain)" 2>/dev/null; }

daemon_pid() { awk '$1 == "pid" && $2 == "=" {print $3; exit}'; }

daemon_config() { awk '/^[ \t]*arguments = \{/ {a=1; next} a && /^[ \t]*\}/ {a=0} a {n++; if (p) {print $1; exit} p = ($1 == "--config")}'; }

# The daemon read its config once at start, so a rewritten file does nothing until it is restarted. The old store is stopped
# on every path that gets here, so a dial that does not fail can only have reached the store this call moved to.
restart_daemon() { # restart_daemon ADDR
  local addr=$1 st before after off waited=0 cfg errs
  if ! st=$(daemon_print); then
    echo "no $DAEMON_LABEL daemon is loaded for this user: nothing to restart (a hand-started 'mnemonic push --watch' must be restarted by hand)"
    return 0
  fi
  cfg=$(daemon_config <<<"$st")
  [[ $cfg == "$CLIENT_CONFIG" ]] || { echo "the daemon reads ${cfg:-no config path}, not $CLIENT_CONFIG: restarting it would not move it to $addr"; return 1; }
  before=$(daemon_pid <<<"$st")
  off=$(wc -c <"$PUSH_LOG" 2>/dev/null | tr -d ' ') || off=0
  [[ $off =~ ^[0-9]+$ ]] || off=0
  launchctl kickstart -k "$(daemon_domain)" || { echo "launchctl kickstart failed"; return 1; }
  while ((waited < DAEMON_WAIT)); do
    after=$(daemon_print | daemon_pid) || after=
    [[ -n $after && $after != "$before" ]] && break
    sleep 1
    waited=$((waited + 1))
  done
  [[ -n ${after:-} && $after != "$before" ]] || { echo "the daemon did not restart within ${DAEMON_WAIT}s (pid ${before:-none} before)"; return 1; }
  waited=0
  while ((waited < DAEMON_WAIT)); do
    sleep 1
    waited=$((waited + 1))
    errs=$(push_log_errors "$PUSH_LOG" "$off") || errs=
    [[ -z $errs ]] || { echo "daemon pid $after reports: $errs"; return 1; }
  done
  push_log_errors "$PUSH_LOG" "$off" >/dev/null || { echo "the push log $PUSH_LOG cannot be read after the restart, so a push error cannot be told from silence"; return 1; }
  [[ $(daemon_print | daemon_pid) == "$after" ]] || { echo "the daemon pid changed again within ${DAEMON_WAIT}s (a crash loop)"; return 1; }
  echo "daemon $DAEMON_LABEL restarted (pid ${before:-none} to $after), reads $addr from $CLIENT_CONFIG, no push error in ${DAEMON_WAIT}s"
}

daemon_check() { # the resident push daemon must read the config this script rewrites, or restarting it moves nothing
  local st cfg
  if ! st=$(daemon_print); then echo "note: no $DAEMON_LABEL daemon loaded for this user; nothing to restart after the client config moves"; return 0; fi
  cfg=$(daemon_config <<<"$st")
  [[ $cfg == "$CLIENT_CONFIG" ]] || { echo "$DAEMON_LABEL reads ${cfg:-no config path}, not $CLIENT_CONFIG (S1_CLIENT_CONFIG): the rewrite would not move it"; return 1; }
  echo "$DAEMON_LABEL pid $(daemon_pid <<<"$st") reads $CLIENT_CONFIG; switch --go and back --go restart it and read its log for a push error"
}

# Every mnemonic client config this Mac shows: the default folders, the --config path of each launch agent and of each MCP client
# file that names mnemonic. Only server lines are printed, never keys. Other configs are listed and left alone: the one-line rewrite
# is for $CLIENT_CONFIG.
client_configs_found() {
  python3 -I - "$CLIENT_CONFIG" "$OLD_ADDR" "$NEW_ADDR" <<'PY'
import glob, os, re, sys
chosen, old, new = sys.argv[1:4]
home = os.path.expanduser("~")
cand = set(glob.glob(home + "/.mnemonic/*.toml") + glob.glob(home + "/.config/mnemonic/*.toml"))
files = glob.glob(home + "/Library/LaunchAgents/*.plist") + [home + "/.claude.json", home + "/.codex/config.toml", home + "/.cursor/mcp.json", home + "/.mcp.json"]
for f in files:
    try:
        t = open(f, errors="replace").read()
    except OSError:
        continue
    if "mnemonic" not in t:
        continue
    for m in re.finditer(r"--config[\s\"',=<>/a-z]*?(~?/[^\"'\s,<\]]+\.toml)", t):
        cand.add(os.path.expanduser(m.group(1)))
others = sorted(c for c in cand if os.path.abspath(c) != os.path.abspath(chosen) and os.path.isfile(c))
print("%d mnemonic config(s) found besides %s" % (len(others), chosen))
for c in others:
    line = next((l.strip() for l in open(c, errors="replace") if l.startswith("server")), "no server line")
    hit = old in line or new in line
    print("  %s: %s%s" % (c, line, "  <- will NOT be rewritten: point it by hand" if hit else ""))
PY
}

FAILS=0
ok() { echo "PASS $1: $2"; }
bad() { echo "FAIL $1: $2"; FAILS=$((FAILS + 1)); }
check() {
  local name=$1 detail
  shift
  if detail=$("$@" 2>&1); then ok "$name" "$detail"; else bad "$name" "${detail:-failed}"; return 1; fi
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

# stat_of NAME FILE: the number rsync printed for NAME; fails when the line is absent or not a number, so a missing statistic is never zero
stat_of() {
  local v
  v=$(grep -m1 "^$1" "$2" | sed -E 's/[^:]*: *([0-9,]+).*/\1/' | tr -d ,) || return 1
  [[ $v =~ ^[0-9]+$ ]] || return 1
  echo "$v"
}

# rsync_stats FILE: sets ST_FILES, ST_DEL and ST_BYTES from an rsync --stats output; fails when any of the three is absent
rsync_stats() {
  ST_BYTES=$(stat_of 'Total transferred file size' "$1") || return 1
  ST_FILES=$(stat_of 'Number of regular files transferred' "$1") || return 1
  ST_DEL=$(stat_of 'Number of deleted files' "$1") || return 1
}

delta_bytes() {
  local dir=$1 out
  out=$(mktemp)
  if ! rsync_run "$dir" dry "$out"; then
    cat "$out" >&2
    rm -f "$out"
    return 1
  fi
  if ! rsync_stats "$out"; then
    echo "rsync printed no usable statistics, so the delta cannot be measured:" >&2
    tail -5 "$out" >&2
    rm -f "$out"
    return 1
  fi
  echo "files $ST_FILES, deleted $ST_DEL, bytes $ST_BYTES" >&2
  echo "$ST_BYTES"
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

search_check() { # search_check ADDR | search_check --config: the second form uses the client config as it is
  local addr=$1 cfg
  cfg=$(mktemp)
  if [[ $addr == --config ]]; then
    awk '/^\[\[sources\]\]/{exit} {print}' "$CLIENT_CONFIG" >"$cfg"
  else
    awk '/^\[\[sources\]\]/{exit} {print}' "$CLIENT_CONFIG" | sed -E "s#^[[:space:]]*server[[:space:]]*=.*#server = \"quic://$addr:8741\"#" >"$cfg"
  fi
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

new_not_enabled() {
  check new-units-not-enabled new "
    for u in mnemonic cloudflared-mnemonic; do systemctl is-enabled -q \$u 2>/dev/null && { echo \"\$u is enabled at boot: powering the box on would start a store\"; exit 1; }; done
    echo 'neither unit is enabled at boot'"
}

owner_clear() {
  local st rc=0
  st=$(owner_state) || rc=$?
  if ((rc)); then echo "the old box does not answer: its owner marker cannot be read"; return 1; fi
  [[ -z $st ]] && { echo "no owner marker on $OLD_ADDR: Hetzner owns the store"; return 0; }
  echo "owner marker on $OLD_ADDR says: $st. AWS took the store; use back (or resume), a switch would overwrite it"
  return 1
}

old_live() {
  check owner-marker owner_clear
  check client-config client_config_state
  check client-daemon daemon_check
  check client-other-configs client_configs_found
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
  # check fails when its check fails; FAILS keeps the count, so a list of checks runs to its end inside || true
  { new_static; new_idle; new_not_enabled; old_live; } || true
  store_delta
  if ((FAILS)); then echo "preflight: $FAILS check(s) failed"; return 1; fi
  echo "preflight: all checks passed"
}

need_go() { [[ ${1:-} == --go ]] || die "$2 changes both boxes; rerun with --go"; }

cmd_switch() {
  need_go "${1:-}" switch
  local t0 out b mib
  { new_static; new_idle; new_not_enabled; check owner-marker owner_clear; } || true
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
  old "echo aws \$(date +%s) >$OWNER_FILE" || die "could not write the owner marker on $OLD_ADDR; nothing was started on $NEW_HOST. Run back --go"
  start_new_units "switch"
  echo "pause $(($(date +%s) - t0)) s (old stop to new start)"
  switch_check
  repoint_client "$NEW_ADDR"
  return "$CLIENT_FIX_RC"
}

start_new_units() { # the units are started, never enabled: a power-on of the box cannot start a store
  # shellcheck disable=SC2016
  new 'for u in mnemonic cloudflared-mnemonic; do sudo -n systemctl disable $u >/dev/null 2>&1 || true; systemctl is-enabled -q $u 2>/dev/null && { echo "$u is still enabled at boot"; exit 1; }; done
    sudo -n systemctl start mnemonic && for i in $(seq 1 60); do ss -ltn | grep -q ":8742 " && exit 0; sleep 1; done; echo "mnemonic is not listening on 8742"; exit 1' ||
    die "new mnemonic did not come up; run $0 back --go"
  new 'sudo -n systemctl start cloudflared-mnemonic' || die "new cloudflared did not start; run $0 back --go"
}

cmd_resume() {
  need_go "${1:-}" resume
  local st rc=0
  st=$(owner_state) || rc=$?
  ((rc == 0)) || die "resume: the old box does not answer, so ownership cannot be read; nothing was started"
  [[ $st == aws* ]] || die "resume: the owner marker on $OLD_ADDR says '${st:-Hetzner}', not AWS; nothing was started. Use switch --go for a first move"
  local why
  why=$(old_stopped_disabled) || die "resume refused: $why. AWS is not started; the old box could bring its store back at a boot"
  new true 2>/dev/null || die "resume: $NEW_HOST does not answer"
  start_new_units resume
  switch_check
  repoint_client "$NEW_ADDR"
  return "$CLIENT_FIX_RC"
}

switch_check() {
  local fail=0 code
  check new-search search_check "$NEW_ADDR" || fail=1
  new 'systemctl is-active mnemonic cloudflared-mnemonic && systemctl show mnemonic cloudflared-mnemonic -p Slice --value' || fail=1
  new 'sudo -n journalctl -u cloudflared-mnemonic --since "-2min" --no-pager | grep -c "Registered tunnel connection"' || fail=1
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$SHARE_URL" || true)
  echo "share hostname answers HTTP $code"
  [[ $code =~ ^[2-4][0-9][0-9]$ ]] || fail=1
  if ((fail)); then die "check after switch failed; run back --go"; fi
  echo "switch done: a search answers from $NEW_ADDR and the share hostname answers"
}

old_units_state() { # prints "unit active enabled" per old unit; fails when the old box does not answer or a unit gives no state
  local st
  # shellcheck disable=SC2016
  st=$(old 'for u in mnemonic cloudflared-mnemonic; do echo "$u $(systemctl is-active $u) $(systemctl is-enabled $u)"; done') || return 1
  [[ $(awk 'NF == 3' <<<"$st" | wc -l) -eq 2 ]] || return 1
  echo "$st"
}

old_running() { # prints the old units that are running; status 2 when the old box does not answer
  local st
  st=$(old_units_state) || return 2
  awk '$2 == "active" || $2 == "activating" || $2 == "reloading" || $2 == "deactivating" {print $1}' <<<"$st"
}

old_stopped_disabled() { # fails unless both old units are positively inactive and disabled: an enabled unit starts at the next boot of the old box
  local st bad
  st=$(old_units_state) || { echo "the old box does not answer or gave no unit state"; return 1; }
  bad=$(awk '!(($2 == "inactive" || $2 == "failed") && ($3 == "disabled" || $3 == "masked")) {printf "%s is %s and %s; ", $1, $2, $3}' <<<"$st")
  [[ -z $bad ]] || { echo "${bad}run on $OLD_ADDR: systemctl disable --now mnemonic cloudflared-mnemonic"; return 1; }
  echo "both old units inactive and disabled"
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
  rsync_stats "$out" || { rm -f "$out"; die "sync-back dry run: rsync printed no usable statistics"; }
  n=$ST_BYTES
  echo "dry run: $total path(s) listed, files $ST_FILES, deleted $ST_DEL, bytes $n ($((n / 1048576)) MiB). Nothing was copied or stopped."
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

left_behind() { # prints "BYTES DELETED" the copy back would carry from the new store to the old one; fails when it cannot be measured
  local out
  out=$(mktemp)
  VANISH_OK=1
  if ! rsync_run reverse dry "$out" || ! rsync_stats "$out"; then rm -f "$out"; return 1; fi
  rm -f "$out"
  echo "$ST_BYTES $ST_DEL"
}

# back_gate OWNER: the answer to "what does the old store lack", for the caller's flags. Prints why it refuses and fails; prints a
# STALE STORE ACCEPTED line and succeeds when --accept-stale-store covers it. A measure that failed or could not be read refuses.
back_gate() { # back_gate ACCEPT(0|1)
  local accept=$1 left why
  if left=$(left_behind); then
    [[ $left != "0 0" ]] || return 0
    why="the new store holds $((${left% *} / 1048576)) MiB (${left% *} bytes) and ${left#* } deletion(s) the old store lacks"
    ((accept)) || { echo "$why"; return 1; }
    echo "STALE STORE ACCEPTED: $why; they stay on $NEW_HOST:$STORE"
  else
    why="what the old store lacks could not be measured"
    ((accept)) || { echo "$why"; return 1; }
    echo "STALE STORE ACCEPTED: $why"
  fi
}

# keep_new_serving WHY: a refusal after the new units were stopped puts the new side back, because it holds the writes
keep_new_serving() {
  local why=$1
  if new 'sudo -n systemctl start mnemonic && sudo -n systemctl start cloudflared-mnemonic' && search_check "$NEW_ADDR" >/dev/null 2>&1; then
    die "$why. The old side is not running. The new units were started again (not enabled) and the new store answers a search, so $NEW_HOST owns the store and no side is down"
  fi
  die "$why. The old side is not running and the new units could NOT be restarted: both sides are down and $NEW_HOST holds the writes. Run: $0 resume --go"
}

# old_start_failed WHY OWNER NEW_UP: the old side did not come up. It is stopped again so two stores cannot run, the owner marker
# is left as it was, and the previous owner is restarted when there is one to restart.
old_start_failed() {
  local why=$1 owner=$2 new_up=$3
  # shellcheck disable=SC2016
  old 'a=0; systemctl disable --now cloudflared-mnemonic || a=1; systemctl disable --now mnemonic || a=1; [ $a -eq 0 ]' ||
    die "$why. The old units could not be stopped again: check both boxes before anything else (owner marker on $OLD_ADDR: ${owner:-none})"
  [[ -n $owner ]] || die "$why. No side serves: the old units are stopped and the owner marker on $OLD_ADDR is clear, so $NEW_HOST holds no taken-over store. Fix the old units, then rerun back --go"
  ((new_up)) || die "$why. No side serves: the old units are stopped and $NEW_HOST was not reached, so the owner marker on $OLD_ADDR still says AWS ($owner) and AWS owns the store. Fix the old units and rerun back --go, or power on $NEW_HOST and run resume --go"
  keep_new_serving "$why. The old units were stopped again and the owner marker on $OLD_ADDR still says AWS ($owner)"
}

cmd_back() {
  local sync=0 dry=0 go=0 risk=0 accept=0 arg out new_up=1 why owner rc=0 gate
  for arg in "$@"; do
    case $arg in
      --go) go=1 ;;
      --sync-back) sync=1 ;;
      --dry-run) dry=1 ;;
      --risk-two-stores) risk=1 ;;
      --accept-stale-store) accept=1 ;;
      *) die "back: unknown argument $arg" ;;
    esac
  done
  if ((dry)); then
    ((sync)) || die "back --dry-run previews --sync-back; add --sync-back"
    sync_back_dry
    return
  fi
  ((go)) || die "back changes both boxes; rerun with --go"
  owner=$(owner_state) || rc=$?
  ((rc == 0)) || die "the old box does not answer, so back cannot start it or read who owns the store; nothing was changed"
  ((sync == 0)) || old_must_be_idle "sync-back refused, nothing was changed"
  if new true 2>/dev/null; then
    if [[ -n $owner && $sync == 0 && $accept == 0 ]]; then
      why=$(back_gate 0) || die "$why. Nothing was stopped. Rerun with --sync-back to copy them over the old store, or --accept-stale-store to start the old store without them"
    fi
    echo "stop and disable new"
    # shellcheck disable=SC2016
    new 'sudo -n systemctl disable --now cloudflared-mnemonic; a=$?; sudo -n systemctl disable --now mnemonic; b=$?; [ $a -eq 0 ] && [ $b -eq 0 ]' ||
      keep_new_serving "could not stop the new units"
    if [[ -n $owner && $sync == 0 ]]; then
      gate=$(back_gate "$accept") || keep_new_serving "$gate (writes may have landed since the check). Rerun with --sync-back to copy them over the old store, or --accept-stale-store to start the old store without them"
      [[ -z $gate ]] || echo "$gate"
    fi
  else
    new_up=0
    ((sync == 0)) || die "$NEW_HOST does not answer, so --sync-back cannot be honoured; nothing was changed. Rerun without --sync-back to start the old side on its own store"
    if search_check "$NEW_ADDR" >/dev/null 2>&1; then
      die "$NEW_HOST does not answer ssh but the store at $NEW_ADDR answers a search: it is serving. The old side was not started, so two stores cannot run. Stop the new units, then rerun"
    fi
    if why=$(new_proven_down); then
      echo "$NEW_HOST does not answer ssh; $why"
    elif ((risk)); then
      echo "RISK ACCEPTED (--risk-two-stores): $NEW_HOST does not answer ssh and nothing proves it is down ($why). Starting the old side; if the new store is up this is two stores, and writes it took are not on the old store"
    else
      die "$NEW_HOST does not answer ssh and nothing proves its store is down ($why). The old side was not started, so two stores cannot run. Stop the new instance and rerun, or rerun with --risk-two-stores to start the old side anyway"
    fi
    if [[ -n $owner ]]; then
      ((accept)) || die "AWS took the store ($owner) and $NEW_HOST cannot be read, so the writes it took cannot be measured; the old store lacks them. The old side was not started. The new units are not enabled at boot, so powering the box on later starts nothing. Rerun with --accept-stale-store to start the old store without them"
      echo "STALE STORE ACCEPTED: $NEW_HOST cannot be read; the writes AWS took stay there, size unknown"
    fi
    echo "starting the old side"
  fi
  if ((sync)); then
    echo "copy new store over old"
    out=$(mktemp)
    rsync_run reverse live "$out" || { cat "$out"; keep_new_serving "reverse rsync failed"; }
    grep -E 'Number of regular files transferred|Total transferred file size' "$out"
    rm -f "$out"
  fi
  echo "start old"
  old 'systemctl enable --now mnemonic && systemctl enable --now cloudflared-mnemonic && systemctl is-active mnemonic cloudflared-mnemonic' ||
    old_start_failed "the old units did not start" "$owner" "$new_up"
  check old-search search_check "$OLD_ADDR" || old_start_failed "the old store does not answer a search" "$owner" "$new_up"
  old "rm -f $OWNER_FILE" || die "the old store is up and answers, and the new units are stopped, so $OLD_ADDR owns the store; but its owner marker $OWNER_FILE could not be cleared and still says AWS. Remove it by hand, then point the client: $(client_fix_line "$OLD_ADDR")"
  if ((new_up)); then echo "back done"; else echo "back done on the old side; $NEW_HOST was not reached. Its units are not enabled at boot, so powering it on starts nothing; do not start them"; fi
  repoint_client "$OLD_ADDR"
  return "$CLIENT_FIX_RC"
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
  resume) cmd_resume "$@" ;;
  *) usage ;;
esac
