import { readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { causalEvidenceFailures } from "./causal-evidence";

type RoleEvidence = {
  transactions: number;
  tps: number | null;
  averageMs: number | null;
  p50Ms: number | null;
  p95Ms: number | null;
  p99Ms: number | null;
  maxMs: number | null;
  stderr: string;
};

const runDirectory = process.argv[2];
if (!runDirectory) throw new Error("usage: bun report.ts RUN_DIRECTORY");

const readText = (path: string) => readFile(path, "utf8");
const readJson = async (path: string) => JSON.parse((await readText(path)).trim());
const percentile = (values: number[], fraction: number) => {
  if (!values.length) return null;
  const sorted = [...values].sort((left, right) => left - right);
  return sorted[Math.max(0, Math.ceil(sorted.length * fraction) - 1)]!;
};
const number = (value: number | null, digits = 2) =>
  value === null ? "n/a" : value.toFixed(digits);
const bytes = (value: number) => `${(value / 1024 / 1024).toFixed(1)} MiB`;

const parseRole = async (role: string): Promise<RoleEvidence> => {
  const directory = join(runDirectory, "workloads");
  const stdout = await readText(join(directory, `${role}.stdout`));
  const stderr = await readText(join(directory, `${role}.stderr`)).catch(() => "");
  const files = await readdir(directory);
  const latencyMicros: number[] = [];
  for (const file of files.filter(
    (name) => name.startsWith(`${role}.`) && /^.+\.\d+(?:\.\d+)?$/.test(name),
  )) {
    for (const line of (await readText(join(directory, file))).split("\n")) {
      if (!line.trim()) continue;
      const value = Number(line.trim().split(/\s+/)[2]);
      if (Number.isFinite(value)) latencyMicros.push(value);
    }
  }
  const transactions = Number(
    stdout.match(/transactions actually processed:\s+(\d+)/)?.[1] ?? 0,
  );
  const average = stdout.match(/latency average =\s+([\d.]+) ms/)?.[1];
  const tps = stdout.match(/tps =\s+([\d.]+)/)?.[1];
  return {
    transactions,
    tps: tps ? Number(tps) : null,
    averageMs: average ? Number(average) : null,
    p50Ms: percentile(latencyMicros, 0.5) === null ? null : percentile(latencyMicros, 0.5)! / 1000,
    p95Ms: percentile(latencyMicros, 0.95) === null ? null : percentile(latencyMicros, 0.95)! / 1000,
    p99Ms: percentile(latencyMicros, 0.99) === null ? null : percentile(latencyMicros, 0.99)! / 1000,
    maxMs: latencyMicros.length ? Math.max(...latencyMicros) / 1000 : null,
    stderr: stderr.trim().slice(-2_000),
  };
};

const workerRoles = ["planner", "revalidator", "executor", "confirmer", "reconciler"];
const workerLogs: Record<string, unknown> = {};
let workerErrorLines = 0;
let localCatalogMissing = false;
let realWorkerClaimed = 0;
let realWorkerCompleted = 0;
const roleProbePasses: Record<string, boolean> = {};
const workerLogSizes: Array<{ role: string; lines: number; bytes: number }> = [];
for (const role of workerRoles) {
  const log = await readText(join(runDirectory, "workers", `${role}.log`));
  const lines = log.split("\n").filter(Boolean);
  const jsonLines = lines.flatMap((line) => {
    try { return [JSON.parse(line)]; } catch { return []; }
  });
  const errorLineCount = lines.filter((line) =>
    line.includes(" ERROR ")
      || /\bfatal\b|\bpanic(?:ked)?\b|operational_error|"status":"[^"]*_failed"/i.test(line)
  ).length;
  workerErrorLines += errorLineCount;
  realWorkerClaimed += jsonLines.reduce(
    (sum, value) => sum + Number(value.claimed ?? 0),
    0,
  );
  realWorkerCompleted += jsonLines.reduce(
    (sum, value) => sum + Number(
      value.completed ?? value.confirmed ?? value.reconciled ?? 0
    ),
    0,
  );
  localCatalogMissing ||= log.includes(
    "fleet position sweep requires a durable shared-market catalog head",
  );
  if (role === "revalidator" || role === "executor") {
    roleProbePasses[role] = jsonLines.some((value) =>
      value.event === "fleet_worker_role_probe"
      && value.status === "pass"
      && value.role === role
      && value.networkAccessed === false
      && value.secretsLoaded === false
      && value.databaseMutated === false
      && value.transactionSent === false
    );
  }
  workerLogSizes.push({ role, lines: lines.length, bytes: Buffer.byteLength(log) });
  workerLogs[role] = {
    lineCount: lines.length,
    jsonLineCount: jsonLines.length,
    errorLineCount,
    statuses: jsonLines
      .map((value) => value.status ?? value.event)
      .filter((value) => typeof value === "string")
      .slice(-20),
    tail: lines.slice(-10),
  };
}

