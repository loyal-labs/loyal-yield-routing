#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE="$ROOT/verification/kamino-fleet-parity/market-epoch-v1.json"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

python3 - "$FIXTURE" "$TMP/blocked.json" <<'PY'
import json, pathlib, sys
fixture = json.loads(pathlib.Path(sys.argv[1]).read_text())
fixture["verifiedReserves"][1]["reserveLastUpdateStale"] = True
fixture["verifiedReserves"][1]["reserveLastUpdateSlot"] = -600
fixture["verifiedReserves"][0]["accountDataHash"] = "bad"
pathlib.Path(sys.argv[2]).write_text(json.dumps(fixture))
PY

for CASE in complete blocked; do
  INPUT="$FIXTURE"
  if [[ "$CASE" == blocked ]]; then INPUT="$TMP/blocked.json"; fi
  (
    cd "$ROOT"
    cargo run --offline --quiet -p loyal-yield-orchestrator --bin kamino-market-epoch-reference \
      < "$INPUT" > "$TMP/rust-$CASE.json"
  )
  (
    cd "$ROOT/go/kamino-fleet-planner"
    GOPROXY=off go run ./cmd/loyal-kamino-market-epoch \
      < "$INPUT" > "$TMP/go-$CASE.json"
  )
done
python3 - "$TMP/rust-complete.json" "$TMP/go-complete.json" "$TMP/rust-blocked.json" "$TMP/go-blocked.json" <<'PY'
import json, pathlib, sys
rust = json.loads(pathlib.Path(sys.argv[1]).read_text())
go = json.loads(pathlib.Path(sys.argv[2]).read_text())
rust_blocked = json.loads(pathlib.Path(sys.argv[3]).read_text())
go_blocked = json.loads(pathlib.Path(sys.argv[4]).read_text())
for name, expected, actual in (("complete", rust, go), ("blocked", rust_blocked, go_blocked)):
    if expected != actual:
        for key in sorted(set(expected) | set(actual)):
            if expected.get(key) != actual.get(key):
                print(f"DIFF ({name}): {key}", file=sys.stderr)
        raise SystemExit(f"FAIL: Go {name} immutable market epoch differs from Rust")
if len(go["reserves"]) < 3:
    raise SystemExit("FAIL: parity fixture does not exercise a material reserve frontier")
coverage = go["mintCoverage"]
if len(coverage) != 1 or not coverage[0]["complete"] or coverage[0]["catalogReserveCount"] != 3:
    raise SystemExit("FAIL: mint coverage is incomplete")
if go["optimizerEpochId"] <= 0 or len(go["fingerprint"]) != 64 or len(go["catalogFingerprint"]) != 64:
    raise SystemExit("FAIL: canonical epoch identity is invalid")
for reserve in go["reserves"]:
    if reserve["stateEventId"] <= 0 or len(reserve["accountDataHash"]) != 64:
        raise SystemExit("FAIL: durable monitor state identity is absent")
blocked_codes = {item["code"] for item in go_blocked["mintCoverage"][0]["blockers"]}
if blocked_codes != {"invalid_state_identity", "explicit_stale_economics", "invalid_economic_slot_order"}:
    raise SystemExit(f"FAIL: blocker evidence drifted: {sorted(blocked_codes)}")
print("PASS: Go emits the complete Rust-compatible ImmutableMarketEpoch contract")
PY
