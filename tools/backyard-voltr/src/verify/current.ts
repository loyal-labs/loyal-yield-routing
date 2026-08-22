import { createHash } from "node:crypto";

import { Obligation, UserMetadata, UserState } from "@kamino-finance/klend-sdk";
import { getMintDecoder, getTokenDecoder } from "@solana-program/token";
import { getAddressDecoder } from "@solana/kit";
import {
  ADAPTOR_ADD_RECEIPT_DISCRIMINATOR,
  getStrategyInitReceiptDecoder,
  getStrategyInitReceiptSize,
  getVaultDecoder,
  getVaultSize,
  STRATEGY_INIT_RECEIPT_DISCRIMINATOR,
  VAULT_DISCRIMINATOR,
} from "@voltr/vault-sdk";

import type { PartnerRouteSpec } from "../domain/route-spec.js";
import type { AccountSnapshot } from "../integrations/solana-compat.js";
import type { VoltrAccounts } from "../integrations/voltr.js";
import type { ReserveGraph } from "../integrations/voltr.js";

export type Gate = Readonly<{
  name: string;
  pass: boolean;
  observed: unknown;
  expected: unknown;
}>;

export type VaultStateReport = Readonly<{
  verdict: "PARTNER_VAULT_CURRENT_PASS" | "PARTNER_VAULT_CURRENT_FAIL";
  failedGateCount: number;
  gates: readonly Gate[];
  state: null | Readonly<{
    vaultDataSha256: string;
    manager: string;
    admin: string;
    pendingAdmin: string;
    waitingPeriodSeconds: bigint;
    totalValueRaw: bigint;
    lpSupplyRaw: bigint;
    idleRaw: bigint;
  }>;
}>;

export function verifyDeploymentIdentities(
  route: PartnerRouteSpec,
  observed: readonly Readonly<{
    programId: string;
    programDataAddress: string | null;
    deployedSlot: bigint | null;
    executableSha256: string | null;
  }>[],
  includedProgramIds: readonly string[] = route.deployments.map(({ programId }) => programId),
): readonly Gate[] {
  const gates: Gate[] = [];
  const expectedDeployments = route.deployments.filter(({ programId }) => includedProgramIds.includes(programId));
  const observedByProgram = new Map(observed.map((identity) => [identity.programId, identity]));
  add(gates, "deployment count", expectedDeployments.every(({ programId }) => observedByProgram.has(programId)), observed.filter(({ programId }) => includedProgramIds.includes(programId)).length, expectedDeployments.length);
  expectedDeployments.forEach((expected) => {
    const actual = observedByProgram.get(expected.programId) ?? null;
    add(gates, `${expected.programId} program id`, actual?.programId === expected.programId, actual?.programId ?? null, expected.programId);
    add(gates, `${expected.programId} program-data address`, actual?.programDataAddress === expected.programDataAddress, actual?.programDataAddress ?? null, expected.programDataAddress);
    add(gates, `${expected.programId} deployed slot`, actual?.deployedSlot === expected.deployedSlot, actual?.deployedSlot ?? null, expected.deployedSlot);
    add(gates, `${expected.programId} executable hash`, actual?.executableSha256 === expected.executableSha256, actual?.executableSha256 ?? null, expected.executableSha256);
  });
  return gates;
}

export function verifyAdaptorReceipt(
  route: PartnerRouteSpec,
  receiptAddress: string,
  receipt: AccountSnapshot | null,
): readonly Gate[] {
  const gates: Gate[] = [];
  add(gates, "adaptor receipt exists", receipt !== null, receipt?.address ?? null, receiptAddress);
  if (!receipt) return gates;
  add(gates, "adaptor receipt owner", receipt.owner === route.programs.voltrVault, receipt.owner, route.programs.voltrVault);
  add(gates, "deployed adaptor receipt length", receipt.data.length === 152, receipt.data.length, 152);
  add(
    gates,
    "adaptor receipt discriminator",
    Buffer.from(receipt.data.subarray(0, ADAPTOR_ADD_RECEIPT_DISCRIMINATOR.length))
      .equals(Buffer.from(ADAPTOR_ADD_RECEIPT_DISCRIMINATOR)),
    Buffer.from(receipt.data.subarray(0, ADAPTOR_ADD_RECEIPT_DISCRIMINATOR.length)).toString("hex"),
    Buffer.from(ADAPTOR_ADD_RECEIPT_DISCRIMINATOR).toString("hex"),
  );
  if (receipt.data.length >= 72) {
    const decoder = getAddressDecoder();
    const observedVault = decoder.decode(receipt.data.subarray(8, 40));
    const observedAdaptor = decoder.decode(receipt.data.subarray(40, 72));
    add(gates, "adaptor receipt vault", observedVault === route.vault, observedVault, route.vault);
    add(gates, "adaptor receipt program", observedAdaptor === route.programs.kaminoAdaptor, observedAdaptor, route.programs.kaminoAdaptor);
  }
  return gates;
}

