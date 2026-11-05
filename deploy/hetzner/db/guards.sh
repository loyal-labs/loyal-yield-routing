#!/usr/bin/env bash
set -euo pipefail

DEFAULTS=/etc/default/prometheus-node-exporter
TEXTFILES=/var/lib/prometheus/node-exporter
want='ARGS="--web.listen-address=10.92.1.1:9100 --collector.disable-defaults --collector.filesystem --collector.textfile --collector.textfile.directory=/var/lib/prometheus/node-exporter"'

if [[ "$(cat "$DEFAULTS" 2>/dev/null)" != "$want" ]]; then
  printf '%s\n' "$want" >"$DEFAULTS"
fi
if ! dpkg -s prometheus-node-exporter >/dev/null 2>&1; then
  apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confold prometheus-node-exporter
fi
install -d -m 0755 "$TEXTFILES"
systemctl daemon-reload
systemctl enable --now prometheus-node-exporter loyal-db-replica-metrics.timer loyal-db-retention.timer
systemctl restart prometheus-node-exporter
systemctl start loyal-db-replica-metrics.service
echo "guards installed"
