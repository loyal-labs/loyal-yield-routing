import { createHash } from "node:crypto";

import {
  borrowObligationLiquidityV2,
  depositReserveLiquidityAndObligationCollateralV2,
  repayObligationLiquidityV2,
  withdrawObligationCollateralAndRedeemReserveCollateralV2,
} from "@kamino-finance/klend-sdk";
import {
  address,
  createNoopSigner,
  isSignerRole,
  isWritableRole,
  none,
  type Address,
  type Instruction,
} from "@solana/kit";
import BN from "bn.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

const PROBE_AMOUNT_RAW = 1_000_000n;
const INSTRUCTIONS_SYSVAR = address("Sysvar1nstructions1111111111111111111111111");
const KAMINO_NULL_PUBKEY = "nu11111111111111111111111111111111111111111";
const SYSTEM_DEFAULT_PUBKEY = "11111111111111111111111111111111";

export const PHASE_TWO_REPRESENTATIVE_LANES = [
  "OnRe/ONyc/USDC",
  "Prime/PRIME/USDC",
  "Maple/syrupUSDC/USDC",
  "AUTO/AUTO/PYUSD",
  "Ethena/USDe/PYUSD",
] as const;

type Json = Record<string, unknown>;
type Reserve = Readonly<{
  address: string;
  liquidityMint: string;
  liquidityTokenProgram: string;
  liquiditySupply: string;
  liquidityFeeReceiver: string;
  collateralMint: string;
  collateralSupply: string;
}>;
type Custody = Readonly<{ address: string; exact: boolean }>;
export type ResolvedLane = Readonly<{
  key: string;
  exact: boolean;
  resolved: Readonly<{
    klendProgram: string;
    vault: string;
    lendingMarket: string;
    lendingMarketAuthority: string;
    collateralReserve: Reserve;
    debtReserve: Reserve;
    obligation: string;
    collateralCustody: Custody;
    debtCustody: Custody;
    instructionSysvar: string;
  }>;
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function string(value: unknown, label: string): string {
  invariant(typeof value === "string" && value.length > 0, `${label} is missing`);
  return value;
}

function publicAddress(value: string, label: string): Address {
  try {
    return address(value);
  } catch {
    throw new Error(`${label} is not a public key`);
  }
}

/**
 * K-Lend encodes an omitted oracle with its own `nu111…` sentinel, not the
 * system-program default. Passing that sentinel to RefreshReserve is a real
 * account meta and KLend rejects it as an invalid Pyth account.
 */
export function hasConfiguredKaminoOracle(value: string): boolean {
  return value !== KAMINO_NULL_PUBKEY && value !== SYSTEM_DEFAULT_PUBKEY;
}

function wireAccounts(ix: Instruction) {
  return ix.accounts?.map((account) => ({
    address: account.address,
    signer: isSignerRole(account.role),
    writable: isWritableRole(account.role),
  })) ?? [];
}

function sha256(value: ArrayLike<number>): string {
  const bytes = new Uint8Array(value.length);
  for (let index = 0; index < value.length; index += 1) {
    bytes[index] = value[index] ?? 0;
  }
  return createHash("sha256").update(bytes).digest("hex");
}

function assertLane(lane: ResolvedLane) {
  invariant(lane.exact, `${lane.key} is not an exact resolved lane`);
  const graph = lane.resolved;
  invariant(graph.klendProgram === RWA_MULTIPLY_ROUTE.kamino.program,
    `${lane.key} KLend program drifted`);
  invariant(graph.vault === RWA_MULTIPLY_ROUTE.squads.vault,
    `${lane.key} Squads vault drifted`);
  invariant(graph.instructionSysvar === INSTRUCTIONS_SYSVAR,
    `${lane.key} instructions sysvar drifted`);
  invariant(graph.collateralCustody.exact && graph.debtCustody.exact,
    `${lane.key} custody boundary is not exact`);
  for (const [label, value] of [
    ["market", graph.lendingMarket], ["market authority", graph.lendingMarketAuthority],
    ["obligation", graph.obligation], ["collateral reserve", graph.collateralReserve.address],
    ["debt reserve", graph.debtReserve.address], ["collateral mint", graph.collateralReserve.liquidityMint],
    ["debt mint", graph.debtReserve.liquidityMint], ["collateral custody", graph.collateralCustody.address],
    ["debt custody", graph.debtCustody.address], ["collateral supply", graph.collateralReserve.liquiditySupply],
    ["debt supply", graph.debtReserve.liquiditySupply], ["debt fee receiver", graph.debtReserve.liquidityFeeReceiver],
    ["collateral receipt mint", graph.collateralReserve.collateralMint],
    ["collateral receipt supply", graph.collateralReserve.collateralSupply],
    ["farms program", RWA_MULTIPLY_ROUTE.kamino.farmsProgram],
  ] as const) publicAddress(string(value, `${lane.key} ${label}`), `${lane.key} ${label}`);
}

function assertInstruction(
  lane: ResolvedLane,
  operation: string,
  ix: Instruction,
  expectedAccounts: number,
  ownerWritable: boolean,
) {
  const accounts = wireAccounts(ix);
  invariant(ix.programAddress === RWA_MULTIPLY_ROUTE.kamino.program,
    `${lane.key} ${operation} has the wrong program`);
  invariant(ix.data !== undefined && ix.data.length === 16,
    `${lane.key} ${operation} does not have the exact u64 KLend shape`);
  invariant(accounts.length === expectedAccounts,
    `${lane.key} ${operation} account count drifted: ${accounts.length}`);
  invariant(accounts[0]?.address === RWA_MULTIPLY_ROUTE.squads.vault
    && accounts[0]?.signer && accounts[0]?.writable === ownerWritable,
  `${lane.key} ${operation} has the wrong Squads vault signer role`);
  return {
    operation,
    programId: ix.programAddress,
    dataBase64: Buffer.from(ix.data).toString("base64"),
    dataSha256: sha256(ix.data),
    dataLength: ix.data.length,
    accountCount: accounts.length,
    accounts,
  } as const;
}

/**
 * Build the exact four K-Lend V2 operation shapes for a fully decoded lane.
 * This is policy compilation input only: it signs nothing and has no RPC or
 * submission path.  Farms are explicitly absent (K-Lend program placeholders)
 * just as in the live Phase-1 policy builder.
 */
export function buildPhaseTwoKaminoLaneOperations(lane: ResolvedLane, amountRaw: bigint = PROBE_AMOUNT_RAW) {
  assertLane(lane);
  const graph = lane.resolved;
  const account = {
    obligation: publicAddress(graph.obligation, `${lane.key} obligation`),
    market: publicAddress(graph.lendingMarket, `${lane.key} market`),
    marketAuthority: publicAddress(graph.lendingMarketAuthority, `${lane.key} market authority`),
    collateralReserve: publicAddress(graph.collateralReserve.address, `${lane.key} collateral reserve`),
    collateralMint: publicAddress(graph.collateralReserve.liquidityMint, `${lane.key} collateral mint`),
    collateralSupply: publicAddress(graph.collateralReserve.liquiditySupply, `${lane.key} collateral supply`),
    collateralReceiptMint: publicAddress(graph.collateralReserve.collateralMint, `${lane.key} collateral receipt mint`),
    collateralReceiptSupply: publicAddress(graph.collateralReserve.collateralSupply, `${lane.key} collateral receipt supply`),
    collateralCustody: publicAddress(graph.collateralCustody.address, `${lane.key} collateral custody`),
    collateralTokenProgram: publicAddress(graph.collateralReserve.liquidityTokenProgram, `${lane.key} collateral token program`),
    debtReserve: publicAddress(graph.debtReserve.address, `${lane.key} debt reserve`),
    debtMint: publicAddress(graph.debtReserve.liquidityMint, `${lane.key} debt mint`),
    debtSupply: publicAddress(graph.debtReserve.liquiditySupply, `${lane.key} debt supply`),
    debtFeeReceiver: publicAddress(graph.debtReserve.liquidityFeeReceiver, `${lane.key} debt fee receiver`),
    debtCustody: publicAddress(graph.debtCustody.address, `${lane.key} debt custody`),
    debtTokenProgram: publicAddress(graph.debtReserve.liquidityTokenProgram, `${lane.key} debt token program`),
    klend: publicAddress(graph.klendProgram, `${lane.key} KLend program`),
  };
  const owner = createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault);
  const farms = {
    obligationFarmUserState: none<Address>(),
    reserveFarmState: none<Address>(),
  };
  invariant(amountRaw > 0n && amountRaw <= 1_000_000_000_000n, `${lane.key} operation amount is outside the constrained policy cap`);
  const amount = new BN(amountRaw.toString());
  const deposit = depositReserveLiquidityAndObligationCollateralV2({ liquidityAmount: amount }, {
    depositAccounts: {
      owner, obligation: account.obligation, lendingMarket: account.market,
      lendingMarketAuthority: account.marketAuthority, reserve: account.collateralReserve,
      reserveLiquidityMint: account.collateralMint,
      reserveLiquiditySupply: account.collateralSupply,
      reserveCollateralMint: account.collateralReceiptMint,
      reserveDestinationDepositCollateral: account.collateralReceiptSupply,
      userSourceLiquidity: account.collateralCustody,
      placeholderUserDestinationCollateral: none<Address>(),
      collateralTokenProgram: account.collateralTokenProgram,
      liquidityTokenProgram: account.collateralTokenProgram,
      instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: farms, farmsProgram: RWA_MULTIPLY_ROUTE.kamino.farmsProgram,
  }, [], account.klend);
  const borrow = borrowObligationLiquidityV2({ liquidityAmount: amount }, {
    borrowAccounts: {
      owner, obligation: account.obligation, lendingMarket: account.market,
      lendingMarketAuthority: account.marketAuthority, borrowReserve: account.debtReserve,
      borrowReserveLiquidityMint: account.debtMint,
      reserveSourceLiquidity: account.debtSupply,
      borrowReserveLiquidityFeeReceiver: account.debtFeeReceiver,
      userDestinationLiquidity: account.debtCustody, referrerTokenState: none<Address>(),
      tokenProgram: account.debtTokenProgram,
      instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: farms, farmsProgram: RWA_MULTIPLY_ROUTE.kamino.farmsProgram,
  }, [], account.klend);
  const repay = repayObligationLiquidityV2({ liquidityAmount: amount }, {
    repayAccounts: {
      owner, obligation: account.obligation, lendingMarket: account.market,
      repayReserve: account.debtReserve, reserveLiquidityMint: account.debtMint,
      reserveDestinationLiquidity: account.debtSupply,
      userSourceLiquidity: account.debtCustody, tokenProgram: account.debtTokenProgram,
      instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: farms, lendingMarketAuthority: account.marketAuthority,
    farmsProgram: RWA_MULTIPLY_ROUTE.kamino.farmsProgram,
  }, [], account.klend);
  const withdraw = withdrawObligationCollateralAndRedeemReserveCollateralV2({ collateralAmount: amount }, {
    withdrawAccounts: {
      owner, obligation: account.obligation, lendingMarket: account.market,
      lendingMarketAuthority: account.marketAuthority, withdrawReserve: account.collateralReserve,
      reserveLiquidityMint: account.collateralMint,
      reserveSourceCollateral: account.collateralReceiptSupply,
      reserveCollateralMint: account.collateralReceiptMint,
      reserveLiquiditySupply: account.collateralSupply,
      userDestinationLiquidity: account.collateralCustody,
      placeholderUserDestinationCollateral: none<Address>(),
      collateralTokenProgram: account.collateralTokenProgram,
      liquidityTokenProgram: account.collateralTokenProgram,
      instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: farms, farmsProgram: RWA_MULTIPLY_ROUTE.kamino.farmsProgram,
  }, [], account.klend);
  return [
    assertInstruction(lane, "deposit", deposit, 17, true),
    assertInstruction(lane, "borrow", borrow, 15, false),
    assertInstruction(lane, "repay", repay, 13, false),
    assertInstruction(lane, "withdraw", withdraw, 17, true),
  ] as const;
}

export function resolutionLanes(value: unknown): ResolvedLane[] {
  const root = value as Json;
  invariant(root?.schema === "loyal-backyard-rwa-policy-resolution/v1"
    && root.commitment === "confirmed" && Array.isArray(root.lanes),
  "confirmed Phase-2 resolution artifact is malformed");
  return root.lanes as ResolvedLane[];
}
