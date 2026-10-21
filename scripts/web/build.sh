#!/bin/sh
# Build loyal-web: the Vite app, embedded into the Go binary.
#
#   scripts/web/build.sh [out]   (default: bin/loyal-web; GOOS/GOARCH pass through)
#
# NEXT_PUBLIC_* in the environment are inlined into the app, as Next does.
set -eu

root=$(git rev-parse --show-toplevel)
out=${1:-$root/bin/loyal-web}

(cd "$root/web/apps/loyal" && bun install --frozen-lockfile >/dev/null && bun run build)
(cd "$root/web/apps/loyal" && bun "$root/scripts/web/static-files.ts" "$root/web/apps/loyal/dist")
rsync -a --delete --exclude .keep "$root/web/apps/loyal/dist/" "$root/go/workers/internal/webserve/dist/"
(cd "$root/go/workers" && GOTOOLCHAIN=local go build -o "$out" ./cmd/loyal-web)
echo "built $out"
