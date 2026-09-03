#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE="$ROOT/verification/kamino-fleet-parity/kamino-route-v1.json"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
(
 cd "$ROOT"
 cargo build --offline --quiet -p loyal-yield-orchestrator --bin kamino-route-reference
 target/debug/kamino-route-reference < "$FIXTURE" > "$TMP/rust.json"
)
(
 cd "$ROOT/go/kamino-fleet-planner"
 KLEND_PROXY_PATH="$ROOT/target/debug/kamino-route-reference" GOPROXY=off \
   go run ./cmd/loyal-kamino-route-reference < "$FIXTURE" > "$TMP/go.json"
)
python3 - "$TMP/rust.json" "$TMP/go.json" <<'PY'
import json,pathlib,sys
r=json.loads(pathlib.Path(sys.argv[1]).read_text());g=json.loads(pathlib.Path(sys.argv[2]).read_text())
if r!=g:
 fields=[k for k in sorted(set(r)|set(g)) if r.get(k)!=g.get(k)]
 raise SystemExit(f"FAIL: KLend proxy boundary changed route output: {fields}")
if len(g['public'])!=4 or len(g['protected'])!=2:
 raise SystemExit('FAIL: incomplete dynamic route')
print('PASS: Go receives exact dynamic same-mint instructions from the Rust KLend proxy')
PY
