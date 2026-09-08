#!/usr/bin/env bash
# Canonical build for the Loyal Voltr RWA NAV adaptor.
#
# The program only fits the deployed ProgramData capacity when it is built with
# link-time optimization, so this is the only supported build path. It prints
# the artifact's size and sha256 and refuses to pass when either the size
# exceeds the pinned ProgramData capacity or the sha256 does not match the
# deployer's ADAPTOR_SPEC_V3 pin, so a build and the deployer can never drift
# apart silently.
#
# Usage: bun run build:adaptor [--expect-sha <sha256>] [--self-test]
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
crate="$repo_root/crates/loyal-voltr-rwa-nav-adaptor"
deployer_source="$repo_root/crates/loyal-voltr-rwa-nav-adaptor-deployer/src/main.rs"
artifact="$repo_root/target/deploy/loyal_voltr_rwa_nav_adaptor.so"

expect_sha=""
self_test=false
while [ $# -gt 0 ]; do
  case "$1" in
    --expect-sha)
      [ $# -ge 2 ] || { echo "--expect-sha requires a sha256" >&2; exit 2; }
      expect_sha="$2"
      shift 2
      ;;
    --self-test)
      self_test=true
      shift
      ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

extract_unique_numeric_field() {
  local field="$1"
  local block="$2"
  local matches match_count value

  # The field name must begin the line after whitespace. This intentionally
  # ignores comments such as `// previous max_data_len: 100_000,`.
  matches="$(printf '%s\n' "$block" | sed -nE "s/^[[:space:]]*${field}:[[:space:]]*([0-9_]+)[[:space:]]*,[[:space:]]*$/\1/p")"
  match_count="$(printf '%s\n' "$matches" | awk 'NF { count++ } END { print count + 0 }')"
  if ! [[ "$match_count" =~ ^[0-9]+$ ]] || (( 10#$match_count != 1 )); then
    echo "expected exactly one ${field} in ADAPTOR_SPEC_V3, found ${match_count}" >&2
    return 2
  fi

  value="${matches//_/}"
  if ! [[ "$value" =~ ^[0-9]+$ ]]; then
    echo "${field} in ADAPTOR_SPEC_V3 is not a decimal integer" >&2
    return 2
  fi
  printf '%s\n' "$value"
}

extract_unique_sha256_field() {
  local block="$1"
  local matches match_count

  matches="$(printf '%s\n' "$block" | sed -nE 's/^[[:space:]]*elf_sha256:[[:space:]]*"([0-9a-f]{64})"[[:space:]]*,[[:space:]]*$/\1/p')"
  match_count="$(printf '%s\n' "$matches" | awk 'NF { count++ } END { print count + 0 }')"
  if ! [[ "$match_count" =~ ^[0-9]+$ ]] || (( 10#$match_count != 1 )); then
    echo "expected exactly one elf_sha256 in ADAPTOR_SPEC_V3, found ${match_count}" >&2
    return 2
  fi
  printf '%s\n' "$matches"
}

run_self_test() {
  local synthetic_block='const ADAPTOR_SPEC_V3: ProgramSpec = ProgramSpec {
    max_data_len: 115_384,
    // previous max_data_len: 100_000,
    max_data_len: 115_384,
    elf_sha256: "836ded9ff4e79cda9fafbafcffcf9f2e9f395c762c69ba5af4630e82a9d8a4d0",
    elf_len: 107_832,
};'
  local status

  if extract_unique_numeric_field max_data_len "$synthetic_block" >/dev/null 2>&1; then
    echo "self-test failed: duplicate max_data_len was accepted" >&2
    return 1
  else
    status=$?
  fi
  if [ "$status" -ne 2 ]; then
    echo "self-test failed: duplicate max_data_len returned ${status}, expected 2" >&2
    return 1
  fi
  echo "self-test: PASS (duplicate max_data_len rejected with exit 2)"
}

if [ "$self_test" = true ]; then
  run_self_test
  exit 0
fi

# Single source of truth: read the v3 pin's capacity and hash straight out of
# the deployer spec.
pin_block="$(awk '/^const ADAPTOR_SPEC_V3: ProgramSpec = ProgramSpec \{/,/^};/' "$deployer_source")"
max_data_len="$(extract_unique_numeric_field max_data_len "$pin_block")"
elf_len="$(extract_unique_numeric_field elf_len "$pin_block")"
pinned_sha="$(extract_unique_sha256_field "$pin_block")"
if [ -z "$expect_sha" ]; then
  expect_sha="$pinned_sha"
fi
if ! [[ "$expect_sha" =~ ^[0-9a-f]{64}$ ]]; then
  echo "--expect-sha must be a lowercase 64-character sha256" >&2
  exit 2
fi

cd "$crate"
CARGO_PROFILE_RELEASE_LTO=fat \
  CARGO_PROFILE_RELEASE_CODEGEN_UNITS=1 \
  cargo-build-sbf --sbf-out-dir "$repo_root/target/deploy"

[ -f "$artifact" ] || { echo "artifact missing: $artifact" >&2; exit 1; }
size="$(stat -f%z "$artifact" 2>/dev/null || stat -c%s "$artifact")"
sha="$(shasum -a 256 "$artifact" | awk '{print $1}')"

if ! [[ "$size" =~ ^[0-9]+$ && "$elf_len" =~ ^[0-9]+$ && "$max_data_len" =~ ^[0-9]+$ ]]; then
  echo "FAIL: artifact size and pinned lengths must be decimal integers" >&2
  exit 1
fi
if ! [[ "$sha" =~ ^[0-9a-f]{64}$ ]]; then
  echo "FAIL: artifact sha256 is not a 64-character lowercase hex digest" >&2
  exit 1
fi
if ! size_int=$((10#$size)); then
  echo "FAIL: could not compare artifact size: $size" >&2
  exit 1
fi
if ! elf_len_int=$((10#$elf_len)); then
  echo "FAIL: could not compare pinned elf_len: $elf_len" >&2
  exit 1
fi
if ! max_data_len_int=$((10#$max_data_len)); then
  echo "FAIL: could not compare pinned max_data_len: $max_data_len" >&2
  exit 1
fi

echo "adaptor artifact: $artifact"
echo "  size:   $size bytes (ProgramData capacity $max_data_len)"
echo "  sha256: $sha"
echo "  pin:    $expect_sha"

status=0
if (( size_int > max_data_len_int )); then
  echo "FAIL: artifact exceeds ProgramData capacity ($size > $max_data_len)" >&2
  status=1
fi
if (( size_int != elf_len_int )); then
  echo "FAIL: artifact size does not match the deployer elf_len pin ($size != $elf_len)" >&2
  status=1
fi
if [ "$sha" != "$expect_sha" ]; then
  echo "FAIL: artifact sha256 does not match the deployer ADAPTOR_SPEC_V3 pin; re-pin only after a reviewed change" >&2
  status=1
fi
exit "$status"
