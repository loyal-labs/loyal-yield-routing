import { createHash } from "node:crypto";

import {
  AccountRole,
  address,
  getAddressEncoder,
  isSignerRole,
  isWritableRole,
  type Address,
  type Instruction,
  type TransactionSigner,
} from "@solana/kit";
import { findAssociatedTokenPda } from "@solana-program/token";
import {
  findAdaptorAddReceiptPda,
  findProtocolPda,
  findRequestWithdrawVaultReceiptPda,
  findStrategyInitReceiptPda,
  findVaultAssetIdleAuthPda,
  findVaultLpMintAuthPda,
  findVaultLpMintPda,
  findVaultStrategyAuthPda,
  getAddAdaptorInstructionAsync,
  getCancelRequestWithdrawVaultInstructionAsync,
  getDepositStrategyInstructionAsync,
  getDepositVaultInstructionAsync,
  getInitializeStrategyInstructionAsync,
  getInitializeVaultInstructionAsync,
  getInstantWithdrawVaultInstructionAsync,
  getRequestWithdrawVaultInstructionAsync,
  getUpdateVaultConfigInstructionAsync,
  getWithdrawStrategyInstructionAsync,
  getWithdrawVaultInstructionAsync,
  VaultConfigField,
} from "@voltr/vault-sdk";

import type { PartnerRouteSpec } from "../domain/route-spec.js";

const ADDRESS_ENCODER = getAddressEncoder();

const KAMINO_INITIALIZE_DISCRIMINATOR = Uint8Array.from([
  35, 35, 189, 193, 155, 48, 170, 203,
]);
const KAMINO_DEPOSIT_DISCRIMINATOR = Uint8Array.from([
  212, 53, 186, 193, 147, 53, 143, 123,
]);
const KAMINO_WITHDRAW_DISCRIMINATOR = Uint8Array.from([
  123, 109, 245, 15, 150, 48, 203, 113,
]);

const INITIALIZE_LABELS = [
  "payer",
  "manager",
  "protocol",
  "vault",
  "strategy",
  "adaptorAddReceipt",
  "strategyInitReceipt",
  "vaultStrategyAuth",
  "adaptorProgram",
  "systemProgram",
  "kaminoUserMetadata",
  "kaminoObligation",
  "lendingMarketAuthority",
  "reserve",
  "reserveFarmState",
  "obligationFarm",
  "lendingMarket",
  "farmsProgram",
  "rentSysvar",
  "klendProgram",
] as const;

const DEPOSIT_LABELS = [
  "manager",
  "protocol",
  "vault",
  "strategy",
  "adaptorAddReceipt",
  "strategyInitReceipt",
  "vaultAssetIdleAuth",
  "vaultStrategyAuth",
  "vaultAssetMint",
  "vaultLpMint",
  "vaultAssetIdleAta",
  "vaultStrategyAssetAta",
  "assetTokenProgram",
  "adaptorProgram",
  "kaminoObligation",
  "lendingMarket",
  "lendingMarketAuthority",
  "reserve",
  "reserveLiquiditySupply",
  "reserveCollateralMint",
  "reserveCollateralSupplyVault",
  "tokenProgram",
  "instructionsSysvar",
  "obligationFarm",
  "reserveFarmState",
  "userMetadata",
  "scope",
  "rentSysvar",
  "systemProgram",
  "farmsProgram",
  "klendProgram",
] as const;

const WITHDRAW_LABELS = [
  "manager",
  "protocol",
  "vault",
  "adaptorAddReceipt",
  "strategyInitReceipt",
  "strategy",
  "adaptorProgram",
  "vaultAssetIdleAuth",
  "vaultStrategyAuth",
  "vaultAssetMint",
  "vaultLpMint",
  "vaultAssetIdleAta",
  "vaultStrategyAssetAta",
  "assetTokenProgram",
  "kaminoObligation",
  "lendingMarket",
  "lendingMarketAuthority",
  "reserve",
  "reserveCollateralSupplyVault",
  "reserveCollateralMint",
  "reserveLiquiditySupply",
  "tokenProgram",
  "instructionsSysvar",
  "obligationFarm",
  "reserveFarmState",
  "scope",
  "farmsProgram",
  "klendProgram",
] as const;

