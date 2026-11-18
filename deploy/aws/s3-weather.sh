#!/usr/bin/env bash
# Switch S3: the live weather bot moves from Hetzner loyal-eu-1 to the AWS box weather-live (Stockholm).
#   s3-weather.sh prepare            install users, dirs, units (disabled), binaries and env on weather-live; a rerun prints "0 change(s)"
#   s3-weather.sh sync [--dry-run] [--pair DST]   niced pre-copy while the old bot runs; --dry-run records the delta; --pair DST
#                                    copies one tree (no stamp); a full sync that fails in any pair writes no stamp
#   s3-weather.sh verify             read-only manifest (path, size, mtime) of every copied tree, old box against new box
#   s3-weather.sh preflight          read-only; exit 0 only when ready and the old units are active
#   s3-weather.sh switch --go        stop and disable the old units, final copy, start the new ones, check quotes and fills
#   s3-weather.sh back --go          the reverse: stop and disable the new units, copy state home, start the old ones
# Runs on the operator's Mac. Both boxes are reached by their ssh aliases; data moves box to box through a throwaway ssh agent
# forwarded to the old box, so no secret or data file touches the Mac. Nothing here places an order or signs anything.
set -euo pipefail

OLD=${S3_OLD:-loyal-eu-1}
NEW=${S3_NEW:-weather-live}
NEWIP=${S3_NEW_IP:-13.62.168.190}
SSH_KEY=${S3_SSH_KEY:-$HOME/.ssh/id_ed25519}
PROOF=${S3_RESTING_PROOF:-/Users/user/loyal/artifacts/orchestrate/aws-migration/proof/ready/S3-resting-order.txt}
BWLIMIT=${S3_BWLIMIT:-15000}
STALE_SECS=${S3_STALE_SECS:-21600}
QUOTE_WAIT=${S3_QUOTE_WAIT:-240}
PAIR_TIMEOUT=${S3_PAIR_TIMEOUT:-1200}
HERE=$(cd "$(dirname "$0")" && pwd)

UNITS=(w20-live.service w20-record.service w2-weather.service w20-scorecard.service w20-scorecard.timer
  weather-reward-pilot-rotate.service weather-reward-pilot-rotate.timer weather-reward-pilot.service)
WATCHED_OLD=(w20-live.service w20-record.service w2-weather.service w20-scorecard.timer weather-reward-pilot-rotate.timer)

die() { echo "s3-weather: $*" >&2; exit 1; }
say() { echo "s3-weather: $*"; }

oldsh() { ssh -o BatchMode=yes -o ConnectTimeout=20 "$OLD" "$@"; }
newsh() { ssh -o BatchMode=yes -o ConnectTimeout=20 "$NEW" "$@"; }

AGENT_SOCK=
AGENT_PID=
agent_up() {
  [[ -n $AGENT_SOCK ]] && return
  [[ -r $SSH_KEY ]] || die "ssh key $SSH_KEY not readable (S3_SSH_KEY)"
  eval "$(ssh-agent -s)" >/dev/null
  AGENT_SOCK=$SSH_AUTH_SOCK
  AGENT_PID=$SSH_AGENT_PID
  ssh-add "$SSH_KEY" </dev/null >/dev/null 2>&1 || die "ssh-add $SSH_KEY failed (needs a key without passphrase)"
  HKLINE=$(ssh-keygen -F "$NEWIP" -f "$HOME/.ssh/known_hosts" 2>/dev/null | awk '$2 == "ssh-ed25519" {print $2, $3; exit}')
  [[ -n $HKLINE ]] || die "no ed25519 host key for $NEWIP in ~/.ssh/known_hosts; ssh $NEW once first"
}
cleanup() {
  [[ -n $AGENT_PID ]] && kill "$AGENT_PID" 2>/dev/null || true
  rmdir /tmp/s3-weather.lock 2>/dev/null || true
}

