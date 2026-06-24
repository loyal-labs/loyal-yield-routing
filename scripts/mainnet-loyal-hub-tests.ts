#!/usr/bin/env bun
import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  Connection,
  Keypair,
  PublicKey,
  SystemProgram,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import {
  ASSOCIATED_TOKEN_PROGRAM_ID,
  TOKEN_2022_PROGRAM_ID,
  TOKEN_PROGRAM_ID,
  createAssociatedTokenAccountIdempotentInstruction,
  createTransferCheckedInstruction,
  getAssociatedTokenAddressSync,
  getMint,
} from "@solana/spl-token";
import {
  ProgramConfig,
  getProgramConfigPda,
  getSettingsPda,
  getSmartAccountPda,
} from "@loyal-labs/loyal-smart-accounts-core";
import { createSmartAccount } from "@loyal-labs/loyal-smart-accounts-core/internal";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import { homedir } from "node:os";
import { spawnSync } from "node:child_process";

import {
  LOYAL_CLUSTER_CONFIGS,
  LoyalCluster,
  MaxFeeBps,
  TREASURY_REBALANCE_ACTION_SEED,
  assertRebalanceAvoidsActiveLanes,
  createLoyalActionsSdk,
  createProgramInteractionPolicyUpdateInstruction,
  deriveActionAccount,
  createSquadsProgramInteractionExecutionInstruction,
  createSquadsSyncTransactionInstruction,
} from "../packages/loyal-actions/src/index.js";
import {
  uniquePubkeys,
} from "../packages/loyal-actions/src/internal/protocols.js";
import { BytesEncoder } from "../packages/loyal-actions/src/internal/bytes.js";
import type { AccountConstraint, DataConstraint, InstructionConstraint } from "../packages/loyal-actions/src/internal/squads.js";

