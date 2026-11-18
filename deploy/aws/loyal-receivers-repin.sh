#!/usr/bin/env bash
# Re-pins the two private receivers for the single-box layout on loyal-main: both listen on 127.0.0.1 and the only
# peer is 127.0.0.1, where the old layout had bind 10.92.1.1 and peer 10.92.1.2 across two Hetzner boxes.
#   loyal-receivers-repin.sh apply       rewrites the pinned files on loyal-main, prints changes, ends "<n> change(s)"
#   loyal-receivers-repin.sh state       copies the receivers' state dirs from loyal-db-primary (needs the old receivers
#                                        stopped for the copy to be final; without them it is a pre-copy and says so)
# What changes (all root-owned 0600 under /etc/loyal-observer-monitor, /etc/loyal-squads-watch, /opt/loyal-*):
#   the three transport.py (HOST and PEER constants), PINS.json (the two transport hashes), firewall.nft,
#   private-tls.json and policy.json (the CA and server certificate hashes), monitor-ca.pem and monitor-server.pem.
# The server certificate names only IP 10.92.1.1, so a new CA signs a new certificate for the SAME public key (the
# sealed observer-tls-key still matches), valid for 127.0.0.1. The CA key lives in /dev/shm for the run and is shredded;
# nothing can mint another certificate. Originals stay beside the new files as *.pre-repin. Reads or prints no key.
# Writes /opt/loyal/ops/receivers-repinned, which loyal-main-config.sh reads to leave these files alone on a rerun.
# Env, all optional: BOX (loyal-main).
set -euo pipefail

BOX=${BOX:-loyal-main}
DIR=$(cd "$(dirname "$0")" && pwd)
mode=${1:-}
case $mode in apply | state) ;; *) echo "usage: $0 apply|state" >&2; exit 2 ;; esac

if [[ $mode == state ]]; then
  # shellcheck source=/dev/null
  source "$DIR/../hetzner/db/remote.sh"
  active=$(box 'systemctl is-active loyal-observer-private-receiver loyal-squads-watch-receiver loyal-squads-watch-tick.timer | grep -c "^active" || true') ||
    { echo "state: the old receivers' state could not be read on loyal-db-primary; nothing copied" >&2; exit 1; }
  [[ $active =~ ^[0-9]+$ ]] || { echo "state: the old receivers' is-active read answered '$active', not a count; nothing copied" >&2; exit 1; }
  for d in loyal-observer-monitor loyal-squads-watch; do
    box "tar -C /var/lib/$d -cf - --numeric-owner ." |
      ssh -o BatchMode=yes "$BOX" "sudo -n bash -c 'install -d -m 0700 -o root -g root /var/lib/$d && tar -C /var/lib/$d -xpf - --same-owner --numeric-owner'"
    echo "/var/lib/$d: copied"
  done
  if ((active > 0)); then
    echo "pre-copy only: $active of the old receivers and tick timer are still active; stop them and rerun for the final copy"
  else
    echo "final copy: the old receivers and tick timer are stopped"
  fi
  exit 0
fi

ssh -o BatchMode=yes "$BOX" 'sudo -n bash -s' <<'REMOTE' | awk '/^changed:/ { n++ } { print } END { print "receivers on loyal-main: " n + 0 " change(s)" }'
set -euo pipefail
E=/etc/loyal-observer-monitor S=/etc/loyal-squads-watch Q=/opt/loyal-squads-watch-recovery-v2
OBS=/opt/loyal-observer-monitor/observer-monitor/push/private-tls/transport.py
changed() { echo "changed: $*"; }
keep() { [ -e "$1.pre-repin" ] || cp -p "$1" "$1.pre-repin"; }

