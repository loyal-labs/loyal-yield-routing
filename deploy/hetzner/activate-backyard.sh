#!/bin/sh
# activate-backyard.sh <sha> <sha256> — switch loyal-backyard to an uploaded
# release. Runs on the host as root, started by scripts/deploy-workers.sh as
# a transient unit (systemd-run --wait --pipe), so a dropped ssh session
# cannot cut it off mid-swap. One deploy at a time: it holds a host lock.
#
# It stops the worker only when no operation is in flight, starts the new
# release from the verified-release drop-in, and restores the previous
# drop-in if the new release does not come up.
set -eu

unit=loyal-backyard
dropin=/etc/systemd/system/$unit.service.d/40-verified-release.conf
releases=/opt/loyal/releases/backyard

sha=${1:-}
want=${2:-}
case $sha in *[!0-9a-f]* | '') sha=bad ;; esac
case $want in *[!0-9a-f]* | '') want=bad ;; esac
if [ ${#sha} -ne 40 ] || [ ${#want} -ne 64 ]; then
	echo "usage: activate-backyard.sh <40-hex commit sha> <64-hex sha256>" >&2
	exit 2
fi
bin=$releases/$sha/loyal-engine

# A dropped ssh session closes the pipe to the driver; the swap must go on.
# Only tee writes to that pipe, and tee -p keeps writing the log without it.
trap '' HUP
mkdir -p /var/log/loyal-deploy
fifo=/run/loyal-deploy-backyard.$$.fifo
rm -f "$fifo"
mkfifo -m 600 "$fifo"
tee -p -a "/var/log/loyal-deploy/backyard-$sha.log" <"$fifo" &
tee_pid=$!
exec >"$fifo" 2>&1
rm -f "$fifo"
finish() {
	status=$?
	exec >/dev/null 2>&1
	wait "$tee_pid" || true
	exit "$status"
}
trap finish EXIT

say() { echo "$(date -u '+%H:%M:%SZ') $*"; }
fail() {
	say "FAILED: $*"
	exit 1
}

say "activate backyard $sha"
exec 9>/run/lock/loyal-deploy-backyard.lock
if ! flock -w 300 9; then
	fail "another Backyard deploy held /run/lock/loyal-deploy-backyard.lock for 300 s; nothing changed"
fi

[ -x "$bin" ] || fail "$bin is missing"
got=$(sha256sum "$bin" | cut -d' ' -f1)
[ "$got" = "$want" ] || fail "$bin sha256 is $got, expected $want"

[ -f "$dropin" ] || fail "$dropin is missing; the first install is manual (deploy/hetzner/README.md)"
prev=$(sed -n "s|^ExecStart=$releases/\([0-9a-f]\{40\}\)/loyal-engine\$|\1|p" "$dropin")
[ ${#prev} -eq 40 ] || fail "cannot read the running release from $dropin"
state=$(systemctl is-active "$unit" || true)
if [ "$prev" = "$sha" ] && [ "$state" = active ]; then
	say "already running $sha"
	exit 0
fi
if [ "$state" != active ]; then
	fail "$unit is $state before the deploy: someone stopped it on purpose. Start it (or find out why it is down) first."
fi

# The gate is the new release's own read of the in-flight rows, under the
# unit's own database credential (its path read from the unit, not here).
# `systemctl show` prints encrypted credentials as "[unprintable]", so read
# the unit's own lines.
dbcred=$(systemctl cat "$unit" |
	sed -n 's/^LoadCredentialEncrypted=\(BACKYARD_DATABASE_URL:[^ ]*\)$/\1/p' | tail -n 1)
[ -n "$dbcred" ] || fail "no BACKYARD_DATABASE_URL in $unit's LoadCredentialEncrypted"
in_flight() {
	systemd-run --quiet --wait --pipe --collect -p DynamicUser=yes \
		-p "LoadCredentialEncrypted=$dbcred" \
		"$bin" backyard in-flight </dev/null
}

say "previous release $prev; waiting for no in-flight operation (up to 120 s)"
deadline=$(($(date +%s) + 120))
while :; do
	count=$(in_flight) || fail "in-flight gate failed; $unit untouched on $prev"
	case $count in '' | *[!0-9]*) fail "in-flight gate printed '$count'; $unit untouched on $prev" ;; esac
	[ "$count" -eq 0 ] && break
	[ "$(date +%s)" -ge "$deadline" ] && fail "$count operation(s) still in flight after 120 s; $unit untouched on $prev"
	say "$count in flight; waiting"
	sleep 5
done

say "stopping $unit ($prev)"
if ! systemctl stop "$unit"; then
	systemctl start "$unit" || true
	fail "systemctl stop failed; started $prev again"
fi
count=$(in_flight || echo error)
if [ "$count" != 0 ]; then
	systemctl start "$unit" || true
	fail "in-flight after stop is '$count'; restarted $prev"
fi

backup=/root/40-verified-release.conf.$prev.bak
rollback() {
	if cp -p "$backup" "$dropin" && systemctl daemon-reload && systemctl restart "$unit"; then
		fail "$1; rolled back to $prev"
	fi
	fail "$1; ROLLBACK TO $prev FAILED, $unit needs a hand now"
}
cp -p "$dropin" "$backup"
since=$(date '+%Y-%m-%d %H:%M:%S')
if ! { printf '[Service]\nExecStart=\nExecStart=%s\n' "$bin" >"$dropin.tmp" &&
	mv -f "$dropin.tmp" "$dropin" && systemctl daemon-reload; }; then
	rollback "writing the drop-in failed"
fi
say "starting $unit ($sha)"
systemctl start "$unit" || true

# Up when this start logged its worker start under the new release and has
# not restarted (an explicit start resets NRestarts).
started=no
deadline=$(($(date +%s) + 60))
while [ "$(date +%s)" -lt "$deadline" ]; do
	sleep 3
	[ "$(systemctl is-active "$unit" || true)" = active ] || continue
	[ "$(systemctl show -p NRestarts --value "$unit")" = 0 ] || break
	if journalctl -u "$unit" --since "$since" -o cat --no-pager |
		grep backyard_worker_start | grep -q "sha-$sha"; then
		started=yes
		break
	fi
done
if [ "$started" != yes ]; then
	say "new release did not come up; last journal lines:"
	journalctl -u "$unit" --since "$since" -o cat --no-pager | tail -n 20
	rollback "$sha did not log its worker start within 60 s without restarting"
fi

say "deployed: previous $prev, new $sha, started $since (host time)"
journalctl -u "$unit" --since "$since" -o cat --no-pager | grep -v backyard_heartbeat | tail -n 12
