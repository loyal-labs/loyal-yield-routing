#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
preparer="$repo_root/scripts/prepare-rust-target-cache.py"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

cd "$fixture"
git init -q -b main
git config user.name 'Cargo Cache Verifier'
git config user.email 'cargo-cache-verifier@example.invalid'
printf 'target/\n' >.gitignore
printf 'unchanged\n' >unchanged.txt
printf 'before\n' >changed.txt
git add .gitignore unchanged.txt changed.txt
git commit -q -m base
base_revision=$(git rev-parse HEAD)

mkdir -p target/release
printf '%s\n' "$base_revision" >target/.ci-source-revision
printf 'after\n' >changed.txt
printf 'added\n' >added.txt
git add changed.txt added.txt
git commit -q -m change

touch -t 202001010000 .gitignore unchanged.txt changed.txt added.txt
python3 "$preparer" restore

python3 - <<'PY'
from pathlib import Path

old = 315532800
assert int(Path("unchanged.txt").stat().st_mtime) == old
assert int(Path(".gitignore").stat().st_mtime) == old
assert int(Path("changed.txt").stat().st_mtime) > old
assert int(Path("added.txt").stat().st_mtime) > old
PY

python3 "$preparer" record
test "$(cat target/.ci-source-revision)" = "$(git rev-parse HEAD)"

printf 'invalid\n' >target/.ci-source-revision
touch -t 202001010000 unchanged.txt
before_fallback=$(python3 -c 'from pathlib import Path; print(int(Path("unchanged.txt").stat().st_mtime))')
python3 "$preparer" restore
after_fallback=$(python3 -c 'from pathlib import Path; print(int(Path("unchanged.txt").stat().st_mtime))')
test "$before_fallback" = "$after_fallback"

printf 'OVERALL: PASS\n'
