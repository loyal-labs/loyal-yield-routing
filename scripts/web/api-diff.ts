import { isDeepStrictEqual } from "node:util";

import { mintMobileSession, register, scrub, tamper, walletKeyFromEnv } from "./wallet-session";

type Auth = "cookie" | "bearer" | "none";

type Row = {
  id: string;
  method: "GET" | "POST";
  path: string;
  auth: Auth;
  ignore: string[];
  known: Record<string, string>;
  contract?: string;
  needs?: "position" | "operations";
  snapshot?: string;
  cache?: "public";
  body?: unknown;
  writes?: boolean;
};

const CONTRACT = "earn-max-v4";
const SOURCE_HEADER = "X-Loyal-API-Source";
const ACTIVITY_BUG_2_TO_5 = "bugs 2-4: Next charges a claim the whole transaction and pairs by instruction order";

const earnMaxSummaryIgnore = ["summary.observedSlot", "summary.balanceUsd", "summary.currentApyBps"];
const earnMaxSummaryKnown = {
  "summary.coverage": "bug 1: Next reports a truncated history as complete",
  "summary.earnedUsd": "bugs 1-4: Next mis-charges claims and mis-pairs requests",
  "summary.realizedApyBps": "bug 6: Next prints an APY of 2^63 bps or more, Go emits null",
  "summary.forecastApyBps": "bug 6: Next prints an APY of 2^63 bps or more, Go emits null",
  "summary.apyHistory[].apyBps": "bug 6: Next prints an APY of 2^63 bps or more, Go emits null",
  "summary.apyHistory.length": "bug 6: Next prints an APY of 2^63 bps or more, Go emits null",
};
const earnMaxActivityIgnore = ["performance[].equityUsd"];
const earnMaxActivityKnown = {
  "operations.length": "bug 5: Go skips a transaction with no blockTime, Next writes the 1970 epoch",
  "operations[].amountRaw": ACTIVITY_BUG_2_TO_5,
  "operations[].requestedRaw": ACTIVITY_BUG_2_TO_5,
  "operations[].timestamp": "bugs 4-5: Next keys results by index and writes 1970 for no blockTime",
  "operations[].signature": "bug 4: Next pairs a reordered batch with the wrong signature",
  "operations[].id": "bug 4: Next pairs a reordered batch with the wrong signature",
  "operations[].action": "bug 3: Next pairs claim and request by instruction order",
  "operations[].status": "bug 3: Next pairs claim and request by instruction order",
  "performance.length": "bug 5: Go skips a transaction with no blockTime, Next writes the 1970 epoch",
  "performance[].timestamp": "bug 5: Next writes the 1970 epoch for no blockTime",
};

const earnMax = (id: string, route: string, auth: Auth, rest: Partial<Row>): Row => ({
  id,
  method: "GET",
  path: `/api/smart-accounts/earn-max/${route}`,
  auth,
  ignore: [],
  known: {},
  contract: CONTRACT,
  ...rest,
});

const ROWS: Row[] = [
  {
    id: "stats",
    method: "GET",
    path: "/api/earn/stats",
    auth: "none",
    ignore: [],
    known: {},
    snapshot: "refreshedAt",
    cache: "public",
  },
  ...(["cookie", "bearer"] as const).flatMap((auth) => [
    earnMax(`summary-${auth}`, "summary", auth, {
      ignore: earnMaxSummaryIgnore,
      known: earnMaxSummaryKnown,
      needs: "position",
    }),
    earnMax(`activity-${auth}`, "activity", auth, {
      ignore: earnMaxActivityIgnore,
      known: earnMaxActivityKnown,
      needs: "operations",
    }),
    earnMax(`invite-${auth}`, "invite", auth, {}),
  ]),
  { id: "dapps", method: "GET", path: "/api/mobile/dapps", auth: "none", ignore: [], known: {} },
  { id: "library", method: "GET", path: "/api/mobile/library", auth: "none", ignore: [], known: {} },
  {
    id: "token",
    method: "GET",
    path: "/api/mobile/tokens/EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
    auth: "none",
    ignore: [],
    known: {},
  },
  { id: "summaries", method: "GET", path: "/api/summaries", auth: "none", ignore: [], known: {} },
  earnMax("invite-post-bearer", "invite", "bearer", {
    method: "POST",
    body: { code: "parity-check-not-a-code" },
    writes: true,
  }),
  {
    id: "push-tokens-bearer",
    method: "POST",
    path: "/api/push-tokens",
    auth: "bearer",
    ignore: [],
    known: {},
    body: { token: "ExponentPushToken[parity-check]", platform: "parity" },
    writes: true,
  },
];

type Negative = { id: string; route: string; send: (creds: Creds) => Headers; code: string; uses: Array<"cookie" | "bearer"> };
type Creds = { cookie: string; bearer: string };

