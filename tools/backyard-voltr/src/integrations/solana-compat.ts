import { createHash } from "node:crypto";
import { DEFAULT_PUBLIC_KEY, Reserve } from "@kamino-finance/klend-sdk";
import {
  AccountRole,
  address,
  isSignerRole,
  isWritableRole,
  type Instruction,
} from "@solana/kit";
import {
  AddressLookupTableAccount,
  Connection,
  Keypair,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
  type AccountInfo,
  type Commitment,
  type VersionedTransactionResponse,
} from "@solana/web3.js";
import bs58 from "bs58";

import type { SigningMaterial } from "./signer.js";
import type {
  PartnerRouteSpec,
  PartnerStrategyCandidate,
} from "../domain/route-spec.js";
import type { ReserveGraph } from "./voltr.js";

const MAINNET_GENESIS_HASH = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
/** One prepared wire is submitted exactly once; recovery is read-only by its expected signature. */
export const MAX_IDENTICAL_SUBMISSION_ATTEMPTS = 1;
const IDENTICAL_SUBMISSION_STATUS_POLLS = 4;
const IDENTICAL_SUBMISSION_STATUS_POLL_MS = 250;
const CONFIRMED_TRANSACTION_READBACK_POLLS = 60;
const CONFIRMED_TRANSACTION_READBACK_POLL_MS = 500;
const BPF_LOADER_UPGRADEABLE_PROGRAM_ID = new PublicKey(
  "BPFLoaderUpgradeab1e11111111111111111111111",
);

function sha256(data: ArrayLike<number>): string {
  return createHash("sha256").update(Uint8Array.from(data)).digest("hex");
}

export type AccountSnapshot = Readonly<{
  address: string;
  owner: string;
  lamports: number;
  executable: boolean;
  data: Uint8Array;
}>;

export type PreparedTransaction = Readonly<{
  cluster: "mainnet-beta";
  genesisHash: string;
  commitment: "confirmed" | "finalized";
  serializedTransaction: Uint8Array;
  serializedMessage: Uint8Array;
  expectedSignature: string;
  latestBlockhash: Readonly<{
    blockhash: string;
    lastValidBlockHeight: number;
  }>;
  prestateSlot: number;
  simulationSlot: number;
  packetBytes: number;
  feeLamports: number;
  simulation: Readonly<{
    err: unknown;
    unitsConsumed: number | null;
    logs: readonly string[];
    postAccounts: readonly (AccountSnapshot | null)[];
  }>;
}>;

export type SubmissionAttemptEvidence = Readonly<{
  attempt: number;
  wireSha256: string;
  expectedSignature: string;
  returnedSignature: string | null;
  error: string | null;
}>;

export class PreparedTransactionSendError extends Error {
  readonly submissionAttemptCount: number;
  readonly submissionWireSha256: string;
  readonly submissionAttempts: readonly SubmissionAttemptEvidence[];
  readonly expectedSignature: string;

  constructor(
    message: string,
    input: Readonly<{
      expectedSignature: string;
      submissionWireSha256: string;
      submissionAttempts: readonly SubmissionAttemptEvidence[];
      cause?: unknown;
    }>,
  ) {
    super(message, input.cause === undefined ? undefined : { cause: input.cause });
    this.name = "PreparedTransactionSendError";
    this.expectedSignature = input.expectedSignature;
    this.submissionWireSha256 = input.submissionWireSha256;
    this.submissionAttempts = input.submissionAttempts;
    this.submissionAttemptCount = input.submissionAttempts.length;
  }
}

export function submissionEvidence(error: unknown, prepared: PreparedTransaction): Readonly<{
  submissionAttemptCount: number;
  submissionWireSha256: string;
  submissionAttempts: readonly SubmissionAttemptEvidence[];
}> {
  if (error instanceof PreparedTransactionSendError) {
    return {
      submissionAttemptCount: error.submissionAttemptCount,
      submissionWireSha256: error.submissionWireSha256,
      submissionAttempts: error.submissionAttempts,
    };
  }
  return {
    submissionAttemptCount: 0,
    submissionWireSha256: sha256(prepared.serializedTransaction),
    submissionAttempts: [],
  };
}

function web3Keypair(material: SigningMaterial): Keypair {
  return material.secretKey.length === 32
    ? Keypair.fromSeed(material.secretKey)
    : Keypair.fromSecretKey(material.secretKey);
}

