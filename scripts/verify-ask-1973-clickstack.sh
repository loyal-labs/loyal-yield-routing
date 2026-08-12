#!/usr/bin/env bash
set -euo pipefail

# Read-only ASK-1973 post-cutover ClickStack gate.
#
# Loyal's OTLP log bridge intentionally exports operational errors only. This
# gate therefore proves the absence of exported fleet operational failures; it
# does not treat an empty result as successful work. The separate production
# evidence verifier must prove successful Neon transitions and finalized-chain
# effects, while its Render check proves that every role has the exact
# observability environment boundary declared by the clean Blueprint.

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Usage:
  verify-ask-1973-clickstack.sh \
    --cutover-at RFC3339 --expected-head COMMIT --output FILE

HYPERDX_ACCESS_KEY is required in the environment. The key is passed to curl
through stdin configuration and is never supplied in command arguments or
written to the evidence artifact. The output path must not already exist.
USAGE
}

cutover_at=""
expected_head=""
output=""

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --cutover-at)
      [[ "$#" -ge 2 ]] || fail "--cutover-at requires an RFC3339 value"
      cutover_at="$2"
      shift 2
      ;;
    --expected-head)
      [[ "$#" -ge 2 ]] || fail "--expected-head requires a commit"
      expected_head="$2"
      shift 2
      ;;
    --output)
      [[ "$#" -ge 2 ]] || fail "--output requires a path"
      output="$2"
      shift 2
      ;;
    --help | -h)
      usage
      exit 0
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

for command_name in curl date git jq mktemp realpath; do
  command -v "$command_name" >/dev/null || fail "$command_name is required"
done

