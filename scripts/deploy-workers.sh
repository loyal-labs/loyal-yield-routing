#!/usr/bin/env bash
set -euo pipefail

usage="usage: bun run deploy backyard|retail|observer|web|admin|stats [sha] [--migrations-applied] [--stopped] [--check-ci]"
family=${1:-}
[[ $# -gt 0 ]] && shift
case $family in
  backyard | retail) binary_name=loyal-engine ;;
  observer) binary_name=loyal-observer ;;
  web) binary_name=loyal-web ;;
  admin) binary_name=loyal-admin ;;
  stats) binary_name=loyal-stats ;;
  *) echo "$usage" >&2; exit 2 ;;
esac
target=origin/main
migrations_applied=no
mode=running
check_ci=no
for arg in "$@"; do
  case $arg in
    --migrations-applied) migrations_applied=yes ;;
    --stopped) mode=stopped ;;
    --check-ci) check_ci=yes ;;
    -*) echo "$usage" >&2; exit 2 ;;
    *) target=$arg ;;
  esac
done
web_family=no
[[ $family == web || $family == admin || $family == stats ]] && web_family=yes
if [[ $web_family == yes && ( $migrations_applied == yes || $mode == stopped ) ]]; then
  echo "web deploys run no migrations check and no rules step" >&2
  echo "$usage" >&2
  exit 2
fi

build_web_release() {
  GOOS=linux GOARCH=$1 CGO_ENABLED=0 \
    sh "$build/src/scripts/web/build.sh" "${family/web/loyal}" "$build/$binary_name" >/dev/null
  binary=$build/$binary_name
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
if [[ $check_ci == yes || ( $web_family == yes && ${LOYAL_DEPLOY_DRY_RUN:-} == 1 ) ]]; then
  :
elif ! git merge-base --is-ancestor "$sha" origin/main; then
  echo "$sha is not on origin/main; deploy only merged commits" >&2
  exit 1
fi

# web-go.yml runs on PRs only, so web families have no verification to look up.
if [[ $web_family == no ]]; then
  artifact=$(scripts/ci-tree-key.sh "$sha")
  green=$(gh api -X GET "repos/{owner}/{repo}/actions/artifacts" -f name="$artifact" \
    --jq '[.artifacts[] | select(.expired == false and .workflow_run.head_repository_id == .workflow_run.repository_id)] | length')
  if [[ ! $green =~ ^[0-9]+$ ]]; then
    echo "cannot read Workers v2 verification artifacts from GitHub: $green" >&2
    exit 1
  fi
  if [[ $green == 0 ]]; then
    echo "no green Workers v2 verification for tree key $artifact; wait for CI or push" >&2
    exit 1
  fi
  echo "$sha has a green Workers v2 verification: $artifact"
  failed=$(gh api -X GET "repos/{owner}/{repo}/actions/workflows/workers-v2-verify.yml/runs" -f head_sha="$sha" -f event=push \
    --jq '[.workflow_runs[] | select(.status == "completed" and .conclusion == "failure")] | length')
  if [[ ! $failed =~ ^[0-9]+$ ]]; then
    echo "cannot read Workers v2 verification runs from GitHub: $failed" >&2
    exit 1
  fi
  if [[ $failed != 0 ]]; then
    echo "main's race-checked run failed for $sha; fix main before deploying" >&2
    exit 1
  fi
fi
if [[ $check_ci == yes ]]; then
  exit 0
fi

if [[ $family == web ]]; then
  if [[ -z ${NEXT_PUBLIC_PRIVY_APP_ID:-} ]]; then
    echo "the web bundle inlines NEXT_PUBLIC_* from this environment; missing: NEXT_PUBLIC_PRIVY_APP_ID" >&2
    echo "it lives in the 1Password Environment loyal-noncritical-env (mounted at .env.1password);" >&2
    echo "run the deploy under: op run --env-file=.env.1password -- bun run deploy web [sha]" >&2
    exit 1
  fi
fi
if [[ $web_family == yes ]]; then
  export NEXT_PUBLIC_GIT_COMMIT_HASH=$sha NEXT_PUBLIC_GIT_BRANCH=main
fi

if [[ $web_family == yes && ${LOYAL_DEPLOY_DRY_RUN:-} == 1 ]]; then
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

facts=$(ssh "$host" sh -s -- "$rules_record" "$dropin" <<'REMOTE'
id -u; uname -m; echo rules $(cat "$1" 2>/dev/null); cat "$2"
REMOTE
)
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

rules_sha=$(sed -n 's/^rules \([0-9a-f]\{40\}\)$/\1/p' <<<"$facts")
install_rules=yes
if [[ $web_family == yes ]]; then
  install_rules=no
elif [[ -n $rules_sha ]] && ! git merge-base --is-ancestor "$rules_sha" "$sha" 2>/dev/null; then
  install_rules=no
  echo "host rules come from $rules_sha, which is not an ancestor of $sha; leaving them"
fi

if [[ $running == "$sha" && $mode == running ]]; then
  state=$(ssh "$host" sh -s -- "$unit" <<'REMOTE' || true
systemctl is-active "$1"
REMOTE
)
  if [[ $web_family == yes ]]; then
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
elif [[ $web_family != yes ]]; then
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

mkdir -p "$HOME/.loyal"
build=$(mktemp -d "$HOME/.loyal/deploy-build.XXXXXX")
if [[ $web_family == yes ]]; then
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
  if [[ $web_family == yes ]]; then
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
  ssh "$host" mkdir -p "$dir"
  scp -q "$binary" "$host:$dir/$binary_name.tmp"
  scp -q "$tree/deploy/hetzner/activate.sh" "$host:$dir/activate.sh.tmp"
  if [[ $family == web && -f $tree/deploy/hetzner/retain-web-assets.sh ]]; then
    scp -q -r "$tree/go/workers/cmd/loyal-web/dist/assets" "$host:$dir/"
    scp -q "$tree/deploy/hetzner/retain-web-assets.sh" "$host:$dir/"
  fi
  ssh "$host" sh -s -- "$dir" "$binary_name" <<'REMOTE'
chmod 555 "$1/$2.tmp" "$1/activate.sh.tmp" &&
  mv -f "$1/$2.tmp" "$1/$2" &&
  mv -f "$1/activate.sh.tmp" "$1/activate.sh"
REMOTE

  set +e
  ssh "$host" systemd-run --unit="loyal-deploy-$family-${sha:0:12}" --quiet --wait --pipe --collect \
    /bin/sh "$dir/activate.sh" "$family" "$sha" "$sum" "$mode"
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
upload=$(ssh "$host" "mktemp -d /tmp/loyal-monitoring.XXXXXX")
scp -q -r "$tree/deploy/hetzner/monitoring" "$tree/deploy/hetzner/install-rules.sh" "$host:$upload/"
set +e
ssh "$host" systemd-run --unit="loyal-deploy-rules-${sha:0:12}" --quiet --wait --pipe --collect \
  /bin/sh "$upload/install-rules.sh" "$sha" "${rules_sha:-none}" "$upload"
status=$?
set -e
if [[ $status -ne 0 ]]; then
  echo "install-rules exited $status; see $host:/var/log/loyal-deploy/rules-$sha.log" >&2
fi
exit "$status"