const INITIALIZE_VAULT_LABELS = [
  "payer",
  "manager",
  "admin",
  "protocol",
  "vault",
  "vaultLpMint",
  "vaultAssetMint",
  "vaultAssetIdleAta",
  "vaultLpMintAuth",
  "vaultAssetIdleAuth",
  "clock",
  "rent",
  "associatedTokenProgram",
  "assetTokenProgram",
  "lpTokenProgram",
  "systemProgram",
] as const;

const ADD_ADAPTOR_LABELS = [
  "payer",
  "admin",
  "protocol",
  "vault",
  "adaptorAddReceipt",
  "adaptorProgram",
  "systemProgram",
] as const;

const DEPOSIT_VAULT_LABELS = [
  "userTransferAuthority",
  "protocol",
  "vault",
  "vaultAssetMint",
  "vaultLpMint",
  "userAssetAta",
  "vaultAssetIdleAta",
  "vaultAssetIdleAuth",
  "userLpAta",
  "vaultLpMintAuth",
  "assetTokenProgram",
  "lpTokenProgram",
  "systemProgram",
] as const;

const REQUEST_WITHDRAW_LABELS = [
  "payer",
  "userTransferAuthority",
  "protocol",
  "vault",
  "vaultLpMint",
  "userLpAta",
  "requestWithdrawLpAta",
  "requestWithdrawVaultReceipt",
  "lpTokenProgram",
  "systemProgram",
] as const;

const CANCEL_WITHDRAW_LABELS = [
  "userTransferAuthority",
  "protocol",
  "vault",
  "vaultLpMint",
  "userLpAta",
  "requestWithdrawLpAta",
  "requestWithdrawVaultReceipt",
  "lpTokenProgram",
  "systemProgram",
] as const;

const CLAIM_WITHDRAW_LABELS = [
  "userTransferAuthority",
  "protocol",
  "vault",
  "vaultAssetMint",
  "vaultLpMint",
  "requestWithdrawLpAta",
  "vaultAssetIdleAta",
  "vaultAssetIdleAuth",
  "userAssetAta",
  "requestWithdrawVaultReceipt",
  "assetTokenProgram",
  "lpTokenProgram",
  "systemProgram",
] as const;

const INSTANT_WITHDRAW_LABELS = [
  "userTransferAuthority",
  "protocol",
  "vault",
  "vaultAssetMint",
  "vaultLpMint",
  "userLpAta",
  "vaultAssetIdleAta",
  "vaultAssetIdleAuth",
  "userAssetAta",
  "assetTokenProgram",
  "lpTokenProgram",
  "systemProgram",
] as const;

/** The live Kamino account graph needed by the Voltr adaptor. */
export type ReserveGraph = Readonly<{
  reserve: Address;
  lendingMarket: Address;
  lendingMarketAuthority: Address;
  obligation: Address;
  userMetadata: Address;
  reserveLiquiditySupply: Address;
  reserveCollateralMint: Address;
  reserveCollateralSupplyVault: Address;
  scope: Address;
  reserveFarmState: Address;
  obligationFarm: Address;
}>;

export type CanonicalAccount = Readonly<{
  index: number;
  label: string;
  address: Address;
  signer: boolean;
  writable: boolean;
}>;

export type CanonicalInstruction = Readonly<{
  programId: Address;
  data: Uint8Array;
  dataBase64: string;
  dataSha256: string;
  dataLength: number;
  accounts: readonly CanonicalAccount[];
}>;

export type VoltrInstruction = Readonly<{
  raw: Instruction;
  canonical: CanonicalInstruction;
}>;

export type VoltrAccounts = Readonly<{
  protocol: Address;
  idleAuth: Address;
  idleAta: Address;
  lpMint: Address;
  lpMintAuth: Address;
  adaptorAddReceipt: Address;
  strategyInitReceipt: Address;
  strategyAuth: Address;
}>;

export type UserVoltrAccounts = VoltrAccounts & Readonly<{
  userAssetAta: Address;
  userLpAta: Address;
  requestWithdrawLpAta: Address;
  requestWithdrawVaultReceipt: Address;
}>;

export type SetupSigners = Readonly<{
  payer: TransactionSigner;
  admin: TransactionSigner;
  vault: TransactionSigner;
}>;

