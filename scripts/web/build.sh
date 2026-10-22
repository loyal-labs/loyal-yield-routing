#!/bin/sh
# Build one web app: its Vite build, embedded into its Go binary.
#
#   scripts/web/build.sh [loyal|admin|stats] [out]   (default: loyal,
#                                               bin/loyal-<app> with loyal-web
#                                               for loyal; GOOS/GOARCH pass through)
#
# NEXT_PUBLIC_* in the environment are inlined into the app, as Next does.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
app=loyal
case ${1:-} in loyal | admin | stats) app=$1 && shift ;; esac
case $app in
  loyal) binary=loyal-web dir=loyal ;;
  admin) binary=loyal-admin dir=admin ;;
  stats) binary=loyal-stats dir=dashboard ;;
esac
out=${1:-$root/bin/$binary}
webserve=github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webserve
ldflags="-X $webserve.stamp=${NEXT_PUBLIC_GIT_COMMIT_HASH:-}"

if [ "$app" = loyal ]; then
  (cd "$root/web/apps/loyal" && bun install --frozen-lockfile >/dev/null && bun run build)
  (cd "$root/web/apps/loyal" && bun "$root/scripts/web/static-files.ts" "$root/web/apps/loyal/dist")
  app_environment=$(cd "$root" && bun -e 'import { resolveAppEnvironment } from "./web/apps/web/src/lib/core/config/shared.ts"; console.log(resolveAppEnvironment(process.env.NEXT_PUBLIC_APP_ENVIRONMENT))')
  ldflags="$ldflags -X main.appEnvironment=$app_environment"
elif [ "$app" = admin ]; then
  (cd "$root/web/apps/loyal" && bun install --frozen-lockfile >/dev/null)
  (cd "$root/web/apps/admin" && bun run build)
else
  (cd "$root/web/apps/loyal" && bun install --frozen-lockfile >/dev/null)
  (cd "$root/web/apps/dashboard" && bun run build:vite)
fi
rsync -a --delete --exclude .keep "$root/web/apps/$dir/dist/" "$root/go/workers/cmd/$binary/dist/"
(cd "$root/go/workers" && GOTOOLCHAIN=local go build -ldflags "$ldflags" -o "$out" "./cmd/$binary")
echo "built $out"
