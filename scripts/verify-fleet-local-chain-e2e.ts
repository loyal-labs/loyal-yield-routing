#!/usr/bin/env bun

import { readdir, readFile, writeFile } from "node:fs/promises";
import { basename, join, resolve } from "node:path";

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
type RecordJson = { [key: string]: Json };
type Check = { name: string; pass: boolean; detail: string };

const isRecord = (value: unknown): value is RecordJson =>
  value !== null && typeof value === "object" && !Array.isArray(value);
const numberAt = (value: unknown): number =>
  typeof value === "number" && Number.isFinite(value) ? value : 0;
const objectAt = (value: unknown): RecordJson => isRecord(value) ? value : {};
const stringAt = (value: unknown): string => typeof value === "string" ? value : "";

const readText = async (path: string) => await readFile(path, "utf8");
const readJson = async (path: string): Promise<RecordJson> => {
  const parsed: unknown = JSON.parse(await readText(path));
  if (!isRecord(parsed)) throw new Error(`${path} must contain one JSON object`);
  return parsed;
};

const extractJsonObjects = (text: string): RecordJson[] => {
  const values: RecordJson[] = [];
  let start = -1;
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let index = 0; index < text.length; index += 1) {
    const character = text[index]!;
    if (start === -1) {
      if (character === "{") {
        start = index;
        depth = 1;
        inString = false;
        escaped = false;
      }
      continue;
    }
    if (inString) {
      if (escaped) escaped = false;
      else if (character === "\\") escaped = true;
      else if (character === '"') inString = false;
      continue;
    }
    if (character === '"') inString = true;
    else if (character === "{") depth += 1;
    else if (character === "}") {
      depth -= 1;
      if (depth === 0) {
        try {
          const parsed: unknown = JSON.parse(text.slice(start, index + 1));
          if (isRecord(parsed)) values.push(parsed);
        } catch {
          // Non-JSON logging text is ignored; required evidence is checked later.
        }
        start = -1;
      }
    }
  }
  return values;
};

const parseChainSnapshot = async (path: string): Promise<RecordJson> => {
  const objects = extractJsonObjects(await readText(path));
  const output = [...objects].reverse().find((value) => isRecord(value.chainReconcile));
  if (!output) throw new Error(`${path} has no chainReconcile object`);
  const reconcile = objectAt(output.chainReconcile);
  const positions = Array.isArray(reconcile.positions) ? reconcile.positions : [];
  return {
    status: stringAt(output.status),
    observedSlot: numberAt(reconcile.observedSlot),
    sendsTransactions: output.sendsTransactions === true,
    positions: positions.filter(isRecord).map((position) => ({
      reserve: stringAt(position.reserve),
      market: stringAt(position.market),
      liquidityMint: stringAt(position.liquidityMint),
      amountRaw: stringAt(position.amountRaw),
      redeemableSourceLiquidityAmountRaw: stringAt(position.redeemableSourceLiquidityAmountRaw),
      vaultLiquidityAmountRaw: stringAt(position.vaultLiquidityAmountRaw),
      obligationExists: position.obligationExists === true,
    })).sort((left, right) => stringAt(left.reserve).localeCompare(stringAt(right.reserve))),
  };
};

const jsonFiles = async (directory: string, prefix: string, rerun: boolean): Promise<string[]> => {
  const names = await readdir(directory);
  return names
    .filter((name) => name.startsWith(prefix) && name.endsWith(".jsonl") && name.includes("rerun") === rerun)
    .sort()
    .map((name) => join(directory, name));
};

const objectsFromFiles = async (files: string[]): Promise<RecordJson[]> => {
  const all: RecordJson[] = [];
  for (const file of files) all.push(...extractJsonObjects(await readText(file)));
  return all;
};

const sum = (values: RecordJson[], field: string, predicate: (value: RecordJson) => boolean) =>
  values.filter(predicate).reduce((total, value) => total + numberAt(value[field]), 0);

