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
import { readFileSync } from "node:fs";

import {
  LOYAL_CLUSTER_CONFIGS,
  LoyalCluster,
  MaxFeeBps,
  RiskBasket,
  SwapLane,
  assertRebalanceAvoidsActiveLanes,
  createLoyalActionsSdk,
  createSquadsProgramInteractionExecutionInstruction,
  createSquadsSyncTransactionInstruction,
  deriveSquadsVault,
} from "../packages/loyal-actions/src/index.js";

const DEFAULT_COMMITMENT = "confirmed";
const DEFAULT_QUOTE_API = "https://api.jup.ag/swap/v1/quote";
const DEFAULT_SWAP_INSTRUCTIONS_API = "https://api.jup.ag/swap/v1/swap-instructions";
const CONFIG_SEED = Buffer.from("config");
const HUB_AUTHORITY_SEED = Buffer.from("hub-authority");
const WITHDRAW_INVENTORY_TAG = 2;
const REBALANCE_INVENTORY_TAG = 5;
const MAX_REBALANCE_TRANSFERS = 16;

type ParsedCli = {
  command: string;
  flags: Map<string, string[]>;
};

type TransactionRun = {
  mode: "simulate" | "execute";
  signature: string | null;
  unitsConsumed: number | null;
  logs: string[];
};

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

type RebalanceTransfer = {
  mint: PublicKey;
  fromLaneId: number;
  toLaneId: number;
  amount: bigint;
};

type BalanceSnapshot = Record<string, bigint>;

export function parseArgs(argv: string[]): ParsedCli {
  const [command, ...rest] = argv;
  if (!command || command.startsWith("--")) {
    throw new Error("usage: bun scripts/loyal-hub-squads-ops.ts <command> [--flag value]");
  }

  const flags = new Map<string, string[]>();
  let activeFlag: string | null = null;
  for (let index = 0; index < rest.length; index += 1) {
    const token = rest[index];
    if (!token.startsWith("--")) {
      if (!activeFlag) {
        throw new Error(`unexpected argument: ${token}`);
      }
      const activeValues = flags.get(activeFlag) ?? [];
      activeValues.push(token);
      flags.set(activeFlag, activeValues);
      continue;
    }
    const name = token.slice(2);
    const next = rest[index + 1];
    const value = next === undefined || next.startsWith("--") ? "1" : next;
    if (value !== "1") {
      index += 1;
    }
    const values = flags.get(name) ?? [];
    values.push(value);
    flags.set(name, values);
    activeFlag = name === "transfer" ? name : null;
  }

  return { command, flags };
}

export function parseRebalanceTransfers(values: readonly string[], defaultMint?: PublicKey): RebalanceTransfer[] {
  const groups = splitTransferGroups(values);
  return groups.map((group) => parseRebalanceTransfer(group, defaultMint));
}

export function instructionFromWire(value: WireInstruction): TransactionInstruction {
  return new TransactionInstruction({
    programId: new PublicKey(value.programId),
    keys: value.accounts.map((account) => ({
      pubkey: new PublicKey(account.pubkey),
      isSigner: Boolean(account.isSigner),
      isWritable: Boolean(account.isWritable),
    })),
    data: Buffer.from(decodeInstructionData(value.data, value.encoding)),
  });
}

async function main(): Promise<void> {
  const parsed = parseArgs(process.argv.slice(2));
  switch (parsed.command) {
    case "check-active-lane-rebalance":
      await checkActiveLaneRebalance(parsed.flags);
      return;
    case "create-vault":
      await createVault(parsed.flags);
      return;
    case "create-all-in-one-policy":
      await createAllInOnePolicy(parsed.flags);
      return;
    case "execute-policy-route":
      await executePolicyRoute(parsed.flags);
      return;
    case "squads-rebalance-inventory":
      await squadsRebalanceInventory(parsed.flags);
      return;
    case "treasury-jupiter-rebalance":
      await treasuryJupiterRebalance(parsed.flags);
      return;
    default:
      throw new Error(`unknown command: ${parsed.command}`);
  }
}

async function createVault(flags: Map<string, string[]>): Promise<void> {
  const { cluster, config, connection } = connectionContext(flags);
  const payer = loadKeypair(requiredFlag(flags, "keypair"));
  const settingsSeed = await resolveSquadsSettingsSeed({
    connection,
    programId: config.squadsSmartAccountProgramId,
    flags,
  });
  const vaultIndex = parseU8(flag(flags, "vault-index") ?? "0", "vault-index");
  const verifier = parsePubkey(flag(flags, "verifier") ?? payer.publicKey.toBase58(), "verifier");
  const treasury = parsePubkey(requiredFlag(flags, "squads-treasury"), "squads-treasury");
  const settings = getSettingsPda({
    accountIndex: settingsSeed,
    programId: config.squadsSmartAccountProgramId,
  })[0];
  const vault = getSmartAccountPda({
    settingsPda: settings,
    accountIndex: vaultIndex,
    programId: config.squadsSmartAccountProgramId,
  })[0];
  const instructions = [
    createSmartAccount({
      treasury,
      creator: payer.publicKey,
      settings,
      settingsAuthority: null,
      threshold: 1,
      signers: [
        {
          key: verifier,
          permissions: { mask: 7 },
        },
      ],
      timeLock: 0,
      rentCollector: null,
      programId: config.squadsSmartAccountProgramId,
    }),
  ];

  const fundVaultLamports = flag(flags, "fund-vault-lamports");
  if (fundVaultLamports) {
    instructions.push(
      SystemProgram.transfer({
        fromPubkey: payer.publicKey,
        toPubkey: vault,
        lamports: Number(parseU64(fundVaultLamports, "fund-vault-lamports")),
      }),
    );
  }

  const run = await executeOrSimulate({
    cluster,
    connection,
    feePayer: payer,
    signers: loadAdditionalSigners(flags, payer),
    instructions,
    lookupTableAddresses: values(flags, "lookup-table"),
    simulateOnly: boolFlag(flags, "simulate"),
  });
  output(flags, {
    command: "create-vault",
    settingsSeed: settingsSeed.toString(),
    settings,
    vault,
    vaultIndex,
    signer: verifier,
    transaction: run,
  });
}

