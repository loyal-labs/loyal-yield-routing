#!/usr/bin/env bun

import {
  Connection,
  Keypair,
  PublicKey,
  Transaction,
  clusterApiUrl,
} from "@solana/web3.js";
import type { TransactionInstruction } from "@solana/web3.js";
import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import {
  LOYAL_CLUSTER_CONFIGS,
  LoyalCluster,
  MaxFeeBps,
  RiskBasket,
  Stablecoin,
  SwapLane,
  createLoyalActionsSdk,
} from "../src/index.js";
import {
  createSquadsSmartAccountInstruction,
  decodeSquadsProgramConfig,
  deriveSquadsProgramConfig,
  deriveSquadsSettings,
  deriveSquadsVault,
} from "../src/internal/squads.js";

const SOLANA_LEGACY_TRANSACTION_PACKET_BYTES = 1_232;

type CliOptions = {
  cluster: LoyalCluster;
  rpcUrl: string;
  keypairPath: string;
  userAddress?: PublicKey;
  delegatedSigner?: PublicKey;
  seed?: bigint;
  treasury?: PublicKey;
  vaultIndex: number;
  risk: RiskBasket;
  stablecoins: Stablecoin[];
  swapLanes: SwapLane[];
  maxFeeBps?: MaxFeeBps;
  dryRun: boolean;
};

const USAGE = `Usage:
  bun ./scripts/create-yield-route-policy.ts --keypair <path> [options]

Options:
  --keypair <path>             User keypair JSON file. Also accepts --user-keypair.
  --user-address <pubkey>      Optional safety check; must match the keypair pubkey.
  --delegated-signer <pubkey>  Policy signer. Defaults to the user address.
  --cluster <name>             devnet or mainnet-beta. Defaults to devnet.
  --rpc-url <url>              Solana RPC URL. Defaults to the selected cluster URL.
  --seed <u128>                Squads smart-account seed. Defaults to program index + 1.
  --treasury <pubkey>          Squads creation-fee treasury. Defaults to program config treasury.
  --vault-index <u8>           Squads vault/account index. Defaults to 0.
  --risk <basket>              safe, medium, or aggressive. Defaults to safe.
  --stablecoins <symbols>      Comma-separated symbols or all. Defaults to USDC,PYUSD.
  --swap-lanes <lanes>         Comma-separated jupiter,loyal lanes. Defaults to jupiter.
  --max-fee-bps <bps>          50, 75, 100, 125, or 150. Defaults to SDK default.
  --dry-run                    Build and report transactions without sending them.
`;

