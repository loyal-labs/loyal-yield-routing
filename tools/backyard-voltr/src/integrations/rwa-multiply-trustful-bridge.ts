import {
  AccountRole,
  getAddressEncoder,
  type Address,
  type Instruction,
  type TransactionSigner,
} from "@solana/kit";
import {
  findAssociatedTokenPda,
  getCreateAssociatedTokenIdempotentInstructionAsync,
  getTransferCheckedInstruction,
} from "@solana-program/token";
import { PublicKey } from "@solana/web3.js";
import {
  findAdaptorAddReceiptPda,
  findProtocolPda,
  findStrategyInitReceiptPda,
  findVaultAssetIdleAuthPda,
  findVaultLpMintPda,
  findVaultStrategyAuthPda,
  getAddAdaptorInstructionAsync,
  getDepositStrategyInstructionAsync,
  getInitializeStrategyInstructionAsync,
  getUpdateVaultConfigInstructionAsync,
  getWithdrawStrategyInstructionAsync,
  VaultConfigField,
} from "@voltr/vault-sdk";

import {
  RWA_MULTIPLY_ROUTE,
  type RwaMultiplyRouteSpec,
} from "../domain/rwa-multiply-route-spec.js";

const ADDRESS_ENCODER = getAddressEncoder();

/** Discriminators published by voltrxyz/trustful-scripts at commit 64c94b0. */
export const TRUSTFUL_ARBITRARY_DISCRIMINATORS = {
  initialize: Uint8Array.from([251, 45, 95, 238, 92, 108, 238, 129]),
  deposit: Uint8Array.from([117, 73, 131, 148, 12, 99, 191, 180]),
  withdraw: Uint8Array.from([35, 58, 217, 109, 98, 184, 147, 14]),
} as const;

export type TrustfulBridgeAccounts = Readonly<{
  protocol: Address;
  adaptorAddReceipt: Address;
  strategyInitReceipt: Address;
  idleAuth: Address;
  idleAta: Address;
  lpMint: Address;
  strategyAuth: Address;
  strategyAssetAta: Address;
  withdrawalHoldingAuth: Address;
  withdrawalHoldingAta: Address;
}>;

function requireSigner(
  signer: TransactionSigner,
  expected: Address,
  label: string,
): void {
  if (signer.address !== expected) {
    throw new Error(`${label} signer ${signer.address} does not match ${expected}`);
  }
}

function appendAccounts(
  instruction: Instruction,
  accounts: readonly Readonly<{ address: Address; role: AccountRole }>[],
): Instruction {
  return { ...instruction, accounts: [...(instruction.accounts ?? []), ...accounts] };
}

function u64Le(value: bigint): Uint8Array {
  if (value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new Error(`u64 value is out of range: ${value}`);
  }
  const bytes = new Uint8Array(8);
  let remaining = value;
  for (let index = 0; index < bytes.length; index += 1) {
    bytes[index] = Number(remaining & 0xffn);
    remaining >>= 8n;
  }
  return bytes;
}

