#!/usr/bin/env bash
set -euo pipefail

QEDGEN="${QEDGEN:-$HOME/.agents/skills/qedgen/tools/qedgen}"
PROGRAM="crates/loyal-hub-swap-program"

"$QEDGEN" probe --program "$PROGRAM" --runtime pinocchio
