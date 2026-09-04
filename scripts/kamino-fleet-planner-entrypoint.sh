#!/bin/sh
set -eu
expected=$(cat /usr/local/share/loyal-klend-proxy.sha256)
actual=$(sha256sum /usr/local/bin/loyal-klend-proxy | cut -d' ' -f1)
if [ "$actual" != "$expected" ]; then
  echo "packaged loyal-klend-proxy digest mismatch" >&2
  exit 70
fi
if [ -n "${KAMINO_KLEND_PROXY_SHA256:-}" ] && [ "$KAMINO_KLEND_PROXY_SHA256" != "$expected" ]; then
  echo "configured loyal-klend-proxy digest differs from packaged digest" >&2
  exit 70
fi
export KAMINO_KLEND_PROXY_SHA256="$expected"
exec "$@"
