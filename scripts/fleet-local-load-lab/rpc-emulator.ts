import { appendFile, writeFile } from "node:fs/promises";

type MethodStats = {
  calls: number;
  errors: number;
  latenciesMs: number[];
  maxInflight: number;
};

const numberArg = (name: string, fallback: number) => {
  const index = process.argv.indexOf(name);
  return index === -1 ? fallback : Number(process.argv[index + 1]);
};
const stringArg = (name: string) => {
  const index = process.argv.indexOf(name);
  return index === -1 ? undefined : process.argv[index + 1];
};

const port = numberArg("--port", 18899);
const latencyMs = numberArg("--latency-ms", 25);
const jitterMs = numberArg("--jitter-ms", 10);
const errorEvery = numberArg("--error-every", 0);
const summaryPath = stringArg("--summary");
const logPath = stringArg("--log");
if (!summaryPath || !logPath || !Number.isInteger(port) || port < 1024) {
  throw new Error("rpc-emulator requires --summary, --log, and a valid --port");
}

const stats = new Map<string, MethodStats>();
let requestSequence = 0;
let inflight = 0;
let maxInflight = 0;
const startedAt = new Date().toISOString();
const localGenesis = "11111111111111111111111111111111";
const localBlockhash = "SysvarC1ock11111111111111111111111111";

const percentile = (values: number[], fraction: number) => {
  if (!values.length) return null;
  const sorted = [...values].sort((left, right) => left - right);
  return sorted[Math.max(0, Math.ceil(sorted.length * fraction) - 1)]!;
};

const summary = () => ({
  startedAt,
  generatedAt: new Date().toISOString(),
  listenHost: "127.0.0.1",
  port,
  configuredLatencyMs: latencyMs,
  configuredJitterMs: jitterMs,
  configuredErrorEvery: errorEvery,
  requests: requestSequence,
  maxInflight,
  methods: Object.fromEntries(
    [...stats.entries()].map(([method, value]) => [
      method,
      {
        calls: value.calls,
        errors: value.errors,
        p50Ms: percentile(value.latenciesMs, 0.5),
        p95Ms: percentile(value.latenciesMs, 0.95),
        p99Ms: percentile(value.latenciesMs, 0.99),
        maxMs: value.latenciesMs.length ? Math.max(...value.latenciesMs) : null,
        maxInflight: value.maxInflight,
      },
    ]),
  ),
});

const resultFor = (method: string, params: unknown[]) => {
  switch (method) {
    case "getGenesisHash":
      return localGenesis;
    case "getSlot":
    case "getBlockHeight":
      return 1_000_002;
    case "getLatestBlockhash":
      return {
        context: { slot: 1_000_002 },
        value: { blockhash: localBlockhash, lastValidBlockHeight: 2_000_000 },
      };
    case "getSignatureStatuses": {
      const signatures = Array.isArray(params[0]) ? params[0] : [];
      return {
        context: { slot: 1_000_002 },
        value: signatures.map(() => ({
          slot: 1_000_002,
          confirmations: null,
          err: null,
          status: { Ok: null },
          confirmationStatus: "finalized",
        })),
      };
    }
    case "getAccountInfo":
      return { context: { slot: 1_000_002 }, value: null };
    case "getMultipleAccounts": {
      const accounts = Array.isArray(params[0]) ? params[0] : [];
      return { context: { slot: 1_000_002 }, value: accounts.map(() => null) };
    }
    case "getBalance":
      return { context: { slot: 1_000_002 }, value: 10_000_000_000 };
    case "getFeeForMessage":
      return { context: { slot: 1_000_002 }, value: 5_000 };
    case "getMinimumBalanceForRentExemption":
      return 0;
    case "simulateTransaction":
      return {
        context: { slot: 1_000_002 },
        value: {
          err: null,
          logs: [],
          accounts: null,
          unitsConsumed: 175_000,
          returnData: null,
          innerInstructions: null,
          replacementBlockhash: null,
        },
      };
    default:
      return null;
  }
};

const server = Bun.serve({
  hostname: "127.0.0.1",
  port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/health") {
      return Response.json({ status: "ok", host: "127.0.0.1", port });
    }
    if (request.method === "GET" && url.pathname === "/metrics") {
      return Response.json(summary());
    }
    if (request.method !== "POST" || url.pathname !== "/") {
      return new Response("not found", { status: 404 });
    }

    const body = await request.json() as {
      id?: string | number | null;
      method?: string;
      params?: unknown[];
    };
    const method = body.method ?? "<missing>";
    const sequence = ++requestSequence;
    const started = performance.now();
    inflight += 1;
    maxInflight = Math.max(maxInflight, inflight);
    const methodStats = stats.get(method) ?? {
      calls: 0,
      errors: 0,
      latenciesMs: [],
      maxInflight: 0,
    };
    methodStats.calls += 1;
    methodStats.maxInflight = Math.max(methodStats.maxInflight, inflight);
    stats.set(method, methodStats);

    const delay = latencyMs + (jitterMs > 0 ? sequence % (jitterMs + 1) : 0);
    if (delay > 0) await Bun.sleep(delay);
    const injectedError = errorEvery > 0 && sequence % errorEvery === 0;
    const transactionBlocked = method === "sendTransaction";
    const requestFailed = injectedError || transactionBlocked;
    if (requestFailed) methodStats.errors += 1;
    const elapsed = performance.now() - started;
    methodStats.latenciesMs.push(elapsed);
    inflight -= 1;
    await appendFile(
      logPath,
      `${JSON.stringify({
        at: new Date().toISOString(),
        sequence,
        method,
        elapsedMs: elapsed,
        injectedError,
        transactionBlocked,
        inflight,
      })}\n`,
    );

    return Response.json(
      requestFailed
        ? {
            jsonrpc: "2.0",
            id: body.id ?? null,
            error: {
              code: transactionBlocked ? -32004 : -32005,
              message: transactionBlocked
                ? "local_component_lab_blocks_transaction_broadcast"
                : "local_component_lab_injected_rpc_failure",
            },
          }
        : {
            jsonrpc: "2.0",
            id: body.id ?? null,
            result: resultFor(method, body.params ?? []),
          },
    );
  },
});

const shutdown = async () => {
  await writeFile(summaryPath, `${JSON.stringify(summary(), null, 2)}\n`);
  server.stop(true);
  process.exit(0);
};
process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);
console.log(JSON.stringify({ status: "rpc_emulator_ready", host: "127.0.0.1", port }));