const DEFAULT_CLUSTER = "mainnet-beta";
const DEFAULT_STATE_FILE = ".agents/loyal-hub-mainnet-test-state.json";
const DEFAULT_ROUTE_WITHDRAW_FILE = "tmp/withdraw-usdc-kamino.json";
const DEFAULT_ROUTE_DEPOSIT_FILE = "tmp/deposit-pyusd-kamino.json";
const DEFAULT_POLICY_SETUP_FILE = "tmp/policy-setup-kamino-usdc.json";
const DEFAULT_HUB_PROGRAM = new PublicKey("LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH");
const DEFAULT_SQUADS_TREASURY = new PublicKey("init9xckLHfofCRp5SCisRK4f6eDehGRtFSAw5mLhE8");
const USDC_MINT = new PublicKey("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
const PYUSD_MINT = new PublicKey("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo");
const DEFAULT_COMMITMENT = "confirmed";
const DEFAULT_QUOTE_API = "https://api.jup.ag/swap/v1/quote";
const DEFAULT_SWAP_INSTRUCTIONS_API = "https://api.jup.ag/swap/v1/swap-instructions";
const CONFIG_SEED = Buffer.from("config");
const HUB_AUTHORITY_SEED = Buffer.from("hub-authority");
const PENDING_ADMIN_SEED = Buffer.from("pending-admin");

const SET_INVENTORY_REBALANCER_TAG = 8;
const SET_HUB_AUTHORIZER_TAG = 7;
const REQUEST_ADMIN_TRANSFER_TAG = 10;
const ACCEPT_ADMIN_TRANSFER_TAG = 11;
const SWAP_EXACT_IN_TAG = 1;
const WITHDRAW_INVENTORY_TAG = 2;
const REBALANCE_INVENTORY_TAG = 5;
const MAX_REBALANCE_TRANSFERS = 16;
const DEFAULT_YIELD_ROUTE_POLICY_SEED = 1n;
const DEFAULT_TREASURY_REBALANCE_POLICY_SEED = TREASURY_REBALANCE_ACTION_SEED;
const SQUADS_FULL_PERMISSIONS_MASK = 7;
const SQUADS_SYNC_SIGNER_COUNT = 1;
const EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR = [138, 209, 64, 163, 79, 67, 233, 76] as const;

type ParsedArgs = Record<string, string[]>;

type WireInstruction = {
  programId: string;
  accounts: {
    pubkey: string;
    isSigner?: boolean;
    isWritable?: boolean;
  }[];
  data: string | number[];
  encoding?: "base64" | "hex" | "bytes";
};

type JupiterInstructionJson = {
  programId: string;
  accounts: {
    pubkey: string;
    isSigner: boolean;
    isWritable: boolean;
  }[];
  data: string;
};

type JupiterSwapInstructions = {
  computeBudgetInstructions?: JupiterInstructionJson[];
  setupInstructions?: JupiterInstructionJson[];
  swapInstruction: JupiterInstructionJson;
  cleanupInstruction?: JupiterInstructionJson;
  addressLookupTableAddresses?: string[];
};

type MintInfo = {
  tokenProgram: PublicKey;
  decimals: number;
};

type HubState = {
  admin: string;
  hub_authorizer: string;
  inventory_rebalancer: string;
  max_fee_bps: number;
  paused: boolean;
  lanes: {
    lane_id: number;
    authority: string;
    inventory: {
      mint: string;
      token_program: string;
      account: string;
      exists: boolean;
      amount: number | string | null;
    }[];
  }[];
};

type StoredVault = {
  settings: string;
  vault: string;
  settingsSeed?: string;
};

type StoredTreasuryVault = StoredVault & {
  policy?: string;
  rebalanceIndexes?: number[];
  rebalancePolicySpec?: string;
};

type TestState = {
  version: 1;
  cluster: string;
  hubProgram: string;
  system: string;
  original?: {
    admin?: string;
    hubAuthorizer?: string;
    inventoryRebalancer?: string;
  };
  initialHubBalances?: Record<string, string>;
  user?: StoredVault & {
    policy?: string;
    routeIndexes?: {
      loyal?: number[];
      jupiter?: number[];
      sameMint?: number[];
    };
  };
  treasury?: StoredTreasuryVault;
  steps?: Record<string, { signature: string | null; at: string }>;
  updatedAt?: string;
};

type TransactionRun = {
  mode: "simulate" | "execute";
  signature: string | null;
  unitsConsumed: number | null;
};

const args = parseArgs(process.argv.slice(2));
if (hasFlag(args, "help") || hasFlag(args, "h")) {
  showHelp();
  process.exit(0);
}

const cluster = value(args, "cluster") ?? DEFAULT_CLUSTER;
const loyalCluster = parseLoyalCluster(cluster);
const clusterConfig = LOYAL_CLUSTER_CONFIGS[loyalCluster];
const connection = new Connection(resolveRpcUrl(cluster), DEFAULT_COMMITMENT);
const systemKeypair = loadKeypair(resolveSystemKeypairPath());
const system = systemKeypair.publicKey;
const hubProgram = pubkey(value(args, "hub-program") ?? DEFAULT_HUB_PROGRAM.toBase58(), "hub-program");
const squadsTreasury = pubkey(value(args, "squads-treasury") ?? DEFAULT_SQUADS_TREASURY.toBase58(), "squads-treasury");
const stateFile = value(args, "state-file") ?? DEFAULT_STATE_FILE;
const executeLive = hasFlag(args, "execute") && !hasFlag(args, "simulate-only");
const cleanupOnly = hasFlag(args, "cleanup-only");
const forceRerun = hasFlag(args, "force-rerun");
const state = loadState(stateFile, cluster, hubProgram, system);

applyManualResumeFlags(state, args);

if (!cleanupOnly && !hasFlag(args, "skip-policy")) {
  defaultArg(args, "route-withdraw-file", DEFAULT_ROUTE_WITHDRAW_FILE);
  defaultArg(args, "route-deposit-file", DEFAULT_ROUTE_DEPOSIT_FILE);
  defaultArg(args, "policy-setup-file", DEFAULT_POLICY_SETUP_FILE);
  required(args, "route-withdraw-file");
  required(args, "route-deposit-file");
}

if (!executeLive) {
  console.log("simulate-only mode: no transactions will be submitted");
} else if (cluster === "mainnet-beta" && process.env.CONFIRM_MAINNET !== "1") {
  throw new Error("live mainnet execution requires CONFIRM_MAINNET=1");
}

await main();

async function main(): Promise<void> {
  console.log(`system: ${system.toBase58()}`);
  console.log(`state: ${stateFile}`);

  const hubState = await fetchHubState();
  rememberOriginalHubAuthorities(hubState);
  rememberInitialHubBalances(hubState);
  saveState();

  if (cleanupOnly) {
    await cleanup("cleanup-only");
    return;
  }

  await ensureHubInventory();
  state.treasury = await ensureVault("treasury", state.treasury);

  if (!hasFlag(args, "skip-policy")) {
    state.user = await ensureVault("user", state.user);
    await ensureMainnetKaminoRouteFiles(usesDefaultRouteFiles() || hasFlag(args, "refresh-route-files"));
    await ensureAllInOnePolicy();
    await fundUserVaultForPolicy();
    await ensureUserVaultTokenAccounts();
    await ensureUserVaultLamportsForPolicySetup();
    await runPolicySetup();
    await ensureMainnetKaminoRouteFiles(usesDefaultRouteFiles() || hasFlag(args, "refresh-route-files"));
    await runPolicyRoute();
  }

  if (!hasFlag(args, "skip-treasury-rebalance")) {
    await ensureTreasuryRebalancePolicy();
    await runTreasuryJupiterRebalance();
  }

  if (!hasFlag(args, "skip-lane-rebalance")) {
    await runActiveLaneRejectionCheck();
    await withTreasuryInventoryRebalancer(async () => {
      await runLaneRebalance();
    });
  }

  if (!hasFlag(args, "skip-cleanup")) {
    await cleanup("final");
  }

  saveState();
  console.log("mainnet Loyal Hub test script finished");
}

async function ensureHubInventory(): Promise<void> {
  const perLaneRaw = u64(value(args, "inventory-per-lane-raw") ?? "1250000", "inventory-per-lane-raw");
  const mintInfos = new Map<string, MintInfo>();
  for (const mint of [USDC_MINT, PYUSD_MINT]) {
    mintInfos.set(mint.toBase58(), await fetchMintInfo(mint));
  }

  const instructions: TransactionInstruction[] = [];
  const stateNow = await fetchHubState();
  for (const lane of [0, 1]) {
    for (const mint of [USDC_MINT, PYUSD_MINT]) {
      const key = hubBalanceKey(lane, mint);
      const initial = BigInt(state.initialHubBalances?.[key] ?? "0");
      const desired = initial + perLaneRaw;
      const current = hubInventoryAmount(stateNow, lane, mint);
      if (current >= desired) {
        console.log(`hub inventory ok lane=${lane} mint=${mint.toBase58()} amount=${current}`);
        continue;
      }
      const mintInfo = requiredMintInfo(mintInfos, mint);
      const amount = desired - current;
      const hubAuthority = deriveHubAuthority(hubProgram, lane);
      const destination = associatedTokenAddress(mint, hubAuthority, mintInfo.tokenProgram, true);
      const source = associatedTokenAddress(mint, system, mintInfo.tokenProgram, false);
      instructions.push(
        createAssociatedTokenAccountIdempotentInstruction(
          system,
          destination,
          hubAuthority,
          mint,
          mintInfo.tokenProgram,
          ASSOCIATED_TOKEN_PROGRAM_ID,
        ),
      );
      instructions.push(
        createTransferCheckedInstruction(
          source,
          mint,
          destination,
          system,
          amount,
          mintInfo.decimals,
          [],
          mintInfo.tokenProgram,
        ),
      );
      console.log(`queue seed lane=${lane} mint=${mint.toBase58()} amount=${amount}`);
    }
  }

  if (instructions.length === 0) {
    return;
  }
  await sendTransaction("seed-hub-inventory", instructions, [systemKeypair]);
}

async function ensureVault(kind: "user" | "treasury", existing: StoredVault | undefined): Promise<StoredVault> {
  if (existing && (await accountExists(new PublicKey(existing.settings)))) {
    console.log(`${kind} vault exists: ${existing.vault}`);
    return existing;
  }

  const verifier = system;
  const programConfig = getProgramConfigPda({ programId: clusterConfig.squadsSmartAccountProgramId })[0];
  const config = await ProgramConfig.fromAccountAddress(connection, programConfig, DEFAULT_COMMITMENT);
  const settingsSeed = toBigInt(config.smartAccountIndex, "smartAccountIndex") + 1n;
  const settings = getSettingsPda({
    accountIndex: settingsSeed,
    programId: clusterConfig.squadsSmartAccountProgramId,
  })[0];
  const vault = getSmartAccountPda({
    settingsPda: settings,
    accountIndex: 0,
    programId: clusterConfig.squadsSmartAccountProgramId,
  })[0];

  const instructions = [
    createSmartAccount({
      treasury: squadsTreasury,
      creator: system,
      settings,
      settingsAuthority: null,
      threshold: 1,
      signers: [{ key: verifier, permissions: { mask: 7 } }],
      timeLock: 0,
      rentCollector: null,
      programId: clusterConfig.squadsSmartAccountProgramId,
    }),
  ];

  const fundVaultLamports = value(args, "fund-vault-lamports");
  if (fundVaultLamports) {
    instructions.push(
      SystemProgram.transfer({
        fromPubkey: system,
        toPubkey: vault,
        lamports: Number(u64(fundVaultLamports, "fund-vault-lamports")),
      }),
    );
  }

  const run = await sendTransaction(`create-${kind}-vault`, instructions, [systemKeypair]);
  const next = {
    settings: settings.toBase58(),
    vault: vault.toBase58(),
    settingsSeed: settingsSeed.toString(),
  };
  if (run.mode === "execute") {
    state[kind] = next;
    saveState();
  }
  console.log(`${kind} settings=${settings.toBase58()} vault=${vault.toBase58()}`);
  return next;
}

async function ensureAllInOnePolicy(): Promise<void> {
  const user = requireUser();
  const settings = new PublicKey(user.settings);
  const vault = new PublicKey(user.vault);
  const policySeed = yieldRoutePolicySeed();
  const actionAccount = deriveActionAccount(clusterConfig, settings, policySeed);
  if (user.policy && user.policy !== actionAccount.toBase58()) {
    console.log(`switching policy seed to ${policySeed}: ${actionAccount.toBase58()}`);
  }
  const existingPolicy = await accountExists(actionAccount);

  const policyUniverse = routePolicyUniverseFromFiles();
  const kaminoRoutePrograms = uniquePubkeys([
    ...policyUniverse.withdrawInstructions,
    ...policyUniverse.depositInstructions,
  ].map((instruction) => instruction.programId));
  if (kaminoRoutePrograms.length === 0) {
    throw new Error("route policy requires at least one Kamino route instruction");
  }
  const constraints = [
    ...kaminoRoutePrograms.map(routeProgramConstraint),
    routeHubConstraint(vault, policyUniverse.stableMints, MaxFeeBps.Bps50),
  ];
  const hubIndex = kaminoRoutePrograms.length;
  const kaminoConstraintIndex = (instruction: TransactionInstruction): number => {
    const index = kaminoRoutePrograms.findIndex((programId) => programId.equals(instruction.programId));
    if (index < 0) {
      throw new Error(`missing Kamino route constraint for program ${instruction.programId.toBase58()}`);
    }
    return index;
  };
  const withdrawIndexes = policyUniverse.withdrawInstructions.map(kaminoConstraintIndex);
  const depositIndexes = policyUniverse.depositInstructions.map(kaminoConstraintIndex);
  const routeIndexes = {
    loyal: [...withdrawIndexes, hubIndex, ...depositIndexes],
    jupiter: [],
    sameMint: [...withdrawIndexes, ...depositIndexes],
  };

  if (existingPolicy && !hasFlag(args, "update-policy")) {
    user.policy = actionAccount.toBase58();
    user.routeIndexes = routeIndexes;
    saveState();
    console.log(`policy exists: ${user.policy}`);
    return;
  }

  const instruction = createRoutePolicyInstruction({
    settings,
    authority: system,
    delegatedSigner: system,
    accountIndex: 0,
    vault,
    policySeed,
    actionAccount,
    constraints,
  });
  const updateInstruction = createRoutePolicyUpdateInstruction({
    settings,
    authority: system,
    delegatedSigner: system,
    accountIndex: 0,
    policy: actionAccount,
    constraints,
  });

  const run = existingPolicy
    ? await sendTransaction("update-route-policy", [updateInstruction], [systemKeypair])
    : await sendTransaction("create-route-policy", [instruction], [systemKeypair]);
  user.policy = actionAccount.toBase58();
  user.routeIndexes = routeIndexes;
  if (run.mode === "execute") {
    saveState();
  }
  console.log(`policy=${user.policy} loyalRoute=${(user.routeIndexes?.loyal ?? []).join(",")}`);
}

function yieldRoutePolicySeed(): bigint {
  return u64(value(args, "policy-seed") ?? DEFAULT_YIELD_ROUTE_POLICY_SEED.toString(), "policy-seed");
}

async function ensureTreasuryRebalancePolicy(): Promise<void> {
  const treasury = requireTreasury();
  const settings = new PublicKey(treasury.settings);
  const vault = new PublicKey(treasury.vault);
  const policySeed = treasuryRebalancePolicySeed();
  const laneId = numberValue(args, "treasury-lane-id", 0);
  const maxWithdrawAmount = u64(value(args, "treasury-rebalance-in-raw") ?? "500000", "treasury-rebalance-in-raw");
  const maxTopUpAmount = u64(value(args, "treasury-rebalance-topup-raw") ?? "495000", "treasury-rebalance-topup-raw");
  const maxSlippageBps = numberValue(args, "slippage-bps", 50);
  const inputMintInfo = await fetchMintInfo(USDC_MINT);
  const outputMintInfo = await fetchMintInfo(PYUSD_MINT);
  const policy = createLoyalActionsSdk({ cluster: loyalCluster }).initTreasuryLoyalHubRebalancePolicy({
    laneId,
    inputMint: USDC_MINT,
    outputMint: PYUSD_MINT,
    inputTokenProgram: inputMintInfo.tokenProgram,
    outputTokenProgram: outputMintInfo.tokenProgram,
    outputMintDecimals: outputMintInfo.decimals,
    maxWithdrawAmount,
    maxTopUpAmount,
    maxSlippageBps,
    squads: {
      settings,
      authority: system,
      delegatedSigner: system,
      accountIndex: 0,
      vault,
      policySeed,
    },
  });
  const actionAccount = policy.actionAccount;
  const policySpec = treasuryRebalancePolicySpec({
    laneId,
    inputMintInfo,
    outputMintInfo,
    maxWithdrawAmount,
    maxTopUpAmount,
    maxSlippageBps,
  });

  if (treasury.policy && treasury.policy !== actionAccount.toBase58()) {
    console.log(`switching treasury rebalance policy seed to ${policySeed}: ${actionAccount.toBase58()}`);
  }

  const existingPolicy = await accountExists(actionAccount);
  const shouldUpdate = existingPolicy
    && (hasFlag(args, "update-treasury-policy") || treasury.rebalancePolicySpec !== policySpec);

  if (existingPolicy && !shouldUpdate) {
    treasury.policy = actionAccount.toBase58();
    treasury.rebalanceIndexes = [...policy.route.instructionConstraintIndexes];
    treasury.rebalancePolicySpec = policySpec;
    saveState();
    console.log(`treasury rebalance policy exists: ${treasury.policy}`);
    return;
  }

  const createInstruction = policy.instructions[0];
  if (!createInstruction) {
    throw new Error("treasury rebalance policy builder returned no setup instruction");
  }
  const updateInstruction = createProgramInteractionPolicyUpdateInstruction(
    clusterConfig,
    {
      settings,
      authority: system,
      delegatedSigner: system,
      accountIndex: 0,
    },
    actionAccount,
    policy.constraints,
  );
  const run = existingPolicy
    ? await sendTransaction("update-treasury-rebalance-policy", [updateInstruction], [systemKeypair])
    : await sendTransaction("create-treasury-rebalance-policy", [createInstruction], [systemKeypair]);

  treasury.policy = actionAccount.toBase58();
  treasury.rebalanceIndexes = [...policy.route.instructionConstraintIndexes];
  treasury.rebalancePolicySpec = policySpec;
  if (run.mode === "execute") {
    saveState();
  }
  console.log(`treasuryPolicy=${treasury.policy} rebalanceRoute=${treasury.rebalanceIndexes.join(",")}`);
}

function treasuryRebalancePolicySeed(): bigint {
  return u64(
    value(args, "treasury-policy-seed") ?? DEFAULT_TREASURY_REBALANCE_POLICY_SEED.toString(),
    "treasury-policy-seed",
  );
}

function treasuryRebalancePolicySpec(input: {
  laneId: number;
  inputMintInfo: MintInfo;
  outputMintInfo: MintInfo;
  maxWithdrawAmount: bigint;
  maxTopUpAmount: bigint;
  maxSlippageBps: number;
}): string {
  return JSON.stringify({
    laneId: input.laneId,
    inputMint: USDC_MINT.toBase58(),
    outputMint: PYUSD_MINT.toBase58(),
    inputTokenProgram: input.inputMintInfo.tokenProgram.toBase58(),
    outputTokenProgram: input.outputMintInfo.tokenProgram.toBase58(),
    outputMintDecimals: input.outputMintInfo.decimals,
    maxWithdrawAmount: input.maxWithdrawAmount.toString(),
    maxTopUpAmount: input.maxTopUpAmount.toString(),
    maxSlippageBps: input.maxSlippageBps,
  });
}

function routePolicyUniverseFromFiles(): {
  withdrawInstructions: TransactionInstruction[];
  depositInstructions: TransactionInstruction[];
  kaminoMarkets: PublicKey[];
  liquidityMints: PublicKey[];
  stableMints: PublicKey[];
} {
  const withdrawInstructions = loadWireInstructions(required(args, "route-withdraw-file"));
  const depositInstructions = loadWireInstructions(required(args, "route-deposit-file"));
  const withdraw = lastWireInstruction(withdrawInstructions, "route withdraw");
  const deposit = lastWireInstruction(depositInstructions, "route deposit");
  const withdrawMarket = instructionKey(withdraw, 2, "withdraw market");
  const withdrawMint = instructionKey(withdraw, 5, "withdraw liquidity mint");
  const depositMarket = instructionKey(deposit, 2, "deposit market");
  const depositMint = instructionKey(deposit, 5, "deposit liquidity mint");
  const kaminoMarkets = uniquePubkeys([withdrawMarket, depositMarket]);
  const liquidityMints = uniquePubkeys([withdrawMint, depositMint]);
  console.log(
    `policy scope markets=${kaminoMarkets.map((key) => key.toBase58()).join(",")} mints=${liquidityMints.map((key) => key.toBase58()).join(",")}`,
  );
  return {
    withdrawInstructions,
    depositInstructions,
    kaminoMarkets,
    liquidityMints,
    stableMints: liquidityMints,
  };
}

function createRoutePolicyInstruction(input: {
  settings: PublicKey;
  authority: PublicKey;
  delegatedSigner: PublicKey;
  accountIndex: number;
  vault: PublicKey;
  policySeed: bigint;
  actionAccount: PublicKey;
  constraints: InstructionConstraint[];
}): TransactionInstruction {
  const data = serializeRawPolicyCreateAction(
    input.delegatedSigner,
    input.policySeed,
    input.accountIndex,
    input.constraints,
  );
  return new TransactionInstruction({
    programId: clusterConfig.squadsSmartAccountProgramId,
    keys: [
      { pubkey: input.settings, isSigner: false, isWritable: true },
      { pubkey: input.authority, isSigner: true, isWritable: true },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
      { pubkey: clusterConfig.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: input.authority, isSigner: true, isWritable: false },
      { pubkey: input.actionAccount, isSigner: false, isWritable: true },
    ],
    data: Buffer.from(data),
  });
}

function createRoutePolicyUpdateInstruction(input: {
  settings: PublicKey;
  authority: PublicKey;
  delegatedSigner: PublicKey;
  accountIndex: number;
  policy: PublicKey;
  constraints: InstructionConstraint[];
}): TransactionInstruction {
  const data = serializeRawPolicyUpdateAction(
    input.policy,
    input.delegatedSigner,
    input.accountIndex,
    input.constraints,
  );
  return new TransactionInstruction({
    programId: clusterConfig.squadsSmartAccountProgramId,
    keys: [
      { pubkey: input.settings, isSigner: false, isWritable: true },
      { pubkey: input.authority, isSigner: true, isWritable: true },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
      { pubkey: clusterConfig.squadsSmartAccountProgramId, isSigner: false, isWritable: false },
      { pubkey: input.authority, isSigner: true, isWritable: false },
      { pubkey: input.policy, isSigner: false, isWritable: true },
    ],
    data: Buffer.from(data),
  });
}

function serializeRawPolicyCreateAction(
  delegatedSigner: PublicKey,
  seed: bigint,
  accountIndex: number,
  constraints: InstructionConstraint[],
): Uint8Array {
  const encoder = new BytesEncoder();
  encoder.pushBytes(EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR);
  encoder.pushU8(SQUADS_SYNC_SIGNER_COUNT);
  encoder.pushVec([undefined], () => {
    encoder.pushU8(7);
    encoder.pushU64(seed);
    encoder.pushU8(3);
    encodeRawProgramInteractionPayload(encoder, accountIndex, constraints);
    encoder.pushVec([delegatedSigner], (signer) => {
      encoder.pushPubkey(signer);
      encoder.pushU8(SQUADS_FULL_PERMISSIONS_MASK);
    });
    encoder.pushU16(1);
    encoder.pushU32(0);
    encoder.pushOption<bigint>(undefined, (timestamp) => encoder.pushU64(timestamp));
    encoder.pushOption<never>(undefined, () => undefined);
  });
  encoder.pushOption<string>(undefined, (memo) => {
    const bytes = new TextEncoder().encode(memo);
    encoder.pushU32(bytes.length);
    encoder.pushBytes(bytes);
  });
  return encoder.finish();
}

function serializeRawPolicyUpdateAction(
  policy: PublicKey,
  delegatedSigner: PublicKey,
  accountIndex: number,
  constraints: InstructionConstraint[],
): Uint8Array {
  const encoder = new BytesEncoder();
  encoder.pushBytes(EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR);
  encoder.pushU8(SQUADS_SYNC_SIGNER_COUNT);
  encoder.pushVec([undefined], () => {
    encoder.pushU8(8);
    encoder.pushPubkey(policy);
    encoder.pushVec([delegatedSigner], (signer) => {
      encoder.pushPubkey(signer);
      encoder.pushU8(SQUADS_FULL_PERMISSIONS_MASK);
    });
    encoder.pushU16(1);
    encoder.pushU32(0);
    encoder.pushU8(3);
    encodeRawProgramInteractionPayload(encoder, accountIndex, constraints);
    encoder.pushOption<never>(undefined, () => undefined);
  });
  encoder.pushOption<string>(undefined, (memo) => {
    const bytes = new TextEncoder().encode(memo);
    encoder.pushU32(bytes.length);
    encoder.pushBytes(bytes);
  });
  return encoder.finish();
}

function encodeRawProgramInteractionPayload(
  encoder: BytesEncoder,
  accountIndex: number,
  constraints: InstructionConstraint[],
): void {
  encoder.pushU8(accountIndex);
  encoder.pushVec(constraints, (constraint) => encodeRawInstructionConstraint(encoder, constraint));
  encoder.pushOption<never>(undefined, () => undefined);
  encoder.pushOption<never>(undefined, () => undefined);
  encoder.pushVec([], () => undefined);
}

function encodeRawInstructionConstraint(encoder: BytesEncoder, constraint: InstructionConstraint): void {
  encoder.pushPubkey(constraint.programId);
  encoder.pushVec(constraint.accountConstraints, (accountConstraint) => {
    encodeRawAccountConstraint(encoder, accountConstraint);
  });
  encoder.pushVec(constraint.dataConstraints, (dataConstraint) => encodeRawDataConstraint(encoder, dataConstraint));
}

function encodeRawAccountConstraint(encoder: BytesEncoder, constraint: AccountConstraint): void {
  encoder.pushU8(constraint.accountIndex);
  if (constraint.kind.type === "pubkey") {
    encoder.pushU8(0);
    encoder.pushVec(constraint.kind.pubkeys, (pubkey) => encoder.pushPubkey(pubkey));
  } else {
    encoder.pushU8(1);
    encoder.pushVec(constraint.kind.dataConstraints, (dataConstraint) => encodeRawDataConstraint(encoder, dataConstraint));
  }
  encoder.pushOption(constraint.owner, (owner) => encoder.pushPubkey(owner));
}

function encodeRawDataConstraint(encoder: BytesEncoder, constraint: DataConstraint): void {
  encoder.pushU64(constraint.dataOffset);
  switch (constraint.dataValue.type) {
    case "u8":
      encoder.pushU8(0);
      encoder.pushU8(constraint.dataValue.value);
      break;
    case "u16Le":
      encoder.pushU8(1);
      encoder.pushU16(constraint.dataValue.value);
      break;
    case "u32Le":
      encoder.pushU8(2);
      encoder.pushU32(constraint.dataValue.value);
      break;
    case "u64Le":
      encoder.pushU8(3);
      encoder.pushU64(constraint.dataValue.value);
      break;
    case "u128Le":
      encoder.pushU8(4);
      encoder.pushU64(constraint.dataValue.value & 0xffffffffffffffffn);
      encoder.pushU64(constraint.dataValue.value >> 64n);
      break;
    case "u8Slice":
      encoder.pushU8(5);
      encoder.pushVec(constraint.dataValue.value, (byte) => encoder.pushU8(byte));
      break;
  }
  encoder.pushU8(routeOperatorTag(constraint.operator));
}

function routeOperatorTag(operator: DataConstraint["operator"]): number {
  switch (operator) {
    case "equals":
      return 0;
    case "notEquals":
      return 1;
    case "greaterThan":
      return 2;
    case "greaterThanOrEqualTo":
      return 3;
    case "lessThan":
      return 4;
    case "lessThanOrEqualTo":
      return 5;
  }
}

function routeProgramConstraint(programId: PublicKey): InstructionConstraint {
  return {
    programId,
    accountConstraints: [],
    dataConstraints: [],
  };
}

function routeHubConstraint(vault: PublicKey, stableMints: PublicKey[], maxFeeBps: number): InstructionConstraint {
  const hubAuthorizer = policyHubAuthorizer(vault);
  return {
    programId: hubProgram,
    accountConstraints: [
      routePubkeyConstraint(0, [deriveConfig(hubProgram)]),
      routePubkeyConstraint(1, [vault]),
      routePubkeyConstraint(6, stableMints),
      routePubkeyConstraint(7, stableMints),
      routePubkeyConstraint(9, [hubAuthorizer]),
    ],
    dataConstraints: [
      routeDataU8Equals(0n, SWAP_EXACT_IN_TAG),
      routeDataU16LeLessThanOrEqualTo(25n, maxFeeBps),
    ],
  };
}

function policyHubAuthorizer(vault: PublicKey): PublicKey {
  return hasFlag(args, "allow-authority-handoff") ? vault : clusterConfig.loyalHubAuthorizer;
}

function routePubkeyConstraint(accountIndex: number, pubkeys: PublicKey[]): AccountConstraint {
  return {
    accountIndex,
    kind: { type: "pubkey" as const, pubkeys },
  };
}

function routeDataU8Equals(offset: bigint, value: number): DataConstraint {
  return {
    dataOffset: offset,
    dataValue: { type: "u8", value },
    operator: "equals",
  };
}

function routeDataU16LeLessThanOrEqualTo(offset: bigint, value: number): DataConstraint {
  return {
    dataOffset: offset,
    dataValue: { type: "u16Le", value },
    operator: "lessThanOrEqualTo",
  };
}

function lastWireInstruction(instructions: TransactionInstruction[], label: string): TransactionInstruction {
  const instruction = instructions[instructions.length - 1];
  if (!instruction) {
    throw new Error(`${label} file contains no instructions`);
  }
  return instruction;
}

function instructionKey(instruction: TransactionInstruction, index: number, label: string): PublicKey {
  const key = instruction.keys[index]?.pubkey;
  if (!key) {
    throw new Error(`missing ${label} account at index ${index}`);
  }
  return key;
}

async function fundUserVaultForPolicy(): Promise<void> {
  if (stepDone("policy-setup")) {
    return;
  }
  const raw = u64(value(args, "user-vault-usdc-fund-raw") ?? value(args, "policy-amount-in-raw") ?? "1000000", "user-vault-usdc-fund-raw");
  if (raw === 0n) {
    return;
  }
  const user = requireUser();
  const mintInfo = await fetchMintInfo(USDC_MINT);
  const userVault = new PublicKey(user.vault);
  const source = associatedTokenAddress(USDC_MINT, system, mintInfo.tokenProgram, false);
  const destination = associatedTokenAddress(USDC_MINT, userVault, mintInfo.tokenProgram, true);
  const current = await fetchTokenBalance(destination);
  if (current >= raw) {
    console.log(`user vault USDC liquid balance ok: ${current}`);
    return;
  }

  await sendTransaction("fund-user-vault-usdc", [
    createAssociatedTokenAccountIdempotentInstruction(
      system,
      destination,
      userVault,
      USDC_MINT,
      mintInfo.tokenProgram,
      ASSOCIATED_TOKEN_PROGRAM_ID,
    ),
    createTransferCheckedInstruction(
      source,
      USDC_MINT,
      destination,
      system,
      raw - current,
      mintInfo.decimals,
      [],
      mintInfo.tokenProgram,
    ),
  ], [systemKeypair]);
}

async function ensureUserVaultTokenAccounts(): Promise<void> {
  const user = requireUser();
  const vault = new PublicKey(user.vault);
  const instructions: TransactionInstruction[] = [];
  for (const mint of [USDC_MINT, PYUSD_MINT]) {
    const mintInfo = await fetchMintInfo(mint);
    const ata = associatedTokenAddress(mint, vault, mintInfo.tokenProgram, true);
    if (await accountExists(ata)) {
      continue;
    }
    instructions.push(
      createAssociatedTokenAccountIdempotentInstruction(
        system,
        ata,
        vault,
        mint,
        mintInfo.tokenProgram,
        ASSOCIATED_TOKEN_PROGRAM_ID,
      ),
    );
  }
  if (instructions.length > 0) {
    await sendTransaction("ensure-user-vault-token-accounts", instructions, [systemKeypair]);
  }
}

async function ensureUserVaultLamportsForPolicySetup(): Promise<void> {
  if (stepDone("policy-setup")) {
    return;
  }
  const target = u64(value(args, "user-vault-setup-lamports") ?? "45000000", "user-vault-setup-lamports");
  if (target === 0n) {
    return;
  }
  const user = requireUser();
  const vault = new PublicKey(user.vault);
  const current = BigInt(await connection.getBalance(vault, DEFAULT_COMMITMENT));
  if (current >= target) {
    console.log(`user vault SOL setup balance ok: ${current}`);
    return;
  }
  await sendTransaction("fund-user-vault-sol", [
    SystemProgram.transfer({
      fromPubkey: system,
      toPubkey: vault,
      lamports: Number(target - current),
    }),
  ], [systemKeypair]);
}

async function ensureMainnetKaminoRouteFiles(force: boolean): Promise<void> {
  if (hasFlag(args, "skip-route-file-generation") || hasFlag(args, "skip-policy")) {
    return;
  }
  const user = requireUser();
  const routeWithdrawFile = required(args, "route-withdraw-file");
  const routeDepositFile = required(args, "route-deposit-file");
  const policySetupFile = required(args, "policy-setup-file");
  if (
    !force
    && existsSync(routeWithdrawFile)
    && existsSync(routeDepositFile)
    && existsSync(policySetupFile)
  ) {
    return;
  }

  const generatorArgs = [
    "run",
    "hub:mainnet-route-files",
    "--",
    "--vault",
    user.vault,
    "--rpc-url",
    resolveRpcUrl(cluster),
    "--setup-amount-raw",
    value(args, "policy-amount-in-raw") ?? "1000000",
    "--route-deposit-amount-raw",
    value(args, "policy-amount-out-raw") ?? "995000",
    "--route-withdraw-file",
    routeWithdrawFile,
    "--route-deposit-file",
    routeDepositFile,
    "--policy-setup-file",
    policySetupFile,
  ];
  const sourceReserve = value(args, "kamino-source-reserve");
  if (sourceReserve) {
    generatorArgs.push("--source-reserve", sourceReserve);
  }
  const targetReserve = value(args, "kamino-target-reserve");
  if (targetReserve) {
    generatorArgs.push("--target-reserve", targetReserve);
  }

  const result = spawnSync("bun", generatorArgs, {
    cwd: process.cwd(),
    encoding: "utf8",
    env: process.env,
  });
  if (result.status !== 0) {
    throw new Error(`Kamino route-file generation failed:\n${result.stdout}\n${result.stderr}`);
  }
  console.log(result.stdout.trim());
}

async function runPolicySetup(): Promise<void> {
  const files = values(args, "policy-setup-file");
  if (files.length === 0 || stepDone("policy-setup")) {
    return;
  }
  const user = requireUser();
  const vault = new PublicKey(user.vault);
  const instructions = files.flatMap(loadWireInstructions).map((instruction) => clearSignerForPubkey(instruction, vault));
  const wrapper = createSquadsSyncTransactionInstruction(clusterConfig, {
    settings: new PublicKey(user.settings),
    signer: system,
    accountIndex: 0,
    instructions,
  });
  const run = await sendTransaction("policy-setup", [wrapper], [systemKeypair], routeLookupTables());
  markStep("policy-setup", run);
}

async function runPolicyRoute(): Promise<void> {
  if (stepDone("policy-route")) {
    return;
  }
  const user = requireUser();
  if (!user.policy) {
    throw new Error("missing user policy");
  }
  const policy = new PublicKey(user.policy);
  const vault = new PublicKey(user.vault);
  const hubSwap = await buildHubSwapInstruction(vault);
  const withdrawInstructions = loadWireInstructions(required(args, "route-withdraw-file"));
  const depositInstructions = loadWireInstructions(required(args, "route-deposit-file"));
  const routeIndexes = values(args, "constraint-indexes").length > 0
    ? parseU8List(required(args, "constraint-indexes"), "constraint-indexes")
    : user.routeIndexes?.loyal ?? [];
  const expectedIndexCount = withdrawInstructions.length + 1 + depositInstructions.length;
  if (routeIndexes.length !== expectedIndexCount) {
    throw new Error(`split policy route requires ${expectedIndexCount} constraint indexes, got ${routeIndexes.join(",")}`);
  }
  const withdrawIndexes = routeIndexes.slice(0, withdrawInstructions.length);
  const hubIndexes = routeIndexes.slice(withdrawInstructions.length, withdrawInstructions.length + 1);
  const depositIndexes = routeIndexes.slice(withdrawInstructions.length + 1);

  const legs = [
    {
      step: "policy-route-withdraw",
      instructions: withdrawInstructions,
      constraintIndexes: withdrawIndexes,
    },
    {
      step: "policy-route-hub-swap",
      instructions: [hubSwap],
      constraintIndexes: hubIndexes,
    },
    {
      step: "policy-route-deposit",
      instructions: depositInstructions,
      constraintIndexes: depositIndexes,
    },
  ];

  const executePendingLegs = async (): Promise<void> => {
    let finalRun: TransactionRun | null = null;
    for (const leg of legs) {
      if (stepDone(leg.step)) {
        continue;
      }
      const instruction = createSquadsProgramInteractionExecutionInstruction(clusterConfig, {
        policy,
        signer: system,
        accountIndex: 0,
        instructions: leg.instructions.map((inner) => clearSignerForPubkey(inner, vault)),
        instructionConstraintIndexes: leg.constraintIndexes,
      });
      finalRun = await sendTransaction(leg.step, [instruction], [systemKeypair], routeLookupTables());
      markStep(leg.step, finalRun);
    }
    if (finalRun) {
      markStep("policy-route", finalRun);
    }
  };

  if (stepDone("policy-route-hub-swap")) {
    await executePendingLegs();
  } else {
    await withPolicyHubAuthorizer(vault, executePendingLegs);
  }
}

async function withPolicyHubAuthorizer(vault: PublicKey, action: () => Promise<void>): Promise<void> {
  const hubAuthorizer = policyHubAuthorizer(vault);
  if (hubAuthorizer.equals(clusterConfig.loyalHubAuthorizer)) {
    await action();
    return;
  }
  if (!hasFlag(args, "allow-authority-handoff")) {
    throw new Error("policy Hub authorizer handoff requires --allow-authority-handoff");
  }
  await setHubAuthorizer(hubAuthorizer, "handoff-hub-authorizer");
  try {
    await action();
  } finally {
    if (!hasFlag(args, "skip-authority-restore")) {
      await restoreHubAuthorities();
    }
  }
}

async function withTreasuryInventoryRebalancer(action: () => Promise<void>): Promise<void> {
  const treasury = requireTreasury();
  const treasuryVault = new PublicKey(treasury.vault);
  const before = await fetchHubState();
  if (!state.original?.admin || !state.original?.inventoryRebalancer) {
    rememberOriginalHubAuthorities(before);
    saveState();
  }
  if (!before.inventory_rebalancer || before.inventory_rebalancer !== treasury.vault) {
    await setInventoryRebalancer(treasuryVault, "handoff-rebalancer");
  }

  try {
    await action();
  } finally {
    if (!hasFlag(args, "skip-authority-restore")) {
      await restoreHubAuthorities();
    }
  }
}

async function runTreasuryJupiterRebalance(): Promise<void> {
  if (stepDone("treasury-jupiter-rebalance")) {
    return;
  }
  const treasury = requireTreasury();
  const treasuryVault = new PublicKey(treasury.vault);
  if (!treasury.policy) {
    throw new Error("missing treasury rebalance policy");
  }
  const treasuryPolicy = new PublicKey(treasury.policy);
  const rebalanceIndexes = treasury.rebalanceIndexes ?? [];
  if (rebalanceIndexes.length !== 3) {
    throw new Error(`treasury rebalance policy requires 3 constraint indexes, got ${rebalanceIndexes.join(",")}`);
  }
  let handedOffAdmin = false;
  const laneId = numberValue(args, "treasury-lane-id", 0);
  const hubInputAmount = u64(value(args, "treasury-rebalance-in-raw") ?? "500000", "treasury-rebalance-in-raw");
  const hubOutputTopUpAmount = u64(value(args, "treasury-rebalance-topup-raw") ?? "495000", "treasury-rebalance-topup-raw");
  const inputMintInfo = await fetchMintInfo(USDC_MINT);
  const outputMintInfo = await fetchMintInfo(PYUSD_MINT);
  const hubAuthority = deriveHubAuthority(hubProgram, laneId);
  const hubOutput = associatedTokenAddress(PYUSD_MINT, hubAuthority, outputMintInfo.tokenProgram, true);
  const treasuryInput = associatedTokenAddress(USDC_MINT, treasuryVault, inputMintInfo.tokenProgram, true);
  const treasuryOutput = associatedTokenAddress(PYUSD_MINT, treasuryVault, outputMintInfo.tokenProgram, true);
  const currentHubState = await fetchHubState();
  let hubAdmin = new PublicKey(currentHubState.admin);
  if (hubAdmin.equals(system) && hasFlag(args, "allow-authority-handoff")) {
    await transferHubAdminFromSystemToTreasury(treasuryVault, "handoff-admin");
    hubAdmin = treasuryVault;
    handedOffAdmin = true;
  }
  if (!hubAdmin.equals(system) && !hubAdmin.equals(treasuryVault)) {
    throw new Error(`treasury rebalance requires current Hub admin ${hubAdmin.toBase58()} to be system or treasury vault`);
  }
  const quote = await fetchJupiterQuote({
    inputMint: USDC_MINT,
    outputMint: PYUSD_MINT,
    amount: hubInputAmount,
    slippageBps: numberValue(args, "slippage-bps", 50),
  });
  const quoteMinOut = quoteAmount(quote, "otherAmountThreshold");
  if (quoteMinOut < hubOutputTopUpAmount) {
    throw new Error(`Jupiter guaranteed output ${quoteMinOut} is below Hub top-up ${hubOutputTopUpAmount}`);
  }
  const swap = await fetchJupiterSwapInstructions(quote, treasuryVault);
  assertSupportedJupiterSetupInstructions(
    jupiterInstructions(swap.setupInstructions),
    treasuryVault,
    [treasuryInput, treasuryOutput],
  );
  if (swap.cleanupInstruction) {
    throw new Error("treasury rebalance policy does not allow Jupiter cleanup instructions inside the guarded payload");
  }
  const withdraw = clearSignerForPubkey(
    buildHubWithdrawInstruction({
      admin: hubAdmin,
      destination: treasuryInput,
      mint: USDC_MINT,
      tokenProgram: inputMintInfo.tokenProgram,
      amount: hubInputAmount,
      laneId,
    }),
    treasuryVault,
  );
  const topUp = clearSignerForPubkey(
    createTransferCheckedInstruction(
      treasuryOutput,
      PYUSD_MINT,
      hubOutput,
      treasuryVault,
      hubOutputTopUpAmount,
      outputMintInfo.decimals,
      [],
      outputMintInfo.tokenProgram,
    ),
    treasuryVault,
  );
  const setupAtas = [
    createAssociatedTokenAccountIdempotentInstruction(system, treasuryInput, treasuryVault, USDC_MINT, inputMintInfo.tokenProgram, ASSOCIATED_TOKEN_PROGRAM_ID),
    createAssociatedTokenAccountIdempotentInstruction(system, treasuryOutput, treasuryVault, PYUSD_MINT, outputMintInfo.tokenProgram, ASSOCIATED_TOKEN_PROGRAM_ID),
  ];
  const innerInstructions = [
    withdraw,
    clearSignerForPubkey(jupiterInstruction(swap.swapInstruction), treasuryVault),
    topUp,
  ];
  const wrapper = createSquadsProgramInteractionExecutionInstruction(clusterConfig, {
    policy: treasuryPolicy,
    signer: system,
    accountIndex: 0,
    instructions: innerInstructions,
    instructionConstraintIndexes: rebalanceIndexes,
  });
  const computeBudgetInstructions = jupiterComputeBudgetInstructions(swap.computeBudgetInstructions);
  const outer = [
    ...(computeBudgetInstructions.length > 0
      ? computeBudgetInstructions
      : [ComputeBudgetProgram.setComputeUnitLimit({ units: numberValue(args, "compute-unit-limit", 800000) })]),
    ...setupAtas,
    wrapper,
  ];
  try {
    const run = await sendTransaction("treasury-jupiter-rebalance", outer, [systemKeypair], [
      ...routeLookupTables(),
      ...(swap.addressLookupTableAddresses ?? []),
    ]);
    markStep("treasury-jupiter-rebalance", run);
  } finally {
    if (handedOffAdmin && !hasFlag(args, "skip-authority-restore")) {
      await restoreHubAuthorities();
    }
  }
}

async function runActiveLaneRejectionCheck(): Promise<void> {
  try {
    assertRebalanceAvoidsActiveLanes([0], [{
      fromLaneId: 0,
      toLaneId: 1,
    }]);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    if (!message.includes("active lane")) {
      throw error;
    }
    console.log(`active lane rejection ok: ${message}`);
    return;
  }
  throw new Error("active lane rebalance check unexpectedly allowed lane 0 -> 1");
}

async function runLaneRebalance(): Promise<void> {
  if (stepDone("lane-rebalance")) {
    return;
  }
  const treasury = requireTreasury();
  const treasuryVault = new PublicKey(treasury.vault);
  const mintInfo = await fetchMintInfo(USDC_MINT);
  const transfer = {
    mint: USDC_MINT,
    fromLaneId: 0,
    toLaneId: 1,
    amount: u64(value(args, "lane-rebalance-raw") ?? "250000", "lane-rebalance-raw"),
  };
  const inner = clearSignerForPubkey(
    buildHubRebalanceInstruction({
      inventoryRebalancer: treasuryVault,
      mint: USDC_MINT,
      tokenProgram: mintInfo.tokenProgram,
      transfers: [transfer],
    }),
    treasuryVault,
  );
  const wrapper = createSquadsSyncTransactionInstruction(clusterConfig, {
    settings: new PublicKey(treasury.settings),
    signer: system,
    accountIndex: 0,
    instructions: [inner],
  });
  const run = await sendTransaction("lane-rebalance", [wrapper], [systemKeypair], routeLookupTables());
  markStep("lane-rebalance", run);
}

async function cleanup(label: string): Promise<void> {
  console.log(`cleanup start: ${label}`);
  await restoreHubAuthorities();
  await withdrawHubInventoryExcess();
  await reclaimVaultLiquidTokens("user", state.user);
  await reclaimVaultLiquidTokens("treasury", state.treasury);
  saveState();
  console.log("cleanup finished");
}

async function restoreHubAuthorities(): Promise<void> {
  const originalAdmin = state.original?.admin ? new PublicKey(state.original.admin) : system;
  const originalHubAuthorizer = state.original?.hubAuthorizer ? new PublicKey(state.original.hubAuthorizer) : system;
  const originalRebalancer = state.original?.inventoryRebalancer ? new PublicKey(state.original.inventoryRebalancer) : system;
  const current = await fetchHubState();
  if (state.treasury && current.admin === state.treasury.vault && current.admin !== originalAdmin.toBase58()) {
    await transferHubAdminFromTreasuryToSystem(new PublicKey(state.treasury.vault), originalAdmin, "restore-admin");
  }
  const afterAdmin = await fetchHubState();
  if (afterAdmin.hub_authorizer !== originalHubAuthorizer.toBase58()) {
    await setHubAuthorizer(originalHubAuthorizer, "restore-hub-authorizer");
  }
  const afterAuthorizer = await fetchHubState();
  if (afterAuthorizer.inventory_rebalancer !== originalRebalancer.toBase58()) {
    await setInventoryRebalancer(originalRebalancer, "restore-rebalancer");
  }
}

async function withdrawHubInventoryExcess(): Promise<void> {
  const current = await fetchHubState();
  const instructions: TransactionInstruction[] = [];
  for (const lane of [0, 1]) {
    for (const mint of [USDC_MINT, PYUSD_MINT]) {
      const key = hubBalanceKey(lane, mint);
      const initial = BigInt(state.initialHubBalances?.[key] ?? "0");
      const amount = hubInventoryAmount(current, lane, mint);
      if (amount <= initial) {
        continue;
      }
      const mintInfo = await fetchMintInfo(mint);
      const destination = associatedTokenAddress(mint, system, mintInfo.tokenProgram, false);
      instructions.push(
        createAssociatedTokenAccountIdempotentInstruction(system, destination, system, mint, mintInfo.tokenProgram, ASSOCIATED_TOKEN_PROGRAM_ID),
      );
      instructions.push(buildHubWithdrawInstruction({
        admin: system,
        destination,
        mint,
        tokenProgram: mintInfo.tokenProgram,
        amount: amount - initial,
        laneId: lane,
      }));
      console.log(`queue cleanup hub withdraw lane=${lane} mint=${mint.toBase58()} amount=${amount - initial}`);
    }
  }
  if (instructions.length > 0) {
    await sendTransaction("cleanup-hub-inventory", instructions, [systemKeypair]);
  }
}

async function reclaimVaultLiquidTokens(kind: "user" | "treasury", vaultState: StoredVault | undefined): Promise<void> {
  if (!vaultState) {
    return;
  }
  const vault = new PublicKey(vaultState.vault);
  const settings = new PublicKey(vaultState.settings);
  const inner: TransactionInstruction[] = [];
  for (const mint of [USDC_MINT, PYUSD_MINT]) {
    const mintInfo = await fetchMintInfo(mint);
    const source = associatedTokenAddress(mint, vault, mintInfo.tokenProgram, true);
    const amount = await fetchTokenBalance(source);
    if (amount === 0n) {
      continue;
    }
    const destination = associatedTokenAddress(mint, system, mintInfo.tokenProgram, false);
    inner.push(
      createAssociatedTokenAccountIdempotentInstruction(system, destination, system, mint, mintInfo.tokenProgram, ASSOCIATED_TOKEN_PROGRAM_ID),
    );
    inner.push(clearSignerForPubkey(
      createTransferCheckedInstruction(source, mint, destination, vault, amount, mintInfo.decimals, [], mintInfo.tokenProgram),
      vault,
    ));
    console.log(`queue reclaim ${kind} vault mint=${mint.toBase58()} amount=${amount}`);
  }
  if (inner.length === 0) {
    return;
  }
  const wrapper = createSquadsSyncTransactionInstruction(clusterConfig, {
    settings,
    signer: system,
    accountIndex: 0,
    instructions: inner,
  });
  await sendTransaction(`cleanup-${kind}-vault-liquid-tokens`, [wrapper], [systemKeypair], routeLookupTables());
}

async function setInventoryRebalancer(newRebalancer: PublicKey, label: string): Promise<void> {
  const current = await fetchHubState();
  if (current.inventory_rebalancer === newRebalancer.toBase58()) {
    return;
  }
  if (current.admin !== system.toBase58()) {
    throw new Error(`cannot set inventory rebalancer directly while Hub admin is ${current.admin}`);
  }
  await sendTransaction(label, [buildHubSetInventoryRebalancerInstruction(system, newRebalancer)], [systemKeypair]);
}

async function setHubAuthorizer(newHubAuthorizer: PublicKey, label: string): Promise<void> {
  const current = await fetchHubState();
  if (current.hub_authorizer === newHubAuthorizer.toBase58()) {
    return;
  }
  if (current.admin !== system.toBase58()) {
    throw new Error(`cannot set Hub authorizer directly while Hub admin is ${current.admin}`);
  }
  await sendTransaction(label, [buildHubSetHubAuthorizerInstruction(system, newHubAuthorizer)], [systemKeypair]);
}

async function transferHubAdminFromSystemToTreasury(treasuryVault: PublicKey, label: string): Promise<void> {
  const current = await fetchHubState();
  if (current.admin === treasuryVault.toBase58()) {
    return;
  }
  if (current.admin !== system.toBase58()) {
    throw new Error(`cannot hand Hub admin to treasury while current admin is ${current.admin}`);
  }
  await sendTransaction(`${label}-request`, [
    buildHubRequestAdminTransferInstruction(system, treasuryVault),
  ], [systemKeypair]);
  await acceptHubAdminTransferThroughTreasury(treasuryVault, `${label}-accept`);
}

async function transferHubAdminFromTreasuryToSystem(currentAdmin: PublicKey, newAdmin: PublicKey, label: string): Promise<void> {
  const treasury = requireTreasury();
  const treasuryVault = new PublicKey(treasury.vault);
  if (!currentAdmin.equals(treasuryVault)) {
    throw new Error(`expected treasury vault admin ${treasuryVault.toBase58()}, got ${currentAdmin.toBase58()}`);
  }
  if (!newAdmin.equals(system)) {
    throw new Error(`cannot accept restored Hub admin ${newAdmin.toBase58()} without its keypair`);
  }
  const current = await fetchHubState();
  if (current.admin === newAdmin.toBase58()) {
    return;
  }
  if (current.admin !== treasuryVault.toBase58()) {
    throw new Error(`cannot restore Hub admin while current admin is ${current.admin}`);
  }
  const inner = clearSignerForPubkey(
    buildHubRequestAdminTransferInstruction(treasuryVault, newAdmin),
    treasuryVault,
  );
  const wrapper = createSquadsSyncTransactionInstruction(clusterConfig, {
    settings: new PublicKey(treasury.settings),
    signer: system,
    accountIndex: 0,
    instructions: [inner],
  });
  await sendTransaction(`${label}-request`, [wrapper], [systemKeypair], routeLookupTables());
  await sendTransaction(`${label}-accept`, [
    buildHubAcceptAdminTransferInstruction(newAdmin),
  ], [systemKeypair]);
}

async function acceptHubAdminTransferThroughTreasury(treasuryVault: PublicKey, label: string): Promise<void> {
  const treasury = requireTreasury();
  const inner = clearSignerForPubkey(
    buildHubAcceptAdminTransferInstruction(treasuryVault),
    treasuryVault,
  );
  const wrapper = createSquadsSyncTransactionInstruction(clusterConfig, {
    settings: new PublicKey(treasury.settings),
    signer: system,
    accountIndex: 0,
    instructions: [inner],
  });
  await sendTransaction(label, [wrapper], [systemKeypair], routeLookupTables());
}

async function buildHubSwapInstruction(vault: PublicKey): Promise<TransactionInstruction> {
  const laneId = numberValue(args, "policy-lane-id", 0);
  const amountIn = u64(value(args, "policy-amount-in-raw") ?? "1000000", "policy-amount-in-raw");
  const amountOut = u64(value(args, "policy-amount-out-raw") ?? "995000", "policy-amount-out-raw");
  const minOut = u64(value(args, "policy-min-out-raw") ?? amountOut.toString(), "policy-min-out-raw");
  const usdcInfo = await fetchMintInfo(USDC_MINT);
  const pyusdInfo = await fetchMintInfo(PYUSD_MINT);
  const hubAuthority = deriveHubAuthority(hubProgram, laneId);
  const hubAuthorizer = policyHubAuthorizer(vault);
  return new TransactionInstruction({
    programId: hubProgram,
    keys: [
      { pubkey: deriveConfig(hubProgram), isSigner: false, isWritable: false },
      { pubkey: vault, isSigner: true, isWritable: false },
      { pubkey: associatedTokenAddress(USDC_MINT, vault, usdcInfo.tokenProgram, true), isSigner: false, isWritable: true },
      { pubkey: associatedTokenAddress(PYUSD_MINT, vault, pyusdInfo.tokenProgram, true), isSigner: false, isWritable: true },
      { pubkey: associatedTokenAddress(USDC_MINT, hubAuthority, usdcInfo.tokenProgram, true), isSigner: false, isWritable: true },
      { pubkey: associatedTokenAddress(PYUSD_MINT, hubAuthority, pyusdInfo.tokenProgram, true), isSigner: false, isWritable: true },
      { pubkey: USDC_MINT, isSigner: false, isWritable: false },
      { pubkey: PYUSD_MINT, isSigner: false, isWritable: false },
      { pubkey: hubAuthority, isSigner: false, isWritable: false },
      { pubkey: hubAuthorizer, isSigner: true, isWritable: false },
      { pubkey: TOKEN_PROGRAM_ID, isSigner: false, isWritable: false },
      { pubkey: TOKEN_2022_PROGRAM_ID, isSigner: false, isWritable: false },
    ],
    data: swapExactInData({ amountIn, amountOut, minOut, maxFeeBps: 50, laneId }),
  });
}

function buildHubRequestAdminTransferInstruction(admin: PublicKey, newAdmin: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: hubProgram,
    keys: [
      { pubkey: deriveConfig(hubProgram), isSigner: false, isWritable: true },
      { pubkey: admin, isSigner: true, isWritable: false },
      { pubkey: derivePendingAdmin(hubProgram), isSigner: false, isWritable: true },
      { pubkey: newAdmin, isSigner: false, isWritable: false },
      { pubkey: SystemProgram.programId, isSigner: false, isWritable: false },
    ],
    data: Buffer.from([REQUEST_ADMIN_TRANSFER_TAG]),
  });
}