async function resolveSquadsSettingsSeed(args: {
  connection: Connection;
  programId: PublicKey;
  flags: Map<string, string[]>;
}): Promise<bigint> {
  const programConfig = getProgramConfigPda({ programId: args.programId })[0];
  const config = await ProgramConfig.fromAccountAddress(args.connection, programConfig, DEFAULT_COMMITMENT);
  const canonicalSeed = toBigInt(config.smartAccountIndex, "smartAccountIndex") + 1n;
  const explicitSeed = flag(args.flags, "seed") ? parseU128(requiredFlag(args.flags, "seed"), "seed") : undefined;

  if (explicitSeed !== undefined && explicitSeed !== canonicalSeed && !boolFlag(args.flags, "allow-noncanonical-seed")) {
    throw new Error(
      `--seed ${explicitSeed} does not match next Squads settings seed ${canonicalSeed}; ` +
        "omit --seed or pass --allow-noncanonical-seed only for non-mainnet debugging",
    );
  }

  const expectedSeed = flag(args.flags, "expected-account-index")
    ? parseU128(requiredFlag(args.flags, "expected-account-index"), "expected-account-index")
    : undefined;
  const selectedSeed = explicitSeed ?? canonicalSeed;
  if (expectedSeed !== undefined && selectedSeed !== expectedSeed) {
    throw new Error(`expected Squads settings seed ${expectedSeed}, got ${selectedSeed}`);
  }

  return selectedSeed;
}

async function createAllInOnePolicy(flags: Map<string, string[]>): Promise<void> {
  const { cluster, config, connection, loyalCluster } = connectionContext(flags);
  const payer = loadKeypair(requiredFlag(flags, "keypair"));
  const settings = parsePubkey(requiredFlag(flags, "settings"), "settings");
  const vaultIndex = parseU8(flag(flags, "vault-index") ?? "0", "vault-index");
  const vault = flag(flags, "vault")
    ? parsePubkey(requiredFlag(flags, "vault"), "vault")
    : deriveSquadsVault(config, settings, vaultIndex).address;
  const authority = parsePubkey(flag(flags, "authority") ?? payer.publicKey.toBase58(), "authority");
  const delegatedSigner = parsePubkey(flag(flags, "delegated-signer") ?? authority.toBase58(), "delegated-signer");
  const policySeed = flag(flags, "policy-seed") ? parseU64(requiredFlag(flags, "policy-seed"), "policy-seed") : undefined;
  const sdk = createLoyalActionsSdk({ cluster: loyalCluster });
  const policy = sdk.initYieldRoutePolicy({
    risk: parseRisk(requiredFlag(flags, "risk")),
    swapLanes: parseSwapLanes(requiredFlag(flags, "swap-lanes")),
    maxFeeBps: parseMaxFee(flag(flags, "max-fee-bps")),
    squads: {
      settings,
      authority,
      delegatedSigner,
      accountIndex: vaultIndex,
      vault,
      policySeed,
    },
  });

  const run = await executeOrSimulate({
    cluster,
    connection,
    feePayer: payer,
    signers: loadAdditionalSigners(flags, payer),
    instructions: policy.instructions,
    lookupTableAddresses: values(flags, "lookup-table"),
    simulateOnly: boolFlag(flags, "simulate"),
  });
  output(flags, {
    command: "create-all-in-one-policy",
    settings,
    vault,
    vaultIndex,
    actionAccount: policy.actionAccount,
    routes: serializeRoutes(policy.routes),
    spec: policy.spec,
    transaction: run,
  });
}

async function executePolicyRoute(flags: Map<string, string[]>): Promise<void> {
  const { cluster, config, connection } = connectionContext(flags);
  const payer = loadKeypair(requiredFlag(flags, "keypair"));
  const policy = parsePubkey(requiredFlag(flags, "policy"), "policy");
  const signer = parsePubkey(flag(flags, "delegated-signer") ?? payer.publicKey.toBase58(), "delegated-signer");
  const accountIndex = parseU8(flag(flags, "vault-index") ?? "0", "vault-index");
  const settings = flag(flags, "settings") ? parsePubkey(requiredFlag(flags, "settings"), "settings") : undefined;
  const vault = flag(flags, "vault")
    ? parsePubkey(requiredFlag(flags, "vault"), "vault")
    : settings
      ? deriveSquadsVault(config, settings, accountIndex).address
      : undefined;
  if (!vault) {
    throw new Error("execute-policy-route requires --vault or --settings to clear the Squads vault signer");
  }
  const routeInstructions = loadInstructionFiles(values(flags, "instruction-file")).map((routeInstruction) =>
    clearSignerForPubkey(routeInstruction, vault),
  );
  const instructionConstraintIndexes = parseU8List(requiredFlag(flags, "constraint-indexes"), "constraint-indexes");
  const instruction = createSquadsProgramInteractionExecutionInstruction(config, {
    policy,
    signer,
    accountIndex,
    instructions: routeInstructions,
    instructionConstraintIndexes,
  });

  const run = await executeOrSimulate({
    cluster,
    connection,
    feePayer: payer,
    signers: loadAdditionalSigners(flags, payer),
    instructions: [instruction],
    lookupTableAddresses: values(flags, "lookup-table"),
    simulateOnly: boolFlag(flags, "simulate"),
  });
  output(flags, {
    command: "execute-policy-route",
    policy,
    settings,
    vault,
    signer,
    instructionConstraintIndexes,
    transaction: run,
  });
}