async function main(): Promise<void> {
  const options = parseCli(Bun.argv.slice(2));
  const userKeypair = await readKeypair(options.keypairPath);
  const keypairAddress = userKeypair.publicKey;
  if (options.userAddress && !options.userAddress.equals(keypairAddress)) {
    throw new Error(
      `--user-address ${options.userAddress.toBase58()} does not match keypair pubkey ${keypairAddress.toBase58()}`,
    );
  }

  const userAddress = options.userAddress ?? keypairAddress;
  const delegatedSigner = options.delegatedSigner ?? userAddress;
  const clusterConfig = LOYAL_CLUSTER_CONFIGS[options.cluster];
  const connection = new Connection(options.rpcUrl, "confirmed");
  const programConfigAddress = deriveSquadsProgramConfig(clusterConfig);
  const programConfigAccount = await connection.getAccountInfo(programConfigAddress, "confirmed");
  if (!programConfigAccount) {
    throw new Error(`Squads program config account not found: ${programConfigAddress.toBase58()}`);
  }
  const programConfig = decodeSquadsProgramConfig(programConfigAccount.data);
  const seed = options.seed ?? programConfig.smartAccountIndex + 1n;
  const treasury = options.treasury ?? programConfig.treasury;
  const settings = deriveSquadsSettings(clusterConfig, seed);
  const vault = deriveSquadsVault(clusterConfig, settings, options.vaultIndex);
  const existingSettings = await connection.getAccountInfo(settings, "confirmed");
  const createSmartAccountInstruction = createSquadsSmartAccountInstruction(clusterConfig, {
    payer: userAddress,
    verifier: userAddress,
    seed,
    treasury,
  });
  const sdk = createLoyalActionsSdk({ cluster: options.cluster });
  const policy = sdk.initYieldRoutePolicy({
    risk: options.risk,
    stablecoins: options.stablecoins,
    swapLanes: options.swapLanes,
    maxFeeBps: options.maxFeeBps,
    squads: {
      settings,
      authority: userAddress,
      delegatedSigner,
      accountIndex: options.vaultIndex,
      vault,
    },
  });

  const createSmartAccountTxSize = await transactionSize(connection, userKeypair, [
    createSmartAccountInstruction,
  ]);
  const createPolicyTxSize = await transactionSize(connection, userKeypair, policy.instructions);

  printPlan({
    options,
    userAddress,
    delegatedSigner,
    programConfigAddress,
    programConfig,
    seed,
    treasury,
    settings,
    vault,
    actionAccount: policy.actionAccount,
    routes: routesToJson(policy.routes),
    existingSettings: existingSettings !== null,
    createSmartAccountTxSize,
    createPolicyTxSize,
  });

  if (options.dryRun) {
    console.log("Dry run only; no transactions were sent.");
    return;
  }
  if (createSmartAccountTxSize > SOLANA_LEGACY_TRANSACTION_PACKET_BYTES) {
    throw new Error(
      `create smart account transaction is ${createSmartAccountTxSize} bytes, above the ${SOLANA_LEGACY_TRANSACTION_PACKET_BYTES} byte packet limit`,
    );
  }
  if (createPolicyTxSize > SOLANA_LEGACY_TRANSACTION_PACKET_BYTES) {
    throw new Error(
      `create policy transaction is ${createPolicyTxSize} bytes, above the ${SOLANA_LEGACY_TRANSACTION_PACKET_BYTES} byte packet limit`,
    );
  }
  if (existingSettings) {
    throw new Error(
      `Squads settings account already exists at ${settings.toBase58()}; choose another --seed`,
    );
  }

  const createSmartAccountSignature = await sendInstructions(
    connection,
    userKeypair,
    [createSmartAccountInstruction],
  );
  const createPolicySignature = await sendInstructions(connection, userKeypair, policy.instructions);
  console.log(
    JSON.stringify(
      {
        createSmartAccountSignature,
        createPolicySignature,
      },
      null,
      2,
    ),
  );
}

function parseCli(args: string[]): CliOptions {
  const flags = collectFlags(args);
  if (flags.has("help") || flags.has("h")) {
    console.log(USAGE);
    process.exit(0);
  }

  const cluster = parseCluster(optionalValue(flags, "cluster") ?? "devnet");
  return {
    cluster,
    rpcUrl: optionalValue(flags, "rpc-url") ?? clusterApiUrl(cluster as "devnet" | "mainnet-beta"),
    keypairPath: requiredValue(flags, ["keypair", "user-keypair"], "user keypair path"),
    userAddress: optionalPubkey(flags, "user-address"),
    delegatedSigner: optionalPubkey(flags, "delegated-signer"),
    seed: optionalBigInt(flags, "seed"),
    treasury: optionalPubkey(flags, "treasury"),
    vaultIndex: optionalInteger(flags, "vault-index", 0, 0, 255),
    risk: parseRisk(optionalValue(flags, "risk") ?? "safe"),
    stablecoins: parseStablecoins(optionalValue(flags, "stablecoins") ?? "USDC,PYUSD"),
    swapLanes: parseSwapLanes(optionalValue(flags, "swap-lanes") ?? "jupiter"),
    maxFeeBps: optionalMaxFeeBps(flags, "max-fee-bps"),
    dryRun: flags.has("dry-run"),
  };
}

