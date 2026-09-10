import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
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
export const BASIC_POLICY_INSTALL_LOCK = resolve(ROOT, "docs/evidence/backyard-rwa-basic/policy-install.lock");
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

function validateReconciliations(value: unknown): void {
  invariant(Array.isArray(value), "basic policy install journal reconciliations drifted");
  for (const [index, rawEntry] of value.entries()) {
    const label = `basic policy install journal reconciliation ${index}`;
    const entry = object(rawEntry, label);
    stringField(entry, "at", label);
    const finalizedSlot = entry.finalizedSlot;
    invariant(typeof finalizedSlot === "number" && Number.isSafeInteger(finalizedSlot), `${label} finalized slot is invalid`);
    stringField(entry, "livePolicySeed", label);
    invariant(Array.isArray(entry.verdicts), `${label} verdicts drifted`);
    for (const [verdictIndex, rawVerdict] of entry.verdicts.entries()) {
      const verdictLabel = `${label} verdict ${verdictIndex}`;
      const verdict = object(rawVerdict, verdictLabel);
      stringField(verdict, "seed", verdictLabel);
      stringField(verdict, "verdict", verdictLabel);
    }
  }
}

export function validateInstallJournal(value: unknown, artifact?: BasicPolicyArtifact): void {
  const journal = object(value, "basic policy install journal");
  invariant(journal.schema === "loyal-backyard-rwa-basic-policy-install-journal/v1", "basic policy install journal schema drifted");
  invariant(journal.broadcast === true && Array.isArray(journal.legs), "basic policy install journal broadcast or legs drifted");
  if (journal.settings !== undefined) invariant(journal.settings === RWA_MULTIPLY_ROUTE.squads.settings, "basic policy install journal settings drifted");
  if (journal.authority !== undefined) invariant(journal.authority === RWA_MULTIPLY_ROUTE.setupAdmin, "basic policy install journal authority drifted");
  if (journal.reconciliations !== undefined) validateReconciliations(journal.reconciliations);
  let priorSeed = 140n;
  let priorState: string | null = null;
  for (const [index, rawLeg] of journal.legs.entries()) {
    const label = `basic policy install journal leg ${index}`;
    const leg = object(rawLeg, label);
    const seed = stringField(leg, "seed", label);
    invariant(POLICY_SEEDS.includes(seed as typeof POLICY_SEEDS[number]), `${label} is not a basic policy seed`);
    invariant(leg.account === derivePolicyAddress(RWA_MULTIPLY_ROUTE.squads.settings, seed), `basic policy install journal leg ${seed} PDA drifted`);
    if (artifact !== undefined) invariant(leg.family === artifactPolicy(artifact, seed).family, `basic policy install journal leg ${seed} family drifted`);
    const seedValue = BigInt(seed);
    const state = stringField(leg, "state", label);
    invariant(state === "planned" || state === "finalized" || state === "blocked" || state === "abandoned",
      `basic policy install journal leg ${seed} state drifted`);
    const retry = priorState === "abandoned" && seedValue === priorSeed;
    invariant(retry || seedValue === priorSeed + 1n, `${label} is not the next policy seed or an abandoned-seed retry`);
    invariant(!retry || state === "planned" || state === "finalized",
      `basic policy install journal leg ${seed} retry after an abandoned leg must be planned or finalized`);
    const wireSha256 = stringField(leg, "wireSha256", label);
    invariant(/^[0-9a-f]{64}$/.test(wireSha256), `basic policy install journal leg ${seed} wire hash is invalid`);
    stringField(leg, "blockhash", label);
    const lastValidBlockHeight = leg.lastValidBlockHeight;
    invariant(typeof lastValidBlockHeight === "number" && Number.isSafeInteger(lastValidBlockHeight),
      `basic policy install journal leg ${seed} block height is invalid`);
    const packetBytes = leg.packetBytes;
    invariant(typeof packetBytes === "number" && Number.isSafeInteger(packetBytes) && packetBytes > 0 && packetBytes <= PACKET_LIMIT,
      `basic policy install journal leg ${seed} packet size is invalid`);
    const signature = stringField(leg, "signature", label);
    invariant(/^[1-9A-HJ-NP-Za-km-z]{32,128}$/.test(signature), `basic policy install journal leg ${seed} signature is not base58`);
    const preSendSimulation = object(leg.preSendSimulation, `basic policy install journal leg ${seed} pre-send simulation`);
    const simulationSlot = preSendSimulation.contextSlot;
    invariant((typeof simulationSlot === "number" && Number.isSafeInteger(simulationSlot)) || simulationSlot === null,
      `basic policy install journal leg ${seed} simulation slot is invalid`);
    const simulationUnits = preSendSimulation.unitsConsumed;
    invariant((typeof simulationUnits === "number" && Number.isFinite(simulationUnits) && simulationUnits >= 0) || simulationUnits === null,
      `basic policy install journal leg ${seed} simulation units are invalid`);
    if (state === "planned") {
      invariant(preSendSimulation.err === null && preSendSimulation.err !== undefined,
        `basic policy install journal leg ${seed} is planned without a passing pre-send simulation`);
    } else if (state === "blocked") {
      invariant(preSendSimulation.err !== null, `basic policy install journal leg ${seed} is blocked without a pre-send simulation error`);
    } else if (state === "abandoned") {
      stringField(leg, "abandonReason", label);
    } else {
      const readback = object(leg.readback, `basic policy install journal leg ${seed} readback`);
      const finalizedSlot = readback.finalizedSlot;
      invariant(typeof finalizedSlot === "number" && Number.isSafeInteger(finalizedSlot),
        `basic policy install journal leg ${seed} readback slot is invalid`);
    }
    priorSeed = seedValue;
    priorState = state;
  }
}