async function squadsRebalanceInventory(flags: Map<string, string[]>): Promise<void> {
  const { cluster, config, connection } = connectionContext(flags);
  const payer = loadKeypair(requiredFlag(flags, "keypair"));
  const programId = parsePubkey(flag(flags, "program-id") ?? config.loyalHubSwapProgramId.toBase58(), "program-id");
  const settings = parsePubkey(requiredFlag(flags, "settings"), "settings");
  const accountIndex = parseU8(flag(flags, "vault-index") ?? "0", "vault-index");
  const signer = parsePubkey(flag(flags, "delegated-signer") ?? payer.publicKey.toBase58(), "delegated-signer");
  const vault = flag(flags, "vault")
    ? parsePubkey(requiredFlag(flags, "vault"), "vault")
    : deriveSquadsVault(config, settings, accountIndex).address;
  const defaultMint = flag(flags, "mint") ? parsePubkey(requiredFlag(flags, "mint"), "mint") : undefined;
  const transfers = parseRebalanceTransfers(values(flags, "transfer"), defaultMint);
  const activeLanes = flag(flags, "active-lanes") ? parseU8List(requiredFlag(flags, "active-lanes"), "active-lanes") : [];
  assertRebalanceAvoidsActiveLanes(activeLanes, transfers);

  const innerInstructions = [];
  for (const batch of groupTransfersByMint(transfers)) {
    const mintInfo = await fetchMintInfo(connection, batch.mint);
    innerInstructions.push(
      clearSignerForPubkey(
        buildHubRebalanceInstruction({
          programId,
          inventoryRebalancer: vault,
          mint: batch.mint,
          tokenProgram: mintInfo.tokenProgram,
          transfers: batch.transfers,
        }),
        vault,
      ),
    );
  }

  const wrapper = createSquadsSyncTransactionInstruction(config, {
    settings,
    signer,
    accountIndex,
    instructions: innerInstructions,
  });
  const run = await executeOrSimulate({
    cluster,
    connection,
    feePayer: payer,
    signers: loadAdditionalSigners(flags, payer),
    instructions: [wrapper],
    lookupTableAddresses: values(flags, "lookup-table"),
    simulateOnly: boolFlag(flags, "simulate"),
  });
  output(flags, {
    command: "squads-rebalance-inventory",
    settings,
    vault,
    signer,
    transfers,
    transaction: run,
  });
}