export function verifyStrategyBootstrap(input: Readonly<{
  route: PartnerRouteSpec;
  accounts: VoltrAccounts;
  graph: ReserveGraph;
  strategyReceipt: AccountSnapshot | null;
  userMetadata: AccountSnapshot | null;
  obligation: AccountSnapshot | null;
  obligationFarm: AccountSnapshot | null;
}>): readonly Gate[] {
  const { route, accounts, graph } = input;
  const gates: Gate[] = [];
  add(gates, "strategy receipt exists", input.strategyReceipt !== null, input.strategyReceipt?.address ?? null, accounts.strategyInitReceipt);
  add(gates, "Kamino user metadata exists", input.userMetadata !== null, input.userMetadata?.address ?? null, graph.userMetadata);
  add(gates, "Kamino farm user state exists", input.obligationFarm !== null, input.obligationFarm?.address ?? null, graph.obligationFarm);
  if (!input.strategyReceipt || !input.userMetadata || !input.obligationFarm) return gates;
  add(gates, "strategy receipt owner", input.strategyReceipt.owner === route.programs.voltrVault, input.strategyReceipt.owner, route.programs.voltrVault);
  add(gates, "strategy receipt size", input.strategyReceipt.data.length === getStrategyInitReceiptSize(), input.strategyReceipt.data.length, getStrategyInitReceiptSize());
  add(gates, "strategy receipt discriminator", Buffer.from(input.strategyReceipt.data.subarray(0, STRATEGY_INIT_RECEIPT_DISCRIMINATOR.length)).equals(Buffer.from(STRATEGY_INIT_RECEIPT_DISCRIMINATOR)), Buffer.from(input.strategyReceipt.data.subarray(0, STRATEGY_INIT_RECEIPT_DISCRIMINATOR.length)).toString("hex"), Buffer.from(STRATEGY_INIT_RECEIPT_DISCRIMINATOR).toString("hex"));
  add(gates, "metadata owner program", input.userMetadata.owner === route.programs.klend, input.userMetadata.owner, route.programs.klend);
  add(gates, "farm state owner program", input.obligationFarm.owner === route.programs.farms, input.obligationFarm.owner, route.programs.farms);
  let strategyPosition: bigint | null = null;
  try {
    const receipt = getStrategyInitReceiptDecoder().decode(input.strategyReceipt.data);
    add(gates, "strategy receipt vault", receipt.vault === route.vault, receipt.vault, route.vault);
    add(gates, "strategy receipt reserve", receipt.strategy === route.strategy.reserve, receipt.strategy, route.strategy.reserve);
    add(gates, "strategy receipt adaptor", receipt.adaptorProgram === route.programs.kaminoAdaptor, receipt.adaptorProgram, route.programs.kaminoAdaptor);
    strategyPosition = receipt.positionValue;
    add(gates, "strategy receipt position decodes", true, receipt.positionValue, "non-negative raw position");
  } catch (error) {
    add(gates, "strategy receipt decodes", false, error instanceof Error ? error.message : String(error), "decoded");
  }
  try {
    const metadata = UserMetadata.decode(Buffer.from(input.userMetadata.data));
    add(gates, "metadata authority", metadata.owner.toString() === accounts.strategyAuth, metadata.owner.toString(), accounts.strategyAuth);
  } catch (error) {
    add(gates, "metadata decodes", false, error instanceof Error ? error.message : String(error), "decoded");
  }
  const flatUnwound = strategyPosition === 0n;
  add(gates, "Kamino obligation exists or strategy is exactly flat", input.obligation !== null || flatUnwound, input.obligation?.address ?? null, input.obligation !== null ? graph.obligation : "absent allowed only at position=0");
  if (input.obligation) {
    add(gates, "obligation owner program", input.obligation.owner === route.programs.klend, input.obligation.owner, route.programs.klend);
    try {
      const obligation = Obligation.decode(Buffer.from(input.obligation.data));
      add(gates, "obligation authority", obligation.owner.toString() === accounts.strategyAuth, obligation.owner.toString(), accounts.strategyAuth);
      add(gates, "obligation Main market", obligation.lendingMarket.toString() === route.strategy.lendingMarket, obligation.lendingMarket.toString(), route.strategy.lendingMarket);
    } catch (error) {
      add(gates, "obligation decodes", false, error instanceof Error ? error.message : String(error), "decoded");
    }
  }
  try {
    const farm = UserState.decode(Buffer.from(input.obligationFarm.data));
    add(gates, "farm authority", farm.owner.toString() === accounts.strategyAuth, farm.owner.toString(), accounts.strategyAuth);
    add(gates, "farm Main state", farm.farmState.toString() === route.strategy.collateralFarm, farm.farmState.toString(), route.strategy.collateralFarm);
  } catch (error) {
    add(gates, "farm state decodes", false, error instanceof Error ? error.message : String(error), "decoded");
  }
  return gates;
}