function snapshot(
  account: string,
  value: {
    owner: string;
    lamports: number;
    executable: boolean;
    data: [string, string];
  } | null,
): AccountSnapshot | null {
  if (!value) return null;
  if (value.data[1] !== "base64") {
    throw new Error(`unsupported RPC account encoding ${value.data[1]}`);
  }
  return {
    address: account,
    owner: value.owner,
    lamports: value.lamports,
    executable: value.executable,
    data: Buffer.from(value.data[0], "base64"),
  };
}

/**
 * The only conversion boundary between kit-native domain/builders and legacy
 * web3.js transaction/RPC APIs required by the pinned external SDK stack.
 */
export function toWeb3Instruction(instruction: Instruction): TransactionInstruction {
  return new TransactionInstruction({
    programId: new PublicKey(instruction.programAddress),
    keys: (instruction.accounts ?? []).map((meta) => ({
      pubkey: new PublicKey(meta.address),
      isSigner: isSignerRole(meta.role),
      isWritable: isWritableRole(meta.role),
    })),
    data: Buffer.from(instruction.data ?? []),
  });
}

export function fromWeb3Instruction(instruction: TransactionInstruction): Instruction {
  return {
    programAddress: address(instruction.programId.toBase58()),
    accounts: instruction.keys.map((meta) => ({
      address: address(meta.pubkey.toBase58()),
      role: meta.isSigner
        ? meta.isWritable ? AccountRole.WRITABLE_SIGNER : AccountRole.READONLY_SIGNER
        : meta.isWritable ? AccountRole.WRITABLE : AccountRole.READONLY,
    })),
    data: new Uint8Array(instruction.data),
  };
}

export function publicKey(value: string): PublicKey {
  return new PublicKey(value);
}

function pda(program: string, seeds: readonly Uint8Array[]): PublicKey {
  return PublicKey.findProgramAddressSync(
    seeds.map((value) => Buffer.from(value)),
    new PublicKey(program),
  )[0];
}

export type ReserveGraphObservation = Readonly<{
  candidate: PartnerStrategyCandidate;
  graph: ReserveGraph;
  contextSlot: number;
  reserveDataSha256: string;
  reserveStatus: number;
  liquidityMint: string;
  liquidityTokenProgram: string;
  liquidityMintDecimals: number;
  reserveLastUpdateSlot: bigint;
  reserveLastUpdateStale: number;
  reservePriceStatus: number;
  hasCollateralFarm: boolean;
}>;

export type ReserveGraphLoadRow = Readonly<{
  candidate: PartnerStrategyCandidate;
  observation: ReserveGraphObservation | null;
  error: string | null;
}>;

function decodeReserveGraphObservation(input: Readonly<{
  route: PartnerRouteSpec;
  candidate: PartnerStrategyCandidate;
  vaultStrategyAuth: string;
  contextSlot: number;
  account: AccountInfo<Buffer>;
}>): ReserveGraphObservation {
  const { route, candidate, contextSlot } = input;
  const reserveKey = new PublicKey(candidate.reserve);
  if (!input.account.owner.equals(new PublicKey(route.programs.klend))) {
    throw new Error(`Kamino reserve owner ${input.account.owner} is not approved KLend`);
  }
  const reserve = Reserve.decode(input.account.data);
  const lendingMarket = new PublicKey(reserve.lendingMarket);
  const farm = new PublicKey(reserve.farmCollateral);
  const hasCollateralFarm = !farm.equals(new PublicKey(DEFAULT_PUBLIC_KEY));
  if (!hasCollateralFarm) {
    throw new Error(
      `Kamino reserve ${candidate.reserve} has no collateral farm; the maintained Voltr graph must not fabricate farm accounts`,
    );
  }
  const strategyAuth = new PublicKey(input.vaultStrategyAuth);
  const system = new PublicKey(route.programs.system);
  const obligation = pda(route.programs.klend, [
    Uint8Array.of(0),
    Uint8Array.of(0),
    strategyAuth.toBytes(),
    lendingMarket.toBytes(),
    system.toBytes(),
    system.toBytes(),
  ]);
  const lendingMarketAuthority = pda(route.programs.klend, [
    Buffer.from("lma"),
    lendingMarket.toBytes(),
  ]);
  const userMetadata = pda(route.programs.klend, [
    Buffer.from("user_meta"),
    strategyAuth.toBytes(),
  ]);
  const obligationFarm = pda(route.programs.farms, [
    Buffer.from("user"),
    farm.toBytes(),
    obligation.toBytes(),
  ]);
  return {
    candidate,
    contextSlot,
    reserveDataSha256: createHash("sha256").update(input.account.data).digest("hex"),
    reserveStatus: reserve.config.status,
    liquidityMint: new PublicKey(reserve.liquidity.mintPubkey).toBase58(),
    liquidityTokenProgram: new PublicKey(reserve.liquidity.tokenProgram).toBase58(),
    liquidityMintDecimals: reserve.liquidity.mintDecimals.toNumber(),
    reserveLastUpdateSlot: BigInt(reserve.lastUpdate.slot.toString()),
    reserveLastUpdateStale: reserve.lastUpdate.stale,
    reservePriceStatus: reserve.lastUpdate.priceStatus,
    hasCollateralFarm,
    graph: {
      reserve: address(reserveKey.toBase58()),
      lendingMarket: address(lendingMarket.toBase58()),
      lendingMarketAuthority: address(lendingMarketAuthority.toBase58()),
      obligation: address(obligation.toBase58()),
      userMetadata: address(userMetadata.toBase58()),
      reserveLiquiditySupply: address(new PublicKey(reserve.liquidity.supplyVault).toBase58()),
      reserveCollateralMint: address(new PublicKey(reserve.collateral.mintPubkey).toBase58()),
      reserveCollateralSupplyVault: address(new PublicKey(reserve.collateral.supplyVault).toBase58()),
      scope: address(new PublicKey(reserve.config.tokenInfo.scopeConfiguration.priceFeed).toBase58()),
      reserveFarmState: address(farm.toBase58()),
      obligationFarm: address(obligationFarm.toBase58()),
    },
  };
}

