import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { FarmState } from "@kamino-finance/farms-sdk";
import { LendingMarket, Reserve } from "@kamino-finance/klend-sdk";
import { OracleMappings } from "@kamino-finance/scope-sdk/dist/@codegen/scope/accounts/OracleMappings.js";
import { OraclePrices } from "@kamino-finance/scope-sdk/dist/@codegen/scope/accounts/OraclePrices.js";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { getMintDecoder, getTokenDecoder } from "@solana-program/token";
import { createNoopSigner, type Address } from "@solana/kit";
import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  Connection,
  PublicKey,
  TransactionMessage,
  TransactionInstruction,
  VersionedTransaction,
  type Commitment,
  type MessageV0,
} from "@solana/web3.js";

import {
  getStrategyInitReceiptDecoder,
} from "@voltr/vault-sdk";
import {
  PARTNER_LOOKUP_TABLE_COMPATIBILITY_IDENTITY,
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_FOUR_MARKET_STRATEGIES,
  PARTNER_ROUTE,
  PARTNER_SCOPE_ORACLE_MAPPINGS,
  PARTNER_SCOPE_PROGRAM,
  PARTNER_STRATEGY_CANDIDATES,
  fourMarketRouteSpecSha256,
  routeSpecSha256,
  type PartnerStrategyGraphIdentity,
  type PartnerRouteSpec,
  type PartnerStrategyCandidate,
} from "../domain/route-spec.js";
import {
  loadDeploymentIdentities,
  loadReserveGraphs,
  toWeb3Instruction,
  type AccountSnapshot,
  type ReserveGraphObservation,
} from "../integrations/solana-compat.js";
import {
  createVoltrRouteBuilder,
  deriveVoltrAccountsForStrategy,
  type CanonicalInstruction,
  type VoltrAccounts,
} from "../integrations/voltr.js";
import { loadRuntimePolicyArtifact } from "../policies/compiler.js";
import { verifyExistingRuntimePolicies } from "../policies/commands.js";
import { buildManagerWrapperForCompatibility } from "../runtime/manager.js";
import {
  verifyDeploymentIdentities,
  verifyStrategyBootstrap,
  type Gate,
} from "./current.js";

const SOLANA_PACKET_LIMIT = 1_232;
const MANAGER_COMPUTE_UNIT_LIMIT = 500_000;
const MANAGER_HEAP_FRAME_BYTES = 256 * 1_024;
const U64_MAX = (1n << 64n) - 1n;
const MAIN_RUNTIME_POLICY_ARTIFACT_LABEL =
  "docs/evidence/backyard-voltr-partner-vault/runtime-policies-precreated-600s-v1.json";
const MAIN_RUNTIME_POLICY_ARTIFACT = resolve(
  fileURLToPath(new URL("../../../..", import.meta.url)),
  MAIN_RUNTIME_POLICY_ARTIFACT_LABEL,
);
const FOUR_MARKET_RUNTIME_POLICY_ARTIFACT_LABEL =
  "docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v1.json";
const FOUR_MARKET_RUNTIME_POLICY_ARTIFACT = resolve(
  fileURLToPath(new URL("../../../..", import.meta.url)),
  FOUR_MARKET_RUNTIME_POLICY_ARTIFACT_LABEL,
);
const REPOSITORY_ROOT = fileURLToPath(new URL("../../../..", import.meta.url));
const COMPATIBILITY_APPROVAL_LABEL =
  "docs/evidence/backyard-voltr-four-market/compatibility-verifier-approval-v1.json";
const COMPATIBILITY_APPROVAL = resolve(
  REPOSITORY_ROOT,
  COMPATIBILITY_APPROVAL_LABEL,
);
const DEPOSIT_CONSTRAINED_INDEXES = [
  0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15, 17, 21, 29, 30,
] as const;
const WITHDRAW_CONSTRAINED_INDEXES = [
  0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12, 13, 14, 15, 17, 21, 26, 27,
] as const;
const PROGRAM_INTERACTION_PAYLOAD_CODEC = (
  squadsGenerated as unknown as {
    programInteractionPolicyCreationPayloadBeet: {
      serialize(value: unknown): readonly [Uint8Array, number];
    };
  }
).programInteractionPolicyCreationPayloadBeet;

type BootstrapState =
  | "READY_FOR_MANAGER_SIMULATION"
  | "PENDING_EXPECTED_BOOTSTRAP"
  | "INVALID_EXISTING_STATE";

type SimulationReport = Readonly<{
  status:
    | "pass"
    | "fail"
    | "observed"
    | "not_run_economic_precondition"
    | "not_run_invalid_support_state"
    | "not_run_expected_bootstrap_missing"
    | "not_run_policy_missing";
  contextSlot: number | null;
  err: unknown;
  unitsConsumed: number | null;
  logsSha256: string | null;
  reasonCode: string | null;
}>;

type PacketReport = Readonly<{
  packetBytes: number;
  messageBytes: number;
  messageSha256: string | null;
  serializedMessageLengthMatches: boolean | null;
  requiredSignatureCount: number;
  signerAddresses: readonly string[];
  staticAccountCount: number;
  lookupTableCount: number;
  loadedWritableCount: number;
  loadedReadonlyCount: number;
  lookupTableAddresses: readonly string[];
  lookupResolutions: readonly Readonly<{
    address: string;
    writable: readonly Readonly<{ index: number; address: string | null }>[];
    readonly: readonly Readonly<{ index: number; address: string | null }>[];
  }>[];
  compiledInstructions: readonly Readonly<{
    programIdIndex: number;
    programId: string | null;
    accountKeyIndexes: readonly number[];
    accounts: readonly Readonly<{
      address: string | null;
      signer: boolean;
      writable: boolean;
    }>[];
    dataLength: number;
    dataSha256: string;
  }>[];
  withinLimit: boolean;
}>;

type AltIdentity = Readonly<{
  address: string;
  authority: string | null;
  deactivationSlot: string;
  lastExtendedSlot: number;
  addressCount: number;
  orderedAddressesSha256: string;
  contextSlot: number;
}>;

type BuiltStrategy = Readonly<{
  candidate: PartnerStrategyCandidate;
  identity: PartnerStrategyGraphIdentity;
  route: PartnerRouteSpec;
  observation: ReserveGraphObservation;
  accounts: VoltrAccounts;
  strategyAssetAta: string;
  initialize: CanonicalInstruction;
  deposit: CanonicalInstruction;
  withdraw: CanonicalInstruction;
  setupInstructions: readonly TransactionInstruction[];
}>;

type SourceBinding = Readonly<{
  files: readonly Readonly<{ path: string; sha256: string }>[];
  aggregateSha256: string;
}>;

type CompatibilityApproval = Readonly<{
  path: string;
  fileSha256: string;
  approvalId: "operator-fixed-verifier-v1";
  sourceBinding: SourceBinding;
  runtimePolicyArtifacts: Readonly<{
    mainBaseline: Readonly<{
      path: string;
      fileSha256: string;
      artifactSha256: string;
      sourceManifestSha256: string;
    }>;
    fourMarketCatalog: Readonly<{
      path: string;
      fileSha256: string;
      artifactSha256: string;
      sourceManifestSha256: string;
    }>;
  }>;
}>;

function sha256(value: string | ArrayLike<number>): string {
  return createHash("sha256")
    .update(typeof value === "string" ? value : Uint8Array.from(value))
    .digest("hex");
}

function stableJson(value: unknown): string {
  return JSON.stringify(value, (_key, entry) => {
    if (typeof entry === "bigint") return entry.toString();
    if (entry instanceof Uint8Array) return Buffer.from(entry).toString("base64");
    return entry;
  });
}

function canonicalJson(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function localSourceBinding(): SourceBinding {
  const labels = [
    "Cargo.lock",
    "Cargo.toml",
    "crates/loyal-actions/Cargo.toml",
    "crates/loyal-actions/src/actions.rs",
    "crates/loyal-actions/src/autonomous_vaults/mod.rs",
    "crates/loyal-actions/src/autonomous_vaults/voltr_kamino.rs",
    "crates/loyal-actions/src/bin/compile_voltr_kamino_runtime_policy.rs",
    "crates/loyal-actions/src/detection.rs",
    "crates/loyal-actions/src/ids.rs",
    "crates/loyal-actions/src/lib.rs",
    "crates/loyal-actions/src/protocols.rs",
    "crates/loyal-actions/src/squads.rs",
    "crates/loyal-actions/src/stablecoins.rs",
    "tools/backyard-voltr/bun.lock",
    "tools/backyard-voltr/package.json",
    "tools/backyard-voltr/src/cli.ts",
    "tools/backyard-voltr/src/domain/route-spec.ts",
    "tools/backyard-voltr/src/integrations/solana-compat.ts",
    "tools/backyard-voltr/src/integrations/voltr.ts",
    "tools/backyard-voltr/src/policies/compiler.ts",
    "tools/backyard-voltr/src/policies/authorization.ts",
    "tools/backyard-voltr/src/policies/commands.ts",
    "tools/backyard-voltr/src/domain/execution-intent.ts",
    "tools/backyard-voltr/src/integrations/signer.ts",
    "tools/backyard-voltr/src/verify/squads.ts",
    "tools/backyard-voltr/src/runtime/manager.ts",
    "tools/backyard-voltr/src/verify/compatibility.ts",
    "tools/backyard-voltr/src/verify/current.ts",
    "tools/backyard-voltr/tsconfig.json",
  ] as const;
  const files = labels.map((label) => ({
    path: label,
    sha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, label))),
  }));
  return {
    files,
    aggregateSha256: sha256(stableJson(files)),
  } as const;
}

type JsonRecord = Record<string, unknown>;

function record(value: unknown, label: string): JsonRecord {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value as JsonRecord;
}

function exactKeys(value: JsonRecord, expected: readonly string[], label: string): void {
  if (Object.keys(value).sort().join("\0") !== [...expected].sort().join("\0")) {
    throw new Error(`${label} keys are not exact`);
  }
}

function stringField(value: JsonRecord, key: string, label: string): string {
  const field = value[key];
  if (typeof field !== "string" || field.length === 0) {
    throw new Error(`${label}.${key} must be a non-empty string`);
  }
  return field;
}

function shaField(value: JsonRecord, key: string, label: string): string {
  const field = stringField(value, key, label);
  if (!/^[0-9a-f]{64}$/.test(field)) {
    throw new Error(`${label}.${key} must be a lowercase SHA-256 digest`);
  }
  return field;
}

function candidateCatalogSha256(): string {
  return sha256(stableJson(PARTNER_STRATEGY_CANDIDATES));
}