function buildHubAcceptAdminTransferInstruction(newAdmin: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: hubProgram,
    keys: [
      { pubkey: deriveConfig(hubProgram), isSigner: false, isWritable: true },
      { pubkey: derivePendingAdmin(hubProgram), isSigner: false, isWritable: false },
      { pubkey: newAdmin, isSigner: true, isWritable: false },
    ],
    data: Buffer.from([ACCEPT_ADMIN_TRANSFER_TAG]),
  });
}

function buildHubSetInventoryRebalancerInstruction(admin: PublicKey, newRebalancer: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: hubProgram,
    keys: [
      { pubkey: deriveConfig(hubProgram), isSigner: false, isWritable: true },
      { pubkey: admin, isSigner: true, isWritable: false },
      { pubkey: newRebalancer, isSigner: false, isWritable: false },
    ],
    data: Buffer.from([SET_INVENTORY_REBALANCER_TAG]),
  });
}

function buildHubSetHubAuthorizerInstruction(admin: PublicKey, newHubAuthorizer: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: hubProgram,
    keys: [
      { pubkey: deriveConfig(hubProgram), isSigner: false, isWritable: true },
      { pubkey: admin, isSigner: true, isWritable: false },
      { pubkey: newHubAuthorizer, isSigner: false, isWritable: false },
    ],
    data: Buffer.from([SET_HUB_AUTHORIZER_TAG]),
  });
}

