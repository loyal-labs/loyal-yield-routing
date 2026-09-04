#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
go_bin="${GO_BIN:-$(command -v go || true)}"
if [[ -z "$go_bin" || ! -x "$go_bin" ]]; then
  echo "Go 1.27.0 is required; set GO_BIN" >&2
  exit 1
fi
if [[ "$($go_bin env GOVERSION)" != "go1.27.0" ]]; then
  echo "expected Go go1.27.0" >&2
  exit 1
fi

export SQLX_OFFLINE=true
cd "$repo_root"
cargo build --release --locked -p balance-sweep-ata-monitor --bin earn-domain-bridge

artifact_dir="$repo_root/build-artifacts/go-laserstream-worker"
mkdir -p "$repo_root/build-artifacts"
staging_dir="$(mktemp -d "$repo_root/build-artifacts/.go-laserstream-worker.XXXXXX")"
cleanup() { rm -rf "$staging_dir"; }
trap cleanup EXIT

cd "$repo_root/go/laserstream-worker"
CGO_ENABLED=0 "$go_bin" build -trimpath -ldflags='-s -w' \
  -o "$staging_dir/loyal-laserstream-worker" ./cmd/loyal-laserstream-worker
install -m 0755 "$repo_root/target/release/earn-domain-bridge" "$staging_dir/earn-domain-bridge"
rm -rf "$artifact_dir"
mv "$staging_dir" "$artifact_dir"
trap - EXIT