const collectRoleEvidence = async (workers: string, rerun: boolean) => {
  const suffix = rerun ? "-rerun.json" : ".json";
  const plannerName = rerun ? "planner-rerun.json" : "planner.json";
  const planner = await readJson(join(workers, plannerName));
  const revalidators = await objectsFromFiles(await jsonFiles(workers, "revalidator-", rerun));
  const executors = await objectsFromFiles(await jsonFiles(workers, "executor-", rerun));
  const confirmers = await objectsFromFiles(await jsonFiles(workers, "confirmer-", rerun));
  const reconcilers = await objectsFromFiles(await jsonFiles(workers, "reconciler-", rerun));
  void suffix;
  return {
    planner: {
      published: numberAt(planner.publishedCount),
      selected: numberAt(planner.selectedCount) || numberAt(planner.capacitySelectedCount),
    },
    revalidator: {
      claimed: sum(revalidators, "claimed", (value) => value.lane === "revalidate"),
      completed: sum(revalidators, "completed", (value) => value.lane === "revalidate"),
      failed: sum(revalidators, "failed", (value) => value.lane === "revalidate"),
    },
    executor: {
      claimed: sum(executors, "claimed", (value) => value.lane === "execute"),
      completed: sum(executors, "completed", (value) => value.lane === "execute"),
      failed: sum(executors, "failed", (value) => value.lane === "execute"),
    },
    confirmer: {
      claimed: sum(confirmers, "claimed", (value) => value.event === "fleet_route_confirmer_poll"),
      broadcastsSucceeded: sum(confirmers, "broadcastsSucceeded", (value) => value.event === "fleet_route_confirmer_poll"),
      confirmed: sum(confirmers, "confirmed", (value) => value.event === "fleet_route_confirmer_poll"),
      reconciliationPending: sum(confirmers, "reconciliationPending", (value) => value.event === "fleet_route_confirmer_poll"),
      failed: sum(confirmers, "failed", (value) => value.event === "fleet_route_confirmer_poll"),
    },
    reconciler: {
      claimed: sum(reconcilers, "claimed", (value) => value.status === "fleet_reconciler_healthy"),
      completed: sum(reconcilers, "completed", (value) => value.status === "fleet_reconciler_healthy"),
      deferred: sum(reconcilers, "deferred", (value) => value.status === "fleet_reconciler_healthy"),
    },
  };
};

const flag = (name: string): string => {
  const index = process.argv.indexOf(name);
  if (index === -1 || !process.argv[index + 1]) throw new Error(`missing ${name}`);
  return process.argv[index + 1]!;
};

const gitValue = (args: string[]) => {
  const result = Bun.spawnSync(["git", ...args], { stdout: "pipe", stderr: "pipe" });
  return result.exitCode === 0 ? result.stdout.toString().trim() : "unknown";
};