function buildHubWithdrawInstruction(args: {
  admin: PublicKey;
  destination: PublicKey;
  mint: PublicKey;
  tokenProgram: PublicKey;
  amount: bigint;
  laneId: number;
}): TransactionInstruction {
  const data = Buffer.alloc(10);
  data[0] = WITHDRAW_INVENTORY_TAG;
  data.writeBigUInt64LE(args.amount, 1);
  data[9] = args.laneId;
  return new TransactionInstruction({
    programId: hubProgram,
    keys: [
      { pubkey: deriveConfig(hubProgram), isSigner: false, isWritable: true },
      { pubkey: args.admin, isSigner: true, isWritable: false },
      { pubkey: associatedTokenAddress(args.mint, deriveHubAuthority(hubProgram, args.laneId), args.tokenProgram, true), isSigner: false, isWritable: true },
      { pubkey: args.destination, isSigner: false, isWritable: true },
      { pubkey: args.mint, isSigner: false, isWritable: false },
      { pubkey: deriveHubAuthority(hubProgram, args.laneId), isSigner: false, isWritable: false },
      { pubkey: args.tokenProgram, isSigner: false, isWritable: false },
    ],
    data,
  });
}

function buildHubRebalanceInstruction(args: {
  inventoryRebalancer: PublicKey;
  mint: PublicKey;
  tokenProgram: PublicKey;
  transfers: { fromLaneId: number; toLaneId: number; amount: bigint }[];
}): TransactionInstruction {
  if (args.transfers.length === 0 || args.transfers.length > MAX_REBALANCE_TRANSFERS) {
    throw new Error(`rebalance supports 1..=${MAX_REBALANCE_TRANSFERS} transfers`);
  }
  const data = Buffer.alloc(2 + args.transfers.length * 10);
  data[0] = REBALANCE_INVENTORY_TAG;
  data[1] = args.transfers.length;
  for (const [index, transfer] of args.transfers.entries()) {
    const offset = 2 + index * 10;
    data[offset] = transfer.fromLaneId;
    data[offset + 1] = transfer.toLaneId;
    data.writeBigUInt64LE(transfer.amount, offset + 2);
  }
  return new TransactionInstruction({
    programId: hubProgram,
    keys: [
      { pubkey: deriveConfig(hubProgram), isSigner: false, isWritable: false },
      { pubkey: args.inventoryRebalancer, isSigner: true, isWritable: false },
      { pubkey: args.tokenProgram, isSigner: false, isWritable: false },
      { pubkey: args.mint, isSigner: false, isWritable: false },
      ...args.transfers.flatMap((transfer) => {
        const sourceAuthority = deriveHubAuthority(hubProgram, transfer.fromLaneId);
        const destinationAuthority = deriveHubAuthority(hubProgram, transfer.toLaneId);
        return [
          { pubkey: sourceAuthority, isSigner: false, isWritable: false },
          { pubkey: associatedTokenAddress(args.mint, sourceAuthority, args.tokenProgram, true), isSigner: false, isWritable: true },
          { pubkey: associatedTokenAddress(args.mint, destinationAuthority, args.tokenProgram, true), isSigner: false, isWritable: true },
        ];
      }),
    ],
    data,
  });
}

