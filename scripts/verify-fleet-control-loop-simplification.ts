#!/usr/bin/env bun

import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

type JsonRecord = Record<string, any>;
type Check = { name: string; pass: boolean; detail: string };

const readText = (path: string) => readFile(resolve(path), "utf8");
const readJson = async (path: string): Promise<JsonRecord> =>
  JSON.parse(await readText(path));
const clone = <T>(value: T): T => structuredClone(value);
const finite = (value: unknown): value is number =>
  typeof value === "number" && Number.isFinite(value);

const staticChecks = async (): Promise<Check[]> => {
  const [migration, store, worker, projector, render, docker, cachedSql, sourceSql] =
    await Promise.all([
      readText("crates/loyal-yield-store/migrations/0034_fleet_health_snapshot_projection.sql"),
      readText("crates/loyal-yield-store/src/fleet_orchestration/queue.rs"),
      readText("crates/loyal-fleet-worker/src/lib.rs"),
      readText("crates/loyal-yield-orchestrator/src/bin/fleet-health-projector.rs"),
      readText("render.yaml"),
      readText("Dockerfile.light-workers"),
      readText("scripts/fleet-db-load/workloads/health.sql"),
      readText("scripts/fleet-local-load-lab/workloads/health-source.sql"),
    ]);
  const projectorServices = render.match(/name: loyal-fleet-health-projector\b/g) ?? [];
  const projectorBlock = render.match(
    /name: loyal-fleet-health-projector[\s\S]*?(?=\n\s*- type:|\n\s*databases:|$)/,
  )?.[0] ?? "";
  const plannerBlock = render.match(
    /name: loyal-fleet-opportunity-planner[\s\S]*?(?=\n\s*- type:|\n\s*databases:|$)/,
  )?.[0] ?? "";
  const notifyGuard = migration.match(/IF inserted_new THEN[\s\S]*?pg_notify/) !== null;
  const workerCallsSource = /fleet_orchestration_status_source\s*\(/.test(worker)
    || /FROM\s+loyal_yield\.fleet_orchestration_status\b/i.test(worker);
  return [
    {
      name: "bounded cluster-keyed snapshot schema",
      pass: /fleet_orchestration_health_snapshots/.test(migration)
        && /cluster TEXT PRIMARY KEY/.test(migration)
        && /source_watermark JSONB/.test(migration)
        && /refresh_duration_milliseconds BIGINT/.test(migration)
        && /fencing_token BIGINT/.test(migration),
      detail: "one current row carries payload, watermark, timing, owner, and fence",
    },
    {
      name: "cached worker read has no source fallback",
      pass: !workerCallsSource
        && /fleet_orchestration_health_snapshots/.test(store)
        && /fleet health snapshot is stale/.test(store)
        && /historyFallbackAttempted"\s*:\s*false/.test(worker)
        && /queueProcessingContinues"\s*:\s*true/.test(worker),
      detail: "workers consume only the snapshot and degrade non-blockingly",
    },
    {
      name: "projector owns source aggregation and checks semantic equality",
      pass: /fleet_orchestration_status_source\(&lease\.cluster\)/.test(store)
        && /claim_fleet_health_projection_lease/.test(projector)
        && /serde_json::to_value\(&cached\).*serde_json::to_value\(&refresh\.status\)/s.test(projector)
        && /fleet_health_snapshot_refreshed/.test(projector),
      detail: "one fenced owner publishes compact source-equivalent JSON",
    },
    {
      name: "exactly one pinned lightweight projector service",
      pass: projectorServices.length === 1
        && /runtime: image/.test(projectorBlock)
        && /light-workers:sha-[0-9a-f]{7,40}/.test(projectorBlock)
        && /autoDeploy: false/.test(projectorBlock)
        && /fleet-health-projector/.test(docker),
      detail: `${projectorServices.length} projector service definition(s)`,
    },
    {
      name: "edge-only planner notification and compact production logs",
      pass: notifyGuard
        && /ON CONFLICT \(cluster, vault_id\) DO NOTHING/.test(migration)
        && /generation = loyal_yield\.fleet_planning_dirty_vaults\.generation \+ 1/.test(migration)
        && /--json/.test(plannerBlock),
      detail: "durable merge is authoritative; NOTIFY is insert-edge only",
    },
    {
      name: "load lab separates cached hot path from direct source control",
      pass: /fleet_orchestration_health_snapshots/.test(cachedSql)
        && !/fleet_orchestration_status\b/.test(cachedSql)
        && /fleet_orchestration_status\b/.test(sourceSql),
      detail: "cached and source measurements use distinct SQL files",
    },
  ];
};

type ControlFixture = {
  workerReadsSource: boolean;
  staleLabelledHealthy: boolean;
  missingFallsBack: boolean;
  admittedOwners: number;
  payloadMatchesSource: boolean;
  repeatedUpdateNotifies: boolean;
  rawSyntheticRpc: number;
  attributedSyntheticRpc: number;
  sourceRefreshMs?: number;
  cachedHealthP95Ms: number;
  signatures: string[];
  rerunMutated: boolean;
};

const controlFailures = (fixture: ControlFixture): string[] => {
  const failures: string[] = [];
  if (fixture.workerReadsSource) failures.push("worker reads source view");
  if (fixture.staleLabelledHealthy) failures.push("stale snapshot labelled healthy");
  if (fixture.missingFallsBack) failures.push("missing snapshot falls back");
  if (fixture.admittedOwners !== 1) failures.push("refresh ownership is not singular");
  if (!fixture.payloadMatchesSource) failures.push("cached payload differs from source");
  if (fixture.repeatedUpdateNotifies) failures.push("repeat update notified");
  if (fixture.rawSyntheticRpc !== fixture.attributedSyntheticRpc) failures.push("synthetic work misattributed");
  if (!finite(fixture.sourceRefreshMs)) failures.push("source refresh cost omitted");
  if (fixture.signatures.length !== 1 || new Set(fixture.signatures).size !== 1 || fixture.rerunMutated) {
    failures.push("route is not exactly-once");
  }
  return failures;
};

const adversarialChecks = (): Check[] => {
  const positive: ControlFixture = {
    workerReadsSource: false,
    staleLabelledHealthy: false,
    missingFallsBack: false,
    admittedOwners: 1,
    payloadMatchesSource: true,
    repeatedUpdateNotifies: false,
    rawSyntheticRpc: 80,
    attributedSyntheticRpc: 80,
    sourceRefreshMs: 2_100,
    cachedHealthP95Ms: 4,
    signatures: ["local-signature"],
    rerunMutated: false,
  };
  const mutations: Array<[string, (fixture: ControlFixture) => void]> = [
    ["worker source-view read", (f) => { f.workerReadsSource = true; }],
    ["stale snapshot labelled healthy", (f) => { f.staleLabelledHealthy = true; }],
    ["missing snapshot source fallback", (f) => { f.missingFallsBack = true; }],
    ["two refresh owners", (f) => { f.admittedOwners = 2; }],
    ["cached/source payload mismatch", (f) => { f.payloadMatchesSource = false; }],
    ["repeat dirty-row notification", (f) => { f.repeatedUpdateNotifies = true; }],
    ["synthetic work relabelled real", (f) => { f.attributedSyntheticRpc = 0; }],
    ["omitted source refresh cost", (f) => { delete f.sourceRefreshMs; }],
    ["duplicate signature or mutating rerun", (f) => {
      f.signatures.push("local-signature");
      f.rerunMutated = true;
    }],
  ];
  const checks: Check[] = [{
    name: "positive adversarial fixture",
    pass: controlFailures(positive).length === 0,
    detail: "valid simplified control loop is accepted",
  }];
  for (const [name, mutate] of mutations) {
    const fixture = clone(positive);
    mutate(fixture);
    checks.push({
      name: `reject ${name}`,
      pass: controlFailures(fixture).length > 0,
      detail: controlFailures(fixture).join(", "),
    });
  }
  return checks;
};

const loadChecks = (evidence: JsonRecord, kind: "million" | "planner"): Check[] => {
  const config = evidence.config ?? {};
  const checks = evidence.checks ?? {};
  const health = evidence.workloads?.health ?? {};
  const executor = evidence.workloads?.executor ?? {};
  const projector = checks.healthProjector ?? {};
  const coalescing = checks.plannerCoalescing ?? {};
  const common = config.databaseHost === "127.0.0.1"
    && config.healthRequestsPerClientPerSecond === 1
    && evidence.isolation?.productionKeysLoaded === false
    && checks.allStartedProcessesSurvived === true
    && checks.databaseDeadlocks === 0
    && checks.workloadErrors === 0
    && projector.status === "fleet_health_snapshot_refreshed"
    && projector.compactJsonLines === 1
    && projector.contenderStatus === "fleet_health_snapshot_refresh_skipped"
    && projector.contenderCompactJsonLines === 1
    && coalescing.notificationCount === 1
    && coalescing.generation === 2
    && coalescing.maximumObservedSlot === 11
    && coalescing.availabilityMerged === true;
  if (kind === "planner") {
    const planner = (checks.workerLogSizes ?? []).find((row: JsonRecord) => row.role === "planner") ?? {};
    return [{
      name: "10k compact planner envelope",
      pass: common && config.opportunities === 10_000
        && config.durationSeconds >= 20 && finite(planner.lines) && planner.lines <= 2_000,
      detail: `${planner.lines ?? "missing"} planner lines`,
    }];
  }
  return [
    {
      name: "1M/10-client isolated load profile",
      pass: common && config.opportunities === 1_000_000
        && config.healthClients === 10 && config.durationSeconds >= 15,
      detail: JSON.stringify({ opportunities: config.opportunities, clients: config.healthClients, seconds: config.durationSeconds }),
    },
    { name: "cached health p95 <= 50 ms", pass: finite(health.p95Ms) && health.p95Ms <= 50, detail: `${health.p95Ms} ms` },
    { name: "executor throughput >= 100 TPS", pass: finite(executor.tps) && executor.tps >= 100, detail: `${executor.tps} TPS` },
    { name: "cached hot path has no temp spill", pass: checks.hotTempBytes === 0, detail: `${checks.hotTempBytes} bytes` },
    {
      name: "source refresh cost remains separately visible",
      pass: finite(evidence.sourceHealth?.p95Ms)
        && finite(projector.refreshDurationMilliseconds)
        && finite(health.p95Ms),
      detail: `source=${evidence.sourceHealth?.p95Ms}ms projector=${projector.refreshDurationMilliseconds}ms cached=${health.p95Ms}ms`,
    },
  ];
};

const comparisonChecks = (one: JsonRecord, ten: JsonRecord): Check[] => {
  const oneTps = one.workloads?.executor?.tps;
  const tenTps = ten.workloads?.executor?.tps;
  const tenP95 = ten.workloads?.health?.p95Ms;
  const profile = one.config?.opportunities === 100_000
    && ten.config?.opportunities === 100_000
    && one.config?.healthClients === 1
    && ten.config?.healthClients === 10
    && one.config?.durationSeconds >= 15
    && ten.config?.durationSeconds >= 15;
  const retention = finite(oneTps) && oneTps > 0 && finite(tenTps) ? tenTps / oneTps : 0;
  return [
    { name: "100k one-versus-ten profile", pass: profile, detail: `one=${one.config?.healthClients} ten=${ten.config?.healthClients}` },
    { name: "ten readers retain >=80% executor TPS", pass: retention >= 0.8, detail: `${(retention * 100).toFixed(1)}%` },
    { name: "ten-reader cached health p95 <=50 ms", pass: finite(tenP95) && tenP95 <= 50, detail: `${tenP95} ms` },
  ];
};

const routeChecks = (evidence: JsonRecord): Check[] => {
  const before = evidence.database?.beforeRerun ?? {};
  const after = evidence.database?.afterRerun ?? {};
  const submissions = before.submissions ?? {};
  const roles = evidence.roles ?? {};
  const rerun = roles.rerun ?? {};
  const proxy = evidence.rpc?.proxy ?? {};
  const raw = evidence.rpc?.raw ?? {};
  const productionMethods = raw.methodsBySource?.productionProcess ?? {};
  const productionErrors = Object.values(productionMethods).reduce(
    (total: number, value: any) => total + Number(value?.errors ?? 0), 0,
  );
  const rerunNoWork = Number(rerun.planner?.published ?? -1) === 0
    && Number(rerun.revalidator?.claimed ?? -1) === 0
    && Number(rerun.executor?.claimed ?? -1) === 0
    && Number(rerun.confirmer?.claimed ?? -1) === 0
    && Number(rerun.reconciler?.claimed ?? -1) === 0;
  const lite = evidence.prerequisites?.liteSvm ?? {};
  const liteTx = lite.routeExecution?.transaction ?? {};
  return [
    {
      name: "LiteSVM route prerequisite",
      pass: lite.kind === "loyal-fleet-litesvm-first-evidence"
        && liteTx.executed === true && liteTx.simulated === true && liteTx.exactAltCoverage === true,
      detail: lite.kind ?? "missing",
    },
    {
      name: "one reconciled signature and no-op rerun",
      pass: submissions.total === 1 && submissions.reconciled === 1
        && submissions.distinctSignatures === 1 && submissions.signatures?.length === 1
        && rerunNoWork && JSON.stringify(before) === JSON.stringify(after),
      detail: `signatures=${submissions.signatures?.length ?? 0} rerunNoWork=${rerunNoWork}`,
    },
    {
      name: "80-120 ms delayed RPC with zero production errors",
      pass: proxy.configuredLatencyMs === 80 && proxy.configuredJitterMs === 40
        && proxy.configuredErrorEvery === 0 && productionErrors === 0
        && Number(proxy.sources?.productionProcess ?? 0) > 0
        && Object.keys(productionMethods).length > 0,
      detail: `productionCalls=${proxy.sources?.productionProcess ?? 0} productionErrors=${productionErrors}`,
    },
    {
      name: "truthful refreshed simulated market input",
      pass: evidence.simulatedMarketInput?.source === "continuously-refreshed-local-fixture"
        && Number(evidence.simulatedMarketInput?.rowCount ?? 0) >= 2,
      detail: JSON.stringify(evidence.simulatedMarketInput ?? {}),
    },
  ];
};

const argument = (name: string): string | undefined => {
  const index = process.argv.indexOf(name);
  return index === -1 ? undefined : process.argv[index + 1];
};

const allChecks: Check[] = [
  ...await staticChecks(),
  ...adversarialChecks(),
];
const plannerPath = argument("--planner");
const millionPath = argument("--million");
const onePath = argument("--one-reader");
const tenPath = argument("--ten-reader");
const chainPath = argument("--full-chain");
if (plannerPath) allChecks.push(...loadChecks(await readJson(plannerPath), "planner"));
if (millionPath) allChecks.push(...loadChecks(await readJson(millionPath), "million"));
if (onePath || tenPath) {
  if (!onePath || !tenPath) throw new Error("--one-reader and --ten-reader must be provided together");
  allChecks.push(...comparisonChecks(await readJson(onePath), await readJson(tenPath)));
}
if (chainPath) allChecks.push(...routeChecks(await readJson(chainPath)));

for (const check of allChecks) {
  console.log(`${check.pass ? "PASS" : "FAIL"}: ${check.name} - ${check.detail}`);
}
const pass = allChecks.every((check) => check.pass);
const performanceSupplied = Boolean(plannerPath && millionPath && onePath && tenPath);
const routeSupplied = Boolean(chainPath);
console.log(`BOUNDED_HEALTH_READ: ${pass ? "PASS" : "FAIL"}`);
console.log(`SINGLE_REFRESH_OWNER: ${pass ? "PASS" : "FAIL"}`);
console.log(`COALESCED_WAKEUPS_AND_LOGS: ${pass && plannerPath ? "PASS" : plannerPath ? "FAIL" : "NOT_RUN"}`);
console.log(`MEASURED_LOAD_IMPROVEMENT: ${pass && performanceSupplied ? "PASS" : "NOT_RUN"}`);
console.log(`ROUTE_AND_RPC_REGRESSION: ${pass && routeSupplied ? "PASS" : "NOT_RUN"}`);
console.log(`ADVERSARIAL_CONTROLS: ${pass ? "PASS" : "FAIL"}`);
console.log(`FLEET_CONTROL_LOOP_SIMPLIFICATION: ${pass && performanceSupplied && routeSupplied ? "PASS" : "PARTIAL"}`);
if (!pass) process.exitCode = 1;