/**
 * Read all candidate reserves from one bank context. Row-level failures are
 * retained so a compatibility matrix reports the first real incompatibility
 * instead of aborting before the remaining exact reserves are inspected.
 */
export async function loadReserveGraphs(
  rpcUrl: string,
  route: PartnerRouteSpec,
  requests: readonly Readonly<{
    candidate: PartnerStrategyCandidate;
    vaultStrategyAuth: string;
  }>[],
  commitment: Commitment,
  minimumContextSlot?: number,
): Promise<Readonly<{
  contextSlot: number;
  rows: readonly ReserveGraphLoadRow[];
}>> {
  const connection = new Connection(rpcUrl, commitment);
  const response = await connection.getMultipleAccountsInfoAndContext(
    requests.map(({ candidate }) => new PublicKey(candidate.reserve)),
    {
      commitment,
      ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }),
    },
  );
  if (response.value.length !== requests.length) {
    throw new Error(
      `reserve batch returned ${response.value.length} rows for ${requests.length} requests`,
    );
  }
  if (minimumContextSlot !== undefined && response.context.slot < minimumContextSlot) {
    throw new Error(
      `reserve batch slot ${response.context.slot} predates minimum ${minimumContextSlot}`,
    );
  }
  const rows = requests.map(({ candidate, vaultStrategyAuth }, index): ReserveGraphLoadRow => {
    const account = response.value[index] ?? null;
    if (!account) {
      return {
        candidate,
        observation: null,
        error: `Kamino reserve ${candidate.reserve} is absent`,
      };
    }
    try {
      return {
        candidate,
        observation: decodeReserveGraphObservation({
          route,
          candidate,
          vaultStrategyAuth,
          contextSlot: response.context.slot,
          account,
        }),
        error: null,
      };
    } catch (error) {
      return {
        candidate,
        observation: null,
        error: error instanceof Error ? error.message : String(error),
      };
    }
  });
  return { contextSlot: response.context.slot, rows };
}

export async function loadMainReserveGraph(
  rpcUrl: string,
  route: PartnerRouteSpec,
  vaultStrategyAuth: string,
  commitment: "confirmed" | "finalized" = "finalized",
  minimumContextSlot?: number,
): Promise<Readonly<{
  graph: ReserveGraph;
  contextSlot: number;
  reserveDataSha256: string;
}>> {
  const result = await loadReserveGraphs(
    rpcUrl,
    route,
    [{
      candidate: { id: "main", reserve: route.strategy.reserve },
      vaultStrategyAuth,
    }],
    commitment,
    minimumContextSlot,
  );
  const row = result.rows[0];
  if (!row?.observation) {
    throw new Error(row?.error ?? "Main reserve graph did not decode");
  }
  const observed = row.observation;
  if (observed.reserveStatus !== 0) {
    throw new Error(`Main reserve status ${observed.reserveStatus} is not Active`);
  }
  if (observed.liquidityMint !== route.asset.mint) {
    throw new Error(`Main reserve mint ${observed.liquidityMint} is not RouteSpec USDC`);
  }
  if (observed.liquidityTokenProgram !== route.asset.tokenProgram) {
    throw new Error(`Main reserve token program ${observed.liquidityTokenProgram} is not RouteSpec Token`);
  }
  if (observed.liquidityMintDecimals !== route.asset.decimals) {
    throw new Error(`Main reserve decimals ${observed.liquidityMintDecimals} do not match RouteSpec`);
  }
  if (observed.graph.lendingMarket !== route.strategy.lendingMarket) {
    throw new Error(`reserve market ${observed.graph.lendingMarket} does not match RouteSpec`);
  }
  if (observed.graph.reserveFarmState !== route.strategy.collateralFarm) {
    throw new Error(`reserve farm ${observed.graph.reserveFarmState} does not match RouteSpec`);
  }
  return {
    graph: observed.graph,
    contextSlot: observed.contextSlot,
    reserveDataSha256: observed.reserveDataSha256,
  };
}

