#!/bin/sh
set -eu

source_assets=$1
archive=$2
[ -d "$source_assets" ] || { echo "missing web release assets: $source_assets" >&2; exit 1; }
install -d -m 755 "$archive"
# Hashed files are immutable; never overwrite files an older tab may be reading.
cp -Rn "$source_assets/." "$archive/"
find "$archive" -type d -exec chmod 755 {} +
find "$archive" -type f -exec chmod 644 {} +
# Retain shared chunks while any newly deployed build still references them.
(cd "$source_assets" && find . -type f -exec touch "$archive/{}" \;)
find "$archive" -type f -mtime +30 -delete
