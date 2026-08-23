#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

bash "$script_dir/verify-earn-laserstream-reconciliation.sh" "$@"

echo "PASS: finalized failed Earn transactions complete as no-op without starving later work"