function swapExactInData(args: {
  amountIn: bigint;
  amountOut: bigint;
  minOut: bigint;
  maxFeeBps: number;
  laneId: number;
}): Buffer {
  const data = Buffer.alloc(28);
  data[0] = SWAP_EXACT_IN_TAG;
  data.writeBigUInt64LE(args.amountIn, 1);
  data.writeBigUInt64LE(args.amountOut, 9);
  data.writeBigUInt64LE(args.minOut, 17);
  data.writeUInt16LE(args.maxFeeBps, 25);
  data[27] = args.laneId;
  return data;
}

async function sendTransaction(
  label: string,
  instructions: TransactionInstruction[],
  signers: Keypair[],
  lookupTableAddresses: readonly string[] = [],
): Promise<TransactionRun> {
  const latest = await connection.getLatestBlockhash(DEFAULT_COMMITMENT);
  const lookupTables = await fetchLookupTables(lookupTableAddresses);
  const transactionInstructions = withComputeBudget(label, instructions);
  const message = new TransactionMessage({
    payerKey: system,
    recentBlockhash: latest.blockhash,
    instructions: transactionInstructions,
  }).compileToV0Message(lookupTables);
  const tx = new VersionedTransaction(message);
  tx.sign(uniqueSigners([systemKeypair, ...signers]));
  const simulation = await connection.simulateTransaction(tx, {
    commitment: DEFAULT_COMMITMENT,
    sigVerify: true,
  });
  if (simulation.value.err) {
    console.error(JSON.stringify({ label, simulation: simulation.value }, bigintJson, 2));
    throw new Error(`${label} simulation failed`);
  }
  console.log(`${label} simulation ok units=${simulation.value.unitsConsumed ?? "unknown"}`);
  if (!executeLive) {
    if (!hasFlag(args, "simulate-all")) {
      console.log(
        `${label}: simulate-only stopped after first pending transaction. Re-run with --execute to submit it, or --simulate-all for a best-effort full dry run.`,
      );
      process.exit(0);
    }
    return {
      mode: "simulate",
      signature: null,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
    };
  }
  const signature = await connection.sendRawTransaction(tx.serialize(), {
    maxRetries: 3,
    skipPreflight: false,
  });
  const confirmation = await connection.confirmTransaction({
    signature,
    blockhash: latest.blockhash,
    lastValidBlockHeight: latest.lastValidBlockHeight,
  }, DEFAULT_COMMITMENT);
  if (confirmation.value.err) {
    throw new Error(`${label} transaction ${signature} failed: ${JSON.stringify(confirmation.value.err)}`);
  }
  console.log(`${label} signature=${signature}`);
  return {
    mode: "execute",
    signature,
    unitsConsumed: simulation.value.unitsConsumed ?? null,
  };
}