/**
 * Read-only chain surface used by reconcile. It deliberately exposes no send
 * method: reconcile can only read, journal, and decide.
 */
export type ReconcileConnection = Pick<Connection,
  "getAccountInfo" |
  "getAccountInfoAndContext" |
  "getMultipleAccountsInfoAndContext" |
  "getSignatureStatuses" |
  "isBlockhashValid" |
  "simulateTransaction" |
  "getLatestBlockhashAndContext">;

async function finalizedSettings(connection: ReconcileConnection): Promise<{ seed: string; contextSlot: number }> {
  const response = await connection.getAccountInfoAndContext(new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), "finalized");
  invariant(response.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, "finalized Settings account is absent or has wrong owner");
  const settings = Settings.fromAccountInfo(response.value)[0];
  return { seed: settings.policySeed?.toString() ?? "0", contextSlot: response.context.slot };
}

async function readFinalizedPolicyPresence(connection: ReconcileConnection, artifact: BasicPolicyArtifact): Promise<{ contextSlot: number; present: ReadonlyMap<string, boolean> }> {
  const response = await connection.getMultipleAccountsInfoAndContext(
    artifact.policies.map((policy) => new PublicKey(policy.account)),
    { commitment: "finalized" },
  );
  const present = new Map<string, boolean>();
  artifact.policies.forEach((policy, index) => present.set(policy.seed, response.value[index] !== null));
  return { contextSlot: response.context.slot, present };
}

async function finalizedPolicyPresence(connection: ReconcileConnection, artifact: BasicPolicyArtifact): Promise<{ contextSlot: number; exists: readonly boolean[] }> {
  const presence = await readFinalizedPolicyPresence(connection, artifact);
  invariant([...presence.present.values()].every((exists) => !exists), "one or more basic policy PDAs already exist at finalized commitment");
  return { contextSlot: presence.contextSlot, exists: artifact.policies.map((policy) => presence.present.get(policy.seed) === true) };
}

export type InstallStartMode = "fresh" | "reconcile" | "complete";

/**
 * Operator rule: one process per run; a run only starts fresh when neither
 * output exists, reconciles when the journal alone exists, and never starts
 * once the readback proves the install complete.
 */
export function classifyInstallStart(input: Readonly<{ journalExists: boolean; readbackExists: boolean }>): InstallStartMode {
  if (input.readbackExists) return "complete";
  return input.journalExists ? "reconcile" : "fresh";
}

