import { PublicKey, SystemProgram } from "@solana/web3.js";
import {
  ASSOCIATED_TOKEN_PROGRAM_ID,
  JUPITER_SWAP_DISCRIMINATOR,
  JUPITER_SWAP_SLIPPAGE_BPS_OFFSET,
  JUPITER_SHARED_ACCOUNTS_ROUTE_V2_DISCRIMINATOR,
  JUPITER_SHARED_ACCOUNTS_ROUTE_V2_SLIPPAGE_BPS_OFFSET,
  KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
  KAMINO_INIT_OBLIGATION_DISCRIMINATOR,
  KAMINO_LEND_PROGRAM_ID,
  KAMINO_USER_METADATA_SEED,
  KAMINO_VANILLA_OBLIGATION_ID,
  KAMINO_VANILLA_OBLIGATION_TAG,
  KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
} from "../constants.js";
import {
  CONFIG_SEED,
  HUB_AUTHORITY_SEED,
  SWAP_EXACT_IN,
  SWAP_EXACT_IN_MAX_FEE_BPS_DATA_OFFSET,
  SWAP_EXACT_IN_TAG_OFFSET,
  WITHDRAW_INVENTORY,
  WITHDRAW_INVENTORY_AMOUNT_DATA_OFFSET,
  WITHDRAW_INVENTORY_LANE_ID_DATA_OFFSET,
  WITHDRAW_INVENTORY_TAG_OFFSET,
  swapExactInAccounts,
  withdrawInventoryAccounts,
} from "../generated/loyal-hub-abi.js";
import type { LoyalClusterConfig } from "../cluster.js";
import type { DataConstraint, InstructionConstraint } from "./squads.js";

const SPL_TOKEN_ACCOUNT_AUTHORITY_OFFSET = 32n;
const SPL_TOKEN_TRANSFER_CHECKED = 12;
const SPL_TOKEN_TRANSFER_CHECKED_AMOUNT_DATA_OFFSET = 1n;
const SPL_TOKEN_TRANSFER_CHECKED_DECIMALS_DATA_OFFSET = 9n;
const SYSVAR_RENT_PUBKEY = new PublicKey("SysvarRent111111111111111111111111111111111");
const DEFAULT_PUBKEY = PublicKey.default;
const TRANSFER_CHECKED_ACCOUNTS = {
  SOURCE: 0,
  MINT: 1,
  DESTINATION: 2,
  AUTHORITY: 3,
} as const;

export type TreasuryLoyalHubRebalanceConstraintInput = {
  vault: PublicKey;
  laneId: number;
  inputMint: PublicKey;
  outputMint: PublicKey;
  inputTokenProgram: PublicKey;
  outputTokenProgram: PublicKey;
  outputMintDecimals: number;
  maxWithdrawAmount: bigint;
  maxTopUpAmount: bigint;
  maxSlippageBps: number;
};

export function kaminoWithdrawConstraint(
  config: LoyalClusterConfig,
  vault: PublicKey,
  markets: readonly PublicKey[],
  liquidityMints: readonly PublicKey[],
): InstructionConstraint {
  return {
    programId: KAMINO_LEND_PROGRAM_ID,
    accountConstraints: [
      pubkeyConstraint(0, [vault]),
      pubkeyConstraint(1, uniquePubkeys(markets)),
      pubkeyConstraint(4, uniquePubkeys(liquidityMints), config.tokenProgramId),
      splTokenAuthorityConstraint(8, vault, config.tokenProgramId),
      pubkeyConstraint(10, [config.tokenProgramId]),
    ],
    dataConstraints: discriminatorConstraint(KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR),
  };
}

export function kaminoDepositConstraint(
  config: LoyalClusterConfig,
  vault: PublicKey,
  markets: readonly PublicKey[],
  liquidityMints: readonly PublicKey[],
): InstructionConstraint {
  return {
    programId: KAMINO_LEND_PROGRAM_ID,
    accountConstraints: [
      pubkeyConstraint(0, [vault]),
      pubkeyConstraint(2, uniquePubkeys(markets)),
      pubkeyConstraint(4, uniquePubkeys(liquidityMints), config.tokenProgramId),
      splTokenAuthorityConstraint(8, vault, config.tokenProgramId),
      pubkeyConstraint(10, [config.tokenProgramId]),
    ],
    dataConstraints: discriminatorConstraint(KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR),
  };
}

