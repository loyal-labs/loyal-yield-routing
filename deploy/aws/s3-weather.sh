#!/usr/bin/env bash
# Switch S3: the live weather bot moves from Hetzner loyal-eu-1 to the AWS box weather-live (Stockholm).
#   s3-weather.sh prepare            install users, dirs, units (disabled), binaries and env on weather-live; a rerun prints "0 change(s)"
#   s3-weather.sh sync [--dry-run] [--checksum] [--pair DST]   niced copy that mirrors the old box (files gone there are deleted on
#                                    the new box); --dry-run records the delta; --checksum compares content, not size and mtime;
#                                    --pair DST copies one tree (no stamp); a full sync that fails in any pair writes no stamp
#   s3-weather.sh verify             read-only manifest of every copied tree, old box against new box: path set (extra files fail),
#                                    size, mtime and sha256 of every file (S3_VERIFY_HASH=start skips the sha256 of the two history
#                                    trees, data/ and w2-weather/data/ except state.db*, and then proves only path, size and mtime there)
#   s3-weather.sh preflight          read-only; exit 0 only when ready and the old units are active
#   s3-weather.sh switch --go        stop and disable the old units, final copy, start the new ones, check quotes and fills
#   s3-weather.sh back --go          the reverse: stop and disable the new units, copy state home only when the owner marker on the new
#                                    box says AWS started (every tree of the forward copy, then the same manifest gate), start the old ones
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
VERIFY_HASH=${S3_VERIFY_HASH:-all}
EXTRA_OK=${S3_EXTRA_OK:-}
OWNER_FILE=/srv/w20/.s3-owner
HERE=$(cd "$(dirname "$0")" && pwd)

UNITS=(w20-live.service w20-record.service w2-weather.service w20-scorecard.service w20-scorecard.timer
  weather-reward-pilot-rotate.service weather-reward-pilot-rotate.timer weather-reward-pilot.service)
WATCHED_OLD=(w20-live.service w20-record.service w2-weather.service w20-scorecard.timer weather-reward-pilot-rotate.timer)

die() { echo "s3-weather: $*" >&2; exit 1; }
say() { echo "s3-weather: $*"; }

oldsh() { ssh -o BatchMode=yes -o ConnectTimeout=20 "$OLD" "$@"; }
newsh() { ssh -o BatchMode=yes -o ConnectTimeout=20 "$NEW" "$@"; }

# rd old|new COMMAND...: the one way a check reads a box. It fails when ssh or the command exits non-zero and when the answer is
# empty, so a failed read is never an empty value that compares equal to another or reads as "nothing wrong". A command whose
# honest answer can be empty or non-zero ends in an echo or "|| true" so it still prints something.
rd() {
  local side=$1 out
  shift
  if [[ $side == old ]]; then out=$(oldsh "$@") || return 1; else out=$(newsh "$@") || return 1; fi
  [[ -n $out ]] || return 1
  printf '%s\n' "$out"
}