const cookieHeader = (value: string) => `${cookieName()}=${value}`;
const cookieName = () => process.env.LOYAL_WEB_SESSION_COOKIE_NAME || "loyal_wallet_session";
const headers = (init: Record<string, string>) => new Headers(init);

const NEGATIVE_ROUTES = ["summary", "activity", "invite"];
const NEGATIVES: Negative[] = NEGATIVE_ROUTES.flatMap((route) => [
  { id: `${route}-no-credentials`, route, send: () => headers({}), code: "unauthenticated", uses: [] },
  {
    id: `${route}-tampered-cookie`,
    route,
    send: (c) => headers({ Cookie: cookieHeader(tamper(c.cookie)) }),
    code: "unauthenticated",
    uses: ["cookie"],
  },
  {
    id: `${route}-tampered-bearer`,
    route,
    send: (c) => headers({ Authorization: `Bearer ${tamper(c.bearer)}` }),
    code: "invalid_mobile_session",
    uses: ["bearer"],
  },
  {
    id: `${route}-bearer-as-cookie`,
    route,
    send: (c) => headers({ Cookie: cookieHeader(c.bearer) }),
    code: "unauthenticated",
    uses: ["cookie", "bearer"],
  },
  {
    id: `${route}-cookie-as-bearer`,
    route,
    send: (c) => headers({ Authorization: `Bearer ${c.cookie}` }),
    code: "invalid_mobile_session",
    uses: ["cookie", "bearer"],
  },
]);

const COLUMNS = (rows: string[][]) => rows.map((row) => row.join("  ")).join("\n");

function help(): string {
  const table = ROWS.map((r) => [
    r.id.padEnd(20),
    r.method.padEnd(4),
    r.path.padEnd(60),
    r.auth.padEnd(6),
    r.writes ? "writes" : "",
  ]);
  const shared = new Map<string, Row[]>();
  for (const r of ROWS.filter((r) => r.ignore.length || Object.keys(r.known).length)) {
    const key = JSON.stringify([r.ignore, r.known]);
    shared.set(key, [...(shared.get(key) ?? []), r]);
  }
  const detail = [...shared.values()].map((rows) =>
    [
      `  ${rows.map((r) => r.id).join(", ")}`,
      rows[0]!.ignore.length ? `    ignore (drift): ${rows[0]!.ignore.join(", ")}` : "",
      ...Object.entries(rows[0]!.known).map(([path, why]) => `    known diff: ${path}  (${why})`),
    ]
      .filter(Boolean)
      .join("\n"),
  );
  return `Usage: bun scripts/web/api-diff.ts <Go origin> <Next origin> [flags]

Compares the Go origin against the Next origin row by row, then checks the
negative auth rows against Go. Exit 1 on any FAIL. Prints no credential.

Flags
  --auth bearer|cookie|both   credential kinds to use (default both)
  --only id,id                run only rows whose id starts with one of these (positive and negative)
  --writes                    also run the rows marked writes (default off)
  --help

Environment
  POLICY_KEYPAIR              test wallet key, mints the bearer via the Next origin
                              (op run --env-file=.env.1password -- bun ...)
  LOYAL_WEB_SESSION_COOKIE    signed-in web session cookie value (see docs/plans/mobile-go.md)
  LOYAL_WEB_SESSION_COOKIE_NAME   default loyal_wallet_session

Every passing row asserts ${SOURCE_HEADER}: go on the Go origin; Earn MAX rows also
assert x-loyal-earn-max-contract: ${CONTRACT} on both. Summary and activity fail with
"parity unverified: fixture has no position" when the fixture has nothing to compare.

Route table (id, method, path, auth)
${COLUMNS(table)}

Ignored drift and known Go/Next diffs (docs/plans/mobile-go.md, "Earn MAX activity bugs").
A known diff prints as KNOWN; any other differing field fails the row.
${detail.join("\n")}

Negative rows (Go only, expect 401 with the error code): ${NEGATIVE_ROUTES.map((r) => `/earn-max/${r}`).join(", ")}
${NEGATIVES.slice(0, 5)
  .map((n) => `  ${n.id.replace(/^summary-/, "<route>-")}: 401 ${n.code}`)
  .join("\n")}
Auth codes not exercised (no fixture): 401 wallet_required (email-only cookie), 409 smart_account_required.
`;
}

function diffPaths(a: unknown, b: unknown, path = ""): string[] {
  if (Array.isArray(a) && Array.isArray(b)) {
    const out = a.length === b.length ? [] : [`${path}.length`];
    for (let i = 0; i < Math.min(a.length, b.length); i++) out.push(...diffPaths(a[i], b[i], `${path}[]`));
    return out;
  }
  const isObject = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
  if (isObject(a) && isObject(b)) {
    const out: string[] = [];
    for (const key of new Set([...Object.keys(a), ...Object.keys(b)])) {
      const child = path ? `${path}.${key}` : key;
      if (!(key in a) || !(key in b)) out.push(child);
      else out.push(...diffPaths(a[key], b[key], child));
    }
    return out;
  }
  return isDeepStrictEqual(a, b) ? [] : [path];
}

