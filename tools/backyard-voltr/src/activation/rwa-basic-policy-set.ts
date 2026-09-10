import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, Keypair, PublicKey, Transaction, TransactionMessage, TransactionInstruction, VersionedTransaction, type AccountInfo } from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
export const BASIC_POLICY_ARTIFACT = resolve(ROOT, "docs/evidence/backyard-rwa-basic/policy-artifact-v1.json");
export const BASIC_POLICY_SIMULATION = resolve(ROOT, "docs/evidence/backyard-rwa-basic/policy-simulation-v1.json");
export const BASIC_POLICY_JOURNAL = resolve(ROOT, "docs/evidence/backyard-rwa-basic/policy-install-journal-v1.json");
export const BASIC_POLICY_READBACK = resolve(ROOT, "docs/evidence/backyard-rwa-basic/policy-install-readback-v1.json");
export const POLICY_SEED_BEFORE = "140";
export const POLICY_SEEDS = ["141", "142", "143", "144"] as const;
export const PACKET_LIMIT = 1_232;

type JsonObject = Record<string, unknown>;
type SettingsState = { policySeed: { toString(): string } | null };
type PolicyState = {
  settings: PublicKey;
  seed: { toString(): string };
  signers: readonly { key: PublicKey }[];
  policyState: { __kind: string; fields?: readonly unknown[] };
};

export type BasicPolicyInstructionAccount = Readonly<{
  address: string;
  signer: boolean;
  writable: boolean;
}>;

export type BasicPolicyInstruction = Readonly<{
  programId: string;
  accounts: readonly BasicPolicyInstructionAccount[];
  dataBase64: string;
}>;

export type BasicPolicyRow = Readonly<{
  seed: string;
  account: string;
  family: string;
  legacyPacketBytes: number;
  dataSha256: string;
  constraints: readonly unknown[];
  instruction: BasicPolicyInstruction;
}>;

export type BasicPolicyArtifact = Readonly<{
  schema: "loyal-backyard-rwa-basic-policy-artifact/v1";
  settings: string;
  authority: string;
  delegate: string;
  policySeedBefore: "140";
  policies: readonly BasicPolicyRow[];
}>;

export type PolicySimulationRow = Readonly<{
  seed: string;
  account: string;
  family: string;
  packetBytes: number;
  contextSlot: number | null;
  err: unknown;
  logs: readonly string[];
  unitsConsumed: number | null;
}>;