# hop: runs the script on stdin on the old box as root, with push/pull rsync helpers aimed at the new box.
hop() {
  agent_up
  {
    printf 'set -uo pipefail\nNEWIP=%q\nHK=%q\nBW=%q\nPT=%q\n' "$NEWIP" "$HKLINE" "$BWLIMIT" "$PAIR_TIMEOUT"
    cat <<'EOF'
NETOPTS="-o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=30 -o ServerAliveCountMax=4"
RSH="ssh $NETOPTS -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/dev/null -o HostKeyAlgorithms=ssh-ed25519 -o \"KnownHostsCommand=/bin/echo $NEWIP $HK\""
SSHN=(ssh -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=30 -o ServerAliveCountMax=4 -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/dev/null -o HostKeyAlgorithms=ssh-ed25519 -o "KnownHostsCommand=/bin/echo $NEWIP $HK")
rs() { nice -n 19 ionice -c 3 timeout -k 20 "$PT" rsync -a --numeric-ids --timeout=300 --rsync-path="sudo rsync" -e "$RSH" "$@"; local rc=$?; [ $rc -eq 24 ] && rc=0; return $rc; }
EOF
    cat
  } | ssh -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=30 -o ServerAliveCountMax=4 -o ForwardAgent="$AGENT_SOCK" "$OLD" 'bash -s'
}

old_bins() { # paths the units name under /srv/w20/bin and /srv/w20/reward-pilot, read from the old box so a new tail binary is followed
  oldsh "cd /etc/systemd/system && grep -ohE '/srv/w20/(bin|reward-pilot)/[A-Za-z0-9._-]+' ${UNITS[*]} w20-record.service.d/*.conf | sort -u"
}

# ---- prepare: host convergence, then units, binaries, env and scripts copied from the old box
cmd_prepare() {
  local total=0 n out script ulist=${UNITS[*]}
  say "host step on $NEW"
  out=$(newsh 'sudo bash -s' <"$HERE/weather-live-prepare.sh") || die "weather-live-prepare.sh failed: $out"
  # shellcheck disable=SC2001
  echo "$out" | sed 's/^/  /'
  n=$(echo "$out" | sed -nE 's/.*: ([0-9]+) change\(s\)$/\1/p'); total=$((total + n))

  say "code step: units, binaries, env, scripts from $OLD (names only)"
  local bins
  bins=$(old_bins | tr '\n' ' ')
  IFS= read -r -d '' script <<EOF || true
cd /
UL=""
for u in $ulist; do UL="\$UL /etc/systemd/system/\$u"; done
rs -R --no-implied-dirs --checksum --out-format='%i %n' \$UL /etc/systemd/system/w20-record.service.d \
  /srv/w20/policy/hc_final.json /srv/w20/ref /srv/w20/scorecard/scorecard.py /srv/w20/w2-weather/*.py \
  /srv/w20/reward-pilot/rotate.py /etc/w20/live.env /etc/w20/reward-pilot/observer.env $bins \
  "ubuntu@\$NEWIP:/"
EOF
  out=$(hop <<<"$script") || die "code copy failed: $out"
  n=$(echo "$out" | grep -cE '^[<>ch.*][a-zA-Z.+]' || true); total=$((total + n))
  echo "$out" | grep -E '^[<>ch.*][a-zA-Z.+]' | grep -v ' /srv/w20/ref/fcst/' | sed 's/^/  /' || true
  echo "  ($n item(s) changed in the code step)"

  say "unit state step on $NEW"
  IFS= read -r -d '' script <<EOF || true
set -u
c=0
systemctl daemon-reload
for u in $ulist; do
  case \$(systemctl is-enabled "\$u" 2>&1) in enabled|enabled-runtime) systemctl disable "\$u" >/dev/null 2>&1; echo "changed: \$u disabled"; c=\$((c+1));; esac
  [ "\$(systemctl is-active "\$u")" = inactive ] || echo "WARN: \$u is \$(systemctl is-active "\$u")"
done
for u in $ulist; do
  f=/etc/systemd/system/\$u
  [ -e "\$f" ] || { echo "MISSING \$f"; continue; }
  if systemd-analyze verify "\$f" >/tmp/s3-verify.out 2>&1; then echo "verify rc=0 \$u"; else echo "verify rc=\$? \$u"; sed 's/^/    /' /tmp/s3-verify.out; fi
done
rm -f /tmp/s3-verify.out
echo "count \$c"
EOF
  out=$(newsh 'sudo bash -s' <<<"$script")
  echo "$out" | grep -v '^count' | sed 's/^/  /'
  n=$(echo "$out" | sed -nE 's/^count ([0-9]+)$/\1/p'); total=$((total + n))
  echo "$out" | grep -q 'verify rc=[1-9]' && die "systemd-analyze verify failed on $NEW, see above"
  echo "$out" | grep -q '^MISSING' && die "a unit file is missing on $NEW"
  say "weather-live: $total change(s)"
}

# ---- sync: state and research, niced, bot running. The dry run records the delta.
# The tree list is shared by sync and verify, so what is copied is exactly what is checked.
pairs_def() {
  cat <<'EOF'
pairs=(
  "/srv/w20/data/|/srv/w20/data/"
  "/srv/w20/out/|/srv/w20/out/"
  "/srv/w20/w2-weather/data/|/srv/w20/w2-weather/data/"
  "/srv/w20/w2-weather/recorder.log|/srv/w20/w2-weather/recorder.log"
  "/srv/w20/band/|/srv/w20/band/"
  "/srv/w20/scorecard/|/srv/w20/scorecard/"
  "/srv/w20/reward-pilot/archive/|/srv/w20/reward-pilot/archive/"
  "/var/lib/private/weather-reward-pilot/|/var/lib/private/weather-reward-pilot/"
  "/var/lib/weather-reward-pilot-rotate/|/var/lib/weather-reward-pilot-rotate/|--exclude=segments/ --exclude=rotate.lock"
  "/var/lib/weather-reward-pilot.rotate-hold/|/var/lib/weather-reward-pilot.rotate-hold/"
  "/var/lib/weather-reward-pilot-rotate/segments/|/var/lib/weather-reward-pilot-rotate/segments/"
)
for d in /srv/w20/behind*/; do pairs+=("$d|$d"); done
EOF
}

