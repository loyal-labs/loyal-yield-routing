#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

usage() {
  cat >&2 <<EOF
usage: $0 <cluster> <step>
  preflight          installs pgmove on the target box; pgmove preflight at 0 DIFF; target units present and stopped
  freeze             stops the writers on CUT_OLD, fences the source roles, catches up, syncs sequences, verify and a full hash at 0 DIFF
  abort              after freeze, before switch: thaw (source roles LOGIN again, the forward on), writers back on CUT_OLD
  switch             only after a freeze that passed: subscriptions off, reverse armed, target roles LOGIN, CUT_SERVICES started on CUT_NEW
  verify             health on CUT_NEW, writes land on the target, the source took none outside the reverse subscription
  unfreeze           verify, then timers and engines on CUT_NEW; units enabled on CUT_NEW and disabled on CUT_OLD
  rollback           before unfreeze: the writers go back to CUT_OLD, every target write carried back by the reverse subscription
  rollback --reverse after unfreeze, until retire: the same path, after the engines have written to the target
  retire             after REVERSE_HOURS: both replication directions, the freeze marker and the forward go
  status             stamps and unit states on both hosts
Writers, hosts and health paths come from clusters/<cluster>.conf (CUT_*).
EOF
  exit 2
}

CLUSTER=${1:-}
STEP=${2:-}
[[ -n $CLUSTER && -n $STEP && -f $HERE/clusters/$CLUSTER.conf ]] || usage
die() { echo "cutover: $*" >&2; exit 1; }
# The AWS boxes log in as ubuntu; every command runs as root through sudo -n, and root@ hosts run it as is.
on() {
  local host=$1 cmd
  shift
  cmd="$*"
  if [[ $host == root@* ]]; then ssh -o BatchMode=yes -o ConnectTimeout=15 "$host" "$cmd"; else ssh -o BatchMode=yes -o ConnectTimeout=15 "$host" "sudo -n bash -c $(printf %q "$cmd")"; fi
}
CUT_OLD='' CUT_NEW='' BOX=''
CUT_TIMERS=() CUT_ONESHOTS=() CUT_ENGINES=() CUT_SERVICES=() CUT_HEALTH=() CUT_STOP=() CUT_SRCSTOP=() CUT_HASH=full CUT_ROLLBACK_VERIFY=1 SRC_VIA=ssh SRC_SSH=''
# shellcheck source=/dev/null
source "$HERE/clusters/$CLUSTER.conf"
[[ -n $CUT_OLD && -n $CUT_NEW && -n $BOX ]] || { echo "cutover: $CLUSTER.conf sets no CUT_OLD, CUT_NEW or BOX; $CLUSTER has no writers to move" >&2; exit 2; }
if [[ ${CUT_SERVICES[*]-}${CUT_HEALTH[*]-} == *LIVE_SLOT* ]]; then
  LIVE=$(on "$CUT_OLD" /usr/local/bin/robinx-slot live) || die "robinx-slot live failed on $CUT_OLD; the writer slot is unknown"
  [[ $LIVE == 8080 || $LIVE == 8081 ]] || die "robinx-slot live on $CUT_OLD answered '$LIVE', not 8080 or 8081"
  CUT_SERVICES=("${CUT_SERVICES[@]//LIVE_SLOT/$LIVE}")
  CUT_HEALTH=("${CUT_HEALTH[@]//LIVE_SLOT/$LIVE}")
  echo "cutover: live slot on $CUT_OLD is $LIVE; writers ${CUT_SERVICES[*]}, health ${CUT_HEALTH[*]}"