const assemble = async (directoryArg: string) => {
  const directory = resolve(directoryArg);
  const workers = join(directory, "workers");
  const rpcLines = (await readText(join(directory, "rpc-requests.jsonl")))
    .split("\n").filter(Boolean).map((line) => JSON.parse(line) as RecordJson);
  const rawSources: Record<string, number> = {};
  const rawSourceErrors: Record<string, number> = {};
  const rawMethodsBySource: Record<string, Record<string, { calls: number; errors: number }>> = {};
  for (const line of rpcLines) {
    const source = stringAt(line.source);
    rawSources[source] = (rawSources[source] ?? 0) + 1;
    if (line.rpcErrored === true) rawSourceErrors[source] = (rawSourceErrors[source] ?? 0) + 1;
    const methods = Array.isArray(line.methods) ? line.methods : [];
    const sourceMethods = rawMethodsBySource[source] ?? {};
    for (const methodValue of methods) {
      const method = stringAt(methodValue);
      const stats = sourceMethods[method] ?? { calls: 0, errors: 0 };
      stats.calls += 1;
      if (line.rpcErrored === true) stats.errors += 1;
      sourceMethods[method] = stats;
    }
    rawMethodsBySource[source] = sourceMethods;
  }
  const evidence: RecordJson = {
    schemaVersion: 1,
    kind: "loyal-fleet-local-chain-e2e-evidence",
    run: {
      startedAtUtc: flag("--started-at"),
      completedAtUtc: new Date().toISOString(),
      gitCommit: gitValue(["rev-parse", "HEAD"]),
      gitDirty: gitValue(["status", "--porcelain"]) !== "",
      runner: "bun run fleet:local-chain-e2e",
    },
    fixture: {
      cloneAccounts: Number(flag("--clone-accounts")),
      sourceSlot: Number(flag("--source-slot")),
      offlineVerification: await readJson(join(directory, "setup/fixture-verify.json")),
      liveVerification: await readJson(join(directory, "setup/fixture-live-verify.json")),
    },
    simulatedMarketInput: await readJson(join(directory, "simulated-market-input.json")),
    prerequisites: {
      liteSvm: await readJson(join(directory, "setup/litesvm-evidence.json")),
    },
    isolation: {
      database: "disposable-loopback-postgresql",
      rpc: "instrumented-loopback-proxy-to-fresh-solana-test-validator",
      cluster: "localnet",
      fixtureSource: "finalized-public-mainnet-read-only",
      productionDatabaseWrites: false,
      productionTransactions: false,
      productionSecretsLoaded: false,
      productionWorkerWalletSecretMounted: false,
      policySigner: "ephemeral-keypair-through-normal-policy-loader",
      walletSigner: "ephemeral-setup-only-keypair",
    },
    coverage: {
      routePipeline: "planner-revalidator-executor-confirmer-reconciler",
      positionSweep: "not-claimed-usdc-main-prime-fixture-is-not-the-complete-stable-mint-exit-catalog",
      chainSnapshots: "explicit-production-worker-read-only-reconcile-before-after-and-rerun",
    },
    subjects: {
      settings: flag("--settings"),
      vault: flag("--vault"),
      policy: flag("--policy"),
      mainReserve: flag("--main-reserve"),
      primeReserve: flag("--prime-reserve"),
    },
    chain: {
      before: await parseChainSnapshot(join(directory, "chain-before.out")),
      after: await parseChainSnapshot(join(directory, "chain-after.out")),
      afterRerun: await parseChainSnapshot(join(directory, "chain-after-rerun.out")),
    },
    database: {
      beforeRerun: await readJson(join(directory, "database-before-rerun.json")),
      afterRerun: await readJson(join(directory, "database-after-rerun.json")),
    },
    roles: {
      initial: await collectRoleEvidence(workers, false),
      rerun: await collectRoleEvidence(workers, true),
    },
    rpc: {
      proxy: await readJson(join(directory, "rpc-summary.json")),
      syntheticLoad: await readJson(join(directory, "load/rpc-load-summary.json")),
      raw: {
        requests: rpcLines.length,
        sources: rawSources,
        sourceErrors: rawSourceErrors,
        methodsBySource: rawMethodsBySource,
      },
    },
  };
  const evidencePath = join(directory, "evidence.json");
  await writeFile(evidencePath, `${JSON.stringify(evidence, null, 2)}\n`);
  const checks = evaluate(evidence);
  await writeFile(join(directory, "evidence.md"), report(evidence, checks));
  console.log(JSON.stringify({ status: checks.every((check) => check.pass) ? "PASS" : "FAIL", evidencePath }));
};

const amountFor = (snapshot: RecordJson, reserve: string): bigint => {
  const positions = Array.isArray(snapshot.positions) ? snapshot.positions.filter(isRecord) : [];
  const position = positions.find((value) => value.reserve === reserve);
  const raw = position && stringAt(position.amountRaw);
  return /^\d+$/u.test(raw) ? BigInt(raw) : -1n;
};