const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(info: AccountInfo<Buffer>): readonly [SettingsState, number] };
}).Settings;
const Policy = (squadsGenerated as unknown as {
  Policy: { fromAccountInfo(info: AccountInfo<Buffer>): readonly [PolicyState, number] };
}).Policy;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function object(value: unknown, label: string): JsonObject {
  invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`);
  return value as JsonObject;
}

function stringField(value: JsonObject, key: string, label: string): string {
  const result = value[key];
  invariant(typeof result === "string" && result.length > 0, `${label}.${key} is not a non-empty string`);
  return result;
}

function booleanField(value: JsonObject, key: string, label: string): boolean {
  const result = value[key];
  invariant(typeof result === "boolean", `${label}.${key} is not a boolean`);
  return result;
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function atomic(path: string, value: unknown): void {
  const next = `${path}.next`;
  writeFileSync(next, `${JSON.stringify(value, null, 2)}\n`, { flag: "w", mode: 0o600 });
  chmodSync(next, 0o600);
  renameSync(next, path);
}

export function derivePolicyAddress(settings: string | PublicKey, seed: string | bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(typeof seed === "bigint" ? seed : BigInt(seed));
  return PublicKey.findProgramAddressSync(
    [Buffer.from("smart_account"), Buffer.from("policy"), new PublicKey(settings).toBuffer(), seedBytes],
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.program),
  )[0].toBase58();
}

function decodeData(encoded: string, label: string): Buffer {
  const data = Buffer.from(encoded, "base64");
  invariant(data.toString("base64") === encoded, `${label} is not canonical base64`);
  return data;
}

export function parseArtifact(value: unknown): BasicPolicyArtifact {
  const root = object(value, "basic policy artifact");
  invariant(root.schema === "loyal-backyard-rwa-basic-policy-artifact/v1", "basic policy artifact schema drifted");
  invariant(root.settings === RWA_MULTIPLY_ROUTE.squads.settings, "basic policy artifact settings drifted");
  invariant(root.authority === RWA_MULTIPLY_ROUTE.setupAdmin, "basic policy artifact authority drifted");
  invariant(root.delegate === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, "basic policy artifact delegate drifted");
  invariant(root.policySeedBefore === POLICY_SEED_BEFORE, "basic policy artifact seed anchor drifted");
  invariant(Array.isArray(root.policies) && root.policies.length === POLICY_SEEDS.length, "basic policy artifact policy count drifted");

  const policies = root.policies.map((raw, index) => {
    const row = object(raw, `basic policy ${index}`);
    const seed = stringField(row, "seed", `basic policy ${index}`);
    invariant(seed === POLICY_SEEDS[index], `basic policy ${index} seed is not consecutive`);
    const account = stringField(row, "account", `basic policy ${index}`);
    invariant(account === derivePolicyAddress(RWA_MULTIPLY_ROUTE.squads.settings, seed), `basic policy ${seed} PDA drifted`);
    const family = stringField(row, "family", `basic policy ${index}`);
    const legacyPacketBytes = row.legacyPacketBytes;
    invariant(typeof legacyPacketBytes === "number" && Number.isSafeInteger(legacyPacketBytes) && legacyPacketBytes > 0 && legacyPacketBytes <= PACKET_LIMIT,
      `basic policy ${seed} packet size is invalid`);
    const dataSha256 = stringField(row, "dataSha256", `basic policy ${index}`);
    invariant(/^[0-9a-f]{64}$/.test(dataSha256), `basic policy ${seed} data hash is invalid`);
    invariant(Array.isArray(row.constraints) && row.constraints.length === 2, `basic policy ${seed} constraint count drifted`);
    const rawInstruction = object(row.instruction, `basic policy ${seed} instruction`);
    const instruction: BasicPolicyInstruction = {
      programId: stringField(rawInstruction, "programId", `basic policy ${seed} instruction`),
      accounts: (() => {
        const accounts = rawInstruction.accounts;
        invariant(Array.isArray(accounts) && accounts.length > 0, `basic policy ${seed} instruction accounts are invalid`);
        return accounts.map((rawAccount, accountIndex) => {
          const accountValue = object(rawAccount, `basic policy ${seed} account ${accountIndex}`);
          return {
            address: stringField(accountValue, "address", `basic policy ${seed} account ${accountIndex}`),
            signer: booleanField(accountValue, "signer", `basic policy ${seed} account ${accountIndex}`),
            writable: booleanField(accountValue, "writable", `basic policy ${seed} account ${accountIndex}`),
          };
        });
      })(),
      dataBase64: stringField(rawInstruction, "dataBase64", `basic policy ${seed} instruction`),
    };
    invariant(instruction.programId === RWA_MULTIPLY_ROUTE.squads.program, `basic policy ${seed} instruction program drifted`);
    const data = decodeData(instruction.dataBase64, `basic policy ${seed} instruction data`);
    invariant(sha256(data) === dataSha256, `basic policy ${seed} instruction data hash drifted`);
    return { seed, account, family, legacyPacketBytes, dataSha256, constraints: row.constraints, instruction };
  });

  return {
    schema: "loyal-backyard-rwa-basic-policy-artifact/v1",
    settings: root.settings as string,
    authority: root.authority as string,
    delegate: root.delegate as string,
    policySeedBefore: "140",
    policies,
  };
}

export function createPolicyInstruction(row: BasicPolicyRow): TransactionInstruction {
  const data = decodeData(row.instruction.dataBase64, `basic policy ${row.seed} instruction data`);
  invariant(sha256(data) === row.dataSha256, `basic policy ${row.seed} instruction data hash drifted`);
  return new TransactionInstruction({
    programId: new PublicKey(row.instruction.programId),
    data,
    keys: row.instruction.accounts.map((account) => ({
      pubkey: new PublicKey(account.address),
      isSigner: account.signer,
      isWritable: account.writable,
    })),
  });
}

export function assertExecuteAuthorization(env: Readonly<Record<string, string | undefined>>): void {
  invariant(env.CONFIRM_MAINNET === "1", "execute mode requires CONFIRM_MAINNET=1");
  invariant(typeof env.SOLANA_TESTING_PK === "string" && env.SOLANA_TESTING_PK.trim().length > 0,
    "execute mode requires SOLANA_TESTING_PK");
}

export function validateInstallJournal(value: unknown): void {
  const journal = object(value, "basic policy install journal");
  invariant(journal.schema === "loyal-backyard-rwa-basic-policy-install-journal/v1", "basic policy install journal schema drifted");
  invariant(journal.broadcast === true && Array.isArray(journal.legs), "basic policy install journal broadcast or legs drifted");
  let priorSeed = 140n;
  for (const [index, rawLeg] of journal.legs.entries()) {
    const leg = object(rawLeg, `basic policy install journal leg ${index}`);
    const seed = stringField(leg, "seed", `basic policy install journal leg ${index}`);
    invariant(BigInt(seed) === priorSeed + 1n && POLICY_SEEDS.includes(seed as typeof POLICY_SEEDS[number]),
      `basic policy install journal leg ${index} is not the next policy seed`);
    invariant(leg.account === derivePolicyAddress(RWA_MULTIPLY_ROUTE.squads.settings, seed), `basic policy install journal leg ${seed} PDA drifted`);
    const state = stringField(leg, "state", `basic policy install journal leg ${index}`);
    invariant(state === "planned" || state === "finalized", `basic policy install journal leg ${seed} state drifted`);
    const wireSha256 = stringField(leg, "wireSha256", `basic policy install journal leg ${index}`);
    invariant(/^[0-9a-f]{64}$/.test(wireSha256), `basic policy install journal leg ${seed} wire hash is invalid`);
    stringField(leg, "blockhash", `basic policy install journal leg ${index}`);
    const lastValidBlockHeight = leg.lastValidBlockHeight;
    invariant(typeof lastValidBlockHeight === "number" && Number.isSafeInteger(lastValidBlockHeight),
      `basic policy install journal leg ${seed} block height is invalid`);
    const packetBytes = leg.packetBytes;
    invariant(typeof packetBytes === "number" && Number.isSafeInteger(packetBytes) && packetBytes > 0 && packetBytes <= PACKET_LIMIT,
      `basic policy install journal leg ${seed} packet size is invalid`);
    stringField(leg, "signature", `basic policy install journal leg ${index}`);
    priorSeed = BigInt(seed);
  }
}

async function finalizedSettings(connection: Connection): Promise<{ seed: string; contextSlot: number }> {
  const response = await connection.getAccountInfoAndContext(new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), "finalized");
  invariant(response.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, "finalized Settings account is absent or has wrong owner");
  const settings = Settings.fromAccountInfo(response.value)[0];
  return { seed: settings.policySeed?.toString() ?? "0", contextSlot: response.context.slot };
}

async function finalizedPolicyPresence(connection: Connection, artifact: BasicPolicyArtifact): Promise<{ contextSlot: number; exists: readonly boolean[] }> {
  const response = await connection.getMultipleAccountsInfoAndContext(
    artifact.policies.map((policy) => new PublicKey(policy.account)),
    { commitment: "finalized" },
  );
  const exists = response.value.map((account) => account !== null);
  invariant(exists.every((present) => !present), "one or more basic policy PDAs already exist at finalized commitment");
  return { contextSlot: response.context.slot, exists };
}

function simulationError(error: unknown): string {
  return error instanceof Error ? error.name : "UnknownError";
}

async function simulatePolicy(
  connection: Connection,
  policy: BasicPolicyRow,
  blockhash: string,
): Promise<PolicySimulationRow> {
  try {
    const transaction = new VersionedTransaction(new TransactionMessage({
      payerKey: new PublicKey(RWA_MULTIPLY_ROUTE.setupAdmin),
      recentBlockhash: blockhash,
      instructions: [createPolicyInstruction(policy)],
    }).compileToV0Message());
    const simulation = await connection.simulateTransaction(transaction, {
      commitment: "finalized",
      sigVerify: false,
      replaceRecentBlockhash: true,
    });
    return {
      seed: policy.seed,
      account: policy.account,
      family: policy.family,
      packetBytes: policy.legacyPacketBytes,
      contextSlot: simulation.context.slot,
      err: simulation.value.err,
      logs: simulation.value.logs ?? [],
      unitsConsumed: simulation.value.unitsConsumed ?? null,
    };
  } catch (error) {
    return {
      seed: policy.seed,
      account: policy.account,
      family: policy.family,
      packetBytes: policy.legacyPacketBytes,
      contextSlot: null,
      err: simulationError(error),
      logs: [],
      unitsConsumed: null,
    };
  }
}

async function simulate(rpc: string, artifact: BasicPolicyArtifact): Promise<JsonObject> {
  const connection = new Connection(rpc, "finalized");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const settings = await finalizedSettings(connection);
  invariant(settings.seed === POLICY_SEED_BEFORE, "finalized Settings policy seed is not exactly 140");
  const presence = await finalizedPolicyPresence(connection, artifact);
  const latest = await connection.getLatestBlockhashAndContext("finalized");
  const policies = await Promise.all(artifact.policies.map((policy) => simulatePolicy(connection, policy, latest.value.blockhash)));
  const failed = policies.filter((policy) => policy.err !== null).length;
  const contextSlots = [settings.contextSlot, presence.contextSlot, latest.context.slot, ...policies.map((policy) => policy.contextSlot ?? 0)];
  return {
    schema: "loyal-backyard-rwa-basic-policy-simulation/v1",
    verdict: failed === 0 ? "PASS" : "FAIL",
    broadcast: false,
    cluster: "mainnet-beta",
    commitment: "finalized",
    genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    settings: { address: artifact.settings, policySeedBefore: settings.seed, contextSlot: settings.contextSlot },
    preflight: {
      contextSlot: presence.contextSlot,
      policies: artifact.policies.map((policy, index) => ({ seed: policy.seed, account: policy.account, exists: presence.exists[index] === true })),
    },
    policies,
    contextSlot: Math.max(...contextSlots),
  };
}

async function finalizedPolicyReadback(connection: Connection, policy: BasicPolicyRow): Promise<{ seed: string; account: string; dataSha256: string; finalizedSlot: number }> {
  const response = await connection.getAccountInfoAndContext(new PublicKey(policy.account), "finalized");
  invariant(response.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, `policy ${policy.seed} readback is absent or has wrong owner`);
  const decoded = Policy.fromAccountInfo(response.value)[0];
  invariant(decoded.settings.toBase58() === RWA_MULTIPLY_ROUTE.squads.settings && decoded.seed.toString() === policy.seed,
    `policy ${policy.seed} readback identity drifted`);
  invariant(decoded.signers.length === 1 && decoded.signers[0]?.key.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, `policy ${policy.seed} delegated signer drifted`);
  invariant(decoded.policyState.__kind === "ProgramInteraction", `policy ${policy.seed} readback is not ProgramInteraction`);
  const body = object(decoded.policyState.fields?.[0], `policy ${policy.seed} readback state`);
  invariant(Array.isArray(body.instructionsConstraints) && body.instructionsConstraints.length === policy.constraints.length,
    `policy ${policy.seed} readback constraint count drifted`);
  return { seed: policy.seed, account: policy.account, dataSha256: sha256(response.value.data), finalizedSlot: response.context.slot };
}

async function execute(rpc: string, artifact: BasicPolicyArtifact): Promise<void> {
  assertExecuteAuthorization(process.env);
  invariant(!existsSync(BASIC_POLICY_READBACK) && !existsSync(BASIC_POLICY_JOURNAL), "basic policy execute output already exists");
  const connection = new Connection(rpc, "finalized");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === RWA_MULTIPLY_ROUTE.setupAdmin, "setup admin signer drifted");
  const adminKeypair = Keypair.fromSecretKey(admin.secretKey);
  const journal: JsonObject = {
    schema: "loyal-backyard-rwa-basic-policy-install-journal/v1",
    broadcast: true,
    artifactSha256: sha256(readFileSync(BASIC_POLICY_ARTIFACT)),
    legs: [],
  };
  validateInstallJournal(journal);
  atomic(BASIC_POLICY_JOURNAL, journal);
  const readbacks: JsonObject[] = [];
  for (const policy of artifact.policies) {
    const settings = await finalizedSettings(connection);
    invariant(settings.seed === String(BigInt(policy.seed) - 1n), `policy ${policy.seed} is not the unique finalized successor`);
    const existing = await connection.getAccountInfo(new PublicKey(policy.account), "finalized");
    invariant(existing === null, `policy ${policy.seed} PDA already exists`);
    const latest = await connection.getLatestBlockhashAndContext("finalized");
    const transaction = new Transaction({ feePayer: adminKeypair.publicKey, recentBlockhash: latest.value.blockhash }).add(createPolicyInstruction(policy));
    transaction.sign(adminKeypair);
    const wire = transaction.serialize();
    invariant(wire.length <= PACKET_LIMIT, `policy ${policy.seed} packet exceeds ${PACKET_LIMIT}`);
    const signatureBytes = transaction.signatures[0]?.signature;
    invariant(signatureBytes, `policy ${policy.seed} signature is absent`);
    const signature = bs58.encode(signatureBytes);
    const leg: JsonObject = {
      seed: policy.seed,
      account: policy.account,
      family: policy.family,
      state: "planned",
      wireSha256: sha256(wire),
      blockhash: latest.value.blockhash,
      lastValidBlockHeight: latest.value.lastValidBlockHeight,
      packetBytes: wire.length,
      signature,
    };
    (journal.legs as JsonObject[]).push(leg);
    validateInstallJournal(journal);
    atomic(BASIC_POLICY_JOURNAL, journal);
    const returned = await connection.sendRawTransaction(wire, { skipPreflight: false, preflightCommitment: "finalized", maxRetries: 0, minContextSlot: latest.context.slot });
    invariant(returned === signature, `policy ${policy.seed} RPC signature mismatch`);
    const confirmation = await connection.confirmTransaction({ signature, blockhash: latest.value.blockhash, lastValidBlockHeight: latest.value.lastValidBlockHeight }, "finalized");
    invariant(confirmation.value.err === null, `policy ${policy.seed} finalized with an error`);
    const status = (await connection.getSignatureStatuses([signature], { searchTransactionHistory: true })).value[0];
    invariant(status?.err === null && status.confirmationStatus === "finalized", `policy ${policy.seed} did not reach finalized status`);
    const readback = await finalizedPolicyReadback(connection, policy);
    Object.assign(leg, { state: "finalized", finalizedSlot: status.slot, readback });
    validateInstallJournal(journal);
    atomic(BASIC_POLICY_JOURNAL, journal);
    readbacks.push(readback);
  }
  const finalSettings = await finalizedSettings(connection);
  invariant(finalSettings.seed === "144", "finalized Settings policy seed did not reach 144");
  const result = {
    schema: "loyal-backyard-rwa-basic-policy-install-readback/v1",
    verdict: "PASS",
    broadcast: true,
    cluster: "mainnet-beta",
    commitment: "finalized",
    genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    policySeedBefore: POLICY_SEED_BEFORE,
    policySeedAfter: finalSettings.seed,
    seeds: artifact.policies.map((policy) => policy.seed),
    PDAs: artifact.policies.map((policy) => policy.account),
    policies: readbacks,
    finalizedSlot: Math.max(...readbacks.map((row) => Number(row.finalizedSlot)), finalSettings.contextSlot),
  };
  atomic(BASIC_POLICY_READBACK, result);
}

function readArtifact(): BasicPolicyArtifact {
  return parseArtifact(JSON.parse(readFileSync(BASIC_POLICY_ARTIFACT, "utf8")) as unknown);
}

async function main(): Promise<void> {
  const simulateMode = process.argv.includes("--simulate");
  const executeMode = process.argv.includes("--execute");
  invariant(simulateMode !== executeMode, "choose exactly one of --simulate or --execute");
  const rpc = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpc, "SOLANA_RPC_URL is required");
  const artifact = readArtifact();
  if (simulateMode) {
    const result = await simulate(rpc, artifact);
    atomic(BASIC_POLICY_SIMULATION, result);
    console.log(JSON.stringify({ verdict: result.verdict, broadcast: false, contextSlot: result.contextSlot, policies: (result.policies as PolicySimulationRow[]).map((policy) => ({ seed: policy.seed, err: policy.err, unitsConsumed: policy.unitsConsumed })) }));
    return;
  }
  await execute(rpc, artifact);
  console.log(JSON.stringify({ verdict: "PASS", broadcast: true }));
}

const INVOKED = process.argv[1] !== undefined && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url));
if (INVOKED) {
  main().catch(() => {
    console.error("basic policy installer failed");
    process.exitCode = 1;
  });
}