type Read = { response: Response; body: any };

async function read(origin: string, row: Row, creds: Creds | null, nonce: string): Promise<Read> {
  const url = new URL(row.path, origin);
  if (row.snapshot) url.searchParams.set("migration-check", nonce);
  const requestHeaders = new Headers({ "Cache-Control": "no-cache" });
  if (row.auth === "cookie" && creds) requestHeaders.set("Cookie", cookieHeader(creds.cookie));
  if (row.auth === "bearer" && creds) requestHeaders.set("Authorization", `Bearer ${creds.bearer}`);
  if (row.body !== undefined) requestHeaders.set("Content-Type", "application/json");
  const response = await fetch(url, {
    method: row.method,
    redirect: "error",
    signal: AbortSignal.timeout(15_000),
    headers: requestHeaders,
    body: row.body === undefined ? undefined : JSON.stringify(row.body),
  });
  const text = await response.text();
  let body: unknown = null;
  try {
    body = JSON.parse(text);
  } catch {}
  return { response, body };
}

const describe = (origin: string, r: Read) => {
  const code = r.body?.error?.code;
  return `${origin} answered HTTP ${r.response.status}${typeof code === "string" ? ` ${code}` : ""}`;
};

const hasPosition = (body: any) =>
  !!body?.summary && (Number(body.summary.balanceUsd) > 0 || body.summary.withdrawal != null);

type Outcome = { lines: string[]; failed: boolean };

async function runRow(row: Row, go: string, next: string, creds: Creds | null): Promise<Outcome> {
  const lines: string[] = [];
  const fail = (reason: string): Outcome => ({ lines: [...lines, `FAIL ${row.id}: ${reason}`], failed: true });

  let pair: [Read, Read] | null = null;
  const attempts = row.snapshot ? 3 : 1;
  for (let attempt = 0; attempt < attempts; attempt++) {
    const nonce = `${Date.now()}-${attempt}`;
    const [g, n] = await Promise.all([read(go, row, creds, nonce), read(next, row, creds, nonce)]);
    pair = [g, n];
    if (!row.snapshot) break;
    if (typeof g.body?.[row.snapshot] !== "string" || typeof n.body?.[row.snapshot] !== "string") {
      return fail(`response is missing ${row.snapshot}`);
    }
    if (g.body[row.snapshot] === n.body[row.snapshot]) break;
    pair = null;
  }
  if (!pair) return fail(`no matching ${row.snapshot} in three reads; parity remains unverified`);
  const [g, n] = pair;

  if (!row.writes && (!g.response.ok || !n.response.ok)) {
    return fail(`expected HTTP 200 from both; ${describe("Go", g)}, ${describe("Next", n)}`);
  }
  if (g.response.headers.get(SOURCE_HEADER) !== "go") {
    return fail(`Go origin did not answer from Go: ${SOURCE_HEADER} is ${g.response.headers.get(SOURCE_HEADER) ?? "absent"}, still proxying`);
  }
  if (row.contract) {
    for (const [name, r] of [["Go", g], ["Next", n]] as const) {
      const got = r.response.headers.get("x-loyal-earn-max-contract");
      if (got !== row.contract) return fail(`${name} x-loyal-earn-max-contract is ${got ?? "absent"}, need ${row.contract}`);
    }
  }
  if (row.cache && g.response.headers.get("Cache-Control")?.split(",")[0] !== row.cache) {
    return fail(`Go response is missing ${row.cache} cache control`);
  }
  if (row.needs === "position" && (!hasPosition(g.body) || !hasPosition(n.body))) {
    return fail("parity unverified: fixture has no position");
  }
  if (row.needs === "operations" && !(g.body?.operations?.length > 0 && n.body?.operations?.length > 0)) {
    return fail("parity unverified: fixture has no position (activity has no operations)");
  }
  if (g.response.status !== n.response.status) {
    return fail(`status differs; ${describe("Go", g)}, ${describe("Next", n)}`);
  }

  const unexpected: string[] = [];
  const known = new Set<string>();
  for (const path of new Set(diffPaths(g.body, n.body))) {
    if (row.ignore.includes(path)) continue;
    if (path in row.known) known.add(path);
    else unexpected.push(path || "(root)");
  }
  for (const path of known) lines.push(`KNOWN ${row.id}: ${path} differs (${row.known[path]})`);
  if (unexpected.length) return fail(`JSON differs at ${unexpected.join(", ")}`);
  const at = row.snapshot ? ` at ${g.body[row.snapshot]}` : "";
  lines.push(`PASS ${row.id}: ${row.method} ${row.path} equal JSON${at}; served by Go`);
  return { lines, failed: false };
}