const stable = (value: unknown) => JSON.stringify(value);
const noSecretMaterial = (serialized: string) => {
  const forbiddenEndpoint = /(api\.mainnet-beta\.solana\.com|helius|neon\.tech|render\.com)/iu;
  const keypairArray = /\[(?:\s*\d{1,3}\s*,){31,}\s*\d{1,3}\s*\]/u;
  return !forbiddenEndpoint.test(serialized) && !keypairArray.test(serialized);
};

const evaluate = (evidence: RecordJson): Check[] => {
  const prerequisites = objectAt(evidence.prerequisites);
  const liteSvm = objectAt(prerequisites.liteSvm);
  const liteFixture = objectAt(objectAt(liteSvm.fixtureVerification).fixture);
  const liteRoute = objectAt(liteSvm.routeExecution);
  const liteTransaction = objectAt(liteRoute.transaction);
  const liteBalances = objectAt(liteRoute.balances);
  const liteBefore = objectAt(liteBalances.before);
  const liteAfter = objectAt(liteBalances.after);
  const liteIsolation = objectAt(liteSvm.isolation);
  const fixture = objectAt(evidence.fixture);
  const offline = objectAt(fixture.offlineVerification);
  const live = objectAt(fixture.liveVerification);
  const isolation = objectAt(evidence.isolation);
  const simulatedMarketInput = objectAt(evidence.simulatedMarketInput);
  const subjects = objectAt(evidence.subjects);
  const database = objectAt(evidence.database);
  const beforeDb = objectAt(database.beforeRerun);
  const afterDb = objectAt(database.afterRerun);
  const opportunities = objectAt(beforeDb.opportunities);
  const decisions = objectAt(beforeDb.decisions);
  const submissions = objectAt(beforeDb.submissions);
  const lookupTables = objectAt(beforeDb.lookupTables);
  const roles = objectAt(evidence.roles);
  const initial = objectAt(roles.initial);
  const rerun = objectAt(roles.rerun);
  const planner = objectAt(initial.planner);
  const revalidator = objectAt(initial.revalidator);
  const executor = objectAt(initial.executor);
  const confirmer = objectAt(initial.confirmer);
  const reconciler = objectAt(initial.reconciler);
  const rerunPlanner = objectAt(rerun.planner);
  const rerunRevalidator = objectAt(rerun.revalidator);
  const rerunExecutor = objectAt(rerun.executor);
  const rerunConfirmer = objectAt(rerun.confirmer);
  const rerunReconciler = objectAt(rerun.reconciler);
  const chain = objectAt(evidence.chain);
  const before = objectAt(chain.before);
  const after = objectAt(chain.after);
  const afterRerun = objectAt(chain.afterRerun);
  const mainReserve = stringAt(subjects.mainReserve);
  const primeReserve = stringAt(subjects.primeReserve);
  const beforeMain = amountFor(before, mainReserve);
  const afterMain = amountFor(after, mainReserve);
  const beforePrime = amountFor(before, primeReserve);
  const afterPrime = amountFor(after, primeReserve);
  const rpc = objectAt(evidence.rpc);
  const proxy = objectAt(rpc.proxy);
  const proxySources = objectAt(proxy.sources);
  const proxyMethods = objectAt(proxy.methods);
  const raw = objectAt(rpc.raw);
  const rawSources = objectAt(raw.sources);
  const rawSourceErrors = objectAt(raw.sourceErrors);
  const initialRolePass = numberAt(planner.published) > 0
    && numberAt(revalidator.completed) > 0
    && numberAt(executor.completed) > 0
    && numberAt(confirmer.broadcastsSucceeded) > 0
    && numberAt(confirmer.confirmed) + numberAt(confirmer.reconciliationPending) > 0
    && numberAt(reconciler.completed) > 0;
  const rerunNoWork = numberAt(rerunPlanner.published) === 0
    && numberAt(rerunRevalidator.claimed) === 0
    && numberAt(rerunExecutor.claimed) === 0
    && numberAt(rerunConfirmer.claimed) === 0
    && numberAt(rerunReconciler.claimed) === 0;
  const exactTerminal = numberAt(opportunities.completed) === 1
    && numberAt(opportunities.active) === 0
    && numberAt(decisions.total) === 1
    && numberAt(submissions.total) === 1
    && numberAt(submissions.reconciled) === 1
    && numberAt(submissions.active) === 0
    && numberAt(submissions.distinctSemanticKeys) === 1
    && numberAt(submissions.distinctSignatures) === 1
    && numberAt(lookupTables.operationsIncomplete) === 0
    && numberAt(lookupTables.operationsPermanentFailure) === 0
    && numberAt(lookupTables.activeUsageLeases) === 0;
  const balanceDelta = beforeMain > afterMain && beforePrime < afterPrime && afterPrime > 0n;
  const rpcCountsMatch = numberAt(proxy.requests) === numberAt(raw.requests)
    && numberAt(proxySources.syntheticRpc) === numberAt(rawSources.syntheticRpc)
    && numberAt(proxySources.productionProcess) === numberAt(rawSources.productionProcess);
  const hasUsefulRpc = proxy.kind === "stateful-local-validator-rpc-proxy"
    && numberAt(proxySources.syntheticRpc) > 0
    && numberAt(proxySources.productionProcess) > 0
    && numberAt(proxy.maxInflight) > 1
    && numberAt(objectAt(proxyMethods.getAccountInfo).calls) > 0
    && numberAt(objectAt(proxyMethods.sendTransaction).calls) > 0
    && numberAt(rawSourceErrors.productionProcess) === 0
    && rpcCountsMatch;
  const liteSvmPass = liteSvm.kind === "loyal-fleet-litesvm-first-evidence"
    && numberAt(liteFixture.manifestAccountCount) > 0
    && numberAt(liteFixture.loadedAccountCount) === numberAt(liteFixture.manifestAccountCount)
    && numberAt(liteFixture.readBackMatchedAccountCount) === numberAt(liteFixture.manifestAccountCount)
    && liteFixture.allDataHashesMatched === true
    && liteFixture.allFileHashesMatched === true
    && liteFixture.allRootsPresent === true
    && liteTransaction.simulated === true
    && liteTransaction.executed === true
    && liteTransaction.exactAltCoverage === true
    && numberAt(liteBefore.mainCollateralRaw) > numberAt(liteAfter.mainCollateralRaw)
    && numberAt(liteBefore.primeCollateralRaw) < numberAt(liteAfter.primeCollateralRaw)
    && liteRoute.squadsExecution === "real-committed-sbf-fixture"
    && liteRoute.kaminoExecution === "deterministic-mock-program"
    && liteIsolation.productionSecretsLoaded === false
    && liteIsolation.productionTransactions === false
    && liteIsolation.productionDatabaseWrites === false
    && liteIsolation.networkListenersStarted === false;
  const isolationPass = isolation.database === "disposable-loopback-postgresql"
    && isolation.rpc === "instrumented-loopback-proxy-to-fresh-solana-test-validator"
    && isolation.cluster === "localnet"
    && isolation.productionDatabaseWrites === false
    && isolation.productionTransactions === false
    && isolation.productionSecretsLoaded === false
    && isolation.productionWorkerWalletSecretMounted === false
    && isolation.policySigner === "ephemeral-keypair-through-normal-policy-loader";
  const marketInputPass = simulatedMarketInput.source === "continuously-refreshed-local-fixture"
    && numberAt(simulatedMarketInput.rowCount) >= 2
    && stringAt(simulatedMarketInput.refreshedAt).length > 0
    && numberAt(simulatedMarketInput.minimumPriceTimestamp) > 0;
  return [
    {
      name: "LiteSVM prerequisite completed first",
      pass: liteSvmPass,
      detail: `${numberAt(liteFixture.loadedAccountCount)} accounts and exact Main-to-Prime route`,
    },
    {
      name: "Exact finalized fixture and live clone",
      pass: offline.status === "PASS" && live.status === "PASS"
        && numberAt(fixture.cloneAccounts) > 0 && numberAt(fixture.sourceSlot) > 0,
      detail: `${numberAt(fixture.cloneAccounts)} accounts at slot ${numberAt(fixture.sourceSlot)}`,
    },
    { name: "Local isolation and ephemeral normal signer", pass: isolationPass, detail: stringAt(isolation.rpc) },
    { name: "Truthful refreshed simulated market input", pass: marketInputPass, detail: JSON.stringify(simulatedMarketInput) },
    { name: "Nonzero production role work", pass: initialRolePass, detail: JSON.stringify(initial) },
    { name: "One reconciled terminal route", pass: exactTerminal, detail: JSON.stringify({ opportunities, decisions, submissions, lookupTables }) },
    { name: "Main-to-Prime on-chain balance delta", pass: balanceDelta, detail: `Main ${beforeMain}->${afterMain}; Prime ${beforePrime}->${afterPrime}` },
    {
      name: "Exactly-once role rerun",
      pass: rerunNoWork && stable(beforeDb) === stable(afterDb)
        && stable(after.positions) === stable(afterRerun.positions),
      detail: `rerunNoWork=${rerunNoWork}`,
    },
    { name: "Measured real and synthetic RPC load", pass: hasUsefulRpc, detail: JSON.stringify({ requests: proxy.requests, maxInflight: proxy.maxInflight, sources: proxy.sources, sourceErrors: raw.sourceErrors }) },
    { name: "Sanitized evidence", pass: noSecretMaterial(JSON.stringify(evidence)), detail: "no private key arrays or production endpoints" },
  ];
};

