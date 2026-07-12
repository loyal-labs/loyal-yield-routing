import { createHmac } from "node:crypto";
import { neon } from "@neondatabase/serverless";

const DEFAULT_REALTIME_URL = "https://loyal-yield-realtime.onrender.com";
const DEFAULT_ALLOWED_ORIGIN = "https://askloyal.com";
const WALLET = "11111111111111111111111111111111";
const SETTINGS = "SysvarRent111111111111111111111111111111111";
const EARN_VAULT = "SysvarC1ock11111111111111111111111111111111";
const OTHER_WALLET = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

type ClientKind = "web" | "mobile";
type Claims = {
  v: number;
  iss: string;
  aud: string;
  iat: number;
  exp: number;
  walletAddress: string;
  settingsPda: string;
  earnVaultAddress: string;
  solanaEnv: string;
  scopes: string[];
  clientKind: ClientKind;
};
type ParsedSseEvent = { id?: string; event?: string; data: string };
type OpenStream = {
  abort: AbortController;
  reader: ReadableStreamDefaultReader<Uint8Array>;
};

function env(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} must be set`);
  return value;
}

function optionalEnv(name: string, fallback: string): string {
  return process.env[name] || fallback;
}

function baseUrl(): string {
  return optionalEnv("REALTIME_URL", DEFAULT_REALTIME_URL);
}

function base64url(input: string | Buffer): string {
  return Buffer.from(input).toString("base64url");
}

function claims(overrides: Partial<Claims> = {}): Claims {
  const now = Math.floor(Date.now() / 1000);
  return {
    v: 1,
    iss: "loyal-apps",
    aud: "loyal-yield-realtime",
    iat: now,
    exp: now + 300,
    walletAddress: WALLET,
    settingsPda: SETTINGS,
    earnVaultAddress: EARN_VAULT,
    solanaEnv: "mainnet-beta",
    scopes: ["earn", "autodeposit"],
    clientKind: "web",
    ...overrides,
  };
}

function signToken(tokenClaims: Claims, secretName = "REALTIME_AUTH_SECRET"): string {
  const payload = base64url(JSON.stringify(tokenClaims));
  const signature = createHmac("sha256", env(secretName))
    .update(payload)
    .digest("base64url");
  return `${payload}.${signature}`;
}

async function emitEvent(args: {
  eventType?: string;
  reason?: string;
  walletAddress?: string;
  solanaEnv?: string;
} = {}): Promise<string> {
  const sql = neon(env("NEON_DATABASE_URL"));
  const rows = await sql`
    SELECT loyal_yield.emit_realtime_event(
      p_event_type => ${args.eventType ?? "realtime.web_mobile.smoke"},
      p_scope => 'autodeposit',
      p_reason => ${args.reason ?? "smoke"},
      p_solana_env => ${args.solanaEnv ?? "mainnet-beta"},
      p_wallet_address => ${args.walletAddress ?? WALLET},
      p_settings_pda => ${SETTINGS},
      p_smart_account_address => ${EARN_VAULT},
      p_vault_pubkey => ${EARN_VAULT},
      p_payload => '{}'::jsonb
    ) AS event_id
  `;
  const eventId = rows[0]?.event_id?.toString();
  if (!eventId || !/^\d+$/.test(eventId)) {
    throw new Error("emit_realtime_event did not return a decimal event id");
  }
  return eventId;
}

async function emitUnrelatedEvents(count: number): Promise<void> {
  const sql = neon(env("NEON_DATABASE_URL"));
  await sql`
    SELECT loyal_yield.emit_realtime_event(
      p_event_type => 'realtime.web_mobile.unrelated',
      p_scope => 'autodeposit',
      p_reason => 'unrelated_flood',
      p_solana_env => 'mainnet-beta',
      p_wallet_address => ${OTHER_WALLET},
      p_settings_pda => ${SETTINGS},
      p_smart_account_address => ${EARN_VAULT},
      p_vault_pubkey => ${EARN_VAULT},
      p_payload => '{}'::jsonb
    )
    FROM generate_series(1, ${count})
  `;
}

async function emitMatchingEvents(count: number): Promise<void> {
  const sql = neon(env("NEON_DATABASE_URL"));
  await sql`
    SELECT loyal_yield.emit_realtime_event(
      p_event_type => 'realtime.web_mobile.matching_overflow',
      p_scope => 'autodeposit',
      p_reason => 'matching_overflow',
      p_solana_env => 'mainnet-beta',
      p_wallet_address => ${WALLET},
      p_settings_pda => ${SETTINGS},
      p_smart_account_address => ${EARN_VAULT},
      p_vault_pubkey => ${EARN_VAULT},
      p_payload => '{}'::jsonb
    )
    FROM generate_series(1, ${count})
  `;
}

async function latestEventId(): Promise<string> {
  const sql = neon(env("NEON_DATABASE_URL"));
  const rows = await sql`
    SELECT COALESCE(MAX(id), 0)::text AS event_id
    FROM loyal_yield.realtime_events
    WHERE deliverable = TRUE
  `;
  return rows[0]?.event_id?.toString() ?? "0";
}

function parseSseBlock(block: string): ParsedSseEvent | null {
  const event: ParsedSseEvent = { data: "" };
  for (const line of block.split(/\r?\n/)) {
    if (!line || line.startsWith(":")) continue;
    const index = line.indexOf(":");
    const field = index === -1 ? line : line.slice(0, index);
    const value = index === -1 ? "" : line.slice(index + 1).trimStart();
    if (field === "id") event.id = value;
    else if (field === "event") event.event = value;
    else if (field === "data") event.data += event.data ? `\n${value}` : value;
  }
  return event.id || event.event || event.data ? event : null;
}

async function withTimeout<T>(
  promise: Promise<T>,
  timeoutMs: number,
  message: string
): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(message)), timeoutMs);
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

async function openStream(args: {
  token: string;
  cursor?: string;
  queryCursor?: string;
  origin?: string;
}): Promise<OpenStream> {
  const abort = new AbortController();
  const url = new URL("/events", baseUrl());
  if (args.queryCursor !== undefined) url.searchParams.set("cursor", args.queryCursor);
  const response = await fetch(url, {
    headers: {
      Accept: "text/event-stream",
      Authorization: `Bearer ${args.token}`,
      ...(args.cursor === undefined ? {} : { "Last-Event-ID": args.cursor }),
      ...(args.origin === undefined ? {} : { Origin: args.origin }),
    },
    signal: abort.signal,
  });
  if (!response.ok) {
    abort.abort();
    throw new Error(`/events returned ${response.status}`);
  }
  if (!(response.headers.get("content-type") || "").includes("text/event-stream")) {
    abort.abort();
    throw new Error("/events did not return text/event-stream");
  }
  if (!response.body) {
    abort.abort();
    throw new Error("/events response had no body");
  }
  return { abort, reader: response.body.getReader() };
}

async function readEvent(
  stream: OpenStream,
  predicate: (event: ParsedSseEvent) => boolean,
  timeoutMs = 15_000
): Promise<ParsedSseEvent> {
  const decoder = new TextDecoder();
  let buffer = "";
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const remaining = deadline - Date.now();
    const result = await withTimeout(
      stream.reader.read(),
      remaining,
      "SSE read timed out"
    );
    if (result.done) throw new Error("SSE stream ended before expected event");
    buffer += decoder.decode(result.value, { stream: true });
    const blocks = buffer.split(/\r?\n\r?\n/);
    buffer = blocks.pop() || "";
    for (const block of blocks) {
      const event = parseSseBlock(block);
      if (event && predicate(event)) return event;
    }
  }
  throw new Error("SSE event timed out");
}

async function closeStream(stream: OpenStream) {
  stream.abort.abort();
  await stream.reader.cancel().catch(() => {});
}

async function expectStatus(args: {
  token?: string;
  tokenInQuery?: string;
  claims?: Claims;
  expected: number;
  cursor?: string;
  queryCursor?: string;
}) {
  const url = new URL("/events", baseUrl());
  if (args.tokenInQuery) url.searchParams.set("token", args.tokenInQuery);
  if (args.queryCursor) url.searchParams.set("cursor", args.queryCursor);
  const token = args.token ?? (args.claims ? signToken(args.claims) : undefined);
  const response = await fetch(url, {
    headers: {
      Accept: "text/event-stream",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(args.cursor ? { "Last-Event-ID": args.cursor } : {}),
    },
  });
  if (response.status !== args.expected) {
    throw new Error(`expected ${args.expected}, got ${response.status}`);
  }
  await response.body?.cancel();
}

async function verifyHealthAndCors() {
  const health = await fetch(new URL("/healthz", baseUrl()));
  if (!health.ok || (await health.text()) !== "ok") {
    throw new Error(`/healthz failed with ${health.status}`);
  }
  const ready = await fetch(new URL("/readyz", baseUrl()));
  if (!ready.ok) throw new Error(`/readyz failed with ${ready.status}`);

  const allowedOrigin = optionalEnv("REALTIME_ALLOWED_ORIGIN", DEFAULT_ALLOWED_ORIGIN);
  const preflight = await fetch(new URL("/events", baseUrl()), {
    method: "OPTIONS",
    headers: {
      Origin: allowedOrigin,
      "Access-Control-Request-Method": "GET",
      "Access-Control-Request-Headers":
        "authorization, accept, last-event-id, content-type",
    },
  });
  if (!preflight.ok) throw new Error(`allowed preflight returned ${preflight.status}`);
  if (preflight.headers.get("access-control-allow-origin") !== allowedOrigin) {
    throw new Error("allowed preflight did not echo the exact origin");
  }
  const vary = preflight.headers.get("vary") || "";
  if (!vary.toLowerCase().includes("origin")) throw new Error("CORS response lacks Vary: Origin");
  if (preflight.headers.has("access-control-allow-credentials")) {
    throw new Error("credentialed CORS must remain disabled");
  }
  const allowedHeaders = (preflight.headers.get("access-control-allow-headers") || "").toLowerCase();
  for (const name of ["authorization", "accept", "last-event-id", "content-type"]) {
    if (!allowedHeaders.includes(name)) throw new Error(`preflight did not allow ${name}`);
  }

  const unknown = await fetch(new URL("/events", baseUrl()), {
    method: "OPTIONS",
    headers: {
      Origin: "https://unknown.invalid",
      "Access-Control-Request-Method": "GET",
      "Access-Control-Request-Headers": "authorization",
    },
  });
  if (unknown.headers.has("access-control-allow-origin")) {
    throw new Error("unknown origin received CORS permission");
  }

  const native = await openStream({ token: signToken(claims({ clientKind: "mobile" })) });
  await closeStream(native);
}

async function verifyAuthContract() {
  const valid = signToken(claims());
  await expectStatus({ expected: 401 });
  await expectStatus({ tokenInQuery: valid, expected: 401 });
  await expectStatus({ claims: claims({ iss: "wrong" }), expected: 401 });
  await expectStatus({ claims: claims({ aud: "wrong" }), expected: 401 });
  await expectStatus({ claims: claims({ solanaEnv: "testnet" }), expected: 401 });
  await expectStatus({ claims: claims({ scopes: ["public"] }), expected: 401 });
  await expectStatus({
    claims: claims({ clientKind: "desktop" as ClientKind }),
    expected: 401,
  });
  await expectStatus({ claims: claims({ walletAddress: "invalid" }), expected: 401 });
  const now = Math.floor(Date.now() / 1000);
  await expectStatus({ claims: claims({ iat: now + 10, exp: now + 20 }), expected: 401 });
  await expectStatus({ claims: claims({ iat: now - 20, exp: now - 1 }), expected: 401 });
  await expectStatus({ claims: claims({ iat: now, exp: now + 301 }), expected: 401 });
  const broken = `${valid.slice(0, -1)}${valid.endsWith("A") ? "B" : "A"}`;
  await expectStatus({ token: broken, expected: 401 });
  await expectStatus({ token: valid, cursor: "10", queryCursor: "11", expected: 400 });
}

async function verifyExpirationClosure() {
  const now = Math.floor(Date.now() / 1000);
  const stream = await openStream({ token: signToken(claims({ iat: now, exp: now + 2 })) });
  const deadline = Date.now() + 7_000;
  while (Date.now() < deadline) {
    const result = await withTimeout(
      stream.reader.read(),
      7_000,
      "open stream did not close at token expiry"
    );
    if (result.done) {
      stream.abort.abort();
      return;
    }
  }
  await closeStream(stream);
  throw new Error("open stream did not close at token expiry");
}

async function verifyConcurrentAndIsolation() {
  const web = await openStream({ token: signToken(claims({ clientKind: "web" })) });
  const mobile = await openStream({ token: signToken(claims({ clientKind: "mobile" })) });
  const otherUser = await openStream({
    token: signToken(claims({ walletAddress: OTHER_WALLET, clientKind: "mobile" })),
  });
  const otherCluster = await openStream({
    token: signToken(claims({ solanaEnv: "devnet", clientKind: "mobile" })),
  });
  try {
    const eventId = await emitEvent();
    const [webEvent, mobileEvent] = await Promise.all([
      readEvent(web, (event) => event.id === eventId),
      readEvent(mobile, (event) => event.id === eventId),
    ]);
    for (const event of [webEvent, mobileEvent]) {
      const payload = JSON.parse(event.data);
      if (
        event.event !== "loyal_yield" ||
        payload.schemaVersion !== 1 ||
        payload.eventId !== eventId ||
        typeof payload.eventId !== "string"
      ) {
        throw new Error("concurrent client received an invalid envelope");
      }
    }
    for (const isolated of [otherUser, otherCluster]) {
      let leaked = false;
      await Promise.race([
        readEvent(isolated, (event) => event.id === eventId, 2_000).then(() => {
          leaked = true;
        }),
        new Promise((resolve) => setTimeout(resolve, 2_100)),
      ]).catch(() => {});
      if (leaked) throw new Error("event crossed user or cluster boundary");
    }
  } finally {
    await Promise.all([web, mobile, otherUser, otherCluster].map(closeStream));
  }
}

async function verifyReplay() {
  const token = signToken(claims({ clientKind: "mobile" }));
  const eventId = await emitEvent({ reason: "replay" });
  const cursor = (BigInt(eventId) - 1n).toString();
  const replay = await openStream({ token, cursor });
  try {
    const event = await readEvent(replay, (candidate) => candidate.id === eventId);
    const payload = JSON.parse(event.data);
    if (payload.eventId !== eventId || typeof payload.eventId !== "string") {
      throw new Error("replayed event id was not an exact string");
    }
  } finally {
    await closeStream(replay);
  }

  if (process.env.REALTIME_RUN_REPLAY_FLOOD === "true") {
    const matchingId = await emitEvent({ reason: "replay_after_unrelated_flood" });
    await emitUnrelatedEvents(501);
    const floodCursor = (BigInt(matchingId) - 1n).toString();
    const afterFlood = await openStream({ token, cursor: floodCursor });
    try {
      await readEvent(afterFlood, (candidate) => candidate.id === matchingId, 30_000);
    } finally {
      await closeStream(afterFlood);
    }
  }

  if (process.env.REALTIME_RUN_MATCHING_OVERFLOW === "true") {
    const overflowCursor = await latestEventId();
    await emitMatchingEvents(501);
    const overflow = await openStream({ token, cursor: overflowCursor });
    try {
      const event = await readEvent(overflow, (candidate) => {
        try {
          return JSON.parse(candidate.data).eventType === "resync_required";
        } catch {
          return false;
        }
      });
      if (JSON.parse(event.data).reason !== "replay_limit_exceeded") {
        throw new Error("matching overflow returned the wrong resync reason");
      }
      const closed = await withTimeout(
        overflow.reader.read(),
        2_000,
        "matching overflow stream did not close after resync"
      );
      if (!closed.done) {
        throw new Error("matching overflow delivered data after resync");
      }
    } finally {
      await closeStream(overflow);
    }
  }

  const staleCursor = process.env.REALTIME_STALE_CURSOR;
  if (staleCursor) {
    const stale = await openStream({ token, cursor: staleCursor });
    try {
      const event = await readEvent(stale, (candidate) => {
        try {
          return JSON.parse(candidate.data).eventType === "resync_required";
        } catch {
          return false;
        }
      });
      if (JSON.parse(event.data).reason !== "cursor_expired") {
        throw new Error("stale cursor returned the wrong resync reason");
      }
    } finally {
      await closeStream(stale);
    }
  }
}

async function main() {
  await verifyHealthAndCors();
  await verifyAuthContract();
  await verifyExpirationClosure();
  await verifyConcurrentAndIsolation();
  await verifyReplay();
  console.log(
    [
      "sse=PASS",
      `url=${baseUrl()}`,
      "cors=true",
      "bearerAuth=true",
      "expiryClosure=true",
      "concurrentClients=true",
      "identityIsolation=true",
      "replay=true",
      `replayFlood=${process.env.REALTIME_RUN_REPLAY_FLOOD === "true"}`,
      `matchingOverflow=${
        process.env.REALTIME_RUN_MATCHING_OVERFLOW === "true"
      }`,
      `staleCursor=${Boolean(process.env.REALTIME_STALE_CURSOR)}`,
    ].join(" ")
  );
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