sync_body() {
  cat <<'EOF'
FLAGS="-i --out-format=%i_%n --stats --inplace --bwlimit=$BW --exclude=*.pid"
[ "$DRY" = 1 ] && FLAGS="$FLAGS -n"
bad=0; ran=0
for p in "${pairs[@]}"; do
  IFS='|' read -r src dst extra <<<"$p"
  [ -z "$ONLY" ] || [ "${dst%/}" = "${ONLY%/}" ] || continue
  [ -e "$src" ] || { echo "skip (absent on old box) $src"; continue; }
  ran=$((ran + 1))
  out=$(rs $FLAGS $extra "$src" "ubuntu@$NEWIP:$dst" 2>&1); rc=$?
  files=$(echo "$out" | grep -cE '^[<>ch.*][a-zA-Z.+?]{10}_' || true)
  n=$(echo "$out" | sed -nE 's/^Number of regular files transferred: ([0-9,]+)/\1/p')
  b=$(echo "$out" | sed -nE 's/^Total transferred file size: ([0-9,]+) bytes/\1/p')
  printf '  %-62s rc=%s changed=%s files=%s bytes=%s\n' "$dst" "$rc" "$files" "${n:-0}" "${b:-0}"
  if [ $rc -ne 0 ]; then
    case $rc in 124 | 137) echo "    TIMEOUT: no finish within ${PT}s (S3_PAIR_TIMEOUT); rsync killed" ;; esac
    echo "$out" | tail -3 | sed 's/^/    /'
    bad=1
  fi
done
if [ -n "$ONLY" ] && [ $ran -eq 0 ]; then
  echo "no tree named $ONLY on the old box; trees:"
  for p in "${pairs[@]}"; do echo "    ${p#*|}" | cut -d'|' -f1; done
  exit 3
fi
[ $bad -eq 0 ] || echo "sync: at least one tree failed"
exit $bad
EOF
}

