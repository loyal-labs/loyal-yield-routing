#!/bin/sh
set -eu

/usr/local/bin/loyal-timescale-migrations --apply
exec /usr/local/bin/kamino-reserve-monitor --sync-supported-reserves
