#!/usr/bin/env bash
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scripts/telegram-topic-name.sh"

TOKEN_FILE="$HOME/.claude/channels/telegram/.env"
DAEMON_TOPICS="$HOME/.claude/channels/telegram/daemon/topics"

if [ ! -f "$TOKEN_FILE" ]; then
  exit 0
fi
TOKEN=$(grep "^TELEGRAM_BOT_TOKEN=" "$TOKEN_FILE" | sed 's/^TELEGRAM_BOT_TOKEN=//')
if [ -z "$TOKEN" ]; then
  exit 0
fi

TOPIC_NAME="$(telegram_topic_name)"

THREAD_ID=""
CHAT_ID=""
if [ -d "$DAEMON_TOPICS" ]; then
  for dir in "$DAEMON_TOPICS"/*/; do
    meta="${dir}meta.json"
    if [ -f "$meta" ]; then
      read -r name thread_id chat_id < <(
        python3 - "$meta" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
print(d.get('topicName', ''), d.get('threadId', ''), d.get('chatId', ''))
PYEOF
      ) || continue
      if [ "$name" = "$TOPIC_NAME" ]; then
        THREAD_ID="$thread_id"
        CHAT_ID="$chat_id"
        break
      fi
    fi
  done
fi

if [ -z "$THREAD_ID" ] || [ -z "$CHAT_ID" ]; then
  echo "No topic found for workspace '$TOPIC_NAME', skipping."
  exit 0
fi

echo "Closing Telegram topic '$TOPIC_NAME' (thread_id: $THREAD_ID)..."
curl -s -X POST "https://api.telegram.org/bot${TOKEN}/closeForumTopic" \
  -d "chat_id=${CHAT_ID}" \
  -d "message_thread_id=${THREAD_ID}" > /dev/null 2>&1 || true

echo "Topic closed."
