#!/usr/bin/env bash
# bun run deploy backyard [sha] [--migrations-applied]
#
# Builds the Backyard engine from a clean snapshot of an origin/main commit,
# uploads it to the workers host as /opt/loyal/releases/backyard/<sha>/ and
# switches loyal-backyard to it with deploy/hetzner/activate-backyard.sh,
# which holds the host's deploy lock, waits for no in-flight operation and
# rolls back if the new release does not come up. Production: owner's go-ahead.
#
# The host is the ssh alias in LOYAL_WORKERS_SSH (default loyal-workers); it
# must log in as root.
# shellcheck disable=SC2029 # the remote commands are built here on purpose
set -euo pipefail

usage="usage: bun run deploy backyard [sha] [--migrations-applied]"
family=${1:-}
[[ $# -gt 0 ]] && shift
if [[ $family != backyard ]]; then
  echo "$usage" >&2
  echo "only the backyard family deploys with this command (got '${family}')" >&2
  exit 2
fi
target=origin/main
migrations_applied=no
for arg in "$@"; do
  case $arg in
    --migrations-applied) migrations_applied=yes ;;
    -*) echo "$usage" >&2; exit 2 ;;
    *) target=$arg ;;
  esac
done

host=${LOYAL_WORKERS_SSH:-loyal-workers}
dropin=/etc/systemd/system/loyal-backyard.service.d/40-verified-release.conf
releases=/opt/loyal/releases/backyard
build=$HOME/.loyal/deploy-build

cd "$(dirname "$0")/.."
git fetch -q origin main
sha=$(git rev-parse --verify --quiet "${target}^{commit}") || {
  echo "unknown commit '$target'" >&2
  exit 1
}
if ! git merge-base --is-ancestor "$sha" origin/main; then
  echo "$sha is not on origin/main; deploy only merged commits" >&2
  exit 1
fi

# One ssh round trip for the host facts: root, arch, running release.
facts=$(ssh "$host" "id -u; uname -m; cat $dropin")
{
  read -r uid
  read -r arch
} <<<"$facts"
if [[ $uid != 0 ]]; then
  echo "$host logs in as uid $uid; the deploy needs root" >&2
  exit 1
fi
case $arch in
  x86_64) goarch=amd64 ;;
  aarch64) goarch=arm64 ;;
  *) echo "unsupported host architecture $arch" >&2; exit 1 ;;
esac
running=$(sed -n "s|^ExecStart=$releases/\([0-9a-f]\{40\}\)/loyal-engine\$|\1|p" <<<"$facts")
if [[ ${#running} -ne 40 ]]; then
  echo "cannot read the running release from $host:$dropin" >&2
  exit 1
fi
echo "host $host runs $running; deploying $sha"
if [[ $running == "$sha" ]]; then
  state=$(ssh "$host" systemctl is-active loyal-backyard || true)
  echo "loyal-backyard already runs $sha ($state); nothing to deploy"
  if [[ $state != active ]]; then
    exit 1
  fi
  exit 0
fi

if ! git cat-file -e "${running}^{commit}" 2>/dev/null; then
  echo "running release $running is not in this clone; cannot compare migrations" >&2
  exit 1
fi
changed=$(git diff --name-only "$running" "$sha" -- migrations/)
if [[ -n $changed ]]; then
  echo "migrations differ between $running and $sha:"
  echo "$changed"
  if [[ $migrations_applied != yes ]]; then
    echo "apply migrations first (bun run yield:migrate / timescale:migrate), then rerun with --migrations-applied" >&2
    exit 1
  fi
  echo "--migrations-applied: proceeding"
fi

# A fixed build dir, emptied, then filled from the commit alone: no
# uncommitted file reaches a release.
mkdir -p "$build"
find "$build" -mindepth 1 -delete
git archive "$sha" go/workers deploy/hetzner | tar -x -C "$build"
if [[ ! -f $build/deploy/hetzner/activate-backyard.sh ]]; then
  echo "$sha predates activate-backyard.sh; it cannot be deployed with this command" >&2
  exit 1
fi
GOFLAGS=-buildvcs=false make -C "$build/go/workers" release RELEASE="sha-$sha" GOARCH="$goarch"
binary=$build/go/workers/bin/loyal-engine
if ! LC_ALL=C grep -qa "sha-$sha" "$binary"; then
  echo "built binary does not carry sha-$sha" >&2
  exit 1
fi
sum=$(shasum -a 256 "$binary" | cut -d' ' -f1)
echo "built loyal-engine sha-$sha sha256 $sum"

dir=$releases/$sha
ssh "$host" "mkdir -p $dir"
scp -q "$binary" "$host:$dir/loyal-engine.tmp"
scp -q "$build/deploy/hetzner/activate-backyard.sh" "$host:$dir/activate-backyard.sh.tmp"
ssh "$host" "chmod 555 $dir/loyal-engine.tmp $dir/activate-backyard.sh.tmp &&
  mv -f $dir/loyal-engine.tmp $dir/loyal-engine &&
  mv -f $dir/activate-backyard.sh.tmp $dir/activate-backyard.sh"

# A transient unit, so the swap finishes even if this ssh session drops; its
# log is also at /var/log/loyal-deploy/backyard-<sha>.log on the host.
set +e
ssh "$host" "systemd-run --unit=loyal-deploy-backyard-${sha:0:12} --quiet --wait --pipe --collect \
  /bin/sh $dir/activate-backyard.sh $sha $sum"
status=$?
set -e
if [[ $status -ne 0 ]]; then
  echo "activate exited $status; see $host:/var/log/loyal-deploy/backyard-$sha.log" >&2
fi
exit "$status"
