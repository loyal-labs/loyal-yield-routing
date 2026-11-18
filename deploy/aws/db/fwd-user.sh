#!/usr/bin/env bash
# Run as root on the source box. Makes the one door pg-forward@ walks through: a system user that
# can open forwards to the named Postgres sockets and listen on one reverse port, nothing else.
# usage: fwd-user.sh <user> <from-address> <rev-port> <socket>...   (public key on stdin)
# A socket is the .s.PGSQL.<port> file; the user gets execute on its directory and read-write on it.
# Both the ACLs and the directory vanish when that cluster's service restarts: rerun after it.
set -euo pipefail

user=${1:?user} from=${2:?from address} rev=${3:?reverse port}
shift 3
(($#)) || { echo "fwd-user: no socket named" >&2; exit 2; }
[[ $user =~ ^[a-z][a-z0-9_-]*$ ]] || { echo "fwd-user: bad user $user" >&2; exit 2; }
[[ $from =~ ^[0-9a-fA-F.:/]+$ ]] || { echo "fwd-user: bad from address" >&2; exit 2; }
[[ $rev =~ ^[0-9]+$ ]] && ((rev > 1023 && rev < 65536)) || { echo "fwd-user: bad reverse port" >&2; exit 2; }
key=$(cat)
[[ $key =~ ^ssh-(ed25519|rsa)\ [A-Za-z0-9+/=]+(\ [^\"]*)?$ ]] || { echo "fwd-user: stdin is not one public key" >&2; exit 2; }
for s in "$@"; do [[ -S $s ]] || { echo "fwd-user: $s is not a socket" >&2; exit 2; }; done

changes=0
changed() { changes=$((changes + 1)); echo "changed: $*"; }

if ! id -u "$user" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$user"
  changed "user $user created (system, no home, nologin, locked password)"
fi

keys=/etc/ssh/$user.authorized_keys
line="from=\"$from\",restrict,port-forwarding,command=\"/usr/sbin/nologin\",permitlisten=\"127.0.0.1:$rev\" $key"
if [[ $(cat "$keys" 2>/dev/null || true) != "$line" ]]; then
  (umask 022 && printf '%s\n' "$line" >"$keys.new") && chown root:root "$keys.new" && mv -f "$keys.new" "$keys"
  changed "$keys holds the key, from $from, one reverse listener"
fi

# sshd 9.6 lets a unix-socket -L through only when PermitOpen allows it, and a [path]:* entry is the
# one spelling that names a socket; every TCP destination then falls outside the list.
open=''
for s in "$@"; do open+="[$s]:* "; done
drop=/etc/ssh/sshd_config.d/50-$user.conf
want="Match User $user
    AuthorizedKeysFile $keys
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    AllowTcpForwarding yes
    AllowStreamLocalForwarding local
    PermitListen 127.0.0.1:$rev
    GatewayPorts no
    X11Forwarding no
    AllowAgentForwarding no
    PermitTunnel no
    PermitTTY no
    PermitOpen ${open% }
    ForceCommand /usr/sbin/nologin"
if [[ $(cat "$drop" 2>/dev/null || true) != "$want" ]]; then
  printf '%s\n' "$want" >"$drop.new"
  mv -f "$drop.new" "$drop"
  if ! sshd -t; then
    echo "fwd-user: sshd -t fails with $drop; removing it" >&2
    rm -f "$drop"
    exit 1
  fi
  systemctl reload ssh
  changed "$drop written, sshd -t clean, ssh reloaded"
fi

for s in "$@"; do
  dir=$(dirname "$s")
  if ! getfacl -p "$dir" 2>/dev/null | grep -x "user:$user:--x" >/dev/null; then
    setfacl -m "u:$user:--x" "$dir"
    changed "$dir: $user may traverse"
  fi
  if ! getfacl -p "$s" 2>/dev/null | grep -x "user:$user:rw-" >/dev/null; then
    setfacl -m "u:$user:rw" "$s"
    changed "$s: $user may connect"
  fi
done
echo "fwd-user: $changes change(s)"
