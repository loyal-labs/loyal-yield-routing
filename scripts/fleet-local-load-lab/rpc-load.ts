import { writeFile } from "node:fs/promises";

const arg = (name: string) => {
  const index = process.argv.indexOf(name);
  return index === -1 ? undefined : process.argv[index + 1];
};
const url = arg("--url");
const durationSeconds = Number(arg("--duration-seconds"));
const concurrency = Number(arg("--concurrency"));
const summaryPath = arg("--summary");
if (
  !url || !summaryPath || !url.startsWith("http://127.0.0.1:")
  || !Number.isInteger(durationSeconds) || durationSeconds < 1
  || !Number.isInteger(concurrency) || concurrency < 1 || concurrency > 256
) {
  throw new Error(
    "rpc-load requires loopback --url, positive --duration-seconds, 1..256 --concurrency, and --summary",
  );
}

const pubkey = "11111111111111111111111111111111";
const signature = "1".repeat(64);
const methods = [
  { method: "getAccountInfo", params: [pubkey, { commitment: "confirmed", encoding: "base64" }] },
  { method: "getAccountInfo", params: [pubkey, { commitment: "confirmed", encoding: "base64" }] },
  { method: "getMultipleAccounts", params: [[pubkey, pubkey, pubkey, pubkey], { commitment: "confirmed", encoding: "base64" }] },
  { method: "getSignatureStatuses", params: [[signature], { searchTransactionHistory: true }] },
  { method: "getBalance", params: [pubkey, { commitment: "confirmed" }] },
  { method: "getLatestBlockhash", params: [{ commitment: "confirmed" }] },
  { method: "simulateTransaction", params: ["AAAA", { encoding: "base64", commitment: "confirmed" }] },
];
const startedAt = new Date();
const deadline = Date.now() + durationSeconds * 1_000;
const latencies: number[] = [];
const byMethod = new Map<string, { calls: number; errors: number }>();
let sequence = 0;
let errors = 0;

const client = async (clientId: number) => {
  while (Date.now() < deadline) {
    const index = sequence++;
    const request = methods[(index + clientId) % methods.length]!;
    const stats = byMethod.get(request.method) ?? { calls: 0, errors: 0 };
    stats.calls += 1;
    byMethod.set(request.method, stats);
    const started = performance.now();
    try {
      const response = await fetch(url, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "x-fleet-load-source": "synthetic",
        },
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: `${clientId}-${index}`,
          method: request.method,
          params: request.params,
        }),
        signal: AbortSignal.timeout(5_000),
      });
      const body = await response.json() as { error?: unknown };
      if (!response.ok || body.error) {
        errors += 1;
        stats.errors += 1;
      }
    } catch {
      errors += 1;
      stats.errors += 1;
    } finally {
      latencies.push(performance.now() - started);
    }
  }
};

await Promise.all(Array.from({ length: concurrency }, (_, index) => client(index)));
latencies.sort((left, right) => left - right);
const percentile = (fraction: number) => latencies[
  Math.max(0, Math.ceil(latencies.length * fraction) - 1)
] ?? null;
const elapsedSeconds = (Date.now() - startedAt.getTime()) / 1_000;
const summary = {
  startedAt: startedAt.toISOString(),
  generatedAt: new Date().toISOString(),
  url: "http://127.0.0.1:[local-rpc-port]",
  concurrency,
  durationSeconds: elapsedSeconds,
  requests: latencies.length,
  errors,
  requestsPerSecond: latencies.length / elapsedSeconds,
  p50Ms: percentile(0.5),
  p95Ms: percentile(0.95),
  p99Ms: percentile(0.99),
  maxMs: latencies.at(-1) ?? null,
  methods: Object.fromEntries(byMethod),
};
await writeFile(summaryPath, `${JSON.stringify(summary, null, 2)}\n`);
console.log(JSON.stringify(summary));
