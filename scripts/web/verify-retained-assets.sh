#!/bin/sh
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/old" "$scratch/new" "$scratch/archive"
printf 'old code' > "$scratch/old/old.js"
printf 'shared code' > "$scratch/old/shared.js"
/bin/sh "$root/deploy/hetzner/retain-web-assets.sh" "$scratch/old" "$scratch/archive"
cp "$scratch/old/shared.js" "$scratch/new/shared.js"
printf 'new code' > "$scratch/new/new.js"
printf 'expired code' > "$scratch/archive/expired.js"
touch -t 202001010000 "$scratch/archive/expired.js" "$scratch/archive/shared.js"
/bin/sh "$root/deploy/hetzner/retain-web-assets.sh" "$scratch/new" "$scratch/archive"
cmp "$scratch/old/old.js" "$scratch/archive/old.js"
cmp "$scratch/new/new.js" "$scratch/archive/new.js"
cmp "$scratch/new/shared.js" "$scratch/archive/shared.js"
[ ! -e "$scratch/archive/expired.js" ]
if /bin/sh "$root/deploy/hetzner/retain-web-assets.sh" "$scratch/missing" "$scratch/archive" 2>/dev/null; then
  echo 'missing release assets must fail before activation' >&2
  exit 1
fi
printf 'must not overwrite an immutable asset' > "$scratch/new/shared.js"
/bin/sh "$root/deploy/hetzner/retain-web-assets.sh" "$scratch/new" "$scratch/archive"
cmp "$scratch/old/shared.js" "$scratch/archive/shared.js"
echo 'asset rollover, retention, expiry, and missing-upload checks passed'