function withComputeBudget(label: string, instructions: TransactionInstruction[]): TransactionInstruction[] {
  if (!needsComputeBudget(label) || instructions.some((instruction) => instruction.programId.equals(ComputeBudgetProgram.programId))) {
    return instructions;
  }
  return [
    ComputeBudgetProgram.setComputeUnitLimit({ units: numberValue(args, "compute-unit-limit", 800000) }),
    ...instructions,
  ];
}

function needsComputeBudget(label: string): boolean {
  return label === "policy-setup"
    || label.startsWith("policy-route")
    || label === "treasury-jupiter-rebalance"
    || label === "lane-rebalance";
}

async function fetchHubState(): Promise<HubState> {
  const result = spawnSync("bun", [
    "run",
    "hub:cli",
    "--",
    "-u",
    resolveRpcUrl(cluster),
    "--program-id",
    hubProgram.toBase58(),
    "--json",
    "state",
  ], {
    cwd: process.cwd(),
    encoding: "utf8",
    env: process.env,
  });
  if (result.status !== 0) {
    throw new Error(`hub state failed:\n${result.stdout}\n${result.stderr}`);
  }
  return parseJsonFromOutput(result.stdout) as HubState;
}

async function fetchMintInfo(mint: PublicKey): Promise<MintInfo> {
  const account = await connection.getAccountInfo(mint, DEFAULT_COMMITMENT);
  if (!account) {
    throw new Error(`mint account does not exist: ${mint.toBase58()}`);
  }
  const tokenProgram = account.owner.equals(TOKEN_PROGRAM_ID)
    ? TOKEN_PROGRAM_ID
    : account.owner.equals(TOKEN_2022_PROGRAM_ID)
      ? TOKEN_2022_PROGRAM_ID
      : null;
  if (!tokenProgram) {
    throw new Error(`unsupported mint owner ${account.owner.toBase58()}`);
  }
  const mintInfo = await getMint(connection, mint, DEFAULT_COMMITMENT, tokenProgram);
  return { tokenProgram, decimals: mintInfo.decimals };
}

