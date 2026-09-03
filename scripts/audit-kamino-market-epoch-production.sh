#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[[ -n "${TIMESCALE_DATABASE_URL:-${TIMESCALEDB_URL:-}}" ]] || {
  echo "FAIL: TIMESCALE_DATABASE_URL or TIMESCALEDB_URL is required" >&2
  exit 1
}
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

(
  cd "$ROOT/go/kamino-fleet-planner"
  GOPROXY=off go run ./cmd/loyal-kamino-market-epoch --live --emit-fixture > "$TMP/fixture.json"
  GOPROXY=off go run ./cmd/loyal-kamino-market-epoch < "$TMP/fixture.json" > "$TMP/go.json"
)
(
  cd "$ROOT"
  cargo run --offline --quiet -p loyal-yield-orchestrator --bin kamino-market-epoch-reference \
    < "$TMP/fixture.json" > "$TMP/rust.json"
)
python3 - "$TMP/rust.json" "$TMP/go.json" <<'PY'
import json, pathlib, sys
rust = json.loads(pathlib.Path(sys.argv[1]).read_text())
go = json.loads(pathlib.Path(sys.argv[2]).read_text())
if rust != go:
    fields = [key for key in sorted(set(rust) | set(go)) if rust.get(key) != go.get(key)]
    raise SystemExit(f"FAIL: production Rust/Go immutable epoch mismatch: {fields}")
coverage = go["mintCoverage"]
if len(coverage) != 1 or not coverage[0]["complete"]:
    raise SystemExit("FAIL: production USDC mint frontier is incomplete")
if len(go["reserves"]) < 3 or go["catalogReserveCount"] != coverage[0]["catalogReserveCount"]:
    raise SystemExit("FAIL: production material frontier or catalog denominator is incomplete")
if any(row["stateEventId"] <= 0 or len(row["accountDataHash"]) != 64 for row in go["reserves"]):
    raise SystemExit("FAIL: production monitor state identity is incomplete")
print(json.dumps({
    "status": "PASS",
    "catalogReserveCount": go["catalogReserveCount"],
    "admittedReserveCount": len(go["reserves"]),
    "eligibleTargetReserveCount": coverage[0]["eligibleTargetReserveCount"],
    "blockerCodes": sorted({item["code"] for item in coverage[0]["blockers"]}),
    "message": "production monitor evidence emits the same Rust and Go ImmutableMarketEpoch"
}, separators=(",", ":")))
PY