fi
TIMERS=${CUT_TIMERS[*]+"${CUT_TIMERS[*]}"}
ONESHOTS=${CUT_ONESHOTS[*]+"${CUT_ONESHOTS[*]}"}
ENGINES=${CUT_ENGINES[*]+"${CUT_ENGINES[*]}"}
SERVICES=${CUT_SERVICES[*]+"${CUT_SERVICES[*]}"}
# CUT_STOP: units stopped and checked with the writers on both hosts and never started, such as the
# robinX slot that is not live, which a deploy may have left running.
STOPS=${CUT_STOP[*]+"${CUT_STOP[*]}"}
ALL="$TIMERS $ENGINES $SERVICES"
# CUT_SRCSTOP: units on the source database host that write there as a superuser, which no fence
# stops (Loyal's chunk retention). freeze stops them and checks them stopped before the hash; abort
# and rollback start their timers again. Root there is PGMOVE_SRC_ADMIN in SRC_VIA=forward.
SRCSTOPS=${CUT_SRCSTOP[*]+"${CUT_SRCSTOP[*]}"}
SRCTIMERS=$(tr ' ' '\n' <<<"$SRCSTOPS" | awk '/\.timer$/' | paste -sd' ' -)
# CUT_HASH=moved: freeze rehashes only the tables whose write counters moved since the last 0-DIFF
# full hash (pgmove hash --moved); full, the default, rehashes every table under the fence.
case $CUT_HASH in full) FREEZE_HASH='' ;; moved) FREEZE_HASH=--moved ;; *) die "CUT_HASH is full or moved, not $CUT_HASH" ;; esac
[[ -n $SERVICES ]] || { echo "cutover: $CLUSTER.conf lists no CUT_SERVICES" >&2; exit 2; }
CUTOVER_PROOF=${CUTOVER_PROOF:-/Users/user/loyal/artifacts/orchestrate/aws-migration/proof/cutover-$CLUSTER}
STATE=/var/lib/pgmove/$CLUSTER
TIMES=()

on_src() {
  if [[ $SRC_VIA == forward ]]; then
    [[ -n ${PGMOVE_SRC_ADMIN:-} ]] || { echo "cutover: CUT_SRCSTOP needs PGMOVE_SRC_ADMIN, the command that runs its one argument as root on the source" >&2; return 1; }
    "$PGMOVE_SRC_ADMIN" "$*"
  else
    on "$SRC_SSH" "$@"
  fi
}

now() { perl -MTime::HiRes=time -e 'printf "%.1f", time'; }
pg() { local sub=$1; shift; "$HERE/remote.sh" run "$sub" "$CLUSTER" "$@"; }
stamped() { [[ $(on "$BOX" "test -e $STATE/$1 && echo yes || echo no") == yes ]]; }
# this_tenure <file>: the file names the current switch, the switched stamp it was written under; one
# from an earlier switch, which a rollback ended, does not count.
this_tenure() { [[ $(on "$BOX" "test -s $STATE/switched && test \"\$(cat $STATE/$1 2>/dev/null)\" = \"\$(cat $STATE/switched)\" && echo yes || echo no") == yes ]]; }
tenure_stamp() { on "$BOX" "mkdir -p \$(dirname $STATE/$1) && cp $STATE/switched $STATE/$1.new && mv -f $STATE/$1.new $STATE/$1 && sync $STATE"; }
# yes once a switch reached its first LOGIN on the target and no switch --reverse has reached its
# first on the source since (pgmove.sh target_primary); no otherwise; anything else is no answer.
target_primary() { on "$BOX" "if test -s $STATE/switched && ! { test -s $STATE/reversed && [ \$(cat $STATE/reversed) -gt \$(cat $STATE/switched) ]; }; then echo yes; else echo no; fi"; }

# A step runs in an OR-list here, where bash turns errexit off for everything it calls: every
# command in a step function carries its own || die or || return 1.
timed() {
  local label=$1 t rc=0
  shift
  t=$(now)
  printf '\n== %s (%s)\n' "$label" "$(date -u +%H:%M:%SZ)"
  "$@" || rc=$?
  TIMES+=("$label $(perl -e "printf '%.1f', $(now) - $t")")
  echo "-- $label: ${TIMES[${#TIMES[@]} - 1]#* }s$( ((rc == 0)) || echo ", FAILED exit $rc")"
  return $rc
}