async function snapshotsAtCommitment(
  rpcUrl: string,
  addresses: readonly string[],
  commitment: "confirmed" | "finalized",
  minimumContextSlot?: number,
): Promise<Readonly<{
  contextSlot: number;
  accounts: readonly (AccountSnapshot | null)[];
}>> {
  const connection = new Connection(rpcUrl, commitment);
  const response = await connection.getMultipleAccountsInfoAndContext(
    addresses.map((value) => new PublicKey(value)),
    {
      commitment,
      ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }),
    },
  );
  if (minimumContextSlot !== undefined && response.context.slot < minimumContextSlot) {
    throw new Error(
      `${commitment} snapshot slot ${response.context.slot} predates minimum ${minimumContextSlot}`,
    );
  }
  return {
    contextSlot: response.context.slot,
    accounts: response.value.map((value, index) => value
      ? {
          address: addresses[index]!,
          owner: value.owner.toBase58(),
          lamports: value.lamports,
          executable: value.executable,
          data: new Uint8Array(value.data),
        }
      : null),
  };
}

export async function finalizedSnapshots(
  rpcUrl: string,
  addresses: readonly string[],
  minimumContextSlot?: number,
) {
  return snapshotsAtCommitment(rpcUrl, addresses, "finalized", minimumContextSlot);
}

export async function confirmedSnapshots(
  rpcUrl: string,
  addresses: readonly string[],
  minimumContextSlot?: number,
) {
  return snapshotsAtCommitment(rpcUrl, addresses, "confirmed", minimumContextSlot);
}

/** Read a confirmed bank timestamp for an already observed slot. */
export async function confirmedBlockTime(
  rpcUrl: string,
  slot: number,
): Promise<number> {
  const connection = new Connection(rpcUrl, "confirmed");
  const value = await connection.getBlockTime(slot);
  if (value === null) {
    throw new Error(`confirmed slot ${slot} has no block time`);
  }
  return value;
}

/** Read the finalized bank timestamp for an already observed slot. */
export async function finalizedBlockTime(
  rpcUrl: string,
  slot: number,
): Promise<number> {
  const connection = new Connection(rpcUrl, "finalized");
  const value = await connection.getBlockTime(slot);
  if (value === null) {
    throw new Error(`finalized slot ${slot} has no block time`);
  }
  return value;
}

/** Return the cluster's current rent-exempt balance for an account layout. */
export async function rentExemptionLamports(
  rpcUrl: string,
  dataLength: number,
): Promise<number> {
  if (!Number.isInteger(dataLength) || dataLength < 0) {
    throw new Error(`rent exemption data length must be a non-negative integer: ${dataLength}`);
  }
  return new Connection(rpcUrl, "finalized").getMinimumBalanceForRentExemption(dataLength);
}

/** Load a previously finalized, successful transaction without reducing it to logs. */
export async function finalizedTransaction(
  rpcUrl: string,
  signature: string,
): Promise<VersionedTransactionResponse> {
  const connection = new Connection(rpcUrl, "finalized");
  const transaction = await connection.getTransaction(signature, {
    commitment: "finalized",
    maxSupportedTransactionVersion: 0,
  });
  if (!transaction) throw new Error(`finalized transaction ${signature} is not readable`);
  if (!transaction.meta || (transaction.meta.err !== null && transaction.meta.err !== undefined)) {
    throw new Error(`transaction ${signature} finalized with error`);
  }
  return transaction;
}

