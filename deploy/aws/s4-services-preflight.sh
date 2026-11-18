#!/usr/bin/env bash
# Read-only gate for the service side of Loyal switch S4 on loyal-main. Starts, enables, writes and copies nothing; the
# ClickStack check only lists what a copy pass would send. Prints "ok <check>: <detail>" or "FAIL <check>: <detail>" per
# check, then "S4-services: <n> check(s), <m> failed"; exit 0 only when m is 0.
#   1 images       both arm64 images exist on the box, their ids equal the unit pins and /opt/loyal/ops/arm64-images
#   2 units        no loyal-* unit is enabled, active or activating, and the two ClickStack units are disabled and inactive
#   3 clickstack   the data pre-copy exists and what a final pass would still send fits the S4 window
#   4 receivers    both receivers' pins agree with the files, the certificate verifies for 127.0.0.1, no 10.92.1.x is left
#   5 web assets   every file of the staged release is in /opt/loyal/assets/web
#   6 neon         the two sealed Neon-URL credentials exist and no unit or drop-in can load them
#   7 host-diff    loyal-workers and loyal-db-primary against loyal-main, every line classified (s4-services-hostdiff.tsv)
# Env, all optional: BOX (loyal-main), SHA (release, default: the release dir on the box), WINDOW_BYTES (1073741824, the
# most a final pass may send), HOST_DIFF_SSH_OPTS (ssh options that reach loyal-db-primary, for example -F <file>).
set -uo pipefail

BOX=${BOX:-loyal-main}
DIR=$(cd "$(dirname "$0")" && pwd)
WINDOW_BYTES=${WINDOW_BYTES:-1073741824}
SSH_CONFIG=/Users/user/loyal/artifacts/orchestrate/aws-migration/proof/loyal-config/ssh_config
[[ -n ${HOST_DIFF_SSH_OPTS:-} || ! -f $SSH_CONFIG ]] || export HOST_DIFF_SSH_OPTS="-F $SSH_CONFIG"
checks=0 failed=0
report() { # report <ok|FAIL> <check> <detail>
  checks=$((checks + 1))
  [[ $1 == ok ]] || failed=$((failed + 1))
  printf '%s %s: %s\n' "$1" "$2" "$3"
}