async function treasuryJupiterRebalance(flags: Map<string, string[]>): Promise<void> {
  const { cluster, config, connection } = connectionContext(flags);
  const payer = loadKeypair(requiredFlag(flags, "keypair"));
  const programId = parsePubkey(flag(flags, "program-id") ?? config.loyalHubSwapProgramId.toBase58(), "program-id");
  const settings = parsePubkey(requiredFlag(flags, "settings"), "settings");
  const accountIndex = parseU8(flag(flags, "vault-index") ?? "0", "vault-index");
  const signer = parsePubkey(flag(flags, "delegated-signer") ?? payer.publicKey.toBase58(), "delegated-signer");
  const vault = flag(flags, "vault")
    ? parsePubkey(requiredFlag(flags, "vault"), "vault")
    : deriveSquadsVault(config, settings, accountIndex).address;
  const laneId = parseU8(requiredFlag(flags, "lane-id"), "lane-id");
  const inputMint = parsePubkey(requiredFlag(flags, "input-mint"), "input-mint");
  const outputMint = parsePubkey(requiredFlag(flags, "output-mint"), "output-mint");
  const hubInputAmount = parseU64(requiredFlag(flags, "hub-input-amount"), "hub-input-amount");
  const hubOutputTopUpAmount = parseU64(requiredFlag(flags, "hub-output-top-up-amount"), "hub-output-top-up-amount");
  const slippageBps = Number(flag(flags, "slippage-bps") ?? "50");
  const quoteApi = flag(flags, "quote-api") ?? DEFAULT_QUOTE_API;
  const swapInstructionsApi = flag(flags, "swap-instructions-api") ?? DEFAULT_SWAP_INSTRUCTIONS_API;
  const computeUnitLimit = Number(flag(flags, "compute-unit-limit") ?? "800000");

  const inputMintInfo = await fetchMintInfo(connection, inputMint);
  const outputMintInfo = await fetchMintInfo(connection, outputMint);
  const hubAuthority = deriveHubAuthority(programId, laneId);
  const hubInput = associatedTokenAddress(inputMint, hubAuthority, inputMintInfo.tokenProgram, true);
  const hubOutput = associatedTokenAddress(outputMint, hubAuthority, outputMintInfo.tokenProgram, true);
  const treasuryInput = associatedTokenAddress(inputMint, vault, inputMintInfo.tokenProgram, true);
  const treasuryOutput = associatedTokenAddress(outputMint, vault, outputMintInfo.tokenProgram, true);

  const quote = await fetchJupiterQuote({
    quoteApi,
    inputMint,
    outputMint,
    amount: hubInputAmount,
    slippageBps,
  });
  const swap = await fetchJupiterSwapInstructions({
    swapInstructionsApi,
    quote,
    userPublicKey: vault,
  });
  const quoteOut = quoteAmount(quote, "outAmount");
  const quoteMinOut = quoteAmount(quote, "otherAmountThreshold");
  if (quoteMinOut < hubOutputTopUpAmount) {
    throw new Error(
      `Jupiter guaranteed output ${quoteMinOut} is below Hub top-up ${hubOutputTopUpAmount}`,
    );
  }

  const setupAtas = [
    createAssociatedTokenAccountIdempotentInstruction(
      payer.publicKey,
      treasuryInput,
      vault,
      inputMint,
      inputMintInfo.tokenProgram,
      ASSOCIATED_TOKEN_PROGRAM_ID,
    ),
    createAssociatedTokenAccountIdempotentInstruction(
      payer.publicKey,
      treasuryOutput,
      vault,
      outputMint,
      outputMintInfo.tokenProgram,
      ASSOCIATED_TOKEN_PROGRAM_ID,
    ),
  ];
  const withdraw = clearSignerForPubkey(
    buildHubWithdrawInstruction({
      programId,
      admin: vault,
      destination: treasuryInput,
      mint: inputMint,
      tokenProgram: inputMintInfo.tokenProgram,
      amount: hubInputAmount,
      laneId,
    }),
    vault,
  );
  const topUp = clearSignerForPubkey(
    createTransferCheckedInstruction(
      treasuryOutput,
      outputMint,
      hubOutput,
      vault,
      hubOutputTopUpAmount,
      outputMintInfo.decimals,
      [],
      outputMintInfo.tokenProgram,
    ),
    vault,
  );
  const innerInstructions = [
    ...setupAtas,
    withdraw,
    ...jupiterInstructions(swap.setupInstructions).map((instruction) => clearSignerForPubkey(instruction, vault)),
    clearSignerForPubkey(jupiterInstruction(swap.swapInstruction), vault),
    ...jupiterInstructions(swap.cleanupInstruction ? [swap.cleanupInstruction] : []).map((instruction) =>
      clearSignerForPubkey(instruction, vault),
    ),
    topUp,
  ];
  const wrapper = createSquadsSyncTransactionInstruction(config, {
    settings,
    signer,
    accountIndex,
    instructions: innerInstructions,
  });
  const outerInstructions = [
    ComputeBudgetProgram.setComputeUnitLimit({ units: computeUnitLimit }),
    ...jupiterComputeBudgetInstructions(swap.computeBudgetInstructions),
    wrapper,
  ];
  const watchedAccounts = [hubInput, hubOutput, treasuryInput, treasuryOutput];
  const before = await fetchTokenBalances(connection, watchedAccounts);

  const run = await executeOrSimulate({
    cluster,
    connection,
    feePayer: payer,
    signers: loadAdditionalSigners(flags, payer),
    instructions: outerInstructions,
    lookupTableAddresses: [...values(flags, "lookup-table"), ...(swap.addressLookupTableAddresses ?? [])],
    simulateOnly: boolFlag(flags, "simulate"),
  });

  let deltas: Record<string, string> | null = null;
  if (run.mode === "execute") {
    const after = await fetchTokenBalances(connection, watchedAccounts);
    deltas = balanceDeltas(before, after);
    const expectedTreasuryOutputDelta = flag(flags, "expect-treasury-output-delta")
      ? parseI64(requiredFlag(flags, "expect-treasury-output-delta"), "expect-treasury-output-delta")
      : quoteOut - hubOutputTopUpAmount;
    assertTokenDelta(deltas, hubInput, -hubInputAmount);
    assertTokenDelta(deltas, hubOutput, hubOutputTopUpAmount);
    assertTokenDelta(deltas, treasuryInput, 0n);
    assertTokenDelta(deltas, treasuryOutput, expectedTreasuryOutputDelta);
  }

  output(flags, {
    command: "treasury-jupiter-rebalance",
    settings,
    vault,
    signer,
    laneId,
    accounts: {
      hubInput,
      hubOutput,
      treasuryInput,
      treasuryOutput,
    },
    quote: {
      outAmount: quoteOut,
      otherAmountThreshold: quoteMinOut,
      topUpAmount: hubOutputTopUpAmount,
    },
    deltas,
    transaction: run,
  });
}

async function checkActiveLaneRebalance(flags: Map<string, string[]>): Promise<void> {
  const defaultMint = flag(flags, "mint") ? parsePubkey(requiredFlag(flags, "mint"), "mint") : undefined;
  const activeLanes = parseU8List(requiredFlag(flags, "active-lanes"), "active-lanes");
  const transfers = parseRebalanceTransfers(values(flags, "transfer"), defaultMint);
  assertRebalanceAvoidsActiveLanes(activeLanes, transfers);
  output(flags, {
    command: "check-active-lane-rebalance",
    status: "ok",
    activeLanes,
    transfers,
  });
}

function connectionContext(flags: Map<string, string[]>): {
  cluster: string;
  loyalCluster: LoyalCluster;
  config: (typeof LOYAL_CLUSTER_CONFIGS)[LoyalCluster];
  connection: Connection;
} {
  const cluster = requiredFlag(flags, "cluster");
  const loyalCluster = parseLoyalCluster(cluster);
  return {
    cluster,
    loyalCluster,
    config: LOYAL_CLUSTER_CONFIGS[loyalCluster],
    connection: new Connection(resolveRpcUrl(cluster), DEFAULT_COMMITMENT),
  };
}