/**
 * A journal may only resume the exact artifact it was opened for: same bytes
 * (hence same wire data) and, when recorded, the same settings and authority.
 */
export function classifyJournalDrift(input: Readonly<{
  journalArtifactSha256: unknown;
  journalSettings: unknown;
  journalAuthority: unknown;
  artifactSha256: string;
  settings: string;
  authority: string;
}>): "match" | "drift" {
  if (input.journalArtifactSha256 !== input.artifactSha256) return "drift";
  if (input.journalSettings !== undefined && input.journalSettings !== input.settings) return "drift";
  if (input.journalAuthority !== undefined && input.journalAuthority !== input.authority) return "drift";
  return "match";
}

export type ReconcileAction = "verify-finalized" | "promote-finalized" | "abandon-and-retry" | "refuse";

export type UnsentLegEvidence = Readonly<{
  signatureStatus: "landed" | "failed" | "unknown";
  blockhashValid: boolean | null;
  statusSlot: number | null;
}>;

export type ReconcileLegDecision = Readonly<{
  action: ReconcileAction;
  reason: string;
  resimulate: boolean;
}>;

/**
 * Resolve one journal leg against finalized chain state.
 *
 * The live settings policy seed is the seed of the last installed policy, so
 * the seed this chain will accept next is `livePolicySeed + 1`. A leg whose
 * PDA is absent is only retryable once its recorded signature provably failed
 * on chain or its blockhash has expired; anything else means the wire may
 * still land, or the seed moved without this journal, and needs manual review.
 */
export function classifyJournalLeg(input: Readonly<{
  state: string;
  seed: string;
  livePolicySeed: string;
  pdaPresent: boolean;
  evidence?: UnsentLegEvidence;
}>): ReconcileLegDecision {
  const nextInstallSeed = BigInt(input.livePolicySeed) + 1n;
  const legSeed = BigInt(input.seed);
  const refuse = (reason: string): ReconcileLegDecision => ({ action: "refuse", reason, resimulate: false });
  if (input.state === "finalized") {
    if (!input.pdaPresent) return refuse(`basic policy ${input.seed} journal says finalized but the PDA is absent at finalized commitment`);
    if (nextInstallSeed <= legSeed) return refuse(`basic policy ${input.seed} journal says finalized but the live policy seed is ${input.livePolicySeed}`);
    return { action: "verify-finalized", reason: `basic policy ${input.seed} PDA is present and the live policy seed is beyond it`, resimulate: false };
  }
  if (input.state === "planned" || input.state === "blocked") {
    if (input.pdaPresent) {
      if (input.state === "blocked") return refuse(`basic policy ${input.seed} is blocked and was never sent, yet its PDA exists`);
      return { action: "promote-finalized", reason: `basic policy ${input.seed} planned leg PDA is present on chain`, resimulate: false };
    }
    if (nextInstallSeed !== legSeed) {
      if (nextInstallSeed > legSeed) return refuse(`basic policy ${input.seed} PDA is absent but the live policy seed ${input.livePolicySeed} is already beyond it; manual review required`);
      return refuse(`basic policy ${input.seed} PDA is absent and the live policy seed ${input.livePolicySeed} is behind it; the journal is ahead of the chain`);
    }
    const evidence = input.evidence;
    if (evidence === undefined) return refuse(`basic policy ${input.seed} has no signature or blockhash evidence; refusing to force past an unknown leg`);
    if (evidence.signatureStatus === "landed") return refuse(`basic policy ${input.seed} signature landed but its PDA is absent at finalized commitment; manual review required`);
    if (evidence.signatureStatus === "unknown" && evidence.blockhashValid !== false) {
      return refuse(`leg ${input.seed} may still land; re-run reconcile after the blockhash expires`);
    }
    return {
      action: "abandon-and-retry",
      reason: evidence.signatureStatus === "failed"
        ? `basic policy ${input.seed} failed on chain; retrying from this seed`
        : `basic policy ${input.seed} never landed and its blockhash expired; retrying from this seed`,
      resimulate: input.state === "blocked",
    };
  }
  return refuse(`basic policy install journal leg ${input.seed} has unreconcilable state ${input.state}`);
}

