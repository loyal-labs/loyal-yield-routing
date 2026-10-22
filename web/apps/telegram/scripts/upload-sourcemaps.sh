#!/bin/bash
set -e

if [ -z "$DATADOG_API_KEY" ]; then
  echo "DATADOG_API_KEY not set, skipping source map upload"
  exit 0
fi

COMMIT_HASH="$(bun -e 'process.stdout.write(require("./scripts/release-version.ts").resolveReleaseVersion())')"

echo "Uploading source maps for version $COMMIT_HASH"

npx datadog-ci sourcemaps upload .next/static \
  --service=app-frontend \
  --release-version="$COMMIT_HASH" \
  --minified-path-prefix=/_next/static/

echo "Source map upload complete"