# sha_set old|new FILE...: sorted "sha256  path" lines, exactly one per file; fails unless every file was read
sha_set() {
  local side=$1 out cmd="sha256sum"
  shift
  [[ $side == new ]] && cmd="sudo sha256sum"
  out=$(rd "$side" "$cmd $*") || return 1
  [[ $(grep -cE '^[0-9a-f]{64}  /' <<<"$out") -eq $# && $(wc -l <<<"$out" | tr -d ' ') -eq $# ]] || return 1
  sort -k2 <<<"$out"
}

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
  rd old "cd /etc/systemd/system && grep -ohE '/srv/w20/(bin|reward-pilot)/[A-Za-z0-9._-]+' ${UNITS[*]} w20-record.service.d/*.conf | sort -u"
}

# owner_marker: prints owned or never from the last line of the answer, so a banner before it is tolerated; fails when the box
# cannot be read or the last line is neither word
owner_marker() {
  local out
  out=$(rd new "if [ -e $OWNER_FILE ]; then echo owned; else echo never; fi") || return 1
  out=$(tail -n1 <<<"$out")
  [[ $out == owned || $out == never ]] || return 1
  echo "$out"
}

refuse_if_aws_owns() { # refuse_if_aws_owns COMMAND: a copy toward $NEW must never run over data the new box has taken writes to
  local m
  m=$(owner_marker) || die "$1 refused: the owner marker on $NEW cannot be read, so it is not known whether AWS owns the data. Nothing was copied"
  [[ $m == never ]] || die "$1 refused: the owner marker on $NEW says AWS owns the data, and a copy toward it would overwrite what it holds. Use back --go to bring the data home"
}

# ---- prepare: host convergence, then units, binaries, env and scripts copied from the old box
cmd_prepare() {
  local total=0 n out script ulist=${UNITS[*]}
  refuse_if_aws_owns prepare
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
FLAGS="-i --out-format=%i_%n --stats --inplace --delete --bwlimit=$BW --exclude=*.pid"
[ "$DRY" = 1 ] && FLAGS="$FLAGS -n"
[ "$SUM" = 1 ] && FLAGS="$FLAGS --checksum"
bad=0; ran=0
for p in "${pairs[@]}"; do
  IFS='|' read -r src dst extra <<<"$p"
  [ -z "$ONLY" ] || [ "${dst%/}" = "${ONLY%/}" ] || continue
  if [ "$DIR" = pull ]; then
    "${SSHN[@]}" "ubuntu@$NEWIP" "sudo test -e '$src'" </dev/null; e=$?
    case $e in
      0) ;;
      1) echo "skip (absent on new box) $src"; continue ;;
      *) echo "  FAIL cannot look for $src on the new box (ssh exit $e)"; bad=1; continue ;;
    esac
  else
    [ -e "$src" ] || { echo "skip (absent on old box) $src"; continue; }
  fi
  ran=$((ran + 1))
  if [ "$DIR" = pull ]; then out=$(rs $FLAGS $extra "ubuntu@$NEWIP:$src" "$dst" 2>&1); rc=$?
  else out=$(rs $FLAGS $extra "$src" "ubuntu@$NEWIP:$dst" 2>&1); rc=$?; fi
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
  local dry=0 only='' sum=0
  refuse_if_aws_owns sync
  while (($#)); do
    case $1 in
      --dry-run) dry=1 ;;
      --checksum) sum=1 ;;
      --pair) [[ -n ${2:-} ]] || die "sync --pair needs a destination path"; only=$2; shift ;;
      *) die "sync: unknown argument $1" ;;
    esac
    shift
  done
  say "sync $([[ $dry == 1 ]] && echo dry-run || echo copy)${only:+ of $only only} $OLD -> $NEW (bwlimit ${BWLIMIT} KB/s, nice 19, idle io, ${PAIR_TIMEOUT}s per tree)"
  local script
  script="$(printf 'DIR=push\nDRY=%s\nSUM=%s\nONLY=%q\n' "$dry" "$sum" "$only")
$(pairs_def)
$(sync_body)"
  # a copy that changes files must not leave an earlier stamp standing as proof the tree is fresh (S2-15)
  if [[ $dry == 0 && -z $only ]]; then newsh 'sudo rm -f /srv/w20/.s3-sync-epoch' || die "could not clear the old sync stamp on $NEW"; fi
  hop <<<"$script" || { echo "s3-weather: sync failed, no stamp written" >&2; return 1; }
  if [[ $dry == 0 && -z $only ]]; then
    newsh 'date +%s | sudo tee /srv/w20/.s3-sync-epoch >/dev/null' || { echo "s3-weather: sync copied but the stamp write failed" >&2; return 1; }
    say "sync stamp written on $NEW"
  elif [[ $dry == 0 ]]; then
    newsh 'sudo rm -f /srv/w20/.s3-sync-epoch' || die "could not clear the old sync stamp on $NEW"
    say "one tree copied; the sync stamp is cleared (a stamp needs a full sync)"
  fi
}

