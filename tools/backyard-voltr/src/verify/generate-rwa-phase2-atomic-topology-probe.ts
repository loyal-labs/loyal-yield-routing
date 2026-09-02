/**
 * Measures the smallest honest pre-install Phase-2 topology:
 *
 *   signed PolicyCreate + signed executePolicyPayloadSync
 *
 * It is deliberately a probe, not Phase-2 simulation evidence. A packet that
 * cannot fit is recorded as an infeasible topology and the command stops
 * before simulation or mutation generation. Nothing in this file broadcasts.
 */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { executePolicyPayloadSync } from "@loyal-labs/loyal-smart-accounts-core/internal";
import bs58 from "bs58";
import { ed25519 } from "@noble/curves/ed25519";
import {
  Connection,
  Keypair,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
  type MessageV0,
  type AccountInfo,
} from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { buildPhaseTwoKaminoLaneOperations, resolutionLanes } from "../policies/rwa-multiply-phase2-kamino.js";

type Json = Record<string, unknown>;
type CompiledPolicy = Readonly<{
  name: string;
  logicalName: string;
  operations: readonly string[];
  policy: string;
  createInstruction: Readonly<{
    programId: string;
    accounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
    dataBase64: string;
    dataSha256: string;
  }>;
}>;

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const COMPILED_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json");
const RESOLUTION_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const OUTPUT_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-atomic-topology-probe-v2.json");
const PACKET_LIMIT = 1_232;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function shortvec(value: number): Buffer {
  invariant(Number.isSafeInteger(value) && value >= 0, "invalid shortvec value");
  const bytes: number[] = [];
  do {
    const next = value & 0x7f;
    value >>>= 7;
    bytes.push(value === 0 ? next : next | 0x80);
  } while (value !== 0);
  return Buffer.from(bytes);
}

function serializeMessageV0(message: MessageV0): Buffer {
  // web3 serializes into a fixed 1232-byte scratch buffer. Reproduce the
  // canonical v0 encoding so an oversize signed wire can still be measured.
  const instructionBytes = message.compiledInstructions.map((instruction) => Buffer.concat([
    Buffer.from([instruction.programIdIndex]),
    shortvec(instruction.accountKeyIndexes.length),
    Buffer.from(instruction.accountKeyIndexes),
    shortvec(instruction.data.length),
    Buffer.from(instruction.data),
  ]));
  const lookupBytes = message.addressTableLookups.map((lookup) => Buffer.concat([
    Buffer.from(lookup.accountKey.toBytes()),
    shortvec(lookup.writableIndexes.length), Buffer.from(lookup.writableIndexes),
    shortvec(lookup.readonlyIndexes.length), Buffer.from(lookup.readonlyIndexes),
  ]));
  return Buffer.concat([
    Buffer.from([0x80, message.header.numRequiredSignatures, message.header.numReadonlySignedAccounts,
      message.header.numReadonlyUnsignedAccounts]),
    shortvec(message.staticAccountKeys.length),
    ...message.staticAccountKeys.map((key) => Buffer.from(key.toBytes())),
    Buffer.from(bs58.decode(message.recentBlockhash)),
    shortvec(instructionBytes.length), ...instructionBytes,
    shortvec(lookupBytes.length), ...lookupBytes,
  ]);
}

function signOversizeV0(transaction: VersionedTransaction, signers: readonly Keypair[]): Buffer {
  invariant(transaction.message.version === 0, "atomic probe expected a v0 message");
  const message = transaction.message as MessageV0;
  const messageBytes = serializeMessageV0(message);
  const signerKeys = message.staticAccountKeys.slice(0, message.header.numRequiredSignatures);
  invariant(signerKeys.length === signers.length, "atomic probe signer count drifted");
  transaction.signatures = signerKeys.map((pubkey, index) => {
    const signer = signers[index]!;
    invariant(pubkey.equals(signer.publicKey), `atomic probe signer ${index} order drifted`);
    return ed25519.sign(messageBytes, signer.secretKey.slice(0, 32));
  });
  return Buffer.concat([shortvec(transaction.signatures.length),
    ...transaction.signatures.map((signature) => Buffer.from(signature)), messageBytes]);
}

