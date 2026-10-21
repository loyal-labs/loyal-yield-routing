#!/bin/sh
# Build loyal-web: the Vite app, embedded into the Go binary.
#
#   scripts/web/build.sh [out]   (default: bin/loyal-web; GOOS/GOARCH pass through)
#
# NEXT_PUBLIC_* in the environment are inlined into the app, as Next does.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
out=${1:-$root/bin/loyal-web}

(cd "$root/web/apps/loyal" && bun install --frozen-lockfile >/dev/null && bun run build)
(cd "$root/web/apps/loyal" && bun "$root/scripts/web/static-files.ts" "$root/web/apps/loyal/dist")
rsync -a --delete --exclude .keep "$root/web/apps/loyal/dist/" "$root/go/workers/internal/webserve/dist/"
app_environment=$(cd "$root" && bun -e 'import { resolveAppEnvironment } from "./web/apps/web/src/lib/core/config/shared.ts"; console.log(resolveAppEnvironment(process.env.NEXT_PUBLIC_APP_ENVIRONMENT))')
(cd "$root/go/workers" && GOTOOLCHAIN=local go build -ldflags "-X main.stamp=${NEXT_PUBLIC_GIT_COMMIT_HASH:-} -X github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve.appEnvironment=$app_environment" -o "$out" ./cmd/loyal-web)
echo "built $out"
