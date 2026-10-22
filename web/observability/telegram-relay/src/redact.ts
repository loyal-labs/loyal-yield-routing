import { escapeHtml } from "./format.ts";

const REDACTED = "<redacted>";

const BOT_TOKEN_URL_SOURCE = String.raw`bot\d+:[\w-]+`;
const SLACK_WEBHOOK_URL_SOURCE = String.raw`https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+`;
/**
 * A bot token is `<numeric id>:<35-character secret>`. Delivered alert text is
 * other services' log output, which can carry a token with no `bot` prefix in
 * front of it — a config dump, or a message quoting the raw value — so shape
 * alone has to be enough there. The length floor keeps ordinary `12:34` text
 * out of it.
 */
const BARE_BOT_TOKEN_SOURCE = String.raw`(?<![\w-])\d{5,16}:[A-Za-z0-9_-]{30,}(?![\w-])`;

const BOT_TOKEN_URL = new RegExp(BOT_TOKEN_URL_SOURCE, "g");
const SLACK_WEBHOOK_URL = new RegExp(SLACK_WEBHOOK_URL_SOURCE, "g");
const BARE_BOT_TOKEN = new RegExp(BARE_BOT_TOKEN_SOURCE, "g");

/**
 * The bot token is embedded in the request URL, so any error that quotes the
 * URL - fetch connection errors especially - would carry it into the logs.
 */
export function redactBotToken(text: string): string {
  return text
    .replace(BOT_TOKEN_URL, `bot${REDACTED}`)
    .replace(SLACK_WEBHOOK_URL, `https://hooks.slack.com/services/${REDACTED}`);
}

/**
 * For text this relay did not author: the rendered alert message, assembled
 * from whatever the alerted services logged. On top of the URL forms it strips
 * bare bot tokens by shape, and `secrets` by exact value — the shape rules are a
 * best effort against any token, the literal match is the guarantee that this
 * relay's own secrets cannot appear in its logs.
 *
 * Every occurrence is located against the original text, and overlapping ones
 * are merged into a single span before anything is rewritten. Rewriting as it
 * scanned was not enough on two counts. Replacing in sequence let each pass read
 * the markers earlier passes had written, so a secret as short as `red` turned a
 * redacted bot token into `bot<<redacted>acted>`. Replacing in one left-to-right
 * pass fixed that but still consumed the start of an overlapping credential and
 * left its tail behind: two secrets sharing a boundary redacted as
 * `<redacted>-BBBB`. Merging spans first means no character of any credential
 * survives, whatever order the credentials are in and however they overlap.
 *
 * The only caller is `RelayCredentials.redactLogText`, which owns the secret
 * set. Redaction is not offered as a standalone function elsewhere: a caller
 * choosing its own secret list is how a credential gets left out.
 */
export function stripSecrets(text: string, secrets: string[]): string {
  const spans = [
    ...patternSpans(text, BOT_TOKEN_URL, `bot${REDACTED}`, 0),
    ...patternSpans(
      text,
      SLACK_WEBHOOK_URL,
      `https://hooks.slack.com/services/${REDACTED}`,
      1
    ),
    ...patternSpans(text, BARE_BOT_TOKEN, REDACTED, 2),
    ...literalSpans(text, secrets),
  ];

  return redactSpans(text, spans);
}

interface RedactionSpan {
  start: number;
  end: number;
  marker: string;
  priority: number;
}

function patternSpans(
  text: string,
  pattern: RegExp,
  marker: string,
  priority: number
): RedactionSpan[] {
  return [...text.matchAll(pattern)].map((match) => ({
    start: match.index,
    end: match.index + match[0].length,
    marker,
    priority,
  }));
}

function literalSpans(text: string, secrets: string[]): RedactionSpan[] {
  const spans: RedactionSpan[] = [];

  for (const form of new Set(secrets.flatMap(secretForms))) {
    for (let from = 0; from <= text.length - form.length; from += 1) {
      const at = text.indexOf(form, from);
      if (at === -1) {
        break;
      }
      spans.push({
        start: at,
        end: at + form.length,
        marker: REDACTED,
        priority: 3,
      });
      from = at;
    }
  }

  return spans;
}

function redactSpans(text: string, spans: RedactionSpan[]): string {
  if (spans.length === 0) {
    return text;
  }

  const ordered = [...spans].sort(
    (left, right) =>
      left.start - right.start ||
      right.end - left.end ||
      left.priority - right.priority
  );

  let result = "";
  let cursor = 0;
  let index = 0;

  while (index < ordered.length) {
    const span = ordered[index];
    let end = span.end;
    let next = index + 1;

    while (next < ordered.length && ordered[next].start <= end) {
      end = Math.max(end, ordered[next].end);
      next += 1;
    }

    result += text.slice(cursor, span.start) + span.marker;
    cursor = end;
    index = next;
  }

  return result + text.slice(cursor);
}

function secretForms(secret: string): string[] {
  const literal = secret.trim();
  if (literal.length === 0) {
    return [];
  }

  const escaped = escapeHtml(literal);
  return escaped === literal ? [literal] : [literal, escaped];
}
