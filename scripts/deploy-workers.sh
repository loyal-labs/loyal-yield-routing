#!/usr/bin/env bash
# bun run deploy backyard|retail|observer|web [sha] [--migrations-applied] [--stopped]
#
# Builds the engine from a clean snapshot of an origin/main commit, uploads
# it to the workers host as /opt/loyal/releases/<family>/<sha>/ and switches
# loyal-<family> to it with deploy/hetzner/activate.sh, which holds the
# host's deploy lock, waits for no in-flight Backyard operation and rolls
# back if the new release does not come up. Then
# deploy/hetzner/install-rules.sh installs the commit's Prometheus rules,
# unless the host's come from a commit that is not its ancestor. A sha that
# already runs only gets the rules step. --stopped deploys to a unit the
# operator stopped on purpose (to apply migrations) and starts it on the new
# release. Production: owner's go-ahead.
#
# web deploys loyal-web the same way, minus the migrations check and the
# rules step: it builds the Vite app and the Go server from a throwaway git
# worktree (the Vite build needs the whole checkout, not a two-dir archive)
# with the NEXT_PUBLIC_* set Vercel production inlines, from the environment
# (run it under `op run --env-file=.env.1password --`; values are never
# echoed). NEXT_PUBLIC_APP_ENVIRONMENT is unset on Vercel production, which
# apps/web reads as "prod". LOYAL_DEPLOY_DRY_RUN=1 builds the linux binary and stops
# before any ssh.
#
# The host is the ssh alias in LOYAL_WORKERS_SSH (default loyal-workers); it
# must log in as root.
# shellcheck disable=SC2029 # the remote commands are built here on purpose
set -euo pipefail

usage="usage: bun run deploy backyard|retail|observer|web [sha] [--migrations-applied] [--stopped]"
family=${1:-}
[[ $# -gt 0 ]] && shift
case $family in
  backyard | retail) binary_name=loyal-engine ;;
  observer) binary_name=loyal-observer ;;
  web) binary_name=loyal-web ;;
  *) echo "$usage" >&2; exit 2 ;;
esac
target=origin/main
migrations_applied=no
mode=running
for arg in "$@"; do
  case $arg in
    --migrations-applied) migrations_applied=yes ;;
    --stopped) mode=stopped ;;
    -*) echo "$usage" >&2; exit 2 ;;
    *) target=$arg ;;
  esac
done
if [[ $family == web && ( $migrations_applied == yes || $mode == stopped ) ]]; then
  echo "web deploys run no migrations check and no rules step" >&2
  echo "$usage" >&2
  exit 2
fi

# build_web_release <goarch> — build the web release for linux from the
# worktree of $sha at $build/src into $build/loyal-web. Sets binary. The
# bundle inlines NEXT_PUBLIC_* from this environment, as Next does; the
# required ones were checked before the build, and their values are never
# echoed. The linker stamp is what puts the sha in /readyz's release header.
build_web_release() {
  GOOS=linux GOARCH=$1 CGO_ENABLED=0 GOFLAGS=-ldflags=-X=main.stamp=$sha \
    sh "$build/src/scripts/web/build.sh" "$build/loyal-web" >/dev/null
  binary=$build/loyal-web
}

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
if [[ $family == web && ${LOYAL_DEPLOY_DRY_RUN:-} == 1 ]]; then
  : # dry run: the checked-out sha need not be merged yet
elif ! git merge-base --is-ancestor "$sha" origin/main; then
  echo "$sha is not on origin/main; deploy only merged commits" >&2
  exit 1
fi

if [[ $family == web ]]; then
  # The bundle inlines NEXT_PUBLIC_* from the environment, as Next does: the
  # operator runs the deploy under `op run --env-file=.env.1password --`.
  # Without the Privy app id nothing signs in; fail before building, naming
  # where it lives; never echo values.
  if [[ -z ${NEXT_PUBLIC_PRIVY_APP_ID:-} ]]; then
    echo "the web bundle inlines NEXT_PUBLIC_* from this environment; missing: NEXT_PUBLIC_PRIVY_APP_ID" >&2
    echo "it lives in the 1Password Environment loyal-noncritical-env (mounted at .env.1password);" >&2
    echo "run the deploy under: op run --env-file=.env.1password -- bun run deploy web [sha]" >&2
    exit 1
  fi
  export NEXT_PUBLIC_GIT_COMMIT_HASH=$sha NEXT_PUBLIC_GIT_BRANCH=main
fi

