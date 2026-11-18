#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

usage() {
  cat >&2 <<EOF
usage: $0 <cluster> <step>
  preflight          installs pgmove on the target box; pgmove preflight at 0 DIFF; target units present and stopped
  freeze             stops the writers on CUT_OLD, fences the source roles, catches up, syncs sequences, verify at 0 DIFF
  switch             subscriptions off, reverse armed, target roles LOGIN, CUT_SERVICES started on CUT_NEW
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
CUT_TIMERS=() CUT_ONESHOTS=() CUT_ENGINES=() CUT_SERVICES=() CUT_HEALTH=() CUT_STOP=()
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
[[ -n $SERVICES ]] || { echo "cutover: $CLUSTER.conf lists no CUT_SERVICES" >&2; exit 2; }
CUTOVER_PROOF=${CUTOVER_PROOF:-/Users/user/loyal/artifacts/orchestrate/aws-migration/proof/cutover-$CLUSTER}
STATE=/var/lib/pgmove/$CLUSTER
TIMES=()

now() { perl -MTime::HiRes=time -e 'printf "%.1f", time'; }
pg() { local sub=$1; shift; "$HERE/remote.sh" run "$sub" "$CLUSTER" "$@"; }
stamped() { [[ $(on "$BOX" "test -e $STATE/$1 && echo yes || echo no") == yes ]]; }

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
    on "$host" "systemctl stop $timers"
    echo "$host: stopped timers $timers"
  fi
  if [[ -n $oneshots ]]; then
    on "$host" "for i in \$(seq 300); do systemctl is-active $oneshots | grep -qvxE 'inactive|failed' || exit 0; sleep 1; done; echo 'a timer run is still running after 300s' >&2; exit 1"
  fi
  for u in $ENGINES; do
    on "$host" "systemctl stop $u; [ \"\$(systemctl show -p Result --value $u)\" = success ] || { echo '$u did not stop cleanly' >&2; exit 1; }"
    echo "$host: stopped $u cleanly"
  done
  on "$host" "systemctl stop $SERVICES $STOPS"
  echo "$host: stopped $SERVICES $STOPS"
  writers_stopped "$host" || die "a writer is still active on $host"
}

writers_stopped() {
  local states
  states=$(on "$1" "systemctl is-active $ENGINES $SERVICES $STOPS $ONESHOTS $TIMERS || true")
  [[ -n $states ]] && ! grep -qvxE 'inactive|failed' <<<"$states"
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
    on "$host" "systemctl start $u; for i in \$(seq 90); do systemctl is-active --quiet $u && curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:$path && { echo \"$host: $u active, $path answers after \${i}s\"; exit 0; }; sleep 1; done; echo '$u: not healthy after 90s' >&2; exit 1"
  done
}

start_timers() {
  local timers
  timers=$TIMERS
  [[ -z $timers ]] || { on "$1" "systemctl start $timers"; echo "$1: started $timers"; }
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

source_outside_reverse() {
  local src dst
  src=$(pg writes diff freeze src || true)
  dst=$(pg writes diff switch dst || true)
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
  timed install "$HERE/remote.sh" install "$CLUSTER"
  # pgmove preflight: tables in state r, lag, verify, the reverse, fence and switch dry runs, marker, volume.
  timed preflight pg preflight >"$out" || { cat "$out"; die "pgmove preflight failed; full output $out"; }
  echo "preflight: $(tail -1 "$out"); full output $out"
  timed units units_ready
  timed writes-save pg writes save preflight src
  timed ssh-roundtrip on "$CUT_OLD" true
  report_times
}

cmd_freeze() {
  local out
  stamped switched && die "the switch has run; freeze is for the source side, rerun switch instead"
  mkdir -p "$CUTOVER_PROOF"
  out=$CUTOVER_PROOF/freeze-verify-$(date -u +%Y%m%dT%H%MZ).txt
  timed install "$HERE/remote.sh" install "$CLUSTER"
  timed stop stop_writers "$CUT_OLD"
  timed fence pg fence
  timed catchup pg catchup
  timed sequences pg sequences
  timed writes-save pg writes save freeze src
  timed verify pg verify >"$out" || { cat "$out"; die "the target differs from the source at the freeze point ($out); rerun freeze"; }
  echo "verify: $(tail -1 "$out") at the freeze point; $out"
  report_times
}

cmd_switch() {
  writers_stopped "$CUT_OLD" || die "writers run on $CUT_OLD; run freeze first"
  timed switch pg switch
  stamped writes/switch-dst-ok || { timed writes-save pg writes save switch dst && on "$BOX" "touch $STATE/writes/switch-dst-ok"; }
  timed start start_healthy "$CUT_NEW" "${CUT_SERVICES[@]}"
  report_times
}

cmd_verify() {
  local out bad=0
  timed health health "$CUT_NEW" "${CUT_SERVICES[@]}" || bad=1
  out=$(pg writes diff switch dst || true)
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
  on "$BOX" "touch $STATE/unfrozen && sync $STATE"
  echo "unfreeze: $STATE/unfrozen on $BOX; plain rollback now refuses, rollback --reverse remains until retire"
  timed start-timers start_timers "$CUT_NEW"
  timed start-engines start_healthy "$CUT_NEW" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"}
  all=$ALL
  on "$CUT_NEW" "systemctl enable $all" && echo "$CUT_NEW: enabled $all"
  on "$CUT_OLD" "systemctl disable $all" && echo "$CUT_OLD: disabled $all"
  sleep 60
  timed health health "$CUT_NEW" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"} "${CUT_SERVICES[@]}"
  report_times
}

cmd_rollback() {
  local all
  stamped switched || die "the switch has not run; nothing to roll back (start the writers on $CUT_OLD if freeze stopped them)"
  stamped retired && die "retire has run; the reverse subscription is gone"
  if stamped unfrozen && [[ ${1:-} != --reverse ]]; then
    die "unfreeze has run, so the engines wrote to the target. 'rollback --reverse' carries those writes back over the reverse subscription"
  fi
  timed stop stop_writers "$CUT_NEW"
  timed fence pg fence --reverse
  timed switch pg switch --reverse
  timed start start_healthy "$CUT_OLD" "${CUT_SERVICES[@]}"
  timed start-timers start_timers "$CUT_OLD"
  timed start-engines start_healthy "$CUT_OLD" ${CUT_ENGINES[@]+"${CUT_ENGINES[@]}"}
  all=$ALL
  on "$CUT_OLD" "systemctl enable $all" && echo "$CUT_OLD: enabled $all"
  on "$CUT_NEW" "systemctl disable $all" && echo "$CUT_NEW: disabled $all"
  report_times
}

cmd_retire() {
  timed retire pg retire
  on "$BOX" "touch $STATE/retired"
  echo "retire: $STATE/retired on $BOX; rollback refuses from here"
  report_times
}

cmd_status() {
  local s all
  for s in switched unfrozen reversed retired; do
    if stamped "$s"; then echo "status: $s"; else echo "status: not $s"; fi
  done
  all=$ALL
  all="$all $STOPS"
  echo "status: $CUT_OLD $(on "$CUT_OLD" "systemctl is-active $all" | paste -sd' ' - || true)"
  echo "status: $CUT_NEW $(on "$CUT_NEW" "systemctl is-active $all" | paste -sd' ' - || true)"
}

case $STEP in
  preflight | freeze | switch | verify | unfreeze | retire | status) "cmd_$STEP" ;;
  rollback) cmd_rollback "${3:-}" ;;
  *) usage ;;
esac
