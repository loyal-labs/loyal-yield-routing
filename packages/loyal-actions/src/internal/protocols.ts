import { PublicKey, SystemProgram } from "@solana/web3.js";
import {
  JUPITER_SWAP_DISCRIMINATOR,
  JUPITER_SWAP_SLIPPAGE_BPS_OFFSET,
  KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
  KAMINO_INIT_OBLIGATION_DISCRIMINATOR,
  KAMINO_LEND_PROGRAM_ID,
  KAMINO_USER_METADATA_SEED,
  KAMINO_VANILLA_OBLIGATION_ID,
  KAMINO_VANILLA_OBLIGATION_TAG,
  KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
} from "../constants.js";
import { CONFIG_SEED, SWAP_EXACT_IN, SWAP_EXACT_IN_MAX_FEE_BPS_DATA_OFFSET, SWAP_EXACT_IN_TAG_OFFSET, swapExactInAccounts } from "../generated/loyal-hub-abi.js";
import type { LoyalClusterConfig } from "../cluster.js";
import type { DataConstraint, InstructionConstraint } from "./squads.js";

const SPL_TOKEN_ACCOUNT_AUTHORITY_OFFSET = 32n;
const SYSVAR_RENT_PUBKEY = new PublicKey("SysvarRent111111111111111111111111111111111");
const DEFAULT_PUBKEY = PublicKey.default;

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

export function deriveLoyalHubConfig(loyalHubProgramId: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([CONFIG_SEED], loyalHubProgramId)[0];
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