function collectFlags(args: string[]): Map<string, string | true> {
  const flags = new Map<string, string | true>();
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    if (!arg?.startsWith("--")) {
      throw new Error(`unexpected positional argument: ${String(arg)}`);
    }
    const raw = arg.slice(2);
    const equalsIndex = raw.indexOf("=");
    if (equalsIndex !== -1) {
      flags.set(raw.slice(0, equalsIndex), raw.slice(equalsIndex + 1));
      continue;
    }
    const next = args[index + 1];
    if (!next || next.startsWith("--")) {
      flags.set(raw, true);
      continue;
    }
    flags.set(raw, next);
    index += 1;
  }
  return flags;
}

function requiredValue(
  flags: Map<string, string | true>,
  names: string[],
  label: string,
): string {
  for (const name of names) {
    const value = optionalValue(flags, name);
    if (value) {
      return value;
    }
  }
  throw new Error(`missing required ${label}\n\n${USAGE}`);
}

function optionalValue(flags: Map<string, string | true>, name: string): string | undefined {
  const value = flags.get(name);
  if (value === undefined) {
    return undefined;
  }
  if (value === true) {
    throw new Error(`--${name} requires a value`);
  }
  if (value.length === 0) {
    throw new Error(`--${name} cannot be empty`);
  }
  return value;
}

function optionalPubkey(flags: Map<string, string | true>, name: string): PublicKey | undefined {
  const value = optionalValue(flags, name);
  return value ? new PublicKey(value) : undefined;
}

function optionalInteger(
  flags: Map<string, string | true>,
  name: string,
  defaultValue: number,
  min: number,
  max: number,
): number {
  const value = optionalValue(flags, name);
  if (!value) {
    return defaultValue;
  }
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < min || parsed > max) {
    throw new Error(`--${name} must be an integer in ${min}..=${max}`);
  }
  return parsed;
}

function optionalBigInt(flags: Map<string, string | true>, name: string): bigint | undefined {
  const value = optionalValue(flags, name);
  if (!value) {
    return undefined;
  }
  const parsed = BigInt(value);
  if (parsed <= 0n || parsed > (1n << 128n) - 1n) {
    throw new Error(`--${name} must be in the range 1..=u128::MAX`);
  }
  return parsed;
}

function parseCluster(value: string): LoyalCluster {
  if (value === LoyalCluster.Devnet || value === LoyalCluster.MainnetBeta) {
    return value;
  }
  throw new Error(`unsupported --cluster ${value}; expected devnet or mainnet-beta`);
}

function parseRisk(value: string): RiskBasket {
  if (value === RiskBasket.Safe || value === RiskBasket.Medium || value === RiskBasket.Aggressive) {
    return value;
  }
  throw new Error(`unsupported --risk ${value}; expected safe, medium, or aggressive`);
}

function parseStablecoins(value: string): Stablecoin[] {
  const symbols =
    value.toLowerCase() === "all"
      ? Object.values(Stablecoin)
      : value.split(",").map((symbol) => symbol.trim()).filter(Boolean);
  if (symbols.length === 0) {
    throw new Error("--stablecoins must include at least one symbol");
  }
  const parsed = symbols.map((symbol) => {
    const stablecoin = Object.values(Stablecoin).find(
      (candidate) => candidate.toLowerCase() === symbol.toLowerCase(),
    );
    if (!stablecoin) {
      throw new Error(
        `unsupported stablecoin ${symbol}; expected one of ${Object.values(Stablecoin).join(", ")}, or all`,
      );
    }
    return stablecoin;
  });
  if (new Set(parsed).size !== parsed.length) {
    throw new Error("--stablecoins cannot contain duplicates");
  }
  return parsed;
}

function parseSwapLanes(value: string): SwapLane[] {
  const lanes = value.split(",").map((lane) => lane.trim()).filter(Boolean);
  if (lanes.length === 0) {
    throw new Error("--swap-lanes must include at least one lane");
  }
  const parsed = lanes.map((lane) => {
    if (lane === SwapLane.Jupiter || lane === SwapLane.Loyal) {
      return lane;
    }
    throw new Error(`unsupported swap lane ${lane}; expected jupiter or loyal`);
  });
  if (new Set(parsed).size !== parsed.length) {
    throw new Error("--swap-lanes cannot contain duplicates");
  }
  return parsed;
}

