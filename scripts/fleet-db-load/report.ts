import { readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

type RoleEvidence = {
  transactions: number;
  averageMs: number | null;
  tps: number | null;
  p50Ms: number | null;
  p95Ms: number | null;
  p99Ms: number | null;
  maxMs: number | null;
};

const runDirectory = process.argv[2];
if (!runDirectory) {
  throw new Error("usage: bun report.ts RUN_DIRECTORY");
}

const yekaterinburgTimestamp = (date: Date) => {
  const parts = new Intl.DateTimeFormat("sv-SE", {
    timeZone: "Asia/Yekaterinburg",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  }).format(date);
  return `${parts.replace(" ", "T")}+05:00`;
};

const readJson = async (path: string) =>
  JSON.parse((await readFile(path, "utf8")).trim());

const percentile = (sorted: number[], fraction: number): number | null => {
  if (sorted.length === 0) return null;
  const index = Math.min(sorted.length - 1, Math.ceil(sorted.length * fraction) - 1);
  return sorted[index] / 1000;
};

const parseRole = async (scenarioDirectory: string, role: string): Promise<RoleEvidence> => {
  const stdout = await readFile(join(scenarioDirectory, `${role}.stdout`), "utf8");
  const files = await readdir(scenarioDirectory);
  const latencyMicros: number[] = [];
  for (const file of files.filter(
    (name) =>
      name.startsWith(`${role}.`) && /^\D+\.\d+(?:\.\d+)?$/.test(name),
  )) {
    const log = await readFile(join(scenarioDirectory, file), "utf8");
    for (const line of log.split("\n")) {
      if (!line.trim()) continue;
      const fields = line.trim().split(/\s+/);
      const value = Number(fields[2]);
      if (Number.isFinite(value)) latencyMicros.push(value);
    }
  }
  latencyMicros.sort((left, right) => left - right);
  const transactions = Number(
    stdout.match(/transactions actually processed:\s+(\d+)/)?.[1] ?? 0,
  );
  const averageMsMatch = stdout.match(/latency average =\s+([\d.]+) ms/);
  const tpsMatch = stdout.match(/tps =\s+([\d.]+)/);
  return {
    transactions,
    averageMs: averageMsMatch ? Number(averageMsMatch[1]) : null,
    tps: tpsMatch ? Number(tpsMatch[1]) : null,
    p50Ms: percentile(latencyMicros, 0.5),
    p95Ms: percentile(latencyMicros, 0.95),
    p99Ms: percentile(latencyMicros, 0.99),
    maxMs: latencyMicros.length ? latencyMicros.at(-1)! / 1000 : null,
  };
};

const config = await readJson(join(runDirectory, "run-config.json"));
const scenarioNames = (await readdir(runDirectory))
  .filter((name) => name.startsWith("scale-"))
  .sort((left, right) => Number(left.slice(6)) - Number(right.slice(6)));

const scenarios = [];
for (const scenarioName of scenarioNames) {
  const scenarioDirectory = join(runDirectory, scenarioName);
  const database = await readJson(join(scenarioDirectory, "database.json"));
  const explain = await readJson(join(scenarioDirectory, "explain.json"));
  const roles: Record<string, RoleEvidence> = {};
  for (const role of [
    "baseline-health",
    "health",
    "executor",
    "confirmer",
    "reconciler",
    "planner",
    "user",
    "mock-chain",
  ]) {
    roles[role] = await parseRole(scenarioDirectory, role);
  }
  scenarios.push({
    scale: Number(scenarioName.slice(6)),
    database,
    healthExplain: {
      planningMs: explain[0]["Planning Time"],
      executionMs: explain[0]["Execution Time"],
      sharedHitBlocks: explain[0].Plan["Shared Hit Blocks"] ?? 0,
      sharedReadBlocks: explain[0].Plan["Shared Read Blocks"] ?? 0,
      tempReadBlocks: explain[0].Plan["Temp Read Blocks"] ?? 0,
      tempWrittenBlocks: explain[0].Plan["Temp Written Blocks"] ?? 0,
    },
    roles,
  });
}

const evidence = {
  generatedAt: yekaterinburgTimestamp(new Date()),
  timeZone: "Asia/Yekaterinburg",
  isolation: {
    externalDatabaseConnections: false,
    externalBlockchainConnections: false,
    databaseGuard: "127.0.0.1 and fleet_verify_* database name",
    blockchainModel: "local updates to production snapshot projection tables",
  },
  config,
  scenarios,
};
await writeFile(
  join(runDirectory, "evidence.json"),
  `${JSON.stringify(evidence, null, 2)}\n`,
);

const formatNumber = (value: number | null, digits = 2) =>
  value === null ? "n/a" : value.toFixed(digits);
const formatBytes = (value: number) => `${(value / 1024 / 1024).toFixed(1)} MiB`;

const markdown = `# Fleet database load reproduction

Generated: ${evidence.generatedAt} (Asia/Yekaterinburg)

This run used the repository's real Yield migrations and the exact
\`fleet_orchestration_status\` worker query. PostgreSQL listened only on
\`127.0.0.1\`; production database and RPC environment variables were removed.
User traffic and blockchain observations were generated locally against the
production queue and snapshot table shapes.

## Health-query scaling

| Opportunities | DB size | Explain execution | Baseline p95 | Loaded p95 | Loaded max | Health tx |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
${scenarios
  .map(
    (scenario) =>
      `| ${scenario.scale.toLocaleString()} | ${formatBytes(scenario.database.databaseBytes)} | ${formatNumber(scenario.healthExplain.executionMs)} ms | ${formatNumber(scenario.roles["baseline-health"].p95Ms)} ms | ${formatNumber(scenario.roles.health.p95Ms)} ms | ${formatNumber(scenario.roles.health.maxMs)} ms | ${scenario.roles.health.transactions.toLocaleString()} |`,
  )
  .join("\n")}

## Concurrent role throughput

| Opportunities | Executor TPS | Confirmer TPS | Reconciler TPS | Planner TPS | User TPS | Mock-chain TPS |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
${scenarios
  .map(
    (scenario) =>
      `| ${scenario.scale.toLocaleString()} | ${formatNumber(scenario.roles.executor.tps)} | ${formatNumber(scenario.roles.confirmer.tps)} | ${formatNumber(scenario.roles.reconciler.tps)} | ${formatNumber(scenario.roles.planner.tps)} | ${formatNumber(scenario.roles.user.tps)} | ${formatNumber(scenario.roles["mock-chain"].tps)} |`,
  )
  .join("\n")}

## Dataset shape

Each opportunity scale includes one decision and signed submission per four
opportunities and one outbox row per two opportunities, plus 1,000 managed
vaults and current position projections. Historical and active states are
mixed deterministically. See \`evidence.json\` and per-scale PostgreSQL plans
for full machine-readable evidence.

## Interpretation boundary

This isolates SQL/schema growth and local contention. It does not model Neon
network latency, Neon autoscaling/cache topology, Solana RPC latency, validator
execution, transaction simulation, or signature confirmation. Those effects
must be added separately when translating the local curve into production
capacity.
`;

await writeFile(join(runDirectory, "evidence.md"), markdown);
console.log(join(runDirectory, "evidence.md"));