export async function deriveTrustfulBridgeAccounts(
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<TrustfulBridgeAccounts> {
  const [protocol] = await findProtocolPda({ programAddress: route.programs.voltr });
  const [adaptorAddReceipt] = await findAdaptorAddReceiptPda(
    { vault: route.vault.address, adaptorProgram: route.trustfulBridge.adaptorProgram },
    { programAddress: route.programs.voltr },
  );
  const [strategyInitReceipt] = await findStrategyInitReceiptPda(
    { vault: route.vault.address, strategy: route.trustfulBridge.strategy },
    { programAddress: route.programs.voltr },
  );
  const [idleAuth] = await findVaultAssetIdleAuthPda(
    { vault: route.vault.address },
    { programAddress: route.programs.voltr },
  );
  const [idleAta] = await findAssociatedTokenPda({
    owner: idleAuth,
    mint: route.assets.assetMint,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const [lpMint] = await findVaultLpMintPda(
    { vault: route.vault.address },
    { programAddress: route.programs.voltr },
  );
  const [strategyAuth] = await findVaultStrategyAuthPda(
    { vault: route.vault.address, strategy: route.trustfulBridge.strategy },
    { programAddress: route.programs.voltr },
  );
  const [strategyAssetAta] = await findAssociatedTokenPda({
    owner: strategyAuth,
    mint: route.assets.assetMint,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const [derivedStrategy] = PublicKey.findProgramAddressSync(
    [Buffer.from(route.trustfulBridge.strategySeed)],
    new PublicKey(route.trustfulBridge.adaptorProgram),
  );
  if (derivedStrategy.toBase58() !== route.trustfulBridge.strategy) {
    throw new Error("Trustful strategy PDA does not match the pinned seed");
  }
  const [withdrawalHoldingAuth] = PublicKey.findProgramAddressSync(
    [new PublicKey(strategyAuth).toBuffer(), derivedStrategy.toBuffer()],
    new PublicKey(route.trustfulBridge.adaptorProgram),
  );
  const [withdrawalHoldingAta] = await findAssociatedTokenPda({
    owner: withdrawalHoldingAuth.toBase58() as Address,
    mint: route.assets.assetMint,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  if (withdrawalHoldingAuth.toBase58() !== route.trustfulBridge.withdrawalHoldingAuth
    || withdrawalHoldingAta !== route.trustfulBridge.withdrawalHoldingAta) {
    throw new Error("Trustful withdrawal holding accounts do not match the pinned route");
  }
  return {
    protocol,
    adaptorAddReceipt,
    strategyInitReceipt,
    idleAuth,
    idleAta,
    lpMint,
    strategyAuth,
    strategyAssetAta,
    withdrawalHoldingAuth: route.trustfulBridge.withdrawalHoldingAuth,
    withdrawalHoldingAta,
  };
}

export async function buildTrustfulBridgeSetupInstructions(
  admin: TransactionSigner,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
) {
  requireSigner(admin, route.setupAdmin, "setup admin");
  const accounts = await deriveTrustfulBridgeAccounts(route);
  const addAdaptor = await getAddAdaptorInstructionAsync({
    payer: admin,
    admin,
    vault: route.vault.address,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    adaptorProgram: route.trustfulBridge.adaptorProgram,
  }, { programAddress: route.programs.voltr });
  const createStrategyAssetAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: admin,
    ata: accounts.strategyAssetAta,
    owner: accounts.strategyAuth,
    mint: route.assets.assetMint,
    systemProgram: route.programs.system,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const createWithdrawalHoldingAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: admin,
    ata: accounts.withdrawalHoldingAta,
    owner: accounts.withdrawalHoldingAuth,
    mint: route.assets.assetMint,
    systemProgram: route.programs.system,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const initializeStrategy = await getInitializeStrategyInstructionAsync({
    payer: admin,
    manager: admin,
    vault: route.vault.address,
    strategy: route.trustfulBridge.strategy,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    strategyInitReceipt: accounts.strategyInitReceipt,
    vaultStrategyAuth: accounts.strategyAuth,
    adaptorProgram: route.trustfulBridge.adaptorProgram,
    instructionDiscriminator: TRUSTFUL_ARBITRARY_DISCRIMINATORS.initialize,
    additionalArgs: null,
  }, { programAddress: route.programs.voltr });
  const handoffManager = await getUpdateVaultConfigInstructionAsync({
    admin,
    vault: route.vault.address,
    field: VaultConfigField.Manager,
    data: ADDRESS_ENCODER.encode(route.squads.vault),
  }, { programAddress: route.programs.voltr });
  return {
    accounts,
    addAdaptor,
    createStrategyAssetAta,
    createWithdrawalHoldingAta,
    initializeStrategy,
    handoffManager,
  } as const;
}

function bridgeCommon(
  manager: TransactionSigner,
  amountRaw: bigint,
  route: RwaMultiplyRouteSpec,
  accounts: TrustfulBridgeAccounts,
) {
  requireSigner(manager, route.squads.vault, "Squads manager");
  if (amountRaw < 0n || amountRaw > route.vault.capRaw) {
    throw new Error(`amount must be in 0..${route.vault.capRaw}`);
  }
  return {
    manager,
    vault: route.vault.address,
    strategy: route.trustfulBridge.strategy,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    strategyInitReceipt: accounts.strategyInitReceipt,
    vaultAssetIdleAuth: accounts.idleAuth,
    vaultStrategyAuth: accounts.strategyAuth,
    vaultAssetMint: route.assets.assetMint,
    vaultLpMint: accounts.lpMint,
    vaultAssetIdleAta: accounts.idleAta,
    vaultStrategyAssetAta: accounts.strategyAssetAta,
    assetTokenProgram: route.assets.tokenProgram,
    adaptorProgram: route.trustfulBridge.adaptorProgram,
    amount: amountRaw,
  } as const;
}

export async function buildTrustfulBridgeDepositInstruction(
  manager: TransactionSigner,
  amountRaw: bigint,
  positionValueAfterDepositRaw: bigint,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<Instruction> {
  const accounts = await deriveTrustfulBridgeAccounts(route);
  const instruction = await getDepositStrategyInstructionAsync({
    ...bridgeCommon(manager, amountRaw, route, accounts),
    instructionDiscriminator: TRUSTFUL_ARBITRARY_DISCRIMINATORS.deposit,
    additionalArgs: u64Le(positionValueAfterDepositRaw),
  }, { programAddress: route.programs.voltr });
  return appendAccounts(instruction, [{
    address: route.squads.assetAta,
    role: AccountRole.WRITABLE,
  }]);
}

export async function buildTrustfulBridgeWithdrawInstruction(
  manager: TransactionSigner,
  amountRaw: bigint,
  positionValueAfterWithdrawRaw: bigint,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<Instruction> {
  const accounts = await deriveTrustfulBridgeAccounts(route);
  const instruction = await getWithdrawStrategyInstructionAsync({
    ...bridgeCommon(manager, amountRaw, route, accounts),
    instructionDiscriminator: TRUSTFUL_ARBITRARY_DISCRIMINATORS.withdraw,
    additionalArgs: u64Le(positionValueAfterWithdrawRaw),
  }, { programAddress: route.programs.voltr });
  return appendAccounts(instruction, [
    { address: accounts.withdrawalHoldingAuth, role: AccountRole.READONLY },
    { address: accounts.withdrawalHoldingAta, role: AccountRole.WRITABLE },
  ]);
}

/**
 * Stage exact withdrawal liquidity under the Trustful adaptor's deterministic
 * holding authority. The Squads vault PDA signs this instruction through a
 * ProgramInteraction policy; no delegate is installed on the Squads USDC ATA.
 */
export async function buildTrustfulBridgeWithdrawalStagingInstruction(
  manager: TransactionSigner,
  amountRaw: bigint,
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): Promise<Instruction> {
  requireSigner(manager, route.squads.vault, "Squads manager");
  if (amountRaw <= 0n || amountRaw > route.vault.capRaw) {
    throw new Error(`staging amount must be in 1..${route.vault.capRaw}`);
  }
  const accounts = await deriveTrustfulBridgeAccounts(route);
  return getTransferCheckedInstruction({
    source: route.squads.assetAta,
    mint: route.assets.assetMint,
    destination: accounts.withdrawalHoldingAta,
    authority: manager,
    amount: amountRaw,
    decimals: route.assets.decimals,
  }, { programAddress: route.assets.tokenProgram });
}