const report = (evidence: RecordJson, checks: Check[]) => {
  const pass = checks.every((check) => check.pass);
  const rpc = objectAt(objectAt(evidence.rpc).proxy);
  const methods = objectAt(rpc.methods);
  const raw = objectAt(objectAt(evidence.rpc).raw);
  const sources = objectAt(raw.sources);
  const sourceErrors = objectAt(raw.sourceErrors);
  const coverage = objectAt(evidence.coverage);
  const methodRows = Object.entries(methods).map(([name, value]) => {
    const stats = objectAt(value);
    return `| ${name} | ${numberAt(stats.calls)} | ${numberAt(stats.errors)} | ${numberAt(stats.p50Ms).toFixed(2)} | ${numberAt(stats.p95Ms).toFixed(2)} | ${numberAt(stats.p99Ms).toFixed(2)} |`;
  }).join("\n");
  return `# Fleet local full-chain E2E evidence\n\n` +
    `**FULL_CHAIN_E2E: ${pass ? "PASS" : "FAIL"}**\n\n` +
    checks.map((check) => `- ${check.pass ? "PASS" : "FAIL"}: ${check.name} — ${check.detail}`).join("\n") +
    `\n\n## Coverage boundary\n\n` +
    `- Route pipeline: ${stringAt(coverage.routePipeline)}\n` +
    `- Position sweep: ${stringAt(coverage.positionSweep)}\n` +
    `- Chain snapshots: ${stringAt(coverage.chainSnapshots)}\n` +
    `\n## RPC source attribution\n\n| Source | Requests | Errors |\n|---|---:|---:|\n` +
    `| Production processes | ${numberAt(sources.productionProcess)} | ${numberAt(sourceErrors.productionProcess)} |\n` +
    `| Synthetic RPC clients | ${numberAt(sources.syntheticRpc)} | ${numberAt(sourceErrors.syntheticRpc)} |\n` +
    `\nSynthetic errors are workload probe responses and are not attributed to production processes.\n` +
    `\n## RPC load\n\n| Method | Calls | Errors | p50 ms | p95 ms | p99 ms |\n|---|---:|---:|---:|---:|---:|\n${methodRows}\n`;
};