if ! openssl x509 -in $E/monitor-server.pem -noout -ext subjectAltName 2>/dev/null | grep -q 'IP Address:127\.0\.0\.1'; then
  t=$(mktemp -d -p /dev/shm)
  trap 'shred -u "$t"/ca.key 2>/dev/null || true; rm -r "$t"' EXIT
  umask 077
  openssl genpkey -algorithm ED25519 -out "$t/ca.key"
  openssl req -x509 -new -key "$t/ca.key" -subj '/CN=Loyal-observer-private-CA' -days 730 \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign' -out "$t/ca.pem"
  openssl x509 -in $E/monitor-server.pem -noout -pubkey >"$t/pub.pem"
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1\n' >"$t/ext"
  openssl x509 -new -force_pubkey "$t/pub.pem" -subj '/CN=Loyal-observer-private' -CA "$t/ca.pem" -CAkey "$t/ca.key" \
    -days 365 -extfile "$t/ext" -out "$t/server.pem"
  openssl verify -CAfile "$t/ca.pem" -verify_ip 127.0.0.1 "$t/server.pem" >/dev/null
  [ "$(openssl x509 -in "$t/server.pem" -noout -pubkey)" = "$(cat "$t/pub.pem")" ]
  for f in monitor-ca.pem monitor-server.pem; do keep $E/$f; done
  install -m 0600 -o root -g root "$t/ca.pem" $E/monitor-ca.pem
  install -m 0600 -o root -g root "$t/server.pem" $E/monitor-server.pem
  shred -u "$t/ca.key"
  changed "$E/monitor-ca.pem and monitor-server.pem (new CA, same public key, IP 127.0.0.1)"
fi

for f in $OBS $Q/dependencies/transport.py $Q/transport.py $S/firewall.nft; do
  if grep -q -e '10\.92\.1\.[12]' "$f"; then
    keep "$f"
    sed -i -e 's/10\.92\.1\.1/127.0.0.1/g' -e 's/10\.92\.1\.2/127.0.0.1/g' "$f"
    changed "$f"
  fi
done

python3 -I - <<'PY'
import hashlib, json, os, sys
E, S, Q = '/etc/loyal-observer-monitor', '/etc/loyal-squads-watch', '/opt/loyal-squads-watch-recovery-v2'
sha = lambda p: hashlib.sha256(open(p, 'rb').read()).hexdigest()
def rewrite(path, mutate):
    raw = open(path, 'rb').read()
    nl = raw.endswith(b'\n')
    for indent in (None, 2):
        canon = (json.dumps(json.loads(raw), sort_keys=True, indent=indent, separators=(',', ':') if indent is None else None).encode()) + (b'\n' if nl else b'')
        if canon == raw:
            break
    else:
        sys.exit(path + ': not canonical JSON, refusing to rewrite')
    d = json.loads(raw)
    mutate(d)
    out = json.dumps(d, sort_keys=True, indent=indent, separators=(',', ':') if indent is None else None).encode() + (b'\n' if nl else b'')
    if out != raw:
        if not os.path.exists(path + '.pre-repin'):
            open(path + '.pre-repin', 'wb').write(raw); os.chmod(path + '.pre-repin', 0o600)
        mode = os.stat(path).st_mode & 0o777
        tmp = path + '.repin'
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
        os.write(fd, out); os.close(fd); os.replace(tmp, path)
        print('changed: ' + path)
ca, cert = sha(E + '/monitor-ca.pem'), sha(E + '/monitor-server.pem')
def tls(d):
    d['ca_sha256'], d['server_certificate_sha256'] = ca, cert
    d['endpoint'] = d['endpoint'].replace('10.92.1.1', '127.0.0.1')
    d['bind_ip'], d['allowed_peer'] = '127.0.0.1', '127.0.0.1'
def policy(d): d['ca_sha256'], d['server_certificate_sha256'] = ca, cert
def pins(d):
    d['transport.py'] = sha(Q + '/transport.py')
    d['dependencies/transport.py'] = sha(Q + '/dependencies/transport.py')
rewrite(E + '/private-tls.json', tls)
rewrite(S + '/policy.json', policy)
rewrite(Q + '/PINS.json', pins)
PY
install -d -m 0755 /opt/loyal/ops
[ -e /opt/loyal/ops/receivers-repinned ] || { date -u +%FT%TZ >/opt/loyal/ops/receivers-repinned; echo 'changed: /opt/loyal/ops/receivers-repinned'; }
REMOTE
