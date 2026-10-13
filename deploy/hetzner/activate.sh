#!/bin/sh
# activate.sh <family> <sha> <sha256> <monitoring dir> — switch loyal-<family>
# (backyard or retail) to an uploaded release, then install the Prometheus
# rules that release carries. Runs on the host as root, started by
# scripts/deploy-workers.sh as a transient unit (systemd-run --wait --pipe),
# so a dropped ssh session cannot cut it off mid-swap. One deploy at a time,
# any family: it holds the host's deploy lock.
#
# Backyard stops only when no operation is in flight. Retail has no such
# gate: a stop leaves its signed rows on their operation rows and the next
# start lands them. Either way the new release starts from the
# verified-release drop-in, and the previous drop-in comes back if the new
# release does not come up.
set -eu

family=${1:-}
sha=${2:-}
want=${3:-}
monitoring=${4:-}
case $family in backyard | retail) ;; *) family=bad ;; esac
case $sha in *[!0-9a-f]* | '') sha=bad ;; esac
case $want in *[!0-9a-f]* | '') want=bad ;; esac
# finish removes the monitoring dir's parent: exactly one mktemp dir.
case $monitoring in
*/../* | /tmp/loyal-monitoring.*/*/*) monitoring=bad ;;
/tmp/loyal-monitoring.*/monitoring) ;;
*) monitoring=bad ;;
esac
if [ "$family" = bad ] || [ ${#sha} -ne 40 ] || [ ${#want} -ne 64 ] || [ "$monitoring" = bad ]; then
	echo "usage: activate.sh backyard|retail <40-hex commit sha> <64-hex sha256> /tmp/loyal-monitoring.*/monitoring" >&2
	exit 2
fi
unit=loyal-$family
dropin=/etc/systemd/system/$unit.service.d/40-verified-release.conf
releases=/opt/loyal/releases/$family
bin=$releases/$sha/loyal-engine
rules=/etc/prometheus/loyal.rules.yml

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
	rm -rf "${monitoring%/monitoring}"
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

# install_rules installs the release's loyal.rules.yml when it differs from
# the host's: only after promtool and the alert-message check pass on the
# uploaded copy, atomically, and verified as loaded by Prometheus itself.
# A reload that fails or loads another rule count restores the previous file.
install_rules() {
	if cmp -s "$monitoring/loyal.rules.yml" "$rules"; then
		say "monitoring rules unchanged"
		return 0
	fi
	say "installing the monitoring rules of $sha"
	cd "$monitoring"
	checked=$(promtool check rules loyal.rules.yml 2>&1) || fail "promtool check rules: $checked; $rules not changed"
	rule_count=$(printf '%s\n' "$checked" | sed -n 's/.*SUCCESS: \([0-9][0-9]*\) rules found.*/\1/p')
	[ -n "$rule_count" ] || fail "promtool printed no rule count; $rules not changed"
	promtool test rules loyal.rules.test.yml || fail "promtool test rules failed; $rules not changed"
	python3 verify-alert-messages.py || fail "verify-alert-messages.py failed; $rules not changed"
	cp -p "$rules" previous.rules.yml
	install -m 0644 -o root -g root loyal.rules.yml "$rules.tmp"
	mv -f "$rules.tmp" "$rules"
	since=$(date +%s)
	systemctl reload prometheus
	if ! loaded=$(rules_loaded "$since" "$rule_count"); then
		mv -f previous.rules.yml "$rules"
		systemctl reload prometheus || true
		fail "Prometheus did not load the new rules ($loaded); restored the previous $rules"
	fi
	say "monitoring rules installed: Prometheus serves $loaded rules from $rules"
}

# rules_loaded <since> <count> waits up to 30 s for a successful Prometheus
# config reload at or after since, then prints the number of rules it serves
# from $rules and fails unless that is count.
rules_loaded() {
	python3 - "$1" "$2" "$rules" <<'EOF'
import json, sys, time, urllib.request
since, count, path = float(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
api = "http://127.0.0.1:9090"
deadline = time.time() + 30
while True:
    lines = urllib.request.urlopen(api + "/metrics", timeout=10).read().decode().splitlines()
    reload = dict(line.split(" ", 1) for line in lines if line.startswith("prometheus_config_last_reload_success"))
    if float(reload["prometheus_config_last_reload_success_timestamp_seconds"]) >= since:
        break
    if time.time() > deadline:
        print("no successful reload within 30 s")
        sys.exit(1)
    time.sleep(1)
groups = json.load(urllib.request.urlopen(api + "/api/v1/rules", timeout=10))["data"]["groups"]
loaded = sum(len(g["rules"]) for g in groups if g["file"] == path)
print(loaded)
sys.exit(0 if loaded == count else 1)
EOF
}

say "activate $family $sha"
exec 9>/run/lock/loyal-deploy.lock
if ! flock -w 300 9; then
	fail "another deploy held /run/lock/loyal-deploy.lock for 300 s; nothing changed"
fi

[ -x "$bin" ] || fail "$bin is missing"
got=$(sha256sum "$bin" | cut -d' ' -f1)
[ "$got" = "$want" ] || fail "$bin sha256 is $got, expected $want"
[ -f "$monitoring/loyal.rules.yml" ] || fail "$monitoring/loyal.rules.yml is missing"
[ -f "$rules" ] || fail "$rules is missing; the first monitoring install is manual (deploy/hetzner/README.md)"

[ -f "$dropin" ] || fail "$dropin is missing; the first install is manual (deploy/hetzner/README.md)"
prev=$(sed -n "s|^ExecStart=$releases/\([0-9a-f]\{40\}\)/loyal-engine\$|\1|p" "$dropin")
[ ${#prev} -eq 40 ] || fail "cannot read the running release from $dropin"
state=$(systemctl is-active "$unit" || true)
if [ "$prev" = "$sha" ] && [ "$state" = active ]; then
	say "already running $sha"
	install_rules
	exit 0
fi
if [ "$state" != active ]; then
	fail "$unit is $state before the deploy: someone stopped it on purpose. Start it (or find out why it is down) first."
fi

# Backyard's gate is the new release's own read of the in-flight rows, under
# the unit's own database credential (its path read from the unit, not here).
# `systemctl show` prints encrypted credentials as "[unprintable]", so read
# the unit's own lines. Retail has no gate: in_flight prints 0.
if [ "$family" = backyard ]; then
	dbcred=$(systemctl cat "$unit" |
		sed -n 's/^LoadCredentialEncrypted=\(BACKYARD_DATABASE_URL:[^ ]*\)$/\1/p' | tail -n 1)
	[ -n "$dbcred" ] || fail "no BACKYARD_DATABASE_URL in $unit's LoadCredentialEncrypted"
fi
in_flight() {
	if [ "$family" = retail ]; then
		echo 0
		return
	fi
	systemd-run --quiet --wait --pipe --collect -p DynamicUser=yes \
		-p "LoadCredentialEncrypted=$dbcred" \
		"$bin" backyard in-flight </dev/null
}

if [ "$family" = backyard ]; then
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

backup=/root/$unit-40-verified-release.conf.$prev.bak
rollback() {
	# The previous Backyard release must not inherit a row the new one
	# signed: it may not know how to send it, and it decides nothing while
	# the row stands.
	systemctl stop "$unit" || true
	count=$(in_flight || echo error)
	if [ "$count" != 0 ]; then
		fail "$1; $unit stopped on $sha with '$count' operation(s) in flight; start $sha again to land them before rolling back to $prev"
	fi
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

# Backyard is up when this start logged its worker start under the new
# release and has not restarted (an explicit start resets NRestarts).
# Retail logs no release, so it is up when it is still active with no
# restart 20 s after the start.
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
if [ "$started" != yes ]; then
	say "new release did not come up; last journal lines:"
	journalctl -u "$unit" --since "$since" -o cat --no-pager | tail -n 20
	rollback "$sha did not $up"
fi

say "deployed: previous $prev, new $sha, started $since (host time)"
journalctl -u "$unit" --since "$since" -o cat --no-pager | grep -v _heartbeat | tail -n 12
install_rules
