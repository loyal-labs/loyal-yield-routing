#!/usr/bin/env bash
# Stages /opt/loyal/assets/web on loyal-main for the release in /opt/loyal/releases/web: the retained assets of older
# builds are copied from loyal-workers (mtimes kept, they drive the 30-day retention), then the release's own assets are
# folded in with the repo's retain-web-assets.sh, exactly as deploy/hetzner/activate.sh does at activation.
# Reads loyal-workers, writes only /opt/loyal/assets/web on loyal-main. Rerun copies nothing new and ends
# "loyal-main: 0 file(s) added". Env: OLD (loyal-workers), NEW (loyal-main), SHA (the release in loyal-main's
# /opt/loyal/releases/web; default: the only one the unit's release pin names is not needed, pass it).
set -euo pipefail

OLD=${OLD:-loyal-workers} NEW=${NEW:-loyal-main} SHA=${SHA:?release sha, for example 249031cd970f23791e354daf294c66a640871f4f}
DIR=/opt/loyal/assets/web
rel=/opt/loyal/releases/web/$SHA

count() { ssh -o BatchMode=yes "$NEW" "sudo -n find $DIR -type f | wc -l" | tr -d ' '; }

ssh -o BatchMode=yes "$NEW" "sudo -n test -d $rel/assets && sudo -n test -f $rel/retain-web-assets.sh" || { echo "$rel lacks assets or retain-web-assets.sh; run loyal-main-config.sh stage" >&2; exit 1; }
before=$(count)
ssh -o BatchMode=yes "$OLD" "sudo -n tar -C $DIR -cf - --numeric-owner ." |
  ssh -o BatchMode=yes "$NEW" "sudo -n tar -C $DIR -xpf - --skip-old-files --same-owner --numeric-owner"
ssh -o BatchMode=yes "$NEW" "sudo -n /bin/sh $rel/retain-web-assets.sh $rel/assets $DIR"
after=$(count)
want=$(ssh -o BatchMode=yes "$OLD" "sudo -n find $DIR -type f | wc -l" | tr -d ' ')
missing=$(ssh -o BatchMode=yes "$NEW" "cd $rel/assets && sudo -n find . -type f | while read -r f; do [ -f $DIR/\$f ] || echo \$f; done | wc -l" | tr -d ' ')
echo "$DIR: $before file(s) before, $after after; loyal-workers holds $want; release assets missing from the archive: $missing"
echo "$NEW: $((after - before)) file(s) added"