cmd_sync() {
  local dry=0 only=
  while (($#)); do
    case $1 in
      --dry-run) dry=1 ;;
      --pair) [[ -n ${2:-} ]] || die "sync --pair needs a destination path"; only=$2; shift ;;
      *) die "sync: unknown argument $1" ;;
    esac
    shift
  done
  say "sync $([[ $dry == 1 ]] && echo dry-run || echo copy)${only:+ of $only only} $OLD -> $NEW (bwlimit ${BWLIMIT} KB/s, nice 19, idle io, ${PAIR_TIMEOUT}s per tree)"
  local script
  script="$(printf 'DRY=%s\nONLY=%q\n' "$dry" "$only")
$(pairs_def)
$(sync_body)"
  hop <<<"$script" || { echo "s3-weather: sync failed, no stamp written" >&2; return 1; }
  if [[ $dry == 0 && -z $only ]]; then
    newsh 'date +%s | sudo tee /srv/w20/.s3-sync-epoch >/dev/null' || { echo "s3-weather: sync copied but the stamp write failed" >&2; return 1; }
    say "sync stamp written on $NEW"
  elif [[ $dry == 0 ]]; then
    say "one tree copied; no stamp (a stamp needs a full sync)"
  fi
}

# verify: every file of every copied tree, path and size and mtime, old box against new box. Extra files on the new box are counted, not failed.
verify_body() {
  cat <<'EOF'
MFPY=$(cat <<'PY'
import os, sys
t, ex = sys.argv[1], sys.argv[2].split()
names = {x[10:] for x in ex if x.startswith("--exclude=") and not x.endswith("/")}
prefixes = [x[10:] for x in ex if x.startswith("--exclude=") and x.endswith("/")]
rows = []
if os.path.isdir(t):
    for d, _, fs in os.walk(t):
        for f in fs:
            p = os.path.normpath(os.path.join(os.path.relpath(d, t), f))
            if f.endswith(".pid") or f in names or any(p.startswith(q) for q in prefixes):
                continue
            st = os.lstat(os.path.join(d, f))
            rows.append("%s\t%d\t%d" % (p, st.st_size, int(st.st_mtime)))
else:
    st = os.lstat(t)
    rows.append("%s\t%d\t%d" % (os.path.basename(t), st.st_size, int(st.st_mtime)))
print("\n".join(sorted(rows)))
PY
)
bad=0; trees=0
for p in "${pairs[@]}"; do
  IFS='|' read -r src dst extra <<<"$p"
  [ -e "$src" ] || continue
  trees=$((trees + 1))
  o=$(python3 -I - "$src" "$extra" <<<"$MFPY") || { echo "  FAIL $dst: manifest unreadable on the old box"; bad=1; continue; }
  n=$("${SSHN[@]}" "ubuntu@$NEWIP" "sudo python3 -I - $(printf %q "$dst") $(printf %q "$extra")" <<<"$MFPY") || { echo "  FAIL $dst: manifest unreadable on the new box (tree missing or no answer)"; bad=1; continue; }
  miss=$(LC_ALL=C comm -23 <(printf '%s\n' "$o") <(printf '%s\n' "$n") | grep . || true)
  nmiss=$(printf '%s\n' "$miss" | grep -c . || true)
  nold=$(printf '%s\n' "$o" | grep -c . || true)
  extras=$(LC_ALL=C comm -13 <(printf '%s\n' "$o" | cut -f1 | LC_ALL=C sort) <(printf '%s\n' "$n" | cut -f1 | LC_ALL=C sort) | grep -c . || true)
  printf '  %-62s files=%s missing_or_different=%s extra_on_new=%s\n' "$dst" "$nold" "$nmiss" "$extras"
  if [ "$nmiss" -ne 0 ]; then printf '%s\n' "$miss" | head -3 | sed 's/^/    old has: /'; bad=1; fi
done
[ $trees -gt 0 ] || { echo "  FAIL no tree found on the old box"; bad=1; }
[ $bad -eq 0 ] && echo "verify: $trees tree(s) identical (path, size, mtime)" || echo "verify: MISMATCH"
exit $bad
EOF
}

cmd_verify() {
  say "verify: manifest of every copied tree, $OLD against $NEW"
  local script
  script="$(pairs_def)
$(verify_body)"
  hop <<<"$script"
}