function loadCompatibilityApproval(
  approvalPath: string | null,
  confirmedFileSha256: string | null,
  observedSourceBinding: SourceBinding,
): CompatibilityApproval {
  if (!approvalPath) {
    throw new Error("verify compatibility requires --approval");
  }
  if (!confirmedFileSha256 || !/^[0-9a-f]{64}$/.test(confirmedFileSha256)) {
    throw new Error(
      "verify compatibility requires a lowercase --confirm-approval-sha256",
    );
  }
  const absolutePath = resolve(approvalPath);
  if (absolutePath !== COMPATIBILITY_APPROVAL) {
    throw new Error(
      `approval path must be ${COMPATIBILITY_APPROVAL_LABEL}`,
    );
  }
  const bytes = readFileSync(absolutePath);
  const fileSha256 = sha256(bytes);
  if (fileSha256 !== confirmedFileSha256) {
    throw new Error(
      `approval SHA-256 mismatch: observed ${fileSha256}, confirmed ${confirmedFileSha256}`,
    );
  }
  const text = bytes.toString("utf8");
  const parsed: unknown = JSON.parse(text);
  if (canonicalJson(parsed) !== text) {
    throw new Error("compatibility approval must use canonical two-space JSON with one trailing newline");
  }
  const root = record(parsed, "compatibility approval");
  exactKeys(root, [
    "schemaVersion",
    "evidenceType",
    "approvalId",
    "routeId",
    "cluster",
    "genesisHash",
    "routeSpec",
    "sourceBinding",
    "runtimePolicyArtifacts",
  ], "compatibility approval");
  if (root.schemaVersion !== 1) throw new Error("compatibility approval schemaVersion must be 1");
  if (root.evidenceType !== "backyard-voltr-four-market-compatibility-approval") {
    throw new Error("compatibility approval evidenceType is not exact");
  }
  if (root.approvalId !== "operator-fixed-verifier-v1") {
    throw new Error("compatibility approval approvalId is not exact");
  }
  if (root.routeId !== PARTNER_FOUR_MARKET_ROUTE.id) {
    throw new Error("compatibility approval routeId is not exact");
  }
  if (root.cluster !== PARTNER_ROUTE.cluster || root.genesisHash !== PARTNER_ROUTE.genesisHash) {
    throw new Error("compatibility approval cluster/genesis is not exact mainnet-beta");
  }

  const routeSpec = record(root.routeSpec, "compatibility approval routeSpec");
  exactKeys(routeSpec, [
    "baseMainRouteSpecSha256",
    "fourMarketRouteSpecSha256",
    "candidateCatalogSha256",
  ], "compatibility approval routeSpec");
  if (
    shaField(routeSpec, "baseMainRouteSpecSha256", "compatibility approval routeSpec")
      !== routeSpecSha256(PARTNER_ROUTE)
    || shaField(routeSpec, "fourMarketRouteSpecSha256", "compatibility approval routeSpec")
      !== fourMarketRouteSpecSha256()
    || shaField(routeSpec, "candidateCatalogSha256", "compatibility approval routeSpec")
      !== candidateCatalogSha256()
  ) {
    throw new Error("compatibility approval route/catalog hashes do not match checked-out code");
  }

  const source = record(root.sourceBinding, "compatibility approval sourceBinding");
  exactKeys(source, ["algorithm", "files", "aggregateSha256"], "compatibility approval sourceBinding");
  if (source.algorithm !== "sha256" || !Array.isArray(source.files)) {
    throw new Error("compatibility approval sourceBinding algorithm/files are invalid");
  }
  const files = source.files.map((value, index) => {
    const item = record(value, `compatibility approval sourceBinding.files[${index}]`);
    exactKeys(item, ["path", "sha256"], `compatibility approval sourceBinding.files[${index}]`);
    const path = stringField(item, "path", `compatibility approval sourceBinding.files[${index}]`);
    if (path.startsWith("/") || path.split("/").includes("..")) {
      throw new Error(`compatibility approval source path escapes repository: ${path}`);
    }
    return {
      path,
      sha256: shaField(item, "sha256", `compatibility approval sourceBinding.files[${index}]`),
    };
  });
  const approvedSourceBinding: SourceBinding = {
    files,
    aggregateSha256: shaField(
      source,
      "aggregateSha256",
      "compatibility approval sourceBinding",
    ),
  };
  if (
    new Set(files.map(({ path }) => path)).size !== files.length
    || stableJson(approvedSourceBinding) !== stableJson(observedSourceBinding)
  ) {
    throw new Error("compatibility approval source binding does not match checked-out files");
  }

  const runtimePolicies = record(
    root.runtimePolicyArtifacts,
    "compatibility approval runtimePolicyArtifacts",
  );
  exactKeys(runtimePolicies, ["mainBaseline", "fourMarketCatalog"], "compatibility approval runtimePolicyArtifacts");
  const readArtifactApproval = (key: "mainBaseline" | "fourMarketCatalog", expectedPath: string) => {
    const runtimePolicy = record(runtimePolicies[key], `compatibility approval runtimePolicyArtifacts.${key}`);
    exactKeys(runtimePolicy, ["path", "fileSha256", "artifactSha256", "sourceManifestSha256"], `compatibility approval runtimePolicyArtifacts.${key}`);
    const runtimePolicyPath = stringField(runtimePolicy, "path", `compatibility approval runtimePolicyArtifacts.${key}`);
    if (runtimePolicyPath !== expectedPath) throw new Error(`compatibility approval ${key} runtime policy path is not exact`);
    return {
      path: runtimePolicyPath,
      fileSha256: shaField(runtimePolicy, "fileSha256", `compatibility approval runtimePolicyArtifacts.${key}`),
      artifactSha256: shaField(runtimePolicy, "artifactSha256", `compatibility approval runtimePolicyArtifacts.${key}`),
      sourceManifestSha256: shaField(runtimePolicy, "sourceManifestSha256", `compatibility approval runtimePolicyArtifacts.${key}`),
    } as const;
  };
  return {
    path: COMPATIBILITY_APPROVAL_LABEL,
    fileSha256,
    approvalId: "operator-fixed-verifier-v1",
    sourceBinding: approvedSourceBinding,
    runtimePolicyArtifacts: {
      mainBaseline: readArtifactApproval("mainBaseline", MAIN_RUNTIME_POLICY_ARTIFACT_LABEL),
      fourMarketCatalog: readArtifactApproval("fourMarketCatalog", FOUR_MARKET_RUNTIME_POLICY_ARTIFACT_LABEL),
    },
  };
}

function add(
  gates: Gate[],
  name: string,
  pass: boolean,
  observed: unknown,
  expected: unknown,
): void {
  gates.push({ name, pass, observed, expected });
}

function unique(values: readonly string[]): string[] {
  return [...new Set(values)];
}

function operationAccount(
  instruction: CanonicalInstruction,
  label: string,
): string | null {
  return instruction.accounts.find((account) => account.label === label)?.address ?? null;
}

function canonicalFingerprint(instruction: CanonicalInstruction): string {
  return sha256(stableJson({
    programId: instruction.programId,
    dataBase64: instruction.dataBase64,
    accounts: instruction.accounts,
  }));
}

function frozenStrategy(
  candidate: PartnerStrategyCandidate,
): PartnerStrategyGraphIdentity {
  const identity = PARTNER_FOUR_MARKET_STRATEGIES.find(
    ({ id }) => id === candidate.id,
  );
  if (!identity || identity.reserve !== candidate.reserve) {
    throw new Error(
      `candidate ${candidate.id}/${candidate.reserve} is absent from the frozen four-market catalog`,
    );
  }
  return identity;
}

function web3FromCanonical(instruction: CanonicalInstruction): TransactionInstruction {
  return new TransactionInstruction({
    programId: new PublicKey(instruction.programId),
    keys: instruction.accounts.map((account) => ({
      pubkey: new PublicKey(account.address),
      isSigner: account.signer,
      isWritable: account.writable,
    })),
    data: Buffer.from(instruction.data),
  });
}

function routeForObservation(observation: ReserveGraphObservation): PartnerRouteSpec {
  const identity = frozenStrategy(observation.candidate);
  const expectedGraph = {
    reserve: identity.reserve,
    ...identity.graph,
  };
  if (stableJson(observation.graph) !== stableJson(expectedGraph)) {
    throw new Error(
      `live ${observation.candidate.id} graph does not match the frozen four-market catalog`,
    );
  }
  return {
    ...PARTNER_ROUTE,
    strategy: {
      reserve: identity.reserve,
      lendingMarket: identity.graph.lendingMarket,
      collateralFarm: identity.graph.reserveFarmState,
    },
  };
}

function derivePolicy(seed: bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [
      Buffer.from("smart_account"),
      Buffer.from("policy"),
      new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(),
      seedBytes,
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}

function catalogPolicyEntry(
  artifact: ReturnType<typeof loadRuntimePolicyArtifact>["artifact"],
  strategyId: PartnerStrategyCandidate["id"],
  operation: "deposit" | "withdraw",
) {
  const entry = artifact.policies.find(
    (candidate) => candidate.strategyId === strategyId && candidate.operation === operation,
  );
  if (!entry) {
    throw new Error(`four-market policy catalog omitted ${strategyId} ${operation}`);
  }
  return entry;
}

function compactU16Length(value: number): number {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff) {
    throw new Error(`compact-u16 value is out of range: ${value}`);
  }
  let remaining = value;
  let bytes = 0;
  do {
    remaining >>>= 7;
    bytes += 1;
  } while (remaining > 0);
  return bytes;
}

function v0MessageLength(message: MessageV0): number {
  const instructionBytes = message.compiledInstructions.reduce(
    (sum, instruction) => sum
      + 1
      + compactU16Length(instruction.accountKeyIndexes.length)
      + instruction.accountKeyIndexes.length
      + compactU16Length(instruction.data.length)
      + instruction.data.length,
    0,
  );
  const lookupBytes = message.addressTableLookups.reduce(
    (sum, lookup) => sum
      + 32
      + compactU16Length(lookup.writableIndexes.length)
      + lookup.writableIndexes.length
      + compactU16Length(lookup.readonlyIndexes.length)
      + lookup.readonlyIndexes.length,
    0,
  );
  return 1
    + 3
    + compactU16Length(message.staticAccountKeys.length)
    + (32 * message.staticAccountKeys.length)
    + 32
    + compactU16Length(message.compiledInstructions.length)
    + instructionBytes
    + compactU16Length(message.addressTableLookups.length)
    + lookupBytes;
}

function compilePacket(
  payer: string,
  blockhash: string,
  instructions: readonly TransactionInstruction[],
  lookupTables: readonly AddressLookupTableAccount[] = [],
): Readonly<{ report: PacketReport; transaction: VersionedTransaction }> {
  const message = new TransactionMessage({
    payerKey: new PublicKey(payer),
    recentBlockhash: blockhash,
    instructions: [...instructions],
  }).compileToV0Message([...lookupTables]);
  const transaction = new VersionedTransaction(message);
  // web3.js deliberately refuses to serialize packets larger than 1,232
  // bytes. Compatibility measurement must still report the exact oversized
  // length, so compute the wire size from shortvec(signature count), the
  // fixed 64-byte signature slots, and the canonical serialized message.
  const messageBytes = v0MessageLength(message);
  const packetBytes = compactU16Length(message.header.numRequiredSignatures)
    + (64 * message.header.numRequiredSignatures)
    + messageBytes;
  const serializedMessage = messageBytes <= SOLANA_PACKET_LIMIT
    ? message.serialize()
    : null;
  const accountKeys = message.getAccountKeys({
    addressLookupTableAccounts: [...lookupTables],
  });
  const tableByAddress = new Map(
    lookupTables.map((table) => [table.key.toBase58(), table]),
  );
  const signerAddresses = message.staticAccountKeys
    .slice(0, message.header.numRequiredSignatures)
    .map((key) => key.toBase58());
  return {
    transaction,
    report: {
      packetBytes,
      messageBytes,
      messageSha256: serializedMessage ? sha256(serializedMessage) : null,
      serializedMessageLengthMatches: serializedMessage
        ? serializedMessage.length === messageBytes
        : null,
      requiredSignatureCount: message.header.numRequiredSignatures,
      signerAddresses,
      staticAccountCount: message.staticAccountKeys.length,
      lookupTableCount: message.addressTableLookups.length,
      loadedWritableCount: message.addressTableLookups.reduce(
        (sum, lookup) => sum + lookup.writableIndexes.length,
        0,
      ),
      loadedReadonlyCount: message.addressTableLookups.reduce(
        (sum, lookup) => sum + lookup.readonlyIndexes.length,
        0,
      ),
      lookupTableAddresses: message.addressTableLookups.map((lookup) =>
        lookup.accountKey.toBase58()),
      lookupResolutions: message.addressTableLookups.map((lookup) => {
        const table = tableByAddress.get(lookup.accountKey.toBase58());
        return {
          address: lookup.accountKey.toBase58(),
          writable: [...lookup.writableIndexes].map((index) => ({
            index,
            address: table?.state.addresses[index]?.toBase58() ?? null,
          })),
          readonly: [...lookup.readonlyIndexes].map((index) => ({
            index,
            address: table?.state.addresses[index]?.toBase58() ?? null,
          })),
        };
      }),
      compiledInstructions: message.compiledInstructions.map((instruction) => ({
        programIdIndex: instruction.programIdIndex,
        programId: accountKeys.get(instruction.programIdIndex)?.toBase58() ?? null,
        accountKeyIndexes: [...instruction.accountKeyIndexes],
        accounts: instruction.accountKeyIndexes.map((index) => ({
          address: accountKeys.get(index)?.toBase58() ?? null,
          signer: message.isAccountSigner(index),
          writable: message.isAccountWritable(index),
        })),
        dataLength: instruction.data.length,
        dataSha256: sha256(instruction.data),
      })),
      withinLimit: packetBytes <= SOLANA_PACKET_LIMIT,
    },
  };
}

async function simulateUnsigned(
  connection: Connection,
  transaction: VersionedTransaction,
  minimumContextSlot: number,
): Promise<SimulationReport> {
  try {
    const simulation = await connection.simulateTransaction(transaction, {
      commitment: "confirmed",
      sigVerify: false,
      minContextSlot: minimumContextSlot,
    });
    const logs = simulation.value.logs ?? [];
    if (simulation.context.slot < minimumContextSlot) {
      return {
        status: "fail",
        contextSlot: simulation.context.slot,
        err: `simulation slot ${simulation.context.slot} predates minimum ${minimumContextSlot}`,
        unitsConsumed: simulation.value.unitsConsumed ?? null,
        logsSha256: sha256(logs.join("\n")),
        reasonCode: "simulation_context_predates_minimum",
      };
    }
    return {
      status: simulation.value.err === null ? "pass" : "fail",
      contextSlot: simulation.context.slot,
      err: simulation.value.err,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
      logsSha256: sha256(logs.join("\n")),
      reasonCode: simulation.value.err === null ? null : "simulation_error",
    };
  } catch (error) {
    return {
      status: "fail",
      contextSlot: null,
      err: error instanceof Error ? error.message : String(error),
      unitsConsumed: null,
      logsSha256: null,
      reasonCode: "rpc_simulation_error",
    };
  }
}

