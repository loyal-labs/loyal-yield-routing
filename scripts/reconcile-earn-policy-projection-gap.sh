#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"

cd "$repo_root"
exec cargo run --quiet -p balance-sweep-ata-monitor \
  --bin earn-policy-projection-gap-reconcile -- "$@"
