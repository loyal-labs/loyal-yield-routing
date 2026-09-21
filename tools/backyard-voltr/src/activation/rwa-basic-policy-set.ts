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
  bump: number;
  transactionIndex: { toString(): string };
  staleTransactionIndex: { toString(): string };
  signers: readonly { key: PublicKey; permissions: { mask: number } }[];
  threshold: number;
  timeLock: number;
  policyState: { __kind: string; fields?: readonly unknown[] };
  start: { toString(): string };
  expiration: unknown;
  rentCollector: PublicKey;
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
const policyDiscriminator = (squadsGenerated as unknown as { policyDiscriminator: readonly number[] }).policyDiscriminator;

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

/** The bump the program itself derives for a Policy PDA, from the same seeds as `derivePolicyAddress`. */
export function canonicalPolicyBump(settings: string, seed: string): number {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(BigInt(seed));
  return PublicKey.findProgramAddressSync(
    [Buffer.from("smart_account"), Buffer.from("policy"), new PublicKey(settings).toBuffer(), seedBytes],
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.program),
  )[1];
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

/** A recorded slot, when recorded at all: null is allowed, a negative or fractional slot never is. */
function optionalSlot(value: JsonObject, key: string, label: string): void {
  const slot = value[key];
  if (slot === undefined) return;
  invariant(slot === null || (typeof slot === "number" && Number.isSafeInteger(slot) && slot >= 0), `${label}.${key} is invalid`);
}