export function kaminoInitObligationConstraint(
  vault: PublicKey,
  markets: readonly PublicKey[],
): InstructionConstraint {
  const marketList = uniquePubkeys(markets);
  const obligations = marketList.map((market) => deriveKaminoVanillaObligation(vault, market));
  const dataPrefix = [
    ...KAMINO_INIT_OBLIGATION_DISCRIMINATOR,
    KAMINO_VANILLA_OBLIGATION_TAG,
    KAMINO_VANILLA_OBLIGATION_ID,
  ];

  return {
    programId: KAMINO_LEND_PROGRAM_ID,
    accountConstraints: [
      pubkeyConstraint(0, [vault]),
      pubkeyConstraint(1, [vault]),
      pubkeyConstraint(2, obligations),
      pubkeyConstraint(3, marketList),
      pubkeyConstraint(4, [DEFAULT_PUBKEY]),
      pubkeyConstraint(5, [DEFAULT_PUBKEY]),
      pubkeyConstraint(6, [deriveKaminoUserMetadata(vault)]),
      pubkeyConstraint(7, [SYSVAR_RENT_PUBKEY]),
      pubkeyConstraint(8, [SystemProgram.programId]),
    ],
    dataConstraints: [dataSliceEquals(0n, dataPrefix)],
  };
}

export function jupiterConstraint(
  config: LoyalClusterConfig,
  vault: PublicKey,
  allowedMints: readonly PublicKey[],
  maxSlippageBps: number,
): InstructionConstraint {
  const mints = uniquePubkeys(allowedMints);
  return {
    programId: config.jupiterV6ProgramId,
    accountConstraints: [
      pubkeyConstraint(0, [vault]),
      splTokenAuthorityConstraint(1, vault, config.tokenProgramId),
      splTokenAuthorityConstraint(2, vault, config.tokenProgramId),
      pubkeyConstraint(3, mints, config.tokenProgramId),
      pubkeyConstraint(4, mints, config.tokenProgramId),
      pubkeyConstraint(5, [config.tokenProgramId]),
    ],
    dataConstraints: [
      dataSliceEquals(0n, JUPITER_SWAP_DISCRIMINATOR),
      dataU16LeLessThanOrEqualTo(BigInt(JUPITER_SWAP_SLIPPAGE_BPS_OFFSET), maxSlippageBps),
    ],
  };
}

export function loyalHubConstraint(
  config: LoyalClusterConfig,
  vault: PublicKey,
  allowedMints: readonly PublicKey[],
  maxFeeBps: number,
): InstructionConstraint {
  const mints = uniquePubkeys(allowedMints);
  return {
    programId: config.loyalHubSwapProgramId,
    accountConstraints: [
      pubkeyConstraint(
        swapExactInAccounts.CONFIG,
        [deriveLoyalHubConfig(config.loyalHubSwapProgramId)],
        config.loyalHubSwapProgramId,
      ),
      pubkeyConstraint(swapExactInAccounts.USER_VAULT, [vault]),
      accountDataConstraint(swapExactInAccounts.HUB_INPUT),
      accountDataConstraint(swapExactInAccounts.HUB_OUTPUT),
      tokenAuthorityConstraint(swapExactInAccounts.USER_INPUT, vault),
      tokenAuthorityConstraint(swapExactInAccounts.USER_OUTPUT, vault),
      pubkeyConstraint(swapExactInAccounts.INPUT_MINT, mints),
      pubkeyConstraint(swapExactInAccounts.OUTPUT_MINT, mints),
      pubkeyConstraint(swapExactInAccounts.HUB_AUTHORIZER, [config.loyalHubAuthorizer]),
      pubkeyConstraint(swapExactInAccounts.TOKEN_PROGRAM, [config.tokenProgramId]),
      pubkeyConstraint(swapExactInAccounts.TOKEN_2022_PROGRAM, [config.token2022ProgramId]),
    ],
    dataConstraints: [
      dataU8Equals(BigInt(SWAP_EXACT_IN_TAG_OFFSET), SWAP_EXACT_IN),
      dataU16LeLessThanOrEqualTo(BigInt(SWAP_EXACT_IN_MAX_FEE_BPS_DATA_OFFSET), maxFeeBps),
    ],
  };
}

export function treasuryLoyalHubRebalanceConstraints(
  config: LoyalClusterConfig,
  input: TreasuryLoyalHubRebalanceConstraintInput,
): [InstructionConstraint, InstructionConstraint, InstructionConstraint] {
  return [
    loyalHubWithdrawInventoryConstraint(config, input),
    treasuryJupiterSwapConstraint(config, input),
    treasuryTopUpTransferCheckedConstraint(config, input),
  ];
}

