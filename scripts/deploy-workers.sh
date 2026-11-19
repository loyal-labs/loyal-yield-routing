#!/usr/bin/env bash
set -euo pipefail

usage="usage: bun run deploy backyard|earn-max|retail|observer|telegram|web|admin|stats [sha] [--migrations-applied] [--stopped] [--check-ci]"
family=${1:-}
[[ $# -gt 0 ]] && shift
case $family in
  backyard | earn-max | retail) binary_name=loyal-engine ;;
  observer) binary_name=loyal-observer ;;
  telegram) binary_name=loyal-telegram ;;
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
# The release architecture is the target's: LOYAL_DEPLOY_GOARCH, else what the
# host says its machine is.
goarch_of() {
  case $1 in
    x86_64 | amd64) goarch=amd64 ;;
    aarch64 | arm64) goarch=arm64 ;;
    *) echo "$host reports machine '$1'; set LOYAL_DEPLOY_GOARCH=amd64 or arm64" >&2; exit 1 ;;
  esac
}
unit=loyal-$family
dropin=/etc/systemd/system/$unit.service.d/40-verified-release.conf
releases=/opt/loyal/releases/$family
rules_file=/etc/prometheus/loyal.rules.yml
rules_path=deploy/hetzner/monitoring/loyal.rules.yml

# The cutover tool disables cloudflared on the retired box and enables it on the live one.
live=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" systemctl is-enabled cloudflared.service 2>&1) || true
if [[ $live != enabled ]]; then
  echo "$host is not the live Loyal box (cloudflared.service read: '${live//$'\n'/ }'); set LOYAL_WORKERS_SSH to the live box (loyal-main after the move)" >&2
  exit 1
fi
remote_uid=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" id -u) || { echo "cannot read the login uid on $host" >&2; exit 1; }
rsudo=()
[[ $remote_uid == 0 ]] || rsudo=(sudo -n)

cd "$(dirname "$0")/.."
git fetch -q origin main
sha=$(git rev-parse --verify --quiet "${target}^{commit}") || {
  echo "unknown commit '$target'" >&2
  exit 1
}
if [[ ${LOYAL_DEPLOY_REQUIRE_HEAD:-} == 1 && $sha != "$(git rev-parse origin/main)" ]]; then
  echo "refusing superseded production release $sha" >&2
  exit 1
fi
if [[ $check_ci == yes || ( $web_family == yes && ${LOYAL_DEPLOY_DRY_RUN:-} == 1 ) ]]; then
  :
elif ! git merge-base --is-ancestor "$sha" origin/main; then
  echo "$sha is not on origin/main; deploy only merged commits" >&2
  exit 1
fi