if [[ $family == web && ${LOYAL_DEPLOY_DRY_RUN:-} == 1 ]]; then
  # Dry run (web only): everything up to and including the linux build, then
  # stop before any ssh. The host arch needs ssh, so this builds amd64.
  mkdir -p "$HOME/.loyal"
  build=$(mktemp -d "$HOME/.loyal/deploy-build.XXXXXX")
  trap 'git worktree remove --force "$build/src" >/dev/null 2>&1 || true; rm -rf "$build"' EXIT
  git worktree add --detach --quiet "$build/src" "$sha"
  (cd "$build/src/web" && bun install --frozen-lockfile >/dev/null)
  build_web_release amd64
  sum=$(shasum -a 256 "$binary" | cut -d' ' -f1)
  echo "built $binary_name sha256 $sum"
  found=$(LC_ALL=C strings "$binary" 2>/dev/null | grep -a -m1 -o ".\{0,40\}$sha.\{0,40\}" || true)
  if [[ -n $found ]]; then
    echo "release string found in the binary: $found"
  else
    echo "no release string in the binary: the -X main.stamp linker stamp (server lane) must land first; the real deploy refuses a binary without $sha" >&2
  fi
  echo "dry run ok: stopping before any ssh to $host"
  exit 0
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
running=$(sed -n "s|^ExecStart=$releases/\([0-9a-f]\{40\}\)/$binary_name\$|\1|p" <<<"$facts")
if [[ ${#running} -ne 40 ]]; then
  echo "cannot read the running release from $host:$dropin" >&2
  exit 1
fi
echo "host $host runs $running; deploying $sha"

# Rules never move backwards: install only over rules from an ancestor of
# sha (or the same commit), or when no record exists.
rules_sha=$(sed -n 's/^rules \([0-9a-f]\{40\}\)$/\1/p' <<<"$facts")
install_rules=yes
if [[ $family == web ]]; then
  install_rules=no # web deploys install no Prometheus rules
elif [[ -n $rules_sha ]] && ! git merge-base --is-ancestor "$rules_sha" "$sha" 2>/dev/null; then
  install_rules=no
  echo "host rules come from $rules_sha, which is not an ancestor of $sha; leaving them"
fi

if [[ $running == "$sha" && $mode == running ]]; then
  state=$(ssh "$host" systemctl is-active "$unit" || true)
  if [[ $family == web ]]; then
    echo "$unit already runs $sha ($state); the web app is unchanged"
  else
    echo "$unit already runs $sha ($state); the engine is unchanged"
  fi
  if [[ $state != active ]]; then
    exit 1
  fi
  if [[ $install_rules != yes ]]; then
    exit 0
  fi
elif [[ $family != web ]]; then
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
if [[ $family == web ]]; then
  # web builds from a throwaway worktree of the commit alone: the Vite build
  # needs the whole checkout (and a fresh bun install), not a two-dir
  # archive. The worktree leaves the git metadata before the dir goes.
  trap 'git worktree remove --force "$build/src" >/dev/null 2>&1 || true; rm -rf "$build"' EXIT
  git worktree add --detach --quiet "$build/src" "$sha"
  (cd "$build/src/web" && bun install --frozen-lockfile >/dev/null)
  tree=$build/src
else
  trap 'rm -rf "$build"' EXIT
  git archive "$sha" go/workers deploy/hetzner | tar -x -C "$build"
  tree=$build
fi
if [[ ! -f $tree/deploy/hetzner/activate.sh || ! -f $tree/deploy/hetzner/install-rules.sh ]]; then
  echo "$sha predates deploy/hetzner/activate.sh and install-rules.sh; it cannot be deployed with this command" >&2
  exit 1
fi

if [[ $running != "$sha" || $mode == stopped ]]; then
  if [[ $family == web ]]; then
    build_web_release "$goarch"
    if ! LC_ALL=C grep -qa "$sha" "$binary"; then
      echo "built binary does not carry $sha" >&2
      exit 1
    fi
    sum=$(shasum -a 256 "$binary" | cut -d' ' -f1)
    echo "built $binary_name $sha sha256 $sum"
  else
    GOFLAGS=-buildvcs=false make -C "$build/go/workers" release RELEASE="sha-$sha" GOARCH="$goarch"
    binary=$build/go/workers/bin/$binary_name
    if ! LC_ALL=C grep -qa "sha-$sha" "$binary"; then
      echo "built binary does not carry sha-$sha" >&2
      exit 1
    fi
    sum=$(shasum -a 256 "$binary" | cut -d' ' -f1)
    echo "built $binary_name sha-$sha sha256 $sum"
  fi

  dir=$releases/$sha
  ssh "$host" "mkdir -p $dir"
  scp -q "$binary" "$host:$dir/$binary_name.tmp"
  scp -q "$tree/deploy/hetzner/activate.sh" "$host:$dir/activate.sh.tmp"
  ssh "$host" "chmod 555 $dir/$binary_name.tmp $dir/activate.sh.tmp &&
    mv -f $dir/$binary_name.tmp $dir/$binary_name &&
    mv -f $dir/activate.sh.tmp $dir/activate.sh"

  # A transient unit, so the swap finishes even if this ssh session drops; its
  # log is also at /var/log/loyal-deploy/<family>-<sha>.log on the host.
  set +e
  ssh "$host" "systemd-run --unit=loyal-deploy-$family-${sha:0:12} --quiet --wait --pipe --collect \
    /bin/sh $dir/activate.sh $family $sha $sum $mode"
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
scp -q -r "$tree/deploy/hetzner/monitoring" "$tree/deploy/hetzner/install-rules.sh" "$host:$upload/"
set +e
ssh "$host" "systemd-run --unit=loyal-deploy-rules-${sha:0:12} --quiet --wait --pipe --collect \
  /bin/sh $upload/install-rules.sh $sha ${rules_sha:-none} $upload"
status=$?
set -e
if [[ $status -ne 0 ]]; then
  echo "install-rules exited $status; see $host:/var/log/loyal-deploy/rules-$sha.log" >&2
fi
exit "$status"
