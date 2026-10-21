#!/bin/sh
# install-rules.sh <sha> <recorded sha|none> <dir> — install the Prometheus
# rules of commit sha, uploaded to dir/monitoring, as
# /etc/prometheus/loyal.rules.yml, and record sha beside it in
# loyal.rules.yml.sha. Runs on the host as root, started by
# scripts/deploy-workers.sh as a transient unit after the engine step. The
# driver installs only over rules from an ancestor of sha (or none) and
# passes the sha it read; under the host's deploy lock the record must still
# say that. The upload is checked first, the file replaced atomically and
# Prometheus must report it loaded; otherwise the previous file comes back.
set -eu

sha=${1:-}
recorded=${2:-}
dir=${3:-}
case $sha in *[!0-9a-f]* | '') sha=bad ;; esac
case $recorded in none) ;; *[!0-9a-f]* | '') recorded=bad ;; esac
case $dir in /tmp/loyal-monitoring.*/*) dir=bad ;; /tmp/loyal-monitoring.*) ;; *) dir=bad ;; esac
if [ ${#sha} -ne 40 ] || [ "$recorded" = bad ] || [ "$dir" = bad ]; then
	echo "usage: install-rules.sh <40-hex commit sha> <40-hex recorded sha|none> /tmp/loyal-monitoring.*" >&2
	exit 2
fi
rules=/etc/prometheus/loyal.rules.yml
monitoring=$dir/monitoring

# A dropped ssh session closes the pipe to the driver; the install must go
# on. Only tee writes to that pipe, and tee -p keeps writing the log without it.
trap '' HUP
mkdir -p /var/log/loyal-deploy
fifo=/run/loyal-deploy-rules.$$.fifo
rm -f "$fifo"
mkfifo -m 600 "$fifo"
tee -p -a "/var/log/loyal-deploy/rules-$sha.log" <"$fifo" &
tee_pid=$!
exec >"$fifo" 2>&1
rm -f "$fifo"
finish() {
	status=$?
	rm -rf "$dir"
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
record() {
	printf '%s\n' "$sha" >"$rules.sha.tmp"
	mv -f "$rules.sha.tmp" "$rules.sha"
}

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

say "install the monitoring rules of $sha"
exec 9>/run/lock/loyal-deploy.lock
if ! flock -w 300 9; then
	fail "another deploy held /run/lock/loyal-deploy.lock for 300 s; nothing changed"
fi
[ -f "$monitoring/loyal.rules.yml" ] || fail "$monitoring/loyal.rules.yml is missing"
[ -f "$rules" ] || fail "$rules is missing; the first monitoring install is manual (deploy/hetzner/README.md)"
now=$(cat "$rules.sha" 2>/dev/null || echo none)
[ "$now" = "$recorded" ] || fail "$rules.sha says $now, not the $recorded the deploy read; another deploy installed rules meanwhile. Rerun the deploy"

if cmp -s "$monitoring/loyal.rules.yml" "$rules"; then
	record
	say "monitoring rules unchanged; recorded $sha"
	exit 0
fi
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
loaded="reload failed"
if ! systemctl reload prometheus || ! loaded=$(rules_loaded "$since" "$rule_count"); then
	mv -f previous.rules.yml "$rules"
	systemctl reload prometheus || true
	fail "Prometheus did not load the new rules ($loaded); restored the previous $rules"
fi
record
say "monitoring rules of $sha installed: Prometheus serves $loaded rules from $rules"