/** Load a confirmed, successful transaction without reducing it to logs. */
export async function confirmedTransaction(
  rpcUrl: string,
  signature: string,
  minimumContextSlot?: number,
): Promise<VersionedTransactionResponse> {
  const connection = new Connection(rpcUrl, "confirmed");
  const transaction = await connection.getTransaction(signature, {
    commitment: "confirmed",
    maxSupportedTransactionVersion: 0,
    ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }),
  });
  if (!transaction) throw new Error(`confirmed transaction ${signature} is not readable`);
  if (minimumContextSlot !== undefined && transaction.slot < minimumContextSlot) {
    throw new Error(`confirmed transaction ${signature} slot ${transaction.slot} predates minimum ${minimumContextSlot}`);
  }
  if (!transaction.meta || (transaction.meta.err !== null && transaction.meta.err !== undefined)) {
    throw new Error(`transaction ${signature} confirmed with error`);
  }
  return transaction;
}

/** Load the finalized logs for a previously finalized runtime transaction. */
export async function finalizedTransactionLogs(
  rpcUrl: string,
  signature: string,
): Promise<Readonly<{ slot: number; blockTime: number | null; logs: readonly string[] }>> {
  const transaction = await finalizedTransaction(rpcUrl, signature);
  return {
    slot: transaction.slot,
    blockTime: transaction.blockTime ?? null,
    logs: transaction.meta?.logMessages ?? [],
  };
}

export async function loadDeploymentIdentities(
  rpcUrl: string,
  route: PartnerRouteSpec,
  minimumContextSlot?: number,
  commitment: Commitment = "finalized",
): Promise<Readonly<{
  contextSlot: number;
  identities: readonly Readonly<{
    programId: string;
    programDataAddress: string | null;
    deployedSlot: bigint | null;
    executableSha256: string | null;
  }>[];
}>> {
  const connection = new Connection(rpcUrl, commitment);
  let contextSlot = minimumContextSlot ?? 0;
  const identities = [];
  for (const expected of route.deployments) {
    const programResponse = await connection.getAccountInfoAndContext(new PublicKey(expected.programId), {
      commitment,
      ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }),
    });
    contextSlot = Math.max(contextSlot, programResponse.context.slot);
    const program = programResponse.value;
    if (!program || !program.executable || !program.owner.equals(BPF_LOADER_UPGRADEABLE_PROGRAM_ID) || program.data.length < 36 || program.data.readUInt32LE(0) !== 2) {
      identities.push({ programId: expected.programId, programDataAddress: null, deployedSlot: null, executableSha256: null });
      continue;
    }
    const programDataAddress = new PublicKey(program.data.subarray(4, 36));
    const dataResponse = await connection.getAccountInfoAndContext(programDataAddress, {
      commitment,
      ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }),
    });
    contextSlot = Math.max(contextSlot, dataResponse.context.slot);
    const data = dataResponse.value?.data;
    if (!data || data.length < 13 || data.readUInt32LE(0) !== 3) {
      identities.push({ programId: expected.programId, programDataAddress: programDataAddress.toBase58(), deployedSlot: null, executableSha256: null });
      continue;
    }
    const headerLength = data[12] === 1 ? 45 : 13;
    identities.push({
      programId: expected.programId,
      programDataAddress: programDataAddress.toBase58(),
      deployedSlot: data.readBigUInt64LE(4),
      executableSha256: createHash("sha256").update(data.subarray(headerLength)).digest("hex"),
    });
  }
  return { contextSlot, identities };
}

