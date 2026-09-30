#!/bin/sh
# Execute only after parent allocates loyal-lane-b-handoff namespace on rehearsal.
set -eu
image=loyal-lane-b-handoff:fixture
container=loyal-lane-b-handoff-fixture
# Run in the extracted allowlisted archive root. No global prune/stop/restart.
DOCKER_BUILDKIT=0 timeout --signal=TERM --kill-after=15s 600s docker build --memory 2g --cpu-period 100000 --cpu-quota 200000 \
  -f scripts/hetzner-backyard-handoff-fixture/Dockerfile -t "$image" .
docker image inspect "$image" > lane-b-image-inspect.json
# Detached so timeout cleanup targets this exact container, not unrelated work.
id=$(docker run -d --name "$container" --network none --read-only \
  --cap-drop ALL --security-opt no-new-privileges \
  --memory 2g --cpus 2 --pids-limit 256 \
  --tmpfs /tmp:rw,nosuid,nodev,size=1g,mode=1777 "$image")
cleanup() { docker rm -f "$id" >/dev/null 2>&1 || true; }
trap cleanup EXIT HUP INT TERM
if ! timeout --signal=TERM --kill-after=5s 300s docker wait "$id" > lane-b-exit.txt; then
  docker logs "$id" > lane-b-result.json 2> lane-b-stderr.log
  echo 'BLOCKED: exact fixture container exceeded 300s' >&2
  exit 2
fi
docker logs "$id" > lane-b-result.json 2> lane-b-stderr.log
cat lane-b-result.json
exit "$(cat lane-b-exit.txt)"