async function executeOrSimulate(args: {
  cluster: string;
  connection: Connection;
  feePayer: Keypair;
  signers: Keypair[];
  instructions: TransactionInstruction[];
  lookupTableAddresses: string[];
  simulateOnly: boolean;
}): Promise<TransactionRun> {
  if (isMainnet(args.cluster) && !args.simulateOnly && process.env.CONFIRM_MAINNET !== "1") {
    throw new Error("live mainnet execution requires CONFIRM_MAINNET=1");
  }

  const latestBlockhash = await args.connection.getLatestBlockhash(DEFAULT_COMMITMENT);
  const lookupTables = await fetchLookupTables(args.connection, args.lookupTableAddresses);
  const message = new TransactionMessage({
    payerKey: args.feePayer.publicKey,
    recentBlockhash: latestBlockhash.blockhash,
    instructions: args.instructions,
  }).compileToV0Message(lookupTables);
  const transaction = new VersionedTransaction(message);
  const signers = uniqueSigners([args.feePayer, ...args.signers]);
  transaction.sign(signers);

  const simulation = await args.connection.simulateTransaction(transaction, {
    commitment: DEFAULT_COMMITMENT,
    sigVerify: true,
  });
  const logs = simulation.value.logs ?? [];
  if (simulation.value.err) {
    throw new Error(`simulation failed: ${JSON.stringify(simulation.value.err)}\n${logs.join("\n")}`);
  }
  if (args.simulateOnly) {
    return {
      mode: "simulate",
      signature: null,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
      logs,
    };
  }

  const signature = await args.connection.sendRawTransaction(transaction.serialize(), {
    maxRetries: 3,
    skipPreflight: false,
  });
  const confirmation = await args.connection.confirmTransaction(
    {
      signature,
      blockhash: latestBlockhash.blockhash,
      lastValidBlockHeight: latestBlockhash.lastValidBlockHeight,
    },
    DEFAULT_COMMITMENT,
  );
  if (confirmation.value.err) {
    throw new Error(`transaction ${signature} failed: ${JSON.stringify(confirmation.value.err)}`);
  }

  return {
    mode: "execute",
    signature,
    unitsConsumed: simulation.value.unitsConsumed ?? null,
    logs,
  };
}

function loadInstructionFiles(paths: readonly string[]): TransactionInstruction[] {
  if (paths.length === 0) {
    throw new Error("at least one --instruction-file is required");
  }
  return paths.flatMap((path) => {
    const parsed = JSON.parse(readFileSync(path, "utf8")) as WireInstruction | WireInstruction[];
    const values = Array.isArray(parsed) ? parsed : [parsed];
    return values.map(instructionFromWire);
  });
}

async function fetchMintInfo(connection: Connection, mint: PublicKey): Promise<MintInfo> {
  const account = await connection.getAccountInfo(mint, DEFAULT_COMMITMENT);
  if (!account) {
    throw new Error(`mint account does not exist: ${mint.toBase58()}`);
  }
  const tokenProgram = supportedTokenProgram(account.owner);
  const mintInfo = await getMint(connection, mint, DEFAULT_COMMITMENT, tokenProgram);
  return { tokenProgram, decimals: mintInfo.decimals };
}

function supportedTokenProgram(owner: PublicKey): PublicKey {
  if (owner.equals(TOKEN_PROGRAM_ID)) {
    return TOKEN_PROGRAM_ID;
  }
  if (owner.equals(TOKEN_2022_PROGRAM_ID)) {
    return TOKEN_2022_PROGRAM_ID;
  }
  throw new Error(`unsupported mint owner ${owner.toBase58()}`);
}

function buildHubWithdrawInstruction(args: {
  programId: PublicKey;
  admin: PublicKey;
  destination: PublicKey;
  mint: PublicKey;
  tokenProgram: PublicKey;
  amount: bigint;
  laneId: number;
}): TransactionInstruction {
  return new TransactionInstruction({
    programId: args.programId,
    keys: [
      { pubkey: deriveConfig(args.programId), isSigner: false, isWritable: true },
      { pubkey: args.admin, isSigner: true, isWritable: false },
      {
        pubkey: associatedTokenAddress(args.mint, deriveHubAuthority(args.programId, args.laneId), args.tokenProgram, true),
        isSigner: false,
        isWritable: true,
      },
      { pubkey: args.destination, isSigner: false, isWritable: true },
      { pubkey: args.mint, isSigner: false, isWritable: false },
      { pubkey: deriveHubAuthority(args.programId, args.laneId), isSigner: false, isWritable: false },
      { pubkey: args.tokenProgram, isSigner: false, isWritable: false },
    ],
    data: Buffer.from(withdrawInventoryData(args.amount, args.laneId)),
  });
}

function buildHubRebalanceInstruction(args: {
  programId: PublicKey;
  inventoryRebalancer: PublicKey;
  mint: PublicKey;
  tokenProgram: PublicKey;
  transfers: Omit<RebalanceTransfer, "mint">[];
}): TransactionInstruction {
  return new TransactionInstruction({
    programId: args.programId,
    keys: [
      { pubkey: deriveConfig(args.programId), isSigner: false, isWritable: false },
      { pubkey: args.inventoryRebalancer, isSigner: true, isWritable: false },
      { pubkey: args.tokenProgram, isSigner: false, isWritable: false },
      { pubkey: args.mint, isSigner: false, isWritable: false },
      ...args.transfers.flatMap((transfer) => [
        { pubkey: deriveHubAuthority(args.programId, transfer.fromLaneId), isSigner: false, isWritable: false },
        {
          pubkey: associatedTokenAddress(
            args.mint,
            deriveHubAuthority(args.programId, transfer.fromLaneId),
            args.tokenProgram,
            true,
          ),
          isSigner: false,
          isWritable: true,
        },
        {
          pubkey: associatedTokenAddress(
            args.mint,
            deriveHubAuthority(args.programId, transfer.toLaneId),
            args.tokenProgram,
            true,
          ),
          isSigner: false,
          isWritable: true,
        },
      ]),
    ],
    data: Buffer.from(rebalanceInventoryData(args.transfers)),
  });
}

function withdrawInventoryData(amount: bigint, laneId: number): Uint8Array {
  const data = Buffer.alloc(10);
  data[0] = WITHDRAW_INVENTORY_TAG;
  data.writeBigUInt64LE(amount, 1);
  data[9] = laneId;
  return data;
}