function snapshot(addressValue: string, account: Awaited<ReturnType<Connection["getAccountInfo"]>>): AccountSnapshot | null {
  if (!account) return null;
  return {
    address: addressValue,
    owner: account.owner.toBase58(),
    lamports: account.lamports,
    executable: account.executable,
    data: new Uint8Array(account.data),
  };
}

function decodeToken(snapshotValue: AccountSnapshot | null): Readonly<{
  mint: string;
  owner: string;
  amount: string;
}> | null {
  if (!snapshotValue || snapshotValue.owner !== PARTNER_ROUTE.programs.token) return null;
  try {
    const token = getTokenDecoder().decode(snapshotValue.data);
    return {
      mint: token.mint,
      owner: token.owner,
      amount: token.amount.toString(),
    };
  } catch {
    return null;
  }
}

function decodeMint(snapshotValue: AccountSnapshot | null): Readonly<{
  supply: string;
  decimals: number;
}> | null {
  if (!snapshotValue || snapshotValue.owner !== PARTNER_ROUTE.programs.token) return null;
  try {
    const mint = getMintDecoder().decode(snapshotValue.data);
    return { supply: mint.supply.toString(), decimals: mint.decimals };
  } catch {
    return null;
  }
}

function pendingSimulation(reasonCode: string): SimulationReport {
  return {
    status: "not_run_expected_bootstrap_missing",
    contextSlot: null,
    err: null,
    unitsConsumed: null,
    logsSha256: null,
    reasonCode,
  };
}

function notRunSimulation(
  status: Extract<SimulationReport["status"],
    | "not_run_economic_precondition"
    | "not_run_invalid_support_state"
    | "not_run_policy_missing">,
  reasonCode: string,
): SimulationReport {
  return {
    ...pendingSimulation(reasonCode),
    status,
  };
}

function policyConstraint(
  instruction: CanonicalInstruction,
  indexes: readonly number[],
) {
  return {
    programId: new PublicKey(instruction.programId),
    accountConstraints: indexes.map((accountIndex) => ({
      accountIndex,
      accountConstraint: {
        __kind: "Pubkey" as const,
        fields: [[new PublicKey(instruction.accounts[accountIndex]!.address)]],
      },
      owner: null,
    })),
    dataConstraints: [
      {
        dataOffset: 0,
        dataValue: {
          __kind: "U8Slice" as const,
          fields: [instruction.data.subarray(0, 8)],
        },
        operator: 0,
      },
      {
        dataOffset: 8,
        dataValue: { __kind: "U64Le" as const, fields: [0] },
        operator: 2,
      },
      {
        dataOffset: 8,
        dataValue: {
          __kind: "U64Le" as const,
          fields: [Number(PARTNER_ROUTE.asset.maxManagerOperationRaw)],
        },
        operator: 5,
      },
      {
        dataOffset: 16,
        dataValue: {
          __kind: "U8Slice" as const,
          fields: [instruction.data.subarray(16)],
        },
        operator: 0,
      },
    ],
  };
}

function policyPayloadBytes(
  instructions: readonly CanonicalInstruction[],
  indexes: readonly number[],
): Uint8Array {
  const payload = {
    accountIndex: PARTNER_ROUTE.squads.vaultIndex,
    instructionsConstraints: instructions.map((instruction) =>
      policyConstraint(instruction, indexes)),
    preHook: null,
    postHook: null,
    spendingLimits: [],
  };
  return PROGRAM_INTERACTION_PAYLOAD_CODEC.serialize(payload)[0];
}

function canonicalReport(instruction: CanonicalInstruction) {
  return {
    programId: instruction.programId,
    dataLength: instruction.dataLength,
    dataBase64: instruction.dataBase64,
    dataSha256: instruction.dataSha256,
    fingerprintSha256: canonicalFingerprint(instruction),
    accounts: instruction.accounts,
  };
}

function verifyCompatibilityWrapper(
  direction: "deposit" | "withdraw",
  wrapper: ReturnType<typeof buildManagerWrapperForCompatibility>,
  packet: PacketReport,
): readonly Gate[] {
  const gates: Gate[] = [];
  const expectedCompute = ComputeBudgetProgram.setComputeUnitLimit({
    units: MANAGER_COMPUTE_UNIT_LIMIT,
  });
  const expectedHeap = ComputeBudgetProgram.requestHeapFrame({
    bytes: MANAGER_HEAP_FRAME_BYTES,
  });
  const actualMetas = wrapper.instruction.keys.map((meta) => ({
    address: meta.pubkey.toBase58(),
    signer: meta.isSigner,
    writable: meta.isWritable,
  }));
  add(gates, `${direction} wrapper program exact`, wrapper.instruction.programId.toBase58() === PARTNER_ROUTE.squads.program, wrapper.instruction.programId.toBase58(), PARTNER_ROUTE.squads.program);
  add(gates, `${direction} wrapper metas exact`, stableJson(actualMetas) === stableJson(wrapper.expectedAccounts), actualMetas, wrapper.expectedAccounts);
  add(gates, `${direction} wrapper data hash self-consistent`, sha256(wrapper.instruction.data) === wrapper.dataSha256, sha256(wrapper.instruction.data), wrapper.dataSha256);
  add(gates, `${direction} compiled instruction count exact`, packet.compiledInstructions.length === 3, packet.compiledInstructions.length, 3);
  const compiledCompute = packet.compiledInstructions[0] ?? null;
  const compiledHeap = packet.compiledInstructions[1] ?? null;
  add(gates, `${direction} compiled compute-budget instruction exact`, compiledCompute?.programId === ComputeBudgetProgram.programId.toBase58() && compiledCompute.dataSha256 === sha256(expectedCompute.data) && compiledCompute.accounts.length === 0, compiledCompute, { programId: ComputeBudgetProgram.programId.toBase58(), accounts: [], dataSha256: sha256(expectedCompute.data) });
  add(gates, `${direction} compiled heap-frame instruction exact`, compiledHeap?.programId === ComputeBudgetProgram.programId.toBase58() && compiledHeap.dataSha256 === sha256(expectedHeap.data) && compiledHeap.accounts.length === 0, compiledHeap, { programId: ComputeBudgetProgram.programId.toBase58(), accounts: [], dataSha256: sha256(expectedHeap.data) });
  const compiled = packet.compiledInstructions[2] ?? null;
  const expectedCompiledMetas = wrapper.expectedAccounts.map((meta) => ({
    ...meta,
    // The sole transaction payer is always writable in the compiled message,
    // even though the raw Squads instruction correctly marks it readonly.
    writable: meta.address === PARTNER_ROUTE.squads.guardian
      ? true
      : meta.writable,
  }));
  add(gates, `${direction} compiled wrapper program exact`, compiled?.programId === PARTNER_ROUTE.squads.program, compiled?.programId ?? null, PARTNER_ROUTE.squads.program);
  add(gates, `${direction} compiled wrapper account order exact`, stableJson(compiled?.accounts.map(({ address: account }) => account) ?? []) === stableJson(wrapper.expectedAccounts.map(({ address: account }) => account)), compiled?.accounts.map(({ address: account }) => account) ?? [], wrapper.expectedAccounts.map(({ address: account }) => account));
  add(gates, `${direction} compiled wrapper effective meta roles exact`, stableJson(compiled?.accounts ?? []) === stableJson(expectedCompiledMetas), compiled?.accounts ?? [], expectedCompiledMetas);
  add(gates, `${direction} compiled wrapper data exact`, compiled?.dataSha256 === wrapper.dataSha256, compiled?.dataSha256 ?? null, wrapper.dataSha256);
  add(gates, `${direction} canonical message hash available`, packet.messageSha256 !== null && packet.serializedMessageLengthMatches === true, { messageSha256: packet.messageSha256, serializedMessageLengthMatches: packet.serializedMessageLengthMatches }, "serialized canonical v0 message and exact measured length");
  const unresolved = packet.lookupResolutions.flatMap((lookup) => [
    ...lookup.writable,
    ...lookup.readonly,
  ]).filter(({ address: account }) => account === null);
  add(gates, `${direction} every ALT lookup index resolves`, unresolved.length === 0, unresolved, []);
  const resolved = packet.lookupResolutions.flatMap((lookup) => [
    ...lookup.writable,
    ...lookup.readonly,
  ]).map(({ address: account }) => account);
  const expectedWrapperAddresses = new Set(wrapper.expectedAccounts.map(({ address: account }) => account));
  add(gates, `${direction} ALT loads only exact wrapper accounts`, resolved.every((account) => account !== null && expectedWrapperAddresses.has(account)), resolved, "subset of exact wrapper accounts");
  add(gates, `${direction} ALT lookup addresses are unique`, resolved.length === new Set(resolved).size, resolved, "unique resolved addresses");
  add(gates, `${direction} ALT lookup counts are self-consistent`, resolved.length === packet.loadedWritableCount + packet.loadedReadonlyCount, resolved.length, packet.loadedWritableCount + packet.loadedReadonlyCount);
  return gates;
}

async function buildStrategy(
  observation: ReserveGraphObservation,
): Promise<Readonly<{ built: BuiltStrategy; gates: readonly Gate[] }>> {
  const gates: Gate[] = [];
  const identity = frozenStrategy(observation.candidate);
  const route = routeForObservation(observation);
  const accounts = await deriveVoltrAccountsForStrategy(route, observation.candidate.reserve);
  const manager = createNoopSigner(route.squads.manager);
  const admin = createNoopSigner(route.setupAdmin);
  const vault = createNoopSigner(route.vault);
  const builder = await createVoltrRouteBuilder(route, observation.graph);
  const rebuilt = await createVoltrRouteBuilder(route, observation.graph);
  const [setManager, initialize, restoreManager, deposit, withdraw] = await Promise.all([
    builder.setup.setManagerToAdmin({ payer: admin, admin, vault }),
    builder.setup.initializeStrategyAsAdmin({ payer: admin, admin, vault }),
    builder.setup.restoreManager({ payer: admin, admin, vault }),
    builder.strategy.deposit(manager, route.asset.proofAmountRaw),
    builder.strategy.withdraw(manager, route.asset.proofAmountRaw),
  ]);
  const [initializeAgain, depositAgain, withdrawAgain] = await Promise.all([
    rebuilt.setup.initializeStrategyAsAdmin({ payer: admin, admin, vault }),
    rebuilt.strategy.deposit(manager, route.asset.proofAmountRaw),
    rebuilt.strategy.withdraw(manager, route.asset.proofAmountRaw),
  ]);
  const strategyAssetAta = operationAccount(deposit.canonical, "vaultStrategyAssetAta");
  if (!strategyAssetAta) throw new Error("canonical deposit omitted vaultStrategyAssetAta");

  add(gates, "live graph matches frozen catalog", stableJson(observation.graph) === stableJson({ reserve: identity.reserve, ...identity.graph }), observation.graph, { reserve: identity.reserve, ...identity.graph });
  add(gates, "derived strategy auth matches frozen catalog", accounts.strategyAuth === identity.voltr.strategyAuth, accounts.strategyAuth, identity.voltr.strategyAuth);
  add(gates, "derived strategy receipt matches frozen catalog", accounts.strategyInitReceipt === identity.voltr.strategyInitReceipt, accounts.strategyInitReceipt, identity.voltr.strategyInitReceipt);
  add(gates, "derived strategy asset ATA matches frozen catalog", strategyAssetAta === identity.voltr.strategyAssetAta, strategyAssetAta, identity.voltr.strategyAssetAta);
  for (const key of [
    "protocol",
    "idleAuth",
    "idleAta",
    "lpMint",
    "lpMintAuth",
    "adaptorAddReceipt",
  ] as const) {
    add(
      gates,
      `derived common Voltr ${key} matches frozen catalog`,
      accounts[key] === PARTNER_FOUR_MARKET_ROUTE.commonVoltr[key],
      accounts[key],
      PARTNER_FOUR_MARKET_ROUTE.commonVoltr[key],
    );
  }

  add(gates, "initialize program", initialize.canonical.programId === route.programs.voltrVault, initialize.canonical.programId, route.programs.voltrVault);
  add(gates, "initialize account count", initialize.canonical.accounts.length === 20, initialize.canonical.accounts.length, 20);
  add(gates, "initialize data length", initialize.canonical.dataLength === 22, initialize.canonical.dataLength, 22);
  add(gates, "deposit program", deposit.canonical.programId === route.programs.voltrVault, deposit.canonical.programId, route.programs.voltrVault);
  add(gates, "deposit account count", deposit.canonical.accounts.length === 31, deposit.canonical.accounts.length, 31);
  add(gates, "deposit data length", deposit.canonical.dataLength === 30, deposit.canonical.dataLength, 30);
  add(gates, "withdraw program", withdraw.canonical.programId === route.programs.voltrVault, withdraw.canonical.programId, route.programs.voltrVault);
  add(gates, "withdraw account count", withdraw.canonical.accounts.length === 28, withdraw.canonical.accounts.length, 28);
  add(gates, "withdraw data length", withdraw.canonical.dataLength === 30, withdraw.canonical.dataLength, 30);
  for (const [operation, instruction] of [["initialize", initialize.canonical], ["deposit", deposit.canonical], ["withdraw", withdraw.canonical]] as const) {
    add(gates, `${operation} selected strategy`, operationAccount(instruction, "strategy") === observation.candidate.reserve, operationAccount(instruction, "strategy"), observation.candidate.reserve);
    add(gates, `${operation} selected reserve`, operationAccount(instruction, "reserve") === observation.candidate.reserve, operationAccount(instruction, "reserve"), observation.candidate.reserve);
    add(gates, `${operation} selected market`, operationAccount(instruction, "lendingMarket") === observation.graph.lendingMarket, operationAccount(instruction, "lendingMarket"), observation.graph.lendingMarket);
  }
  add(gates, "initialize deterministic rebuild", canonicalFingerprint(initialize.canonical) === canonicalFingerprint(initializeAgain.canonical), canonicalFingerprint(initializeAgain.canonical), canonicalFingerprint(initialize.canonical));
  add(gates, "deposit deterministic rebuild", canonicalFingerprint(deposit.canonical) === canonicalFingerprint(depositAgain.canonical), canonicalFingerprint(depositAgain.canonical), canonicalFingerprint(deposit.canonical));
  add(gates, "withdraw deterministic rebuild", canonicalFingerprint(withdraw.canonical) === canonicalFingerprint(withdrawAgain.canonical), canonicalFingerprint(withdrawAgain.canonical), canonicalFingerprint(withdraw.canonical));

  return {
    built: {
      candidate: observation.candidate,
      identity,
      route,
      observation,
      accounts,
      strategyAssetAta,
      initialize: initialize.canonical,
      deposit: deposit.canonical,
      withdraw: withdraw.canonical,
      setupInstructions: [setManager.raw, initialize.raw, restoreManager.raw].map(toWeb3Instruction),
    },
    gates,
  };
}