report_times() {
  ((${#TIMES[@]})) || return 0
  echo
  echo "== step timings"
  printf '%s\n' "${TIMES[@]}" | awk '{ printf "  %-22s %7.1fs\n", $1, $2; t += $2 } END { printf "  %-22s %7.1fs\n", "total", t }'
}

health_path() {
  local e
  for e in ${CUT_HEALTH[@]+"${CUT_HEALTH[@]}"}; do [[ ${e%%=*} == "$1" ]] && { echo "${e#*=}"; return 0; }; done
  echo -
}

stop_writers() {
  local host=$1 u timers oneshots
  timers=$TIMERS oneshots=$ONESHOTS
  if [[ -n $timers ]]; then
    on "$host" "systemctl stop $timers" || die "$host: stopping $timers failed"
    echo "$host: stopped timers $timers"
  fi
  if [[ -n $oneshots ]]; then
    on "$host" "for i in \$(seq 300); do systemctl is-active $oneshots | grep -qvxE 'inactive|failed' || exit 0; sleep 1; done; echo 'a timer run is still running after 300s' >&2; exit 1" || die "$host: a timer run of $oneshots did not end"
  fi
  for u in $ENGINES; do
    on "$host" "systemctl stop $u; [ \"\$(systemctl show -p Result --value $u)\" = success ] || { echo '$u did not stop cleanly' >&2; exit 1; }" || die "$host: $u did not stop cleanly"
    echo "$host: stopped $u cleanly"
  done
  on "$host" "systemctl stop $SERVICES" || die "$host: stopping $SERVICES failed"
  echo "$host: stopped $SERVICES"
  # A CUT_STOP unit may be absent from a host; one systemctl call for all would fail on it.
  for u in $STOPS; do
    on "$host" "[ \"\$(systemctl show -p LoadState --value $u)\" = not-found ] || systemctl stop $u" || die "$host: stopping $u failed"
  done
  [[ -z $STOPS ]] || echo "$host: stopped $STOPS where loaded"
  writers_stopped "$host" || die "a writer is still active on $host"
}

# One "unit LoadState ActiveState" line per configured unit, each loaded and inactive or failed. A
# CUT_STOP unit may be absent from a host; an absent writer is a typo, not a stopped writer.
writers_stopped() {
  local states u want=0
  states=$(on "$1" "for u in $ENGINES $SERVICES $STOPS $ONESHOTS $TIMERS; do echo \"\$u \$(systemctl show -p LoadState --value \$u) \$(systemctl show -p ActiveState --value \$u)\"; done") || { echo "cutover: $1 did not answer for its units" >&2; return 1; }
  for u in $ENGINES $SERVICES $STOPS $ONESHOTS $TIMERS; do
    want=$((want + 1))
    if [[ " $STOPS " == *" $u "* ]]; then
      grep -qxE "$u (loaded (inactive|failed)|not-found inactive)" <<<"$states" || { echo "cutover: $1 $u: $(grep "^$u " <<<"$states" || echo 'no state')" >&2; return 1; }
    else
      grep -qxE "$u loaded (inactive|failed)" <<<"$states" || { echo "cutover: $1 $u: $(grep "^$u " <<<"$states" || echo 'no state')" >&2; return 1; }
    fi
  done
  [[ $(grep -c . <<<"$states") == "$want" ]] || { echo "cutover: $1 answered $(grep -c . <<<"$states") unit lines for $want units" >&2; return 1; }
}

src_writers_stopped() {
  local states u
  [[ -n $SRCSTOPS ]] || return 0
  states=$(on_src "for u in $SRCSTOPS; do echo \"\$u \$(systemctl show -p LoadState --value \$u) \$(systemctl show -p ActiveState --value \$u)\"; done") || { echo "cutover: the source did not answer for $SRCSTOPS" >&2; return 1; }
  for u in $SRCSTOPS; do
    grep -qxE "$u loaded (inactive|failed)" <<<"$states" || { echo "cutover: source $u: $(grep "^$u " <<<"$states" || echo 'no state')" >&2; return 1; }
  done
}

stop_src_writers() {
  [[ -n $SRCSTOPS ]] || return 0
  on_src "systemctl stop $SRCSTOPS" || die "source: stopping $SRCSTOPS failed"
  echo "source: stopped $SRCSTOPS"
  src_writers_stopped || die "a superuser writer is still active on the source"
}

# start_src_timers [enable]: the source's own timers again, once it is the primary.
start_src_timers() {
  local verb=start
  [[ -n $SRCTIMERS ]] || return 0
  [[ ${1:-} != enable ]] || verb='enable --now'
  on_src "systemctl $verb $SRCTIMERS" || die "source: starting $SRCTIMERS failed"
  echo "source: started $SRCTIMERS"
}

start_healthy() {
  local host=$1 u path
  shift
  for u in "$@"; do
    path=$(health_path "$u")
    if [[ $path == - ]]; then
      on "$host" "systemctl start $u; sleep 5; systemctl is-active --quiet $u" || die "$host: $u not active 5 s after start"
      echo "$host: $u active (no health path in $CLUSTER.conf)"
      continue
    fi
    on "$host" "systemctl start $u; for i in \$(seq 90); do systemctl is-active --quiet $u && curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:$path && { echo \"$host: $u active, $path answers after \${i}s\"; exit 0; }; sleep 1; done; echo '$u: not healthy after 90s' >&2; exit 1" || die "$host: $u not healthy after start"
  done
}

start_timers() {
  local timers
  timers=$TIMERS
  [[ -z $timers ]] || { on "$1" "systemctl start $timers" || die "$1: starting $timers failed"; echo "$1: started $timers"; }
}

health() {
  local host=$1 u path bad=0
  shift
  for u in "$@"; do
    path=$(health_path "$u")
    if on "$host" "systemctl is-active --quiet $u && { [ $path = - ] || curl -fsS -o /dev/null --max-time 5 http://127.0.0.1:$path; }"; then
      echo "health: $host $u active, $path ok, restarts $(on "$host" "systemctl show -p NRestarts --value $u")"
    else
      echo "health: FAIL $host $u ($(on "$host" "systemctl is-active $u" || true), $path)"
      bad=1
    fi
  done
  return $bad
}

units_ready() {
  local u bad=0 all
  all=$ALL
  for u in $all; do
    if ! on "$CUT_NEW" "systemctl cat $u >/dev/null 2>&1"; then echo "preflight: $CUT_NEW has no unit $u"; bad=1; continue; fi
    if on "$CUT_NEW" "systemctl is-active --quiet $u"; then echo "preflight: $CUT_NEW runs $u before the switch"; bad=1; fi
  done
  for u in $STOPS; do
    if on "$CUT_NEW" "systemctl is-active --quiet $u"; then echo "preflight: $CUT_NEW runs $u, which the switch never starts"; bad=1; fi
  done
  ((bad == 0)) && echo "preflight: $CUT_NEW has every unit of $CLUSTER.conf, all stopped"
  return $bad
}

writes_total() { sed -n 's/^writes: .* total \([0-9]*\) since .*/\1/p' <<<"$1"; }

# pg_counted <tag> <side>: pgmove writes diff, which exits 0 only once it read every counter and
# prints its "total" line; anything else is no answer, never zero writes.
pg_counted() {
  local out
  out=$(pg writes diff "$1" "$2") || { echo "verify: FAIL writes diff $1 $2 did not answer (exit $?); writes cannot be compared" >&2; return 1; }
  [[ -n $(writes_total "$out") ]] || { echo "verify: FAIL writes diff $1 $2 printed no total; writes cannot be compared" >&2; return 1; }
  printf '%s\n' "$out"
}

source_outside_reverse() {
  local src dst
  # The target first, then the source once the reverse slot carried everything the target took.
  dst=$(pg_counted switch dst) || return 1
  src=$(pg_counted freeze src) || return 1
  printf '%s\n%s\n' "$src" "$dst" | sed -n 's/^writes: /  /p'
  grep -q 'counters reset' <<<"$src$dst" && { echo "verify: FAIL counters reset on one side; writes cannot be compared"; return 1; }
  awk '$1 == "DIFF" && $2 == "dst" { d[$3 " " $4] = $5 } $1 == "DIFF" && $2 == "src" { s[$3 " " $4] = $5 }
    END { for (k in s) if (s[k] > d[k] + 0) { print "verify: FAIL source " k " took " s[k] " writes since freeze, the target " d[k] + 0 " since switch"; bad = 1 } exit bad }' \
    <(printf '%s\n%s\n' "$src" "$dst")
}

cmd_preflight() {
  local out
  mkdir -p "$CUTOVER_PROOF"
  out=$CUTOVER_PROOF/preflight-verify-$(date -u +%Y%m%dT%H%MZ).txt
  timed install "$HERE/remote.sh" install "$CLUSTER" || die "install failed"
  # pgmove preflight: tables in state r, lag, verify, the reverse, fence and switch dry runs, marker, volume.
  timed preflight pg preflight >"$out" || { cat "$out"; die "pgmove preflight failed; full output $out"; }
  echo "preflight: $(grep ' DIFF$' "$out" | tail -1); full output $out"
  timed units units_ready || die "the units on $CUT_NEW are not ready"
  timed writes-save pg writes save preflight src || die "writes save failed"
  timed ssh-roundtrip on "$CUT_OLD" true || die "$CUT_OLD did not answer"
  report_times
}

cmd_freeze() {
  local out tp
  tp=$(target_primary) || die "$BOX did not answer for its stamps"
  [[ $tp == no ]] || die "the switch has run and no rollback since; freeze is for the source side, rerun switch instead"
  on "$BOX" "rm -f $STATE/freeze-ok" || die "cannot remove $STATE/freeze-ok on $BOX"
  mkdir -p "$CUTOVER_PROOF"
  out=$CUTOVER_PROOF/freeze-verify-$(date -u +%Y%m%dT%H%MZ).txt
  timed install "$HERE/remote.sh" install "$CLUSTER" || die "install failed"
  timed stop stop_writers "$CUT_OLD" || die "the writers did not stop"
  timed stop-source stop_src_writers || die "the source's own writers did not stop"
  timed fence pg fence || die "fence failed; rerun freeze, or abort"
  timed catchup pg catchup || die "catchup failed"
  timed sequences pg sequences || die "sequences failed"
  timed writes-save pg writes save freeze src || die "writes save failed"
  # Under the fence one full hash covers every row, the old ones verify's tail does not read. It runs
  # before verify, which needs a 0-DIFF hash: a rerun after a bad one would otherwise never hash again.
  timed source-stopped src_writers_stopped || die "a writer on the source runs again; rerun freeze, or abort"
  timed hash pg hash $FREEZE_HASH >"$out" || { tail -20 "$out"; die "the hash under the fence differs ($out); rerun freeze, or abort"; }
  timed verify pg verify >>"$out" || { tail -40 "$out"; die "the target differs from the source at the freeze point ($out); rerun freeze, or abort"; }
  echo "verify: $(grep ' DIFF$' "$out" | tail -1) at the freeze point; $out"
  on "$BOX" "touch $STATE/freeze-ok" || die "cannot stamp $STATE/freeze-ok on $BOX"
  report_times
}

cmd_abort() {
  local tp
  tp=$(target_primary) || die "$BOX did not answer for its stamps"
  [[ $tp == no ]] || die "the switch has run and no rollback since; rollback is the way back"
  on "$BOX" "rm -f $STATE/freeze-ok" || die "cannot remove $STATE/freeze-ok on $BOX"
  timed thaw pg thaw || die "thaw failed; the source roles may still be NOLOGIN"
  timed start start_healthy "$CUT_OLD" "${CUT_SERVICES[@]}" || die "the writers did not start healthy on $CUT_OLD"
  timed start-timers start_timers "$CUT_OLD" || die "the timers did not start on $CUT_OLD"
  timed start-engines start_healthy "$CUT_OLD" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"} || die "the engines did not start healthy on $CUT_OLD"
  timed start-source start_src_timers || die "the source's timers did not start"
  report_times
}

cmd_switch() {
  stamped freeze-ok || die "no $STATE/freeze-ok on $BOX: freeze has not passed since it last started; run freeze"
  writers_stopped "$CUT_OLD" || die "writers run on $CUT_OLD; run freeze first"
  src_writers_stopped || die "a writer on the source runs; run freeze first"
  timed switch pg switch || die "pg switch failed"
  this_tenure writes/switch-dst-ok || { timed writes-save pg writes save switch dst && tenure_stamp writes/switch-dst-ok; } || die "writes save switch dst failed"
  timed start start_healthy "$CUT_NEW" "${CUT_SERVICES[@]}" || die "the writers did not start healthy on $CUT_NEW"
  report_times
}

cmd_verify() {
  local out bad=0
  timed health health "$CUT_NEW" "${CUT_SERVICES[@]}" || bad=1
  out=$(pg_counted switch dst) || out=''
  if [[ -z $(writes_total "$out") || $(writes_total "$out") == 0 ]] || grep -q 'counters reset' <<<"$out"; then
    echo "verify: FAIL no write has landed on the target since switch, or its counters cannot be read"
    bad=1
  else
    echo "verify: the target took $(writes_total "$out") row writes since switch"
  fi
  timed source-writes source_outside_reverse || bad=1
  report_times
  ((bad == 0)) || die "verify failed"
  echo "verify: ok"
}

cmd_unfreeze() {
  local all
  stamped switched || die "the switch has not run"
  cmd_verify
  tenure_stamp unfrozen || die "cannot stamp $STATE/unfrozen on $BOX"
  echo "unfreeze: $STATE/unfrozen on $BOX; plain rollback now refuses, rollback --reverse remains until retire"
  timed start-timers start_timers "$CUT_NEW" || die "the timers did not start on $CUT_NEW"
  timed start-engines start_healthy "$CUT_NEW" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"} || die "the engines did not start healthy on $CUT_NEW"
  all=$ALL
  on "$CUT_NEW" "systemctl enable $all" || die "$CUT_NEW: enabling $all failed"
  echo "$CUT_NEW: enabled $all"
  on "$CUT_OLD" "systemctl disable $all" || die "$CUT_OLD: disabling $all failed"
  echo "$CUT_OLD: disabled $all"
  if [[ -n $SRCTIMERS ]]; then on_src "systemctl disable $SRCTIMERS" || die "source: disabling $SRCTIMERS failed"; echo "source: disabled $SRCTIMERS"; fi
  sleep 60
  timed health health "$CUT_NEW" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"} "${CUT_SERVICES[@]}" || die "health failed on $CUT_NEW"
  report_times
}

# A rollback that stops leaves the target the primary, so its writers go back on there. pgmove gives
# a fence --reverse that stopped its LOGIN back itself; thaw --reverse does it for a switch --reverse
# that stopped before its stamp, and turns the reverse on again.
rollback_failed() {
  local tp
  tp=$(target_primary) || die "$1; $BOX did not answer for its stamps, so the target may be fenced"
  [[ $tp == yes ]] || die "$1 after its stamp: the source is the primary; rerun rollback to finish it"
  timed thaw-reverse pg thaw --reverse || die "$1, and thaw --reverse failed: the target may still be fenced; rerun rollback, or run 'thaw $CLUSTER --reverse'"
  timed restart start_healthy "$CUT_NEW" "${CUT_SERVICES[@]}" || die "$1; the target takes writes again but the writers did not start healthy on $CUT_NEW"
  if this_tenure unfrozen; then
    timed restart-timers start_timers "$CUT_NEW" || die "$1; the timers did not start on $CUT_NEW"
    timed restart-engines start_healthy "$CUT_NEW" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"} || die "$1; the engines did not start healthy on $CUT_NEW"
  fi
  report_times
  die "$1; the target takes writes again and its writers run on $CUT_NEW: fix the cause above, then rerun rollback"
}

cmd_rollback() {
  local all out
  stamped switched || die "the switch has not run; nothing to roll back (after a freeze, abort thaws the source and restarts the writers on $CUT_OLD)"
  stamped retired && die "retire has run; the reverse subscription is gone"
  if this_tenure unfrozen && [[ ${1:-} != --reverse ]]; then
    die "unfreeze has run, so the engines wrote to the target. 'rollback --reverse' carries those writes back over the reverse subscription"
  fi
  timed stop stop_writers "$CUT_NEW" || die "the writers did not stop on $CUT_NEW"
  timed fence pg fence --reverse || rollback_failed "fence --reverse failed"
  timed switch pg switch --reverse || rollback_failed "switch --reverse failed"
  # The reverse carries replicated tables whole, but a hypertable only above its floor: verify hashes
  # each one's tail on both sides before any writer runs on the source again.
  if ((CUT_ROLLBACK_VERIFY)); then
    mkdir -p "$CUTOVER_PROOF"
    out=$CUTOVER_PROOF/rollback-verify-$(date -u +%Y%m%dT%H%MZ).txt
    timed verify pg verify --reverse >"$out" || { tail -40 "$out"; die "the source is the primary, its writers on $CUT_OLD still stopped, and it differs from the target ($out): repair, then rerun rollback; CUT_ROLLBACK_VERIFY=0 in $CLUSTER.conf starts them without this check"; }
    echo "verify: $(grep ' DIFF$' "$out" | tail -1) after switch --reverse; $out"
  fi
  timed start start_healthy "$CUT_OLD" "${CUT_SERVICES[@]}" || die "the writers did not start healthy on $CUT_OLD"
  timed start-timers start_timers "$CUT_OLD" || die "the timers did not start on $CUT_OLD"
  timed start-engines start_healthy "$CUT_OLD" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"} || die "the engines did not start healthy on $CUT_OLD"
  timed start-source start_src_timers enable || die "the source's timers did not start"
  all=$ALL
  on "$CUT_OLD" "systemctl enable $all" || die "$CUT_OLD: enabling $all failed"
  echo "$CUT_OLD: enabled $all"
  on "$CUT_NEW" "systemctl disable $all" || die "$CUT_NEW: disabling $all failed"
  echo "$CUT_NEW: disabled $all"
  report_times
}

cmd_retire() {
  timed retire pg retire || die "retire failed"
  on "$BOX" "touch $STATE/retired" || die "cannot stamp $STATE/retired on $BOX"
  echo "retire: $STATE/retired on $BOX; rollback refuses from here"
  report_times
}

cmd_status() {
  local s all
  for s in switched reversed retired; do
    if stamped "$s"; then echo "status: $s"; else echo "status: not $s"; fi
  done
  if this_tenure unfrozen; then echo "status: unfrozen"; else echo "status: not unfrozen since the last switch"; fi
  all=$ALL
  all="$all $STOPS"
  echo "status: $CUT_OLD $(on "$CUT_OLD" "systemctl is-active $all" | paste -sd' ' - || true)"
  echo "status: $CUT_NEW $(on "$CUT_NEW" "systemctl is-active $all" | paste -sd' ' - || true)"
}

case $STEP in
  preflight | freeze | abort | switch | verify | unfreeze | retire | status) "cmd_$STEP" ;;
  rollback) cmd_rollback "${3:-}" ;;
  *) usage ;;
esac