function add(
  gates: Gate[],
  name: string,
  pass: boolean,
  observed: unknown,
  expected: unknown,
): void {
  gates.push({ name, pass, observed, expected });
}

function fixedUtf8(value: ArrayLike<number>): string {
  const bytes = Uint8Array.from(value);
  const zero = bytes.indexOf(0);
  return Buffer.from(zero < 0 ? bytes : bytes.subarray(0, zero)).toString("utf8");
}

function optionAddress(value: unknown): string | null {
  if (!value || typeof value !== "object") return null;
  const record = value as { __option?: string; value?: unknown };
  return record.__option === "Some" && typeof record.value === "string"
    ? record.value
    : null;
}

export function verifyVaultCurrentState(input: Readonly<{
  route: PartnerRouteSpec;
  accounts: VoltrAccounts;
  vault: AccountSnapshot | null;
  lpMint: AccountSnapshot | null;
  idleAta: AccountSnapshot | null;
  assetMint: AccountSnapshot | null;
  requireEmpty?: boolean;
  requireIdleOnly?: boolean;
}>): VaultStateReport {
  const { route, accounts } = input;
  const gates: Gate[] = [];
  add(gates, "vault exists", input.vault !== null, input.vault?.address ?? null, route.vault);
  add(gates, "LP mint exists", input.lpMint !== null, input.lpMint?.address ?? null, accounts.lpMint);
  add(gates, "idle ATA exists", input.idleAta !== null, input.idleAta?.address ?? null, accounts.idleAta);
  add(gates, "asset mint exists", input.assetMint !== null, input.assetMint?.address ?? null, route.asset.mint);
  if (!input.vault || !input.lpMint || !input.idleAta || !input.assetMint) {
    return {
      verdict: "PARTNER_VAULT_CURRENT_FAIL",
      failedGateCount: gates.filter(({ pass }) => !pass).length,
      gates,
      state: null,
    };
  }
  add(gates, "vault owner", input.vault.owner === route.programs.voltrVault, input.vault.owner, route.programs.voltrVault);
  add(gates, "vault is not executable", !input.vault.executable, input.vault.executable, false);
  add(gates, "vault data length", input.vault.data.length === getVaultSize(), input.vault.data.length, getVaultSize());
  add(
    gates,
    "vault discriminator",
    Buffer.from(input.vault.data.subarray(0, VAULT_DISCRIMINATOR.length))
      .equals(Buffer.from(VAULT_DISCRIMINATOR)),
    Buffer.from(input.vault.data.subarray(0, VAULT_DISCRIMINATOR.length)).toString("hex"),
    Buffer.from(VAULT_DISCRIMINATOR).toString("hex"),
  );
  add(gates, "LP mint token program", input.lpMint.owner === route.programs.token, input.lpMint.owner, route.programs.token);
  add(gates, "idle ATA token program", input.idleAta.owner === route.programs.token, input.idleAta.owner, route.programs.token);
  add(gates, "asset mint token program", input.assetMint.owner === route.programs.token, input.assetMint.owner, route.programs.token);
  if (gates.some(({ pass }) => !pass)) {
    return {
      verdict: "PARTNER_VAULT_CURRENT_FAIL",
      failedGateCount: gates.filter(({ pass }) => !pass).length,
      gates,
      state: null,
    };
  }
  const vault = getVaultDecoder().decode(input.vault.data);
  const lpMint = getMintDecoder().decode(input.lpMint.data);
  const idle = getTokenDecoder().decode(input.idleAta.data);
  const assetMint = getMintDecoder().decode(input.assetMint.data);
  for (const [name, observed, expected] of [
    ["vault name", fixedUtf8(vault.name), route.vaultConfiguration.name],
    ["vault description", fixedUtf8(vault.description), route.vaultConfiguration.description],
    ["vault admin", vault.admin, route.setupAdmin],
    ["vault pending admin", vault.pendingAdmin, route.setupAdmin],
    ["vault manager", vault.manager, route.squads.manager],
    ["asset mint", vault.asset.mint, route.asset.mint],
    ["idle ATA", vault.asset.idleAta, accounts.idleAta],
    ["LP mint", vault.lp.mint, accounts.lpMint],
    ["maximum cap", vault.vaultConfiguration.maxCap, route.asset.vaultCapRaw],
    ["start timestamp", vault.vaultConfiguration.startAtTs, route.vaultConfiguration.startAtTs],
    ["locked-profit duration", vault.vaultConfiguration.lockedProfitDegradationDuration, route.vaultConfiguration.lockedProfitDegradationDurationSeconds],
    ["withdrawal waiting period", vault.vaultConfiguration.withdrawalWaitingPeriod, route.vaultConfiguration.withdrawalWaitingPeriodSeconds],
    ["disabled operations", vault.vaultConfiguration.disabledOperations, route.vaultConfiguration.disabledOperations],
    ["manager performance fee", vault.feeConfiguration.managerPerformanceFee, route.vaultConfiguration.managerPerformanceFeeBps],
    ["admin performance fee", vault.feeConfiguration.adminPerformanceFee, route.vaultConfiguration.adminPerformanceFeeBps],
    ["manager management fee", vault.feeConfiguration.managerManagementFee, route.vaultConfiguration.managerManagementFeeBps],
    ["admin management fee", vault.feeConfiguration.adminManagementFee, route.vaultConfiguration.adminManagementFeeBps],
    ["redemption fee", vault.feeConfiguration.redemptionFee, route.vaultConfiguration.redemptionFeeBps],
    ["issuance fee", vault.feeConfiguration.issuanceFee, route.vaultConfiguration.issuanceFeeBps],
    ["adaptor bypass disabled", vault.allowAnyAdaptor, route.vaultConfiguration.allowAnyAdaptor],
    ["LP mint authority", optionAddress(lpMint.mintAuthority), accounts.lpMintAuth],
    ["LP mint decimals", lpMint.decimals, 9],
    ["asset mint decimals", assetMint.decimals, route.asset.decimals],
    ["idle ATA mint", idle.mint, route.asset.mint],
    ["idle ATA owner", idle.owner, accounts.idleAuth],
  ] as const) {
    add(gates, name, observed === expected, observed, expected);
  }
  add(
    gates,
    "idle ATA does not exceed vault total value",
    idle.amount <= vault.asset.totalValue,
    { idleAmount: idle.amount, totalValue: vault.asset.totalValue },
    "idleAmount <= totalValue",
  );
  if (input.requireIdleOnly || input.requireEmpty) {
    add(
      gates,
      "idle-only vault amount matches total value",
      idle.amount === vault.asset.totalValue,
      idle.amount,
      vault.asset.totalValue,
    );
  }
  if (input.requireEmpty) {
    for (const [name, observed] of [
      ["empty vault total value", vault.asset.totalValue],
      ["empty LP supply", lpMint.supply],
      ["empty idle ATA", idle.amount],
      ["empty manager fees", vault.feeState.accumulatedLpManagerFees],
      ["empty admin fees", vault.feeState.accumulatedLpAdminFees],
      ["empty protocol fees", vault.feeState.accumulatedLpProtocolFees],
      ["empty dead weight", vault.deadWeight],
    ] as const) {
      add(gates, name, observed === 0n, observed, 0n);
    }
  }
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return {
    verdict: failedGateCount === 0 ? "PARTNER_VAULT_CURRENT_PASS" : "PARTNER_VAULT_CURRENT_FAIL",
    failedGateCount,
    gates,
    state: {
      vaultDataSha256: createHash("sha256").update(input.vault.data).digest("hex"),
      manager: vault.manager,
      admin: vault.admin,
      pendingAdmin: vault.pendingAdmin,
      waitingPeriodSeconds: vault.vaultConfiguration.withdrawalWaitingPeriod,
      totalValueRaw: vault.asset.totalValue,
      lpSupplyRaw: lpMint.supply,
      idleRaw: idle.amount,
    },
  };
}
