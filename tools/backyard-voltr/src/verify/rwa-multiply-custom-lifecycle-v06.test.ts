import { strict as assert } from "node:assert";
import { test } from "node:test";
import { PublicKey } from "@solana/web3.js";

import {
  validateV06ActionCoverage,
  validateV06Lifecycle,
  validateV06LaunchYieldAttestation,
  validateV06PositionSnapshot,
  validateV06ReturnDataNAV,
  validateV06FinalTicket,
  validateV06ReportIdentity,
  v06LaunchYieldAttestationHash,
  type V06LaunchYieldAttestation,
  type V06RouteBindings,
} from "./rwa-multiply-custom-lifecycle-v06.js";

const address = "11111111111111111111111111111111";
const reportTicket = PublicKey.findProgramAddressSync([
  Buffer.from("report_ticket"), new PublicKey(address).toBuffer(),
], new PublicKey(address))[0].toBase58();
const route: V06RouteBindings = {
  routeKey: "rwa-multiply:test",
  genesisHash: "mainnet",
  withdrawalWaitSeconds: 600,
  targetLtvBps: 5_000,
  maxReportAgeSlots: 32,
  manifestSha256: "11".repeat(32),
  policyCatalogSha256: "22".repeat(32),
  programs: {
    voltr: address, adaptor: address, squads: address, kamino: address,
    jupiter: address, token: address, associatedToken: address,
  },
  accounts: {
    voltrVault: address, strategy: address, strategyReceipt: address,
    voltrIdleAta: address, strategyAta: address, squadsUsdcAta: address,
    squadsPrimeAta: address, obligation: address, collateralReserve: address,
    debtReserve: address, squadsSettings: address, squadsVault: address, reportTicket,
  },
  mints: { usdc: address, prime: address },
};

test("V06 never treats declarations as independent lifecycle evidence", () => {
  const result = validateV06Lifecycle(
    { schema: "loyal-backyard-rwa-live-lifecycle/v2", broadcast: true },
    route,
    { attempted: false, error: "RPC unavailable", genesisHash: null, transactions: [], finalContextSlot: null, finalAccounts: [], finalAccountData: {} },
    { attempted: false, error: "database unavailable", rows: [], position: null, nonterminalCount: null, lifecycleNonterminalCount: null, hold: null, riskAfterHoldCount: null },
  );
  assert.equal(result.pass, false);
  assert.equal(result.checks.independentConfirmedTransactions, false);
  assert.equal(result.checks.persistedBeforeSendAndReconciled, false);
  assert.equal(result.checks.utilizationHoldPreventedRiskIncrease, false);
});

test("V06 requires the exact reconciled utilization-HOLD unwind coverage", () => {
  const complete: Array<[string, number]> = [
    ["VOLTR_ALLOCATE_TO_SQUADS", 1], ["REPORT_NAV", 2],
    ["DELEVER_PRIME_USDC_STEP", 1], ["SWAP_PRIME_TO_USDC_STEP", 1],
    ["STAGE_SQUADS_TO_VOLTR", 1], ["VOLTR_RESTORE_IDLE", 1],
  ];
  assert.equal(validateV06ActionCoverage(complete), true);
  assert.equal(validateV06ActionCoverage(complete.map(([action, count]) =>
    [action, action === "REPORT_NAV" ? 1 : count])), false);
  assert.equal(validateV06ActionCoverage([...complete, ["OPEN_PRIME_USDC_STEP", 1]]), false);
});

test("V06 launch snapshot is post-redeposit and within target LTV without inventing APY", () => {
  const snapshot = {
    observedSlot: 120,
    collateralRaw: "150",
    debtRaw: "50",
    ltvBps: 3_334,
    valuationSource: "backyard_rwa_v1_onchain_position_only",
  };
  assert.equal(validateV06PositionSnapshot(snapshot, 120, 140, 5_000), true);
  assert.equal(validateV06PositionSnapshot({ ...snapshot, observedSlot: 119 }, 120, 140, 5_000), false);
  assert.equal(validateV06PositionSnapshot({ ...snapshot, ltvBps: 5_001 }, 120, 140, 5_000), false);
});