function rebalanceInventoryData(transfers: readonly Omit<RebalanceTransfer, "mint">[]): Uint8Array {
  if (transfers.length === 0 || transfers.length > MAX_REBALANCE_TRANSFERS) {
    throw new Error(`Loyal Hub rebalance supports 1..=${MAX_REBALANCE_TRANSFERS} transfers per mint`);
  }
  const data = Buffer.alloc(2 + transfers.length * 10);
  data[0] = REBALANCE_INVENTORY_TAG;
  data[1] = transfers.length;
  for (const [index, transfer] of transfers.entries()) {
    if (transfer.amount <= 0n) {
      throw new Error("rebalance amount must be positive");
    }
    const offset = 2 + index * 10;
    data[offset] = transfer.fromLaneId;
    data[offset + 1] = transfer.toLaneId;
    data.writeBigUInt64LE(transfer.amount, offset + 2);
  }
  return data;
}

function clearSignerForPubkey(instruction: TransactionInstruction, pubkey: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: instruction.programId,
    keys: instruction.keys.map((account) => ({
      ...account,
      isSigner: account.pubkey.equals(pubkey) ? false : account.isSigner,
    })),
    data: instruction.data,
  });
}

function groupTransfersByMint(transfers: readonly RebalanceTransfer[]): {
  mint: PublicKey;
  transfers: Omit<RebalanceTransfer, "mint">[];
}[] {
  const groups: {
    mint: PublicKey;
    transfers: Omit<RebalanceTransfer, "mint">[];
  }[] = [];
  for (const transfer of transfers) {
    const existing = groups.find((group) => group.mint.equals(transfer.mint));
    const laneTransfer = {
      fromLaneId: transfer.fromLaneId,
      toLaneId: transfer.toLaneId,
      amount: transfer.amount,
    };
    if (existing) {
      existing.transfers.push(laneTransfer);
    } else {
      groups.push({ mint: transfer.mint, transfers: [laneTransfer] });
    }
  }
  return groups;
}

function splitTransferGroups(values: readonly string[]): string[][] {
  const tokens = values.flatMap((value) => value.split(/[,\s]+/u).filter(Boolean));
  const groups: string[][] = [];
  let current: string[] = [];
  const seen = new Set<string>();
  for (const token of tokens) {
    const key = transferKey(token);
    if (current.length > 0 && (seen.has(key) || (isTransferStartKey(key) && hasCompleteTransferFields(seen)))) {
      groups.push(current);
      current = [];
      seen.clear();
    }
    current.push(token);
    seen.add(key);
  }
  if (current.length > 0) {
    groups.push(current);
  }
  if (groups.length === 0) {
    throw new Error("at least one --transfer is required");
  }
  return groups;
}

function parseRebalanceTransfer(group: readonly string[], defaultMint?: PublicKey): RebalanceTransfer {
  let mint = defaultMint;
  let fromLaneId: number | null = null;
  let toLaneId: number | null = null;
  let amount: bigint | null = null;

  for (const token of group) {
    const [rawKey, value] = splitTransferToken(token);
    const key = normalizeKey(rawKey);
    switch (key) {
      case "mint":
        mint = parsePubkey(value, "mint");
        break;
      case "from_lane_id":
      case "from_lane":
      case "from":
        fromLaneId = parseU8(value, "fromLaneId");
        break;
      case "to_lane_id":
      case "to_lane":
      case "to":
        toLaneId = parseU8(value, "toLaneId");
        break;
      case "raw_token_amount":
      case "amount":
        amount = parseU64(value, "amount");
        break;
      default:
        throw new Error(`unknown transfer field ${rawKey}`);
    }
  }

  if (!mint) {
    throw new Error("transfer mint is required; pass --mint or mint:<PUBKEY>");
  }
  if (fromLaneId === null) {
    throw new Error("transfer from_lane_id is required");
  }
  if (toLaneId === null) {
    throw new Error("transfer to_lane_id is required");
  }
  if (amount === null) {
    throw new Error("transfer raw_token_amount is required");
  }
  return { mint, fromLaneId, toLaneId, amount };
}

function transferKey(token: string): string {
  return normalizeKey(splitTransferToken(token)[0]);
}

function splitTransferToken(token: string): [string, string] {
  const separator = token.includes(":") ? ":" : "=";
  const [key, value] = token.split(separator, 2);
  if (!key || value === undefined) {
    throw new Error(`transfer field must be key:value, got ${token}`);
  }
  return [key, value];
}

function normalizeKey(value: string): string {
  return value.replaceAll("-", "_").toLowerCase();
}

function isTransferStartKey(key: string): boolean {
  return key === "from_lane_id" || key === "from_lane" || key === "from" || key === "mint";
}

function hasCompleteTransferFields(fields: Set<string>): boolean {
  return (
    [...fields].some((field) => field === "from_lane_id" || field === "from_lane" || field === "from") &&
    [...fields].some((field) => field === "to_lane_id" || field === "to_lane" || field === "to") &&
    [...fields].some((field) => field === "raw_token_amount" || field === "amount")
  );
}

function parseRisk(value: string): RiskBasket {
  if (Object.values(RiskBasket).includes(value as RiskBasket)) {
    return value as RiskBasket;
  }
  throw new Error(`unsupported risk basket: ${value}`);
}

function parseSwapLanes(value: string): SwapLane[] {
  if (value.trim() === "") {
    return [];
  }
  return value.split(",").map((lane) => {
    const normalized = lane.trim().toLowerCase();
    if (normalized === "loyal" || normalized === "hub" || normalized === "loyal-hub") {
      return SwapLane.Loyal;
    }
    if (normalized === "jupiter") {
      return SwapLane.Jupiter;
    }
    throw new Error(`unsupported swap lane: ${lane}`);
  });
}

