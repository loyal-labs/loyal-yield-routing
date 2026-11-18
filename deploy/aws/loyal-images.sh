#!/usr/bin/env bash
# Builds, proves and wires the two docker images Loyal runs on loyal-main (arm64), natively on the box.
#   loyal-images.sh build    ensures docker-buildx and the loyal-observability network, builds both images from the
#                            committed web/observability tree (the tag is that tree's hash, so a rerun finds it and
#                            builds nothing), then points the two unit files at the new image ids
#   loyal-images.sh prove    starts each image in a throwaway container with no network and no published port,
#                            waits for its health check, removes the container
# Run from the repo root on the Mac. It records the ids in /opt/loyal/ops/arm64-images, which loyal-main-config.sh
# reads to rewrite the unit pins when it stages (so a rerun of that script keeps them). Starts docker if it was stopped and stops it again when done. Starts no Loyal unit.
# Env, all optional: BOX (loyal-main).
set -euo pipefail

BOX=${BOX:-loyal-main}
mode=${1:-}
case $mode in build | prove) ;; *) echo "usage: $0 build|prove" >&2; exit 2 ;; esac

git diff --quiet HEAD -- web/observability || { echo "web/observability has uncommitted changes; the tag names a committed tree" >&2; exit 1; }
tree=$(git rev-parse --short=12 HEAD:web/observability)
relay_tree=$(git rev-parse --short=12 HEAD:web/observability/telegram-relay)
CS=loyal-clickstack:arm64-$tree
RELAY=loyal-clickstack-relay:arm64-$relay_tree

remote() { ssh -o BatchMode=yes "$BOX" "sudo -n bash -s -- $*"; }

docker_up=$(ssh "$BOX" 'systemctl is-active docker || true')
if [[ $docker_up != active ]]; then
  ssh "$BOX" 'sudo -n systemctl start docker'
  trap 'ssh "$BOX" "sudo -n systemctl stop docker.service docker.socket"' EXIT
fi

build_one() {
  local tag=$1 treeish=$2
  if ssh "$BOX" "sudo -n docker image inspect $tag >/dev/null 2>&1"; then
    echo "$tag: present"
    return
  fi
  git archive --format=tar "$treeish" |
    ssh "$BOX" "d=\$(mktemp -d) && tar -C \$d -xf - && cd \$d && nice -n 19 sudo -n docker buildx build --platform linux/arm64 --load --pull=false --provenance=false --sbom=false -t $tag . ; s=\$?; cd /; rm -r \$d; exit \$s"
  echo "$tag: built"
}

if [[ $mode == build ]]; then
  ssh "$BOX" 'dpkg -s docker-buildx >/dev/null 2>&1 || sudo -n apt-get install -y -q docker-buildx >/dev/null'
  ssh "$BOX" 'sudo -n docker network inspect loyal-observability >/dev/null 2>&1 || sudo -n docker network create loyal-observability >/dev/null'
  build_one "$CS" HEAD:web/observability
  build_one "$RELAY" HEAD:web/observability/telegram-relay
  cs_id=$(ssh "$BOX" "sudo -n docker image inspect -f '{{.Id}}' $CS")
  relay_id=$(ssh "$BOX" "sudo -n docker image inspect -f '{{.Id}}' $RELAY")
  ssh "$BOX" "sudo -n bash -s" <<EOF
set -euo pipefail
changed=0
pin() {
  local unit=/etc/systemd/system/\$1 id=\$2
  grep -Eq ' sha256:[0-9a-f]{64} --service ' "\$unit" || { echo "\$1: no image id before --service" >&2; exit 1; }
  if grep -q " \$id --service " "\$unit"; then echo "\$1: already \$id"; return; fi
  sed -i -E "s| sha256:[0-9a-f]{64} --service | \$id --service |" "\$unit"
  echo "\$1: now \$id"
  changed=1
}
ids=/opt/loyal/ops/arm64-images
want=\$(printf 'clickstack=%s\\nrelay=%s\\n' $cs_id $relay_id)
[ "\$(cat \$ids 2>/dev/null)" = "\$want" ] || { install -d -m 0755 /opt/loyal/ops; printf '%s\\n' "\$want" >\$ids; chmod 0644 \$ids; echo "\$ids: written"; }
pin loyal-clickstack.service $cs_id
pin loyal-clickstack-relay.service $relay_id
[ \$changed = 0 ] || systemctl daemon-reload
EOF
  echo "build: done"
  exit 0
fi

cs_id=$(ssh "$BOX" "sudo -n docker image inspect -f '{{.Id}}' $CS")
relay_id=$(ssh "$BOX" "sudo -n docker image inspect -f '{{.Id}}' $RELAY")
ssh "$BOX" 'sudo -n bash -s' <<EOF
set -uo pipefail
name_cs=loyal-clickstack-smoke name_relay=loyal-clickstack-relay-smoke
docker rm -fv \$name_cs \$name_relay >/dev/null 2>&1
fail=0

echo "clickstack $cs_id arch \$(docker image inspect -f '{{.Architecture}}' $cs_id)"
docker run -d --name \$name_cs --network none --cpus 2 --memory 4g --pids-limit 2048 \
  -e EXPRESS_SESSION_SECRET=smoke-\$RANDOM\$RANDOM\$RANDOM -e INGESTION_API_KEY=smoke-\$RANDOM\$RANDOM\$RANDOM \
  -e FRONTEND_URL=http://localhost:8080 $cs_id >/dev/null
end=\$((SECONDS+300))
until [ "\$(docker inspect -f '{{.State.Health.Status}}' \$name_cs)" = healthy ] || [ \$SECONDS -ge \$end ]; do sleep 5; done
status=\$(docker inspect -f '{{.State.Health.Status}}' \$name_cs)
echo "clickstack health after \$((SECONDS))s of shell time: \$status"
[ "\$status" = healthy ] || { fail=1; docker inspect -f '{{range .State.Health.Log}}{{.ExitCode}} {{.Output}}{{end}}' \$name_cs | tail -c 600; }
docker rm -fv \$name_cs >/dev/null

echo "relay $relay_id arch \$(docker image inspect -f '{{.Architecture}}' $relay_id)"
docker run -d --name \$name_relay --network none --memory 512m --pids-limit 512 \
  -e HOST=0.0.0.0 -e PORT=10000 -e CLICKSTACK_WEBHOOK_SECRET=smoke -e TELEGRAM_BOT_TOKEN=smoke -e TELEGRAM_CHAT_ID=1 \
  -e SLACK_WEBHOOK_URL=https://smoke.invalid/x -e DAILY_RECAP_ENABLED=false $relay_id >/dev/null
sleep 3
if docker exec \$name_relay bun -e 'const r = await fetch("http://127.0.0.1:10000/healthz"); console.log(r.status, await r.text()); process.exit(r.ok ? 0 : 1)'; then
  echo "relay /healthz ok"
else
  fail=1; docker logs --tail 5 \$name_relay 2>&1 | cut -c1-200
fi
docker rm -fv \$name_relay >/dev/null

echo "leftover smoke containers: \$(docker ps -aq --filter name=-smoke | wc -l)"
exit \$fail
EOF