async function loadSnapshots(
  connection: Connection,
  addresses: readonly string[],
  minimumContextSlot: number,
): Promise<Readonly<{
  contextSlot: number;
  byAddress: ReadonlyMap<string, AccountSnapshot | null>;
}>> {
  const response = await connection.getMultipleAccountsInfoAndContext(
    addresses.map((value) => new PublicKey(value)),
    { commitment: "confirmed", minContextSlot: minimumContextSlot },
  );
  if (response.context.slot < minimumContextSlot) {
    throw new Error(
      `support-state slot ${response.context.slot} predates reserve slot ${minimumContextSlot}`,
    );
  }
  if (response.value.length !== addresses.length) {
    throw new Error(
      `support-state batch returned ${response.value.length} rows for ${addresses.length} addresses`,
    );
  }
  return {
    contextSlot: response.context.slot,
    byAddress: new Map(addresses.map((value, index) => [
      value,
      snapshot(value, response.value[index] ?? null),
    ])),
  };
}

async function loadAlt(
  connection: Connection,
  minimumContextSlot: number,
): Promise<Readonly<{
  account: AddressLookupTableAccount;
  identity: AltIdentity;
}>> {
  const response = await connection.getAddressLookupTable(
    new PublicKey(PARTNER_ROUTE.lookupTable.address),
    { commitment: "confirmed", minContextSlot: minimumContextSlot },
  );
  if (!response.value) {
    throw new Error(`lookup table ${PARTNER_ROUTE.lookupTable.address} is absent`);
  }
  if (response.context.slot < minimumContextSlot) {
    throw new Error(
      `lookup table slot ${response.context.slot} predates support-state slot ${minimumContextSlot}`,
    );
  }
  const account = response.value;
  const orderedAddresses = account.state.addresses.map((key) => key.toBase58());
  return {
    account,
    identity: {
      address: account.key.toBase58(),
      authority: account.state.authority?.toBase58() ?? null,
      deactivationSlot: account.state.deactivationSlot.toString(),
      lastExtendedSlot: account.state.lastExtendedSlot,
      addressCount: orderedAddresses.length,
      orderedAddressesSha256: sha256(orderedAddresses.join("\n")),
      contextSlot: response.context.slot,
    },
  };
}

function supportAddresses(built: readonly BuiltStrategy[]): string[] {
  return unique([
    PARTNER_ROUTE.programs.token,
    PARTNER_ROUTE.programs.associatedToken,
    PARTNER_ROUTE.programs.system,
    PARTNER_SCOPE_PROGRAM,
    PARTNER_SCOPE_ORACLE_MAPPINGS,
    ...built.flatMap(({ observation, accounts, strategyAssetAta }) => [
      observation.graph.reserve,
      observation.graph.lendingMarket,
      observation.graph.lendingMarketAuthority,
      observation.graph.reserveLiquiditySupply,
      observation.graph.reserveCollateralMint,
      observation.graph.reserveCollateralSupplyVault,
      observation.graph.scope,
      observation.graph.reserveFarmState,
      accounts.strategyInitReceipt,
      observation.graph.userMetadata,
      observation.graph.obligation,
      observation.graph.obligationFarm,
      strategyAssetAta,
      accounts.idleAta,
    ]),
  ]);
}

