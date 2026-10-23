telegram_topic_name() {
  local branch
  branch="$(git branch --show-current 2>/dev/null)" || true
  printf '%s' "${SUPERSET_WORKSPACE_NAME:-${branch:-$(basename "$PWD")}}"
}
