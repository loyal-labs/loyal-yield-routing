#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
DEST=/opt/pgmove

conf_box() {
  [[ -f $HERE/clusters/$1.conf ]] || { echo "remote: no cluster $1" >&2; exit 2; }
  # shellcheck disable=SC2016
  bash -c 'BOX=; source "$1"; echo "$BOX"' _ "$HERE/clusters/$1.conf"
}

pgmove_on() {
  local box
  box=$(conf_box "$2")
  ssh -o BatchMode=yes "$box" "$DEST/pgmove.sh $(printf '%q ' "$@")"
}

install_on() {
  local box
  box=$(conf_box "$1")
  COPYFILE_DISABLE=1 tar -C "$HERE" -cf - pgmove.sh pg-forward@.service clusters |
    ssh -o BatchMode=yes "$box" "install -d -m 0755 $DEST && tar -C $DEST --no-same-owner -xf - && chown -R root:root $DEST"
  echo "remote: $DEST on $box holds pgmove.sh at $(git -C "$HERE" rev-parse --short HEAD 2>/dev/null || echo '?')"
}

[[ ${BASH_SOURCE[0]} == "$0" ]] || return 0

case ${1:-} in
  install)
    [[ -n ${2:-} ]] || { echo "usage: $0 install <cluster>" >&2; exit 2; }
    install_on "$2"
    ;;
  run)
    [[ -n ${2:-} && -n ${3:-} ]] || { echo "usage: $0 run <pgmove subcommand> <cluster> [flags]" >&2; exit 2; }
    shift
    pgmove_on "$@"
    ;;
  *)
    echo "usage: $0 install <cluster> | run <pgmove subcommand> <cluster> [flags]  (on the cluster's BOX)" >&2
    exit 2
    ;;
esac