async function fetchTokenBalance(account: PublicKey): Promise<bigint> {
  const info = await connection.getAccountInfo(account, DEFAULT_COMMITMENT);
  if (!info) {
    return 0n;
  }
  const balance = await connection.getTokenAccountBalance(account, DEFAULT_COMMITMENT);
  return BigInt(balance.value.amount);
}

async function accountExists(account: PublicKey): Promise<boolean> {
  return (await connection.getAccountInfo(account, DEFAULT_COMMITMENT)) !== null;
}

async function fetchJupiterQuote(quoteArgs: {
  inputMint: PublicKey;
  outputMint: PublicKey;
  amount: bigint;
  slippageBps: number;
}): Promise<Record<string, unknown>> {
  const url = new URL(value(args, "quote-api") ?? DEFAULT_QUOTE_API);
  url.searchParams.set("inputMint", quoteArgs.inputMint.toBase58());
  url.searchParams.set("outputMint", quoteArgs.outputMint.toBase58());
  url.searchParams.set("amount", quoteArgs.amount.toString());
  url.searchParams.set("slippageBps", quoteArgs.slippageBps.toString());
  url.searchParams.set("restrictIntermediateTokens", "true");
  return fetchJson(url.toString(), { method: "GET", headers: jupiterApiHeaders() });
}

async function fetchJupiterSwapInstructions(
  quote: Record<string, unknown>,
  userPublicKey: PublicKey,
): Promise<JupiterSwapInstructions> {
  const response = await fetchJson(value(args, "swap-instructions-api") ?? DEFAULT_SWAP_INSTRUCTIONS_API, {
    method: "POST",
    headers: { "content-type": "application/json", ...jupiterApiHeaders() },
    body: JSON.stringify({
      quoteResponse: quote,
      userPublicKey: userPublicKey.toBase58(),
      dynamicComputeUnitLimit: true,
      prioritizationFeeLamports: {
        priorityLevelWithMaxLamports: {
          maxLamports: Number(value(args, "priority-max-lamports") ?? "1000000"),
          priorityLevel: value(args, "priority-level") ?? "veryHigh",
        },
      },
    }),
  });
  if (!response.swapInstruction) {
    throw new Error(`Jupiter swap-instructions response missing swapInstruction: ${JSON.stringify(response)}`);
  }
  return response as JupiterSwapInstructions;
}

async function fetchJson(url: string, init: RequestInit): Promise<Record<string, unknown>> {
  const response = await fetch(url, init);
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`HTTP ${response.status} from ${url}: ${text}`);
  }
  return JSON.parse(text) as Record<string, unknown>;
}

function jupiterApiHeaders(): Record<string, string> {
  return process.env.JUPITER_API_KEY ? { "x-api-key": process.env.JUPITER_API_KEY } : {};
}

function jupiterInstructions(values: readonly JupiterInstructionJson[] = []): TransactionInstruction[] {
  return values.map(jupiterInstruction);
}

function jupiterComputeBudgetInstructions(values: readonly JupiterInstructionJson[] = []): TransactionInstruction[] {
  return jupiterInstructions(values).filter((instruction) => instruction.programId.equals(ComputeBudgetProgram.programId));
}

function assertSupportedJupiterSetupInstructions(
  instructions: readonly TransactionInstruction[],
  treasuryVault: PublicKey,
  allowedAtas: readonly PublicKey[],
): void {
  const allowed = new Set(allowedAtas.map((pubkey) => pubkey.toBase58()));
  for (const instruction of instructions) {
    const ata = instruction.keys[1]?.pubkey;
    const owner = instruction.keys[2]?.pubkey;
    const isIdempotentAtaCreate = instruction.programId.equals(ASSOCIATED_TOKEN_PROGRAM_ID)
      && instruction.data.length === 1
      && instruction.data[0] === 1
      && ata !== undefined
      && owner !== undefined
      && owner.equals(treasuryVault)
      && allowed.has(ata.toBase58());
    if (!isIdempotentAtaCreate) {
      throw new Error(
        `treasury rebalance policy only supports Jupiter ATA setup outside the guarded payload, got ${instruction.programId.toBase58()}`,
      );
    }
  }
}

function jupiterInstruction(value: JupiterInstructionJson): TransactionInstruction {
  return new TransactionInstruction({
    programId: new PublicKey(value.programId),
    keys: value.accounts.map((account) => ({
      pubkey: new PublicKey(account.pubkey),
      isSigner: account.isSigner,
      isWritable: account.isWritable,
    })),
    data: Buffer.from(value.data, "base64"),
  });
}

async function fetchLookupTables(addresses: readonly string[]): Promise<AddressLookupTableAccount[]> {
  const unique = [...new Set(addresses.filter(Boolean))];
  const tables = await Promise.all(unique.map(async (address) => {
    const result = await connection.getAddressLookupTable(new PublicKey(address), { commitment: DEFAULT_COMMITMENT });
    if (!result.value) {
      throw new Error(`lookup table not found: ${address}`);
    }
    return result.value;
  }));
  return tables;
}

function loadWireInstructions(path: string): TransactionInstruction[] {
  const parsed = JSON.parse(readFileSync(path, "utf8")) as WireInstruction | WireInstruction[];
  return (Array.isArray(parsed) ? parsed : [parsed]).map((value) => new TransactionInstruction({
    programId: new PublicKey(value.programId),
    keys: value.accounts.map((account) => ({
      pubkey: new PublicKey(account.pubkey),
      isSigner: Boolean(account.isSigner),
      isWritable: Boolean(account.isWritable),
    })),
    data: decodeInstructionData(value.data, value.encoding),
  }));
}

function decodeInstructionData(data: string | number[], encoding: WireInstruction["encoding"]): Buffer {
  if (Array.isArray(data)) {
    return Buffer.from(data);
  }
  if ((encoding ?? "base64") === "hex") {
    return Buffer.from(data, "hex");
  }
  return Buffer.from(data, "base64");
}

function clearSignerForPubkey(instruction: TransactionInstruction, target: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: instruction.programId,
    keys: instruction.keys.map((account) => ({
      ...account,
      isSigner: account.pubkey.equals(target) ? false : account.isSigner,
    })),
    data: instruction.data,
  });
}

function deriveConfig(programId: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([CONFIG_SEED], programId)[0];
}

function derivePendingAdmin(programId: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([PENDING_ADMIN_SEED], programId)[0];
}

function deriveHubAuthority(programId: PublicKey, laneId: number): PublicKey {
  return PublicKey.findProgramAddressSync([HUB_AUTHORITY_SEED, Uint8Array.of(laneId)], programId)[0];
}

function associatedTokenAddress(mint: PublicKey, owner: PublicKey, tokenProgram: PublicKey, allowOwnerOffCurve: boolean): PublicKey {
  return getAssociatedTokenAddressSync(mint, owner, allowOwnerOffCurve, tokenProgram, ASSOCIATED_TOKEN_PROGRAM_ID);
}

function hubInventoryAmount(state: HubState, laneId: number, mint: PublicKey): bigint {
  const lane = state.lanes.find((candidate) => candidate.lane_id === laneId);
  const inventory = lane?.inventory.find((item) => item.mint === mint.toBase58());
  if (!inventory?.amount) {
    return 0n;
  }
  return BigInt(inventory.amount);
}

function hubBalanceKey(lane: number, mint: PublicKey): string {
  return `${lane}:${mint.toBase58()}`;
}

function rememberOriginalHubAuthorities(hubState: HubState): void {
  state.original ??= {};
  state.original.admin ??= hubState.admin;
  state.original.hubAuthorizer ??= hubState.hub_authorizer;
  state.original.inventoryRebalancer ??= hubState.inventory_rebalancer;
}

function rememberInitialHubBalances(hubState: HubState): void {
  state.initialHubBalances ??= {};
  for (const lane of [0, 1]) {
    for (const mint of [USDC_MINT, PYUSD_MINT]) {
      const key = hubBalanceKey(lane, mint);
      state.initialHubBalances[key] ??= hubInventoryAmount(hubState, lane, mint).toString();
    }
  }
}

function markStep(step: string, run: TransactionRun): void {
  if (run.mode !== "execute") {
    return;
  }
  state.steps ??= {};
  state.steps[step] = {
    signature: run.signature,
    at: new Date().toISOString(),
  };
  saveState();
}

function stepDone(step: string): boolean {
  return !forceRerun && Boolean(state.steps?.[step]?.signature);
}

function requireUser(): NonNullable<TestState["user"]> {
  if (!state.user?.settings || !state.user.vault) {
    throw new Error("missing user vault state");
  }
  return state.user;
}