# verify: every file of every copied tree, old box against new box: the path sets must be equal (a file only on the new box fails
# unless S3_EXTRA_OK, an anchored regex on dst+path, allows it), and size and mtime and, for the state the bot reads at start, the
# sha256 of every file by default (S3_VERIFY_HASH=all). rsync's quick check trusts equal size and mtime, so a copy that differs in
# bytes only is caught here and nowhere else; S3_VERIFY_HASH=start skips the sha256 of the two history trees (data/ 2.9 GB,
# w2-weather/data/ except state.db* 1.9 GB on Nov 18 2026) and then proves only path, size and mtime for them.
verify_body() {
  cat <<'EOF'
MFPY=$(cat <<'PY'
import hashlib, os, sys
t, ex, mode = sys.argv[1], sys.argv[2].split(), sys.argv[3]
names = {x[10:] for x in ex if x.startswith("--exclude=") and not x.endswith("/")}
prefixes = [x[10:] for x in ex if x.startswith("--exclude=") and x.endswith("/")]
def digest(p, f):
    if mode != "all" and (t == "/srv/w20/data/" or (t == "/srv/w20/w2-weather/data/" and not f.startswith("state.db"))):
        return "-"
    h = hashlib.sha256()
    with open(p, "rb") as fh:
        for blk in iter(lambda: fh.read(1 << 20), b""):
            h.update(blk)
    return h.hexdigest()
rows = []
if os.path.isdir(t):
    for d, _, fs in os.walk(t):
        for f in fs:
            p = os.path.normpath(os.path.join(os.path.relpath(d, t), f))
            if f.endswith(".pid") or f in names or any(p.startswith(q) for q in prefixes):
                continue
            fp = os.path.join(d, f)
            st = os.lstat(fp)
            rows.append("%s\t%d\t%d\t%s" % (p, st.st_size, int(st.st_mtime), digest(fp, f) if os.path.isfile(fp) and not os.path.islink(fp) else "-"))
elif os.path.lexists(t):
    st = os.lstat(t)
    rows.append("%s\t%d\t%d\t%s" % (os.path.basename(t), st.st_size, int(st.st_mtime), digest(t, os.path.basename(t))))
else:
    rows.append("__ABSENT__")
print("\n".join(sorted(rows)))
PY
)
bad=0; trees=0
for p in "${pairs[@]}"; do
  IFS='|' read -r src dst extra <<<"$p"
  o=$(python3 -I - "$src" "$extra" "$HASHMODE" <<<"$MFPY") || { echo "  FAIL $dst: manifest unreadable on the old box"; bad=1; continue; }
  n=$("${SSHN[@]}" "ubuntu@$NEWIP" "sudo python3 -I - $(printf %q "$dst") $(printf %q "$extra") $(printf %q "$HASHMODE")" <<<"$MFPY") || { echo "  FAIL $dst: manifest unreadable on the new box (no answer)"; bad=1; continue; }
  [ "$o" != __ABSENT__ ] && trees=$((trees + 1))
  miss=$(LC_ALL=C comm -23 <(printf '%s\n' "$o") <(printf '%s\n' "$n") | grep . || true)
  nmiss=$(printf '%s\n' "$miss" | grep -c . || true)
  nold=$(printf '%s\n' "$o" | grep -c . || true)
  extra_list=$(LC_ALL=C comm -13 <(printf '%s\n' "$o" | cut -f1 | LC_ALL=C sort) <(printf '%s\n' "$n" | cut -f1 | LC_ALL=C sort) | grep . || true)
  if [ -n "$EXTRA_OK" ] && [ -n "$extra_list" ]; then
    extra_list=$(while IFS= read -r x; do printf '%s\n' "$dst$x" | grep -qE "^($EXTRA_OK)$" || printf '%s\n' "$x"; done <<<"$extra_list")
  fi
  extras=$(printf '%s\n' "$extra_list" | grep -c . || true)
  printf '  %-62s files=%s missing_or_different=%s extra_on_new=%s\n' "$dst" "$nold" "$nmiss" "$extras"
  if [ "$nmiss" -ne 0 ]; then printf '%s\n' "$miss" | head -3 | cut -f1-3 | sed 's/^/    old has: /'; bad=1; fi
  if [ "$extras" -ne 0 ]; then printf '%s\n' "$extra_list" | head -3 | sed 's/^/    only on new: /'; echo "    (sync deletes it; or allow it with S3_EXTRA_OK)"; bad=1; fi
done
newdirs=$("${SSHN[@]}" "ubuntu@$NEWIP" 'for d in /srv/w20/behind*/; do [ -d "$d" ] && echo "$d"; done; true' </dev/null) || { echo "  FAIL cannot list /srv/w20/behind*/ on the new box (no answer)"; bad=1; newdirs=; }
for d in $newdirs; do
  known=0
  for p in "${pairs[@]}"; do [ "${p%%|*}" = "$d" ] && known=1; done
  [ $known = 1 ] || { echo "  FAIL $d exists on the new box and is not on the old box"; bad=1; }
done
[ $trees -gt 0 ] || { echo "  FAIL no tree found on the old box"; bad=1; }
if [ $bad -eq 0 ]; then
  if [ "$HASHMODE" = all ]; then echo "verify: $trees tree(s) identical (paths, size, mtime, sha256 of every file)"
  else echo "verify: $trees tree(s) identical (paths, size, mtime; sha256 only of the state read at start, NOT of data/ and w2-weather/data/ except state.db*)"; fi
else echo "verify: MISMATCH (a file with the same size and mtime but other bytes is fixed by: sync --checksum)"; fi
exit $bad
EOF
}