const processStatusRows = (await readText(join(runDirectory, "process-status.csv")))
  .trim().split("\n").slice(1).filter(Boolean).map((line) => {
    const [role, pid, alive] = line.split(",");
    return { role, pid: Number(pid), aliveBeforeShutdown: alive === "true" };
  });
const samples = (await readText(join(runDirectory, "process-samples.csv")))
  .trim().split("\n").slice(1).filter(Boolean).map((line) => {
    const [capturedAt, role, pid, cpu, rssKiB, elapsed] = line.split(",");
    return { capturedAt, role, pid: Number(pid), cpu: Number(cpu), rssKiB: Number(rssKiB), elapsed };
  });
const processMetrics = Object.fromEntries(workerRoles.map((role) => {
  const rows = samples.filter((sample) => sample.role === role);
  const rss = rows.map((row) => row.rssKiB);
  const cpu = rows.map((row) => row.cpu);
  return [role, {
    samples: rows.length,
    rssP95MiB: percentile(rss, 0.95) === null ? null : percentile(rss, 0.95)! / 1024,
    rssMaxMiB: rss.length ? Math.max(...rss) / 1024 : null,
    cpuAveragePercent: cpu.length ? cpu.reduce((sum, value) => sum + value, 0) / cpu.length : null,
    cpuMaxPercent: cpu.length ? Math.max(...cpu) : null,
  }];
}));

const roleNames = [
  "baseline-health", "health", "executor", "confirmer",
  "reconciler", "planner", "user", "mock-chain",
];
const workloads: Record<string, RoleEvidence> = {};
for (const role of roleNames) workloads[role] = await parseRole(role);
const sourceHealth = await parseRole("source-health");
const projectorLines = (await readText(join(runDirectory, "workers", "health-projector.log")))
  .split("\n").filter(Boolean);
const projectorJsonLines = projectorLines.flatMap((line) => {
  try { return [JSON.parse(line)]; } catch { return []; }
});
const projector = projectorJsonLines.findLast(
  (value) => value.status === "fleet_health_snapshot_refreshed",
);
if (!projector) throw new Error("health projector did not publish a refresh record");
const contenderLines = (await readText(join(runDirectory, "workers", "health-projector-contender.log")))
  .split("\n").filter(Boolean);
const contenderJsonLines = contenderLines.flatMap((line) => {
  try { return [JSON.parse(line)]; } catch { return []; }
});
const contender = contenderJsonLines.findLast(
  (value) => value.status === "fleet_health_snapshot_refresh_skipped",
);
if (!contender) throw new Error("health projector contender was not fenced");
const plannerCoalescing = await readJson(join(runDirectory, "planner-coalescing.json"));
const healthHotPath = await readJson(join(runDirectory, "health-hot-path.json"));

const config = await readJson(join(runDirectory, "run-config.json"));
const databaseBefore = await readJson(join(runDirectory, "database-before.json"));
const databaseAfter = await readJson(join(runDirectory, "database-after.json"));
const rpc = await readJson(join(runDirectory, "rpc-summary.json"));
const rpcLoad = await readJson(join(runDirectory, "rpc-load-summary.json"));
const plannerBenchmark = await readJson(join(runDirectory, "planner-benchmark.json"));
const allStartedProcessesSurvived = processStatusRows.every(
  (row) => row.aliveBeforeShutdown,
);
const deadlocks = Number(databaseAfter.databaseStats?.deadlocks ?? 0);
const concurrentTempBytes = Number(databaseAfter.databaseStats?.temp_bytes ?? 0)
  - Number(databaseBefore.databaseStats?.temp_bytes ?? 0);
