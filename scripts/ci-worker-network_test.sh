#!/usr/bin/env bash
# Real WireGuard/nftables contract test; isolated namespaces, ephemeral keys.
set -euo pipefail
[[ "$EUID" == 0 ]] || { echo 'Run with sudo' >&2; exit 1; }
root=$(cd "$(dirname "$0")/.." && pwd)
server="loyal-ci-server-$$"
client="loyal-ci-client-$$"
target="loyal-ci-target-$$"
temporary=$(mktemp -d)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for namespace in "$client" "$server" "$target"; do ip netns delete "$namespace" 2>/dev/null || true; done
  rm -rf "$temporary"
}
trap cleanup EXIT
trap 'exit 1' INT TERM
for namespace in "$server" "$client" "$target"; do
  ip netns add "$namespace"
  ip -n "$namespace" link set lo up
done
ip -n "$server" link add underlay type veth peer name underlay netns "$client"
ip -n "$server" address add 192.0.2.1/30 dev underlay
ip -n "$client" address add 192.0.2.2/30 dev underlay
ip -n "$server" link set underlay up
ip -n "$client" link set underlay up
ip -n "$server" link add backend type veth peer name backend netns "$target"
ip -n "$server" address add 198.51.100.1/30 dev backend
ip -n "$target" address add 198.51.100.2/30 dev backend
ip -n "$server" link set backend up
ip -n "$target" link set backend up
ip -n "$target" route add 10.203.0.2/32 via 198.51.100.1
ip netns exec "$server" sysctl -q -w net.ipv4.ip_forward=1

export WG_PRIVATE_KEY DEPLOY_HOST WG_PUBLIC_KEY
WG_PRIVATE_KEY=$(wg genkey)
client_public=$(printf '%s\n' "$WG_PRIVATE_KEY" | wg pubkey)
DEPLOY_HOST=192.0.2.1
sed -e "s|CI_PUBLIC_KEY|$client_public|" -e '/^PreUp =/d' -e '/^PostUp =/d' \
  "$root/deploy/hetzner/ci-network/loyal-ci.conf" > "$temporary/loyal-ci.conf"
chmod 600 "$temporary/loyal-ci.conf"
ip netns exec "$server" nft -f "$root/deploy/hetzner/ci-network/loyal-ci.nft"
# Loading twice must remain safe and must not duplicate rules.
ip netns exec "$server" nft -f "$root/deploy/hetzner/ci-network/loyal-ci.nft"
ip netns exec "$server" wg-quick up "$temporary/loyal-ci.conf"
wg genkey | ip netns exec "$server" wg set loyal-ci private-key /dev/stdin
WG_PUBLIC_KEY=$(ip netns exec "$server" wg show loyal-ci public-key)

cat > "$temporary/listen.py" <<'PY'
import socket, sys, threading
def listen(port):
    with socket.socket() as listener:
        listener.bind(("0.0.0.0", port))
        listener.listen()
        while True:
            client, _ = listener.accept()
            client.close()
for port in sys.argv[1:]:
    threading.Thread(target=listen, args=(int(port),)).start()
PY
cat > "$temporary/connect.py" <<'PY'
import socket, sys
try:
    with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1):
        connected = True
except OSError:
    connected = False
assert connected == (sys.argv[3] == "open"), f"{sys.argv[1]}:{sys.argv[2]} unexpected connectivity"
PY
ip netns exec "$server" python3 "$temporary/listen.py" 22 8080 & pids+=("$!")
ip netns exec "$target" python3 "$temporary/listen.py" 8080 & pids+=("$!")
sleep 1
ip netns exec "$client" bash "$root/scripts/ci-worker-network.sh" up
ip netns exec "$client" python3 "$temporary/connect.py" 10.203.0.1 22 open
ip netns exec "$server" python3 "$temporary/connect.py" 10.203.0.1 8080 open
ip netns exec "$client" python3 "$temporary/connect.py" 10.203.0.1 8080 closed

# Even a peer that widens its own routes cannot access the host's private network.
ip netns exec "$client" wg set loyal-ci peer "$WG_PUBLIC_KEY" allowed-ips 10.203.0.1/32,198.51.100.2/32
ip -n "$client" route add 198.51.100.2/32 dev loyal-ci
ip netns exec "$server" python3 "$temporary/connect.py" 198.51.100.2 8080 open
ip netns exec "$client" python3 "$temporary/connect.py" 198.51.100.2 8080 closed
ip netns exec "$server" nft delete table inet loyal_ci
ip netns exec "$client" python3 "$temporary/connect.py" 10.203.0.1 8080 open
ip netns exec "$client" python3 "$temporary/connect.py" 198.51.100.2 8080 open
ip netns exec "$server" nft -f "$root/deploy/hetzner/ci-network/loyal-ci.nft"
ip netns exec "$client" python3 "$temporary/connect.py" 198.51.100.2 8080 closed
ip netns exec "$client" bash "$root/scripts/ci-worker-network.sh" down
ip netns exec "$client" bash "$root/scripts/ci-worker-network.sh" down
if ip -n "$client" link show loyal-ci >/dev/null 2>&1; then exit 1; fi

# A failed handshake must remove the interface and its route.
WG_PUBLIC_KEY=$(wg genkey | wg pubkey)
if ip netns exec "$client" bash "$root/scripts/ci-worker-network.sh" up; then exit 1; fi
if ip -n "$client" link show loyal-ci >/dev/null 2>&1; then exit 1; fi
echo 'Private CI network: SSH allowed, other ports/forwarding blocked, failure cleanup passed'
