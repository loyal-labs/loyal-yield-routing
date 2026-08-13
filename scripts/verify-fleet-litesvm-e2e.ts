#!/usr/bin/env bun

import { readFile, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
type RecordJson = { [key: string]: Json };
type Check = { name: string; pass: boolean; detail: string };

const ROOTS = {
  squadsProgram: "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG",
  kaminoLendProgram: "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD",
  mainMarket: "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
  primeMarket: "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
  mainUsdcReserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
  primeUsdcReserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu",
  usdcMint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
};

const isRecord = (value: unknown): value is RecordJson =>
  value !== null && typeof value === "object" && !Array.isArray(value);
const objectAt = (value: unknown): RecordJson => isRecord(value) ? value : {};
const numberAt = (value: unknown): number => typeof value === "number" && Number.isFinite(value) ? value : 0;
const stringAt = (value: unknown): string => typeof value === "string" ? value : "";
const readJson = async (path: string): Promise<RecordJson> => {
  const value: unknown = JSON.parse(await readFile(path, "utf8"));
  if (!isRecord(value)) throw new Error(`${path} must contain a JSON object`);
  return value;
};

const marker = (text: string, name: string): RecordJson => {
  const line = text.split("\n").find((candidate) => candidate.startsWith(`${name}=`));
  if (!line) throw new Error(`missing ${name} marker`);
  const value: unknown = JSON.parse(line.slice(name.length + 1));
  if (!isRecord(value)) throw new Error(`${name} must be an object`);
  return value;
};

const noSecrets = (serialized: string) =>
  !/(api\.mainnet-beta\.solana\.com|helius|neon\.tech|render\.com)/iu.test(serialized)
  && !/\[(?:\s*\d{1,3}\s*,){31,}\s*\d{1,3}\s*\]/u.test(serialized);

const evaluate = (evidence: RecordJson): Check[] => {
  const fixtureVerification = objectAt(evidence.fixtureVerification);
  const fixture = objectAt(fixtureVerification.fixture);
  const roots = objectAt(fixtureVerification.roots);
  const fixtureBoundary = objectAt(fixtureVerification.boundary);
  const route = objectAt(evidence.routeExecution);
  const routeAddresses = objectAt(route.route);
  const balances = objectAt(route.balances);
  const before = objectAt(balances.before);
  const after = objectAt(balances.after);
  const transaction = objectAt(route.transaction);
  const catalog = objectAt(evidence.altCatalog);
  const catalogFixtures = Array.isArray(catalog.fixtures) ? catalog.fixtures.filter(isRecord) : [];
  const isolation = objectAt(evidence.isolation);
  const exactRoots = Object.entries(ROOTS).every(([name, value]) => roots[name] === value);
  const routeRoots = routeAddresses.mainMarket === ROOTS.mainMarket
    && routeAddresses.primeMarket === ROOTS.primeMarket
    && routeAddresses.mainReserve === ROOTS.mainUsdcReserve
    && routeAddresses.primeReserve === ROOTS.primeUsdcReserve
    && routeAddresses.liquidityMint === ROOTS.usdcMint;
  const manifestCount = numberAt(fixture.manifestAccountCount);
  const closure = fixtureVerification.kind === "loyal-fleet-litesvm-fixture-verification"
    && fixtureVerification.engine === "LiteSVM"
    && manifestCount > 0
    && numberAt(fixture.loadedAccountCount) === manifestCount
    && numberAt(fixture.readBackMatchedAccountCount) === manifestCount
    && fixture.allDataHashesMatched === true
    && fixture.allFileHashesMatched === true
    && fixture.allRootsPresent === true
    && fixture.rootsMatchLoyalActions === true
    && exactRoots;
  const mainBefore = numberAt(before.mainCollateralRaw);
  const primeBefore = numberAt(before.primeCollateralRaw);
  const mainAfter = numberAt(after.mainCollateralRaw);
  const primeAfter = numberAt(after.primeCollateralRaw);
  const balanceDelta = mainBefore > 0 && primeBefore === 0 && mainAfter === 0 && primeAfter === mainBefore;
  const routeProof = route.engine === "LiteSVM"
    && route.squadsExecution === "real-committed-sbf-fixture"
    && route.loyalActionsExecution === "production-builders"
    && route.kaminoExecution === "deterministic-mock-program"
    && route.ephemeralDelegatedSigner === true
    && routeRoots && balanceDelta
    && transaction.version === "v0"
    && transaction.exactAltCoverage === true
    && transaction.simulated === true
    && transaction.executed === true
    && transaction.nonzeroCompute === true
    && transaction.packetBelowLimit === true;
  const ordinary = catalogFixtures.find((value) => value.name === "ordinary_same_mint_source_withdrawal_target_deposit");
  const catalogPass = numberAt(catalog.fixtureCount) === numberAt(catalog.expectedFixtureCount)
    && numberAt(catalog.fixtureCount) > 0 && !!ordinary
    && numberAt(ordinary?.singleClassExpansion) > 0
    && numberAt(catalog.largestAtomicExpansion) > 0
    && numberAt(catalog.allocationHighWater) > 0;
  const honestIsolation = route.kaminoExecution === "deterministic-mock-program"
    && route.networkAccessed === false && route.rpcUsed === false && route.databaseUsed === false
    && fixtureBoundary.networkAccessed === false && fixtureBoundary.rpcUsed === false
    && fixtureBoundary.databaseUsed === false && fixtureBoundary.transactionsSentToNetwork === false
    && fixtureBoundary.privateKeysLoaded === false
    && isolation.productionSecretsLoaded === false
    && isolation.productionTransactions === false
    && isolation.productionDatabaseWrites === false
    && isolation.networkListenersStarted === false;
  return [
    { name: "Exact captured fixture closure", pass: closure, detail: `${manifestCount} accounts loaded and read back` },
    { name: "Transaction-level Main-to-Prime proof", pass: routeProof, detail: `Main ${mainBefore}->${mainAfter}; Prime ${primeBefore}->${primeAfter}` },
    { name: "Exact v0 ALT coverage catalog", pass: catalogPass, detail: `${numberAt(catalog.fixtureCount)} fixtures; largest expansion ${numberAt(catalog.largestAtomicExpansion)}` },
    { name: "Truthful LiteSVM boundary and isolation", pass: honestIsolation, detail: stringAt(route.kaminoExecution) },
    { name: "Sanitized evidence", pass: noSecrets(JSON.stringify(evidence)), detail: "no private-key arrays or production endpoints" },
  ];
};

const report = (evidence: RecordJson, checks: Check[]) => {
  const pass = checks.every((check) => check.pass);
  return `# Fleet LiteSVM-first evidence\n\n` +
    `**LITESVM_E2E: ${pass ? "PASS" : "FAIL"}**\n\n` +
    checks.map((check) => `- ${check.pass ? "PASS" : "FAIL"}: ${check.name} — ${check.detail}`).join("\n") +
    `\n\nValidator node gate: **${pass ? "READY" : "BLOCKED"}**\n`;
};

const assemble = async (directoryArg: string) => {
  const directory = resolve(directoryArg);
  const testLog = await readFile(join(directory, "litesvm-route-test.log"), "utf8");
  const evidence: RecordJson = {
    schemaVersion: 1,
    kind: "loyal-fleet-litesvm-first-evidence",
    generatedAtUtc: new Date().toISOString(),
    fixtureVerification: await readJson(join(directory, "litesvm-fixture.json")),
    routeExecution: marker(testLog, "fleet_litesvm_main_prime_evidence"),
    altCatalog: marker(testLog, "reusable_alt_catalog_summary"),
    isolation: {
      productionSecretsLoaded: false,
      productionTransactions: false,
      productionDatabaseWrites: false,
      networkListenersStarted: false,
    },
  };
  const checks = evaluate(evidence);
  await writeFile(join(directory, "evidence.json"), `${JSON.stringify(evidence, null, 2)}\n`);
  await writeFile(join(directory, "evidence.md"), report(evidence, checks));
  console.log(JSON.stringify({ status: checks.every((check) => check.pass) ? "PASS" : "FAIL", evidence: join(directory, "evidence.json") }));
};

const verify = async (pathArg: string) => {
  const evidence = await readJson(resolve(pathArg));
  const checks = evaluate(evidence);
  for (const check of checks) console.log(`${check.pass ? "PASS" : "FAIL"}: ${check.name} - ${check.detail}`);
  const pass = checks.every((check) => check.pass);
  console.log(`LITESVM_E2E: ${pass ? "PASS" : "FAIL"}`);
  console.log(`VALIDATOR_NODE_E2E: ${pass ? "READY" : "BLOCKED"}`);
  if (!pass) process.exitCode = 1;
};

const fixture = (): RecordJson => ({
  schemaVersion: 1,
  kind: "loyal-fleet-litesvm-first-evidence",
  fixtureVerification: {
    kind: "loyal-fleet-litesvm-fixture-verification", engine: "LiteSVM",
    fixture: {
      manifestAccountCount: 27, loadedAccountCount: 27, readBackMatchedAccountCount: 27,
      allDataHashesMatched: true, allFileHashesMatched: true, allRootsPresent: true, rootsMatchLoyalActions: true,
    },
    roots: { ...ROOTS, kaminoFarmsProgram: "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr" },
    boundary: { networkAccessed: false, rpcUsed: false, databaseUsed: false, transactionsSentToNetwork: false, privateKeysLoaded: false },
  },
  routeExecution: {
    engine: "LiteSVM", squadsExecution: "real-committed-sbf-fixture", loyalActionsExecution: "production-builders",
    kaminoExecution: "deterministic-mock-program", networkAccessed: false, rpcUsed: false, databaseUsed: false,
    ephemeralDelegatedSigner: true,
    route: { mainMarket: ROOTS.mainMarket, primeMarket: ROOTS.primeMarket, mainReserve: ROOTS.mainUsdcReserve, primeReserve: ROOTS.primeUsdcReserve, liquidityMint: ROOTS.usdcMint },
    balances: { before: { mainCollateralRaw: 1_000_000, primeCollateralRaw: 0 }, after: { mainCollateralRaw: 0, primeCollateralRaw: 1_000_000 } },
    transaction: { version: "v0", exactAltCoverage: true, simulated: true, executed: true, nonzeroCompute: true, packetBelowLimit: true },
  },
  altCatalog: {
    fixtureCount: 1, expectedFixtureCount: 1, largestAtomicExpansion: 10, allocationHighWater: 230,
    fixtures: [{ name: "ordinary_same_mint_source_withdrawal_target_deposit", singleClassExpansion: 10 }],
  },
  isolation: { productionSecretsLoaded: false, productionTransactions: false, productionDatabaseWrites: false, networkListenersStarted: false },
});

const selfTest = () => {
  const positive = fixture();
  if (!evaluate(positive).every((check) => check.pass)) throw new Error("positive fixture failed");
  const controls: Array<[string, (value: RecordJson) => void]> = [
    ["missing fixture account", (value) => { objectAt(objectAt(value.fixtureVerification).fixture).readBackMatchedAccountCount = 26; }],
    ["root mismatch", (value) => { objectAt(objectAt(value.fixtureVerification).roots).mainUsdcReserve = "wrong"; }],
    ["no balance delta", (value) => { objectAt(objectAt(objectAt(value.routeExecution).balances).after).primeCollateralRaw = 0; }],
    ["mock falsely labelled real", (value) => { objectAt(value.routeExecution).kaminoExecution = "real-mainnet-kamino"; }],
    ["missing simulation or ALT proof", (value) => { objectAt(objectAt(value.routeExecution).transaction).simulated = false; }],
    ["production endpoint or secret", (value) => { objectAt(value.isolation).productionSecretsLoaded = true; }],
  ];
  console.log("PASS: positive LiteSVM fixture");
  for (const [name, mutate] of controls) {
    const negative = structuredClone(positive) as RecordJson;
    mutate(negative);
    if (evaluate(negative).every((check) => check.pass)) throw new Error(`negative control passed: ${name}`);
    console.log(`PASS: negative control rejected - ${name}`);
  }
  console.log("LITESVM_E2E_VERIFIER: PASS");
};

const command = process.argv[2];
if (command === "assemble") await assemble(process.argv[3] ?? "");
else if (command === "verify") await verify(process.argv[3] ?? "");
else if (command === undefined) selfTest();
else throw new Error(`unknown command: ${command}`);
