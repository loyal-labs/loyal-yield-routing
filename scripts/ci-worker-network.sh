#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == down ]]; then
  if sudo ip link show dev loyal-ci >/dev/null 2>&1; then
    sudo ip link delete dev loyal-ci
  fi
  exit 0
fi
[[ "$#" == 1 && "$1" == up ]] || { echo 'Usage: ci-worker-network.sh up|down' >&2; exit 1; }

if [[ ! "${DEPLOY_HOST:-}" =~ ^[a-zA-Z0-9.-]+$ ||
      ! "${WG_PUBLIC_KEY:-}" =~ ^[A-Za-z0-9+/]{43}=$ ||
      ! "${WG_PRIVATE_KEY:-}" =~ ^[A-Za-z0-9+/]{43}=$ ]]; then
  echo 'Configure LOYAL_WORKERS_HOST, LOYAL_WORKERS_WIREGUARD_PUBLIC_KEY and LOYAL_WORKERS_WIREGUARD_KEY in production' >&2
  exit 1
fi

# Install the cleanup trap only after creating our own interface.
sudo ip link add dev loyal-ci type wireguard
trap 'status=$?; if (( status != 0 )); then sudo ip link delete dev loyal-ci; fi' EXIT
trap 'exit 1' INT TERM
printf '%s\n' "$WG_PRIVATE_KEY" | sudo wg set loyal-ci private-key /dev/stdin \
  peer "$WG_PUBLIC_KEY" allowed-ips 10.203.0.1/32 \
  endpoint "$DEPLOY_HOST:51820" persistent-keepalive 25
unset WG_PRIVATE_KEY
sudo ip address add 10.203.0.2/32 dev loyal-ci
sudo ip link set dev loyal-ci mtu 1420 up
sudo ip route add 10.203.0.1/32 dev loyal-ci
[[ "$(sudo wg show loyal-ci allowed-ips)" == "$WG_PUBLIC_KEY"$'\t10.203.0.1/32' ]]

for _ in {1..15}; do
  if ping -n -c 1 -W 1 10.203.0.1 >/dev/null 2>&1; then
    echo 'Private workers tunnel connected'
    exit 0
  fi
  sleep 1
done
echo 'Private workers tunnel unavailable; check UDP 51820, the peer keys and wg-quick@loyal-ci' >&2
exit 1
