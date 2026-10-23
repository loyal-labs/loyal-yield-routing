// Runs one Earn prepare route handler (the Next route.ts itself) against a recorded fixture and prints
// what it answers, so a Go port can be diffed against it field for field and byte for byte.
//
//   bun scripts/web/earn-tx-parity.ts --route <name> --input scripts/web/earn-parity/fixtures/<name>.json
//   op run --env-file=.env.1password -- sh -c 'bun scripts/web/earn-tx-parity.ts --route <name> --input <fixture> --record'
//   op run --env-file=.env.1password -- sh -c 'bun scripts/web/earn-tx-parity.ts --route <name> --input <fixture> --simulate'
//
// Needs `cd web && bun install --frozen-lockfile && bun run build:solana-packages` once.
//
// Replay (default) serves every RPC and Kamino API call from fixture.transport through a stub fetch and
// every DB, Timescale, auth and env read from fixture.boundary / principal / serverEnv
// (earn-parity/boundary.ts lists the stubbed modules); a call the fixture lacks throws. Stdout is
//   {"status": <http status>, "json": <the route's response body>,
//    "messages": [{"path": <JSON path of a prepared op in json>, "base64": <v0 message>}]}
// where each message is that prepared op hydrated and compiled exactly as the client does
// (compilePreparedOperation) with blockhash 11111111111111111111111111111111. Output is deterministic.
//
// --record runs the same handler against SOLANA_RPC_URL and api.kamino.finance and rewrites
// fixture.transport. Only get* RPC methods and simulateTransaction pass; anything else throws.
// --simulate also runs simulateTransaction on each printed message (sigVerify off, replaceRecentBlockhash
// on) against SOLANA_RPC_URL and prints {path, err, logs} lines to stderr. Nothing is signed or sent.
//
// Go side contract: a git-ignored go/workers/internal/webapp/<route>_scratch_test.go reads
// EARN_PARITY_INPUT (the fixture path) and EARN_PARITY_TS (a file holding this script's stdout), serves
// fixture.transport and fixture.boundary to the Go handler, deep-equals its response body with
// json, and checks earnprep.CompileV0(payer, 1111…, instructions, tables) of each prepared op against
// messages[i].base64 at the same path.
import { readFileSync, writeFileSync } from "node:fs";
import { parseArgs } from "node:util";

import { Connection, VersionedTransaction } from "../../web/node_modules/@solana/web3.js";
import { compilePreparedOperation } from "../../web/packages/loyal-smart-accounts-core/src/core/transport/index";
import {
  hydratePreparedOperation,
  type WirePreparedLoyalSmartAccountsOperation,
} from "../../web/packages/smart-account-vaults/src/wire/prepared-operation-wire.shared";
import { type Fixture, installBoundary, WEB_SRC } from "./earn-parity/boundary";
import { type RecordedCall, recordingFetch, replayFetch } from "./earn-parity/transport";

const PINNED_BLOCKHASH = "11111111111111111111111111111111";
const API = `${WEB_SRC}/app/api/smart-accounts/yield-optimization`;

const routes: Record<string, string> = {
  policies: `${API}/policies/prepare/route.ts`,
  deposit: `${API}/deposits/prepare/route.ts`,
  "withdraw-idle": `${API}/withdrawals/prepare/route.ts`,
  "withdraw-reserve": `${API}/withdrawals/prepare/route.ts`,
  cleanup: `${API}/withdrawals/cleanup/prepare/route.ts`,
  refund: `${API}/policy-refunds/prepare/route.ts`,
};

const { values } = parseArgs({
  options: {
    route: { type: "string" },
    input: { type: "string" },
    record: { type: "boolean" },
    simulate: { type: "boolean" },
  },
});
const routePath = values.route ? routes[values.route] : undefined;
if (!routePath || !values.input) {
  console.error(`usage: bun scripts/web/earn-tx-parity.ts --route <${Object.keys(routes).join("|")}> --input <fixture.json> [--record] [--simulate]`);
  process.exit(2);
}
const fixture = JSON.parse(readFileSync(values.input, "utf8")) as Fixture;
if (fixture.route !== values.route) throw new Error(`fixture is for route ${fixture.route}, not ${values.route}`);

const rpcUrl = (): string => {
  const url = process.env.SOLANA_RPC_URL;
  if (!url) throw new Error("SOLANA_RPC_URL is unset; run under op run --env-file=.env.1password");
  return url;
};
const realFetch = globalThis.fetch;
const recorded: RecordedCall[] = [];
globalThis.fetch = values.record ? recordingFetch(rpcUrl(), realFetch, recorded) : replayFetch(fixture.transport);
Object.assign(process.env, fixture.processEnv);
installBoundary(fixture);

const { POST } = (await import(routePath)) as { POST: (request: Request) => Promise<Response> };
const response = await POST(
  new Request("https://earn-parity.invalid/route", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(fixture.request.body),
  })
);
const json = (await response.json()) as unknown;

function preparedOps(value: unknown, path: string): { path: string; op: WirePreparedLoyalSmartAccountsOperation }[] {
  if (Array.isArray(value)) return value.flatMap((inner, index) => preparedOps(inner, `${path}[${index}]`));
  if (!value || typeof value !== "object") return [];
  const record = value as Record<string, unknown>;
  if (Array.isArray(record.instructions) && Array.isArray(record.lookupTableAccounts) && typeof record.payer === "string") {
    return [{ path, op: record as WirePreparedLoyalSmartAccountsOperation }];
  }
  return Object.keys(record)
    .sort()
    .flatMap((key) => preparedOps(record[key], `${path}.${key}`));
}

const compiled = preparedOps(json, "$").map(({ path, op }) => ({
  path,
  transaction: compilePreparedOperation({ prepared: hydratePreparedOperation(op), blockhash: PINNED_BLOCKHASH }),
}));
console.log(
  JSON.stringify(
    {
      status: response.status,
      json,
      messages: compiled.map(({ path, transaction }) => ({ path, base64: Buffer.from(transaction.message.serialize()).toString("base64") })),
    },
    null,
    2
  )
);

if (values.record) {
  writeFileSync(values.input, `${JSON.stringify({ ...fixture, transport: recorded }, null, 2)}\n`);
  console.error(`recorded ${recorded.length} calls into ${values.input}`);
}

if (values.simulate) {
  const connection = new Connection(rpcUrl(), { commitment: "confirmed", fetch: realFetch });
  for (const { path, transaction } of compiled) {
    const result = await connection.simulateTransaction(new VersionedTransaction(transaction.message), {
      sigVerify: false,
      replaceRecentBlockhash: true,
    });
    console.error(JSON.stringify({ path, err: result.value.err, logs: result.value.logs }));
  }
}