function inspectSupportState(
  built: BuiltStrategy,
  byAddress: ReadonlyMap<string, AccountSnapshot | null>,
  instructionGates: readonly Gate[],
): Readonly<{
  bootstrapState: BootstrapState;
  gates: readonly Gate[];
  support: unknown;
  economic: Readonly<{
    idleRaw: bigint | null;
    positionRaw: bigint | null;
  }>;
}> {
  const { observation, accounts, route, strategyAssetAta, identity } = built;
  const gates: Gate[] = [...instructionGates];
  add(gates, "reserve status active", observation.reserveStatus === 0, observation.reserveStatus, 0);
  // Kamino persists this one-byte refresh marker and it can legitimately flip
  // between reads without changing route compatibility. Economic freshness is
  // owned by Earn's confirmed observation pipeline; this probe only requires
  // the native flag to decode to its closed 0/1 domain and records the value.
  add(gates, "reserve last-update stale flag decodes", observation.reserveLastUpdateStale === 0 || observation.reserveLastUpdateStale === 1, observation.reserveLastUpdateStale, "0|1; Earn observer owns economic freshness");
  add(gates, "reserve liquidity mint", observation.liquidityMint === route.asset.mint, observation.liquidityMint, route.asset.mint);
  add(gates, "reserve liquidity token program", observation.liquidityTokenProgram === route.asset.tokenProgram, observation.liquidityTokenProgram, route.asset.tokenProgram);
  add(gates, "reserve liquidity decimals", observation.liquidityMintDecimals === route.asset.decimals, observation.liquidityMintDecimals, route.asset.decimals);
  add(gates, "reserve has collateral farm", observation.hasCollateralFarm, observation.hasCollateralFarm, true);

  const reserveSnapshot = byAddress.get(observation.graph.reserve) ?? null;
  const market = byAddress.get(observation.graph.lendingMarket) ?? null;
  const lendingMarketAuthority = byAddress.get(observation.graph.lendingMarketAuthority) ?? null;
  const reserveLiquiditySupply = byAddress.get(observation.graph.reserveLiquiditySupply) ?? null;
  const reserveCollateralMint = byAddress.get(observation.graph.reserveCollateralMint) ?? null;
  const reserveCollateralSupply = byAddress.get(observation.graph.reserveCollateralSupplyVault) ?? null;
  const scope = byAddress.get(observation.graph.scope) ?? null;
  const oracleMappings = byAddress.get(PARTNER_SCOPE_ORACLE_MAPPINGS) ?? null;
  const farm = byAddress.get(observation.graph.reserveFarmState) ?? null;
  const idleAta = byAddress.get(accounts.idleAta) ?? null;
  const liquidityToken = decodeToken(reserveLiquiditySupply);
  const collateralMint = decodeMint(reserveCollateralMint);
  const collateralSupply = decodeToken(reserveCollateralSupply);
  const idleToken = decodeToken(idleAta);

  let decodedReserve: Reserve | null = null;
  let reserveDecodeError: string | null = null;
  try {
    if (reserveSnapshot?.owner !== route.programs.klend) {
      throw new Error(`owner ${reserveSnapshot?.owner ?? "absent"} is not KLend`);
    }
    decodedReserve = Reserve.decode(Buffer.from(reserveSnapshot.data));
  } catch (error) {
    reserveDecodeError = error instanceof Error ? error.message : String(error);
  }
  const supportGraph = decodedReserve
    ? {
        reserve: identity.reserve,
        lendingMarket: new PublicKey(decodedReserve.lendingMarket).toBase58(),
        reserveLiquiditySupply: new PublicKey(decodedReserve.liquidity.supplyVault).toBase58(),
        reserveCollateralMint: new PublicKey(decodedReserve.collateral.mintPubkey).toBase58(),
        reserveCollateralSupplyVault: new PublicKey(decodedReserve.collateral.supplyVault).toBase58(),
        scope: new PublicKey(decodedReserve.config.tokenInfo.scopeConfiguration.priceFeed).toBase58(),
        reserveFarmState: new PublicKey(decodedReserve.farmCollateral).toBase58(),
      }
    : null;
  const expectedSupportGraph = {
    reserve: identity.reserve,
    lendingMarket: identity.graph.lendingMarket,
    reserveLiquiditySupply: identity.graph.reserveLiquiditySupply,
    reserveCollateralMint: identity.graph.reserveCollateralMint,
    reserveCollateralSupplyVault: identity.graph.reserveCollateralSupplyVault,
    scope: identity.graph.scope,
    reserveFarmState: identity.graph.reserveFarmState,
  };
  add(gates, "support reserve exists, is KLend-owned, and decodes", decodedReserve !== null && reserveSnapshot?.executable === false, reserveDecodeError ?? { owner: reserveSnapshot?.owner ?? null, executable: reserveSnapshot?.executable ?? null }, { owner: route.programs.klend, executable: false, decoded: true });
  add(gates, "support reserve immutable graph matches frozen catalog", stableJson(supportGraph) === stableJson(expectedSupportGraph), supportGraph, expectedSupportGraph);
  add(gates, "support reserve remains active", decodedReserve?.config.status === 0, decodedReserve?.config.status ?? null, 0);
  add(gates, "support reserve permits collateral-token use", decodedReserve?.config.blockCtokenUsage === 0, decodedReserve?.config.blockCtokenUsage ?? null, 0);
  add(gates, "support reserve remains classic USDC", decodedReserve !== null
    && new PublicKey(decodedReserve.liquidity.mintPubkey).toBase58() === route.asset.mint
    && new PublicKey(decodedReserve.liquidity.tokenProgram).toBase58() === route.asset.tokenProgram
    && decodedReserve.liquidity.mintDecimals.toNumber() === route.asset.decimals, decodedReserve ? {
      mint: new PublicKey(decodedReserve.liquidity.mintPubkey).toBase58(),
      tokenProgram: new PublicKey(decodedReserve.liquidity.tokenProgram).toBase58(),
      decimals: decodedReserve.liquidity.mintDecimals.toNumber(),
    } : null, { mint: route.asset.mint, tokenProgram: route.asset.tokenProgram, decimals: route.asset.decimals });

  let decodedMarket: LendingMarket | null = null;
  let marketDecodeError: string | null = null;
  try {
    if (market?.owner !== route.programs.klend || market.executable) {
      throw new Error(`owner/executable ${market?.owner ?? "absent"}/${market?.executable ?? "absent"} is not a KLend data account`);
    }
    decodedMarket = LendingMarket.decode(Buffer.from(market.data));
  } catch (error) {
    marketDecodeError = error instanceof Error ? error.message : String(error);
  }
  const [derivedLendingMarketAuthority, derivedLendingMarketAuthorityBump] = PublicKey.findProgramAddressSync(
    [Buffer.from("lma"), new PublicKey(identity.graph.lendingMarket).toBuffer()],
    new PublicKey(route.programs.klend),
  );
  add(gates, "lending market exists, is KLend-owned, and decodes", decodedMarket !== null, marketDecodeError ?? market?.owner ?? null, "decoded KLend LendingMarket");
  add(gates, "lending market authority derivation matches frozen catalog", derivedLendingMarketAuthority.toBase58() === identity.graph.lendingMarketAuthority, derivedLendingMarketAuthority.toBase58(), identity.graph.lendingMarketAuthority);
  add(gates, "lending market authority bump matches decoded market", decodedMarket?.bumpSeed.toNumber() === derivedLendingMarketAuthorityBump, decodedMarket?.bumpSeed.toString() ?? null, derivedLendingMarketAuthorityBump);
  add(gates, "lending market emergency mode disabled", decodedMarket?.emergencyMode === 0, decodedMarket?.emergencyMode ?? null, 0);

  add(gates, "reserve liquidity supply is classic USDC", liquidityToken?.mint === route.asset.mint, liquidityToken, { mint: route.asset.mint, tokenProgram: route.programs.token });
  add(gates, "reserve liquidity supply authority is exact market authority", liquidityToken?.owner === identity.graph.lendingMarketAuthority, liquidityToken?.owner ?? null, identity.graph.lendingMarketAuthority);
  add(gates, "reserve collateral mint decodes under classic Token", collateralMint !== null, collateralMint, "classic SPL mint");
  add(gates, "reserve collateral supply matches collateral mint", collateralSupply?.mint === observation.graph.reserveCollateralMint, collateralSupply, { mint: observation.graph.reserveCollateralMint });
  add(gates, "reserve collateral supply authority is exact market authority", collateralSupply?.owner === identity.graph.lendingMarketAuthority, collateralSupply?.owner ?? null, identity.graph.lendingMarketAuthority);
  add(gates, "vault idle ATA is classic USDC under frozen idle authority", idleToken?.mint === route.asset.mint && idleToken.owner === PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAuth, idleToken, { mint: route.asset.mint, owner: PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAuth });

  let decodedScope: OraclePrices | null = null;
  let scopeDecodeError: string | null = null;
  try {
    if (scope?.owner !== PARTNER_SCOPE_PROGRAM || scope.executable) {
      throw new Error(`owner/executable ${scope?.owner ?? "absent"}/${scope?.executable ?? "absent"} is not a Scope data account`);
    }
    decodedScope = OraclePrices.decode(Buffer.from(scope.data));
  } catch (error) {
    scopeDecodeError = error instanceof Error ? error.message : String(error);
  }
  add(gates, "Scope OraclePrices exists, has exact owner, and decodes", decodedScope !== null, scopeDecodeError ?? scope?.owner ?? null, { owner: PARTNER_SCOPE_PROGRAM, decoded: "OraclePrices" });
  add(gates, "Scope OraclePrices binds exact mappings account", decodedScope?.oracleMappings === PARTNER_SCOPE_ORACLE_MAPPINGS, decodedScope?.oracleMappings ?? null, PARTNER_SCOPE_ORACLE_MAPPINGS);

  let decodedMappings: OracleMappings | null = null;
  let mappingsDecodeError: string | null = null;
  try {
    if (
      oracleMappings?.owner !== PARTNER_SCOPE_PROGRAM
      || oracleMappings.executable
    ) {
      throw new Error(
        `owner/executable ${oracleMappings?.owner ?? "absent"}/${oracleMappings?.executable ?? "absent"} is not a Scope data account`,
      );
    }
    decodedMappings = OracleMappings.decode(Buffer.from(oracleMappings.data));
  } catch (error) {
    mappingsDecodeError = error instanceof Error ? error.message : String(error);
  }
  add(
    gates,
    "Scope OracleMappings exists, has exact owner, and decodes",
    decodedMappings !== null,
    mappingsDecodeError ?? (oracleMappings
      ? { owner: oracleMappings.owner, executable: oracleMappings.executable }
      : null),
    { address: PARTNER_SCOPE_ORACLE_MAPPINGS, owner: PARTNER_SCOPE_PROGRAM, executable: false, decoded: "OracleMappings" },
  );

  let decodedFarm: FarmState | null = null;
  let farmDecodeError: string | null = null;
  try {
    if (farm?.owner !== route.programs.farms || farm.executable) {
      throw new Error(`owner/executable ${farm?.owner ?? "absent"}/${farm?.executable ?? "absent"} is not a Farms data account`);
    }
    decodedFarm = FarmState.decode(Buffer.from(farm.data));
  } catch (error) {
    farmDecodeError = error instanceof Error ? error.message : String(error);
  }
  add(gates, "collateral farm exists, is Farms-owned, and decodes", decodedFarm !== null, farmDecodeError ?? farm?.owner ?? null, "decoded Farms FarmState");
  add(gates, "collateral farm is the delegated KLend layout", decodedFarm?.isFarmDelegated === 1, decodedFarm?.isFarmDelegated ?? null, 1);
  add(gates, "delegated collateral farm authority is exact market authority", decodedFarm?.delegateAuthority === identity.graph.lendingMarketAuthority, decodedFarm?.delegateAuthority ?? null, identity.graph.lendingMarketAuthority);
  add(gates, "delegated collateral farm leaves token fields disabled", decodedFarm?.token.mint === route.programs.system && decodedFarm.token.tokenProgram === route.programs.system && decodedFarm.token.decimals.isZero(), decodedFarm ? { mint: decodedFarm.token.mint, tokenProgram: decodedFarm.token.tokenProgram, decimals: decodedFarm.token.decimals.toString() } : null, { mint: route.programs.system, tokenProgram: route.programs.system, decimals: "0" });
  add(gates, "collateral farm accepts deposits", decodedFarm?.isFarmFrozen === 0, decodedFarm?.isFarmFrozen ?? null, 0);
  add(gates, "delegated collateral farm has no warmup or withdrawal cooldown", decodedFarm?.depositWarmupPeriod === 0 && decodedFarm.withdrawalCooldownPeriod === 0, decodedFarm ? { depositWarmupPeriod: decodedFarm.depositWarmupPeriod, withdrawalCooldownPeriod: decodedFarm.withdrawalCooldownPeriod } : null, { depositWarmupPeriod: 0, withdrawalCooldownPeriod: 0 });

  const receipt = byAddress.get(accounts.strategyInitReceipt) ?? null;
  const metadata = byAddress.get(observation.graph.userMetadata) ?? null;
  const obligation = byAddress.get(observation.graph.obligation) ?? null;
  const obligationFarm = byAddress.get(observation.graph.obligationFarm) ?? null;
  const strategyAta = byAddress.get(strategyAssetAta) ?? null;
  let positionRaw: bigint | null = receipt === null ? 0n : null;
  if (receipt !== null) {
    try {
      positionRaw = getStrategyInitReceiptDecoder().decode(receipt.data).positionValue;
    } catch {
      positionRaw = null;
    }
  }
  const presence = {
    strategyReceipt: receipt !== null,
    userMetadata: metadata !== null,
    obligation: obligation !== null,
    obligationFarm: obligationFarm !== null,
    strategyAssetAta: strategyAta !== null,
  };
  const allAbsent = Object.values(presence).every((present) => !present);
  let bootstrapState: BootstrapState;
  if (allAbsent) {
    bootstrapState = "PENDING_EXPECTED_BOOTSTRAP";
    add(gates, "bootstrap state is cleanly absent or valid", true, presence, "all absent before atomic bootstrap");
  } else if (receipt !== null) {
    const bootstrapGates = verifyStrategyBootstrap({
      route,
      accounts,
      graph: observation.graph,
      strategyReceipt: receipt,
      userMetadata: metadata,
      obligation,
      obligationFarm,
    });
    gates.push(...bootstrapGates.map((gate) => ({
      ...gate,
      name: `initialized strategy: ${gate.name}`,
    })));
    const strategyToken = decodeToken(strategyAta);
    add(gates, "strategy USDC ATA decodes", strategyToken !== null, strategyToken, "classic SPL token account");
    add(gates, "strategy USDC ATA mint", strategyToken?.mint === route.asset.mint, strategyToken?.mint ?? null, route.asset.mint);
    add(gates, "strategy USDC ATA authority", strategyToken?.owner === accounts.strategyAuth, strategyToken?.owner ?? null, accounts.strategyAuth);
    const valid = bootstrapGates.every(({ pass }) => pass)
      && strategyToken?.mint === route.asset.mint
      && strategyToken.owner === accounts.strategyAuth;
    bootstrapState = valid
      ? "READY_FOR_MANAGER_SIMULATION"
      : "INVALID_EXISTING_STATE";
    add(gates, "bootstrap state is cleanly absent or valid", valid, presence, "exact initialized state");
  } else {
    bootstrapState = "INVALID_EXISTING_STATE";
    add(gates, "bootstrap state is cleanly absent or valid", false, presence, "all absent or exact initialized state");
  }

  return {
    bootstrapState,
    gates,
    support: {
      market: market ? { owner: market.owner, dataSha256: sha256(market.data) } : null,
      decodedMarket: decodedMarket ? {
        version: decodedMarket.version.toString(),
        bumpSeed: decodedMarket.bumpSeed.toString(),
        emergencyMode: decodedMarket.emergencyMode,
        borrowDisabled: decodedMarket.borrowDisabled,
        immutable: decodedMarket.immutable,
      } : null,
      lendingMarketAuthority: {
        address: identity.graph.lendingMarketAuthority,
        bump: derivedLendingMarketAuthorityBump,
        accountExists: lendingMarketAuthority !== null,
      },
      reserve: reserveSnapshot ? {
        owner: reserveSnapshot.owner,
        dataSha256: sha256(reserveSnapshot.data),
        graph: supportGraph,
        blockCtokenUsage: decodedReserve?.config.blockCtokenUsage ?? null,
      } : null,
      reserveLiquiditySupply: liquidityToken,
      reserveCollateralMint: collateralMint,
      reserveCollateralSupply,
      idleAta: idleToken,
      scope: scope ? {
        owner: scope.owner,
        dataSha256: sha256(scope.data),
        oracleMappings: decodedScope?.oracleMappings ?? null,
      } : null,
      oracleMappings: oracleMappings ? {
        owner: oracleMappings.owner,
        executable: oracleMappings.executable,
        dataSha256: sha256(oracleMappings.data),
        decoded: decodedMappings !== null,
      } : null,
      collateralFarm: farm ? {
        owner: farm.owner,
        dataSha256: sha256(farm.data),
        tokenMint: decodedFarm?.token.mint ?? null,
        tokenProgram: decodedFarm?.token.tokenProgram ?? null,
        isFarmFrozen: decodedFarm?.isFarmFrozen ?? null,
        isFarmDelegated: decodedFarm?.isFarmDelegated ?? null,
        delegateAuthority: decodedFarm?.delegateAuthority ?? null,
        strategyId: decodedFarm?.strategyId ?? null,
        vaultId: decodedFarm?.vaultId ?? null,
        globalConfig: decodedFarm?.globalConfig ?? null,
        farmVault: decodedFarm?.farmVault ?? null,
        farmVaultsAuthority: decodedFarm?.farmVaultsAuthority ?? null,
        depositWarmupPeriod: decodedFarm?.depositWarmupPeriod ?? null,
        withdrawalCooldownPeriod: decodedFarm?.withdrawalCooldownPeriod ?? null,
      } : null,
      bootstrapPresence: presence,
    },
    economic: {
      idleRaw: idleToken?.amount === undefined ? null : BigInt(idleToken.amount),
      positionRaw,
    },
  };
}

