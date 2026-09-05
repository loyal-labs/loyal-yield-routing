#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
comparator="$root/scripts/verify-kamino-planner-revalidator-parity.py"
contract="$root/docs/verifiers/kamino-fleet-parity/contract-v1.json"
fail() { echo "FAIL: $*" >&2; exit 1; }
mode="${1:---audit-current}"
case "$mode" in
  --self-test) [[ $# -eq 1 ]] || exit 2; exec python3 "$comparator" --self-test ;;
  --compare) [[ $# -eq 3 ]] || exit 2; exec python3 "$comparator" --reference "$2" --candidate "$3" ;;
  --audit-current) [[ $# -le 1 ]] || exit 2 ;;
  *) echo "usage: $0 [--audit-current | --self-test | --compare RUST.json GO.json]" >&2; exit 2 ;;
esac
# Whitelist environment rather than trying to enumerate every production secret.
# Local PostgreSQL is started by the integration verifier; artifact producers do
# not access a database or RPC. Offline dependency resolution fails closed.
if [[ "${KAMINO_VERIFY_ISOLATED:-}" != 1 ]]; then
  exec env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" \
    KAMINO_VERIFY_ISOLATED=1 bash "$0" --audit-current
fi
export CARGO_NET_OFFLINE=true GOPROXY=off GOSUMDB=off OBSERVABILITY_ENABLED=false
export NO_PROXY="127.0.0.1,localhost,::1" no_proxy="127.0.0.1,localhost,::1"
export HTTP_PROXY="http://127.0.0.1:9" HTTPS_PROXY="http://127.0.0.1:9" ALL_PROXY="http://127.0.0.1:9"
for command_name in python3 shasum jq cargo go; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
python3 "$comparator" --self-test
# Execution-backed database tests and retained Rust lifecycle checks are a
# separate required gate, never replaced by self-reported artifact booleans.
"$root/scripts/verify-kamino-fleet-planner-e2e.sh"
"$root/scripts/verify-kamino-route-parity.sh"
scratch="$(mktemp -d /tmp/kamino-fleet-parity.XXXXXX)"
trap 'rm -rf "$scratch"' EXIT
cd "$root"
cargo build --locked --offline -p loyal-yield-orchestrator --bin kamino-fleet-parity-reference --bin loyal-klend-proxy
proxy="$root/target/debug/loyal-klend-proxy"
export KAMINO_PARITY_KLEND_PROXY="$proxy"
export KAMINO_PARITY_KLEND_PROXY_SHA256="$(shasum -a 256 "$proxy" | awk '{print $1}')"
export KAMINO_PARITY_CONTRACT_SHA256="$(shasum -a 256 "$contract" | awk '{print $1}')"
export KAMINO_PARITY_CLOCK="2026-01-01T00:00:00Z"
(cd "$root/go/kamino-fleet-planner"; go build -o "$scratch/go-parity" ./cmd/loyal-kamino-fleet-parity)
"$root/target/debug/kamino-fleet-parity-reference" "$contract" >"$scratch/rust.json"
"$scratch/go-parity" "$contract" >"$scratch/go.json"
for artifact in "$scratch/rust.json" "$scratch/go.json"; do
  jq -e --arg digest "$KAMINO_PARITY_CONTRACT_SHA256" '
    .fixture.id == "kamino-planner-revalidator-replacement-v1"
    and .fixture.clock == "2026-01-01T00:00:00Z"
    and .fixture.sha256 == $digest
  ' "$artifact" >/dev/null || fail "artifact fixture binding failed"
done
python3 "$comparator" --reference "$scratch/rust.json" --candidate "$scratch/go.json"
echo "PASS: all local Go/database, retained Rust lifecycle, and deterministic parity gates completed"
echo "NOTE: local fixtures do not prove live RPC/Jupiter availability or production cutover safety; follow the shadow rollout gates"