export async function prepareSignedV0Transaction(input: Readonly<{
  rpcUrl: string;
  feePayer: SigningMaterial;
  additionalSigners?: readonly SigningMaterial[];
  instructions: readonly Instruction[];
  lookupTableAddresses?: readonly string[];
  /** Full protected prestate set; simulation post-account return is capped at 31. */
  prestateAddresses?: readonly string[];
  inspectedAddresses: readonly string[];
  minimumContextSlot?: number;
  commitment?: "confirmed" | "finalized";
}>): Promise<PreparedTransaction> {
  const commitment = input.commitment ?? "finalized";
  const connection = new Connection(input.rpcUrl, commitment);
  const genesisHash = await connection.getGenesisHash();
  if (genesisHash !== MAINNET_GENESIS_HASH) {
    throw new Error(`refusing cluster genesis ${genesisHash}; expected Solana mainnet-beta`);
  }
  const feePayer = web3Keypair(input.feePayer);
  if (feePayer.publicKey.toBase58() !== input.feePayer.signer.address) {
    throw new Error("fee-payer kit/web3 signer mismatch");
  }
  const additional = (input.additionalSigners ?? []).map((material) => {
    const signer = web3Keypair(material);
    if (signer.publicKey.toBase58() !== material.signer.address) {
      throw new Error("additional kit/web3 signer mismatch");
    }
    return signer;
  });
  const uniqueSigners = [feePayer, ...additional].filter(
    (candidate, index, signers) => signers.findIndex((other) => other.publicKey.equals(candidate.publicKey)) === index,
  );
  const latestBlockhash = await connection.getLatestBlockhash(commitment);
  const lookupTables: AddressLookupTableAccount[] = [];
  for (const tableAddress of input.lookupTableAddresses ?? []) {
    const response = await connection.getAddressLookupTable(new PublicKey(tableAddress), {
      commitment,
    });
    if (!response.value) throw new Error(`lookup table ${tableAddress} is absent`);
    lookupTables.push(response.value);
  }
  const message = new TransactionMessage({
    payerKey: feePayer.publicKey,
    recentBlockhash: latestBlockhash.blockhash,
    instructions: input.instructions.map(toWeb3Instruction),
  }).compileToV0Message(lookupTables);
  const transaction = new VersionedTransaction(message);
  transaction.sign(uniqueSigners);
  const serializedTransaction = transaction.serialize();
  if (serializedTransaction.length > 1_232) {
    throw new Error(`transaction packet is ${serializedTransaction.length} bytes; Solana limit is 1232`);
  }
  const prestateAddresses = input.prestateAddresses ?? input.inspectedAddresses;
  const prestate = await connection.getMultipleAccountsInfoAndContext(
    prestateAddresses.map((value) => new PublicKey(value)),
    {
      commitment,
      ...(input.minimumContextSlot === undefined ? {} : { minContextSlot: input.minimumContextSlot }),
    },
  );
  const fee = await connection.getFeeForMessage(message, commitment);
  let simulation: Awaited<ReturnType<Connection["simulateTransaction"]>> | null = null;
  let lastSimulationError: unknown = null;
  for (let attempt = 0; attempt < 8; attempt += 1) {
    try {
      simulation = await connection.simulateTransaction(transaction, {
        commitment,
        sigVerify: true,
        minContextSlot: prestate.context.slot,
        accounts: {
          encoding: "base64",
          addresses: [...input.inspectedAddresses],
        },
      });
      break;
    } catch (error) {
      lastSimulationError = error;
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("Minimum context slot has not been reached") || attempt === 7) {
        throw error;
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
  if (!simulation) {
    throw new Error(`${commitment} simulation bank did not reach the prestate slot`, {
      cause: lastSimulationError,
    });
  }
  if (fee.value === null) throw new Error("RPC did not quote a transaction fee");
  if (simulation.context.slot < prestate.context.slot) {
    throw new Error(`simulation context predates the ${commitment} prestate`);
  }
  if ((simulation.value.accounts?.length ?? -1) !== input.inspectedAddresses.length) {
    throw new Error("simulation omitted an inspected post-account image");
  }
  return {
    cluster: "mainnet-beta",
    genesisHash,
    commitment,
    serializedTransaction,
    serializedMessage: message.serialize(),
    expectedSignature: bs58.encode(transaction.signatures[0]!),
    latestBlockhash,
    prestateSlot: prestate.context.slot,
    simulationSlot: simulation.context.slot,
    packetBytes: serializedTransaction.length,
    feeLamports: fee.value,
    simulation: {
      err: simulation.value.err,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
      logs: simulation.value.logs ?? [],
      postAccounts: input.inspectedAddresses.map((account, index) =>
        snapshot(account, simulation.value.accounts?.[index] as Parameters<typeof snapshot>[1] ?? null)),
    },
  };
}

type SettledPreparedTransaction = Readonly<{
  signature: string;
  confirmationSlot: number;
  settledSlot: number;
  settlementCommitment: "confirmed" | "finalized";
  authorizedContextSlot: number;
  submissionAttemptCount: number;
  submissionWireSha256: string;
  submissionAttempts: readonly SubmissionAttemptEvidence[];
  err: unknown;
  feeLamports: number | null;
  logs: readonly string[];
  /** Token deltas resolved against this transaction's static keys and ALT addresses. */
  tokenDeltas: readonly Readonly<{ address: string; mint: string; deltaRaw: string }>[];
  /** Lamport deltas resolved against this transaction's complete account-key list. */
  lamportDeltas: readonly Readonly<{ address: string; deltaRaw: string }>[];
}>;

async function sendPreparedAtCommitment(
  rpcUrl: string,
  prepared: PreparedTransaction,
  authorizedContextSlot: number,
  commitment: "confirmed" | "finalized",
): Promise<SettledPreparedTransaction> {
  const submissionWireSha256 = sha256(prepared.serializedTransaction);
  const submissionAttempts: SubmissionAttemptEvidence[] = [];
  try {
    if (process.env.CONFIRM_MAINNET !== "1") {
      throw new Error("raw transaction send requires CONFIRM_MAINNET=1");
    }
    if (prepared.commitment !== commitment) {
      throw new Error(
        `prepared transaction commitment ${prepared.commitment} does not match settlement ${commitment}`,
      );
    }
    if (!Number.isSafeInteger(authorizedContextSlot) || authorizedContextSlot < prepared.simulationSlot) {
      throw new Error(`send authorization slot ${authorizedContextSlot} must be at or after simulation slot ${prepared.simulationSlot}`);
    }
    const connection = new Connection(rpcUrl, commitment);
    const genesisHash = await connection.getGenesisHash();
    if (genesisHash !== MAINNET_GENESIS_HASH || prepared.genesisHash !== MAINNET_GENESIS_HASH) {
      throw new Error(`refusing send on genesis ${genesisHash}; prepared for ${prepared.genesisHash}`);
    }
    const blockHeight = await connection.getBlockHeight(commitment);
    if (blockHeight > prepared.latestBlockhash.lastValidBlockHeight) {
      throw new Error("prepared transaction blockhash expired before send");
    }
    let rpcSlot = 0;
    for (let attempt = 0; attempt < 8; attempt += 1) {
      rpcSlot = await connection.getSlot(commitment);
      if (rpcSlot >= authorizedContextSlot) break;
      if (attempt === 7) {
        throw new Error(
          `${commitment} RPC slot ${rpcSlot} did not reach authorization slot ${authorizedContextSlot}`,
        );
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }

    let returned = prepared.expectedSignature;
    let confirmation: Awaited<ReturnType<Connection["confirmTransaction"]>> | null = null;
    for (let attempt = 1; attempt <= MAX_IDENTICAL_SUBMISSION_ATTEMPTS; attempt += 1) {
      const currentHeight = await connection.getBlockHeight(commitment);
      if (currentHeight > prepared.latestBlockhash.lastValidBlockHeight) {
        throw new Error("prepared transaction blockhash expired before byte-identical recovery submission");
      }
      let returnedSignature: string | null = null;
      let submissionError: string | null = null;
      try {
        // Every call submits the same immutable signed bytes. RPC-level retries
        // remain disabled; this loop is the bounded provider recovery fence.
        returnedSignature = await connection.sendRawTransaction(prepared.serializedTransaction, {
          skipPreflight: false,
          preflightCommitment: commitment,
          maxRetries: 0,
          minContextSlot: authorizedContextSlot,
        });
        if (returnedSignature !== prepared.expectedSignature) {
          throw new Error(`RPC returned signature ${returnedSignature}; expected ${prepared.expectedSignature}`);
        }
        returned = returnedSignature;
      } catch (error) {
        submissionError = error instanceof Error ? error.message : String(error);
        if (returnedSignature !== null && returnedSignature !== prepared.expectedSignature) throw error;
      }
      submissionAttempts.push({
        attempt,
        wireSha256: submissionWireSha256,
        expectedSignature: prepared.expectedSignature,
        returnedSignature,
        error: submissionError,
      });

      for (let poll = 0; poll < IDENTICAL_SUBMISSION_STATUS_POLLS; poll += 1) {
        const statusResponse = await connection.getSignatureStatuses([prepared.expectedSignature]);
        const status = statusResponse.value[0];
        if (status?.confirmationStatus === "confirmed" || status?.confirmationStatus === "finalized") {
          if (statusResponse.context.slot < authorizedContextSlot) {
            throw new Error(`${commitment} confirmation slot ${statusResponse.context.slot} predates authorization ${authorizedContextSlot}`);
          }
          confirmation = await connection.confirmTransaction(
            { signature: prepared.expectedSignature, ...prepared.latestBlockhash },
            commitment,
          );
          break;
        }
        const observedHeight = await connection.getBlockHeight(commitment);
        if (observedHeight > prepared.latestBlockhash.lastValidBlockHeight) break;
        if (poll < IDENTICAL_SUBMISSION_STATUS_POLLS - 1) await new Promise((resolve) => setTimeout(resolve, IDENTICAL_SUBMISSION_STATUS_POLL_MS));
      }
      if (confirmation !== null) break;
    }
    if (confirmation === null) {
      confirmation = await connection.confirmTransaction(
        { signature: returned, ...prepared.latestBlockhash },
        commitment,
      );
    }
    if (confirmation.context.slot < authorizedContextSlot) {
      throw new Error(`${commitment} confirmation slot ${confirmation.context.slot} predates authorization ${authorizedContextSlot}`);
    }
    let transaction: VersionedTransactionResponse | null = null;
    for (let attempt = 0; attempt < CONFIRMED_TRANSACTION_READBACK_POLLS; attempt += 1) {
      transaction = await connection.getTransaction(returned, {
        commitment,
        maxSupportedTransactionVersion: 0,
      });
      if (transaction) break;
      if (attempt < CONFIRMED_TRANSACTION_READBACK_POLLS - 1) {
        await new Promise((resolve) => setTimeout(resolve, CONFIRMED_TRANSACTION_READBACK_POLL_MS));
      }
    }
    if (!transaction) throw new Error(`${commitment} transaction ${returned} is not readable`);
    if (!transaction.meta) throw new Error(`${commitment} transaction ${returned} has no metadata`);
    const observedSignature = transaction.transaction.signatures[0];
    if (observedSignature !== returned) {
      throw new Error(
        `${commitment} transaction signature ${observedSignature ?? "absent"} does not match ${returned}`,
      );
    }
    if (
      !Buffer.from(transaction.transaction.message.serialize())
        .equals(Buffer.from(prepared.serializedMessage))
    ) {
      throw new Error(`${commitment} transaction message differs from signed prepared message`);
    }
    if (transaction.slot < authorizedContextSlot) {
      throw new Error(
        `${commitment} transaction slot ${transaction.slot} predates authorization ${authorizedContextSlot}`,
      );
    }
    const meta = transaction.meta;
    const keys = [
      ...transaction.transaction.message.staticAccountKeys,
      ...(meta.loadedAddresses?.writable ?? []),
      ...(meta.loadedAddresses?.readonly ?? []),
    ].map((key) => key.toBase58());
    const deltas = new Map<string, { address: string; mint: string; delta: bigint }>();
    const addBalance = (row: { accountIndex: number; mint: string; uiTokenAmount: { amount: string } }, sign: 1n | -1n) => {
      const addressValue = keys[row.accountIndex];
      if (!addressValue) return;
      const key = `${addressValue}\0${row.mint}`;
      const current = deltas.get(key) ?? { address: addressValue, mint: row.mint, delta: 0n };
      current.delta += BigInt(row.uiTokenAmount.amount) * sign;
      deltas.set(key, current);
    };
    for (const row of meta.preTokenBalances ?? []) addBalance(row, -1n);
    for (const row of meta.postTokenBalances ?? []) addBalance(row, 1n);
    const lamportDeltas = keys.map((addressValue, index) => ({
      address: addressValue,
      deltaRaw: (BigInt(meta.postBalances[index] ?? 0) - BigInt(meta.preBalances[index] ?? 0)).toString(),
    }));
    return {
      signature: returned,
      confirmationSlot: confirmation.context.slot,
      settledSlot: transaction.slot,
      settlementCommitment: commitment,
      authorizedContextSlot,
      submissionAttemptCount: submissionAttempts.length,
      submissionWireSha256,
      submissionAttempts,
      err: meta.err ?? confirmation.value.err,
      feeLamports: meta.fee,
      logs: meta.logMessages ?? [],
      tokenDeltas: [...deltas.values()].map(({ address: addressValue, mint, delta }) => ({ address: addressValue, mint, deltaRaw: delta.toString() })),
      lamportDeltas,
    };
  } catch (error) {
    if (error instanceof PreparedTransactionSendError) throw error;
    throw new PreparedTransactionSendError(
      error instanceof Error ? error.message : String(error),
      {
        expectedSignature: prepared.expectedSignature,
        submissionWireSha256,
        submissionAttempts,
        cause: error,
      },
    );
  }
}

export async function sendPreparedOnce(
  rpcUrl: string,
  prepared: PreparedTransaction,
  authorizedContextSlot: number,
) {
  const settled = await sendPreparedAtCommitment(
    rpcUrl,
    prepared,
    authorizedContextSlot,
    "finalized",
  );
  return { ...settled, finalizedSlot: settled.settledSlot } as const;
}

export async function sendPreparedConfirmedOnce(
  rpcUrl: string,
  prepared: PreparedTransaction,
  authorizedContextSlot: number,
) {
  const settled = await sendPreparedAtCommitment(
    rpcUrl,
    prepared,
    authorizedContextSlot,
    "confirmed",
  );
  return { ...settled, confirmedSlot: settled.settledSlot } as const;
}