function simulationError(error: unknown): string {
  return error instanceof Error ? error.name : "UnknownError";
}

async function simulatePolicy(
  connection: ReconcileConnection,
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

type ConstraintBeet = {
  toFixedFromData(data: Buffer, offset: number): { read(data: Buffer, offset: number): unknown; byteSize: number };
  toFixedFromValue(value: unknown): { write(buffer: Buffer, offset: number, value: unknown): void; byteSize: number };
};

const instructionConstraintBeet = (squadsGenerated as unknown as { instructionConstraintBeet: ConstraintBeet }).instructionConstraintBeet;

/**
 * Byte-exact payload identity. The on-chain constraints are re-encoded with the
 * generated codec and must appear verbatim, exactly once, inside the artifact
 * instruction data. Since parseArtifact pins sha256 of those bytes to
 * `dataSha256`, containment proves the on-chain program ids, account
 * conditions, and data conditions are exactly the ones the artifact hashed.
 */
export function assertPolicyPayloadMatchesArtifact(policy: BasicPolicyRow, constraints: readonly unknown[]): void {
  const expected = decodeData(policy.instruction.dataBase64, `basic policy ${policy.seed} instruction data`);
  invariant(sha256(expected) === policy.dataSha256, `basic policy ${policy.seed} instruction data hash drifted`);
  const chunks: Buffer[] = [];
  let length = 0;
  for (const constraint of constraints) {
    const fixed = instructionConstraintBeet.toFixedFromValue(constraint);
    const chunk = Buffer.alloc(fixed.byteSize);
    fixed.write(chunk, 0, constraint);
    chunks.push(chunk);
    length += chunk.byteLength;
  }
  const encoded = Buffer.concat(chunks, length);
  invariant(encoded.length > 0, `policy ${policy.seed} readback has no constraints to verify`);
  const first = expected.indexOf(encoded);
  invariant(first >= 0 && expected.lastIndexOf(encoded) === first, `policy ${policy.seed} on-chain constraints do not match the artifact payload`);
}

async function finalizedPolicyReadback(connection: ReconcileConnection, artifact: BasicPolicyArtifact, seed: string): Promise<{ seed: string; account: string; dataSha256: string; finalizedSlot: number }> {
  const policy = artifactPolicy(artifact, seed);
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
  assertPolicyPayloadMatchesArtifact(policy, body.instructionsConstraints);
  return { seed: policy.seed, account: policy.account, dataSha256: sha256(response.value.data), finalizedSlot: response.context.slot };
}

type InstallRuntime = Readonly<{
  connection: Connection;
  artifact: BasicPolicyArtifact;
  adminKeypair: Keypair;
  journal: JsonObject;
}>;

function artifactPolicy(artifact: BasicPolicyArtifact, seed: string): BasicPolicyRow {
  const policy = artifact.policies.find((row) => row.seed === seed);
  invariant(policy !== undefined, `basic policy ${seed} is not part of the artifact`);
  return policy;
}

function loadInstallJournal(artifact: BasicPolicyArtifact): JsonObject {
  const journal = object(JSON.parse(readFileSync(BASIC_POLICY_JOURNAL, "utf8")) as unknown, "basic policy install journal");
  validateInstallJournal(journal, artifact);
  return journal;
}

function recordReconciliation(journal: JsonObject, entry: JsonObject): void {
  const reconciliations = Array.isArray(journal.reconciliations) ? [...journal.reconciliations as unknown[]] : [];
  reconciliations.push(entry);
  journal.reconciliations = reconciliations;
}

/**
 * Install one policy seed: gate on the live successor seed, sign a fresh wire,
 * pre-send simulate, journal, send, confirm finalized, and read the policy
 * back before the leg is marked finalized. Sent exactly once per journal leg.
 */
async function installSeedLeg(runtime: InstallRuntime, policy: BasicPolicyRow): Promise<JsonObject> {
  const { connection, adminKeypair, journal } = runtime;
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
  const preSendSimulation = await simulatePolicy(connection, policy, latest.value.blockhash);
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
    preSendSimulation: {
      contextSlot: preSendSimulation.contextSlot,
      unitsConsumed: preSendSimulation.unitsConsumed,
      err: preSendSimulation.err,
    },
  };
  if (preSendSimulation.err !== null) leg.state = "blocked";
  (journal.legs as JsonObject[]).push(leg);
  validateInstallJournal(journal, runtime.artifact);
  atomic(BASIC_POLICY_JOURNAL, journal);
  invariant(preSendSimulation.err === null, `policy ${policy.seed} pre-send simulation failed`);
  const returned = await connection.sendRawTransaction(wire, { skipPreflight: false, preflightCommitment: "finalized", maxRetries: 0, minContextSlot: latest.context.slot });
  invariant(returned === signature, `policy ${policy.seed} RPC signature mismatch`);
  const confirmation = await connection.confirmTransaction({ signature, blockhash: latest.value.blockhash, lastValidBlockHeight: latest.value.lastValidBlockHeight }, "finalized");
  invariant(confirmation.value.err === null, `policy ${policy.seed} finalized with an error`);
  const status = (await connection.getSignatureStatuses([signature], { searchTransactionHistory: true })).value[0];
  invariant(status?.err === null && status.confirmationStatus === "finalized", `policy ${policy.seed} did not reach finalized status`);
  const readback = await finalizedPolicyReadback(connection, runtime.artifact, policy.seed);
  Object.assign(leg, { state: "finalized", finalizedSlot: status.slot, readback });
  validateInstallJournal(journal, runtime.artifact);
  atomic(BASIC_POLICY_JOURNAL, journal);
  return readback;
}