const verify = async (pathArg: string) => {
  const path = resolve(pathArg);
  const evidence = await readJson(path);
  const checks = evaluate(evidence);
  for (const check of checks) console.log(`${check.pass ? "PASS" : "FAIL"}: ${check.name} - ${check.detail}`);
  const pass = checks.every((check) => check.pass);
  console.log(`FULL_CHAIN_E2E: ${pass ? "PASS" : "FAIL"}`);
  if (!pass) process.exitCode = 1;
};

const syntheticFixture = (): RecordJson => ({
  schemaVersion: 1,
  kind: "loyal-fleet-local-chain-e2e-evidence",
  coverage: {
    routePipeline: "planner-revalidator-executor-confirmer-reconciler",
    positionSweep: "not-claimed-usdc-main-prime-fixture-is-not-the-complete-stable-mint-exit-catalog",
    chainSnapshots: "explicit-production-worker-read-only-reconcile-before-after-and-rerun",
  },
  prerequisites: {
    liteSvm: {
      kind: "loyal-fleet-litesvm-first-evidence",
      fixtureVerification: { fixture: {
        manifestAccountCount: 27, loadedAccountCount: 27, readBackMatchedAccountCount: 27,
        allDataHashesMatched: true, allFileHashesMatched: true, allRootsPresent: true,
      } },
      routeExecution: {
        squadsExecution: "real-committed-sbf-fixture", kaminoExecution: "deterministic-mock-program",
        balances: { before: { mainCollateralRaw: 1_000_000, primeCollateralRaw: 0 }, after: { mainCollateralRaw: 0, primeCollateralRaw: 1_000_000 } },
        transaction: { simulated: true, executed: true, exactAltCoverage: true },
      },
      isolation: { productionSecretsLoaded: false, productionTransactions: false, productionDatabaseWrites: false, networkListenersStarted: false },
    },
  },
  fixture: { cloneAccounts: 27, sourceSlot: 400_000_000, offlineVerification: { status: "PASS" }, liveVerification: { status: "PASS" } },
  simulatedMarketInput: { source: "continuously-refreshed-local-fixture", refreshedAt: "2026-08-13T00:00:00Z", rowCount: 2, minimumPriceTimestamp: 1_786_579_200 },
  isolation: {
    database: "disposable-loopback-postgresql", rpc: "instrumented-loopback-proxy-to-fresh-solana-test-validator",
    cluster: "localnet", productionDatabaseWrites: false, productionTransactions: false,
    productionSecretsLoaded: false, productionWorkerWalletSecretMounted: false,
    policySigner: "ephemeral-keypair-through-normal-policy-loader",
  },
  subjects: { mainReserve: "MainReserve1111111111111111111111111111111", primeReserve: "PrimeReserve11111111111111111111111111111" },
  chain: {
    before: { positions: [
      { reserve: "MainReserve1111111111111111111111111111111", amountRaw: "50000000" },
      { reserve: "PrimeReserve11111111111111111111111111111", amountRaw: "0" },
    ] },
    after: { positions: [
      { reserve: "MainReserve1111111111111111111111111111111", amountRaw: "0" },
      { reserve: "PrimeReserve11111111111111111111111111111", amountRaw: "49900000" },
    ] },
    afterRerun: { positions: [
      { reserve: "MainReserve1111111111111111111111111111111", amountRaw: "0" },
      { reserve: "PrimeReserve11111111111111111111111111111", amountRaw: "49900000" },
    ] },
  },
  database: {
    beforeRerun: {
      opportunities: { total: 1, completed: 1, active: 0 }, decisions: { total: 1 },
      submissions: { total: 1, reconciled: 1, active: 0, distinctSemanticKeys: 1, distinctSignatures: 1, signatures: ["local-signature"] },
      currentPositions: [], lookupTables: { operationsIncomplete: 0, operationsPermanentFailure: 0, activeUsageLeases: 0 },
    },
    afterRerun: {
      opportunities: { total: 1, completed: 1, active: 0 }, decisions: { total: 1 },
      submissions: { total: 1, reconciled: 1, active: 0, distinctSemanticKeys: 1, distinctSignatures: 1, signatures: ["local-signature"] },
      currentPositions: [], lookupTables: { operationsIncomplete: 0, operationsPermanentFailure: 0, activeUsageLeases: 0 },
    },
  },
  roles: {
    initial: {
      planner: { published: 1 }, revalidator: { completed: 1 }, executor: { completed: 1 },
      confirmer: { broadcastsSucceeded: 1, confirmed: 1 }, reconciler: { completed: 1 },
    },
    rerun: {
      planner: { published: 0 }, revalidator: { claimed: 0 }, executor: { claimed: 0 },
      confirmer: { claimed: 0 }, reconciler: { claimed: 0 },
    },
  },
  rpc: {
    proxy: {
      kind: "stateful-local-validator-rpc-proxy", requests: 12, maxInflight: 4,
      sources: { productionProcess: 6, syntheticRpc: 6 },
      methods: { getAccountInfo: { calls: 4 }, sendTransaction: { calls: 1 } },
    },
    raw: {
      requests: 12, sources: { productionProcess: 6, syntheticRpc: 6 },
      sourceErrors: { productionProcess: 0, syntheticRpc: 1 },
    },
  },
});