function asRecord(value: unknown, label: string): Json {
  invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`);
  return value as Json;
}

function asString(value: unknown, label: string): string {
  invariant(typeof value === "string" && value.length > 0, `${label} is not a non-empty string`);
  return value;
}

function decodedBase64(value: unknown, label: string): Buffer {
  const encoded = asString(value, label);
  const decoded = Buffer.from(encoded, "base64");
  invariant(decoded.length > 0 && decoded.toString("base64") === encoded, `${label} is not canonical base64`);
  return decoded;
}

function compileCreateInstruction(value: unknown): TransactionInstruction {
  const row = asRecord(value, "compiled create instruction");
  const data = decodedBase64(row.dataBase64, "compiled create instruction data");
  invariant(sha256(data) === asString(row.dataSha256, "compiled create instruction hash"),
    "compiled create instruction data hash drifted");
  invariant(Array.isArray(row.accounts) && row.accounts.length > 0, "compiled create instruction accounts are absent");
  return new TransactionInstruction({
    programId: new PublicKey(asString(row.programId, "compiled create instruction program")),
    data,
    keys: row.accounts.map((value, index) => {
      const account = asRecord(value, `compiled create account ${index}`);
      invariant(typeof account.signer === "boolean" && typeof account.writable === "boolean",
        `compiled create account ${index} roles are malformed`);
      return {
        pubkey: new PublicKey(asString(account.address, `compiled create account ${index} address`)),
        isSigner: account.signer,
        isWritable: account.writable,
      };
    }),
  });
}

function compileInner(instruction: TransactionInstruction) {
  const accounts: Array<{ pubkey: PublicKey; isWritable: boolean; isSigner: false }> = [];
  const indexOf = (pubkey: PublicKey, writable: boolean) => {
    const prior = accounts.findIndex((account) => account.pubkey.equals(pubkey));
    if (prior >= 0) {
      accounts[prior]!.isWritable ||= writable;
      return prior;
    }
    invariant(accounts.length < 255, "atomic probe inner account table exceeds u8");
    accounts.push({ pubkey, isWritable: writable, isSigner: false });
    return accounts.length - 1;
  };
  invariant(instruction.keys.every((key) => !key.isSigner || key.pubkey.toBase58() === RWA_MULTIPLY_ROUTE.squads.vault),
    "atomic probe canonical inner instruction has a signer other than the Squads vault");
  const indexes = instruction.keys.map((key) => indexOf(key.pubkey, key.isWritable));
  const programIndex = indexOf(instruction.programId, false);
  invariant(instruction.data.length <= 65_535, "atomic probe inner data exceeds u16");
  const length = Buffer.alloc(2);
  length.writeUInt16LE(instruction.data.length);
  return {
    accounts,
    bytes: Buffer.concat([Buffer.from([1, programIndex, indexes.length, ...indexes]), length, instruction.data]),
  } as const;
}

function stateSha256(infos: readonly AccountInfo<Buffer>[]): string {
  return sha256(JSON.stringify(infos.map((info) => ({
    owner: info.owner.toBase58(), executable: info.executable, lamports: info.lamports,
    dataBase64: info.data.toString("base64"),
  }))));
}

function loadRepresentative(): Readonly<{ compiledSha256: string; policy: CompiledPolicy; inner: TransactionInstruction }> {
  const compiledBytes = readFileSync(COMPILED_PATH);
  const compiled = asRecord(JSON.parse(compiledBytes.toString("utf8")), "compiled artifact");
  invariant(compiled.broadcast === false && Array.isArray(compiled.policies),
    "compiled artifact is not a no-broadcast policy artifact");
  const candidate = compiled.policies.find((value) => {
    const row = asRecord(value, "compiled policy");
    return Array.isArray(row.operations) && row.operations.length === 1 && typeof row.logicalName === "string";
  });
  invariant(candidate, "compiled artifact has no selected single-operation policy for the atomic topology probe");
  const row = asRecord(candidate, "representative compiled policy");
  const policy: CompiledPolicy = {
    name: asString(row.name, "representative policy name"),
    logicalName: asString(row.logicalName, "representative policy logical name"),
    operations: row.operations as string[],
    policy: asString(row.policy, "representative policy address"),
    createInstruction: asRecord(row.createInstruction, "representative policy create instruction") as CompiledPolicy["createInstruction"],
  };
  const [prefix, market, collateral, debt] = policy.logicalName.split("/");
  invariant(prefix === "lane" && market && collateral && debt, "representative policy logical lane is malformed");
  const operation = policy.operations[0]?.toLowerCase();
  const resolution = asRecord(JSON.parse(readFileSync(RESOLUTION_PATH, "utf8")), "resolution artifact");
  const lane = resolutionLanes(resolution).find((value) => value.key === `${market}/${collateral}/${debt}`);
  invariant(lane, "representative policy lane is absent from the confirmed resolver artifact");
  const canonical = buildPhaseTwoKaminoLaneOperations(lane).find((value) => value.operation === operation);
  invariant(canonical, "representative policy operation is absent from the canonical Kamino builder");
  const inner = new TransactionInstruction({
    programId: new PublicKey(canonical.programId),
    data: Buffer.from(canonical.dataBase64, "base64"),
    keys: canonical.accounts.map((account) => ({
      pubkey: new PublicKey(account.address), isSigner: account.signer, isWritable: account.writable,
    })),
  });
  return { compiledSha256: sha256(compiledBytes), policy, inner };
}

async function main() {
  invariant(!existsSync(OUTPUT_PATH), `atomic topology probe already exists at ${OUTPUT_PATH}; refusing to replace evidence`);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const { compiledSha256, policy, inner } = loadRepresentative();
  const adminMaterial = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  const delegatedMaterial = await signingMaterialFromEnvironment("POLICY_KEYPAIR");
  const admin = Keypair.fromSecretKey(adminMaterial.secretKey);
  const delegated = Keypair.fromSecretKey(delegatedMaterial.secretKey);
  invariant(admin.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin,
    "SOLANA_TESTING_PK is not the pinned Phase-2 settings authority");
  invariant(delegated.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
    "POLICY_KEYPAIR is not the pinned Phase-2 delegated executor");
  const create = compileCreateInstruction(policy.createInstruction);
  const compiled = compileInner(inner);
  const execute = executePolicyPayloadSync({
    feePayer: delegated.publicKey,
    policy: new PublicKey(policy.policy),
    accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
    numSigners: 1,
    policyPayload: {
      __kind: "ProgramInteraction",
      fields: [{
        instructionConstraintIndices: new Uint8Array([0]),
        transactionPayload: { __kind: "SyncTransaction", fields: [{
          accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex, instructions: compiled.bytes,
        }] },
      }],
    },
    instruction_accounts: [
      { pubkey: delegated.publicKey, isSigner: true, isWritable: false },
      ...compiled.accounts,
    ],
  });
  const connection = new Connection(rpcUrl, "confirmed");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const latest = await connection.getLatestBlockhashAndContext("confirmed");
  const transaction = new VersionedTransaction(new TransactionMessage({
    payerKey: admin.publicKey, recentBlockhash: latest.value.blockhash, instructions: [create, execute],
  }).compileToV0Message());
  const wire = signOversizeV0(transaction, [admin, delegated]);
  const signatures = transaction.signatures.map((signature) => Buffer.from(signature).toString("base64"));
  const expectedSignature = bs58.encode(transaction.signatures[0]!);
  const inspectedAddresses = [...new Set([
    RWA_MULTIPLY_ROUTE.squads.settings, RWA_MULTIPLY_ROUTE.squads.vault, policy.policy,
    ...compiled.accounts.map(({ pubkey }) => pubkey.toBase58()),
  ])];
  const common = {
    schema: "loyal-backyard-rwa-phase2-atomic-topology-probe/v1",
    broadcast: false,
    signedUnsent: true,
    cluster: "mainnet-beta",
    commitment: "confirmed",
    genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    compiledArtifactSha256: compiledSha256,
    topology: "PolicyCreate+executePolicyPayloadSync",
    lookupTables: [],
    representative: { policyName: policy.name, logicalName: policy.logicalName, operation: policy.operations[0], policy: policy.policy },
    transactionBase64: Buffer.from(wire).toString("base64"),
    transactionSha256: sha256(wire),
    messageSha256: sha256(wire.subarray(1 + transaction.signatures.length * 64)),
    signatures,
    expectedSignature,
    packetBytes: wire.length,
    packetLimitBytes: PACKET_LIMIT,
    inspectedAddresses,
  } as const;
  if (wire.length > PACKET_LIMIT) {
    writeFileSync(OUTPUT_PATH, `${JSON.stringify({ ...common,
      verdict: "ATOMIC_TOPOLOGY_INFEASIBLE_PACKET",
      blocker: `signed PolicyCreate+executePolicyPayloadSync packet is ${wire.length} bytes (> ${PACKET_LIMIT})`,
      downstreamSimulationRun: false,
      downstreamMutationsRun: false,
    }, null, 2)}\n`, { flag: "wx", mode: 0o600 });
    console.log(JSON.stringify({ verdict: "ATOMIC_TOPOLOGY_INFEASIBLE_PACKET", packetBytes: wire.length, output: OUTPUT_PATH }));
    return;
  }
  const before = await connection.getMultipleAccountsInfoAndContext(inspectedAddresses.map((value) => new PublicKey(value)), {
    commitment: "confirmed", minContextSlot: latest.context.slot,
  });
  invariant(before.value.every((value): value is AccountInfo<Buffer> => value !== null), "atomic probe protected account is absent");
  const preSimulationSignature = await connection.getSignatureStatuses([expectedSignature], { searchTransactionHistory: true });
  invariant(preSimulationSignature.value[0] === null, "atomic probe signed-unsent wire already landed before simulation");
  const simulation = await connection.simulateTransaction(VersionedTransaction.deserialize(wire), {
    commitment: "confirmed", sigVerify: true, replaceRecentBlockhash: false, minContextSlot: before.context.slot,
  });
  const postSimulationSignature = await connection.getSignatureStatuses([expectedSignature], { searchTransactionHistory: true });
  invariant(postSimulationSignature.value[0] === null, "atomic probe simulation unexpectedly landed a signed wire");
  const after = await connection.getMultipleAccountsInfoAndContext(inspectedAddresses.map((value) => new PublicKey(value)), {
    commitment: "confirmed", minContextSlot: simulation.context.slot,
  });
  invariant(after.value.every((value): value is AccountInfo<Buffer> => value !== null), "atomic probe post-readback account is absent");
  writeFileSync(OUTPUT_PATH, `${JSON.stringify({ ...common,
    verdict: simulation.value.err === null ? "ATOMIC_TOPOLOGY_SIMULATION_PASS" : "ATOMIC_TOPOLOGY_SIMULATION_REJECTED",
    simulation: { sigVerify: true, replaceRecentBlockhash: false, contextSlot: simulation.context.slot,
      err: simulation.value.err, logsSha256: sha256((simulation.value.logs ?? []).join("\n")) },
    signatureAbsentOnChain: preSimulationSignature.value[0] === null && postSimulationSignature.value[0] === null,
    chainPreStateSha256: stateSha256(before.value), chainPostStateSha256: stateSha256(after.value),
    confirmedReadbackSlot: after.context.slot,
  }, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  console.log(JSON.stringify({ verdict: simulation.value.err === null ? "ATOMIC_TOPOLOGY_SIMULATION_PASS" : "ATOMIC_TOPOLOGY_SIMULATION_REJECTED", packetBytes: wire.length, output: OUTPUT_PATH }));
}

main().catch((error) => {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  const message = error instanceof Error ? error.message : String(error);
  console.error(rpcUrl ? message.replaceAll(rpcUrl, "<rpc>") : message);
  process.exitCode = 1;
});
