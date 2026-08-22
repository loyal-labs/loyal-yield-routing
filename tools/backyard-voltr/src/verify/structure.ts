import { PublicKey } from "@solana/web3.js";
import { address } from "@solana/kit";

import {
  assertIntentForRouteBinding,
  type ManagerRuntimeIntent,
} from "../domain/execution-intent.js";
import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
} from "../domain/route-spec.js";
import { RUNTIME_OPERATIONS } from "../runtime/commands.js";
import type { Gate } from "./current.js";

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function deriveManager(): string {
  return PublicKey.findProgramAddressSync(
    [
      Buffer.from("smart_account"),
      new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(),
      Buffer.from("smart_account"),
      Buffer.from([PARTNER_ROUTE.squads.vaultIndex]),
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}

function derivePolicy(seed: bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [
      Buffer.from("smart_account"),
      Buffer.from("policy"),
      new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(),
      seedBytes,
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}

function rejected(intent: ManagerRuntimeIntent): boolean {
  try {
    assertIntentForRouteBinding(intent, {
      routeId: PARTNER_FOUR_MARKET_ROUTE.id,
      routeSpecSha256: fourMarketRouteSpecSha256(),
      maxManagerOperationRaw: PARTNER_ROUTE.asset.maxManagerOperationRaw,
    });
    return false;
  } catch {
    return true;
  }
}

export function verifyPartnerStructure() {
  const route = PARTNER_ROUTE;
  const gates: Gate[] = [];
  const derivedManager = deriveManager();
  const runtimePolicies = PARTNER_FOUR_MARKET_ROUTE.strategies.flatMap((strategy, index) => {
    const depositSeed = route.squads.policySeedBefore + 1n + BigInt(index * 2);
    const withdrawSeed = depositSeed + 1n;
    return [
      { strategyId: strategy.id, operation: "deposit" as const, seed: depositSeed, policy: derivePolicy(depositSeed) },
      { strategyId: strategy.id, operation: "withdraw" as const, seed: withdrawSeed, policy: derivePolicy(withdrawSeed) },
    ];
  });
  const fourMarketHash = fourMarketRouteSpecSha256();
  add(gates, "withdrawal timeout is exactly ten minutes", route.vaultConfiguration.withdrawalWaitingPeriodSeconds === 600n && PARTNER_FOUR_MARKET_ROUTE.withdrawalWaitingPeriodSeconds === 600n, { vault: route.vaultConfiguration.withdrawalWaitingPeriodSeconds, router: PARTNER_FOUR_MARKET_ROUTE.withdrawalWaitingPeriodSeconds }, { vault: 600n, router: 600n });
  add(gates, "normal optimization interval is exactly one hour", PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds === 3_600n, PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds, 3_600n);
  add(gates, "Squads manager PDA derives exactly", derivedManager === route.squads.manager, derivedManager, route.squads.manager);
  add(gates, "admin manager and guardian are distinct", new Set([route.setupAdmin, route.squads.manager, route.squads.guardian]).size === 3, [route.setupAdmin, route.squads.manager, route.squads.guardian], "three distinct identities");
  add(gates, "guardian authority is threshold one with permission mask seven", route.squads.threshold === 1 && route.squads.guardianPermissionsMask === 7, { threshold: route.squads.threshold, permissionsMask: route.squads.guardianPermissionsMask }, { threshold: 1, permissionsMask: 7 });
  add(gates, "runtime policies are the exact eight sequential route pairs", runtimePolicies.length === 8 && runtimePolicies.every(({ seed }, index) => seed === route.squads.policySeedBefore + 1n + BigInt(index)), runtimePolicies, "main/onre/prime/maple deposit+withdraw at seeds 43..50");
  add(gates, "all eight runtime policy PDAs are distinct", new Set(runtimePolicies.map(({ policy }) => policy)).size === 8, runtimePolicies.map(({ policy }) => policy), "eight distinct PDAs");
  add(gates, "runtime surface contains no setup authority operation", RUNTIME_OPERATIONS.length === 5 && RUNTIME_OPERATIONS.every((operation) => !operation.includes("initialize") && !operation.includes("policy") && !operation.includes("adaptor") && !operation.includes("instant")), RUNTIME_OPERATIONS, ["user-deposit", "manager-deposit", "manager-withdraw", "withdraw-request", "withdraw-claim"]);
  add(gates, "manager limit is positive and no larger than vault cap", route.asset.maxManagerOperationRaw > 0n && route.asset.maxManagerOperationRaw <= route.asset.vaultCapRaw, route.asset.maxManagerOperationRaw, `1..${route.asset.vaultCapRaw}`);
  add(gates, "Main route binds native USDC token program", route.asset.mint === "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" && route.asset.tokenProgram === route.programs.token, { mint: route.asset.mint, tokenProgram: route.asset.tokenProgram }, { mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", tokenProgram: route.programs.token });
  add(gates, "Main reserve market and farm identities are distinct and fixed", new Set([route.strategy.reserve, route.strategy.lendingMarket, route.strategy.collateralFarm]).size === 3, route.strategy, "three distinct approved identities");
  add(gates, "four-market reserve catalog is exact and closed", JSON.stringify(PARTNER_FOUR_MARKET_ROUTE.strategies.map(({ id, reserve }) => ({ id, reserve }))) === JSON.stringify([
    { id: "main", reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59" },
    { id: "onre", reserve: "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z" },
    { id: "prime", reserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu" },
    { id: "maple", reserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo" },
  ]), PARTNER_FOUR_MARKET_ROUTE.strategies.map(({ id, reserve }) => ({ id, reserve })), "exact four approved reserve identities");
  add(gates, "adaptor bypass is disabled", route.vaultConfiguration.allowAnyAdaptor === 0, route.vaultConfiguration.allowAnyAdaptor, 0);
  add(gates, "deployment identities cover every privileged program", route.deployments.map(({ programId }) => programId).join(",") === [route.programs.voltrVault, route.programs.kaminoAdaptor, route.programs.klend, route.programs.farms, route.squads.program].join(","), route.deployments.map(({ programId }) => programId), [route.programs.voltrVault, route.programs.kaminoAdaptor, route.programs.klend, route.programs.farms, route.squads.program]);

  const valid: ManagerRuntimeIntent = {
    schemaVersion: 1,
    kind: "runtime",
    operation: "manager-deposit",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketHash,
    signerRole: "guardian",
    guardian: route.squads.guardian,
    policy: address(runtimePolicies[0]!.policy),
    amountRaw: route.asset.maxManagerOperationRaw,
    nonce: "structure-verifier-valid-intent",
    prestateSlot: 1n,
    expiresAtUnix: 1n,
    canonicalMessageSha256: "00".repeat(32),
    lifecycleId: "11".repeat(32),
    protectedPrestateSha256: "22".repeat(32),
    routeAuthorizationSha256: "33".repeat(32),
  };
  let validAccepted = true;
  try {
    assertIntentForRouteBinding(valid, {
      routeId: PARTNER_FOUR_MARKET_ROUTE.id,
      routeSpecSha256: fourMarketHash,
      maxManagerOperationRaw: route.asset.maxManagerOperationRaw,
      routeAuthorizationSha256: valid.routeAuthorizationSha256,
    });
  } catch {
    validAccepted = false;
  }
  add(gates, "bounded manager intent is accepted", validAccepted, valid.amountRaw, route.asset.maxManagerOperationRaw);
  add(gates, "zero manager amount is rejected", rejected({ ...valid, amountRaw: 0n }), 0n, "rejected");
  add(gates, "over-limit manager amount is rejected", rejected({ ...valid, amountRaw: route.asset.maxManagerOperationRaw + 1n }), route.asset.maxManagerOperationRaw + 1n, "rejected");
  add(gates, "wrong route hash is rejected", rejected({ ...valid, routeSpecSha256: "ff".repeat(32) }), "ff".repeat(32), "rejected");
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    verdict: failedGateCount === 0 ? "PARTNER_STRUCTURE_PASS" : "PARTNER_STRUCTURE_FAIL",
    broadcast: false,
    routeSpecSha256: fourMarketHash,
    derived: { manager: derivedManager, runtimePolicies },
    failedGateCount,
    gates,
  } as const;
}