# A scoped release receipt proves this service's selected checks passed on main.
scoped=no
if git cat-file -e "$sha:scripts/production-scope.py" 2>/dev/null; then scoped=yes; fi
if [[ $web_family == no || ( $scoped == yes && ( ${LOYAL_DEPLOY_DRY_RUN:-} != 1 || $check_ci == yes ) ) ]]; then
  verification_workflow=workers-v2-verify.yml
  artifact_filter=''
  if git cat-file -e "$sha:.github/workflows/production.yml" 2>/dev/null; then
    verification_workflow=production.yml
    # Main's marker includes race tests. A later rollout failure does not revoke it.
    artifact_filter=' | select(.workflow_run.head_branch == "main")'
  fi
  if [[ $scoped == yes ]]; then
    artifact="production-verified-$family-$sha"
    artifact_filter=" | select(.workflow_run.head_branch == \"main\" and .workflow_run.head_sha == \"$sha\")"
    if [[ -n ${GITHUB_RUN_ID:-} ]]; then
      [[ $GITHUB_RUN_ID =~ ^[0-9]+$ ]] || exit 1
      artifact_filter+=" | select(.workflow_run.id == $GITHUB_RUN_ID)"
    fi
  else
    artifact=$(scripts/ci-tree-key.sh "$sha")
  fi
  green=$(gh api -X GET "repos/{owner}/{repo}/actions/artifacts" -f name="$artifact" \
    --jq "[.artifacts[] | select(.expired == false and .workflow_run.head_repository_id == .workflow_run.repository_id)$artifact_filter] | length")
  if [[ ! $green =~ ^[0-9]+$ ]]; then
    echo "cannot read Workers v2 verification artifacts from GitHub: $green" >&2
    exit 1
  fi
  if [[ $green == 0 ]]; then
    echo "no green Workers v2 verification for tree key $artifact; wait for CI or push" >&2
    exit 1
  fi
  echo "$sha has a green Workers v2 verification: $artifact"
  if [[ $verification_workflow == workers-v2-verify.yml ]]; then
    failed=$(gh api -X GET "repos/{owner}/{repo}/actions/workflows/$verification_workflow/runs" -f head_sha="$sha" -f event=push \
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
fi
if [[ $check_ci == yes ]]; then
  exit 0
fi

if [[ $family == web && -z ${LOYAL_DEPLOY_ARTIFACT_DIR:-} ]]; then
  if [[ -z ${NEXT_PUBLIC_PRIVY_APP_ID:-} ]]; then
    echo "the web bundle inlines the names in web/apps/loyal/vite/public-env.ts from this environment; missing: NEXT_PUBLIC_PRIVY_APP_ID" >&2
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
  if [[ -n ${LOYAL_DEPLOY_GOARCH:-} ]]; then
    goarch_of "$LOYAL_DEPLOY_GOARCH"
  else
    machine=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$host" uname -m) ||
      { echo "Cannot ask $host for its architecture; set LOYAL_DEPLOY_GOARCH=amd64 or arm64" >&2; exit 1; }
    goarch_of "$machine"
  fi
  build_web_release "$goarch"
  sum=$(shasum -a 256 "$binary" | cut -d' ' -f1)
  echo "built $binary_name sha256 $sum"
  found=$(LC_ALL=C strings "$binary" 2>/dev/null | grep -a -o ".\{0,40\}$sha.\{0,40\}" | sed -n 1p || true)
  if [[ -n $found ]]; then
    echo "release string found in the binary: $found"
  else
    echo "no release string in the binary: the -X main.stamp linker stamp (server lane) must land first; the real deploy refuses a binary without $sha" >&2
  fi
  echo "dry run ok: stopping before any change on $host"
  exit 0
fi

facts=$(ssh "$host" ${rsudo[@]+"${rsudo[@]}"} sh -s -- "$rules_file" "$dropin" <<'REMOTE'
blob() {
  [ -f "$1" ] || return 0
  { printf 'blob %d\0' "$(wc -c <"$1")"; cat "$1"; } | sha1sum | cut -d' ' -f1
}
id -u; uname -m; echo rules $(blob "$1"); cat "$2"
REMOTE
)
{
  read -r _
  read -r arch
} <<<"$facts"
goarch_of "$arch"
if [[ -n ${LOYAL_DEPLOY_GOARCH:-} ]]; then
  want=$goarch
  goarch_of "$LOYAL_DEPLOY_GOARCH"
  if [[ $goarch != "$want" ]]; then
    echo "LOYAL_DEPLOY_GOARCH=$LOYAL_DEPLOY_GOARCH but $host is $arch" >&2
    exit 1
  fi
fi
running=$(sed -n "s|^ExecStart=$releases/\([0-9a-f]\{40\}\)/$binary_name\$|\1|p" <<<"$facts")
if [[ ${#running} -ne 40 ]]; then
  echo "cannot read the running release from $host:$dropin" >&2
  exit 1
fi
echo "host $host runs $running; deploying $sha"
if [[ -n ${LOYAL_DEPLOY_EXPECTED_RELEASE:-} && $running != "$LOYAL_DEPLOY_EXPECTED_RELEASE" ]]; then
  echo "release changed after preflight; refusing $family" >&2
  exit 1
fi

install_rules=yes
rules_exit=0
if [[ $web_family == yes ]]; then
  install_rules=no
else
  host_blob=$(sed -n 's/^rules \([0-9a-f]\{40\}\)$/\1/p' <<<"$facts")
  target_blob=$(git rev-parse --verify --quiet "$sha:$rules_path") || {
    echo "$sha has no $rules_path" >&2
    exit 1
  }
  if [[ $host_blob == "$target_blob" ]]; then
    install_rules=no
    echo "monitoring rules unchanged"
  elif [[ -n $host_blob ]]; then
    # Newest, so a revert never places the host's rules before a later version.
    from=$(git log --format=%H --raw --no-abbrev origin/main -- "$rules_path" |
      awk -v blob="$host_blob" 'NF == 1 { c = $1 } $4 == blob && !found { print c; found = 1 }')
    if [[ -z $from ]]; then
      install_rules=no
      rules_exit=1
      echo "host rules (blob $host_blob) match no version on origin/main: installed from a branch or edited by hand" >&2
      echo "merge that branch, or install main's rules by hand with deploy/hetzner/install-rules.sh" >&2
    elif ! git merge-base --is-ancestor "$from" "$sha"; then
      install_rules=no
      echo "host rules come from $from, newer than $sha; leaving them"
    fi
  fi
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
    exit "$rules_exit"
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
if [[ $web_family == yes && -z ${LOYAL_DEPLOY_ARTIFACT_DIR:-} ]]; then
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
  if [[ -n ${LOYAL_DEPLOY_ARTIFACT_DIR:-} ]]; then
    artifacts=$LOYAL_DEPLOY_ARTIFACT_DIR
    [[ $goarch == amd64 ]] || artifacts=$artifacts/$goarch
    binary=$artifacts/$binary_name
    [[ -f $binary ]] || { echo "missing verified $goarch artifact $binary_name" >&2; exit 1; }
    LC_ALL=C grep -qa "$sha" "$binary" || { echo "artifact does not carry $sha" >&2; exit 1; }
    sum=$(shasum -a 256 "$binary" | cut -d' ' -f1)
    echo "using verified $binary_name $sha sha256 $sum"
  elif [[ $web_family == yes ]]; then
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
  ssh "$host" ${rsudo[@]+"${rsudo[@]}"} mkdir -p "$dir"
  put=$dir
  if [[ ${#rsudo[@]} -gt 0 ]]; then
    put=$(ssh "$host" "mktemp -d /tmp/loyal-stage.XXXXXX")
  fi
  scp -q "$binary" "$host:$put/$binary_name.tmp"
  scp -q "$tree/deploy/hetzner/activate.sh" "$host:$put/activate.sh.tmp"
  if [[ $family == web && -f $tree/deploy/hetzner/retain-web-assets.sh ]]; then
    assets=$tree/go/workers/cmd/loyal-web/dist/assets
    if [[ -n ${LOYAL_DEPLOY_ARTIFACT_DIR:-} ]]; then
      assets=$build/assets
      cp -a "$artifacts/web-assets" "$assets"
    fi
    scp -q -r "$assets" "$host:$put/"
    scp -q "$tree/deploy/hetzner/retain-web-assets.sh" "$host:$put/"
  fi
  if [[ $put != "$dir" ]]; then
    ssh "$host" sudo -n sh -s -- "$put" "$dir" <<'REMOTE'
cp -R "$1"/. "$2"/ && rm -r "$1"
REMOTE
  fi
  ssh "$host" ${rsudo[@]+"${rsudo[@]}"} sh -s -- "$dir" "$binary_name" <<'REMOTE'
chmod 555 "$1/$2.tmp" "$1/activate.sh.tmp" &&
  mv -f "$1/$2.tmp" "$1/$2" &&
  mv -f "$1/activate.sh.tmp" "$1/activate.sh"
REMOTE

  set +e
  ssh "$host" ${rsudo[@]+"${rsudo[@]}"} systemd-run --unit="loyal-deploy-$family-${sha:0:12}" --quiet --wait --pipe --collect \
    --setenv="LOYAL_DEPLOY_EXPECTED_RELEASE=${LOYAL_DEPLOY_EXPECTED_RELEASE:-}" \
    /bin/sh "$dir/activate.sh" "$family" "$sha" "$sum" "$mode"
  status=$?
  set -e
  if [[ $status -ne 0 ]]; then
    echo "activate exited $status; see $host:/var/log/loyal-deploy/$family-$sha.log" >&2
    exit "$status"
  fi
fi

if [[ $install_rules != yes ]]; then
  exit "$rules_exit"
fi
upload=$(ssh "$host" "mktemp -d /tmp/loyal-monitoring.XXXXXX")
scp -q -r "$tree/deploy/hetzner/monitoring" "$tree/deploy/hetzner/install-rules.sh" "$host:$upload/"
set +e
ssh "$host" ${rsudo[@]+"${rsudo[@]}"} systemd-run --unit="loyal-deploy-rules-${sha:0:12}" --quiet --wait --pipe --collect \
  /bin/sh "$upload/install-rules.sh" "$sha" "${host_blob:-none}" "$upload"
status=$?
set -e
if [[ $status -ne 0 ]]; then
  echo "install-rules exited $status; see $host:/var/log/loyal-deploy/rules-$sha.log" >&2
fi
exit "$status"
