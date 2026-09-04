#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURE="$ROOT/docs/verifiers/kamino-fleet-parity/kamino-route-v1.json"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
python3 - "$FIXTURE" "$TMP/proxy-input.json" <<'PY'
import json,pathlib,sys
request=json.loads(pathlib.Path(sys.argv[1]).read_text())
pathlib.Path(sys.argv[2]).write_text(json.dumps({"schemaVersion":1,"operation":"buildSameMintRoute","request":request}))
PY
(
 cd "$ROOT"
 cargo build --offline --quiet -p loyal-yield-orchestrator --bin loyal-klend-proxy
 target/debug/loyal-klend-proxy < "$TMP/proxy-input.json" > "$TMP/rust-envelope.json"
)
PROXY_SHA256="$(python3 - "$ROOT/target/debug/loyal-klend-proxy" <<'PY'
import hashlib,pathlib,sys
print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())
PY
)"
(
 cd "$ROOT/go/kamino-fleet-planner"
 KLEND_PROXY_PATH="$ROOT/target/debug/loyal-klend-proxy" KLEND_PROXY_SHA256="$PROXY_SHA256" GOPROXY=off \
   go run ./cmd/loyal-kamino-route-proxy-client < "$FIXTURE" > "$TMP/go.json"
)
python3 - "$TMP/rust-envelope.json" "$TMP/go.json" <<'PY'
import json,pathlib,sys
raw=json.loads(pathlib.Path(sys.argv[1]).read_text());g=json.loads(pathlib.Path(sys.argv[2]).read_text())
if raw.get('schemaVersion')!=1 or raw.get('operation')!='buildSameMintRoute':
 raise SystemExit('FAIL: KLend proxy envelope drifted')
r=raw['route']
if r!=g:
 fields=[k for k in sorted(set(r)|set(g)) if r.get(k)!=g.get(k)]
 raise SystemExit(f"FAIL: KLend proxy boundary changed route output: {fields}")
if len(g['public'])!=4 or len(g['protected'])!=2:
 raise SystemExit('FAIL: incomplete dynamic route')
print('PASS: Go receives exact dynamic same-mint instructions from the isolated Rust KLend proxy')
PY