const hotTempBytes = Number(healthHotPath.tempBytes ?? 0);
const workloadErrors = Object.values(workloads).filter((role) => role.stderr).length;
const rpcErrors = Object.values(rpc.methods ?? {}).reduce(
  (sum: number, method: any) => sum + Number(method.errors ?? 0), 0,
);
const healthP95 = workloads.health.p95Ms;
const outboxRowsAdded = Number(databaseAfter.rows?.outbox ?? 0)
  - Number(databaseBefore.rows?.outbox ?? 0);
const outboxRowsPerSecond = outboxRowsAdded / Number(config.durationSeconds);
const syntheticOutboxRowsAdded = Number(databaseAfter.rows?.syntheticOutbox ?? 0)
  - Number(databaseBefore.rows?.syntheticOutbox ?? 0);
const workerOutboxRowsAdded = outboxRowsAdded - syntheticOutboxRowsAdded;
const syntheticRpcRequests = Number(rpcLoad.requests ?? 0);
const realWorkerRpcRequests = Math.max(
  0,
  Number(rpc.requests ?? 0) - syntheticRpcRequests,
);
const realWorkerProgressPasses = realWorkerCompleted > 0;
const fullChainE2e = "NOT_RUN" as const;
const componentLoadLab = allStartedProcessesSurvived
  && plannerBenchmark.status === "pass"
  && projector.status === "fleet_health_snapshot_refreshed"
  && projectorJsonLines.length === 1
  && contenderJsonLines.length === 1
  && contender.reason === "another_projector_holds_live_lease"
  && plannerCoalescing.notificationCount === 1
  && plannerCoalescing.generation === 2
  && JSON.stringify(plannerCoalescing.reasons) === JSON.stringify(["first", "second"])
  && plannerCoalescing.maximumObservedSlot === 11
  && plannerCoalescing.availabilityMerged === true
  && deadlocks === 0
  && hotTempBytes === 0
  && workloadErrors === 0
  && roleProbePasses.revalidator === true
  && roleProbePasses.executor === true
  ? "PASS"
  : "FAIL";

const findings: Array<{ severity: string; code: string; detail: string }> = [];
if (!allStartedProcessesSurvived) findings.push({
  severity: "critical", code: "worker_process_exit",
  detail: "At least one started fleet process exited before controlled shutdown.",
});
if (deadlocks > 0) findings.push({
  severity: "critical", code: "postgres_deadlocks",
  detail: `PostgreSQL recorded ${deadlocks} deadlocks during the run.`,
});
if (hotTempBytes > 0) findings.push({
  severity: "warning", code: "hot_path_temp_spill",
  detail: `Cached hot-path workloads spilled ${hotTempBytes.toLocaleString()} PostgreSQL temp bytes.`,
});
if (concurrentTempBytes > 0) findings.push({
  severity: "warning", code: "concurrent_workload_temp_spill",
  detail: `Non-health concurrent workloads spilled ${concurrentTempBytes.toLocaleString()} PostgreSQL temp bytes.`,
});
if (healthP95 !== null && healthP95 > 1_000) findings.push({
  severity: "critical", code: "health_query_p95_over_1s",
  detail: `Loaded fleet health p95 was ${healthP95.toFixed(2)} ms.`,
});
else if (healthP95 !== null && healthP95 > 250) findings.push({
  severity: "warning", code: "health_query_p95_over_250ms",
  detail: `Loaded fleet health p95 was ${healthP95.toFixed(2)} ms.`,
});
if (rpcErrors > 0) findings.push({
  severity: "warning", code: "rpc_injected_errors_observed",
  detail: `${rpcErrors} configured RPC failures were observed; inspect worker recovery logs.`,
});
if (Number(rpcLoad.errors ?? 0) > 0) findings.push({
  severity: "warning", code: "rpc_load_errors",
  detail: `${rpcLoad.errors} of ${rpcLoad.requests} synthetic RPC load requests returned transport or JSON-RPC errors.`,
});
if (workloadErrors > 0) findings.push({
  severity: "warning", code: "workload_stderr",
  detail: `${workloadErrors} pgbench roles wrote stderr output.`,
});
if (workerErrorLines > 0) findings.push({
  severity: "warning", code: "worker_error_logs",
  detail: `${workerErrorLines} real-worker log lines matched error/fatal/panic; inspect per-role tails.`,
});
if (localCatalogMissing) findings.push({
  severity: "warning", code: "local_catalog_fixture_missing",
  detail: "The reconciler stayed alive, but its position sweep failed closed because the local shared-market catalog is intentionally empty; chain-position sweep load is not covered yet.",
});
const amplifiedWorkerLog = workerLogSizes
  .filter((entry) => entry.lines > 10_000 || entry.bytes > 10 * 1024 * 1024)
  .sort((left, right) => right.bytes - left.bytes)[0];
