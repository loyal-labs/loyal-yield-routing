#!/usr/bin/env bash
# bun run deploy backyard|retail [sha] [--migrations-applied]
#
# Builds the engine from a clean snapshot of an origin/main commit, uploads
# it to the workers host as /opt/loyal/releases/<family>/<sha>/ and switches
# loyal-<family> to it with deploy/hetzner/activate.sh, which holds the
# host's deploy lock, waits for no in-flight Backyard operation and rolls
# back if the new release does not come up. Then
# deploy/hetzner/install-rules.sh installs the commit's Prometheus rules,
# unless the host's come from a commit that is not its ancestor. A sha that
# already runs only gets the rules step. Production: owner's go-ahead.
#
# The host is the ssh alias in LOYAL_WORKERS_SSH (default loyal-workers); it
# must log in as root.
# shellcheck disable=SC2029 # the remote commands are built here on purpose
set -euo pipefail

usage="usage: bun run deploy backyard|retail [sha] [--migrations-applied]"
family=${1:-}
[[ $# -gt 0 ]] && shift
if [[ $family != backyard && $family != retail ]]; then
  echo "$usage" >&2
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
unit=loyal-$family
dropin=/etc/systemd/system/$unit.service.d/40-verified-release.conf
releases=/opt/loyal/releases/$family
rules_record=/etc/prometheus/loyal.rules.yml.sha

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

# One ssh round trip for the host facts: root, arch, the commit the
# installed rules came from, running release.
facts=$(ssh "$host" "id -u; uname -m; echo rules \$(cat $rules_record 2>/dev/null); cat $dropin")
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

# Rules never move backwards: install only over rules from an ancestor of
# sha (or the same commit), or when no record exists.
rules_sha=$(sed -n 's/^rules \([0-9a-f]\{40\}\)$/\1/p' <<<"$facts")
install_rules=yes
if [[ -n $rules_sha ]] && ! git merge-base --is-ancestor "$rules_sha" "$sha" 2>/dev/null; then
  install_rules=no
  echo "host rules come from $rules_sha, which is not an ancestor of $sha; leaving them"
fi

if [[ $running == "$sha" ]]; then
  state=$(ssh "$host" systemctl is-active "$unit" || true)
  echo "$unit already runs $sha ($state); the engine is unchanged"
  if [[ $state != active ]]; then
    exit 1
  fi
  if [[ $install_rules != yes ]]; then
    exit 0
  fi
else
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
fi

# A fresh build dir per run, filled from the commit alone: no uncommitted
# file reaches a release, and two deploys on one machine never share one.
mkdir -p "$HOME/.loyal"
build=$(mktemp -d "$HOME/.loyal/deploy-build.XXXXXX")
trap 'rm -rf "$build"' EXIT
git archive "$sha" go/workers deploy/hetzner | tar -x -C "$build"
if [[ ! -f $build/deploy/hetzner/activate.sh || ! -f $build/deploy/hetzner/install-rules.sh ]]; then
  echo "$sha predates deploy/hetzner/activate.sh and install-rules.sh; it cannot be deployed with this command" >&2
  exit 1
fi

if [[ $running != "$sha" ]]; then
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
  scp -q "$build/deploy/hetzner/activate.sh" "$host:$dir/activate.sh.tmp"
  ssh "$host" "chmod 555 $dir/loyal-engine.tmp $dir/activate.sh.tmp &&
    mv -f $dir/loyal-engine.tmp $dir/loyal-engine &&
    mv -f $dir/activate.sh.tmp $dir/activate.sh"

  # A transient unit, so the swap finishes even if this ssh session drops; its
  # log is also at /var/log/loyal-deploy/<family>-<sha>.log on the host.
  set +e
  ssh "$host" "systemd-run --unit=loyal-deploy-$family-${sha:0:12} --quiet --wait --pipe --collect \
    /bin/sh $dir/activate.sh $family $sha $sum"
  status=$?
  set -e
  if [[ $status -ne 0 ]]; then
    echo "activate exited $status; see $host:/var/log/loyal-deploy/$family-$sha.log" >&2
    exit "$status"
  fi
fi

if [[ $install_rules != yes ]]; then
  exit 0
fi
# The commit's monitoring files and install-rules.sh go to a fresh host temp
# dir, which install-rules.sh checks, installs from and removes.
upload=$(ssh "$host" "mktemp -d /tmp/loyal-monitoring.XXXXXX")
scp -q -r "$build/deploy/hetzner/monitoring" "$build/deploy/hetzner/install-rules.sh" "$host:$upload/"
set +e
ssh "$host" "systemd-run --unit=loyal-deploy-rules-${sha:0:12} --quiet --wait --pipe --collect \
  /bin/sh $upload/install-rules.sh $sha ${rules_sha:-none} $upload"
status=$?
set -e
if [[ $status -ne 0 ]]; then
  echo "install-rules exited $status; see $host:/var/log/loyal-deploy/rules-$sha.log" >&2
fi
exit "$status"