function optionalMaxFeeBps(
  flags: Map<string, string | true>,
  name: string,
): MaxFeeBps | undefined {
  const value = optionalValue(flags, name);
  if (!value) {
    return undefined;
  }
  const parsed = Number(value);
  if (
    parsed === MaxFeeBps.Bps50 ||
    parsed === MaxFeeBps.Bps75 ||
    parsed === MaxFeeBps.Bps100 ||
    parsed === MaxFeeBps.Bps125 ||
    parsed === MaxFeeBps.Bps150
  ) {
    return parsed;
  }
  throw new Error(`--${name} must be one of 50, 75, 100, 125, or 150`);
}

async function readKeypair(path: string): Promise<Keypair> {
  const content = await readFile(expandHome(path), "utf8");
  const parsed: unknown = JSON.parse(content);
  const secretKey = Array.isArray(parsed)
    ? parsed
    : typeof parsed === "object" && parsed !== null && "secretKey" in parsed
      ? (parsed as { secretKey: unknown }).secretKey
      : undefined;
  if (!Array.isArray(secretKey) || secretKey.some((byte) => !Number.isInteger(byte))) {
    throw new Error("keypair file must contain a JSON byte array or {\"secretKey\": [...] }");
  }
  return Keypair.fromSecretKey(Uint8Array.from(secretKey));
}

function expandHome(path: string): string {
  return path.startsWith("~/") ? `${homedir()}${path.slice(1)}` : path;
}

async function transactionSize(
  connection: Connection,
  payer: Keypair,
  instructions: TransactionInstruction[],
): Promise<number> {
  const { tx } = await unsignedTransaction(connection, payer, instructions);
  return legacyTransactionWireSize(tx, instructions);
}

async function sendInstructions(
  connection: Connection,
  payer: Keypair,
  instructions: TransactionInstruction[],
): Promise<string> {
  const { tx, latestBlockhash } = await unsignedTransaction(connection, payer, instructions);
  tx.sign(payer);
  const signature = await connection.sendRawTransaction(tx.serialize(), {
    skipPreflight: false,
  });
  const confirmation = await connection.confirmTransaction(
    { signature, ...latestBlockhash },
    "confirmed",
  );
  if (confirmation.value.err) {
    throw new Error(`transaction ${signature} failed: ${JSON.stringify(confirmation.value.err)}`);
  }
  return signature;
}

async function unsignedTransaction(
  connection: Connection,
  payer: Keypair,
  instructions: TransactionInstruction[],
): Promise<{
  tx: Transaction;
  latestBlockhash: { blockhash: string; lastValidBlockHeight: number };
}> {
  const latestBlockhash = await connection.getLatestBlockhash("confirmed");
  const tx = new Transaction({
    feePayer: payer.publicKey,
    blockhash: latestBlockhash.blockhash,
    lastValidBlockHeight: latestBlockhash.lastValidBlockHeight,
  }).add(...instructions);
  return { tx, latestBlockhash };
}

function legacyTransactionWireSize(
  tx: Transaction,
  instructions: TransactionInstruction[],
): number {
  const message = tx.compileMessage();
  const messageSize =
    3 +
    shortvecLength(message.accountKeys.length) +
    message.accountKeys.length * 32 +
    32 +
    shortvecLength(message.instructions.length) +
    message.instructions.reduce((total, instruction, index) => {
      const dataLength = instructions[index]?.data.length ?? 0;
      return (
        total +
        1 +
        shortvecLength(instruction.accounts.length) +
        instruction.accounts.length +
        shortvecLength(dataLength) +
        dataLength
      );
    }, 0);
  return (
    shortvecLength(message.header.numRequiredSignatures) +
    message.header.numRequiredSignatures * 64 +
    messageSize
  );
}

