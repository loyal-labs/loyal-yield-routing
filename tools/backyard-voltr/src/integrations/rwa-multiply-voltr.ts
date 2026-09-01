import {
  AccountRole,
  address,
  createNoopSigner,
  getAddressEncoder,
  type Address,
  type Instruction,
  type TransactionSigner,
} from "@solana/kit";
import { PublicKey } from "@solana/web3.js";
import {
  findAssociatedTokenPda,
  getCreateAssociatedTokenIdempotentInstructionAsync,
  getApproveCheckedInstruction,
  getTransferCheckedInstruction,
} from "@solana-program/token";
import {
  findAdaptorAddReceiptPda,
  findProtocolPda,
  findStrategyInitReceiptPda,
  findVaultAssetIdleAuthPda,
  findVaultLpMintAuthPda,
  findVaultLpMintPda,
  findVaultStrategyAuthPda,
  getAddAdaptorInstructionAsync,
  getDepositStrategyInstructionAsync,
  getInitializeStrategyInstructionAsync,
  getInitializeVaultInstructionAsync,
  getUpdateVaultConfigInstructionAsync,
  getWithdrawStrategyInstructionAsync,
  VaultConfigField,
} from "@voltr/vault-sdk";

import {
  RWA_MULTIPLY_ROUTE,
  type RwaMultiplyRouteSpec,
} from "../domain/rwa-multiply-route-spec.js";

const ADDRESS_ENCODER = getAddressEncoder();

export const RWA_ADAPTOR_DISCRIMINATORS = {
  initializeConfig: Uint8Array.from([208, 127, 21, 1, 194, 190, 196, 70]),
  initializeReportTicket: Uint8Array.from([124, 41, 223, 13, 165, 246, 70, 62]),
  armReport: Uint8Array.from([164, 175, 246, 41, 178, 140, 35, 3]),
  initialize: Uint8Array.from([175, 175, 109, 31, 13, 152, 155, 237]),
  deposit: Uint8Array.from([242, 35, 198, 137, 82, 225, 242, 182]),
  withdraw: Uint8Array.from([183, 18, 70, 156, 148, 109, 161, 34]),
} as const;

export type RwaMultiplyVoltrAccounts = Readonly<{
  protocol: Address;
  idleAuth: Address;
  idleAta: Address;
  lpMint: Address;
  lpMintAuth: Address;
  adaptorAddReceipt: Address;
  strategyInitReceipt: Address;
  strategyAuth: Address;
  strategyAssetAta: Address;
  reportTicket: Address;
}>;

function appendAccounts(
  instruction: Instruction,
  accounts: readonly Readonly<{ address: Address; role: AccountRole }>[],
): Instruction {
  return {
    ...instruction,
    accounts: [...(instruction.accounts ?? []), ...accounts],
  };
}

function readonly(account: Address) {
  return { address: account, role: AccountRole.READONLY } as const;
}

function readonlySigner(account: Address) {
  return { address: account, role: AccountRole.READONLY_SIGNER } as const;
}

function writable(account: Address) {
  return { address: account, role: AccountRole.WRITABLE } as const;
}

function requireSigner(
  signer: TransactionSigner,
  expected: Address,
  label: string,
): void {
  if (signer.address !== expected) {
    throw new Error(`${label} signer ${signer.address} does not match ${expected}`);
  }
}