export type UserSigners = Readonly<{
  user: TransactionSigner;
  payer?: TransactionSigner;
}>;

type MetaSpec = Readonly<{
  label: string;
  address: Address;
  role: AccountRole;
}>;

function digest(data: Uint8Array): string {
  return createHash("sha256").update(data).digest("hex");
}

function base64(data: Uint8Array): string {
  return Buffer.from(data).toString("base64");
}

function labelled(
  instruction: Instruction,
  labels: readonly string[],
): VoltrInstruction {
  const accounts = instruction.accounts ?? [];
  if (accounts.length !== labels.length) {
    throw new Error(`Voltr account label count ${labels.length} does not match ${accounts.length}`);
  }
  const data = new Uint8Array(instruction.data ?? []);
  return {
    raw: instruction,
    canonical: {
      programId: instruction.programAddress,
      data,
      dataBase64: base64(data),
      dataSha256: digest(data),
      dataLength: data.length,
      accounts: accounts.map((meta, index) => ({
        index,
        label: labels[index]!,
        address: meta.address,
        signer: isSignerRole(meta.role),
        writable: isWritableRole(meta.role),
      })),
    },
  };
}

function appendRemaining(
  instruction: Instruction,
  metas: readonly MetaSpec[],
): Instruction {
  return {
    ...instruction,
    accounts: [
      ...(instruction.accounts ?? []),
      ...metas.map(({ address: account, role }) => ({ address: account, role })),
    ],
  };
}

function writable(label: string, account: Address): MetaSpec {
  return { label, address: account, role: AccountRole.WRITABLE };
}

function readonly(label: string, account: Address): MetaSpec {
  return { label, address: account, role: AccountRole.READONLY };
}

function assertRouteGraph(route: PartnerRouteSpec, graph: ReserveGraph): void {
  if (graph.reserve !== route.strategy.reserve) {
    throw new Error(`reserve graph ${graph.reserve} is not the route reserve ${route.strategy.reserve}`);
  }
  if (graph.lendingMarket !== route.strategy.lendingMarket) {
    throw new Error("reserve graph lending market is not the route market");
  }
  if (graph.reserveFarmState !== route.strategy.collateralFarm) {
    throw new Error("reserve graph farm is not the route collateral farm");
  }
}

export async function deriveVoltrAccountsForStrategy(
  route: PartnerRouteSpec,
  strategyReserve: Address,
): Promise<VoltrAccounts> {
  const [protocol] = await findProtocolPda({ programAddress: route.programs.voltrVault });
  const [idleAuth] = await findVaultAssetIdleAuthPda({
    vault: route.vault,
  }, { programAddress: route.programs.voltrVault });
  const [idleAta] = await findAssociatedTokenPda({
    owner: idleAuth,
    mint: route.asset.mint,
    tokenProgram: route.programs.token,
  }, { programAddress: route.programs.associatedToken });
  const [lpMint] = await findVaultLpMintPda({
    vault: route.vault,
  }, { programAddress: route.programs.voltrVault });
  const [lpMintAuth] = await findVaultLpMintAuthPda({
    vault: route.vault,
  }, { programAddress: route.programs.voltrVault });
  const [adaptorAddReceipt] = await findAdaptorAddReceiptPda({
    vault: route.vault,
    adaptorProgram: route.programs.kaminoAdaptor,
  }, { programAddress: route.programs.voltrVault });
  const [strategyInitReceipt] = await findStrategyInitReceiptPda({
    vault: route.vault,
    strategy: strategyReserve,
  }, { programAddress: route.programs.voltrVault });
  const [strategyAuth] = await findVaultStrategyAuthPda({
    vault: route.vault,
    strategy: strategyReserve,
  }, { programAddress: route.programs.voltrVault });
  return {
    protocol,
    idleAuth,
    idleAta,
    lpMint,
    lpMintAuth,
    adaptorAddReceipt,
    strategyInitReceipt,
    strategyAuth,
  };
}

export async function deriveVoltrAccounts(route: PartnerRouteSpec): Promise<VoltrAccounts> {
  return deriveVoltrAccountsForStrategy(route, route.strategy.reserve);
}

