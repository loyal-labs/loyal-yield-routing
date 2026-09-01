import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

import {
  borrowObligationLiquidityV2,
  depositReserveLiquidityAndObligationCollateralV2,
  repayObligationLiquidityV2,
  withdrawObligationCollateralAndRedeemReserveCollateralV2,
} from "@kamino-finance/klend-sdk";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import {
  AccountRole,
  address,
  createNoopSigner,
  isSignerRole,
  isWritableRole,
  none,
  type Address,
  type Instruction,
} from "@solana/kit";
import { Connection, PublicKey } from "@solana/web3.js";
import BN from "bn.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { resolveCurrentPhaseOnePrimeUsdcJupiterHeaders } from "./rwa-multiply-jupiter-headers.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const CATALOG_PATH = resolve(REPOSITORY_ROOT,
  "crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json");
const COMPILER = "compile-backyard-rwa-resolved-policies";
const MAX_OPERATION_RAW = 1_000_000_000_000n;
const PROOF_AMOUNT_RAW = 1_000_000n;
const INSTRUCTIONS_SYSVAR = address("Sysvar1nstructions1111111111111111111111111");
const TOKEN_PROGRAM = RWA_MULTIPLY_ROUTE.assets.tokenProgram;
const KLEND = RWA_MULTIPLY_ROUTE.kamino.program;
const FARMS = RWA_MULTIPLY_ROUTE.kamino.farmsProgram;
const MARKET_AUTHORITY = address("9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM");
const PRIME_LIQUIDITY_SUPPLY = address("FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352");
const PRIME_RECEIPT_MINT = address("FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW");
const PRIME_RECEIPT_SUPPLY = address("Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN");
const USDC_LIQUIDITY_SUPPLY = address("H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC");
const USDC_FEE_VAULT = address("BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6");

type Json = Record<string, unknown>;
type Constraint = Readonly<{
  programId: string;
  accountPubkeys: readonly Readonly<{ index: number; pubkeys: readonly string[] }>[];
  data: readonly Json[];
}>;

const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [{
    policySeed: { toString(): string } | null;
    threshold: number;
    timeLock: number;
    signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
  }, number] };
}).Settings;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function wireAccounts(ix: Instruction) {
  return ix.accounts?.map((account) => ({
    address: account.address,
    signer: isSignerRole(account.role),
    writable: isWritableRole(account.role),
  })) ?? [];
}

function kaminoConstraint(ix: Instruction): Constraint {
  const accounts = wireAccounts(ix);
  const data = ix.data;
  invariant(ix.programAddress === KLEND && data !== undefined && data.length === 16 && accounts.length > 0,
    "canonical Kamino Phase 1 instruction shape drifted");
  return {
    programId: ix.programAddress,
    accountPubkeys: accounts.map(({ address: pubkey }, index) => ({ index, pubkeys: [pubkey] })),
    data: [
      { kind: "slice-equals", offset: 0, valueHex: Buffer.from(data.subarray(0, 8)).toString("hex") },
      { kind: "u64-less-than-or-equal", offset: 8, value: Number(MAX_OPERATION_RAW) },
    ],
  };
}

function jupiterConstraint(row: Json): Constraint {
  const instruction = row.instruction as Json;
  const accounts = instruction.accounts as Json[];
  const data = Buffer.from(String(instruction.dataBase64), "base64");
  invariant(String(instruction.programId) === RWA_MULTIPLY_ROUTE.programs.jupiter
    && data.length >= 28 && Array.isArray(accounts), "Phase 1 Jupiter header is incomplete");
  const v2 = data.subarray(0, 8).equals(Buffer.from([209, 152, 83, 147, 124, 254, 216, 233]));
  const indexes = v2
    ? { authority: 1, source: 2, destination: 5, sourceMint: 6, destinationMint: 7,
      sourceProgram: 8, destinationProgram: 9, slippage: 25, fee: 27 }
    : { authority: 2, source: 3, destination: 6, sourceMint: 7, destinationMint: 8,
      sourceProgram: 0, destinationProgram: 0, slippage: data.length - 3, fee: data.length - 1 };
  const constrained = [...new Set([
    indexes.authority, indexes.source, indexes.destination, indexes.sourceMint,
    indexes.destinationMint, indexes.sourceProgram, indexes.destinationProgram,
  ])].sort((left, right) => left - right);
  return {
    programId: String(instruction.programId),
    accountPubkeys: constrained.map((index) => ({
      index,
      pubkeys: [String(accounts[index]?.pubkey)],
    })),
    data: [
      { kind: "slice-equals", offset: 0, valueHex: data.subarray(0, 8).toString("hex") },
      { kind: "u64-less-than-or-equal", offset: data.length - 19, value: Number(MAX_OPERATION_RAW) },
      { kind: "u16-less-than-or-equal", offset: indexes.slippage,
        value: RWA_MULTIPLY_ROUTE.assets.maxSlippageBps },
      { kind: "u8-equals", offset: indexes.fee, value: 0 },
    ],
  };
}