test("V06 requires a hash-bound contemporaneous positive external total-route-yield attestation", () => {
  const launchBlockTime = 1_800_000_000;
  const unsigned = {
    schema: "loyal-backyard-rwa-launch-yield/v1",
    routeKey: route.routeKey,
    strategyKey: "PRIME/USDC",
    method: "manual_external_total_route_yield",
    observedAt: new Date((launchBlockTime - 60) * 1_000).toISOString(),
    validUntil: new Date((launchBlockTime + 600) * 1_000).toISOString(),
    totalRouteYieldBps: 275,
    source: "manual:backyard-fixed-route-review",
    attestationSha256: "00".repeat(32),
  } as const satisfies V06LaunchYieldAttestation;
  const attestation = { ...unsigned, attestationSha256: v06LaunchYieldAttestationHash(unsigned) };
  assert.equal(validateV06LaunchYieldAttestation(attestation, route.routeKey, launchBlockTime), true);
  assert.equal(validateV06LaunchYieldAttestation({ ...attestation, totalRouteYieldBps: 0 }, route.routeKey, launchBlockTime), false);
  assert.equal(validateV06LaunchYieldAttestation({ ...attestation, totalRouteYieldBps: 276 }, route.routeKey, launchBlockTime), false);
  assert.equal(validateV06LaunchYieldAttestation({ ...attestation, observedAt: new Date((launchBlockTime - 86_401) * 1_000).toISOString() }, route.routeKey, launchBlockTime), false);
});

test("V06 ticket report binds sequence to its observed slot", () => {
  const report = {
    signature: "1",
    sequence: "120",
    observedSlot: "120",
    navAfterRaw: "42",
    snapshotDigest: "ab".repeat(32),
  };
  assert.equal(validateV06ReportIdentity(report), true);
  assert.equal(validateV06ReportIdentity({ ...report, sequence: "119" }), false);
  assert.equal(validateV06ReportIdentity({ ...report, observedSlot: "0" }), false);
  const nav = Buffer.alloc(8);
  nav.writeBigUInt64LE(42n);
  assert.equal(validateV06ReturnDataNAV({ returnData: { programId: route.programs.adaptor, dataBase64: nav.toString("base64") }, logs: [] }, report, route), true);
  assert.equal(validateV06ReturnDataNAV({ returnData: null, logs: [`Program return: ${route.programs.adaptor} ${nav.toString("base64")}`] }, report, route), true);
  assert.equal(validateV06ReturnDataNAV({ returnData: null, logs: [`Program log: Program return: ${route.programs.adaptor} ${nav.toString("base64")}`] }, report, route), false);
  assert.equal(validateV06ReturnDataNAV({ returnData: null, logs: [
    `Program return: ${route.programs.adaptor} ${nav.toString("base64")}`,
    `Program return: ${route.programs.adaptor} AAAAAAAAAAA=`,
  ] }, report, route), false);
  assert.equal(validateV06ReturnDataNAV({ returnData: null, logs: [`Program return: SysvarRent111111111111111111111111111111111 ${nav.toString("base64")}`] }, report, route), false);
  assert.equal(validateV06ReturnDataNAV({ returnData: { programId: "SysvarRent111111111111111111111111111111111", dataBase64: nav.toString("base64") }, logs: [] }, report, route), false);
  assert.equal(validateV06ReturnDataNAV({
    returnData: { programId: route.programs.adaptor, dataBase64: "AAAAAAAAAAA=" },
    logs: [`Program return: ${route.programs.adaptor} ${nav.toString("base64")}`],
  }, report, route), false);
  nav.writeBigUInt64LE(43n);
  assert.equal(validateV06ReturnDataNAV({ returnData: { programId: route.programs.voltr, dataBase64: nav.toString("base64") }, logs: [] }, report, route), false);
});

test("V06 final report ticket is inactive and retains only the consumed sequence", () => {
  const data = Buffer.alloc(96);
  Buffer.from("f568b6c53ae774ed", "hex").copy(data);
  data[8] = 1;
  data[9] = 254;
  new PublicKey(route.accounts.strategy).toBuffer().copy(data, 16);
  data.writeBigUInt64LE(120n, 48);
  assert.equal(validateV06FinalTicket(data.toString("base64"), route, "120"), true);
  data[10] = 1;
  assert.equal(validateV06FinalTicket(data.toString("base64"), route, "120"), false);
});