function shortvecLength(length: number): number {
  let remaining = length;
  let bytes = 0;
  for (;;) {
    bytes += 1;
    remaining >>= 7;
    if (remaining === 0) {
      return bytes;
    }
  }
}

function routesToJson(routes: {
  sameMint: { actionAccount: PublicKey; instructionConstraintIndexes: readonly number[] };
  jupiter?: { actionAccount: PublicKey; instructionConstraintIndexes: readonly number[] };
  loyal?: { actionAccount: PublicKey; instructionConstraintIndexes: readonly number[] };
}): Record<string, unknown> {
  const output: Record<string, unknown> = {
    sameMint: routeToJson(routes.sameMint),
  };
  if (routes.jupiter) {
    output.jupiter = routeToJson(routes.jupiter);
  }
  if (routes.loyal) {
    output.loyal = routeToJson(routes.loyal);
  }
  return output;
}

function routeToJson(route: {
  actionAccount: PublicKey;
  instructionConstraintIndexes: readonly number[];
}): Record<string, unknown> {
  return {
    actionAccount: route.actionAccount.toBase58(),
    instructionConstraintIndexes: [...route.instructionConstraintIndexes],
  };
}

function redactedRpcUrl(value: string): string {
  try {
    const url = new URL(value);
    if (url.username) {
      url.username = "REDACTED";
    }
    if (url.password) {
      url.password = "REDACTED";
    }
    for (const key of [...url.searchParams.keys()]) {
      url.searchParams.set(key, "REDACTED");
    }
    return url.toString();
  } catch {
    return value;
  }
}

function printPlan(input: {
  options: CliOptions;
  userAddress: PublicKey;
  delegatedSigner: PublicKey;
  programConfigAddress: PublicKey;
  programConfig: {
    smartAccountIndex: bigint;
    authority: PublicKey;
    smartAccountCreationFeeLamports: bigint;
    treasury: PublicKey;
  };
  seed: bigint;
  treasury: PublicKey;
  settings: PublicKey;
  vault: PublicKey;
  actionAccount: PublicKey;
  routes: Record<string, unknown>;
  existingSettings: boolean;
  createSmartAccountTxSize: number;
  createPolicyTxSize: number;
}): void {
  console.log(
    JSON.stringify(
      {
        dryRun: input.options.dryRun,
        cluster: input.options.cluster,
        rpcUrl: redactedRpcUrl(input.options.rpcUrl),
        userAddress: input.userAddress.toBase58(),
        delegatedSigner: input.delegatedSigner.toBase58(),
        programConfig: {
          address: input.programConfigAddress.toBase58(),
          smartAccountIndex: input.programConfig.smartAccountIndex.toString(),
          authority: input.programConfig.authority.toBase58(),
          smartAccountCreationFeeLamports:
            input.programConfig.smartAccountCreationFeeLamports.toString(),
          treasury: input.programConfig.treasury.toBase58(),
        },
        smartAccount: {
          seed: input.seed.toString(),
          settings: input.settings.toBase58(),
          vaultIndex: input.options.vaultIndex,
          vault: input.vault.toBase58(),
          exists: input.existingSettings,
          treasury: input.treasury.toBase58(),
        },
        policy: {
          actionAccount: input.actionAccount.toBase58(),
          risk: input.options.risk,
          stablecoins: input.options.stablecoins,
          swapLanes: input.options.swapLanes,
          maxFeeBps: input.options.maxFeeBps ?? "sdk-default",
          routes: input.routes,
        },
        transactions: {
          createSmartAccountBytes: input.createSmartAccountTxSize,
          createSmartAccountFitsPacket:
            input.createSmartAccountTxSize <= SOLANA_LEGACY_TRANSACTION_PACKET_BYTES,
          createPolicyBytes: input.createPolicyTxSize,
          createPolicyFitsPacket:
            input.createPolicyTxSize <= SOLANA_LEGACY_TRANSACTION_PACKET_BYTES,
        },
      },
      null,
      2,
    ),
  );
}

main().catch((error: unknown) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
