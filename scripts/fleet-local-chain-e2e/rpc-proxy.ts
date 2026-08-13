#!/usr/bin/env bun

import { appendFile, writeFile } from "node:fs/promises";

type MethodStats = {
  calls: number;
  errors: number;
  latenciesMs: number[];
  maxInflight: number;
};

const valueArg = (name: string): string | undefined => {
  const index = process.argv.indexOf(name);
  return index === -1 ? undefined : process.argv[index + 1];
};
const integerArg = (name: string, fallback: number): number => {
  const value = valueArg(name);
  return value === undefined ? fallback : Number(value);
};

const port = integerArg("--port", 18_899);
const upstream = valueArg("--upstream");
const latencyMs = integerArg("--latency-ms", 0);
const jitterMs = integerArg("--jitter-ms", 0);
const errorEvery = integerArg("--error-every", 0);
const logPath = valueArg("--log");
const summaryPath = valueArg("--summary");
if (
  !upstream || !/^http:\/\/127\.0\.0\.1:\d+$/u.test(upstream) ||
  !logPath || !summaryPath ||
  !Number.isInteger(port) || port < 1024 || port > 65_535 ||
  !Number.isInteger(latencyMs) || latencyMs < 0 ||
  !Number.isInteger(jitterMs) || jitterMs < 0 ||
  !Number.isInteger(errorEvery) || errorEvery < 0
) {
  throw new Error(
    "rpc-proxy requires loopback --upstream, --log, --summary, a non-privileged --port, and nonnegative latency/fault values",
  );
}

const stats = new Map<string, MethodStats>();
const sourceCounts = new Map<string, number>();
let sequence = 0;
let inflight = 0;
let maxInflight = 0;
const startedAt = new Date().toISOString();

const percentile = (values: number[], fraction: number): number | null => {
  if (values.length === 0) return null;
  const sorted = [...values].sort((left, right) => left - right);
  return sorted[Math.max(0, Math.ceil(sorted.length * fraction) - 1)]!;
};

const summary = () => ({
  kind: "stateful-local-validator-rpc-proxy",
  startedAt,
  generatedAt: new Date().toISOString(),
  listenHost: "127.0.0.1",
  upstream: "http://127.0.0.1:[local-validator-port]",
  configuredLatencyMs: latencyMs,
  configuredJitterMs: jitterMs,
  configuredErrorEvery: errorEvery,
  requests: sequence,
  maxInflight,
  sources: Object.fromEntries([...sourceCounts.entries()].sort()),
  methods: Object.fromEntries(
    [...stats.entries()].sort().map(([method, value]) => [method, {
      calls: value.calls,
      errors: value.errors,
      p50Ms: percentile(value.latenciesMs, 0.5),
      p95Ms: percentile(value.latenciesMs, 0.95),
      p99Ms: percentile(value.latenciesMs, 0.99),
      maxMs: value.latenciesMs.length === 0 ? null : Math.max(...value.latenciesMs),
      maxInflight: value.maxInflight,
    }]),
  ),
});

const server = Bun.serve({
  hostname: "127.0.0.1",
  port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/health") {
      return Response.json({ status: "ok", kind: "stateful-local-validator-rpc-proxy" });
    }
    if (request.method === "GET" && url.pathname === "/metrics") {
      return Response.json(summary());
    }
    if (request.method !== "POST" || url.pathname !== "/") {
      return new Response("not found", { status: 404 });
    }

    const raw = await request.text();
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      return Response.json({ error: "invalid JSON-RPC body" }, { status: 400 });
    }
    const bodies = Array.isArray(parsed) ? parsed : [parsed];
    const methods = bodies.map((body) =>
      body && typeof body === "object" && "method" in body && typeof body.method === "string"
        ? body.method
        : "<missing>"
    );
    const sourceHeader = request.headers.get("x-fleet-load-source");
    const source = sourceHeader === "synthetic" ? "syntheticRpc" : "productionProcess";
    sourceCounts.set(source, (sourceCounts.get(source) ?? 0) + 1);

    const currentSequence = ++sequence;
    const started = performance.now();
    inflight += 1;
    maxInflight = Math.max(maxInflight, inflight);
    for (const method of methods) {
      const value = stats.get(method) ?? { calls: 0, errors: 0, latenciesMs: [], maxInflight: 0 };
      value.calls += 1;
      value.maxInflight = Math.max(value.maxInflight, inflight);
      stats.set(method, value);
    }

    const delay = latencyMs + (jitterMs > 0 ? currentSequence % (jitterMs + 1) : 0);
    if (delay > 0) await Bun.sleep(delay);
    const injectedError = errorEvery > 0 && currentSequence % errorEvery === 0;
    let response: Response;
    if (injectedError) {
      const body = bodies[0] as { id?: unknown } | undefined;
      response = Response.json({
        jsonrpc: "2.0",
        id: body?.id ?? null,
        error: { code: -32005, message: "local_chain_e2e_injected_rpc_failure" },
      });
    } else {
      response = await fetch(upstream, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: raw,
        signal: AbortSignal.timeout(30_000),
      });
    }
    const responseBody = await response.arrayBuffer();
    const elapsedMs = performance.now() - started;
    inflight -= 1;
    const responseText = new TextDecoder().decode(responseBody);
    const rpcErrored = injectedError || !response.ok || /"error"\s*:\s*\{/u.test(responseText);
    for (const method of methods) {
      const value = stats.get(method)!;
      if (rpcErrored) value.errors += 1;
      value.latenciesMs.push(elapsedMs);
    }
    await appendFile(logPath, `${JSON.stringify({
      atUtc: new Date().toISOString(),
      sequence: currentSequence,
      methods,
      source,
      elapsedMs,
      injectedError,
      rpcErrored,
      inflightAfter: inflight,
    })}\n`);
    return new Response(responseBody, {
      status: response.status,
      headers: { "content-type": response.headers.get("content-type") ?? "application/json" },
    });
  },
});

let closing = false;
const shutdown = async () => {
  if (closing) return;
  closing = true;
  await writeFile(summaryPath, `${JSON.stringify(summary(), null, 2)}\n`);
  server.stop(true);
  process.exit(0);
};
process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);
console.log(JSON.stringify({ status: "ready", kind: "stateful-local-validator-rpc-proxy", port }));