export function loyalHubWithdrawInventoryConstraint(
  config: LoyalClusterConfig,
  input: TreasuryLoyalHubRebalanceConstraintInput,
): InstructionConstraint {
  const hubAuthority = deriveLoyalHubAuthority(config.loyalHubSwapProgramId, input.laneId);
  const hubSource = deriveAssociatedTokenAddress(input.inputMint, hubAuthority, input.inputTokenProgram);
  const treasuryInput = deriveAssociatedTokenAddress(input.inputMint, input.vault, input.inputTokenProgram);

  return {
    programId: config.loyalHubSwapProgramId,
    accountConstraints: [
      pubkeyConstraint(
        withdrawInventoryAccounts.CONFIG,
        [deriveLoyalHubConfig(config.loyalHubSwapProgramId)],
        config.loyalHubSwapProgramId,
      ),
      pubkeyConstraint(withdrawInventoryAccounts.ADMIN, [input.vault]),
      pubkeyConstraint(withdrawInventoryAccounts.HUB_SOURCE, [hubSource], input.inputTokenProgram),
      tokenAuthorityConstraint(withdrawInventoryAccounts.HUB_SOURCE, hubAuthority),
      pubkeyConstraint(withdrawInventoryAccounts.DESTINATION, [treasuryInput], input.inputTokenProgram),
      tokenAuthorityConstraint(withdrawInventoryAccounts.DESTINATION, input.vault),
      pubkeyConstraint(withdrawInventoryAccounts.MINT, [input.inputMint], input.inputTokenProgram),
      pubkeyConstraint(withdrawInventoryAccounts.HUB_AUTHORITY, [hubAuthority]),
      pubkeyConstraint(withdrawInventoryAccounts.TOKEN_PROGRAM, [input.inputTokenProgram]),
    ],
    dataConstraints: [
      dataU8Equals(BigInt(WITHDRAW_INVENTORY_TAG_OFFSET), WITHDRAW_INVENTORY),
      dataU64LeLessThanOrEqualTo(BigInt(WITHDRAW_INVENTORY_AMOUNT_DATA_OFFSET), input.maxWithdrawAmount),
      dataU8Equals(BigInt(WITHDRAW_INVENTORY_LANE_ID_DATA_OFFSET), input.laneId),
    ],
  };
}

export function treasuryJupiterSwapConstraint(
  config: LoyalClusterConfig,
  input: TreasuryLoyalHubRebalanceConstraintInput,
): InstructionConstraint {
  const treasuryInput = deriveAssociatedTokenAddress(input.inputMint, input.vault, input.inputTokenProgram);
  const treasuryOutput = deriveAssociatedTokenAddress(input.outputMint, input.vault, input.outputTokenProgram);

  return {
    programId: config.jupiterV6ProgramId,
    accountConstraints: [
      pubkeyConstraint(1, [input.vault]),
      pubkeyConstraint(2, [treasuryInput], input.inputTokenProgram),
      tokenAuthorityConstraint(2, input.vault),
      pubkeyConstraint(5, [treasuryOutput], input.outputTokenProgram),
      tokenAuthorityConstraint(5, input.vault),
      pubkeyConstraint(6, [input.inputMint], input.inputTokenProgram),
      pubkeyConstraint(7, [input.outputMint], input.outputTokenProgram),
      pubkeyConstraint(8, [input.inputTokenProgram]),
      ...(input.outputTokenProgram.equals(input.inputTokenProgram)
        ? []
        : [pubkeyConstraint(9, [input.outputTokenProgram])]),
    ],
    dataConstraints: [
      dataSliceEquals(0n, JUPITER_SHARED_ACCOUNTS_ROUTE_V2_DISCRIMINATOR),
      dataU16LeLessThanOrEqualTo(BigInt(JUPITER_SHARED_ACCOUNTS_ROUTE_V2_SLIPPAGE_BPS_OFFSET), input.maxSlippageBps),
    ],
  };
}

export function treasuryTopUpTransferCheckedConstraint(
  config: LoyalClusterConfig,
  input: TreasuryLoyalHubRebalanceConstraintInput,
): InstructionConstraint {
  const hubAuthority = deriveLoyalHubAuthority(config.loyalHubSwapProgramId, input.laneId);
  const treasuryOutput = deriveAssociatedTokenAddress(input.outputMint, input.vault, input.outputTokenProgram);
  const hubOutput = deriveAssociatedTokenAddress(input.outputMint, hubAuthority, input.outputTokenProgram);

  return {
    programId: input.outputTokenProgram,
    accountConstraints: [
      pubkeyConstraint(TRANSFER_CHECKED_ACCOUNTS.SOURCE, [treasuryOutput], input.outputTokenProgram),
      tokenAuthorityConstraint(TRANSFER_CHECKED_ACCOUNTS.SOURCE, input.vault),
      pubkeyConstraint(TRANSFER_CHECKED_ACCOUNTS.MINT, [input.outputMint], input.outputTokenProgram),
      pubkeyConstraint(TRANSFER_CHECKED_ACCOUNTS.DESTINATION, [hubOutput], input.outputTokenProgram),
      tokenAuthorityConstraint(TRANSFER_CHECKED_ACCOUNTS.DESTINATION, hubAuthority),
      pubkeyConstraint(TRANSFER_CHECKED_ACCOUNTS.AUTHORITY, [input.vault]),
    ],
    dataConstraints: [
      dataU8Equals(0n, SPL_TOKEN_TRANSFER_CHECKED),
      dataU64LeLessThanOrEqualTo(SPL_TOKEN_TRANSFER_CHECKED_AMOUNT_DATA_OFFSET, input.maxTopUpAmount),
      dataU8Equals(SPL_TOKEN_TRANSFER_CHECKED_DECIMALS_DATA_OFFSET, input.outputMintDecimals),
    ],
  };
}

