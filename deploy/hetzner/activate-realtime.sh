#!/usr/bin/env bash
# Keep the existing credential launcher; atomically replace only its pinned binary.
set -euo pipefail
sha=${1:-}
want=${2:-}
[[ $sha =~ ^[0-9a-f]{40}$ && $want =~ ^[0-9a-f]{64}$ ]] || exit 2
unit=loyal-realtime.service
current=/opt/loyal/bin/loyal-yield-realtime
directory=/opt/loyal/releases/realtime/$sha
candidate=$directory/loyal-yield-realtime
dropin=/etc/systemd/system/$unit.d/40-verified-release.conf
mkdir -p /var/log/loyal-deploy
trap '' HUP
exec > >(tee -p -a "/var/log/loyal-deploy/realtime-$sha.log") 2>&1
exec 9>/run/lock/loyal-deploy.lock
flock -w 300 9 || { echo 'another deployment holds the host lock'; exit 1; }
[[ $(sha256sum "$candidate" | cut -d' ' -f1) == "$want" ]] || { echo 'realtime artifact checksum mismatch'; exit 1; }
[[ $(systemctl is-active "$unit") == active ]] || { echo 'realtime was stopped; refusing to start it'; exit 1; }
prev=$(systemctl show "$unit" -p Environment --value | sed -n 's/.*LOYAL_IMAGE_VERSION=sha-\([0-9a-f]\{40\}\).*/\1/p')
[[ $prev =~ ^[0-9a-f]{40}$ ]] || { echo 'previous realtime release identity is missing'; exit 1; }
if [[ -n ${LOYAL_DEPLOY_EXPECTED_RELEASE:-} && $prev != "$LOYAL_DEPLOY_EXPECTED_RELEASE" ]]; then
  echo 'realtime release changed after preflight'
  exit 1
fi
if [[ $prev == "$sha" ]]; then
  [[ $(sha256sum "$current" | cut -d' ' -f1) == "$want" ]] || exit 1
  echo "realtime already runs $sha"
  exit 0
fi
chmod 0555 "$candidate"
ldd "$candidate" > "$directory/ldd.log"
if grep -q 'not found' "$directory/ldd.log"; then echo 'realtime runtime library missing'; exit 1; fi
previous=/opt/loyal/releases/realtime/$prev/loyal-yield-realtime
mkdir -p "$(dirname "$previous")" "$(dirname "$dropin")"
if [[ -f $previous ]]; then
  cmp -s "$previous" "$current" || { echo 'saved previous realtime binary differs from the running release'; exit 1; }
else
  install -m 0555 "$current" "$previous"
fi
had_dropin=no
if [[ -f $dropin ]]; then
  cp -p "$dropin" "$directory/previous-release.conf"
  had_dropin=yes
fi
# shellcheck disable=SC2317 # Called by the EXIT trap.
restore() {
  systemctl stop "$unit" &&
    install -m 0555 "$previous" "$current.tmp" && mv -f "$current.tmp" "$current" &&
    { if [[ $had_dropin == yes ]]; then cp -p "$directory/previous-release.conf" "$dropin"; else rm -f "$dropin"; fi; } &&
    systemctl daemon-reload && systemctl start "$unit" &&
    curl -fsS --max-time 10 http://127.0.0.1:10000/readyz >/dev/null
}
# shellcheck disable=SC2317 # Called by the EXIT trap.
finish() {
  status=$?
  trap - EXIT
  if [[ $status != 0 ]]; then
    if restore; then
      echo "realtime restored to $prev after failed activation"
    else
      echo "realtime restoration to $prev failed; operator intervention required"
    fi
  fi
  exit "$status"
}
trap 'finish' EXIT
systemctl stop "$unit"
install -m 0555 "$candidate" "$current.tmp"
mv -f "$current.tmp" "$current"
printf '[Service]\nEnvironment=LOYAL_IMAGE_VERSION=sha-%s\n' "$sha" > "$dropin.tmp"
mv -f "$dropin.tmp" "$dropin"
systemctl daemon-reload
systemctl start "$unit"
deadline=$(( $(date +%s) + 60 ))
healthy_since=0
while [[ $(date +%s) -lt $deadline ]]; do
  [[ $(systemctl show "$unit" -p NRestarts --value) == 0 ]] || { echo 'realtime restarted'; exit 1; }
  [[ $(systemctl is-active "$unit") == active ]] || { echo 'realtime exited'; exit 1; }
  if curl -fsS --max-time 5 http://127.0.0.1:10000/readyz >/dev/null; then
    [[ $healthy_since != 0 ]] || healthy_since=$(date +%s)
    if [[ $(( $(date +%s) - healthy_since )) -ge 20 ]]; then
      echo "realtime deployed $sha sha256 $want"
      exit 0
    fi
  else
    healthy_since=0
  fi
  sleep 2
done
echo 'realtime readiness did not stabilize'
exit 1