# ---- checks shared by preflight and switch
BLOCKERS=()
block() { BLOCKERS+=("$1"); echo "  BLOCKER $1"; }
okl() { echo "  ok      $1"; }

geoblock() { # geoblock <alias>: the box's own read of the venue's geoblock endpoint
  ssh -o BatchMode=yes "$1" 'curl -s --max-time 15 https://polymarket.com/api/geoblock'
}

quotes_check() { # quotes_check <alias> <since "YYYY-MM-DD HH:MM:SS"> <wait secs> <sudo or empty>
  ssh -o BatchMode=yes "$1" "${4:+$4 }bash -s -- '$2' '$3'" <<'EOF'
since=$1; wait=$2
iso=$(date -u -d "$since" +%Y-%m-%dT%H:%M:%S)
end=$((SECONDS + wait))
while :; do
  line=$(journalctl -u w20-live --since "$since" --no-pager -o cat 2>/dev/null | grep 'probe: run=' | tail -1)
  rest=$(echo "$line" | sed -nE 's/.*resting_usd=([0-9.]+).*/\1/p')
  stats=$(tail -n 30000 /srv/w20/out/live/venue.journal.jsonl 2>/dev/null | python3 -c '
import sys, json, re
iso = sys.argv[1]; posts = fills = 0
for l in sys.stdin:
    try: d = json.loads(l)
    except ValueError: continue
    if d.get("at", "") < iso: continue
    k = d.get("key", "")
    if k.startswith("post/"): posts += 1
    if re.search(r"fill|trade|matched|mined", k, re.I) and not k.startswith("reward/"): fills += 1
print(posts, fills)' "$iso")
  posts=${stats% *}; fills=${stats#* }
  active=$(systemctl is-active w20-live)
  if [ "$active" = active ] && [ -n "$rest" ] && awk -v r="$rest" 'BEGIN{exit !(r > 0)}' && [ "${posts:-0}" -ge 1 ]; then
    echo "QUOTES_OK unit=$active resting_usd=$rest posts_since_start=$posts fill_rows_since_start=$fills"
    exit 0
  fi
  [ "$active" = active ] || { echo "QUOTES_FAIL unit=$active"; journalctl -u w20-live --since "$since" --no-pager -o cat | tail -8 | cut -c1-200; exit 1; }
  [ $SECONDS -ge $end ] && { echo "QUOTES_FAIL unit=$active resting_usd=${rest:-none} posts_since_start=${posts:-0} fill_rows=${fills:-0} after ${wait}s"; exit 1; }
  sleep 10
done
EOF
}

cmd_preflight() {
  local u out a b n
  echo "S3 preflight $(date -u +%FT%TZ) old=$OLD new=$NEW"
  echo "architecture"
  a=$(oldsh uname -m); b=$(newsh uname -m)
  if [[ $a == "$b" ]]; then okl "both $a"; else block "architecture differs: $OLD $a, $NEW $b; binaries cannot run as copied"; fi
  for f in $(old_bins | grep -E '/bin/w20|rewardcollect'); do
    m=$(newsh "od -An -tx1 -j18 -N2 $f 2>/dev/null | tr -d ' \n'") || m=
    if [[ $m == b700 ]]; then okl "$f is an aarch64 ELF on $NEW"; else block "$f missing or not aarch64 on $NEW (run prepare)"; fi
  done

  echo "old units (must be active)"
  for u in "${WATCHED_OLD[@]}"; do
    a=$(oldsh "systemctl is-active $u" || true)
    if [[ $a == active ]]; then okl "$OLD $u active"; else block "$OLD $u is $a, preflight needs the old bot running"; fi
  done
  if oldsh 'test ! -e /etc/systemd/system/w20-live.service.d/zz-s3-moved.conf'; then okl "old w20-live not already fenced"; else block "old w20-live carries the S3 fence drop-in (a switch ran; use back)"; fi

  echo "new units (installed, disabled, inactive)"
  for u in "${UNITS[@]}"; do
    out=$(newsh "systemctl is-enabled $u 2>&1; systemctl is-active $u 2>&1" | tr '\n' ' ' || true)
    case $out in
      "enabled "* | "enabled-runtime "*) block "$NEW $u enabled" ;;
      *"inactive "*) okl "$NEW $u: $out" ;;
      *) block "$NEW $u: $out" ;;
    esac
  done
  # shellcheck disable=SC2016
  out=$(newsh 'for u in '"${UNITS[*]}"'; do sudo systemd-analyze verify /etc/systemd/system/$u >/dev/null 2>&1 || echo "$u"; done')
  if [[ -z $out ]]; then okl "systemd-analyze verify rc=0 on all ${#UNITS[@]} units"; else block "systemd-analyze verify fails: $out"; fi

  echo "copies match (sha256 by name)"
  local list="/etc/w20/live.env /etc/w20/reward-pilot/observer.env /srv/w20/policy/hc_final.json /srv/w20/reward-pilot/rotate.py /srv/w20/reward-pilot/config.json /srv/w20/scorecard/scorecard.py /srv/w20/w2-weather/recorder.py /etc/systemd/system/w20-record.service.d/weather-ahead.conf"
  for u in "${UNITS[@]}"; do list="$list /etc/systemd/system/$u"; done
  list="$list $(old_bins | tr '\n' ' ')"
  a=$(oldsh "sha256sum $list 2>&1" | sort -k2)
  b=$(newsh "sudo sha256sum $list 2>&1" | sort -k2)
  if [[ $a == "$b" ]]; then
    n=$(echo "$a" | wc -l | tr -d ' '); okl "$n files identical on both boxes (units, binaries, scripts, policy, env files)"
  else
    diff <(echo "$a" | awk '{print $2, $1}') <(echo "$b" | awk '{print $2, $1}') | grep '^[<>]' | awk '{print $2}' | sort -u | while read -r f; do echo "          differs: $f"; done
    block "copies differ from the old box (a lineage change since the last prepare?): run prepare"
  fi
  if oldsh "grep -q '^ARM=yes' /etc/w20/live.env" && newsh "sudo grep -q '^ARM=yes' /etc/w20/live.env"; then okl "ARM=yes in both live.env (gate 1)"; else block "ARM=yes missing from a live.env"; fi
  if newsh 'test ! -e /srv/w20/out/KILL'; then okl "no KILL file on $NEW"; else block "KILL file exists on $NEW"; fi

  echo "venue geoblock from $NEW's own address"
  out=$(geoblock "$NEW" || true)
  if echo "$out" | grep -q '"blocked":false'; then okl "$(echo "$out" | sed -E 's/"ip":"[^"]*",?//')"; else block "geoblock read from $NEW: ${out:-no answer}"; fi

  echo "disk, clock"
  out=$(newsh "df -B1G --output=avail /srv/w20 | tail -n1 | tr -d ' '")
  if [[ $out -ge 20 ]]; then okl "${out} GiB free on $NEW (the collector and w2-weather refuse below 15)"; else block "only ${out} GiB free on $NEW"; fi
  if newsh 'timedatectl show -p NTPSynchronized --value' | grep -q yes; then okl "clock synchronized on $NEW"; else block "clock not synchronized on $NEW"; fi

  echo "state copy"
  out=$(newsh 'cat /srv/w20/.s3-sync-epoch 2>/dev/null' || true)
  if [[ -z $out ]]; then block "no sync stamp on $NEW: run sync"
  elif (($(date +%s) - out > STALE_SECS)); then block "last sync was $((($(date +%s) - out) / 60)) min ago (limit $((STALE_SECS / 60))): run sync"
  else okl "last sync $((($(date +%s) - out) / 60)) min ago"; fi

  echo "owed from others"
  if [[ -s $PROOF ]]; then okl "resting-order proof on file: $PROOF"; else block "resting-order proof from $NEW not on file ($PROOF); owed by the peer session, not this lane"; fi

  echo
  if ((${#BLOCKERS[@]})); then
    echo "NOT READY: ${#BLOCKERS[@]} blocker(s)"
    printf '  - %s\n' "${BLOCKERS[@]}"
    return 1
  fi
  echo "READY: old units active, new units installed disabled, copies identical, geoblock false, state fresh"
}

# ---- switch and back
stop_old() {
  say "fencing and stopping the old units on $OLD (w20-live first)"
  oldsh 'bash -s' <<'EOF'
set -u
mkdir -p /etc/systemd/system/w20-live.service.d
printf '[Unit]\nConditionPathExists=/nonexistent-moved-to-aws\n' >/etc/systemd/system/w20-live.service.d/zz-s3-moved.conf
systemctl daemon-reload
systemctl stop w20-live.service
systemctl disable --now w20-scorecard.timer weather-reward-pilot-rotate.timer >/dev/null 2>&1
systemctl stop weather-reward-pilot-rotate.service w20-scorecard.service weather-reward-pilot.service
systemctl disable --now w2-weather.service w20-record.service >/dev/null 2>&1
bad=0
for u in w20-live w20-record w2-weather w20-scorecard.timer weather-reward-pilot-rotate.timer weather-reward-pilot weather-reward-pilot-rotate w20-scorecard; do
  a=$(systemctl is-active "$u"); e=$(systemctl is-enabled "$u" 2>&1)
  echo "  $u active=$a enabled=$e"
  [ "$a" = active ] || [ "$a" = activating ] && bad=1
  [ "$e" = enabled ] && bad=1
done
pgrep -af '/srv/w20/bin/w20|rewardcollect' && bad=1
exit $bad
EOF
}

stop_new() {
  say "stopping and disabling the new units on $NEW (w20-live first)"
  newsh 'sudo bash -s' <<'EOF'
set -u
systemctl stop w20-live.service
systemctl disable --now w20-scorecard.timer weather-reward-pilot-rotate.timer >/dev/null 2>&1
systemctl stop weather-reward-pilot-rotate.service w20-scorecard.service weather-reward-pilot.service
systemctl disable --now w2-weather.service w20-record.service >/dev/null 2>&1
bad=0
for u in w20-live w20-record w2-weather w20-scorecard.timer weather-reward-pilot-rotate.timer weather-reward-pilot; do
  a=$(systemctl is-active "$u"); e=$(systemctl is-enabled "$u" 2>&1)
  echo "  $u active=$a enabled=$e"
  [ "$a" = active ] || [ "$a" = activating ] && bad=1
  [ "$e" = enabled ] && bad=1
done
pgrep -af '/srv/w20/bin/w20|rewardcollect' && bad=1
exit $bad
EOF
}

start_new() {
  newsh 'sudo bash -s' <<'EOF'
set -e
systemctl daemon-reload
systemctl enable --now w2-weather.service w20-record.service
systemctl enable --now w20-scorecard.timer weather-reward-pilot-rotate.timer
EOF
}

start_old() {
  oldsh 'bash -s' <<'EOF'
set -e
rm -f /etc/systemd/system/w20-live.service.d/zz-s3-moved.conf
rmdir /etc/systemd/system/w20-live.service.d 2>/dev/null || true
systemctl daemon-reload
systemctl enable --now w2-weather.service w20-record.service
systemctl enable --now w20-scorecard.timer weather-reward-pilot-rotate.timer
EOF
}

copy_final() { # copy_final <new-to-old|old-to-new>
  local dir=$1
  if [[ $dir == old-to-new ]]; then
    cmd_sync
  else
    say "copying state from $NEW back to $OLD"
    hop <<'EOF'
pl() { nice -n 19 ionice -c 3 timeout -k 20 "$PT" rsync -a --numeric-ids --timeout=300 --inplace --exclude='*.pid' --rsync-path="sudo rsync" -e "$RSH" "$@"; }
bad=0
for p in /srv/w20/data /srv/w20/out /srv/w20/w2-weather/data /srv/w20/band /srv/w20/scorecard /var/lib/private/weather-reward-pilot /var/lib/weather-reward-pilot-rotate; do
  extra=""; [ "$p" = /var/lib/weather-reward-pilot-rotate ] && extra="--exclude=segments/ --exclude=rotate.lock"
  pl $extra "ubuntu@$NEWIP:$p/" "$p/" || { echo "pull failed: $p"; bad=1; }
done
pl "ubuntu@$NEWIP:/srv/w20/w2-weather/recorder.log" /srv/w20/w2-weather/recorder.log || bad=1
exit $bad
EOF
  fi
}

cmd_switch() {
  [[ ${1:-} == --go ]] || die "switch needs --go (owner's go; the peer session that owns w20-live must be present)"
  say "1/7 preflight"
  cmd_preflight || die "preflight failed, nothing was touched"
  say "2/7 pre-pass copy with the old bot still running"
  cmd_sync || die "pre-pass copy failed, nothing was touched"
  say "3/7 stop and disable the old units"
  stop_old || die "old units not fully stopped and disabled; nothing was started on $NEW. Old state needs a look; 'back --go' restores"
  say "4/7 final copy with everything stopped"
  copy_final old-to-new || die "final copy failed; old units are stopped. Run 'back --go' or fix and rerun the copy"
  say "5/7 cross-check: every copied tree by path, size and mtime, then the files that matter by sha256"
  cmd_verify || die "manifest mismatch after final copy; old units are stopped, nothing started on $NEW. Run 'back --go' or fix and rerun the copy"
  local list="/srv/w20/out/live/venue.journal.jsonl /srv/w20/out/live/merge.journal.jsonl /srv/w20/out/live/redeem.journal.jsonl /srv/w20/w2-weather/data/state.db /etc/w20/live.env /srv/w20/bin/w20"
  [[ $(oldsh "sha256sum $list" | sort -k2) == "$(newsh "sudo sha256sum $list" | sort -k2)" ]] || die "sha256 mismatch after final copy; old units are stopped, nothing started on $NEW. Run 'back --go'"
  echo "  identical: $list"
  say "6/7 geoblock, then start the new units"
  geoblock "$NEW" | grep -q '"blocked":false' || die "geoblock true from $NEW now; nothing started there. Run 'back --go'"
  start_new
  local since
  since=$(newsh 'date -u "+%F %T"')
  newsh 'sudo systemctl start w20-live.service' || { newsh 'sudo journalctl -u w20-live --no-pager -n 15 -o cat | cut -c1-200'; die "w20-live did not start on $NEW. Run 'back --go'"; }
  newsh 'sudo systemctl start --no-block weather-reward-pilot.service' || true
  say "7/7 quotes and fills in the journal from $NEW ($(geoblock "$NEW" | sed -nE 's/.*"country":"([^"]*)".*/\1/p'), $NEWIP), up to ${QUOTE_WAIT}s"
  quotes_check "$NEW" "$since" "$QUOTE_WAIT" sudo || die "no quotes seen on $NEW. Decide: wait, or 'back --go'"
  say "switch done; the bot runs on $NEW. Undo with: s3-weather.sh back --go"
}

cmd_back() {
  [[ ${1:-} == --go ]] || die "back needs --go"
  say "1/4 stop and disable the new units"
  stop_new || die "new units not fully stopped; the old units were not started, so two quoters cannot run"
  say "2/4 copy state home"
  copy_final new-to-old || die "copy home failed; both sides stopped. Fix and rerun back --go"
  say "3/4 start the old units"
  start_old
  local since
  since=$(oldsh 'date -u "+%F %T"')
  oldsh 'systemctl start w20-live.service' || die "old w20-live did not start"
  oldsh 'systemctl start --no-block weather-reward-pilot.service' || true
  say "4/4 quotes and fills from $OLD"
  quotes_check "$OLD" "$since" "$QUOTE_WAIT" "" || die "no quotes seen on $OLD"
  say "back done; the bot runs on $OLD again"
}

main() {
  local cmd=${1:-}
  [[ $# -gt 0 ]] && shift
  case $cmd in
    prepare | sync | verify | preflight | switch | back) ;;
    *) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
  esac
  mkdir /tmp/s3-weather.lock 2>/dev/null || die "another s3-weather.sh run holds /tmp/s3-weather.lock"
  trap cleanup EXIT
  case $cmd in
    prepare) cmd_prepare ;;
    sync) cmd_sync "$@" ;;
    verify) cmd_verify ;;
    preflight) cmd_preflight ;;
    switch) cmd_switch "$@" ;;
    back) cmd_back "$@" ;;
  esac
}
main "$@"