function policyShape(
  built: readonly BuiltStrategy[],
  loaded: ReturnType<typeof loadRuntimePolicyArtifact>,
) {
  const gates: Gate[] = [];
  const baselineDeposit = loaded.artifact.policies.find(({ operation }) => operation === "deposit");
  const baselineWithdraw = loaded.artifact.policies.find(({ operation }) => operation === "withdraw");
  if (!baselineDeposit || !baselineWithdraw) {
    throw new Error("verified runtime artifact omitted a baseline policy direction");
  }
  const complete = built.length === PARTNER_STRATEGY_CANDIDATES.length;
  add(gates, "all four canonical graphs available for topology measurement", complete, built.map(({ candidate }) => candidate.id), PARTNER_STRATEGY_CANDIDATES.map(({ id }) => id));
  if (!complete) {
    return {
      verdict: "FOUR_MARKET_POLICY_TOPOLOGY_UNMEASURED",
      acceptedTopology: "eight-physical-policies",
      rejectedTopology: "two-policy-cartesian-graph",
      baselineArtifactPath: MAIN_RUNTIME_POLICY_ARTIFACT_LABEL,
      baselineArtifactFileSha256: loaded.fileSha256,
      baselineArtifactCanonicalSha256: loaded.artifact.artifactSha256,
      baselineSourceManifestSha256: loaded.artifact.sourceManifestSha256,
      baselineRouteSpecSha256: loaded.artifact.routeSpecSha256,
      gates,
    } as const;
  }
  const depositInstructions = built.map(({ deposit }) => deposit);
  const withdrawInstructions = built.map(({ withdraw }) => withdraw);
  for (const [label, values] of [
    ["reserve", built.map(({ identity }) => identity.reserve)],
    ["lending market", built.map(({ identity }) => identity.graph.lendingMarket)],
    ["collateral farm", built.map(({ identity }) => identity.graph.reserveFarmState)],
    ["strategy auth", built.map(({ identity }) => identity.voltr.strategyAuth)],
    ["strategy receipt", built.map(({ identity }) => identity.voltr.strategyInitReceipt)],
    ["strategy asset ATA", built.map(({ identity }) => identity.voltr.strategyAssetAta)],
  ] as const) {
    add(gates, `four ${label} identities are unique`, new Set(values).size === built.length, values, `${built.length} unique identities`);
  }
  for (const [direction, instructions, indexes] of [
    ["deposit", depositInstructions, DEPOSIT_CONSTRAINED_INDEXES],
    ["withdraw", withdrawInstructions, WITHDRAW_CONSTRAINED_INDEXES],
  ] as const) {
    add(gates, `${direction} constrained indexes are unique non-negative integers`, new Set(indexes).size === indexes.length && indexes.every((index) => Number.isInteger(index) && index >= 0), indexes, "unique non-negative integers");
    add(gates, `${direction} constrained indexes fit every exact instruction`, instructions.every((instruction) => indexes.every((index) => index < instruction.accounts.length)), instructions.map((instruction) => instruction.accounts.length), `every index <= ${Math.max(...indexes)} is in range`);
  }
  const oneDepositPayload = policyPayloadBytes([depositInstructions[0]!], DEPOSIT_CONSTRAINED_INDEXES);
  const fourDepositPayload = policyPayloadBytes(depositInstructions, DEPOSIT_CONSTRAINED_INDEXES);
  const oneWithdrawPayload = policyPayloadBytes([withdrawInstructions[0]!], WITHDRAW_CONSTRAINED_INDEXES);
  const fourWithdrawPayload = policyPayloadBytes(withdrawInstructions, WITHDRAW_CONSTRAINED_INDEXES);
  const depositDelta = fourDepositPayload.length - oneDepositPayload.length;
  const withdrawDelta = fourWithdrawPayload.length - oneWithdrawPayload.length;
  const fourDepositPacketBytes = baselineDeposit.policyCreatePacketBytes + depositDelta;
  const fourWithdrawPacketBytes = baselineWithdraw.policyCreatePacketBytes + withdrawDelta;
  const fourDepositDataBytes = baselineDeposit.policyCreate.dataLength + depositDelta;
  const fourWithdrawDataBytes = baselineWithdraw.policyCreate.dataLength + withdrawDelta;
  const compactLengthPrefixStable = [
    baselineDeposit.policyCreate.dataLength,
    baselineWithdraw.policyCreate.dataLength,
    fourDepositDataBytes,
    fourWithdrawDataBytes,
  ].every((length) => length >= 128 && length < 16_384);
  add(gates, "baseline policy artifact reverified", loaded.artifact.verdict === "RUNTIME_POLICY_ARTIFACT_COMPILED_AND_VERIFIED", loaded.artifact.verdict, "RUNTIME_POLICY_ARTIFACT_COMPILED_AND_VERIFIED");
  add(gates, "baseline policy artifact binds exact Main RouteSpec", loaded.artifact.routeSpecSha256 === routeSpecSha256(PARTNER_ROUTE), loaded.artifact.routeSpecSha256, routeSpecSha256(PARTNER_ROUTE));
  add(gates, "baseline policy source manifest binds exact Main RouteSpec", loaded.artifact.sourceManifest.routeSpecSha256 === routeSpecSha256(PARTNER_ROUTE), loaded.artifact.sourceManifest.routeSpecSha256, routeSpecSha256(PARTNER_ROUTE));
  add(gates, "baseline policy source manifest hash is present", /^[0-9a-f]{64}$/.test(loaded.artifact.sourceManifestSha256), loaded.artifact.sourceManifestSha256, "64 lowercase hex characters; independently reverified by the Rust compiler");
  add(gates, "baseline policy canonical artifact hash is present", /^[0-9a-f]{64}$/.test(loaded.artifact.artifactSha256), loaded.artifact.artifactSha256, "64 lowercase hex characters; independently reverified by the Rust compiler");
  add(gates, "compact instruction-data prefix width remains exact", compactLengthPrefixStable, { baselineDeposit: baselineDeposit.policyCreate.dataLength, baselineWithdraw: baselineWithdraw.policyCreate.dataLength, fourDepositDataBytes, fourWithdrawDataBytes }, "all 128..16383 (two-byte compact-u16)");
  add(gates, "single deposit policy fits", baselineDeposit.policyCreatePacketBytes <= SOLANA_PACKET_LIMIT, baselineDeposit.policyCreatePacketBytes, `<=${SOLANA_PACKET_LIMIT}`);
  add(gates, "single withdrawal policy fits", baselineWithdraw.policyCreatePacketBytes <= SOLANA_PACKET_LIMIT, baselineWithdraw.policyCreatePacketBytes, `<=${SOLANA_PACKET_LIMIT}`);
  add(gates, "four-alternative deposit policy does not fit", fourDepositPacketBytes > SOLANA_PACKET_LIMIT, fourDepositPacketBytes, `>${SOLANA_PACKET_LIMIT}`);
  add(gates, "four-alternative withdrawal policy does not fit", fourWithdrawPacketBytes > SOLANA_PACKET_LIMIT, fourWithdrawPacketBytes, `>${SOLANA_PACKET_LIMIT}`);
  return {
    verdict: gates.every(({ pass }) => pass)
      ? "EIGHT_POLICY_TOPOLOGY_REQUIRED"
      : "FOUR_MARKET_POLICY_TOPOLOGY_FAIL",
    acceptedTopology: "eight-physical-policies",
    rejectedTopology: "two-policy-cartesian-graph",
    reason: "Independent route-field allowlists would authorize mixed Cartesian graphs; four exact instruction alternatives exceed the packet limit.",
    solanaPacketLimit: SOLANA_PACKET_LIMIT,
    baselineArtifactPath: MAIN_RUNTIME_POLICY_ARTIFACT_LABEL,
    baselineArtifactFileSha256: loaded.fileSha256,
    baselineArtifactCanonicalSha256: loaded.artifact.artifactSha256,
    baselineSourceManifestSha256: loaded.artifact.sourceManifestSha256,
    baselineRouteSpecSha256: loaded.artifact.routeSpecSha256,
    measurementMethod: "verified single-policy create packet plus exact generated ProgramInteraction payload byte delta; compact instruction-data prefix width is unchanged",
    deposit: {
      singlePolicyCreateDataBytes: baselineDeposit.policyCreate.dataLength,
      singlePolicyCreatePacketBytes: baselineDeposit.policyCreatePacketBytes,
      oneConstraintPayloadBytes: oneDepositPayload.length,
      fourConstraintPayloadBytes: fourDepositPayload.length,
      fourConstraintPayloadSha256: sha256(fourDepositPayload),
      fourAlternativeCreateDataBytes: fourDepositDataBytes,
      fourAlternativePacketBytes: fourDepositPacketBytes,
    },
    withdraw: {
      singlePolicyCreateDataBytes: baselineWithdraw.policyCreate.dataLength,
      singlePolicyCreatePacketBytes: baselineWithdraw.policyCreatePacketBytes,
      oneConstraintPayloadBytes: oneWithdrawPayload.length,
      fourConstraintPayloadBytes: fourWithdrawPayload.length,
      fourConstraintPayloadSha256: sha256(fourWithdrawPayload),
      fourAlternativeCreateDataBytes: fourWithdrawDataBytes,
      fourAlternativePacketBytes: fourWithdrawPacketBytes,
    },
    gates,
  } as const;
}

export function buildCompatibilityApproval() {
  const sourceBinding = localSourceBinding();
  const mainBaseline = loadRuntimePolicyArtifact(MAIN_RUNTIME_POLICY_ARTIFACT);
  const fourMarketCatalog = loadRuntimePolicyArtifact(FOUR_MARKET_RUNTIME_POLICY_ARTIFACT);
  return {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-four-market-compatibility-approval",
    approvalId: "operator-fixed-verifier-v1",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    cluster: PARTNER_ROUTE.cluster,
    genesisHash: PARTNER_ROUTE.genesisHash,
    routeSpec: {
      baseMainRouteSpecSha256: routeSpecSha256(PARTNER_ROUTE),
      fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(),
      candidateCatalogSha256: candidateCatalogSha256(),
    },
    sourceBinding: {
      algorithm: "sha256",
      ...sourceBinding,
    },
    runtimePolicyArtifacts: {
      mainBaseline: {
        path: MAIN_RUNTIME_POLICY_ARTIFACT_LABEL,
        fileSha256: mainBaseline.fileSha256,
        artifactSha256: mainBaseline.artifact.artifactSha256,
        sourceManifestSha256: mainBaseline.artifact.sourceManifestSha256,
      },
      fourMarketCatalog: {
        path: FOUR_MARKET_RUNTIME_POLICY_ARTIFACT_LABEL,
        fileSha256: fourMarketCatalog.fileSha256,
        artifactSha256: fourMarketCatalog.artifact.artifactSha256,
        sourceManifestSha256: fourMarketCatalog.artifact.sourceManifestSha256,
      },
    },
  } as const;
}