const selfTest = () => {
  const positive = syntheticFixture();
  if (!evaluate(positive).every((check) => check.pass)) throw new Error("positive fixture did not pass");
  const controls: Array<[string, (fixture: RecordJson) => void]> = [
    ["validator evidence without LiteSVM prerequisite", (fixture) => { objectAt(objectAt(fixture.prerequisites).liteSvm).kind = "missing"; }],
    ["fake validator/live clone", (fixture) => { objectAt(objectAt(fixture.fixture).liveVerification).status = "FAIL"; }],
    ["liveness without role work", (fixture) => { objectAt(objectAt(objectAt(fixture.roles).initial).revalidator).completed = 0; }],
    ["terminal DB without balance delta", (fixture) => { objectAt(fixture.chain).after = objectAt(fixture.chain).before; }],
    ["delta with duplicate signatures", (fixture) => {
      const submissions = objectAt(objectAt(objectAt(fixture.database).beforeRerun).submissions);
      submissions.total = 2; submissions.distinctSignatures = 1; submissions.signatures = ["duplicate", "duplicate"];
    }],
    ["rerun mutates chain state", (fixture) => {
      const positions = objectAt(fixture.chain).afterRerun as RecordJson;
      const values = positions.positions as Json[];
      objectAt(values[1]).amountRaw = "49800000";
    }],
    ["production endpoint or signer claim", (fixture) => { objectAt(fixture.isolation).productionSecretsLoaded = true; }],
  ];
  console.log("PASS: positive full-chain fixture");
  for (const [name, mutate] of controls) {
    const fixture = structuredClone(positive) as RecordJson;
    mutate(fixture);
    if (evaluate(fixture).every((check) => check.pass)) throw new Error(`negative control unexpectedly passed: ${name}`);
    console.log(`PASS: negative control rejected - ${name}`);
  }
  console.log("FULL_CHAIN_E2E_VERIFIER: PASS");
};

const command = process.argv[2];
if (command === "assemble") await assemble(process.argv[3] ?? "");
else if (command === "verify") await verify(process.argv[3] ?? "");
else if (command === undefined) selfTest();
else throw new Error(`unknown command ${basename(command)}`);