async function installSeeds(runtime: InstallRuntime, seeds: readonly string[]): Promise<Map<string, JsonObject>> {
  const readbacks = new Map<string, JsonObject>();
  for (const seed of seeds) {
    readbacks.set(seed, await installSeedLeg(runtime, artifactPolicy(runtime.artifact, seed)));
  }
  return readbacks;
}

/**
 * One process per run. The lock is created exclusively before any
 * classification or journal write, so a second installer (or a second
 * reconcile of the same journal) refuses instead of racing the first.
 */
export function acquireInstallLock(path: string = BASIC_POLICY_INSTALL_LOCK, now: () => string = (): string => new Date().toISOString()): void {
  try {
    writeFileSync(path, `${JSON.stringify({ pid: process.pid, startedAt: now() })}\n`, { flag: "wx", mode: 0o600 });
  } catch {
    throw new Error(`basic policy install lock already exists at ${path}; another install process is active`);
  }
  chmodSync(path, 0o600);
}

export function releaseInstallLock(path: string = BASIC_POLICY_INSTALL_LOCK): void {
  rmSync(path, { force: true });
}

/** The first journal is created, never overwritten: an existing journal is a resume. */
function createInstallJournalFile(journal: JsonObject): void {
  writeFileSync(BASIC_POLICY_JOURNAL, `${JSON.stringify(journal, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  chmodSync(BASIC_POLICY_JOURNAL, 0o600);
}

/**
 * Resolve whether an unsent leg's recorded wire can still land: a confirmed or
 * finalized signature means it did land, an on-chain error means it cannot,
 * and an unknown signature is only safe to abandon once its blockhash has
 * expired.
 */
async function resolveUnsentLegEvidence(connection: ReconcileConnection, seed: string, leg: JsonObject): Promise<UnsentLegEvidence> {
  const signature = stringField(leg, "signature", "basic policy install journal leg");
  const status = (await connection.getSignatureStatuses([signature], { searchTransactionHistory: true })).value[0];
  if (status && status.err === null && (status.confirmationStatus === "finalized" || status.confirmationStatus === "confirmed")) {
    return { signatureStatus: "landed", blockhashValid: null, statusSlot: status.slot };
  }
  if (status && status.err !== null) return { signatureStatus: "failed", blockhashValid: null, statusSlot: status.slot };
  const blockhash = stringField(leg, "blockhash", "basic policy install journal leg");
  const validity = await connection.isBlockhashValid(blockhash, { commitment: "finalized" });
  return { signatureStatus: "unknown", blockhashValid: validity.value === true, statusSlot: null };
}

export type ReconcileOutcome = Readonly<{
  status: "reconciled" | "refused";
  reason: string | null;
  readbacks: ReadonlyMap<string, JsonObject>;
  pendingSeeds: readonly string[];
  /** Null only when the journal is refused before any chain state is read. */
  reconciliation: JsonObject | null;
}>;

/**
 * Reconcile an existing journal against finalized chain state before any
 * resume: every leg is classified from the live policy seed, its PDA presence,
 * and its signature/blockhash evidence, the journal legs are updated in place,
 * and the seeds that still need installing are handed back. This reads and
 * journals only; it never sends, and it refuses rather than force past a leg
 * whose on-chain state is unknown.
 */
export async function reconcileInstallJournal(input: Readonly<{
  connection: ReconcileConnection;
  artifact: BasicPolicyArtifact;
  journal: JsonObject;
  artifactSha256: string;
  now?: () => string;
}>): Promise<ReconcileOutcome> {
  const { connection, artifact, journal } = input;
  const drift = classifyJournalDrift({
    journalArtifactSha256: journal.artifactSha256,
    journalSettings: journal.settings,
    journalAuthority: journal.authority,
    artifactSha256: input.artifactSha256,
    settings: artifact.settings,
    authority: artifact.authority,
  });
  if (drift !== "match") {
    const reason = "basic policy install journal drifted from the current artifact; refusing to resume";
    return { status: "refused", reason, readbacks: new Map(), pendingSeeds: [], reconciliation: null };
  }
  const live = await finalizedSettings(connection);
  const presence = await readFinalizedPolicyPresence(connection, artifact);
  const context = { at: (input.now ?? ((): string => new Date().toISOString()))(), finalizedSlot: Math.max(live.contextSlot, presence.contextSlot), livePolicySeed: live.seed };
  const readbacks = new Map<string, JsonObject>();
  const verdicts: JsonObject[] = [];
  for (const rawLeg of journal.legs as JsonObject[]) {
    const seed = stringField(rawLeg, "seed", "basic policy install journal leg");
    const state = stringField(rawLeg, "state", "basic policy install journal leg");
    if (state === "abandoned") {
      verdicts.push({ seed, recordedState: state, verdict: "skipped" });
      continue;
    }
    let pdaPresent = presence.present.get(seed) === true;
    let evidence: UnsentLegEvidence | undefined;
    if (!pdaPresent) {
      evidence = await resolveUnsentLegEvidence(connection, seed, rawLeg);
      if (evidence.signatureStatus === "landed") {
        const landed = await connection.getAccountInfo(new PublicKey(artifactPolicy(artifact, seed).account), "finalized");
        if (landed !== null) {
          pdaPresent = true;
          evidence = undefined;
        }
      }
    }
    const decision = classifyJournalLeg({ state, seed, livePolicySeed: live.seed, pdaPresent, ...(evidence === undefined ? {} : { evidence }) });
    if (decision.action === "refuse") {
      return {
        status: "refused",
        reason: decision.reason,
        readbacks,
        pendingSeeds: [],
        reconciliation: {
          ...context,
          verdicts: [...verdicts, { seed, recordedState: state, verdict: "refuse", reason: decision.reason, ...verdictEvidence(evidence) }],
          refusal: decision.reason,
        },
      };
    }
    if (decision.action === "verify-finalized" || decision.action === "promote-finalized") {
      const readback = await finalizedPolicyReadback(connection, artifact, seed);
      if (decision.action === "promote-finalized") Object.assign(rawLeg, { state: "finalized", finalizedSlot: readback.finalizedSlot, readback });
      verdicts.push({ seed, recordedState: state, verdict: decision.action, ...verdictEvidence(evidence) });
      readbacks.set(seed, readback);
      continue;
    }
    if (decision.resimulate) {
      const latest = await connection.getLatestBlockhashAndContext("finalized");
      const simulation = await simulatePolicy(connection, artifactPolicy(artifact, seed), latest.value.blockhash);
      invariant(simulation.err === null, `basic policy ${seed} blocked leg re-simulation still fails; refusing to force past it`);
    }
    Object.assign(rawLeg, { state: "abandoned", abandonReason: decision.reason });
    verdicts.push({ seed, recordedState: state, verdict: "abandon-and-retry", reason: decision.reason, resimulated: decision.resimulate, ...verdictEvidence(evidence) });
  }
  const finalizedSeeds = new Set((journal.legs as JsonObject[])
    .filter((leg) => leg.state === "finalized")
    .map((leg) => String(leg.seed)));
  return {
    status: "reconciled",
    reason: null,
    readbacks,
    pendingSeeds: artifact.policies.map((policy) => policy.seed).filter((seed) => !finalizedSeeds.has(seed)),
    reconciliation: { ...context, verdicts },
  };
}

function verdictEvidence(evidence: UnsentLegEvidence | undefined): JsonObject {
  return evidence === undefined ? {} : { signatureStatus: evidence.signatureStatus, blockhashValid: evidence.blockhashValid, statusSlot: evidence.statusSlot };
}

async function writeInstallReadback(connection: Connection, artifact: BasicPolicyArtifact, readbacks: ReadonlyMap<string, JsonObject>): Promise<void> {
  const finalSettings = await finalizedSettings(connection);
  invariant(finalSettings.seed === "144", "finalized Settings policy seed did not reach 144");
  const policies = artifact.policies.map((policy) => {
    const readback = readbacks.get(policy.seed);
    invariant(readback !== undefined, `basic policy ${policy.seed} has no finalized readback`);
    return readback;
  });
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
    policies,
    finalizedSlot: Math.max(...policies.map((row) => Number(row.finalizedSlot)), finalSettings.contextSlot),
  };
  atomic(BASIC_POLICY_READBACK, result);
}

async function execute(rpc: string, artifact: BasicPolicyArtifact): Promise<void> {
  assertExecuteAuthorization(process.env);
  acquireInstallLock();
  try {
    const start = classifyInstallStart({
      journalExists: existsSync(BASIC_POLICY_JOURNAL),
      readbackExists: existsSync(BASIC_POLICY_READBACK),
    });
    invariant(start !== "complete", "basic policy install readback already exists; this install is complete");
    const connection = new Connection(rpc, "finalized");
    invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
    const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
    invariant(admin.signer.address === RWA_MULTIPLY_ROUTE.setupAdmin, "setup admin signer drifted");
    const adminKeypair = Keypair.fromSecretKey(admin.secretKey);
    if (start === "fresh") {
      const journal: JsonObject = {
        schema: "loyal-backyard-rwa-basic-policy-install-journal/v1",
        broadcast: true,
        artifactSha256: sha256(readFileSync(BASIC_POLICY_ARTIFACT)),
        settings: artifact.settings,
        authority: artifact.authority,
        legs: [],
      };
      validateInstallJournal(journal, artifact);
      createInstallJournalFile(journal);
      const runtime: InstallRuntime = { connection, artifact, adminKeypair, journal };
      await writeInstallReadback(connection, artifact, await installSeeds(runtime, artifact.policies.map((policy) => policy.seed)));
      return;
    }
    const journal = loadInstallJournal(artifact);
    const runtime: InstallRuntime = { connection, artifact, adminKeypair, journal };
    const reconciled = await reconcileInstallJournal({
      connection,
      artifact,
      journal,
      artifactSha256: sha256(readFileSync(BASIC_POLICY_ARTIFACT)),
    });
    if (reconciled.reconciliation !== null) {
      recordReconciliation(journal, reconciled.reconciliation);
      validateInstallJournal(journal, artifact);
      atomic(BASIC_POLICY_JOURNAL, journal);
    }
    invariant(reconciled.status !== "refused", reconciled.reason ?? "basic policy install journal could not be reconciled");
    const readbacks = await installSeeds(runtime, reconciled.pendingSeeds);
    for (const [seed, readback] of reconciled.readbacks) readbacks.set(seed, readback);
    await writeInstallReadback(connection, artifact, readbacks);
  } finally {
    releaseInstallLock();
  }
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