export async function verifyFourMarketCompatibility(
  commitment: Commitment,
  approvalPath: string | null,
  confirmedApprovalSha256: string | null,
) {
  if (commitment !== "confirmed") {
    throw new Error("four-market compatibility requires --commitment confirmed");
  }
  const sourceBinding = localSourceBinding();
  const approval = loadCompatibilityApproval(
    approvalPath,
    confirmedApprovalSha256,
    sourceBinding,
  );
  const baselineRuntimePolicy = loadRuntimePolicyArtifact(
    MAIN_RUNTIME_POLICY_ARTIFACT,
  );
  const catalogRuntimePolicy = loadRuntimePolicyArtifact(
    FOUR_MARKET_RUNTIME_POLICY_ARTIFACT,
  );
  if (
    baselineRuntimePolicy.fileSha256 !== approval.runtimePolicyArtifacts.mainBaseline.fileSha256
    || baselineRuntimePolicy.artifact.artifactSha256
      !== approval.runtimePolicyArtifacts.mainBaseline.artifactSha256
    || baselineRuntimePolicy.artifact.sourceManifestSha256
      !== approval.runtimePolicyArtifacts.mainBaseline.sourceManifestSha256
  ) {
    throw new Error(
      "approved baseline runtime-policy artifact does not match the independently reverified artifact",
    );
  }
  if (
    catalogRuntimePolicy.fileSha256 !== approval.runtimePolicyArtifacts.fourMarketCatalog.fileSha256
    || catalogRuntimePolicy.artifact.artifactSha256
      !== approval.runtimePolicyArtifacts.fourMarketCatalog.artifactSha256
    || catalogRuntimePolicy.artifact.sourceManifestSha256
      !== approval.runtimePolicyArtifacts.fourMarketCatalog.sourceManifestSha256
  ) {
    throw new Error(
      "approved four-market runtime-policy catalog does not match the independently reverified artifact",
    );
  }
  const connection = new Connection(
    (() => {
      const value = process.env.SOLANA_RPC_URL;
      if (!value) throw new Error("SOLANA_RPC_URL is required");
      return value;
    })(),
    commitment,
  );
  const genesisHash = await connection.getGenesisHash();
  if (genesisHash !== PARTNER_ROUTE.genesisHash) {
    throw new Error(
      `refusing cluster genesis ${genesisHash}; expected ${PARTNER_ROUTE.genesisHash}`,
    );
  }
  const globalGates: Gate[] = [];

  // The compatibility probe is confirmed-commitment by design, but policy
  // meaning is independently read from finalized state.  This keeps wrapper
  // sizing from becoming an authorization claim: an initialized strategy is
  // eligible for manager simulation only when this exact eight-policy
  // verifier has proved every current policy account and its creation origin.
  const policyEvidence = await verifyExistingRuntimePolicies(
    FOUR_MARKET_RUNTIME_POLICY_ARTIFACT,
  );
  const catalogPolicyRows = catalogRuntimePolicy.artifact.policies;
  const policyEvidenceRows = policyEvidence.policies;
  const policyEvidenceByKey = new Map(
    policyEvidenceRows.map((row) => [`${row.seed}:${row.policy}`, row]),
  );
  const exactCatalogPolicyEvidence = catalogPolicyRows.length === 8
    && policyEvidence.verdict === "PARTNER_RUNTIME_POLICIES_FINALIZED_PASS"
    && policyEvidence.failedGateCount === 0
    && policyEvidenceRows.length === 8
    && policyEvidence.gates.some((gate) => gate.name === "Settings includes the complete runtime-policy catalog" && gate.pass)
    && policyEvidence.nonCatalogIsolation.failedGateCount === 0
    && catalogPolicyRows.every((entry) => {
      const row = policyEvidenceByKey.get(`${entry.seed}:${entry.policy}`);
      return row !== undefined
        && row.seed === entry.seed
        && row.policy === entry.policy
        && row.origin !== null;
    });
  add(globalGates, "exact eight-policy catalog is independently verified from current finalized state", exactCatalogPolicyEvidence, {
    verdict: policyEvidence.verdict,
    failedGateCount: policyEvidence.failedGateCount,
    policyCount: policyEvidenceRows.length,
    catalogSeedGate: policyEvidence.gates.find((gate) => gate.name === "Settings includes the complete runtime-policy catalog")?.pass ?? false,
    nonCatalogIsolationGate: policyEvidence.nonCatalogIsolation.failedGateCount === 0,
    origins: policyEvidenceRows.map((row) => ({ seed: row.seed, policy: row.policy, origin: row.origin?.signature ?? null })),
  }, "eight semantic policy readbacks, exact creation origins, complete current catalog through seed 50, immutable legacy generation classified, and every other live policy constrained away from Voltr");
  add(globalGates, "exact catalog policy PDAs are the four physical wrapper pairs", catalogPolicyRows.every((entry) => entry.policy === derivePolicy(BigInt(entry.seed))), catalogPolicyRows.map(({ strategyId, operation, seed, policy }) => ({ strategyId, operation, seed, policy })), "seeds 43..50 derive from the approved Settings PDA");

  add(globalGates, "commitment exact", commitment === "confirmed", commitment, "confirmed");
  add(globalGates, "mainnet genesis exact", genesisHash === PARTNER_ROUTE.genesisHash, genesisHash, PARTNER_ROUTE.genesisHash);
  add(globalGates, "external compatibility approval SHA confirmed", approval.fileSha256 === confirmedApprovalSha256, approval.fileSha256, confirmedApprovalSha256);
  add(globalGates, "approved source binding matches checked-out source", stableJson(approval.sourceBinding) === stableJson(sourceBinding), approval.sourceBinding.aggregateSha256, sourceBinding.aggregateSha256);
  const candidateIds = PARTNER_STRATEGY_CANDIDATES.map(({ id }) => id);
  const candidateReserves = PARTNER_STRATEGY_CANDIDATES.map(({ reserve }) => reserve);
  add(globalGates, "four strategy ids unique", new Set(candidateIds).size === 4, candidateIds, ["main", "onre", "prime", "maple"]);
  add(globalGates, "four reserve addresses unique", new Set(candidateReserves).size === 4, candidateReserves, "four unique exact reserves");
  add(globalGates, "candidate catalog matches frozen graph catalog", stableJson(PARTNER_STRATEGY_CANDIDATES) === stableJson(PARTNER_FOUR_MARKET_STRATEGIES.map(({ id, reserve }) => ({ id, reserve }))), PARTNER_STRATEGY_CANDIDATES, PARTNER_FOUR_MARKET_STRATEGIES.map(({ id, reserve }) => ({ id, reserve })));
  add(globalGates, "four-market catalog binds exact singular Main baseline", PARTNER_FOUR_MARKET_ROUTE.baseMainRouteSpecSha256 === routeSpecSha256(PARTNER_ROUTE), PARTNER_FOUR_MARKET_ROUTE.baseMainRouteSpecSha256, routeSpecSha256(PARTNER_ROUTE));
  add(globalGates, "four-market withdrawal wait is exactly ten minutes", PARTNER_FOUR_MARKET_ROUTE.withdrawalWaitingPeriodSeconds === 600n && PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds === 600n, { fourMarket: PARTNER_FOUR_MARKET_ROUTE.withdrawalWaitingPeriodSeconds, vault: PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds }, 600n);
  add(globalGates, "normal optimization interval is exactly one hour", PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds === 3_600n, PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds, 3_600n);
  add(globalGates, "compatibility source binding contains 29 exact local files", sourceBinding.files.length === 29 && sourceBinding.files.every(({ sha256: digest }) => /^[0-9a-f]{64}$/.test(digest)), sourceBinding, "29 SHA-256-bound verifier, builder, decoder, policy authorization/commands, compiler, manifest, and lock files");

  const candidateAccounts = await Promise.all(
    PARTNER_STRATEGY_CANDIDATES.map(async (candidate) => ({
      candidate,
      accounts: await deriveVoltrAccountsForStrategy(PARTNER_ROUTE, candidate.reserve),
    })),
  );
  const reserveBatch = await loadReserveGraphs(
    connection.rpcEndpoint,
    PARTNER_ROUTE,
    candidateAccounts.map(({ candidate, accounts }) => ({
      candidate,
      vaultStrategyAuth: accounts.strategyAuth,
    })),
    commitment,
  );

  const buildById = new Map<string, Readonly<{
    built: BuiltStrategy;
    gates: readonly Gate[];
  }>>();
  const buildErrors = new Map<string, string>();
  for (const row of reserveBatch.rows) {
    if (!row.observation) {
      buildErrors.set(row.candidate.id, row.error ?? "reserve graph unavailable");
      continue;
    }
    try {
      buildById.set(row.candidate.id, await buildStrategy(row.observation));
    } catch (error) {
      buildErrors.set(
        row.candidate.id,
        error instanceof Error ? error.message : String(error),
      );
    }
  }
  const built = PARTNER_STRATEGY_CANDIDATES
    .map(({ id }) => buildById.get(id)?.built ?? null)
    .filter((value): value is BuiltStrategy => value !== null);

  const addresses = supportAddresses(built);
  const support = await loadSnapshots(connection, addresses, reserveBatch.contextSlot);
  const alt = await loadAlt(connection, support.contextSlot);
  add(globalGates, "lookup table address exact", alt.identity.address === PARTNER_ROUTE.lookupTable.address, alt.identity.address, PARTNER_ROUTE.lookupTable.address);
  add(globalGates, "lookup table authority exact", alt.identity.authority === PARTNER_ROUTE.lookupTable.authority, alt.identity.authority, PARTNER_ROUTE.lookupTable.authority);
  add(globalGates, "lookup table active", BigInt(alt.identity.deactivationSlot) === U64_MAX, alt.identity.deactivationSlot, U64_MAX.toString());
  add(globalGates, "lookup table address count matches frozen compatibility identity", alt.identity.addressCount === PARTNER_LOOKUP_TABLE_COMPATIBILITY_IDENTITY.addressCount, alt.identity.addressCount, PARTNER_LOOKUP_TABLE_COMPATIBILITY_IDENTITY.addressCount);
  add(globalGates, "lookup table ordered addresses match frozen compatibility identity", alt.identity.orderedAddressesSha256 === PARTNER_LOOKUP_TABLE_COMPATIBILITY_IDENTITY.orderedAddressesSha256, alt.identity.orderedAddressesSha256, PARTNER_LOOKUP_TABLE_COMPATIBILITY_IDENTITY.orderedAddressesSha256);
  add(globalGates, "lookup table read reaches support-state slot", alt.identity.contextSlot >= support.contextSlot, alt.identity.contextSlot, `>=${support.contextSlot}`);

  for (const [label, programId] of [
    ["classic Token", PARTNER_ROUTE.programs.token],
    ["Associated Token", PARTNER_ROUTE.programs.associatedToken],
    ["System", PARTNER_ROUTE.programs.system],
    ["Scope", PARTNER_SCOPE_PROGRAM],
  ] as const) {
    const program = support.byAddress.get(programId) ?? null;
    add(globalGates, `${label} program exists and is executable`, program?.executable === true, program ? { owner: program.owner, executable: program.executable, dataSha256: sha256(program.data) } : null, { programId, executable: true });
  }

  const deployments = await loadDeploymentIdentities(
    connection.rpcEndpoint,
    PARTNER_ROUTE,
    alt.identity.contextSlot,
    commitment,
  );
  const deploymentGates = verifyDeploymentIdentities(
    PARTNER_ROUTE,
    deployments.identities,
  );
  globalGates.push(...deploymentGates.map((gate) => ({
    ...gate,
    name: `deployment: ${gate.name}`,
  })));
  add(globalGates, "deployment reads reach ALT slot", deployments.contextSlot >= alt.identity.contextSlot, deployments.contextSlot, `>=${alt.identity.contextSlot}`);

  const blockhash = await connection.getLatestBlockhashAndContext({
    commitment,
    minContextSlot: deployments.contextSlot,
  });
  add(globalGates, "blockhash read reaches deployment slot", blockhash.context.slot >= deployments.contextSlot, blockhash.context.slot, `>=${deployments.contextSlot}`);
  const contextChain = [
    reserveBatch.contextSlot,
    support.contextSlot,
    alt.identity.contextSlot,
    deployments.contextSlot,
    blockhash.context.slot,
  ];
  add(
    globalGates,
    "confirmed context chain is monotonic",
    contextChain.every((slot, index) => index === 0 || slot >= contextChain[index - 1]!),
    contextChain,
    "reserveBatch <= supportState <= lookupTable <= deployments <= blockhash",
  );
  const compute = ComputeBudgetProgram.setComputeUnitLimit({
    units: MANAGER_COMPUTE_UNIT_LIMIT,
  });
  const heap = ComputeBudgetProgram.requestHeapFrame({
    bytes: MANAGER_HEAP_FRAME_BYTES,
  });
  const catalogPolicyByStrategy = new Map(
    PARTNER_STRATEGY_CANDIDATES.map((candidate) => [candidate.id, {
      deposit: catalogPolicyEntry(catalogRuntimePolicy.artifact, candidate.id, "deposit"),
      withdraw: catalogPolicyEntry(catalogRuntimePolicy.artifact, candidate.id, "withdraw"),
    }]),
  );

  const strategyRows = [];
  const pendingOperations: string[] = [];
  for (const candidate of PARTNER_STRATEGY_CANDIDATES) {
    const reserveRow = reserveBatch.rows.find((row) => row.candidate.id === candidate.id);
    const buildResult = buildById.get(candidate.id);
    if (!reserveRow?.observation || !buildResult) {
      const error = buildErrors.get(candidate.id) ?? reserveRow?.error ?? "candidate graph did not build";
      const gates: Gate[] = [];
      add(gates, "reserve graph and canonical builders available", false, error, "decoded active USDC farm-backed graph");
      pendingOperations.push(`${candidate.id}:graph-and-build`);
      strategyRows.push({
        id: candidate.id,
        reserve: candidate.reserve,
        reserveDataSha256: reserveRow?.observation?.reserveDataSha256 ?? null,
        bootstrapState: "INVALID_EXISTING_STATE" as BootstrapState,
        bootstrapReady: false,
        lifecycleReady: false,
        error,
        gates,
      });
      continue;
    }
    const current = buildResult.built;
    const inspected = inspectSupportState(current, support.byAddress, buildResult.gates);
    const gates = [...inspected.gates];
    const initDirect = compilePacket(
      PARTNER_ROUTE.setupAdmin,
      blockhash.value.blockhash,
      [web3FromCanonical(current.initialize)],
    ).report;
    const depositDirect = compilePacket(
      PARTNER_ROUTE.squads.manager,
      blockhash.value.blockhash,
      [web3FromCanonical(current.deposit)],
    ).report;
    const withdrawDirect = compilePacket(
      PARTNER_ROUTE.squads.manager,
      blockhash.value.blockhash,
      [web3FromCanonical(current.withdraw)],
    ).report;
    const setupPacket = compilePacket(
      PARTNER_ROUTE.setupAdmin,
      blockhash.value.blockhash,
      current.setupInstructions,
    );
    const catalogPolicies = catalogPolicyByStrategy.get(candidate.id);
    if (!catalogPolicies) throw new Error(`missing exact policy pair for ${candidate.id}`);
    const depositWrapper = buildManagerWrapperForCompatibility(
      catalogPolicies.deposit.policy,
      current.deposit,
    );
    const withdrawWrapper = buildManagerWrapperForCompatibility(
      catalogPolicies.withdraw.policy,
      current.withdraw,
    );
    const depositWrappedNoAlt = compilePacket(
      PARTNER_ROUTE.squads.guardian,
      blockhash.value.blockhash,
      [compute, heap, depositWrapper.instruction],
    ).report;
    const withdrawWrappedNoAlt = compilePacket(
      PARTNER_ROUTE.squads.guardian,
      blockhash.value.blockhash,
      [compute, heap, withdrawWrapper.instruction],
    ).report;
    const depositOperational = compilePacket(
      PARTNER_ROUTE.squads.guardian,
      blockhash.value.blockhash,
      [compute, heap, depositWrapper.instruction],
      [alt.account],
    );
    const withdrawOperational = compilePacket(
      PARTNER_ROUTE.squads.guardian,
      blockhash.value.blockhash,
      [compute, heap, withdrawWrapper.instruction],
      [alt.account],
    );
    add(gates, "direct initialize packet measured and fits", initDirect.withinLimit, initDirect.packetBytes, `<=${SOLANA_PACKET_LIMIT}`);
    add(gates, "atomic setup packet measured and fits", setupPacket.report.withinLimit, setupPacket.report.packetBytes, `<=${SOLANA_PACKET_LIMIT}`);
    add(gates, "direct deposit packet measured and fits", depositDirect.withinLimit, depositDirect.packetBytes, `<=${SOLANA_PACKET_LIMIT}`);
    add(gates, "direct withdrawal packet measured and fits", withdrawDirect.withinLimit, withdrawDirect.packetBytes, `<=${SOLANA_PACKET_LIMIT}`);
    add(gates, "operational deposit packet fits", depositOperational.report.withinLimit, depositOperational.report.packetBytes, `<=${SOLANA_PACKET_LIMIT}`);
    add(gates, "operational withdrawal packet fits", withdrawOperational.report.withinLimit, withdrawOperational.report.packetBytes, `<=${SOLANA_PACKET_LIMIT}`);
    add(gates, "operational deposit uses exact ALT", depositOperational.report.lookupTableCount === 1 && depositOperational.report.lookupTableAddresses[0] === PARTNER_ROUTE.lookupTable.address, depositOperational.report.lookupTableAddresses, [PARTNER_ROUTE.lookupTable.address]);
    add(gates, "operational withdrawal uses exact ALT", withdrawOperational.report.lookupTableCount === 1 && withdrawOperational.report.lookupTableAddresses[0] === PARTNER_ROUTE.lookupTable.address, withdrawOperational.report.lookupTableAddresses, [PARTNER_ROUTE.lookupTable.address]);
    add(gates, "deposit wrapper sole signer is guardian", depositOperational.report.requiredSignatureCount === 1 && depositOperational.report.signerAddresses[0] === PARTNER_ROUTE.squads.guardian, depositOperational.report.signerAddresses, [PARTNER_ROUTE.squads.guardian]);
    add(gates, "withdraw wrapper sole signer is guardian", withdrawOperational.report.requiredSignatureCount === 1 && withdrawOperational.report.signerAddresses[0] === PARTNER_ROUTE.squads.guardian, withdrawOperational.report.signerAddresses, [PARTNER_ROUTE.squads.guardian]);
    gates.push(...verifyCompatibilityWrapper(
      "deposit",
      depositWrapper,
      depositOperational.report,
    ));
    gates.push(...verifyCompatibilityWrapper(
      "withdraw",
      withdrawWrapper,
      withdrawOperational.report,
    ));

    const currentDepositPolicy = policyEvidenceByKey.get(
      `${catalogPolicies.deposit.seed}:${catalogPolicies.deposit.policy}`,
    );
    const currentWithdrawPolicy = policyEvidenceByKey.get(
      `${catalogPolicies.withdraw.seed}:${catalogPolicies.withdraw.policy}`,
    );
    const exactStrategyPolicyEvidence = exactCatalogPolicyEvidence
      && currentDepositPolicy?.origin !== null
      && currentDepositPolicy?.origin !== undefined
      && currentWithdrawPolicy?.origin !== null
      && currentWithdrawPolicy?.origin !== undefined;
    if (inspected.bootstrapState === "READY_FOR_MANAGER_SIMULATION") {
      add(gates, "initialized strategy has exact current catalog deposit and withdrawal policies", exactStrategyPolicyEvidence, {
        strategyId: candidate.id,
        deposit: { seed: catalogPolicies.deposit.seed, policy: catalogPolicies.deposit.policy, origin: currentDepositPolicy?.origin?.signature ?? null },
        withdraw: { seed: catalogPolicies.withdraw.seed, policy: catalogPolicies.withdraw.policy, origin: currentWithdrawPolicy?.origin?.signature ?? null },
      }, "exact finalized semantic policy pair and exact creation origins");
    }

    let initializeSimulation: SimulationReport;
    let depositSimulation: SimulationReport;
    let withdrawSimulation: SimulationReport;
    const supportStatePass = gates.every(({ pass }) => pass);
    if (!supportStatePass) {
      initializeSimulation = notRunSimulation(
        "not_run_invalid_support_state",
        "one_or_more_support_state_gates_failed",
      );
      depositSimulation = notRunSimulation(
        "not_run_invalid_support_state",
        "one_or_more_support_state_gates_failed",
      );
      withdrawSimulation = notRunSimulation(
        "not_run_invalid_support_state",
        "one_or_more_support_state_gates_failed",
      );
    } else if (inspected.bootstrapState === "PENDING_EXPECTED_BOOTSTRAP") {
      initializeSimulation = await simulateUnsigned(
        connection,
        setupPacket.transaction,
        blockhash.context.slot,
      );
      add(gates, "atomic initialize simulation succeeds for cleanly absent strategy", initializeSimulation.status === "pass", initializeSimulation.err, null);
      depositSimulation = pendingSimulation("expected_strategy_bootstrap_and_policy_missing");
      withdrawSimulation = pendingSimulation("expected_strategy_bootstrap_and_policy_missing");
    } else if (inspected.bootstrapState === "READY_FOR_MANAGER_SIMULATION" && exactStrategyPolicyEvidence) {
      initializeSimulation = {
        status: "observed",
        contextSlot: null,
        err: null,
        unitsConsumed: null,
        logsSha256: null,
        reasonCode: "already_initialized",
      };
      if (
        inspected.economic.idleRaw !== null
        && inspected.economic.idleRaw >= PARTNER_ROUTE.asset.proofAmountRaw
      ) {
        depositSimulation = await simulateUnsigned(
          connection,
          depositOperational.transaction,
          blockhash.context.slot,
        );
        add(gates, "deposit manager simulation succeeds when idle funds are sufficient", depositSimulation.status === "pass", depositSimulation.err, null);
      } else {
        depositSimulation = notRunSimulation(
          "not_run_economic_precondition",
          `idle_${inspected.economic.idleRaw?.toString() ?? "unknown"}_below_${PARTNER_ROUTE.asset.proofAmountRaw}`,
        );
      }
      if (
        inspected.economic.positionRaw !== null
        && inspected.economic.positionRaw >= PARTNER_ROUTE.asset.proofAmountRaw
      ) {
        withdrawSimulation = await simulateUnsigned(
          connection,
          withdrawOperational.transaction,
          blockhash.context.slot,
        );
        add(gates, "withdraw manager simulation succeeds when strategy position is sufficient", withdrawSimulation.status === "pass", withdrawSimulation.err, null);
      } else {
        withdrawSimulation = notRunSimulation(
          "not_run_economic_precondition",
          `position_${inspected.economic.positionRaw?.toString() ?? "unknown"}_below_${PARTNER_ROUTE.asset.proofAmountRaw}`,
        );
      }
    } else if (inspected.bootstrapState === "READY_FOR_MANAGER_SIMULATION") {
      initializeSimulation = {
        status: "observed",
        contextSlot: null,
        err: null,
        unitsConsumed: null,
        logsSha256: null,
        reasonCode: "already_initialized",
      };
      depositSimulation = notRunSimulation("not_run_policy_missing", "strategy_specific_policy_not_verified_by_exact_current_catalog");
      withdrawSimulation = notRunSimulation("not_run_policy_missing", "strategy_specific_policy_not_verified_by_exact_current_catalog");
    } else {
      initializeSimulation = pendingSimulation("invalid_or_preinitialized_state");
      depositSimulation = notRunSimulation(
        "not_run_policy_missing",
        "strategy_specific_policy_not_installed",
      );
      withdrawSimulation = notRunSimulation(
        "not_run_policy_missing",
        "strategy_specific_policy_not_installed",
      );
    }

    for (const [operation, simulation] of [
      ["initialize", initializeSimulation],
      ["deposit", depositSimulation],
      ["withdraw", withdrawSimulation],
    ] as const) {
      if (simulation.status.startsWith("not_run_")) {
        pendingOperations.push(`${candidate.id}:${operation}:${simulation.reasonCode}`);
      }
    }

    strategyRows.push({
      id: candidate.id,
      reserve: candidate.reserve,
      reserveDataSha256: current.observation.reserveDataSha256,
      reserveContextSlot: current.observation.contextSlot,
      bootstrapState: inspected.bootstrapState,
      bootstrapReady: inspected.bootstrapState === "READY_FOR_MANAGER_SIMULATION",
      lifecycleReady: false,
      reserveState: {
        status: current.observation.reserveStatus,
        lastUpdateSlot: current.observation.reserveLastUpdateSlot,
        lastUpdateStale: current.observation.reserveLastUpdateStale,
        priceStatus: current.observation.reservePriceStatus,
        liquidityMint: current.observation.liquidityMint,
        liquidityTokenProgram: current.observation.liquidityTokenProgram,
        liquidityDecimals: current.observation.liquidityMintDecimals,
        hasCollateralFarm: current.observation.hasCollateralFarm,
      },
      graph: current.observation.graph,
      voltr: {
        strategyAuth: current.accounts.strategyAuth,
        strategyInitReceipt: current.accounts.strategyInitReceipt,
        strategyAssetAta: current.strategyAssetAta,
        initialize: canonicalReport(current.initialize),
        deposit: canonicalReport(current.deposit),
        withdraw: canonicalReport(current.withdraw),
      },
      support: inspected.support,
      economicPreconditions: inspected.economic,
      packets: {
        directInitializeNoAlt: initDirect,
        atomicSetupNoAlt: setupPacket.report,
        directDepositNoAlt: depositDirect,
        directWithdrawNoAlt: withdrawDirect,
        wrappedDepositNoAlt: depositWrappedNoAlt,
        wrappedWithdrawNoAlt: withdrawWrappedNoAlt,
        operationalDepositWithAlt: depositOperational.report,
        operationalWithdrawWithAlt: withdrawOperational.report,
        sizingPolicyScope: "Each strategy wrapper uses its exact catalog deposit/withdraw policy PDA; policy meaning and creation origin come only from the independent current-catalog verifier.",
      },
      simulations: {
        atomicInitialize: initializeSimulation,
        managerDeposit: depositSimulation,
        managerWithdraw: withdrawSimulation,
      },
      failedGateCount: gates.filter(({ pass }) => !pass).length,
      gates,
    });
  }

  const topology = policyShape(built, baselineRuntimePolicy);
  const topologyGates = topology.gates;
  const rowFailedGateCount = strategyRows.reduce(
    (sum, row) => sum + row.gates.filter(({ pass }) => !pass).length,
    0,
  );
  const failedGateCount = globalGates.filter(({ pass }) => !pass).length
    + topologyGates.filter(({ pass }) => !pass).length
    + rowFailedGateCount;
  const pendingGateCount = pendingOperations.length;
  const firstFailedRow = strategyRows.find((row) => row.gates.some(({ pass }) => !pass));
  const nextExperiment = failedGateCount === 0
    ? "Chunk 2: use the approved frozen catalog to atomically initialize OnRe first."
    : firstFailedRow?.error
      ? `Resolve ${firstFailedRow.id} graph incompatibility first: ${firstFailedRow.error}`
      : globalGates.some(({ name, pass }) => !pass && name.includes("lookup table"))
        ? "Prepare a no-send ALT extension or replacement covering the exact four graphs, then rerun compatibility."
        : "Inspect the first failed named gate and rerun only this no-broadcast compatibility probe.";
  const body = {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-four-market-compatibility",
    verdict: failedGateCount === 0
      ? "BACKYARD_VOLTR_FOUR_MARKET_COMPATIBILITY_PASS"
      : "BACKYARD_VOLTR_FOUR_MARKET_COMPATIBILITY_FAIL",
    broadcast: false,
    commitment: "confirmed",
    lifecycleReady: false,
    scope: "No-broadcast live graph, canonical instruction, packet, ALT, deployment, and atomic bootstrap compatibility across one monotonic confirmed context chain. This is not a same-bank snapshot and is not the final four-market lifecycle verdict.",
    genesisHash,
    context: {
      reserveBatchSlot: reserveBatch.contextSlot,
      supportStateSlot: support.contextSlot,
      lookupTableSlot: alt.identity.contextSlot,
      deploymentSlot: deployments.contextSlot,
      blockhashSlot: blockhash.context.slot,
    },
    baseMainRouteSpecSha256: routeSpecSha256(PARTNER_ROUTE),
    baseRouteStatus: "singular-main execution baseline retained; immutable four-market catalog is separately frozen and hashed",
    fourMarketRouteSpecSha256: fourMarketRouteSpecSha256(),
    candidateCatalogSha256: candidateCatalogSha256(),
    approval: {
      path: approval.path,
      fileSha256: approval.fileSha256,
      approvalId: approval.approvalId,
      confirmedFileSha256: confirmedApprovalSha256,
      runtimePolicyArtifacts: approval.runtimePolicyArtifacts,
    },
    sourceBinding,
    candidates: PARTNER_STRATEGY_CANDIDATES,
    limits: {
      vaultCapRaw: PARTNER_ROUTE.asset.vaultCapRaw,
      maxManagerOperationRaw: PARTNER_ROUTE.asset.maxManagerOperationRaw,
      withdrawalWaitingPeriodSeconds: PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds,
      managerComputeUnitLimit: MANAGER_COMPUTE_UNIT_LIMIT,
      managerHeapFrameBytes: MANAGER_HEAP_FRAME_BYTES,
      solanaPacketBytes: SOLANA_PACKET_LIMIT,
    },
    lookupTable: alt.identity,
    deployments: {
      observed: deployments.identities,
      gates: deploymentGates,
    },
    policyShape: topology,
    policyCatalogEvidence: {
      artifact: {
        path: FOUR_MARKET_RUNTIME_POLICY_ARTIFACT_LABEL,
        fileSha256: catalogRuntimePolicy.fileSha256,
        artifactSha256: catalogRuntimePolicy.artifact.artifactSha256,
        sourceManifestSha256: catalogRuntimePolicy.artifact.sourceManifestSha256,
      },
      verifier: policyEvidence,
      exactCatalogPolicyEvidence,
    },
    strategies: strategyRows,
    failedGateCount,
    pendingGateCount,
    pendingOperations,
    nextExperiment,
    gates: globalGates,
  };
  return {
    ...body,
    artifactSha256: sha256(stableJson(body)),
  } as const;
}