async function deriveUserAccounts(
  route: PartnerRouteSpec,
  accounts: VoltrAccounts,
  user: Address,
): Promise<UserVoltrAccounts> {
  const [userAssetAta] = await findAssociatedTokenPda({
    owner: user,
    mint: route.asset.mint,
    tokenProgram: route.programs.token,
  }, { programAddress: route.programs.associatedToken });
  const [userLpAta] = await findAssociatedTokenPda({
    owner: user,
    mint: accounts.lpMint,
    tokenProgram: route.programs.token,
  }, { programAddress: route.programs.associatedToken });
  const [requestWithdrawVaultReceipt] = await findRequestWithdrawVaultReceiptPda({
    vault: route.vault,
    userTransferAuthority: user,
  }, { programAddress: route.programs.voltrVault });
  const [requestWithdrawLpAta] = await findAssociatedTokenPda({
    owner: requestWithdrawVaultReceipt,
    mint: accounts.lpMint,
    tokenProgram: route.programs.token,
  }, { programAddress: route.programs.associatedToken });
  return {
    ...accounts,
    userAssetAta,
    userLpAta,
    requestWithdrawLpAta,
    requestWithdrawVaultReceipt,
  };
}

function requireSignerAddress(signer: TransactionSigner, expected: Address, label: string): void {
  if (signer.address !== expected) {
    throw new Error(`${label} signer ${signer.address} does not match ${expected}`);
  }
}

export type VoltrRouteBuilder = Readonly<{
  accounts: VoltrAccounts;
  userAccounts(user: Address): Promise<UserVoltrAccounts>;
  setup: Readonly<{
    initializeVault(signers: SetupSigners): Promise<VoltrInstruction>;
    addAdaptor(signers: SetupSigners): Promise<VoltrInstruction>;
    setManagerToAdmin(signers: SetupSigners): Promise<VoltrInstruction>;
    initializeStrategyAsAdmin(signers: SetupSigners): Promise<VoltrInstruction>;
    restoreManager(signers: SetupSigners): Promise<VoltrInstruction>;
  }>;
  strategy: Readonly<{
    deposit(manager: TransactionSigner, amountRaw?: bigint): Promise<VoltrInstruction>;
    withdraw(manager: TransactionSigner, amountRaw?: bigint): Promise<VoltrInstruction>;
  }>;
  user: Readonly<{
    deposit(signers: UserSigners, amountRaw?: bigint): Promise<VoltrInstruction>;
    requestWithdraw(signers: UserSigners, amountLpRaw: bigint, withdrawAll?: boolean): Promise<VoltrInstruction>;
    instantWithdraw(signers: UserSigners, amountLpRaw: bigint, withdrawAll?: boolean): Promise<VoltrInstruction>;
    cancelWithdraw(user: TransactionSigner): Promise<VoltrInstruction>;
    claimWithdraw(user: TransactionSigner): Promise<VoltrInstruction>;
  }>;
}>;

/**
 * Builds every supported Voltr instruction from one route graph. No RPC,
 * web3.js, signer secret, or transaction compilation is performed here.
 */