function exactKaminoInstructions() {
  const owner = createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault);
  const commonFarms = {
    obligationFarmUserState: none<Address>(),
    reserveFarmState: none<Address>(),
  };
  const amount = new BN(PROOF_AMOUNT_RAW.toString());
  const deposit = depositReserveLiquidityAndObligationCollateralV2({ liquidityAmount: amount }, {
    depositAccounts: {
      owner, obligation: RWA_MULTIPLY_ROUTE.kamino.obligation,
      lendingMarket: RWA_MULTIPLY_ROUTE.kamino.market, lendingMarketAuthority: MARKET_AUTHORITY,
      reserve: RWA_MULTIPLY_ROUTE.kamino.collateralReserve,
      reserveLiquidityMint: RWA_MULTIPLY_ROUTE.assets.collateralMint,
      reserveLiquiditySupply: PRIME_LIQUIDITY_SUPPLY, reserveCollateralMint: PRIME_RECEIPT_MINT,
      reserveDestinationDepositCollateral: PRIME_RECEIPT_SUPPLY,
      userSourceLiquidity: RWA_MULTIPLY_ROUTE.squads.collateralAta,
      placeholderUserDestinationCollateral: none<Address>(), collateralTokenProgram: TOKEN_PROGRAM,
      liquidityTokenProgram: TOKEN_PROGRAM, instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: commonFarms, farmsProgram: FARMS,
  }, [], KLEND);
  const borrow = borrowObligationLiquidityV2({ liquidityAmount: amount }, {
    borrowAccounts: {
      owner, obligation: RWA_MULTIPLY_ROUTE.kamino.obligation,
      lendingMarket: RWA_MULTIPLY_ROUTE.kamino.market, lendingMarketAuthority: MARKET_AUTHORITY,
      borrowReserve: RWA_MULTIPLY_ROUTE.kamino.debtReserve,
      borrowReserveLiquidityMint: RWA_MULTIPLY_ROUTE.assets.assetMint,
      reserveSourceLiquidity: USDC_LIQUIDITY_SUPPLY,
      borrowReserveLiquidityFeeReceiver: USDC_FEE_VAULT,
      userDestinationLiquidity: RWA_MULTIPLY_ROUTE.squads.assetAta,
      referrerTokenState: none<Address>(), tokenProgram: TOKEN_PROGRAM,
      instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: commonFarms, farmsProgram: FARMS,
  }, [], KLEND);
  const repay = repayObligationLiquidityV2({ liquidityAmount: amount }, {
    repayAccounts: {
      owner, obligation: RWA_MULTIPLY_ROUTE.kamino.obligation,
      lendingMarket: RWA_MULTIPLY_ROUTE.kamino.market,
      repayReserve: RWA_MULTIPLY_ROUTE.kamino.debtReserve,
      reserveLiquidityMint: RWA_MULTIPLY_ROUTE.assets.assetMint,
      reserveDestinationLiquidity: USDC_LIQUIDITY_SUPPLY,
      userSourceLiquidity: RWA_MULTIPLY_ROUTE.squads.assetAta,
      tokenProgram: TOKEN_PROGRAM, instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: commonFarms, lendingMarketAuthority: MARKET_AUTHORITY, farmsProgram: FARMS,
  }, [], KLEND);
  const withdraw = withdrawObligationCollateralAndRedeemReserveCollateralV2({ collateralAmount: amount }, {
    withdrawAccounts: {
      owner, obligation: RWA_MULTIPLY_ROUTE.kamino.obligation,
      lendingMarket: RWA_MULTIPLY_ROUTE.kamino.market, lendingMarketAuthority: MARKET_AUTHORITY,
      withdrawReserve: RWA_MULTIPLY_ROUTE.kamino.collateralReserve,
      reserveLiquidityMint: RWA_MULTIPLY_ROUTE.assets.collateralMint,
      reserveSourceCollateral: PRIME_RECEIPT_SUPPLY, reserveCollateralMint: PRIME_RECEIPT_MINT,
      reserveLiquiditySupply: PRIME_LIQUIDITY_SUPPLY,
      userDestinationLiquidity: RWA_MULTIPLY_ROUTE.squads.collateralAta,
      placeholderUserDestinationCollateral: none<Address>(), collateralTokenProgram: TOKEN_PROGRAM,
      liquidityTokenProgram: TOKEN_PROGRAM, instructionSysvarAccount: INSTRUCTIONS_SYSVAR,
    }, farmsAccounts: commonFarms, farmsProgram: FARMS,
  }, [], KLEND);
  return [
    { operation: "deposit", action: "OPEN_PRIME_USDC_STEP", instruction: deposit },
    { operation: "borrow", action: "OPEN_PRIME_USDC_STEP", instruction: borrow },
    { operation: "repay", action: "DELEVER_PRIME_USDC_STEP", instruction: repay },
    { operation: "withdraw", action: "DELEVER_PRIME_USDC_STEP", instruction: withdraw },
  ] as const;
}

export async function compileCurrentPhaseOnePrimeUsdcPolicies(connection: Connection) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const settingsRead = await connection.getAccountInfoAndContext(
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), { commitment: "finalized" });
  invariant(settingsRead.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program,
    "Squads Settings is absent or has the wrong owner");
  const [settings] = Settings.fromAccountInfo(settingsRead.value);
  invariant(settings.threshold === 1 && settings.timeLock === 0 && settings.signers.length === 1
    && settings.signers[0]?.key.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin
    && settings.signers[0]?.permissions.mask === 7, "Squads Settings authority boundary drifted");
  const policySeedBefore = BigInt(settings.policySeed?.toString() ?? "0");

  const currentGraph = [
    RWA_MULTIPLY_ROUTE.kamino.obligation, RWA_MULTIPLY_ROUTE.squads.collateralAta,
    RWA_MULTIPLY_ROUTE.squads.assetAta, RWA_MULTIPLY_ROUTE.kamino.collateralReserve,
    RWA_MULTIPLY_ROUTE.kamino.debtReserve,
  ];
  const graphRead = await connection.getMultipleAccountsInfoAndContext(
    currentGraph.map((value) => new PublicKey(value)),
    { commitment: "finalized", minContextSlot: settingsRead.context.slot });
  invariant(graphRead.value.every((value) => value !== null), "fixed PRIME/USDC graph is incomplete");
  invariant(graphRead.value[0]?.owner.toBase58() === KLEND
    && graphRead.value[3]?.owner.toBase58() === KLEND
    && graphRead.value[4]?.owner.toBase58() === KLEND
    && graphRead.value[1]?.owner.toBase58() === TOKEN_PROGRAM
    && graphRead.value[2]?.owner.toBase58() === TOKEN_PROGRAM,
  "fixed PRIME/USDC graph owner boundary drifted");

  const swaps = await resolveCurrentPhaseOnePrimeUsdcJupiterHeaders(connection);
  invariant(swaps.verdict === "PASS_HEADERS_RESOLVED",
    `Phase 1 Jupiter headers are blocked: ${JSON.stringify(swaps.rows)}`);
  const kamino = exactKaminoInstructions();
  const compilerInput = {
    schema: "loyal-backyard-rwa-policy-compiler-input/v1",
    addressesResolved: true,
    swapHeadersResolved: true,
    catalogSha256: sha256(readFileSync(CATALOG_PATH)),
    resolutionSha256: sha256(JSON.stringify({ slot: graphRead.context.slot, currentGraph,
      graphHashes: graphRead.value.map((value) => sha256(value!.data)), swaps })),
    settings: RWA_MULTIPLY_ROUTE.squads.settings,
    authority: RWA_MULTIPLY_ROUTE.setupAdmin,
    delegatedSigner: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
    accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
    policySeedBefore: policySeedBefore.toString(),
    policies: [
      ...kamino.map(({ operation, instruction }) => ({
        name: `lane/Prime/PRIME/USDC/${operation}`,
        semanticEdgeCount: 1,
        constraints: [kaminoConstraint(instruction)],
      })),
      { name: "swap/Prime/PRIME/USDC", semanticEdgeCount: 2,
        constraints: swaps.rows.map((row) => jupiterConstraint(row as unknown as Json)) },
    ],
  };
  const source = Buffer.from(JSON.stringify(compilerInput));
  const result = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin", COMPILER, "--", "--phase1"], {
    cwd: REPOSITORY_ROOT, input: source, encoding: "utf8", maxBuffer: 32 * 1024 * 1024,
  });
  invariant(result.status === 0, `Phase 1 policy compiler failed: ${(result.stderr || result.stdout).trim()}`);
  const artifact = JSON.parse(result.stdout) as Json;
  const policies = artifact.policies as Json[];
  invariant(artifact.phase === "phase1" && artifact.physicalPolicyCount === 5 && policies.length === 5,
    "Phase 1 compiler escaped its five-policy boundary");
  return {
    schema: "loyal-backyard-rwa-phase1-policy-bindings/v1",
    verdict: "COMPILED_SIGNED_SIMULATION_REQUIRED",
    broadcast: false,
    contextSlot: graphRead.context.slot,
    settingsSlot: settingsRead.context.slot,
    compilerInput,
    artifact,
    manifestBindings: {
      packets: kamino.map(({ operation, action, instruction }, policyConstraintIndex) => {
        invariant(instruction.data !== undefined, "canonical Kamino instruction has no data");
        const instructionData = Uint8Array.from(instruction.data);
        const policy = policies[policyConstraintIndex]!;
        return {
        operation, action, policy: policy.policy,
        policyCreateDataSha256: (policy.createInstruction as Json).dataSha256,
        policyConstraintIndex: 0,
        policyAccountDataSha256: null,
        accounts: wireAccounts(instruction),
        dataBase64: Buffer.from(instructionData).toString("base64"),
        dataSha256: sha256(instructionData),
        amountOffset: 8,
        };
      }),
      blocker: "Install/finalize the exact lane policy, then replace null policyAccountDataSha256 values with the independently read deployed policy account hash before enabling Go execution.",
    },
  } as const;
}