function requireTreasury(): StoredVault {
  if (!state.treasury?.settings || !state.treasury.vault) {
    throw new Error("missing treasury vault state");
  }
  return state.treasury;
}

function routeLookupTables(): string[] {
  return values(args, "lookup-table");
}

function usesDefaultRouteFiles(): boolean {
  return value(args, "route-withdraw-file") === DEFAULT_ROUTE_WITHDRAW_FILE
    && value(args, "route-deposit-file") === DEFAULT_ROUTE_DEPOSIT_FILE
    && value(args, "policy-setup-file") === DEFAULT_POLICY_SETUP_FILE;
}

function requiredMintInfo(infos: Map<string, MintInfo>, mint: PublicKey): MintInfo {
  const info = infos.get(mint.toBase58());
  if (!info) {
    throw new Error(`missing mint info for ${mint.toBase58()}`);
  }
  return info;
}

function parseArgs(values: string[]): ParsedArgs {
  const parsed: ParsedArgs = {};
  for (let index = 0; index < values.length; index += 1) {
    const key = values[index];
    if (!key.startsWith("--")) {
      throw new Error(`unexpected argument: ${key}`);
    }
    const name = key.slice(2);
    const next = values[index + 1];
    if (next === undefined || next.startsWith("--")) {
      parsed[name] = [...(parsed[name] ?? []), "1"];
    } else {
      parsed[name] = [...(parsed[name] ?? []), next];
      index += 1;
    }
  }
  return parsed;
}

function value(args: ParsedArgs, name: string): string | undefined {
  return args[name]?.[args[name].length - 1];
}

function values(args: ParsedArgs, name: string): string[] {
  return args[name] ?? [];
}

function defaultArg(args: ParsedArgs, name: string, defaultValue: string): void {
  if (!args[name] || args[name].length === 0) {
    args[name] = [defaultValue];
  }
}

function hasFlag(args: ParsedArgs, name: string): boolean {
  const item = value(args, name);
  return item === "1" || item === "true";
}

function required(args: ParsedArgs, name: string): string {
  const item = value(args, name);
  if (!item || item === "1") {
    throw new Error(`missing --${name}`);
  }
  return item;
}

function resolveSystemKeypairPath(): string {
  return value(args, "keypair")
    ?? process.env.SOLANA_KEYPAIR
    ?? `${homedir()}/.config/solana/id.json`;
}

function pubkey(item: string, name: string): PublicKey {
  try {
    return new PublicKey(item);
  } catch {
    throw new Error(`invalid ${name}: ${item}`);
  }
}

function u64(item: string, name: string): bigint {
  if (!/^\d+$/.test(item)) {
    throw new Error(`${name} must be an unsigned integer`);
  }
  return BigInt(item);
}

function numberValue(args: ParsedArgs, name: string, defaultValue: number): number {
  const item = value(args, name);
  if (item === undefined) {
    return defaultValue;
  }
  const parsed = Number(item);
  if (!Number.isInteger(parsed) || parsed < 0) {
    throw new Error(`${name} must be a non-negative integer`);
  }
  return parsed;
}

function parseU8List(item: string, name: string): number[] {
  return item.split(",").filter(Boolean).map((part) => {
    const parsed = Number(part);
    if (!Number.isInteger(parsed) || parsed < 0 || parsed > 255) {
      throw new Error(`${name} includes invalid u8: ${part}`);
    }
    return parsed;
  });
}

function parseLoyalCluster(item: string): LoyalCluster {
  switch (item) {
    case "m":
    case "mainnet":
    case "mainnet-beta":
      return LoyalCluster.MainnetBeta;
    case "d":
    case "devnet":
      return LoyalCluster.Devnet;
    default:
      throw new Error(`unsupported Loyal cluster: ${item}`);
  }
}

function resolveRpcUrl(item: string): string {
  if ((item === "m" || item === "mainnet" || item === "mainnet-beta") && process.env.SOLANA_RPC_URL) {
    return process.env.SOLANA_RPC_URL;
  }
  switch (item) {
    case "m":
    case "mainnet":
    case "mainnet-beta":
      return "https://api.mainnet-beta.solana.com";
    case "d":
    case "devnet":
      return "https://api.devnet.solana.com";
    default:
      return item;
  }
}

function loadKeypair(path: string): Keypair {
  const parsed = JSON.parse(readFileSync(path, "utf8")) as unknown;
  if (!Array.isArray(parsed) || !parsed.every((item) => Number.isInteger(item))) {
    throw new Error(`keypair file must contain a JSON byte array: ${path}`);
  }
  return Keypair.fromSecretKey(Uint8Array.from(parsed as number[]));
}

function loadState(path: string, cluster: string, hubProgram: PublicKey, system: PublicKey): TestState {
  try {
    const parsed = JSON.parse(readFileSync(path, "utf8")) as TestState;
    if (parsed.cluster !== cluster || parsed.hubProgram !== hubProgram.toBase58() || parsed.system !== system.toBase58()) {
      throw new Error(`state file ${path} belongs to another cluster/program/system key`);
    }
    return parsed;
  } catch (error) {
    if (error && typeof error === "object" && "code" in error && error.code === "ENOENT") {
      return {
        version: 1,
        cluster,
        hubProgram: hubProgram.toBase58(),
        system: system.toBase58(),
      };
    }
    throw error;
  }
}

function saveState(): void {
  state.updatedAt = new Date().toISOString();
  mkdirSync(dirname(stateFile), { recursive: true });
  writeFileSync(stateFile, `${JSON.stringify(state, null, 2)}\n`);
}

function applyManualResumeFlags(state: TestState, args: ParsedArgs): void {
  const userSettings = value(args, "user-settings");
  const userVault = value(args, "user-vault");
  const treasurySettings = value(args, "treasury-settings");
  const treasuryVault = value(args, "treasury-vault");
  const treasuryPolicy = value(args, "treasury-policy");
  const policy = value(args, "policy");
  if (userSettings || userVault || policy) {
    state.user = {
      ...(state.user ?? {}),
      settings: userSettings ?? state.user?.settings ?? "",
      vault: userVault ?? state.user?.vault ?? "",
      policy: policy ?? state.user?.policy,
    };
  }
  if (treasurySettings || treasuryVault || treasuryPolicy) {
    state.treasury = {
      ...(state.treasury ?? {}),
      settings: treasurySettings ?? state.treasury?.settings ?? "",
      vault: treasuryVault ?? state.treasury?.vault ?? "",
      policy: treasuryPolicy ?? state.treasury?.policy,
    };
  }
}

function uniqueSigners(signers: Keypair[]): Keypair[] {
  const seen = new Set<string>();
  const unique: Keypair[] = [];
  for (const signer of signers) {
    const key = signer.publicKey.toBase58();
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    unique.push(signer);
  }
  return unique;
}

function quoteAmount(quote: Record<string, unknown>, name: string): bigint {
  const item = quote[name];
  if (typeof item !== "string" || !/^\d+$/.test(item)) {
    throw new Error(`Jupiter quote missing ${name}`);
  }
  return BigInt(item);
}

function parseJsonFromOutput(output: string): unknown {
  const start = output.indexOf("{");
  const end = output.lastIndexOf("}");
  if (start < 0 || end < start) {
    throw new Error(`no JSON object found in output:\n${output}`);
  }
  return JSON.parse(output.slice(start, end + 1));
}

function toBigInt(value: unknown, field: string): bigint {
  if (typeof value === "bigint") {
    return value;
  }
  if (typeof value === "number") {
    return BigInt(value);
  }
  if (typeof value === "string" && /^\d+$/.test(value)) {
    return BigInt(value);
  }
  if (value && typeof value === "object" && "toString" in value) {
    return BigInt(value.toString());
  }
  throw new Error(`invalid bigint for ${field}`);
}

function bigintJson(_key: string, value: unknown): unknown {
  return typeof value === "bigint" ? value.toString() : value;
}

function showHelp(): void {
  console.log(`Run the tight-budget mainnet Loyal Hub smoke tests.

Default mode simulates transactions only. Live execution requires both --execute
and CONFIRM_MAINNET=1.

Keypair:
  --keypair <path>                 System Solana keypair. Defaults to SOLANA_KEYPAIR or ~/.config/solana/id.json.
                                   Used as fee payer, fund source, Hub authorizer, and Squads verifier.

Policy route inputs:
  --route-withdraw-file <json>     Wire instruction JSON for Kamino USDC withdraw. Default: ${DEFAULT_ROUTE_WITHDRAW_FILE}
  --route-deposit-file <json>      Wire instruction JSON for Kamino PYUSD deposit. Default: ${DEFAULT_ROUTE_DEPOSIT_FILE}
  --policy-setup-file <json>       Wire instruction JSON run once through user Squads before route. Default: ${DEFAULT_POLICY_SETUP_FILE}
  --refresh-route-files            Regenerate the route/setup JSON files even when they already exist.
  --policy-seed <n>                Squads policy seed for the user route policy. Default: ${DEFAULT_YIELD_ROUTE_POLICY_SEED}
  --update-policy                  Update an existing policy at --policy-seed instead of reusing it as-is.
  --treasury-policy <pubkey>       Resume with an existing treasury rebalance policy account.
  --treasury-policy-seed <n>       Squads policy seed for the treasury rebalance policy. Default: ${DEFAULT_TREASURY_REBALANCE_POLICY_SEED}
  --update-treasury-policy         Force-refresh an existing treasury rebalance policy.
  --kamino-source-reserve <pubkey> Override the default Main USDC source reserve.
  --kamino-target-reserve <pubkey> Override the default Main PYUSD target reserve.

Common:
  --execute                        Submit after each successful simulation.
  --simulate-only                  Force no-submit mode; stops after the first pending transaction.
  --simulate-all                   No-submit mode that continues after simulations; only useful once setup accounts exist.
  --allow-authority-handoff        Allow live temporary Hub admin handoff to treasury Squads vault.
  --cleanup-only                   Only restore authorities and reclaim liquid funds.
  --force-rerun                    Rerun steps even if state file has signatures.
  --state-file <path>              Default: ${DEFAULT_STATE_FILE}
  --inventory-per-lane-raw <n>     Default: 1250000 for each of USDC/PYUSD on lanes 0 and 1.
  --policy-amount-in-raw <n>       Default: 1000000.
  --policy-amount-out-raw <n>      Default: 995000.
  --treasury-rebalance-in-raw <n>  Default: 500000.
  --treasury-rebalance-topup-raw <n> Default: 495000.
  --lane-rebalance-raw <n>         Default: 250000.
  --lookup-table <address>         Repeat for policy/Kamino route lookup tables.

Skips:
  --skip-policy
  --skip-route-file-generation
  --skip-treasury-rebalance
  --skip-lane-rebalance
  --skip-cleanup

Example:
  CONFIRM_MAINNET=1 bun run hub:mainnet-test -- \\
    --keypair "$SYSTEM_KEYPAIR" \\
    --route-withdraw-file ./tmp/withdraw-usdc-kamino.json \\
    --route-deposit-file ./tmp/deposit-pyusd-kamino.json \\
    --allow-authority-handoff \\
    --execute
`);
}
