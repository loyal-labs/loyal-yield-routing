#!/bin/sh
# activate.sh <family> <sha> <sha256> [stopped] — switch loyal-<family>
# (backyard, retail, observer or web) to an uploaded release. Runs on the host as
# root, started by scripts/deploy-workers.sh as a transient unit
# (systemd-run --wait --pipe), so a dropped ssh session cannot cut it off
# mid-swap. One deploy at a time, any family: it holds the host's deploy lock.
#
# Backyard stops only when no operation is in flight. Retail and the observer
# have no such gate: a stop leaves retail's signed rows on their operation
# rows and the next start lands them. Either way the new release starts from
# the verified-release drop-in, and the previous drop-in comes back if the new
# release does not come up.
#
# A running unit is the normal case. "stopped" says the operator stopped the
# unit on purpose (to apply migrations): it must be inactive, the deploy
# starts it on the new release, and a rollback leaves it stopped on the
# previous drop-in.
#
# web (loyal-web) is up when it has stayed active without restarting for 20 s
# and then /readyz on the unit's LOYAL_WEB_ADDRESS returns 200 naming
# X-Loyal-Release: <sha>.
set -eu

family=${1:-}
sha=${2:-}
want=${3:-}
mode=${4:-running}
case $family in backyard | retail) binary=loyal-engine ;; observer) binary=loyal-observer ;; web) binary=loyal-web ;; *) family=bad ;; esac
case $sha in *[!0-9a-f]* | '') sha=bad ;; esac
case $want in *[!0-9a-f]* | '') want=bad ;; esac
case $mode in running | stopped) ;; *) mode=bad ;; esac
if [ "$family" = bad ] || [ ${#sha} -ne 40 ] || [ ${#want} -ne 64 ] || [ "$mode" = bad ]; then
	echo "usage: activate.sh backyard|retail|observer|web <40-hex commit sha> <64-hex sha256> [stopped]" >&2
	exit 2
fi
unit=loyal-$family
dropin=/etc/systemd/system/$unit.service.d/40-verified-release.conf
releases=/opt/loyal/releases/$family
bin=$releases/$sha/$binary

# A dropped ssh session closes the pipe to the driver; the swap must go on.
# Only tee writes to that pipe, and tee -p keeps writing the log without it.
trap '' HUP
mkdir -p /var/log/loyal-deploy
fifo=/run/loyal-deploy-$family.$$.fifo
rm -f "$fifo"
mkfifo -m 600 "$fifo"
tee -p -a "/var/log/loyal-deploy/$family-$sha.log" <"$fifo" &
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

say "activate $family $sha"
exec 9>/run/lock/loyal-deploy.lock
if ! flock -w 300 9; then
	fail "another deploy held /run/lock/loyal-deploy.lock for 300 s; nothing changed"
fi

[ -x "$bin" ] || fail "$bin is missing"
got=$(sha256sum "$bin" | cut -d' ' -f1)
[ "$got" = "$want" ] || fail "$bin sha256 is $got, expected $want"

[ -f "$dropin" ] || fail "$dropin is missing; the first install is manual (deploy/hetzner/README.md)"
prev=$(sed -n "s|^ExecStart=$releases/\([0-9a-f]\{40\}\)/$binary\$|\1|p" "$dropin")
[ ${#prev} -eq 40 ] || fail "cannot read the running release from $dropin"
state=$(systemctl is-active "$unit" || true)
if [ "$prev" = "$sha" ] && [ "$state" = active ]; then
	say "already running $sha"
	exit 0
fi
if [ "$mode" = running ] && [ "$state" != active ]; then
	fail "$unit is $state before the deploy: someone stopped it on purpose. Start it (or find out why it is down) first, or deploy with --stopped."
fi
if [ "$mode" = stopped ] && [ "$state" = active ]; then
	fail "--stopped, but $unit is active; nothing changed"
fi

# Backyard's gate is the new release's own read of the in-flight rows, under
# the unit's own database credential (its path read from the unit, not here).
# `systemctl show` prints encrypted credentials as "[unprintable]", so read
# the unit's own lines. Retail and the observer have no gate: in_flight
# prints 0.
if [ "$family" = backyard ]; then
	dbcred=$(systemctl cat "$unit" |
		sed -n 's/^LoadCredentialEncrypted=\(BACKYARD_DATABASE_URL:[^ ]*\)$/\1/p' | tail -n 1)
	[ -n "$dbcred" ] || fail "no BACKYARD_DATABASE_URL in $unit's LoadCredentialEncrypted"
fi
if [ "$family" = web ]; then
	webaddr=$(systemctl cat "$unit" |
		sed -n 's/^Environment=LOYAL_WEB_ADDRESS=\([^ ]*\)$/\1/p' | tail -n 1)
	[ -n "$webaddr" ] || fail "no LOYAL_WEB_ADDRESS in $unit; cannot check /readyz"
fi
in_flight() {
	if [ "$family" != backyard ]; then
		echo 0
		return
	fi
	systemd-run --quiet --wait --pipe --collect -p DynamicUser=yes \
		-p "LoadCredentialEncrypted=$dbcred" \
		"$bin" backyard in-flight </dev/null
}

if [ "$family" = backyard ] && [ "$mode" = running ]; then
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
else
	say "previous release $prev"
fi

if [ "$mode" = running ]; then
	say "stopping $unit ($prev)"
	if ! systemctl stop "$unit"; then
		systemctl start "$unit" || true
		fail "systemctl stop failed; started $prev again"
	fi
fi
count=$(in_flight || echo error)
if [ "$count" != 0 ]; then
	[ "$mode" = stopped ] || systemctl start "$unit" || true
	fail "in-flight after stop is '$count'; $unit left on $prev ($mode before the deploy)"
fi

backup=/root/$unit-40-verified-release.conf.$prev.bak
rollback() {
	systemctl stop "$unit" || true
	count=$(in_flight || echo error)
	if [ "$count" != 0 ]; then
		fail "$1; $unit stopped on $sha with '$count' operation(s) in flight; start $sha again to land them before rolling back to $prev"
	fi
	if [ "$mode" = stopped ]; then
		if cp -p "$backup" "$dropin" && systemctl daemon-reload; then
			fail "$1; rolled back to $prev, left stopped as before the deploy"
		fi
	elif cp -p "$backup" "$dropin" && systemctl daemon-reload && systemctl restart "$unit"; then
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

started=no
if [ "$family" = backyard ]; then
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
	up="log its worker start within 60 s without restarting"
else
	deadline=$(($(date +%s) + 20))
	started=yes
	while [ "$(date +%s)" -lt "$deadline" ]; do
		sleep 2
		[ "$(systemctl show -p NRestarts --value "$unit")" = 0 ] || {
			started=no
			break
		}
	done
	[ "$(systemctl is-active "$unit" || true)" = active ] || started=no
	up="stay active without restarting for 20 s"
fi

if [ "$family" = web ] && [ "$started" = yes ]; then
	say "waiting for http://$webaddr/readyz to name $sha (up to 30 s)"
	deadline=$(($(date +%s) + 30))
	ready=no
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if curl -fsS -o /dev/null -D- "http://$webaddr/readyz" 2>/dev/null |
			grep -i "^x-loyal-release: $sha"; then
			ready=yes
			break
		fi
		sleep 2
	done
	if [ "$ready" != yes ]; then
		started=no
		up="serve /readyz with X-Loyal-Release: $sha"
	fi
fi
if [ "$started" != yes ]; then
	say "new release did not come up; last journal lines:"
	journalctl -u "$unit" --since "$since" -o cat --no-pager | tail -n 20
	rollback "$sha did not $up"
fi

say "deployed: previous $prev, new $sha, started $since (host time)"
journalctl -u "$unit" --since "$since" -o cat --no-pager | grep -v _heartbeat | tail -n 12