function parseMaxFee(value: string | undefined): MaxFeeBps | undefined {
  if (!value) {
    return undefined;
  }
  const parsed = Number(value);
  if (Object.values(MaxFeeBps).includes(parsed as MaxFeeBps)) {
    return parsed as MaxFeeBps;
  }
  throw new Error(`unsupported max-fee-bps: ${value}`);
}

function parseLoyalCluster(value: string): LoyalCluster {
  const normalized = value.toLowerCase();
  if (normalized === "m" || normalized === "mainnet" || normalized === "mainnet-beta") {
    return LoyalCluster.MainnetBeta;
  }
  if (normalized === "d" || normalized === "devnet") {
    return LoyalCluster.Devnet;
  }
  throw new Error(`unsupported Loyal cluster: ${value}`);
}

function resolveRpcUrl(value: string): string {
  switch (value.toLowerCase()) {
    case "m":
    case "mainnet":
    case "mainnet-beta":
      return "https://api.mainnet-beta.solana.com";
    case "d":
    case "devnet":
      return "https://api.devnet.solana.com";
    case "t":
    case "testnet":
      return "https://api.testnet.solana.com";
    case "l":
    case "local":
    case "localhost":
      return "http://localhost:8899";
    default:
      return value;
  }
}

function isMainnet(value: string): boolean {
  const normalized = value.toLowerCase();
  return normalized === "m" || normalized === "mainnet" || normalized === "mainnet-beta" || normalized.includes("mainnet");
}

function loadKeypair(path: string): Keypair {
  const value = JSON.parse(readFileSync(path, "utf8")) as unknown;
  if (!Array.isArray(value) || !value.every((item) => Number.isInteger(item))) {
    throw new Error(`keypair file must contain a JSON byte array: ${path}`);
  }
  return Keypair.fromSecretKey(Uint8Array.from(value as number[]));
}

function loadAdditionalSigners(flags: Map<string, string[]>, payer: Keypair): Keypair[] {
  const signers = [payer, ...values(flags, "signer-keypair").map(loadKeypair)];
  return uniqueSigners(signers);
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

async function fetchJupiterQuote(args: {
  quoteApi: string;
  inputMint: PublicKey;
  outputMint: PublicKey;
  amount: bigint;
  slippageBps: number;
}): Promise<Record<string, unknown>> {
  const url = new URL(args.quoteApi);
  url.searchParams.set("inputMint", args.inputMint.toBase58());
  url.searchParams.set("outputMint", args.outputMint.toBase58());
  url.searchParams.set("amount", args.amount.toString());
  url.searchParams.set("swapMode", "ExactIn");
  url.searchParams.set("slippageBps", String(args.slippageBps));
  url.searchParams.set("restrictIntermediateTokens", "true");
  url.searchParams.set("instructionVersion", "V2");
  return fetchJson(url.toString(), { method: "GET", headers: jupiterApiHeaders() });
}

async function fetchJupiterSwapInstructions(args: {
  swapInstructionsApi: string;
  quote: Record<string, unknown>;
  userPublicKey: PublicKey;
}): Promise<JupiterSwapInstructions> {
  return fetchJson(args.swapInstructionsApi, {
    method: "POST",
    headers: { "content-type": "application/json", ...jupiterApiHeaders() },
    body: JSON.stringify({
      quoteResponse: args.quote,
      userPublicKey: args.userPublicKey.toBase58(),
      wrapAndUnwrapSol: false,
      dynamicComputeUnitLimit: false,
    }),
  }) as Promise<JupiterSwapInstructions>;
}

async function fetchJson(url: string, init: RequestInit): Promise<Record<string, unknown>> {
  const maxAttempts = 5;
  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    const response = await fetch(url, init);
    const text = await response.text();
    if (response.ok) {
      return JSON.parse(text) as Record<string, unknown>;
    }
    if (attempt + 1 < maxAttempts && (response.status === 429 || response.status >= 500)) {
      await sleep(500 * 2 ** attempt);
      continue;
    }
    throw new Error(`${url} failed with ${response.status}: ${text}`);
  }
  throw new Error(`${url} failed after ${maxAttempts} attempts`);
}

function jupiterApiHeaders(): Record<string, string> {
  return process.env.JUPITER_API_KEY ? { "x-api-key": process.env.JUPITER_API_KEY } : {};
}

function jupiterInstructions(values: readonly JupiterInstructionJson[] = []): TransactionInstruction[] {
  return values.map(jupiterInstruction);
}

