#!/usr/bin/env bash
# Diagnostic, not a release gate. Nonzero means decisions differ or tools failed.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# != 1 || "$1" != /* || -e "$1" ]]; then
  echo "usage: $0 /absolute/new-output-directory" >&2
  exit 2
fi
if [[ "${FLEET_DECISION_ISOLATED:-}" != 1 ]]; then
  exec env -i PATH="$PATH" HOME="$HOME" TMPDIR="${TMPDIR:-/tmp}" FLEET_DECISION_ISOLATED=1 bash "$0" "$1"
fi
mkdir -m 700 "$1"
out="$1"
export CARGO_NET_OFFLINE=true GOPROXY=off GOSUMDB=off OBSERVABILITY_ENABLED=false
export HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 ALL_PROXY=http://127.0.0.1:9
export FLEET_DECISION_FIXTURE="$out/fixture.json"
python3 "$root/scripts/test_compare_fleet_decisions.py"
python3 "$root/scripts/compare-fleet-decisions.py" --generate "$FLEET_DECISION_FIXTURE"
cd "$root"
FLEET_DECISION_OUTPUT="$out/rust.json" cargo test --locked --offline -p loyal-yield-orchestrator --bin fleet-opportunity-planner \
  -- --exact decision_parity::produce_shared_input_decisions --ignored >"$out/rust.log" 2>&1
cd "$root/go/kamino-fleet-planner"
go build -o "$out/go-decisions" ./cmd/loyal-fleet-decision-parity
FLEET_DECISION_OUTPUT="$out/go.json" "$out/go-decisions"
status=0
python3 "$root/scripts/compare-fleet-decisions.py" --fixture "$FLEET_DECISION_FIXTURE" --rust "$out/rust.json" --go "$out/go.json" >"$out/report.json" || status=$?
python3 - "$out/report.json" <<'PY'
import json,sys
r=json.load(open(sys.argv[1]))
print(json.dumps({k:r[k] for k in ('fixtureSha256','caseCount','matchingCases','mismatchingCases')}))
for d in r['differences']:
    print(d['name'], 'rust-selected='+str(len(d['rust'])), 'go-selected='+str(len(d['go'])))
PY
printf 'Decision artifacts: %s\n' "$out"
exit "$status"