if (amplifiedWorkerLog) findings.push({
  severity: "warning", code: "worker_log_amplification",
  detail: `${amplifiedWorkerLog.role} emitted ${amplifiedWorkerLog.lines.toLocaleString()} lines (${bytes(amplifiedWorkerLog.bytes)}) during the run.`,
});
if (workerOutboxRowsAdded > Number(config.opportunities) * 10) findings.push({
  severity: "warning", code: "outbox_write_amplification",
  detail: `${workerOutboxRowsAdded.toLocaleString()} non-synthetic outbox rows were added, more than 10x the seeded opportunity count.`,
});
if (!findings.length) findings.push({
  severity: "info", code: "no_local_threshold_breach",
  detail: "No configured local threshold was breached in this run.",
});

const evidence = {
  generatedAt: new Date().toISOString(),
  isolation: {
    externalDatabaseConnections: false,
    externalBlockchainConnections: false,
    productionKeysLoaded: false,
    databaseGuard: "127.0.0.1 and fleet_e2e_* database name",
    rpcGuard: "127.0.0.1 HTTP JSON-RPC emulator",
    transactionBroadcastsEnabled: false,
  },
  config,
  raw: {
    rpcTotalRequests: Number(rpc.requests ?? 0),
    rpcSyntheticDriverRequests: syntheticRpcRequests,
    outboxTotalRowsAdded: outboxRowsAdded,
    localUserOutboxRowsAdded: syntheticOutboxRowsAdded,
  },
  checks: {
    allStartedProcessesSurvived,
    realWorkerProgress: {
      claimed: realWorkerClaimed,
      completed: realWorkerCompleted,
      passes: realWorkerProgressPasses,
      livenessIsNotProgress: true,
    },
    roleProbePasses,
    plannerBenchmarkPassed: plannerBenchmark.status === "pass",
    rpcInterceptorWasUsed: Number(rpc.requests ?? 0) > 0,
    databaseDeadlocks: deadlocks,
    workloadErrors,
    workerErrorLines,
    outboxRowsAdded,
    outboxRowsPerSecond,
    workerLogSizes,
    hotTempBytes,
    healthProjector: {
      status: projector.status,
      rowCount: Number(projector.rowCount ?? 0),
      refreshDurationMilliseconds: Number(projector.refreshDurationMilliseconds ?? 0),
      sourceWatermark: projector.sourceWatermark,
      compactJsonLines: projectorJsonLines.length,
      physicalLogLines: projectorLines.length,
      contenderStatus: contender.status,
      contenderReason: contender.reason,
      contenderCompactJsonLines: contenderJsonLines.length,
      contenderPhysicalLogLines: contenderLines.length,
    },
    plannerCoalescing,
    healthHotPath,
    concurrentTempBytes,
  },
  plannerBenchmark,
  sourceHealth,
  workloads,
  processes: { status: processStatusRows, metrics: processMetrics, logs: workerLogs },
  rpc,
  rpcLoad,
  database: { before: databaseBefore, after: databaseAfter },
  attribution: {
    realWorker: {
      rpcRequests: realWorkerRpcRequests,
      claimed: realWorkerClaimed,
      completed: realWorkerCompleted,
      outboxRows: workerOutboxRowsAdded,
    },
    syntheticSql: {
      outboxRows: syntheticOutboxRowsAdded,
      transactions: Object.fromEntries(
        roleNames
          .filter((role) => role !== "baseline-health")
          .map((role) => [role, workloads[role]!.transactions]),
      ),
    },
    syntheticRpc: {
      requests: syntheticRpcRequests,
      errors: Number(rpcLoad.errors ?? 0),
    },
  },
  verdicts: {
    componentLoadLab,
    fullChainE2e,
  },
  findings,
};
const causalFailures = causalEvidenceFailures(evidence);
if (causalFailures.length) {
  throw new Error(`causal evidence invalid: ${causalFailures.join("; ")}`);
}
await writeFile(join(runDirectory, "evidence.json"), `${JSON.stringify(evidence, null, 2)}\n`);

