#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

# SQLx queries used by these binaries are checked against the committed query
# metadata. The image build must not depend on a live database.
export SQLX_OFFLINE=true

family=all
list_binaries=false
while (($# > 0)); do
  case "$1" in
    --family)
      family=$2
      shift 2
      ;;
    --list-binaries)
      list_binaries=true
      shift
      ;;
    *)
      printf 'Usage: %s [--family all|laserstream-workers|light-workers|operator-tools] [--list-binaries]\n' "$0" >&2
      exit 2
      ;;
  esac
done

# Each family is one Cargo invocation. The default still builds the complete
# inventory for local verification; CI can run the three disjoint image paths
# in parallel instead of making one four-core runner link all 22 executables.
case "$family" in
  all)
    packages=(
      balance-sweep-ata-monitor
      balance-sweep-ata-projector
      balance-sweep-autodeposit-trigger
      kamino-reserve-monitor
      loyal-fleet-worker
      loyal-squads-policy-monitor
      loyal-timescale-migrations
      loyal-yield-orchestrator
      loyal-yield-realtime
    )
    binaries=(
      balance-sweep-ata-monitor
      balance-sweep-ata-projector
      balance-sweep-autodeposit-trigger
      fleet-health-projector
      fleet-opportunity-planner
      fleet-orchestration-production-evidence
      fleet-orchestration-verifier
      fleet-route-confirmer
      kamino-reserve-monitor
      loyal-timescale-migrations
      loyal-squads-policy-monitor
      loyal-yield-realtime
      route-lookup-table-alert-monitor
      route-lookup-table-cleanup
      route-lookup-table-legacy-import
      route-lookup-table-provisioner
      route-lookup-table-shared-catalog
      same-mint-monitor-e2e
      same-mint-reserve-swap
      same-mint-yield-monitor
      signer-balance-monitor
      yield-migrations
    )
    ;;
  laserstream-workers)
    packages=(balance-sweep-ata-monitor kamino-reserve-monitor loyal-timescale-migrations loyal-yield-orchestrator)
    binaries=(balance-sweep-ata-monitor kamino-reserve-monitor loyal-timescale-migrations yield-migrations)
    ;;
  light-workers)
    packages=(
      balance-sweep-ata-projector
      balance-sweep-autodeposit-trigger
      loyal-fleet-worker
      loyal-squads-policy-monitor
      loyal-yield-orchestrator
      loyal-yield-realtime
    )
    binaries=(
      balance-sweep-ata-projector
      balance-sweep-autodeposit-trigger
      fleet-health-projector
      fleet-opportunity-planner
      fleet-route-confirmer
      loyal-squads-policy-monitor
      loyal-yield-realtime
      route-lookup-table-provisioner
      same-mint-reserve-swap
      same-mint-yield-monitor
      yield-migrations
    )
    ;;
  operator-tools)
    packages=(loyal-timescale-migrations loyal-yield-orchestrator)
    binaries=(
      fleet-orchestration-production-evidence
      fleet-orchestration-verifier
      loyal-timescale-migrations
      route-lookup-table-alert-monitor
      route-lookup-table-cleanup
      route-lookup-table-legacy-import
      route-lookup-table-shared-catalog
      same-mint-monitor-e2e
      signer-balance-monitor
    )
    ;;
  *)
    printf 'Unknown image family: %s\n' "$family" >&2
    exit 2
    ;;
esac

if $list_binaries; then
  printf '%s\n' "${binaries[@]}"
  exit 0
fi

package_args=()
for package in "${packages[@]}"; do
  package_args+=(-p "$package")
done
binary_args=()
for binary in "${binaries[@]}"; do
  binary_args+=(--bin "$binary")
done
cargo build --release --locked "${package_args[@]}" "${binary_args[@]}"

artifact_root="$repo_root/build-artifacts"
mkdir -p "$artifact_root"
staging_dir=$(mktemp -d "$artifact_root/.rust.XXXXXX")
trap 'rm -rf "$staging_dir"' EXIT

stage_binary() {
  local binary=$1
  install -m 0755 "$repo_root/target/release/$binary" "$staging_dir/$binary"
}

for binary in "${binaries[@]}"; do
  stage_binary "$binary"
done

rm -rf "$artifact_root/rust"
mv "$staging_dir" "$artifact_root/rust"
trap - EXIT