function jupiterComputeBudgetInstructions(values: readonly JupiterInstructionJson[] = []): TransactionInstruction[] {
  return jupiterInstructions(values).filter(
    (instruction) => !instruction.programId.equals(ComputeBudgetProgram.programId) || instruction.data[0] !== 2,
  );
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

function quoteAmount(quote: Record<string, unknown>, name: string): bigint {
  const value = quote[name];
  if (typeof value !== "string" || !/^\d+$/u.test(value)) {
    throw new Error(`Jupiter quote missing valid ${name}`);
  }
  return BigInt(value);
}

async function fetchLookupTables(connection: Connection, addresses: readonly string[]): Promise<AddressLookupTableAccount[]> {
  const uniqueAddresses = [...new Set(addresses.filter(Boolean))];
  if (uniqueAddresses.length === 0) {
    return [];
  }
  const keys = uniqueAddresses.map((address) => new PublicKey(address));
  const accounts = await connection.getMultipleAccountsInfo(keys, DEFAULT_COMMITMENT);
  return accounts.map((account, index) => {
    if (!account) {
      throw new Error(`missing address lookup table ${keys[index].toBase58()}`);
    }
    return new AddressLookupTableAccount({
      key: keys[index],
      state: AddressLookupTableAccount.deserialize(account.data),
    });
  });
}

function associatedTokenAddress(mint: PublicKey, owner: PublicKey, tokenProgram: PublicKey, allowOwnerOffCurve: boolean): PublicKey {
  return getAssociatedTokenAddressSync(mint, owner, allowOwnerOffCurve, tokenProgram, ASSOCIATED_TOKEN_PROGRAM_ID);
}

function deriveConfig(programId: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([CONFIG_SEED], programId)[0];
}

function deriveHubAuthority(programId: PublicKey, laneId: number): PublicKey {
  return PublicKey.findProgramAddressSync([HUB_AUTHORITY_SEED, Buffer.from([laneId])], programId)[0];
}

async function fetchTokenBalances(connection: Connection, accounts: readonly PublicKey[]): Promise<BalanceSnapshot> {
  const entries = await Promise.all(
    accounts.map(async (account) => [account.toBase58(), await fetchTokenBalance(connection, account)] as const),
  );
  return Object.fromEntries(entries);
}

async function fetchTokenBalance(connection: Connection, account: PublicKey): Promise<bigint> {
  try {
    const balance = await connection.getTokenAccountBalance(account, DEFAULT_COMMITMENT);
    return BigInt(balance.value.amount);
  } catch (error) {
    if (error instanceof Error && /could not find account|Invalid param|AccountNotFound/i.test(error.message)) {
      return 0n;
    }
    throw error;
  }
}

function balanceDeltas(before: BalanceSnapshot, after: BalanceSnapshot): Record<string, string> {
  const deltas: Record<string, string> = {};
  for (const [account, beforeAmount] of Object.entries(before)) {
    deltas[account] = ((after[account] ?? 0n) - beforeAmount).toString();
  }
  return deltas;
}

function assertTokenDelta(deltas: Record<string, string>, account: PublicKey, expected: bigint): void {
  const actual = BigInt(deltas[account.toBase58()] ?? "0");
  if (actual !== expected) {
    throw new Error(`unexpected token delta for ${account.toBase58()}: expected ${expected}, got ${actual}`);
  }
}

function decodeInstructionData(data: string | number[], encoding: WireInstruction["encoding"]): Uint8Array {
  if (Array.isArray(data)) {
    return Uint8Array.from(data);
  }
  switch (encoding ?? "base64") {
    case "base64":
      return Buffer.from(data, "base64");
    case "hex":
      return Buffer.from(data.replace(/^0x/u, ""), "hex");
    case "bytes":
      return Uint8Array.from(data.split(",").map((value) => Number(value.trim())));
  }
}

function serializeRoutes(routes: unknown): unknown {
  return JSON.parse(stringify(routes));
}

function output(flags: Map<string, string[]>, value: unknown): void {
  if (boolFlag(flags, "json")) {
    console.log(stringify(value));
    return;
  }
  console.log(stringify(value));
}

function stringify(value: unknown): string {
  return JSON.stringify(
    value,
    (_key, inner) => {
      if (typeof inner === "bigint") {
        return inner.toString();
      }
      if (inner instanceof PublicKey) {
        return inner.toBase58();
      }
      if (inner instanceof Uint8Array) {
        return Buffer.from(inner).toString("base64");
      }
      return inner;
    },
    2,
  );
}

function flag(flags: Map<string, string[]>, name: string): string | undefined {
  return flags.get(name)?.at(-1);
}

function values(flags: Map<string, string[]>, name: string): string[] {
  return flags.get(name) ?? [];
}

function boolFlag(flags: Map<string, string[]>, name: string): boolean {
  return flag(flags, name) === "1" || flag(flags, name) === "true";
}

function requiredFlag(flags: Map<string, string[]>, name: string): string {
  const value = flag(flags, name);
  if (!value) {
    throw new Error(`missing --${name}`);
  }
  return value;
}

function parsePubkey(value: string, field: string): PublicKey {
  try {
    return new PublicKey(value);
  } catch (error) {
    throw new Error(`invalid ${field} pubkey: ${error instanceof Error ? error.message : String(error)}`);
  }
}

function parseU8(value: string, field: string): number {
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 0 || parsed > 255) {
    throw new Error(`${field} must be a u8`);
  }
  return parsed;
}

function parseU8List(value: string, field: string): number[] {
  if (value.trim() === "") {
    return [];
  }
  return value.split(",").map((entry) => parseU8(entry.trim(), field));
}

function parseU64(value: string, field: string): bigint {
  if (!/^\d+$/u.test(value)) {
    throw new Error(`${field} must be an unsigned integer`);
  }
  const parsed = BigInt(value);
  if (parsed > 0xffffffffffffffffn) {
    throw new Error(`${field} must fit in u64`);
  }
  return parsed;
}

function toBigInt(value: unknown, field: string): bigint {
  if (typeof value === "bigint") {
    return value;
  }
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value) || value < 0) {
      throw new Error(`${field} must be a safe unsigned integer`);
    }
    return BigInt(value);
  }
  if (typeof value === "string") {
    return parseU128(value, field);
  }
  if (value && typeof value === "object" && "toString" in value && typeof value.toString === "function") {
    return parseU128(value.toString(), field);
  }
  throw new Error(`${field} must be an unsigned integer`);
}

function parseI64(value: string, field: string): bigint {
  if (!/^-?\d+$/u.test(value)) {
    throw new Error(`${field} must be an integer`);
  }
  return BigInt(value);
}

function parseU128(value: string, field: string): bigint {
  if (!/^\d+$/u.test(value)) {
    throw new Error(`${field} must be an unsigned integer`);
  }
  const parsed = BigInt(value);
  if (parsed > 0xffffffffffffffffffffffffffffffffn) {
    throw new Error(`${field} must fit in u128`);
  }
  return parsed;
}

function sleep(delayMs: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, delayMs));
}

if (import.meta.main) {
  main().catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exit(1);
  });
}