function validateReconciliations(value: unknown): void {
  invariant(Array.isArray(value), "basic policy install journal reconciliations drifted");
  for (const [index, rawEntry] of value.entries()) {
    const label = `basic policy install journal reconciliation ${index}`;
    const entry = object(rawEntry, label);
    stringField(entry, "at", label);
    const finalizedSlot = entry.finalizedSlot;
    invariant(typeof finalizedSlot === "number" && Number.isSafeInteger(finalizedSlot) && finalizedSlot >= 0, `${label} finalized slot is invalid`);
    stringField(entry, "livePolicySeed", label);
    invariant(Array.isArray(entry.verdicts), `${label} verdicts drifted`);
    for (const [verdictIndex, rawVerdict] of entry.verdicts.entries()) {
      const verdictLabel = `${label} verdict ${verdictIndex}`;
      const verdict = object(rawVerdict, verdictLabel);
      stringField(verdict, "seed", verdictLabel);
      stringField(verdict, "verdict", verdictLabel);
      optionalSlot(verdict, "statusSlot", verdictLabel);
      optionalSlot(verdict, "validityContextSlot", verdictLabel);
      optionalSlot(verdict, "finalizedBlockHeight", verdictLabel);
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
  const attempts = new Map<string, string[]>();
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
    invariant(seedValue === priorSeed || seedValue === priorSeed + 1n, `${label} is not the next policy seed or an abandoned-seed retry`);
    invariant(leg.finalizedSlot === undefined || (typeof leg.finalizedSlot === "number" && Number.isSafeInteger(leg.finalizedSlot) && leg.finalizedSlot >= 0),
      `basic policy install journal leg ${seed} finalized slot is invalid`);
    const wireSha256 = stringField(leg, "wireSha256", label);
    invariant(/^[0-9a-f]{64}$/.test(wireSha256), `basic policy install journal leg ${seed} wire hash is invalid`);
    stringField(leg, "blockhash", label);
    const lastValidBlockHeight = leg.lastValidBlockHeight;
    invariant(typeof lastValidBlockHeight === "number" && Number.isSafeInteger(lastValidBlockHeight) && lastValidBlockHeight > 0,
      `basic policy install journal leg ${seed} block height is invalid`);
    const packetBytes = leg.packetBytes;
    invariant(typeof packetBytes === "number" && Number.isSafeInteger(packetBytes) && packetBytes > 0 && packetBytes <= PACKET_LIMIT,
      `basic policy install journal leg ${seed} packet size is invalid`);
    const signature = stringField(leg, "signature", label);
    let signatureBytes: Uint8Array;
    try {
      signatureBytes = bs58.decode(signature);
    } catch {
      throw new Error(`basic policy install journal leg ${seed} signature is not base58`);
    }
    invariant(signatureBytes.length === 64, `basic policy install journal leg ${seed} signature is not a 64-byte ed25519 signature`);
    assertLegWireBound(leg, seed, label, artifact);
    const preSendSimulation = object(leg.preSendSimulation, `basic policy install journal leg ${seed} pre-send simulation`);
    const simulationSlot = preSendSimulation.contextSlot;
    invariant((typeof simulationSlot === "number" && Number.isSafeInteger(simulationSlot) && simulationSlot >= 0) || simulationSlot === null,
      `basic policy install journal leg ${seed} simulation slot is invalid`);
    const simulationUnits = preSendSimulation.unitsConsumed;
    invariant((typeof simulationUnits === "number" && Number.isFinite(simulationUnits) && simulationUnits >= 0) || simulationUnits === null,
      `basic policy install journal leg ${seed} simulation units are invalid`);
    if (state === "planned") {
      invariant(preSendSimulation.err === null && preSendSimulation.err !== undefined,
        `basic policy install journal leg ${seed} is planned without a passing pre-send simulation`);
    } else if (state === "blocked") {
      invariant(preSendSimulation.err !== undefined && preSendSimulation.err !== null,
        `basic policy install journal leg ${seed} is blocked without a pre-send simulation error`);
    } else if (state === "abandoned") {
      stringField(leg, "abandonReason", label);
    } else {
      const readback = object(leg.readback, `basic policy install journal leg ${seed} readback`);
      invariant(readback.seed === seed, `basic policy install journal leg ${seed} readback seed drifted`);
      invariant(readback.account === leg.account, `basic policy install journal leg ${seed} readback account drifted`);
      invariant(typeof readback.dataSha256 === "string" && /^[0-9a-f]{64}$/.test(readback.dataSha256),
        `basic policy install journal leg ${seed} readback data hash is invalid`);
      if (artifact !== undefined) invariant(readback.dataSha256 === artifactPolicy(artifact, seed).dataSha256,
        `basic policy install journal leg ${seed} readback data hash drifted from the artifact`);
      invariant(typeof readback.accountDataSha256 === "string" && /^[0-9a-f]{64}$/.test(readback.accountDataSha256),
        `basic policy install journal leg ${seed} readback account hash is invalid`);
      const finalizedSlot = readback.finalizedSlot;
      invariant(typeof finalizedSlot === "number" && Number.isSafeInteger(finalizedSlot) && finalizedSlot >= 0,
        `basic policy install journal leg ${seed} readback slot is invalid`);
      const readbackBlockTime = readback.blockTime;
      invariant(readbackBlockTime === null || (typeof readbackBlockTime === "number" && Number.isSafeInteger(readbackBlockTime) && readbackBlockTime >= 0),
        `basic policy install journal leg ${seed} readback block time is invalid`);
      const readbackStart = readback.start;
      invariant(readbackBlockTime === null ? readbackStart === undefined : (typeof readbackStart === "number" && Number.isSafeInteger(readbackStart) && readbackStart > 0),
        `basic policy install journal leg ${seed} readback start is invalid`);
    }
    attempts.set(seed, [...(attempts.get(seed) ?? []), state]);
    priorSeed = seedValue;
  }
  // Repeated attempts at one seed: every attempt before the last must be
  // abandoned, so an unbroken attempt can never be retried and nothing follows
  // a finalized leg. The next seed may only open once the previous seed's last
  // attempt is finalized, since that is what moves the live policy seed.
  let priorOrderSeed: bigint | null = null;
  let priorLastState: string | null = null;
  for (const [seed, states] of attempts) {
    for (const state of states.slice(0, -1)) {
      invariant(state === "abandoned", state === "finalized"
        ? `basic policy install journal leg ${seed} retries a seed whose leg is already finalized`
        : `basic policy install journal leg ${seed} is a second unbroken attempt at the same seed; only an abandoned attempt may be retried`);
    }
    const lastState = states.at(-1);
    invariant(lastState !== undefined, `basic policy install journal leg ${seed} has no recorded attempts`);
    invariant(priorLastState === null || priorOrderSeed === null || priorLastState === "finalized",
      `basic policy install journal leg ${seed} cannot follow seed ${priorOrderSeed}, whose last attempt is ${priorLastState}, not finalized`);
    priorOrderSeed = BigInt(seed);
    priorLastState = lastState;
  }
}

/**
 * Byte-level binding between a journal leg and the wire it claims was signed:
 * the recorded wire is deserialized and every field reconcile or a resume
 * relies on - its hash, its signature, its blockhash, its fee payer, and its
 * single instruction - is proven against the leg's own records and the
 * artifact. A leg without the wire is an older journal and refuses.
 */
function assertLegWireBound(leg: JsonObject, seed: string, label: string, artifact?: BasicPolicyArtifact): void {
  const wire = decodeData(stringField(leg, "wire", label), `${label} wire`);
  invariant(sha256(wire) === stringField(leg, "wireSha256", label), `basic policy install journal leg ${seed} wire hash does not match its recorded wire`);
  let parsed: Transaction;
  try {
    parsed = Transaction.from(wire);
  } catch (error) {
    throw new Error(`basic policy install journal leg ${seed} wire is not a legacy Solana transaction (${String(error)})`);
  }
  invariant(parsed.recentBlockhash === stringField(leg, "blockhash", label), `basic policy install journal leg ${seed} wire recent blockhash does not match its recorded blockhash`);
  invariant(parsed.feePayer?.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin, `basic policy install journal leg ${seed} wire fee payer is not the artifact authority`);
  invariant(parsed.signatures.length === 1, `basic policy install journal leg ${seed} wire does not carry exactly one signature`);
  const wireSignature = parsed.signatures[0]?.signature;
  invariant(wireSignature !== undefined && wireSignature !== null, `basic policy install journal leg ${seed} wire carries no signature`);
  invariant(bs58.encode(wireSignature) === stringField(leg, "signature", label), `basic policy install journal leg ${seed} wire signature does not match its recorded signature`);
  invariant(parsed.instructions.length === 1, `basic policy install journal leg ${seed} wire does not carry exactly one instruction`);
  if (artifact === undefined) return;
  const expected = decodeData(artifactPolicy(artifact, seed).instruction.dataBase64, `basic policy ${seed} instruction data`);
  invariant(parsed.instructions[0]?.data.equals(expected), `basic policy install journal leg ${seed} wire instruction data does not match the artifact instruction data`);
}

/**
 * Read-only chain surface used by reconcile. It deliberately exposes no send
 * method: reconcile can only read, journal, and decide.
 */
export type ReconcileConnection = Pick<Connection,
  "getAccountInfo" |
  "getAccountInfoAndContext" |
  "getBlockHeight" |
  "getBlockTime" |
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
  signatureStatus: "landed" | "failed" | "replayable-failure" | "unknown";
  blockhashValid: boolean | null;
  /** Slot the validity answer was produced at, null when no validity answer was taken. */
  validityContextSlot: number | null;
  /** Finalized block height sampled with the validity answer, null when it could not be read. */
  finalizedBlockHeight: number | null;
  statusSlot: number | null;
  confirmationStatus?: string | null;
}>;

