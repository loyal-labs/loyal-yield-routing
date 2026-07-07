import { createHmac } from "node:crypto";
import { neon } from "@neondatabase/serverless";

const DEFAULT_REALTIME_URL = "https://loyal-yield-realtime.onrender.com";
const DEFAULT_WALLET = "render_sse_v2_smoke_wallet";

type ParsedSseEvent = {
  id?: string;
  event?: string;
  data: string;
};

function printHelp() {
  console.log(`Usage: bun run verify:realtime:sse

Required env:
  REALTIME_AUTH_SECRET
  NEON_DATABASE_URL

Optional env:
  REALTIME_URL          default ${DEFAULT_REALTIME_URL}
  REALTIME_SMOKE_WALLET default ${DEFAULT_WALLET}

Signs a short-lived mainnet-default token, opens SSE, emits a safe realtime
event through Yield Neon, verifies live delivery, and verifies Last-Event-ID
replay. Secrets and tokens are never printed.`);
}

function env(name: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} must be set`);
  }
  return value;
}

function optionalEnv(name: string, fallback: string): string {
  return process.env[name] || fallback;
}

function base64url(input: string | Buffer): string {
  return Buffer.from(input).toString("base64url");
}

function signToken(walletAddress: string): string {
  const claims = {
    exp: Math.floor(Date.now() / 1000) + 300,
    walletAddress,
    scopes: ["autodeposit"],
  };
  const payload = base64url(JSON.stringify(claims));
  const signature = createHmac("sha256", env("REALTIME_AUTH_SECRET"))
    .update(payload)
    .digest("base64url");
  return `${payload}.${signature}`;
}

async function emitSmokeEvent(walletAddress: string): Promise<number> {
  const sql = neon(env("NEON_DATABASE_URL"));
  const rows = await sql`
    SELECT loyal_yield.emit_realtime_event(
      p_event_type => 'realtime_v2_sse_smoke',
      p_scope => 'autodeposit',
      p_reason => 'v2_sse_smoke',
      p_wallet_address => ${walletAddress},
      p_payload => jsonb_build_object('smoke', true, 'verifier', 'realtime_v2')
    ) AS event_id
  `;
  const eventId = Number(rows[0]?.event_id);
  if (!Number.isSafeInteger(eventId)) {
    throw new Error("emit_realtime_event did not return a valid event_id");
  }
  return eventId;
}

function parseSseBlock(block: string): ParsedSseEvent | null {
  const event: ParsedSseEvent = { data: "" };
  for (const line of block.split(/\r?\n/)) {
    if (!line || line.startsWith(":")) {
      continue;
    }
    const index = line.indexOf(":");
    const field = index === -1 ? line : line.slice(0, index);
    const value = index === -1 ? "" : line.slice(index + 1).trimStart();
    if (field === "id") {
      event.id = value;
    } else if (field === "event") {
      event.event = value;
    } else if (field === "data") {
      event.data += event.data ? `\n${value}` : value;
    }
  }
  return event.id || event.event || event.data ? event : null;
}

async function waitForSseEvent(args: {
  token: string;
  expectedEventId?: number;
  lastEventId?: number;
  timeoutMs: number;
  afterConnected?: () => Promise<number>;
}): Promise<{ event: ParsedSseEvent; eventId: number }> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), args.timeoutMs);
  try {
    const url = new URL("/events", optionalEnv("REALTIME_URL", DEFAULT_REALTIME_URL));
    url.searchParams.set("token", args.token);
    const response = await fetch(url, {
      headers: {
        Accept: "text/event-stream",
        ...(args.lastEventId === undefined
          ? {}
          : { "Last-Event-ID": String(args.lastEventId) }),
      },
      signal: controller.signal,
    });
    if (!response.ok) {
      throw new Error(`/events returned ${response.status}`);
    }
    const contentType = response.headers.get("content-type") || "";
    if (!contentType.includes("text/event-stream")) {
      throw new Error(`/events returned unexpected content-type ${contentType}`);
    }
    if (!response.body) {
      throw new Error("/events response had no body");
    }

    const reader = response.body.getReader();
    const expectedEventId =
      args.expectedEventId ?? (await args.afterConnected?.());
    if (!Number.isSafeInteger(expectedEventId)) {
      throw new Error("expectedEventId was not available");
    }
    const decoder = new TextDecoder();
    let buffer = "";
    while (true) {
      const { value, done } = await reader.read();
      if (done) {
        throw new Error("SSE stream ended before expected event");
      }
      buffer += decoder.decode(value, { stream: true });
      const parts = buffer.split(/\r?\n\r?\n/);
      buffer = parts.pop() || "";
      for (const part of parts) {
        const event = parseSseBlock(part);
        if (!event) {
          continue;
        }
        if (event.id === String(expectedEventId)) {
          if (event.event !== "loyal_yield") {
            throw new Error(`expected loyal_yield event, got ${event.event}`);
          }
          const payload = JSON.parse(event.data);
          if (
            payload.type !== "realtime_v2_sse_smoke" ||
            payload.eventId !== expectedEventId ||
            payload.scope !== "autodeposit" ||
            payload.reason !== "v2_sse_smoke"
          ) {
            throw new Error(`unexpected SSE payload for event ${expectedEventId}`);
          }
          return { event, eventId: expectedEventId };
        }
      }
    }
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
}

async function checkHealthAndAuth() {
  const base = optionalEnv("REALTIME_URL", DEFAULT_REALTIME_URL);
  const health = await fetch(new URL("/healthz", base));
  if (!health.ok || (await health.text()) !== "ok") {
    throw new Error(`/healthz failed with ${health.status}`);
  }
  const badToken = await fetch(`${base}/events?token=bad`, {
    headers: { Accept: "text/event-stream" },
  });
  if (badToken.status !== 401) {
    throw new Error(`bad token returned ${badToken.status}, expected 401`);
  }
}

async function main() {
  if (process.argv.includes("--help") || process.argv.includes("-h")) {
    printHelp();
    return;
  }

  await checkHealthAndAuth();

  const wallet = optionalEnv("REALTIME_SMOKE_WALLET", DEFAULT_WALLET);
  const token = signToken(wallet);
  const live = await waitForSseEvent({
    token,
    timeoutMs: 25_000,
    afterConnected: async () => {
      await new Promise((resolve) => setTimeout(resolve, 1_000));
      return emitSmokeEvent(wallet);
    },
  });
  const eventId = live.eventId;

  await waitForSseEvent({
    token,
    expectedEventId: eventId,
    lastEventId: eventId - 1,
    timeoutMs: 10_000,
  });

  console.log(
    [
      "sse=PASS",
      `url=${optionalEnv("REALTIME_URL", DEFAULT_REALTIME_URL)}`,
      `eventId=${eventId}`,
      "liveDelivery=true",
      "lastEventIdReplay=true",
    ].join(" ")
  );
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