[[ "$cutover_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?Z$ ]] ||
  fail "--cutover-at must be an RFC3339 UTC timestamp ending in Z"
[[ "$expected_head" =~ ^[0-9a-f]{40}$ ]] ||
  fail "--expected-head must be a lowercase 40-character commit SHA"
[[ -n "$output" ]] || fail "--output is required"
[[ ! -e "$output" ]] || fail "output already exists: $output"
[[ -n "${HYPERDX_ACCESS_KEY:-}" ]] || fail "HYPERDX_ACCESS_KEY is required"
[[ "$HYPERDX_ACCESS_KEY" != *$'\n'* && "$HYPERDX_ACCESS_KEY" != *$'\r'* && "$HYPERDX_ACCESS_KEY" != *'"'* ]] ||
  fail "HYPERDX_ACCESS_KEY contains an unsupported header character"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
[[ "$(git -C "$repo_root" rev-parse HEAD)" == "$expected_head" ]] ||
  fail "--expected-head must equal the inspected checkout HEAD"
[[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=normal)" ]] ||
  fail "ClickStack evidence requires a clean checkout"
output_parent="$(realpath "$(dirname "$output")")"
output="$output_parent/$(basename "$output")"
case "$output" in
  "$repo_root" | "$repo_root/"*) fail "output must be outside the repository" ;;
esac

clickstack_origin="https://loyal-clickstack.onrender.com"
clickstack_api="$clickstack_origin/api/api/v2"
scratch_dir="$(mktemp -d /tmp/ask1973-clickstack.XXXXXX)"
cleanup() {
  if [[ "$scratch_dir" == /tmp/ask1973-clickstack.* ]]; then
    rm -rf "$scratch_dir"
  fi
}
trap cleanup EXIT

# curl reads the bearer header from stdin. Keeping the value out of argv avoids
# exposing it through process inspection and command logs.
authenticated_curl() {
  {
    printf 'header = "Authorization: Bearer '
    printf '%s' "$HYPERDX_ACCESS_KEY"
    printf '"\n'
  } | curl --config - "$@"
}

health_file="$scratch_dir/health.json"
sources_file="$scratch_dir/sources.json"
request_file="$scratch_dir/request.json"
response_file="$scratch_dir/response.json"

curl -fsS "$clickstack_origin/api/health" -o "$health_file" ||
  fail "ClickStack health request failed"
jq -e '.version | type == "string" and length > 0' "$health_file" >/dev/null ||
  fail "ClickStack health response has no version"

authenticated_curl -fsS "$clickstack_api/sources" -o "$sources_file" ||
  fail "ClickStack source listing failed"
log_source_id="$(
  jq -er '[.data[] | select(.kind == "log") | (.id // ._id)] | if length == 1 then .[0] else error("expected exactly one log source") end' \
    "$sources_file"
)" || fail "ClickStack must expose exactly one log source"

captured_at="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
roles_json='[]'
failed=0
service_names=(
  loyal-fleet-opportunity-planner
  loyal-fleet-route-revalidator
  loyal-fleet-route-executor
  loyal-fleet-route-confirmer
  loyal-fleet-route-reconciler
  loyal-route-lookup-table-provisioner
)

for service_name in "${service_names[@]}"; do
  where_sql="ServiceName = '$service_name' AND (lower(SeverityText) IN ('error', 'fatal') OR multiSearchAnyCaseInsensitive(Body, ['fatal', 'panicked', 'transition_failed', 'join_failed', 'recovery_required']) > 0)"
  jq -n \
    --arg sourceId "$log_source_id" \
    --arg startTime "$cutover_at" \
    --arg endTime "$captured_at" \
    --arg where "$where_sql" \
    '{
      sourceId: $sourceId,
      startTime: $startTime,
      endTime: $endTime,
      where: $where,
      whereLanguage: "sql",
      select: "Timestamp,SeverityText,ServiceName,ResourceAttributes",
      orderBy: "Timestamp DESC",
      maxResults: 1
    }' >"$request_file"
  authenticated_curl \
    -fsS \
    -H 'Content-Type: application/json' \
    --data-binary "@$request_file" \
    "$clickstack_api/search" \
    -o "$response_file" || fail "ClickStack search failed for $service_name"

  jq -e '(.rows | type == "number") and .rows >= 0 and (.data | type == "array")' \
    "$response_file" >/dev/null || fail "ClickStack returned an invalid search response"
  forbidden_count="$(jq -er '.rows' "$response_file")"
  role_status="PASS"
  if [[ "$forbidden_count" -ne 0 ]]; then
    role_status="FAIL"
    failed=1
  fi
  sample="$(
    jq -c '
      if .rows == 0 then null else {
        timestamp: .data[0].Timestamp,
        severity: .data[0].SeverityText,
        serviceName: .data[0].ServiceName,
        serviceVersion: .data[0].ResourceAttributes["service.version"]
      } end
    ' "$response_file"
  )"
  roles_json="$(
    jq -c \
      --arg serviceName "$service_name" \
      --arg status "$role_status" \
      --argjson forbiddenEventCount "$forbidden_count" \
      --argjson firstForbiddenEvent "$sample" \
      '. + [{
        serviceName: $serviceName,
        status: $status,
        forbiddenEventCount: $forbiddenEventCount,
        firstForbiddenEvent: $firstForbiddenEvent
      }]' <<<"$roles_json"
  )"
done

overall_status="PASS"
if [[ "$failed" -ne 0 ]]; then
  overall_status="FAIL"
fi

jq -n \
  --arg status "$overall_status" \
  --arg event "ask_1973_clickstack_verification" \
  --arg headCommit "$expected_head" \
  --arg cutoverAt "$cutover_at" \
  --arg capturedAt "$captured_at" \
  --arg endpoint "$clickstack_api" \
  --arg clickstackVersion "$(jq -er '.version' "$health_file")" \
  --arg logSourceId "$log_source_id" \
  --argjson roles "$roles_json" \
  '{
    schemaVersion: 1,
    event: $event,
    status: $status,
    productionMutation: false,
    headCommit: $headCommit,
    cutoverAt: $cutoverAt,
    capturedAt: $capturedAt,
    endpoint: $endpoint,
    clickstackVersion: $clickstackVersion,
    logSourceId: $logSourceId,
    coverage: {
      channel: "otlp_operational_errors_only",
      successfulWorkProvenByThisArtifact: false,
      successfulWorkRequiredFromProductionEndState: true,
      forbiddenSignals: [
        "error_or_fatal_severity",
        "fatal",
        "panicked",
        "transition_failed",
        "join_failed",
        "recovery_required"
      ]
    },
    roles: $roles
  }' >"$output"

if [[ "$overall_status" != "PASS" ]]; then
  fail "ClickStack contains a post-cutover fleet operational failure; evidence: $output"
fi

echo "PASS: no post-cutover fleet operational failures found in ClickStack"
echo "evidence: $output"