export async function createVoltrRouteBuilder(
  route: PartnerRouteSpec,
  graph: ReserveGraph,
): Promise<VoltrRouteBuilder> {
  assertRouteGraph(route, graph);
  const accounts = await deriveVoltrAccounts(route);
  const managerSigner = (signer: TransactionSigner) => {
    requireSignerAddress(signer, route.squads.manager, "manager");
  };
  const amount = (value: bigint | undefined, limit: bigint): bigint => {
    const result = value ?? route.asset.proofAmountRaw;
    if (result <= 0n || result > limit) throw new Error(`amount must be in the range 1..${limit}`);
    return result;
  };
  const initializeStrategy = async (
    payer: TransactionSigner,
    manager: TransactionSigner,
  ): Promise<VoltrInstruction> => {
    const instruction = await getInitializeStrategyInstructionAsync({
      payer,
      manager,
      vault: route.vault,
      strategy: route.strategy.reserve,
      adaptorAddReceipt: accounts.adaptorAddReceipt,
      strategyInitReceipt: accounts.strategyInitReceipt,
      vaultStrategyAuth: accounts.strategyAuth,
      adaptorProgram: route.programs.kaminoAdaptor,
      instructionDiscriminator: KAMINO_INITIALIZE_DISCRIMINATOR,
      additionalArgs: null,
    }, { programAddress: route.programs.voltrVault });
    const withRemaining = appendRemaining(instruction, [
      writable("kaminoUserMetadata", graph.userMetadata),
      writable("kaminoObligation", graph.obligation),
      readonly("lendingMarketAuthority", graph.lendingMarketAuthority),
      writable("reserve", graph.reserve),
      writable("reserveFarmState", graph.reserveFarmState),
      writable("obligationFarm", graph.obligationFarm),
      readonly("lendingMarket", graph.lendingMarket),
      readonly("farmsProgram", route.programs.farms),
      readonly("rentSysvar", address("SysvarRent111111111111111111111111111111111")),
      readonly("klendProgram", route.programs.klend),
    ]);
    return labelled(withRemaining, INITIALIZE_LABELS);
  };

  return {
    accounts,
    userAccounts: (user) => deriveUserAccounts(route, accounts, user),
    setup: {
      async initializeVault(signers) {
        requireSignerAddress(signers.payer, route.setupAdmin, "setup payer");
        requireSignerAddress(signers.admin, route.setupAdmin, "setup admin");
        requireSignerAddress(signers.vault, route.vault, "vault");
        const instruction = await getInitializeVaultInstructionAsync({
          payer: signers.payer,
          manager: route.squads.manager,
          admin: route.setupAdmin,
          vault: signers.vault,
          vaultAssetMint: route.asset.mint,
          vaultAssetIdleAta: accounts.idleAta,
          assetTokenProgram: route.programs.token,
          maxCap: route.asset.vaultCapRaw,
          startAtTs: route.vaultConfiguration.startAtTs,
          managerPerformanceFee: route.vaultConfiguration.managerPerformanceFeeBps,
          adminPerformanceFee: route.vaultConfiguration.adminPerformanceFeeBps,
          managerManagementFee: route.vaultConfiguration.managerManagementFeeBps,
          adminManagementFee: route.vaultConfiguration.adminManagementFeeBps,
          lockedProfitDegradationDuration: route.vaultConfiguration.lockedProfitDegradationDurationSeconds,
          redemptionFee: route.vaultConfiguration.redemptionFeeBps,
          issuanceFee: route.vaultConfiguration.issuanceFeeBps,
          // This is intentionally sourced from RouteSpec, never from an SDK
          // default or a caller-provided CLI value.
          withdrawalWaitingPeriod: route.vaultConfiguration.withdrawalWaitingPeriodSeconds,
          name: route.vaultConfiguration.name,
          description: route.vaultConfiguration.description,
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, INITIALIZE_VAULT_LABELS);
      },
      async addAdaptor(signers) {
        requireSignerAddress(signers.payer, route.setupAdmin, "setup payer");
        requireSignerAddress(signers.admin, route.setupAdmin, "setup admin");
        const instruction = await getAddAdaptorInstructionAsync({
          payer: signers.payer,
          admin: signers.admin,
          vault: route.vault,
          adaptorAddReceipt: accounts.adaptorAddReceipt,
          adaptorProgram: route.programs.kaminoAdaptor,
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, ADD_ADAPTOR_LABELS);
      },
      async setManagerToAdmin(signers) {
        requireSignerAddress(signers.admin, route.setupAdmin, "setup admin");
        const instruction = await getUpdateVaultConfigInstructionAsync({
          admin: signers.admin,
          vault: route.vault,
          field: VaultConfigField.Manager,
          data: ADDRESS_ENCODER.encode(route.setupAdmin),
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, ["admin", "protocol", "vault", "rent"]);
      },
      async initializeStrategyAsAdmin(signers) {
        requireSignerAddress(signers.payer, route.setupAdmin, "setup payer");
        requireSignerAddress(signers.admin, route.setupAdmin, "setup admin");
        return initializeStrategy(signers.payer, signers.admin);
      },
      async restoreManager(signers) {
        requireSignerAddress(signers.admin, route.setupAdmin, "setup admin");
        const instruction = await getUpdateVaultConfigInstructionAsync({
          admin: signers.admin,
          vault: route.vault,
          field: VaultConfigField.Manager,
          data: ADDRESS_ENCODER.encode(route.squads.manager),
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, ["admin", "protocol", "vault", "rent"]);
      },
    },
    strategy: {
      async deposit(manager, amountRaw) {
        managerSigner(manager);
        const instruction = await getDepositStrategyInstructionAsync({
          manager,
          vault: route.vault,
          strategy: route.strategy.reserve,
          vaultAssetMint: route.asset.mint,
          vaultAssetIdleAuth: accounts.idleAuth,
          vaultStrategyAuth: accounts.strategyAuth,
          vaultLpMint: accounts.lpMint,
          vaultAssetIdleAta: accounts.idleAta,
          vaultStrategyAssetAta: await associatedToken(accounts.strategyAuth, route.asset.mint, route),
          assetTokenProgram: route.programs.token,
          adaptorProgram: route.programs.kaminoAdaptor,
          adaptorAddReceipt: accounts.adaptorAddReceipt,
          strategyInitReceipt: accounts.strategyInitReceipt,
          amount: amount(amountRaw, route.asset.maxManagerOperationRaw),
          instructionDiscriminator: KAMINO_DEPOSIT_DISCRIMINATOR,
          additionalArgs: null,
        }, { programAddress: route.programs.voltrVault });
        const withRemaining = appendRemaining(instruction, [
          writable("kaminoObligation", graph.obligation),
          readonly("lendingMarket", graph.lendingMarket),
          readonly("lendingMarketAuthority", graph.lendingMarketAuthority),
          writable("reserve", graph.reserve),
          writable("reserveLiquiditySupply", graph.reserveLiquiditySupply),
          writable("reserveCollateralMint", graph.reserveCollateralMint),
          writable("reserveCollateralSupplyVault", graph.reserveCollateralSupplyVault),
          readonly("tokenProgram", route.programs.token),
          readonly("instructionsSysvar", address("Sysvar1nstructions1111111111111111111111111")),
          writable("obligationFarm", graph.obligationFarm),
          writable("reserveFarmState", graph.reserveFarmState),
          writable("userMetadata", graph.userMetadata),
          readonly("scope", graph.scope),
          readonly("rentSysvar", address("SysvarRent111111111111111111111111111111111")),
          readonly("systemProgram", route.programs.system),
          readonly("farmsProgram", route.programs.farms),
          readonly("klendProgram", route.programs.klend),
        ]);
        return labelled(withRemaining, DEPOSIT_LABELS);
      },
      async withdraw(manager, amountRaw) {
        managerSigner(manager);
        const instruction = await getWithdrawStrategyInstructionAsync({
          manager,
          vault: route.vault,
          strategy: route.strategy.reserve,
          vaultAssetMint: route.asset.mint,
          vaultAssetIdleAuth: accounts.idleAuth,
          vaultStrategyAuth: accounts.strategyAuth,
          vaultLpMint: accounts.lpMint,
          vaultAssetIdleAta: accounts.idleAta,
          vaultStrategyAssetAta: await associatedToken(accounts.strategyAuth, route.asset.mint, route),
          assetTokenProgram: route.programs.token,
          adaptorProgram: route.programs.kaminoAdaptor,
          adaptorAddReceipt: accounts.adaptorAddReceipt,
          strategyInitReceipt: accounts.strategyInitReceipt,
          amount: amount(amountRaw, route.asset.maxManagerOperationRaw),
          instructionDiscriminator: KAMINO_WITHDRAW_DISCRIMINATOR,
          additionalArgs: null,
        }, { programAddress: route.programs.voltrVault });
        const withRemaining = appendRemaining(instruction, [
          writable("kaminoObligation", graph.obligation),
          readonly("lendingMarket", graph.lendingMarket),
          readonly("lendingMarketAuthority", graph.lendingMarketAuthority),
          writable("reserve", graph.reserve),
          writable("reserveCollateralSupplyVault", graph.reserveCollateralSupplyVault),
          writable("reserveCollateralMint", graph.reserveCollateralMint),
          writable("reserveLiquiditySupply", graph.reserveLiquiditySupply),
          readonly("tokenProgram", route.programs.token),
          readonly("instructionsSysvar", address("Sysvar1nstructions1111111111111111111111111")),
          writable("obligationFarm", graph.obligationFarm),
          writable("reserveFarmState", graph.reserveFarmState),
          readonly("scope", graph.scope),
          readonly("farmsProgram", route.programs.farms),
          readonly("klendProgram", route.programs.klend),
        ]);
        return labelled(withRemaining, WITHDRAW_LABELS);
      },
    },
    user: {
      async deposit(signers, amountRaw) {
        const user = await deriveUserAccounts(route, accounts, signers.user.address);
        const instruction = await getDepositVaultInstructionAsync({
          userTransferAuthority: signers.user,
          vault: route.vault,
          vaultAssetMint: route.asset.mint,
          vaultLpMint: user.lpMint,
          userAssetAta: user.userAssetAta,
          vaultAssetIdleAta: user.idleAta,
          vaultAssetIdleAuth: user.idleAuth,
          userLpAta: user.userLpAta,
          vaultLpMintAuth: user.lpMintAuth,
          assetTokenProgram: route.programs.token,
          amount: amount(amountRaw, route.asset.vaultCapRaw),
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, DEPOSIT_VAULT_LABELS);
      },
      async requestWithdraw(signers, amountLpRaw, withdrawAll = false) {
        const payer = signers.payer ?? signers.user;
        const user = await deriveUserAccounts(route, accounts, signers.user.address);
        const instruction = await getRequestWithdrawVaultInstructionAsync({
          payer,
          userTransferAuthority: signers.user,
          vault: route.vault,
          vaultLpMint: user.lpMint,
          userLpAta: user.userLpAta,
          requestWithdrawLpAta: user.requestWithdrawLpAta,
          requestWithdrawVaultReceipt: user.requestWithdrawVaultReceipt,
          amount: amountLpRaw,
          isAmountInLp: true,
          isWithdrawAll: withdrawAll,
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, REQUEST_WITHDRAW_LABELS);
      },
      async instantWithdraw(signers, amountLpRaw, withdrawAll = false) {
        const user = await deriveUserAccounts(route, accounts, signers.user.address);
        const instruction = await getInstantWithdrawVaultInstructionAsync({
          userTransferAuthority: signers.user,
          vault: route.vault,
          vaultAssetMint: route.asset.mint,
          vaultLpMint: user.lpMint,
          userLpAta: user.userLpAta,
          vaultAssetIdleAta: user.idleAta,
          vaultAssetIdleAuth: user.idleAuth,
          userAssetAta: user.userAssetAta,
          assetTokenProgram: route.programs.token,
          amount: amountLpRaw,
          isAmountInLp: true,
          isWithdrawAll: withdrawAll,
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, INSTANT_WITHDRAW_LABELS);
      },
      async cancelWithdraw(userSigner) {
        const user = await deriveUserAccounts(route, accounts, userSigner.address);
        const instruction = await getCancelRequestWithdrawVaultInstructionAsync({
          userTransferAuthority: userSigner,
          vault: route.vault,
          vaultLpMint: user.lpMint,
          userLpAta: user.userLpAta,
          requestWithdrawLpAta: user.requestWithdrawLpAta,
          requestWithdrawVaultReceipt: user.requestWithdrawVaultReceipt,
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, CANCEL_WITHDRAW_LABELS);
      },
      async claimWithdraw(userSigner) {
        const user = await deriveUserAccounts(route, accounts, userSigner.address);
        const instruction = await getWithdrawVaultInstructionAsync({
          userTransferAuthority: userSigner,
          vault: route.vault,
          vaultAssetMint: route.asset.mint,
          vaultLpMint: user.lpMint,
          requestWithdrawLpAta: user.requestWithdrawLpAta,
          vaultAssetIdleAta: user.idleAta,
          vaultAssetIdleAuth: user.idleAuth,
          userAssetAta: user.userAssetAta,
          requestWithdrawVaultReceipt: user.requestWithdrawVaultReceipt,
          assetTokenProgram: route.programs.token,
        }, { programAddress: route.programs.voltrVault });
        return labelled(instruction, CLAIM_WITHDRAW_LABELS);
      },
    },
  };
}

async function associatedToken(
  owner: Address,
  mint: Address,
  route: PartnerRouteSpec,
): Promise<Address> {
  const [ata] = await findAssociatedTokenPda({
    owner,
    mint,
    tokenProgram: route.programs.token,
  }, { programAddress: route.programs.associatedToken });
  return ata;
}