collect=$(
  ssh -o BatchMode=yes "$BOX" 'sudo -n bash -s' <<'REMOTE'
set -u
ids=/opt/loyal/ops/arm64-images
want_cs=$(sed -n 's/^clickstack=//p' $ids 2>/dev/null); want_relay=$(sed -n 's/^relay=//p' $ids 2>/dev/null)
echo "ids_file ${want_cs:-none} ${want_relay:-none}"
for u in loyal-clickstack loyal-clickstack-relay; do
  pin=$(grep -o -E ' sha256:[0-9a-f]{64} --service' /etc/systemd/system/$u.service 2>/dev/null | awk '{ print $1 }')
  echo "pin $u ${pin:-none}"
done
for id in $want_cs $want_relay; do
  # the id is the single-platform manifest digest; its config blob names the architecture
  arch=$(python3 -I -c 'import json,sys;d="/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/";m=json.load(open(d+sys.argv[1]));print(json.load(open(d+m["config"]["digest"][7:])).get("architecture","?"))' "${id#sha256:}" 2>/dev/null || echo missing)
  echo "image $id $arch"
done
echo "enabled $(systemctl list-unit-files 'loyal-*' --state=enabled --no-legend | wc -l)"
echo "active $(systemctl list-units 'loyal-*' --all --no-legend --state=active,activating,reloading | wc -l)"
for u in loyal-clickstack.service loyal-clickstack-relay.service; do
  echo "unit $u $(systemctl is-enabled $u 2>&1 | sed -n 1p) $(systemctl is-active $u 2>&1 | sed -n 1p)"
done
echo "docker $(systemctl is-active docker 2>&1 | sed -n 1p)"
E=/etc/loyal-observer-monitor S=/etc/loyal-squads-watch Q=/opt/loyal-squads-watch-recovery-v2
echo "cert_verify $(openssl verify -CAfile $E/monitor-ca.pem -verify_ip 127.0.0.1 $E/monitor-server.pem >/dev/null 2>&1 && echo yes || echo no)"
echo "cert_days $(( ( $(date -d "$(openssl x509 -in $E/monitor-server.pem -noout -enddate | cut -d= -f2)" +%s) - $(date +%s) ) / 86400 ))"
echo "marker $([ -e /opt/loyal/ops/receivers-repinned ] && echo yes || echo no)"
python3 -I - <<'PY'
import hashlib, json
sha = lambda p: hashlib.sha256(open(p, 'rb').read()).hexdigest()
E, S, Q = '/etc/loyal-observer-monitor', '/etc/loyal-squads-watch', '/opt/loyal-squads-watch-recovery-v2'
t, p, pins = (json.load(open(f)) for f in (E + '/private-tls.json', S + '/policy.json', Q + '/PINS.json'))
ca, ce = sha(E + '/monitor-ca.pem'), sha(E + '/monitor-server.pem')
print('pins_tls', (t['ca_sha256'], t['server_certificate_sha256']) == (ca, ce))
print('pins_policy', (p['ca_sha256'], p['server_certificate_sha256']) == (ca, ce))
print('pins_manifest', t['manifest_sha256'] == sha(E + '/release-manifest.json'))
print('pins_files', all(sha(Q + '/' + k) == v for k, v in pins.items()))
print('hosts', t['bind_ip'], t['allowed_peer'], t['endpoint'].split('/')[2].split(':')[0])
PY
echo "old_ip_left $(grep -rIl --exclude='*.pre-repin' -e '10\.92\.1\.[12]' $E $S /opt/loyal-observer-monitor $Q 2>/dev/null | wc -l)"
rel=$(ls -d /opt/loyal/releases/web/*/ 2>/dev/null | sed -n '1,2p' | wc -l)
echo "release_dirs $rel"
sha=${SHA:-$(ls /opt/loyal/releases/web | sed -n 1p)}
echo "release_sha $sha"
missing=$(cd /opt/loyal/releases/web/$sha/assets 2>/dev/null && find . -type f | while read -r f; do [ -f /opt/loyal/assets/web/$f ] || echo "$f"; done | wc -l)
echo "assets_missing $missing"
echo "assets_files $(find /opt/loyal/assets/web -type f | wc -l)"
for n in loyal-web-app-db.a347 loyal-web-app-db.owner; do
  echo "neon_file $n $(stat -c '%U:%G %a' /etc/credstore.encrypted/$n 2>/dev/null || echo missing)"
done
units=$(find /etc/systemd/system /usr/lib/systemd/system -type f \( -name '*.service' -o -name '*.conf' -o -name '*.socket' -o -name '*.timer' \) 2>/dev/null)
echo "neon_mentions $(echo "$units" | xargs grep -l -e 'app-db\.a347' -e 'app-db\.owner' 2>/dev/null | wc -l)"
echo "neon_globs $(echo "$units" | xargs grep -h -E '^(ImportCredential|LoadCredentialEncrypted|LoadCredential)=' 2>/dev/null | grep -E '[*?]|credstore\.encrypted/?$' | grep -c -i loyal)"
echo "clickstack_dir $([ -d /srv/loyal-clickstack/.clickstack ] && echo yes || echo no)"
REMOTE
)
get() { sed -n "s/^$1 //p" <<<"$collect" | sed -n 1p; }

# 1 images
read -r _ cs_id relay_id <<<"$(grep '^ids_file' <<<"$collect")"
cs_pin=$(sed -n 's/^pin loyal-clickstack //p' <<<"$collect") relay_pin=$(sed -n 's/^pin loyal-clickstack-relay //p' <<<"$collect")
cs_arch=$(sed -n "s/^image ${cs_id//\//\\/} //p" <<<"$collect") relay_arch=$(sed -n "s/^image ${relay_id//\//\\/} //p" <<<"$collect")
if [[ $cs_id == "$cs_pin" && $relay_id == "$relay_pin" && $cs_arch == arm64 && $relay_arch == arm64 && $cs_id != none ]]; then
  report ok images "clickstack ${cs_id:7:12} and relay ${relay_id:7:12} are arm64 and pinned in the units"
else
  report FAIL images "ids file $cs_id/$relay_id, unit pins $cs_pin/$relay_pin, arch ${cs_arch:-?}/${relay_arch:-?}"
fi

# 2 units
en=$(get enabled) ac=$(get active)
u1=$(sed -n 's/^unit loyal-clickstack.service //p' <<<"$collect") u2=$(sed -n 's/^unit loyal-clickstack-relay.service //p' <<<"$collect")
if [[ $en == 0 && $ac == 0 && $u1 == "disabled inactive" && $u2 == "disabled inactive" ]]; then
  report ok units "0 loyal-* units enabled, 0 active; both ClickStack units disabled and inactive; docker $(get docker)"
else
  report FAIL units "enabled $en, active $ac, clickstack '$u1', relay '$u2'"
fi

# 3 clickstack data (read-only listing)
cd_out=$("$DIR/loyal-clickstack-copy.sh" delta 2>&1)
cd_line=$(grep ' delta: ' <<<"$cd_out" | sed -n 1p)
cd_bytes=$(sed -n 's/.*to copy [0-9]* entries (\([0-9]*\) bytes).*/\1/p' <<<"$cd_line")
if [[ $(get clickstack_dir) == yes && -n $cd_bytes && $cd_bytes -le $WINDOW_BYTES ]]; then
  report ok clickstack "a final pass would send $cd_bytes bytes (limit $WINDOW_BYTES); ${cd_line#* delta: }"
else
  report FAIL clickstack "dir $(get clickstack_dir), delta '${cd_line:-no output}', limit $WINDOW_BYTES"
fi

# 4 receivers
hosts=$(get hosts)
if [[ $(get cert_verify) == yes && $(get pins_tls) == True && $(get pins_policy) == True && $(get pins_manifest) == True &&
  $(get pins_files) == True && $(get old_ip_left) == 0 && $(get marker) == yes && $hosts == "127.0.0.1 127.0.0.1 127.0.0.1" ]]; then
  report ok receivers "pins match, certificate verifies for 127.0.0.1 ($(get cert_days) days left), bind/peer/endpoint $hosts, 0 files name 10.92.1.x"
else
  report FAIL receivers "verify $(get cert_verify), tls $(get pins_tls), policy $(get pins_policy), manifest $(get pins_manifest), files $(get pins_files), old ips $(get old_ip_left), marker $(get marker), hosts $hosts"
fi

# 5 web assets
if [[ $(get assets_missing) == 0 && $(get release_dirs) == 1 && $(get assets_files) -gt 0 ]]; then
  report ok "web assets" "release $(get release_sha | cut -c1-9): 0 files missing from /opt/loyal/assets/web ($(get assets_files) files)"
else
  report FAIL "web assets" "missing $(get assets_missing), release dirs $(get release_dirs), files $(get assets_files)"
fi

# 6 neon credentials
if [[ $(grep -c '^neon_file .* root:root 600$' <<<"$collect") == 2 && $(get neon_mentions) == 0 && $(get neon_globs) == 0 ]]; then
  report ok neon "loyal-web-app-db.a347 and .owner are sealed root:root 600, named by 0 unit files or drop-ins, 0 loyal credential globs"
else
  report FAIL neon "$(grep '^neon_' <<<"$collect" | tr '\n' ';')"
fi

# 7 host-diff, classified
unclassified=0 classified=0 union=0 tmp=$(mktemp -d)
trap 'rm -r "$tmp"' EXIT
"$DIR/host-diff.sh" loyal-workers "$BOX" >"$tmp/w" 2>&1 || true
"$DIR/host-diff.sh" loyal-db-primary "$BOX" >"$tmp/d" 2>&1 || true
for f in w d; do grep -q 'difference(s)$' "$tmp/$f" || { report FAIL host-diff "host-diff.sh produced no summary for $f: $(head -1 "$tmp/$f")"; unclassified=1000; }; done
grep -v 'difference(s)$' "$tmp/w" | sort >"$tmp/w.l"
grep -v 'difference(s)$' "$tmp/d" | sort >"$tmp/d.l"
grep 'only on new' "$tmp/w.l" >"$tmp/w.new" || true
grep 'only on new' "$tmp/d.l" >"$tmp/d.new" || true
while IFS= read -r line; do
  [[ -n $line ]] || continue
  in_w=0 in_d=0
  grep -q -F -x -- "$line" "$tmp/w.new" && in_w=1
  grep -q -F -x -- "$line" "$tmp/d.new" && in_d=1
  if ((in_w != in_d)); then
    union=$((union + 1)) # only on new against one old box, so the other old box holds it
    continue
  fi
  hit=0
  while IFS=$'\t' read -r re _; do
    [[ $re == '#'* || -z $re ]] && continue
    if [[ $line =~ $re ]]; then hit=1; break; fi
  done <"$DIR/s4-services-hostdiff.tsv"
  if ((hit)); then classified=$((classified + 1)); else unclassified=$((unclassified + 1)); echo "  unclassified: $line"; fi
done < <(sort -u "$tmp/w.l" "$tmp/d.l")
if ((unclassified == 0)); then
  report ok host-diff "0 unclassified lines ($union held by the other old box, $classified in s4-services-hostdiff.tsv); workers $(tail -1 "$tmp/w"), db-primary $(tail -1 "$tmp/d")"
else
  report FAIL host-diff "$unclassified unclassified line(s)"
fi

echo "S4-services: $checks check(s), $failed failed"
((failed == 0))
