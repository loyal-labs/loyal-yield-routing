#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
comparator="$root/scripts/verify-kamino-planner-revalidator-parity.py"
contract="$root/verification/kamino-fleet-parity/contract-v1.json"
reference_source="$root/crates/loyal-yield-orchestrator/src/bin/kamino-fleet-parity-reference.rs"
candidate_source="$root/go/kamino-fleet-planner/cmd/loyal-kamino-fleet-parity/main.go"

fail() { echo "FAIL: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
usage:
  scripts/verify-kamino-planner-revalidator-parity.sh --self-test
  scripts/verify-kamino-planner-revalidator-parity.sh --compare RUST.json GO.json
  scripts/verify-kamino-planner-revalidator-parity.sh --audit-current

--self-test proves the comparator detects protected-field mutations.
--compare validates and compares two previously generated isolated artifacts.
--audit-current runs the local planner slice and then requires both artifact
producers. It remains red until the Go worker implements complete planner and
route-revalidator parity.
EOF
}

for command_name in python3 shasum jq; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done
[[ -f "$contract" ]] || fail "parity contract is missing"
[[ -x "$comparator" ]] || chmod +x "$comparator"

mode="${1:---audit-current}"
case "$mode" in
  --self-test)
    [[ $# -eq 1 ]] || { usage; exit 2; }
    exec python3 "$comparator" --self-test
    ;;
  --compare)
    [[ $# -eq 3 ]] || { usage; exit 2; }
    exec python3 "$comparator" --reference "$2" --candidate "$3"
    ;;
  --audit-current)
    [[ $# -eq 1 ]] || { usage; exit 2; }
    ;;
  *)
    usage
    exit 2
    ;;
esac

# The audit is intentionally incapable of reading production. Dependency
# resolution is offline, all credential-bearing variables are removed, and the
# candidate's database/RPC evidence must identify disposable/loopback services.
unset NEON_DATABASE_URL TIMESCALEDB_URL SOLANA_RPC_URL SOLANA_WS_URL \
  HELIUS_API_KEY LASERSTREAM_ENDPOINT KAMINO_API_BASE POLICY_KEYPAIR \
  YIELD_ROUTER_KEYPAIR SOLANA_TESTING_PK YIELD_ROUTE_FEE_PAYER_KEYPAIRS
export CARGO_NET_OFFLINE=true
export GOPROXY=off
export GOSUMDB=off
export OBSERVABILITY_ENABLED=false
export NO_PROXY="127.0.0.1,localhost,::1"
export no_proxy="$NO_PROXY"
export HTTP_PROXY="http://127.0.0.1:9"
export HTTPS_PROXY="http://127.0.0.1:9"
export ALL_PROXY="http://127.0.0.1:9"

python3 "$comparator" --self-test

echo "== Current isolated Go planner slice"
(
  cd "$root/go/kamino-fleet-planner"
  go test ./...
  go test -race ./...
)
CARGO_NET_OFFLINE=true "$root/scripts/verify-kamino-fleet-planner-e2e.sh"

echo "== Full replacement artifact producers"
missing=0
if [[ ! -f "$reference_source" ]]; then
  echo "BLOCKED: Rust reference artifact producer is missing: ${reference_source#$root/}" >&2
  missing=1
fi
if [[ ! -f "$candidate_source" ]]; then
  echo "BLOCKED: Go candidate artifact producer is missing: ${candidate_source#$root/}" >&2
  missing=1
fi
if [[ "$missing" -ne 0 ]]; then
  fail "full planner + route-revalidator parity is not implemented; scoped planner checks above are not cutover evidence"
fi

for command_name in cargo go initdb pg_ctl createdb psql; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

scratch="$(mktemp -d /tmp/kamino-fleet-parity.XXXXXX)"
data="$scratch/postgres"
socket="$scratch/socket"
port="$((60100 + RANDOM % 300))"
server_started=0
cleanup() {
  if [[ "$server_started" == 1 ]]; then
    pg_ctl -D "$data" -m immediate -w stop >/dev/null 2>&1 || true
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT

mkdir -p "$socket"
initdb -D "$data" -A trust --no-locale -E UTF8 >/dev/null
pg_ctl -D "$data" -o "-F -k '$socket' -p $port -c listen_addresses=127.0.0.1" -w start >/dev/null
server_started=1
createdb -h "$socket" -p "$port" parity
local_database_url="postgresql://$(id -un)@127.0.0.1:$port/parity"
contract_sha256="$(shasum -a 256 "$contract" | awk '{print $1}')"

cargo build --offline -p loyal-yield-orchestrator --bin kamino-fleet-parity-reference
(
  cd "$root/go/kamino-fleet-planner"
  go build -o "$scratch/loyal-kamino-fleet-parity" ./cmd/loyal-kamino-fleet-parity
)

# Both producers receive the same frozen clock, contract bytes, disposable DB,
# and loopback RPC endpoint. They must emit complete artifacts even for negative
# cases; the comparator rejects missing/skipped evidence.
KAMINO_PARITY_DATABASE_URL="$local_database_url" \
KAMINO_PARITY_RPC_URL="http://127.0.0.1:1" \
KAMINO_PARITY_CLOCK="2026-01-01T00:00:00Z" \
KAMINO_PARITY_CONTRACT_SHA256="$contract_sha256" \
  "$root/target/debug/kamino-fleet-parity-reference" "$contract" >"$scratch/rust.json"
KAMINO_PARITY_DATABASE_URL="$local_database_url" \
KAMINO_PARITY_RPC_URL="http://127.0.0.1:1" \
KAMINO_PARITY_CLOCK="2026-01-01T00:00:00Z" \
KAMINO_PARITY_CONTRACT_SHA256="$contract_sha256" \
  "$scratch/loyal-kamino-fleet-parity" "$contract" >"$scratch/go.json"

for artifact in "$scratch/rust.json" "$scratch/go.json"; do
  jq -e --arg digest "$contract_sha256" '
    .fixture.id == "kamino-planner-revalidator-replacement-v1"
    and .fixture.clock == "2026-01-01T00:00:00Z"
    and .fixture.sha256 == $digest
  ' "$artifact" >/dev/null || fail "artifact is not bound to the exact checked-in contract: $artifact"
done
python3 "$comparator" --reference "$scratch/rust.json" --candidate "$scratch/go.json"
echo "PASS: the Rust planner and revalidator services are replaceable by one isolated Go service"