async function runNegative(n: Negative, go: string, creds: Creds): Promise<Outcome> {
  const response = await fetch(new URL(`/api/smart-accounts/earn-max/${n.route}`, go), {
    redirect: "error",
    signal: AbortSignal.timeout(15_000),
    headers: n.send(creds),
  });
  const body = (await response.json().catch(() => null)) as { error?: { code?: string } } | null;
  const code = body?.error?.code;
  const label = `negative ${n.id}`;
  if (response.status !== 401) {
    return { lines: [`FAIL ${label}: expected HTTP 401, Go answered HTTP ${response.status}`], failed: true };
  }
  if (code !== n.code) {
    return { lines: [`FAIL ${label}: expected 401 ${n.code}, Go answered 401 ${code ?? "without a code"}`], failed: true };
  }
  return { lines: [`PASS ${label}: 401 ${code}`], failed: false };
}

function parse(argv: string[]) {
  const flags = { auth: "both", only: [] as string[], writes: false, help: false };
  const origins: string[] = [];
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]!;
    if (arg === "--help" || arg === "-h") flags.help = true;
    else if (arg === "--writes") flags.writes = true;
    else if (arg === "--auth") flags.auth = argv[++i] ?? "";
    else if (arg === "--only") flags.only = (argv[++i] ?? "").split(",").filter(Boolean);
    else if (arg.startsWith("--")) throw new Error(`Unknown flag ${arg}. See --help.`);
    else origins.push(arg);
  }
  if (!["both", "cookie", "bearer"].includes(flags.auth)) throw new Error("--auth takes bearer, cookie or both");
  return { flags, origins };
}

async function main() {
  const { flags, origins } = parse(process.argv.slice(2));
  if (flags.help) {
    console.log(help());
    return 0;
  }
  const [go, next] = origins;
  if (!go || !next) throw new Error("Usage: bun scripts/web/api-diff.ts <Go origin> <Next origin> (see --help)");

  const wantBearer = flags.auth !== "cookie";
  const wantCookie = flags.auth !== "bearer";
  const cookie = register(process.env.LOYAL_WEB_SESSION_COOKIE);
  if (wantCookie && !cookie) {
    throw new Error("LOYAL_WEB_SESSION_COOKIE is unset. Supply it, or pass --auth bearer to skip the cookie rows.");
  }
  const rows = ROWS.filter((r) => {
    if (r.auth === "cookie" && !wantCookie) return false;
    if (r.auth === "bearer" && !wantBearer) return false;
    return !flags.only.length || flags.only.some((id) => r.id.startsWith(id));
  });
  const negatives = NEGATIVES.filter((n) => {
    if (n.uses.includes("cookie") && !wantCookie) return false;
    if (n.uses.includes("bearer") && !wantBearer) return false;
    return !flags.only.length || flags.only.some((id) => n.id.startsWith(id));
  });

  const needsToken = wantBearer && (rows.some((r) => r.auth === "bearer") || negatives.some((n) => n.uses.includes("bearer")));
  let creds: Creds = { cookie, bearer: "" };
  if (needsToken) {
    const session = await mintMobileSession(next, walletKeyFromEnv());
    creds = { cookie, bearer: session.token };
    console.log(`minted a mobile session at ${next}; expires ${session.expiresAt}`);
  }

  const outcomes: Outcome[] = [];
  for (const row of rows) {
    if (row.writes && !flags.writes) {
      console.log(`SKIP ${row.id}: writes state; pass --writes to run it`);
      continue;
    }
    const outcome = await runRow(row, go, next, creds).catch((error): Outcome => ({
      lines: [`FAIL ${row.id}: ${error instanceof Error ? error.message : String(error)}`],
      failed: true,
    }));
    outcomes.push(outcome);
    for (const line of outcome.lines) console.log(scrub(line));
  }
  for (const negative of negatives) {
    const outcome = await runNegative(negative, go, creds).catch((error): Outcome => ({
      lines: [`FAIL negative ${negative.id}: ${error instanceof Error ? error.message : String(error)}`],
      failed: true,
    }));
    outcomes.push(outcome);
    for (const line of outcome.lines) console.log(scrub(line));
  }
  const failed = outcomes.filter((o) => o.failed).length;
  console.log(`${outcomes.length - failed} passed, ${failed} failed`);
  return failed ? 1 : 0;
}

if (import.meta.main) {
  process.exitCode = await main().catch((error) => {
    console.error(scrub(error instanceof Error ? error.message : String(error)));
    return 1;
  });
}