export async function deriveRwaMultiplyVoltrAccounts(
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<RwaMultiplyVoltrAccounts> {
  const [protocol] = await findProtocolPda({ programAddress: route.programs.voltr });
  const [idleAuth] = await findVaultAssetIdleAuthPda(
    { vault: route.vault.address },
    { programAddress: route.programs.voltr },
  );
  const [lpMint] = await findVaultLpMintPda(
    { vault: route.vault.address },
    { programAddress: route.programs.voltr },
  );
  const [lpMintAuth] = await findVaultLpMintAuthPda(
    { vault: route.vault.address },
    { programAddress: route.programs.voltr },
  );
  const [idleAta] = await findAssociatedTokenPda({
    owner: idleAuth,
    mint: route.assets.assetMint,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const [adaptorAddReceipt] = await findAdaptorAddReceiptPda(
    { vault: route.vault.address, adaptorProgram: route.customAdaptor.program },
    { programAddress: route.programs.voltr },
  );
  const [strategyInitReceipt] = await findStrategyInitReceiptPda(
    { vault: route.vault.address, strategy: route.customAdaptor.strategyConfig },
    { programAddress: route.programs.voltr },
  );
  const [strategyAuth] = await findVaultStrategyAuthPda(
    { vault: route.vault.address, strategy: route.customAdaptor.strategyConfig },
    { programAddress: route.programs.voltr },
  );
  const [strategyAssetAta] = await findAssociatedTokenPda({
    owner: strategyAuth,
    mint: route.assets.assetMint,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const [reportTicket] = PublicKey.findProgramAddressSync([
    Buffer.from("report_ticket"),
    new PublicKey(route.customAdaptor.strategyConfig).toBuffer(),
  ], new PublicKey(route.customAdaptor.program));
  return {
    protocol,
    idleAuth,
    idleAta,
    lpMint,
    lpMintAuth,
    adaptorAddReceipt,
    strategyInitReceipt,
    strategyAuth,
    strategyAssetAta,
    reportTicket: address(reportTicket.toBase58()),
  };
}

export async function initializeRwaAdaptorReportTicketInstruction(
  payer: TransactionSigner,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<Instruction> {
  requireSigner(payer, route.setupAdmin, "ticket setup payer");
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  return {
    programAddress: route.customAdaptor.program,
    accounts: [
      { address: payer.address, role: AccountRole.WRITABLE_SIGNER },
      readonly(route.customAdaptor.strategyConfig),
      writable(accounts.reportTicket),
      readonly(route.programs.system),
    ],
    data: RWA_ADAPTOR_DISCRIMINATORS.initializeReportTicket,
  };
}

export function initializeRwaAdaptorConfigInstruction(
  payer: TransactionSigner,
  strategyConfig: TransactionSigner,
  accounts: RwaMultiplyVoltrAccounts,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Instruction {
  requireSigner(payer, route.setupAdmin, "setup payer");
  requireSigner(strategyConfig, route.customAdaptor.strategyConfig, "strategy config");
  return {
    programAddress: route.customAdaptor.program,
    accounts: [
      { address: payer.address, role: AccountRole.WRITABLE_SIGNER },
      { address: strategyConfig.address, role: AccountRole.WRITABLE_SIGNER },
      readonly(route.programs.voltr),
      readonly(route.vault.address),
      readonly(accounts.strategyAuth),
      readonly(route.squads.program),
      readonly(route.squads.settings),
      readonly(route.customAdaptor.settingsSigner),
      readonly(route.squads.vault),
      readonly(route.assets.assetMint),
      readonly(route.assets.tokenProgram),
      readonly(route.squads.assetAta),
      readonly(route.programs.system),
    ],
    data: Uint8Array.from([
      ...RWA_ADAPTOR_DISCRIMINATORS.initializeConfig,
      route.squads.vaultIndex,
      ...u64Le(route.customAdaptor.maxReportedNavRaw),
      ...u64Le(route.customAdaptor.maxReportAgeSlots),
    ]),
  };
}

function u64Le(value: bigint): Uint8Array {
  if (value < 0n || value > (1n << 64n) - 1n) {
    throw new Error(`u64 value out of range: ${value}`);
  }
  const out = new Uint8Array(8);
  let remaining = value;
  for (let index = 0; index < out.length; index += 1) {
    out[index] = Number(remaining & 0xffn);
    remaining >>= 8n;
  }
  return out;
}

function u32Le(value: number): Uint8Array {
  const output = new Uint8Array(4);
  new DataView(output.buffer).setUint32(0, value, true);
  return output;
}

export type RwaReportV1 = Readonly<{
  sequence: bigint;
  observedSlot: bigint;
  navAfterRaw: bigint;
  snapshotDigest: Uint8Array;
}>;

export function encodeRwaReportV1(report: RwaReportV1): Uint8Array {
  if (report.snapshotDigest.length !== 32 || report.snapshotDigest.every((value) => value === 0)) {
    throw new Error("ReportV1 digest must be exactly 32 nonzero bytes");
  }
  return Uint8Array.from([
    1,
    ...u64Le(report.sequence),
    ...u64Le(report.observedSlot),
    ...u64Le(report.navAfterRaw),
    ...report.snapshotDigest,
  ]);
}

function initializeRemainingAccounts(route: RwaMultiplyRouteSpec) {
  return [
    readonly(route.squads.settings),
    readonly(route.squads.vault),
    readonly(route.assets.assetMint),
    readonly(route.assets.tokenProgram),
    readonly(route.squads.assetAta),
    readonly(route.squads.program),
  ] as const;
}

function positionRemainingAccounts(
  route: RwaMultiplyRouteSpec,
  accounts: RwaMultiplyVoltrAccounts,
) {
  return [
    readonly(route.squads.settings),
    readonlySigner(route.squads.vault),
    writable(route.squads.assetAta),
    writable(accounts.reportTicket),
  ] as const;
}


export async function buildRwaMultiplyArmReportInstruction(
  manager: TransactionSigner,
  operation: "deposit" | "withdraw",
  amountRaw: bigint,
  report: RwaReportV1,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<Instruction> {
  requireSigner(manager, route.squads.vault, "Squads manager");
  if (amountRaw < 0n || amountRaw > route.vault.capRaw) {
    throw new Error(`amount must be in 0..${route.vault.capRaw}`);
  }
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const encodedReport = encodeRwaReportV1(report);
  return {
    programAddress: route.customAdaptor.program,
    accounts: [
      readonly(route.customAdaptor.strategyConfig),
      writable(accounts.reportTicket),
      readonly(route.squads.settings),
      readonlySigner(route.squads.vault),
      readonly(route.squads.program),
    ],
    data: Uint8Array.from([
      ...RWA_ADAPTOR_DISCRIMINATORS.armReport,
      operation === "deposit" ? 0 : 1,
      ...u64Le(amountRaw),
      1,
      ...u32Le(encodedReport.length),
      ...encodedReport,
    ]),
  };
}

export async function buildRwaMultiplyVoltrSetup(
  signers: Readonly<{
    admin: TransactionSigner;
    vault: TransactionSigner;
    strategyConfig: TransactionSigner;
  }>,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
) {
  requireSigner(signers.admin, route.setupAdmin, "setup admin");
  requireSigner(signers.vault, route.vault.address, "vault");
  requireSigner(signers.strategyConfig, route.customAdaptor.strategyConfig, "strategy config");
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const initializeVault = await getInitializeVaultInstructionAsync({
    payer: signers.admin,
    manager: route.setupAdmin,
    admin: signers.admin.address,
    vault: signers.vault,
    vaultAssetMint: route.assets.assetMint,
    vaultAssetIdleAta: accounts.idleAta,
    assetTokenProgram: route.assets.tokenProgram,
    maxCap: route.vault.capRaw,
    startAtTs: 0n,
    managerPerformanceFee: 0,
    adminPerformanceFee: 500,
    managerManagementFee: 0,
    adminManagementFee: 0,
    lockedProfitDegradationDuration: 86_400n,
    redemptionFee: 0,
    issuanceFee: 0,
    withdrawalWaitingPeriod: route.vault.withdrawalWaitingPeriodSeconds,
    name: route.vault.name,
    description: route.vault.description,
  }, { programAddress: route.programs.voltr });
  const addAdaptor = await getAddAdaptorInstructionAsync({
    payer: signers.admin,
    admin: signers.admin,
    vault: route.vault.address,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    adaptorProgram: route.customAdaptor.program,
  }, { programAddress: route.programs.voltr });
  const initializeConfig = initializeRwaAdaptorConfigInstruction(
    signers.admin,
    signers.strategyConfig,
    accounts,
    route,
  );
  const initializeReportTicket = await initializeRwaAdaptorReportTicketInstruction(
    signers.admin,
    route,
  );
  const initializeStrategyBase = await getInitializeStrategyInstructionAsync({
    payer: signers.admin,
    manager: signers.admin,
    vault: route.vault.address,
    strategy: route.customAdaptor.strategyConfig,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    strategyInitReceipt: accounts.strategyInitReceipt,
    vaultStrategyAuth: accounts.strategyAuth,
    adaptorProgram: route.customAdaptor.program,
    instructionDiscriminator: RWA_ADAPTOR_DISCRIMINATORS.initialize,
    additionalArgs: null,
  }, { programAddress: route.programs.voltr });
  const initializeStrategy = appendAccounts(
    initializeStrategyBase,
    initializeRemainingAccounts(route),
  );
  const createStrategyAssetAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: signers.admin,
    ata: accounts.strategyAssetAta,
    owner: accounts.strategyAuth,
    mint: route.assets.assetMint,
    systemProgram: route.programs.system,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const handoffManager = await getUpdateVaultConfigInstructionAsync({
    admin: signers.admin,
    vault: route.vault.address,
    field: VaultConfigField.Manager,
    data: ADDRESS_ENCODER.encode(route.squads.vault),
  }, { programAddress: route.programs.voltr });
  return {
    accounts,
    instructions: {
      initializeVault,
      addAdaptor,
      initializeConfig,
      initializeReportTicket,
      createStrategyAssetAta,
      initializeStrategy,
      handoffManager,
    },
  } as const;
}

export async function buildRwaMultiplyManagerInstructions(
  manager: TransactionSigner,
  amountRaw: bigint,
  report: RwaReportV1,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
) {
  requireSigner(manager, route.squads.vault, "Squads manager");
  if (amountRaw < 0n || amountRaw > route.vault.capRaw) {
    throw new Error(`amount must be in 0..${route.vault.capRaw}`);
  }
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const common = {
    manager,
    vault: route.vault.address,
    strategy: route.customAdaptor.strategyConfig,
    vaultAssetMint: route.assets.assetMint,
    vaultAssetIdleAuth: accounts.idleAuth,
    vaultStrategyAuth: accounts.strategyAuth,
    vaultLpMint: accounts.lpMint,
    vaultAssetIdleAta: accounts.idleAta,
    vaultStrategyAssetAta: accounts.strategyAssetAta,
    assetTokenProgram: route.assets.tokenProgram,
    adaptorProgram: route.customAdaptor.program,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    strategyInitReceipt: accounts.strategyInitReceipt,
    amount: amountRaw,
    additionalArgs: encodeRwaReportV1(report),
  } as const;
  const depositBase = await getDepositStrategyInstructionAsync({
    ...common,
    instructionDiscriminator: RWA_ADAPTOR_DISCRIMINATORS.deposit,
  }, { programAddress: route.programs.voltr });
  const withdrawBase = await getWithdrawStrategyInstructionAsync({
    ...common,
    instructionDiscriminator: RWA_ADAPTOR_DISCRIMINATORS.withdraw,
  }, { programAddress: route.programs.voltr });
  return {
    accounts,
    deposit: appendAccounts(depositBase, positionRemainingAccounts(route, accounts)),
    withdraw: appendAccounts(withdrawBase, positionRemainingAccounts(route, accounts)),
  } as const;
}

/**
 * One-time Squads policy payload. Squads makes its vault PDA the signer and
 * grants only the immutable Voltr strategy PDA permission to pull USDC back
 * during adaptor withdraw. This instruction does not install or execute a
 * policy by itself.
 */
export async function buildRwaMultiplyBridgeApprovalInstruction(
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<Instruction> {
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  return getApproveCheckedInstruction({
    source: route.squads.assetAta,
    mint: route.assets.assetMint,
    delegate: accounts.strategyAuth,
    owner: createNoopSigner(route.squads.vault),
    amount: (1n << 64n) - 1n,
    decimals: route.assets.decimals,
  }, { programAddress: route.assets.tokenProgram });
}

export async function buildRwaMultiplyWithdrawalStagingInstruction(
  manager: TransactionSigner,
  amountRaw: bigint,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<Instruction> {
  requireSigner(manager, route.squads.vault, "Squads manager");
  if (amountRaw <= 0n || amountRaw > route.vault.capRaw) {
    throw new Error(`staging amount must be in 1..${route.vault.capRaw}`);
  }
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  return getTransferCheckedInstruction({
    source: route.squads.assetAta,
    mint: route.assets.assetMint,
    destination: accounts.strategyAssetAta,
    authority: manager,
    amount: amountRaw,
    decimals: route.assets.decimals,
  }, { programAddress: route.assets.tokenProgram });
}
