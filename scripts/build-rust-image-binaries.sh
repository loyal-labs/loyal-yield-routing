#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

# SQLx queries used by these binaries are checked against the committed query
# metadata. The image build must not depend on a live database.
export SQLX_OFFLINE=true

# This is intentionally one Cargo invocation. Cargo resolves the union of the
# selected packages once and shares dependency artifacts across every binary.
cargo build --release --locked \
  -p balance-sweep-ata-monitor \
  -p balance-sweep-ata-projector \
  -p balance-sweep-autodeposit-trigger \
  -p kamino-reserve-monitor \
  -p loyal-fleet-worker \
  -p loyal-squads-policy-monitor \
  -p loyal-timescale-migrations \
  -p loyal-yield-orchestrator \
  -p loyal-yield-realtime \
  --bin balance-sweep-ata-monitor \
  --bin balance-sweep-ata-projector \
  --bin balance-sweep-autodeposit-trigger \
  --bin fleet-health-projector \
  --bin fleet-opportunity-planner \
  --bin fleet-orchestration-production-evidence \
  --bin fleet-orchestration-verifier \
  --bin fleet-route-confirmer \
  --bin kamino-reserve-monitor \
  --bin loyal-timescale-migrations \
  --bin loyal-squads-policy-monitor \
  --bin loyal-yield-realtime \
  --bin route-lookup-table-alert-monitor \
  --bin route-lookup-table-cleanup \
  --bin route-lookup-table-legacy-import \
  --bin route-lookup-table-provisioner \
  --bin route-lookup-table-shared-catalog \
  --bin same-mint-monitor-e2e \
  --bin same-mint-reserve-swap \
  --bin same-mint-yield-monitor \
  --bin signer-balance-monitor \
  --bin yield-migrations

artifact_root="$repo_root/build-artifacts"
mkdir -p "$artifact_root"
staging_dir=$(mktemp -d "$artifact_root/.rust.XXXXXX")
trap 'rm -rf "$staging_dir"' EXIT

stage_binary() {
  local binary=$1
  install -m 0755 "$repo_root/target/release/$binary" "$staging_dir/$binary"
}

stage_binary balance-sweep-ata-monitor
stage_binary balance-sweep-ata-projector
stage_binary balance-sweep-autodeposit-trigger
stage_binary fleet-health-projector
stage_binary fleet-opportunity-planner
stage_binary fleet-orchestration-production-evidence
stage_binary fleet-orchestration-verifier
stage_binary fleet-route-confirmer
stage_binary kamino-reserve-monitor
stage_binary loyal-timescale-migrations
stage_binary loyal-squads-policy-monitor
stage_binary loyal-yield-realtime
stage_binary route-lookup-table-alert-monitor
stage_binary route-lookup-table-cleanup
stage_binary route-lookup-table-legacy-import
stage_binary route-lookup-table-provisioner
stage_binary route-lookup-table-shared-catalog
stage_binary same-mint-monitor-e2e
stage_binary same-mint-reserve-swap
stage_binary same-mint-yield-monitor
stage_binary signer-balance-monitor
stage_binary yield-migrations

rm -rf "$artifact_root/rust"
mv "$staging_dir" "$artifact_root/rust"
trap - EXIT