const markdown = `# Local fleet component load lab evidence

Generated: ${evidence.generatedAt} (UTC)

## Verdict

- Started process liveness: **${allStartedProcessesSurvived ? "PASS" : "FAIL"}**
- Planner algorithm benchmark: **${plannerBenchmark.status === "pass" ? "PASS" : "FAIL"}**
- Real-worker progress: **${realWorkerProgressPasses ? "PASS" : "NOT PROVEN"}**
- OVERALL COMPONENT LAB: **${componentLoadLab}**
- FULL_CHAIN_E2E: **${fullChainE2e}**
- PostgreSQL deadlocks: **${deadlocks}**

## Load profile

| Opportunities | Duration | Exact health clients | DB size before | DB size after |
| ---: | ---: | ---: | ---: | ---: |
| ${config.opportunities.toLocaleString()} | ${config.durationSeconds}s | ${config.healthClients} | ${bytes(databaseBefore.databaseBytes)} | ${bytes(databaseAfter.databaseBytes)} |

Synthetic SQL outbox growth: **${syntheticOutboxRowsAdded.toLocaleString()} rows**. Non-synthetic outbox growth: **${workerOutboxRowsAdded.toLocaleString()} rows**. Synthetic growth is not fleet amplification.

| Workload | TPS | p50 | p95 | p99 | max | Transactions |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
${roleNames.map((role) => `| synthetic-sql:${role} | ${number(workloads[role]!.tps)} | ${number(workloads[role]!.p50Ms)} ms | ${number(workloads[role]!.p95Ms)} ms | ${number(workloads[role]!.p99Ms)} ms | ${number(workloads[role]!.maxMs)} ms | ${workloads[role]!.transactions.toLocaleString()} |`).join("\n")}

Direct history-source control p95: **${number(sourceHealth.p95Ms)} ms**. Fenced
snapshot refresh duration: **${number(Number(projector.refreshDurationMilliseconds))} ms**.
These source costs are reported separately from cached health latency. Cached
health-only PostgreSQL temp spill: **${hotTempBytes.toLocaleString()} bytes**.
Other concurrent workloads spilled **${concurrentTempBytes.toLocaleString()} bytes**;
that value is not attributed to cached health.

## Real worker process envelope

| Role | Runtime evidence | RSS p95 | RSS max | CPU avg | CPU max |
| --- | --- | ---: | ---: | ---: | ---: |
${workerRoles.map((role) => {
  const status = processStatusRows.find((row) => row.role === role);
  const metric: any = processMetrics[role];
  const runtimeEvidence = status
    ? (status.aliveBeforeShutdown ? "alive before shutdown" : "exited early")
    : "signer-free role probe";
  return `| ${role} | ${runtimeEvidence} | ${number(metric.rssP95MiB)} MiB | ${number(metric.rssMaxMiB)} MiB | ${number(metric.cpuAveragePercent)}% | ${number(metric.cpuMaxPercent)}% |`;
}).join("\n")}

## RPC interceptor

Configured latency was ${rpc.configuredLatencyMs} ms plus up to ${rpc.configuredJitterMs} ms deterministic jitter. The emulator handled **${rpc.requests}** requests: **${syntheticRpcRequests} synthetic** and **${realWorkerRpcRequests} attributable to real processes**, with maximum concurrency **${rpc.maxInflight}**.

The sustained driver used **${rpcLoad.concurrency} clients**, achieved **${number(rpcLoad.requestsPerSecond)} requests/s**, and measured p95 **${number(rpcLoad.p95Ms)} ms** with **${rpcLoad.errors} errors**.

| Method | Calls | Errors | p95 | max inflight |
| --- | ---: | ---: | ---: | ---: |
${Object.entries(rpc.methods ?? {}).map(([method, value]: [string, any]) => `| ${method} | ${value.calls} | ${value.errors} | ${number(value.p95Ms)} ms | ${value.maxInflight} |`).join("\n")}

## Findings

${findings.map((finding) => `- **${finding.severity.toUpperCase()} ${finding.code}:** ${finding.detail}`).join("\n")}

## Interpretation boundary

This is a component load lab. Synthetic SQL and RPC drive contention while signer-free role probes and selected real processes validate entrypoints and polling. It is not chain E2E and does not prove successful worker execution, Kamino decoding, validator execution, transaction landing, Neon autoscaling, or provider capacity.
`;
await writeFile(join(runDirectory, "evidence.md"), markdown);
console.log(join(runDirectory, "evidence.md"));