export function deriveLoyalHubConfig(loyalHubProgramId: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([CONFIG_SEED], loyalHubProgramId)[0];
}

export function deriveLoyalHubAuthority(loyalHubProgramId: PublicKey, laneId: number): PublicKey {
  return PublicKey.findProgramAddressSync([HUB_AUTHORITY_SEED, Uint8Array.of(laneId)], loyalHubProgramId)[0];
}

export function deriveAssociatedTokenAddress(mint: PublicKey, owner: PublicKey, tokenProgram: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync(
    [owner.toBytes(), tokenProgram.toBytes(), mint.toBytes()],
    ASSOCIATED_TOKEN_PROGRAM_ID,
  )[0];
}

export function deriveKaminoVanillaObligation(vault: PublicKey, lendingMarket: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync(
    [
      Uint8Array.of(KAMINO_VANILLA_OBLIGATION_TAG),
      Uint8Array.of(KAMINO_VANILLA_OBLIGATION_ID),
      vault.toBytes(),
      lendingMarket.toBytes(),
      DEFAULT_PUBKEY.toBytes(),
      DEFAULT_PUBKEY.toBytes(),
    ],
    KAMINO_LEND_PROGRAM_ID,
  )[0];
}

export function deriveKaminoUserMetadata(vault: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([KAMINO_USER_METADATA_SEED, vault.toBytes()], KAMINO_LEND_PROGRAM_ID)[0];
}

function pubkeyConstraint(accountIndex: number, pubkeys: readonly PublicKey[], owner?: PublicKey) {
  return {
    accountIndex,
    kind: { type: "pubkey" as const, pubkeys: [...pubkeys] },
    owner,
  };
}

function accountDataConstraint(accountIndex: number, owner?: PublicKey) {
  return {
    accountIndex,
    kind: { type: "accountData" as const, dataConstraints: [] },
    owner,
  };
}

function splTokenAuthorityConstraint(accountIndex: number, authority: PublicKey, tokenProgramId: PublicKey) {
  return tokenAuthorityConstraint(accountIndex, authority, tokenProgramId);
}

function tokenAuthorityConstraint(accountIndex: number, authority: PublicKey, owner?: PublicKey) {
  return {
    accountIndex,
    kind: {
      type: "accountData" as const,
      dataConstraints: [dataSliceEquals(SPL_TOKEN_ACCOUNT_AUTHORITY_OFFSET, [...authority.toBytes()])],
    },
    owner,
  };
}

function discriminatorConstraint(discriminator: readonly number[]): DataConstraint[] {
  return [dataSliceEquals(0n, discriminator)];
}

function dataSliceEquals(offset: bigint, bytes: readonly number[]): DataConstraint {
  return {
    dataOffset: offset,
    dataValue: { type: "u8Slice", value: [...bytes] },
    operator: "equals",
  };
}

function dataU8Equals(offset: bigint, value: number): DataConstraint {
  return {
    dataOffset: offset,
    dataValue: { type: "u8", value },
    operator: "equals",
  };
}

function dataU16LeLessThanOrEqualTo(offset: bigint, value: number): DataConstraint {
  return {
    dataOffset: offset,
    dataValue: { type: "u16Le", value },
    operator: "lessThanOrEqualTo",
  };
}

function dataU64LeLessThanOrEqualTo(offset: bigint, value: bigint): DataConstraint {
  return {
    dataOffset: offset,
    dataValue: { type: "u64Le", value },
    operator: "lessThanOrEqualTo",
  };
}

export function uniquePubkeys(pubkeys: readonly PublicKey[]): PublicKey[] {
  const seen = new Set<string>();
  const unique: PublicKey[] = [];
  for (const pubkey of pubkeys) {
    const key = pubkey.toBase58();
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    unique.push(pubkey);
  }
  return unique;
}