cmd_verify() {
  say "verify: manifest of every copied tree (hash: $VERIFY_HASH), $OLD against $NEW"
  local script
  script="$(printf 'HASHMODE=%q\nEXTRA_OK=%q\n' "$VERIFY_HASH" "$EXTRA_OK")
$(pairs_def)
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

# Every read goes through rd and every failed read becomes a blocker; nothing here relies on errexit, so the list is always reached,
# whether preflight runs alone or as the left side of an OR-list in switch (where errexit is off).
cmd_preflight() { ( set +e; preflight_checks ); }

presence() { # presence old|new PATH: prints present or absent; fails when the box could not be read
  rd "$1" "if [ -e '$2' ]; then echo present; else echo absent; fi"
}

preflight_checks() {
  local u out a b n f m bins list files
  echo "S3 preflight $(date -u +%FT%TZ) old=$OLD new=$NEW"
  echo "architecture"
  if a=$(rd old uname -m) && b=$(rd new uname -m); then
    if [[ $a == "$b" ]]; then okl "both $a"; else block "architecture differs: $OLD $a, $NEW $b; binaries cannot run as copied"; fi
  else
    block "architecture could not be read on both boxes ($OLD: ${a:-no answer}, $NEW: ${b:-no answer})"
  fi
  if bins=$(old_bins); then
    files=$(grep -E '/bin/w20|rewardcollect' <<<"$bins")
    [[ -n $files ]] || block "the old units name no /srv/w20/bin/w20 or rewardcollect binary; the aarch64 check has nothing to read"
    for f in $files; do
      if m=$(rd new "od -An -tx1 -j18 -N2 $f 2>/dev/null | tr -d ' \n'"); then
        if [[ $m == b700 ]]; then okl "$f is an aarch64 ELF on $NEW"; else block "$f is not aarch64 on $NEW (run prepare)"; fi
      else
        block "$f could not be read on $NEW (missing, or no answer; run prepare)"
      fi
    done
  else
    bins=
    block "the binary list could not be read from the old units on $OLD"
  fi

  echo "old units (must be active)"
  for u in "${WATCHED_OLD[@]}"; do
    if a=$(rd old "systemctl is-active $u || true"); then
      if [[ $a == active ]]; then okl "$OLD $u active"; else block "$OLD $u is $a, preflight needs the old bot running"; fi
    else
      block "$OLD $u: state could not be read"
    fi
  done
  if a=$(presence old /etc/systemd/system/w20-live.service.d/zz-s3-moved.conf); then
    if [[ $a == absent ]]; then okl "old w20-live not already fenced"; else block "old w20-live carries the S3 fence drop-in (a switch ran; use back)"; fi
  else
    block "the fence drop-in on $OLD could not be looked for"
  fi
  if a=$(presence new "$OWNER_FILE"); then
    if [[ $a == absent ]]; then okl "no AWS owner marker on $NEW"; else block "$NEW carries the AWS owner marker $OWNER_FILE (a switch started the new units; use back)"; fi
  else
    block "the owner marker on $NEW could not be looked for"
  fi

  echo "new units (installed, disabled, inactive)"
  for u in "${UNITS[@]}"; do
    if out=$(rd new "systemctl is-enabled $u 2>&1; systemctl is-active $u 2>&1; true"); then
      out="${out//$'\n'/ } "
      case $out in
        "enabled "* | "enabled-runtime "*) block "$NEW $u enabled" ;;
        *"inactive "*) okl "$NEW $u: $out" ;;
        *) block "$NEW $u: $out" ;;
      esac
    else
      block "$NEW $u: state could not be read"
    fi
  done
  # shellcheck disable=SC2016
  if out=$(rd new 'for u in '"${UNITS[*]}"'; do sudo systemd-analyze verify /etc/systemd/system/$u >/dev/null 2>&1 || echo "FAIL $u"; done; echo VERIFIED'); then
    if [[ $out == VERIFIED ]]; then okl "systemd-analyze verify rc=0 on all ${#UNITS[@]} units"; else block "systemd-analyze verify fails: $(grep '^FAIL' <<<"$out" | tr '\n' ' ')"; fi
  else
    block "systemd-analyze verify could not be run on $NEW"
  fi

  echo "copies match (sha256 by name)"
  list="/etc/w20/live.env /etc/w20/reward-pilot/observer.env /srv/w20/policy/hc_final.json /srv/w20/reward-pilot/rotate.py /srv/w20/reward-pilot/config.json /srv/w20/scorecard/scorecard.py /srv/w20/w2-weather/recorder.py /etc/systemd/system/w20-record.service.d/weather-ahead.conf"
  for u in "${UNITS[@]}"; do list="$list /etc/systemd/system/$u"; done
  list="$list $(tr '\n' ' ' <<<"$bins")"
  # shellcheck disable=SC2086
  if a=$(sha_set old $list) && b=$(sha_set new $list); then
    if [[ $a == "$b" ]]; then
      n=$(wc -l <<<"$a" | tr -d ' '); okl "$n files identical on both boxes (units, binaries, scripts, policy, env files)"
    else
      { diff <(awk '{print $2, $1}' <<<"$a") <(awk '{print $2, $1}' <<<"$b") | grep '^[<>]' | awk '{print $2}' | sort -u | while read -r f; do echo "          differs: $f"; done; } || true
      block "copies differ from the old box (a lineage change since the last prepare?): run prepare"
    fi
  else
    block "the checksums could not be read for every file on both boxes, so the copies are not proven equal"
  fi
  if a=$(rd old "grep -c '^ARM=yes' /etc/w20/live.env || true") && b=$(rd new "sudo grep -c '^ARM=yes' /etc/w20/live.env || true"); then
    if ((a > 0 && b > 0)); then okl "ARM=yes in both live.env (gate 1)"; else block "ARM=yes missing from a live.env"; fi
  else
    block "ARM=yes could not be read from both live.env files"
  fi
  if a=$(presence new /srv/w20/out/KILL); then
    if [[ $a == absent ]]; then okl "no KILL file on $NEW"; else block "KILL file exists on $NEW"; fi
  else
    block "the KILL file on $NEW could not be looked for"
  fi

  echo "venue geoblock from $NEW's own address"
  out=$(geoblock "$NEW" || true)
  if echo "$out" | grep -q '"blocked":false'; then okl "$(echo "$out" | sed -E 's/"ip":"[^"]*",?//')"; else block "geoblock read from $NEW: ${out:-no answer}"; fi

  echo "disk, clock"
  if out=$(rd new "df -B1G --output=avail /srv/w20 | tail -n1 | tr -d ' '") && [[ $out =~ ^[0-9]+$ ]]; then
    if ((out >= 20)); then okl "${out} GiB free on $NEW (the collector and w2-weather refuse below 15)"; else block "only ${out} GiB free on $NEW"; fi
  else
    block "the free disk on $NEW could not be read"
  fi
  if out=$(rd new 'timedatectl show -p NTPSynchronized --value') && [[ $out == yes ]]; then okl "clock synchronized on $NEW"; else block "clock not synchronized on $NEW (${out:-no answer})"; fi

  echo "state copy"
  if out=$(rd new 'cat /srv/w20/.s3-sync-epoch 2>/dev/null') && [[ $out =~ ^[0-9]+$ ]]; then
    if (($(date +%s) - out > STALE_SECS)); then block "last sync was $((($(date +%s) - out) / 60)) min ago (limit $((STALE_SECS / 60))): run sync"
    else okl "last sync $((($(date +%s) - out) / 60)) min ago"; fi
  else
    block "no readable sync stamp on $NEW: run sync"
  fi

  echo "owed from others"
  if [[ -s $PROOF ]]; then okl "resting-order proof on file: $PROOF"
  else block "PARKED resting-order proof from $NEW, not on file ($PROOF). No order-free proof exists (an unsigned POST gets 401 from every region, a signed FAK never rests). One order from the wallet is needed, so only the owner can allow it. The command that produces it is: $0 switch --go (step 7 reads resting_usd > 0 from $NEW's own venue journal and fails into back); save that line to the file"; fi

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

# copy_home: the reverse of the forward sync. The same tree list (archive, segments and behind*/ included), the same rsync flags with
# --delete so the old box ends equal to the new one, and the same manifest gate with sha256 of every file. Only back calls it, and
# only when the owner marker says AWS started.
copy_home() {
  say "copying state from $NEW back to $OLD (every tree the forward copy mirrors)"
  local script
  script="$(printf 'DIR=pull\nDRY=0\nSUM=0\nONLY=\n')
$(pairs_def)
$(sync_body)"
  hop <<<"$script" || return 1
  cmd_verify
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
  cmd_sync || die "final copy failed; old units are stopped. Run 'back --go' or fix and rerun the copy"
  say "5/7 cross-check: every copied tree by path set, size, mtime and the start state by sha256, then the files that matter"
  cmd_verify || die "manifest mismatch after final copy; old units are stopped, nothing started on $NEW. Run 'back --go' or fix and rerun the copy"
  local list="/srv/w20/out/live/venue.journal.jsonl /srv/w20/out/live/merge.journal.jsonl /srv/w20/out/live/redeem.journal.jsonl /srv/w20/w2-weather/data/state.db /etc/w20/live.env /srv/w20/bin/w20"
  local a b
  # shellcheck disable=SC2086
  a=$(sha_set old $list) || die "the checksums of the files that matter could not be read on $OLD; nothing started on $NEW. Run 'back --go'"
  # shellcheck disable=SC2086
  b=$(sha_set new $list) || die "the checksums of the files that matter could not be read on $NEW; old units are stopped, nothing started there. Run 'back --go'"
  [[ $a == "$b" ]] || die "sha256 mismatch after final copy; old units are stopped, nothing started on $NEW. Run 'back --go'"
  echo "  identical: $list"
  say "6/7 geoblock, then start the new units"
  geoblock "$NEW" | grep -q '"blocked":false' || die "geoblock true from $NEW now; nothing started there. Run 'back --go'"
  newsh "date -u +%FT%TZ | sudo tee $OWNER_FILE >/dev/null && sudo test -s $OWNER_FILE" || die "the owner marker could not be written on $NEW; nothing started there. Run 'back --go'"
  start_new || die "the new units did not all start on $NEW; the owner marker is set, so 'back --go' copies home and restarts the old side"
  local since
  since=$(newsh 'date -u "+%F %T"') || die "the clock of $NEW could not be read after its units started; w20-live was NOT started there. Run 'back --go'"
  newsh 'sudo systemctl start w20-live.service' || { newsh 'sudo journalctl -u w20-live --no-pager -n 15 -o cat | cut -c1-200'; die "w20-live did not start on $NEW. Run 'back --go'"; }
  newsh 'sudo systemctl start --no-block weather-reward-pilot.service' || true
  say "7/7 quotes and fills in the journal from $NEW ($(geoblock "$NEW" | sed -nE 's/.*"country":"([^"]*)".*/\1/p'), $NEWIP), up to ${QUOTE_WAIT}s"
  quotes_check "$NEW" "$since" "$QUOTE_WAIT" sudo || die "no quotes seen on $NEW. Decide: wait, or 'back --go'"
  say "switch done; the bot runs on $NEW. Undo with: s3-weather.sh back --go"
}

cmd_back() {
  [[ ${1:-} == --go ]] || die "back needs --go"
  local owned
  owned=$(owner_marker) || die "cannot read the owner marker on $NEW, so it is not known whether AWS ever held the data; nothing was stopped or copied"
  say "1/4 stop and disable the new units ($NEW owner marker: $owned)"
  stop_new || die "new units not fully stopped; the old units were not started, so two quoters cannot run"
  if [[ $owned == owned ]]; then
    say "2/4 copy state home, then the manifest gate with sha256 of every file"
    copy_home || die "copy home or its manifest gate failed; both sides stopped, $NEW keeps the owner marker. Fix and rerun back --go"
    newsh "sudo rm -f $OWNER_FILE" || die "state is home and verified, but the owner marker on $NEW could not be cleared; remove $OWNER_FILE there, then rerun back --go"
  else
    say "2/4 no copy home: AWS never started the units, so $OLD still holds the only complete state and is not overwritten"
  fi
  say "3/4 start the old units"
  start_old || die "the old units did not all start on $OLD"
  local since
  since=$(oldsh 'date -u "+%F %T"') || die "the clock of $OLD could not be read after its units started; w20-live was NOT started and no quotes were checked. The state is home and the marker is cleared: rerun back --go"
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
