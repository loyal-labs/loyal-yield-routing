#!/usr/bin/env bash
# Restage robinx-main with the release robinx-prod runs now: read the live sha, build it for linux/arm64 from a clean
# git archive of wen (never its working tree), stage with robinx-main-config.sh, then rerun it (must print 0 change(s)).
# Usage: robinx-main-restage.sh. Prints S5_BUILD=<dir> last, for switch/s5-robinx.sh preflight.
# A half-built /tmp/s5prep/build-<sha9> must be removed by hand first.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
WEN=${WEN:-$HOME/loyal/wen}
SHA=$(ssh -o BatchMode=yes root@5.161.126.23 'readlink /opt/robinx/current' | sed -n 's|^/opt/robinx/releases/robinx-\([0-9a-f]\{40\}\)$|\1|p')
[[ $SHA =~ ^[0-9a-f]{40}$ ]] || { echo "restage: no live sha" >&2; exit 1; }
B=/tmp/s5prep/build-${SHA:0:9}
git -C "$WEN" cat-file -e "$SHA^{commit}" 2>/dev/null || git -C "$WEN" fetch -q origin
if [[ ! -f $B/.built ]]; then
  [[ ! -e $B ]] || { echo "restage: $B exists without .built; remove it by hand" >&2; exit 1; }
  mkdir -p "$B"
  git -C "$WEN" archive "$SHA" | tar -x -C "$B"
  (
    cd "$B"
    bun install --frozen-lockfile
    POSTHOG_API_KEY=unused-upload-stripped POSTHOG_RELEASE=$SHA ROBINX_GOARCH=arm64 ROBINX_RELEASE=$SHA sh deploy/build-release.sh
    (cd server && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o ../bin/wallet-indexer ./cmd/wallet-indexer)
    file bin/robinx-linux bin/wallet-indexer
    touch .built
  )
fi
export BUILD=$B WEN
bash "$HERE/robinx-main-config.sh" stage
bash "$HERE/robinx-main-config.sh" stage | tail -3
echo "S5_BUILD=$B"