/**
 * The blockhash context a leg recorded when it was planned: the slot its
 * pre-send simulation ran at and the height its blockhash was valid to. An
 * expiry claim is only as good as these recorded anchors.
 */
export type LegBlockhashAnchor = Readonly<{
  simulationContextSlot: number | null;
  lastValidBlockHeight: number | null;
}>;

/** The recorded anchors of one journal leg, null where the leg records nothing usable. */
function legBlockhashAnchor(leg: JsonObject): LegBlockhashAnchor {
  const simulation = object(leg.preSendSimulation, "basic policy install journal leg pre-send simulation");
  const slot = simulation.contextSlot;
  const height = leg.lastValidBlockHeight;
  return {
    simulationContextSlot: typeof slot === "number" && Number.isSafeInteger(slot) && slot >= 0 ? slot : null,
    lastValidBlockHeight: typeof height === "number" && Number.isSafeInteger(height) && height > 0 ? height : null,
  };
}

/**
 * Whether a `false` from `isBlockhashValid` proves the leg's blockhash expired.
 * It only does when the answer was produced no earlier than the leg's own
 * pre-send simulation slot and the finalized block height has passed the
 * height the blockhash was valid to; a lagging or missing context is stale
 * evidence and never an abandon.
 */
function blockhashExpiryVerdict(seed: string, evidence: UnsentLegEvidence, anchor: LegBlockhashAnchor): Readonly<{ proven: boolean; reason: string | null }> {
  if (evidence.blockhashValid !== false) return { proven: false, reason: null };
  if (anchor.lastValidBlockHeight === null) {
    return { proven: false, reason: `basic policy ${seed} records no lastValidBlockHeight, so its expiry is ambiguous; blockhash expiry evidence is stale; re-run reconcile` };
  }
  if (anchor.simulationContextSlot === null) {
    return { proven: false, reason: `basic policy ${seed} records no pre-send simulation context slot; blockhash expiry evidence is stale; re-run reconcile` };
  }
  if (evidence.validityContextSlot === null || evidence.finalizedBlockHeight === null) {
    return { proven: false, reason: `basic policy ${seed} blockhash validity carries no context slot or finalized block height; blockhash expiry evidence is stale; re-run reconcile` };
  }
  if (evidence.validityContextSlot < anchor.simulationContextSlot) {
    return {
      proven: false,
      reason: `basic policy ${seed} blockhash validity was answered at slot ${evidence.validityContextSlot}, before the leg's pre-send simulation slot ${anchor.simulationContextSlot}; blockhash expiry evidence is stale; re-run reconcile`,
    };
  }
  if (evidence.finalizedBlockHeight <= anchor.lastValidBlockHeight) {
    return {
      proven: false,
      reason: `basic policy ${seed} finalized block height ${evidence.finalizedBlockHeight} has not passed its lastValidBlockHeight ${anchor.lastValidBlockHeight}; blockhash expiry evidence is stale; re-run reconcile`,
    };
  }
  return { proven: true, reason: null };
}

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
  /** The leg's recorded blockhash context, read straight out of the journal. */
  anchor?: LegBlockhashAnchor;
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
    if (evidence.signatureStatus === "failed") {
      // The only on-chain verdict that can never be reversed: a finalized
      // transaction with an error cannot land afterwards.
      return {
        action: "abandon-and-retry",
        reason: `basic policy ${input.seed} failed on chain at finalized; retrying from this seed`,
        resimulate: input.state === "blocked",
      };
    }
    if (evidence.blockhashValid !== false) {
      // A wire that errored before finalized commitment can still be replayed
      // by anyone holding it until its blockhash expires, so it is never a
      // safe abandon.
      return refuse(evidence.signatureStatus === "replayable-failure"
        ? `leg ${input.seed} failed at ${evidence.confirmationStatus ?? "an unconfirmed commitment"} but may still be replayed; re-run reconcile after the blockhash expires`
        : `leg ${input.seed} may still land; re-run reconcile after the blockhash expires`);
    }
    const expiry = blockhashExpiryVerdict(input.seed, evidence, input.anchor ?? { simulationContextSlot: null, lastValidBlockHeight: null });
    if (!expiry.proven) return refuse(expiry.reason ?? `leg ${input.seed} blockhash expiry is unproven; re-run reconcile`);
    return {
      action: "abandon-and-retry",
      reason: evidence.signatureStatus === "replayable-failure"
        ? `basic policy ${input.seed} failed at ${evidence.confirmationStatus ?? "an unconfirmed commitment"} without finalizing and its blockhash expired; retrying from this seed`
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

type SettingsActionArgs = {
  numSigners: unknown;
  actions: ReadonlyArray<{
    __kind: string;
    seed: { toString(): string };
    policyCreationPayload: { __kind: string; fields?: readonly unknown[] };
    signers: readonly { key: PublicKey; permissions: { mask: number } }[];
    threshold: number;
    timeLock: number;
    startTimestamp: unknown;
    expirationArgs: unknown;
  }>;
};

const syncSettingsTransactionArgsBeet = (squadsGenerated as unknown as {
  syncSettingsTransactionArgsBeet: {
    toFixedFromData(data: Buffer, offset: number): { read(data: Buffer, offset: number): unknown; byteSize: number };
  };
}).syncSettingsTransactionArgsBeet;

export type PolicyExpectation = Readonly<{
  settings: string;
  seed: string;
  signers: readonly { key: string; permissionsMask: number }[];
  threshold: number;
  timeLock: number;
  rentCollector: string;
  policyKind: string;
  accountIndex: number;
  constraints: readonly unknown[];
}>;

/**
 * Derive the complete on-chain Policy identity the artifact instruction must
 * produce. Every expected value is read out of the artifact instruction
 * itself - the settings account meta and the decoded PolicyCreate action -
 * so a readback is accepted only when the chain holds exactly what this
 * installer specified, field by field. The two fields PolicyCreate leaves to
 * the program are handled explicitly: rentCollector is the paying authority,
 * and start is the program-assigned landing timestamp verified against the
 * leg's block time by `assertPolicyMatchesArtifact`.
 */
export function policyExpectationFromArtifact(policy: BasicPolicyRow): PolicyExpectation {
  const data = decodeData(policy.instruction.dataBase64, `basic policy ${policy.seed} instruction data`);
  invariant(sha256(data) === policy.dataSha256, `basic policy ${policy.seed} instruction data hash drifted`);
  const fixed = syncSettingsTransactionArgsBeet.toFixedFromData(data, 8);
  const args = fixed.read(data, 8) as Partial<SettingsActionArgs> | null;
  invariant(args !== null && typeof args === "object", `basic policy ${policy.seed} instruction does not decode to settings actions`);
  invariant(data.length === 8 + fixed.byteSize, `basic policy ${policy.seed} instruction data has trailing bytes`);
  invariant(args.numSigners === 1 && Array.isArray(args.actions) && args.actions.length === 1,
    `basic policy ${policy.seed} instruction is not exactly one settings action`);
  const action = args.actions[0];
  invariant(action !== undefined && typeof action === "object" && action.__kind !== undefined,
    `basic policy ${policy.seed} settings action is malformed`);
  invariant(action.__kind === "PolicyCreate", `basic policy ${policy.seed} action is not PolicyCreate`);
  invariant(action.policyCreationPayload.__kind === "ProgramInteraction", `basic policy ${policy.seed} payload is not ProgramInteraction`);
  const body = object(action.policyCreationPayload.fields?.[0], `basic policy ${policy.seed} payload body`);
  invariant(Array.isArray(body.instructionsConstraints), `basic policy ${policy.seed} payload has no constraint vector`);
  invariant(typeof body.accountIndex === "number", `basic policy ${policy.seed} payload account index is invalid`);
  invariant(Array.isArray(body.spendingLimits) && body.spendingLimits.length === 0, `basic policy ${policy.seed} payload sets spending limits, which this installer never creates`);
  invariant(body.preHook === null && body.postHook === null, `basic policy ${policy.seed} payload sets hooks, which this installer never creates`);
  invariant(action.startTimestamp === null || action.startTimestamp === undefined, `basic policy ${policy.seed} sets an unsupported start timestamp`);
  invariant(action.expirationArgs === null || action.expirationArgs === undefined, `basic policy ${policy.seed} sets an unsupported expiration`);
  const settings = policy.instruction.accounts[0]?.address;
  invariant(typeof settings === "string" && settings.length > 0, `basic policy ${policy.seed} instruction has no settings account`);
  return {
    settings,
    seed: action.seed.toString(),
    signers: action.signers.map((signer: { key: PublicKey; permissions: { mask: number } }) => ({
      key: signer.key.toBase58(),
      permissionsMask: signer.permissions.mask,
    })),
    threshold: action.threshold,
    timeLock: action.timeLock,
    // PolicyCreate sets no rent collector, so the program fills it with the
    // paying authority: the instruction fee payer, which parseArtifact pins to
    // the artifact authority. start is likewise program-assigned - the clock
    // unix timestamp at execution - so it is checked against the leg's landing
    // block time instead of a literal from the artifact.
    rentCollector: RWA_MULTIPLY_ROUTE.setupAdmin,
    policyKind: "ProgramInteraction",
    accountIndex: body.accountIndex,
    constraints: body.instructionsConstraints,
  };
}

function policyExpirationUnset(value: unknown): boolean {
  if (value === null || value === undefined) return true;
  return typeof value === "object" && (value as { __kind?: unknown }).__kind === "None";
}

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

/**
 * Full policy identity check. The on-chain Policy account is decoded with the
 * generated Squads decoder and every field the PolicyCreate instruction sets
 * is compared against the value derived from the artifact instruction; the
 * constraint vector is additionally verified byte-exactly. Because the program
 * assigns `start` itself, it is only accepted when it sits within 120 seconds
 * of the landing block time; without a block time the fallback rule accepts
 * any positive timestamp no further than 120 seconds in the future. Any
 * difference refuses.
 */
export function assertPolicyMatchesArtifact(policy: BasicPolicyRow, decoded: PolicyState, landing?: Readonly<{ blockTime: number }>): void {
  const expected = policyExpectationFromArtifact(policy);
  const seed = policy.seed;
  invariant(decoded.settings.toBase58() === expected.settings,
    `policy ${seed} on-chain settings ${decoded.settings.toBase58()} does not match the artifact ${expected.settings}`);
  invariant(decoded.seed.toString() === expected.seed,
    `policy ${seed} on-chain seed ${decoded.seed.toString()} does not match the artifact ${expected.seed}`);
  invariant(decoded.bump === canonicalPolicyBump(expected.settings, expected.seed),
    `policy ${seed} on-chain bump ${decoded.bump} does not match the canonical PDA bump`);
  invariant(decoded.transactionIndex.toString() === "0" && decoded.staleTransactionIndex.toString() === "0",
    `policy ${seed} on-chain transaction index ${decoded.transactionIndex}/${decoded.staleTransactionIndex} is not the fresh-install zero state`);
  invariant(decoded.signers.length === expected.signers.length,
    `policy ${seed} on-chain signer count ${decoded.signers.length} does not match the artifact ${expected.signers.length}`);
  for (const [index, signer] of decoded.signers.entries()) {
    const want = expected.signers[index];
    invariant(want !== undefined && signer.key.toBase58() === want.key,
      `policy ${seed} on-chain delegated signer ${signer.key.toBase58()} does not match the artifact ${want?.key ?? "none"}`);
    invariant(want !== undefined && signer.permissions.mask === want.permissionsMask,
      `policy ${seed} on-chain signer permissions ${signer.permissions.mask} do not match the artifact ${want?.permissionsMask}`);
  }
  invariant(decoded.threshold === expected.threshold,
    `policy ${seed} on-chain threshold ${decoded.threshold} does not match the artifact ${expected.threshold}`);
  invariant(decoded.timeLock === expected.timeLock,
    `policy ${seed} on-chain time lock ${decoded.timeLock} does not match the artifact ${expected.timeLock}`);
  const start = Number(decoded.start.toString());
  invariant(landing === undefined
    ? start > 0 && start <= Math.floor(Date.now() / 1000) + 120
    : Math.abs(start - landing.blockTime) <= 120,
    landing === undefined
      ? `policy ${seed} on-chain start ${start} is not a positive timestamp within 120s of now`
      : `policy ${seed} on-chain start ${start} is not within 120s of the landing block time ${landing.blockTime}`);
  invariant(policyExpirationUnset(decoded.expiration),
    `policy ${seed} on-chain policy expires, but the artifact sets no expiration`);
  invariant(decoded.rentCollector.toBase58() === expected.rentCollector,
    `policy ${seed} on-chain rent collector ${decoded.rentCollector.toBase58()} does not match the paying authority ${expected.rentCollector}`);
  invariant(decoded.policyState.__kind === expected.policyKind,
    `policy ${seed} on-chain policy kind ${decoded.policyState.__kind} does not match the artifact ${expected.policyKind}`);
  const body = object(decoded.policyState.fields?.[0], `policy ${seed} on-chain policy state`);
  invariant(body.accountIndex === expected.accountIndex,
    `policy ${seed} on-chain account index ${String(body.accountIndex)} does not match the artifact ${expected.accountIndex}`);
  invariant(body.preHook === null && body.postHook === null,
    `policy ${seed} on-chain policy has hooks, but the artifact sets none`);
  invariant(Array.isArray(body.spendingLimits) && body.spendingLimits.length === 0,
    `policy ${seed} on-chain policy has spending limits, but the artifact sets none`);
  invariant(Array.isArray(body.instructionsConstraints) && body.instructionsConstraints.length === expected.constraints.length,
    `policy ${seed} on-chain constraint count ${String(body.instructionsConstraints)} does not match the artifact ${expected.constraints.length}`);
  assertPolicyPayloadMatchesArtifact(policy, body.instructionsConstraints);
}

export type PolicyReadback = Readonly<{
  seed: string;
  account: string;
  dataSha256: string;
  accountDataSha256: string;
  finalizedSlot: number;
  blockTime: number | null;
  start?: number;
}>;

/**
 * The slot that landed a leg's wire, resolved from its recorded signature: the
 * block time of this slot is what the program-assigned policy start is checked
 * against. Null when the signature has no status, in which case the readback
 * falls back to the no-block-time rule.
 */
async function finalizedLandingSlot(connection: ReconcileConnection, leg: JsonObject): Promise<number | null> {
  const signature = stringField(leg, "signature", "basic policy install journal leg");
  const status = (await connection.getSignatureStatuses([signature], { searchTransactionHistory: true })).value[0];
  return status?.err === null && typeof status.slot === "number" ? status.slot : null;
}

async function finalizedPolicyReadback(connection: ReconcileConnection, artifact: BasicPolicyArtifact, seed: string, landingSlot: number | null): Promise<PolicyReadback> {
  const policy = artifactPolicy(artifact, seed);
  const response = await connection.getAccountInfoAndContext(new PublicKey(policy.account), "finalized");
  invariant(response.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, `policy ${policy.seed} readback is absent or has wrong owner`);
  // The generated decoder drops the account discriminator, so it is checked
  // against the raw bytes before anything else is trusted.
  invariant(response.value.data.subarray(0, policyDiscriminator.length).equals(Buffer.from(policyDiscriminator)),
    `policy ${policy.seed} on-chain account discriminator does not match the generated Policy discriminator`);
  const decoded = Policy.fromAccountInfo(response.value)[0];
  const rawBlockTime = landingSlot === null ? null : await connection.getBlockTime(landingSlot);
  const blockTime = typeof rawBlockTime === "number" ? rawBlockTime : null;
  assertPolicyMatchesArtifact(policy, decoded, ...(blockTime === null ? [] : [{ blockTime }]));
  return {
    seed: policy.seed,
    account: policy.account,
    dataSha256: policy.dataSha256,
    accountDataSha256: sha256(response.value.data),
    finalizedSlot: response.context.slot,
    blockTime,
    ...(blockTime === null ? {} : { start: Number(decoded.start.toString()) }),
  };
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
    wire: wire.toString("base64"),
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
  const readback = await finalizedPolicyReadback(connection, runtime.artifact, policy.seed,
    typeof status.slot === "number" ? status.slot : null);
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
  let created = false;
  try {
    writeFileSync(path, `${JSON.stringify({ pid: process.pid, startedAt: now() })}\n`, { flag: "wx", mode: 0o600 });
    created = true;
    chmodSync(path, 0o600);
  } catch (error) {
    // Never leave a half-created lock behind: a failed chmod would otherwise
    // block every later run even though no other installer is active.
    if (created) rmSync(path, { force: true });
    throw new Error(`basic policy install lock already exists at ${path}; another install process is active. Remove docs/evidence/backyard-rwa-basic/policy-install.lock only after confirming no installer process is running: pgrep -fl rwa-basic-policy-set (${String(error)})`);
  }
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
 * finalized success means it did land, only a *finalized* error means it
 * cannot, and any other outcome (no status, or an error below finalized) can
 * still be replayed while its blockhash is valid.
 */
async function resolveUnsentLegEvidence(connection: ReconcileConnection, seed: string, leg: JsonObject): Promise<UnsentLegEvidence> {
  const signature = stringField(leg, "signature", "basic policy install journal leg");
  const status = (await connection.getSignatureStatuses([signature], { searchTransactionHistory: true })).value[0];
  if (status && status.err === null && (status.confirmationStatus === "finalized" || status.confirmationStatus === "confirmed")) {
    return { signatureStatus: "landed", blockhashValid: null, validityContextSlot: null, finalizedBlockHeight: null, statusSlot: status.slot };
  }
  if (status && status.err !== null && status.confirmationStatus === "finalized") {
    return { signatureStatus: "failed", blockhashValid: null, validityContextSlot: null, finalizedBlockHeight: null, statusSlot: status.slot };
  }
  const blockhash = stringField(leg, "blockhash", "basic policy install journal leg");
  // Sampled in this order on purpose: the block height is read after the
  // validity answer, so it can only be at least as advanced as the slot the
  // answer describes.
  const validity = await connection.isBlockhashValid(blockhash, { commitment: "finalized" });
  const blockHeight = await connection.getBlockHeight("finalized");
  const replayableFailure = Boolean(status && status.err !== null);
  return {
    signatureStatus: replayableFailure ? "replayable-failure" : "unknown",
    blockhashValid: validity.value === true,
    validityContextSlot: validity.context.slot,
    finalizedBlockHeight: blockHeight,
    statusSlot: status ? status.slot : null,
    ...(replayableFailure && typeof status?.confirmationStatus === "string" ? { confirmationStatus: status.confirmationStatus } : {}),
  };
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
  const abandonedAttempts = new Map<string, number>();
  for (const [index, rawLeg] of (journal.legs as JsonObject[]).entries()) {
    const seed = stringField(rawLeg, "seed", "basic policy install journal leg");
    // Every leg - recorded abandons included - must still bind to the wire it
    // claims was signed, or the evidence below describes a transaction nobody
    // can account for.
    assertLegWireBound(rawLeg, seed, `basic policy install journal leg ${index}`, artifact);
    const anchor = legBlockhashAnchor(rawLeg);
    const state = stringField(rawLeg, "state", "basic policy install journal leg");
    if (state === "abandoned") {
      // A recorded abandon is a claim, not evidence: every abandoned leg -
      // historical attempts included - is re-verified against finalized chain
      // state before any retry at that seed is authorized. Only a finalized
      // error or a proven expired blockhash means the wire can never land.
      const attempt = (abandonedAttempts.get(seed) ?? 0) + 1;
      abandonedAttempts.set(seed, attempt);
      const evidence = await resolveUnsentLegEvidence(connection, seed, rawLeg);
      const expiry = blockhashExpiryVerdict(seed, evidence, anchor);
      const stillReplayable = evidence.signatureStatus === "landed" ||
        (evidence.signatureStatus !== "failed" && !expiry.proven);
      if (stillReplayable) {
        const reason = expiry.reason ?? `abandoned leg ${seed} attempt ${attempt} is still replayable; manual review`;
        return {
          status: "refused",
          reason,
          readbacks,
          pendingSeeds: [],
          reconciliation: {
            ...context,
            verdicts: [...verdicts, { seed, attempt, recordedState: state, verdict: "refuse", reason, ...verdictEvidence(evidence) }],
            refusal: reason,
          },
        };
      }
      verdicts.push({ seed, attempt, recordedState: state, verdict: "skipped", ...verdictEvidence(evidence) });
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
    const decision = classifyJournalLeg({ state, seed, livePolicySeed: live.seed, pdaPresent, anchor, ...(evidence === undefined ? {} : { evidence }) });
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
      const readback = await finalizedPolicyReadback(connection, artifact, seed, await finalizedLandingSlot(connection, rawLeg));
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
  if (evidence === undefined) return {};
  return {
    signatureStatus: evidence.signatureStatus,
    blockhashValid: evidence.blockhashValid,
    validityContextSlot: evidence.validityContextSlot,
    finalizedBlockHeight: evidence.finalizedBlockHeight,
    statusSlot: evidence.statusSlot,
    ...(evidence.confirmationStatus === undefined || evidence.confirmationStatus === null ? {} : { confirmationStatus: evidence.confirmationStatus }),
  };
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

async function execute(rpc: string): Promise<void> {
  assertExecuteAuthorization(process.env);
  acquireInstallLock();
  try {
    const start = classifyInstallStart({
      journalExists: existsSync(BASIC_POLICY_JOURNAL),
      readbackExists: existsSync(BASIC_POLICY_READBACK),
    });
    invariant(start !== "complete", "basic policy install readback already exists; this install is complete");
    // Exactly one read of the artifact bytes, under the lock: the parsed
    // instructions and the journal's drift hash both come from those bytes, so
    // a file rewritten between two reads can never split the run in half.
    const { artifact, artifactSha256 } = loadArtifactWithSha256();
    const connection = new Connection(rpc, "finalized");
    invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
    const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
    invariant(admin.signer.address === RWA_MULTIPLY_ROUTE.setupAdmin, "setup admin signer drifted");
    const adminKeypair = Keypair.fromSecretKey(admin.secretKey);
    if (start === "fresh") {
      const journal: JsonObject = {
        schema: "loyal-backyard-rwa-basic-policy-install-journal/v1",
        broadcast: true,
        artifactSha256,
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
      artifactSha256,
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

/** One read of the artifact file; the parsed artifact and its hash share those exact bytes. */
function loadArtifactWithSha256(): { artifact: BasicPolicyArtifact; artifactSha256: string } {
  const bytes = readFileSync(BASIC_POLICY_ARTIFACT);
  return {
    artifact: parseArtifact(JSON.parse(bytes.toString("utf8")) as unknown),
    artifactSha256: sha256(bytes),
  };
}

async function main(): Promise<void> {
  const simulateMode = process.argv.includes("--simulate");
  const executeMode = process.argv.includes("--execute");
  invariant(simulateMode !== executeMode, "choose exactly one of --simulate or --execute");
  const rpc = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpc, "SOLANA_RPC_URL is required");
  if (simulateMode) {
    const result = await simulate(rpc, readArtifact());
    atomic(BASIC_POLICY_SIMULATION, result);
    console.log(JSON.stringify({ verdict: result.verdict, broadcast: false, contextSlot: result.contextSlot, policies: (result.policies as PolicySimulationRow[]).map((policy) => ({ seed: policy.seed, err: policy.err, unitsConsumed: policy.unitsConsumed })) }));
    return;
  }
  await execute(rpc);
  console.log(JSON.stringify({ verdict: "PASS", broadcast: true }));
}

const INVOKED = process.argv[1] !== undefined && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url));
if (INVOKED) {
  main().catch((error: unknown) => {
    console.error(`basic policy installer failed: ${error instanceof Error ? error.message : String(error)}`);
    process.exitCode = 1;
  });
}
