/**
 * Signed-but-unsent Phase-2 negative policy probes.
 *
 * This command only calls Helius `simulateBundle`; it contains no send or
 * broadcast path.  Each bundle first creates the exact artifact prefix in the
 * simulation bank, then sends one deliberately-invalid Squads execution.
 */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { executePolicyPayloadSync } from "@loyal-labs/loyal-smart-accounts-core/internal";
import bs58 from "bs58";
import {
  Connection,
  Keypair,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
  type AccountInfo,
} from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  buildPhaseTwoKaminoLaneOperations,
  resolutionLanes,
} from "../policies/rwa-multiply-phase2-kamino.js";
import { buildExactJupiterSquadsExecution } from "./rwa-phase2-jupiter-execution.js";
import { buildExactKaminoSquadsExecution } from "./rwa-phase2-kamino-execution.js";

type Json = Record<string, unknown>;
type CaseName =
  | "same-mint-wrong-reserve"
  | "cross-lane-obligation"
  | "unapproved-edge"
  | "extra-instruction"
  | "amount-cap-breach"
  | "signer-substitution"
  | "writable-role-substitution";
type Wire = Readonly<{
  role: string;
  wire: Uint8Array;
  signature: string;
  packetBytes: number;
}>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const COMPILED = resolve(
  ROOT,
  "docs/evidence/backyard-rwa-go/policy-compiled-v1.json",
);
const RESOLUTION = resolve(
  ROOT,
  "docs/evidence/backyard-rwa-go/policy-resolution-v1.json",
);
const HEADERS = resolve(
  ROOT,
  "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json",
);
const OUTPUT = resolve(
  ROOT,
  `docs/evidence/backyard-rwa-go/policy-helius-negative-bundles-${process.env.RWA_PHASE2_NEGATIVE_VERSION?.trim() || "v1"}.json`,
);
const EXPECTED_ARTIFACT_SHA256 =
  "8322303a592ea5441f433edf6e40246ccb8f466fde2a0d9d5544d3e76b6a88bd";
const PACKET_LIMIT = 1_232;
const MAX_TRANSACTIONS = 20;
const CASES: readonly CaseName[] = [
  "same-mint-wrong-reserve",
  "cross-lane-obligation",
  "unapproved-edge",
  "extra-instruction",
  "amount-cap-breach",
  "signer-substitution",
  "writable-role-substitution",
];

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}
function object(value: unknown, label: string): Json {
  invariant(
    value !== null && typeof value === "object" && !Array.isArray(value),
    `${label} is not an object`,
  );
  return value as Json;
}
function array(value: unknown, label: string): unknown[] {
  invariant(Array.isArray(value), `${label} is not an array`);
  return value;
}
function text(value: unknown, label: string): string {
  invariant(
    typeof value === "string" && value.length > 0,
    `${label} is missing`,
  );
  return value;
}
function integer(value: unknown, label: string): number {
  invariant(
    typeof value === "number" && Number.isSafeInteger(value),
    `${label} is not an integer`,
  );
  return value;
}
function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}
function stateSha256(infos: readonly (AccountInfo<Buffer> | null)[]): string {
  return sha256(
    JSON.stringify(
      infos.map((info) =>
        info === null
          ? null
          : {
              owner: info.owner.toBase58(),
              executable: info.executable,
              lamports: info.lamports,
              data: info.data.toString("base64"),
            },
      ),
    ),
  );
}
function createInstruction(value: unknown): TransactionInstruction {
  const row = object(value, "compiled PolicyCreate");
  const data = Buffer.from(text(row.dataBase64, "create data"), "base64");
  invariant(
    data.toString("base64") === row.dataBase64 &&
      sha256(data) === text(row.dataSha256, "create data hash"),
    "compiled PolicyCreate data drifted",
  );
  return new TransactionInstruction({
    programId: new PublicKey(text(row.programId, "create program")),
    data,
    keys: array(row.accounts, "create accounts").map((entry, index) => {
      const account = object(entry, `create account ${index}`);
      return {
        pubkey: new PublicKey(
          text(account.address, `create account ${index} address`),
        ),
        isSigner: account.signer === true,
        isWritable: account.writable === true,
      };
    }),
  });
}
function sign(
  payer: Keypair,
  blockhash: string,
  instruction: TransactionInstruction,
  role: string,
): Wire {
  const tx = new VersionedTransaction(
    new TransactionMessage({
      payerKey: payer.publicKey,
      recentBlockhash: blockhash,
      instructions: [instruction],
    }).compileToV0Message(),
  );
  tx.sign([payer]);
  const wire = tx.serialize();
  invariant(
    wire.length <= PACKET_LIMIT,
    `${role} packet ${wire.length} exceeds ${PACKET_LIMIT}`,
  );
  return {
    role,
    wire,
    signature: bs58.encode(tx.signatures[0]!),
    packetBytes: wire.length,
  };
}
function cloneInstruction(
  instruction: TransactionInstruction,
  mutate: (
    key: { pubkey: PublicKey; isSigner: boolean; isWritable: boolean },
    index: number,
  ) => { pubkey: PublicKey; isSigner: boolean; isWritable: boolean },
): TransactionInstruction {
  return new TransactionInstruction({
    programId: instruction.programId,
    data: Buffer.from(instruction.data),
    keys: instruction.keys.map((key, index) =>
      mutate(
        {
          pubkey: key.pubkey,
          isSigner: key.isSigner,
          isWritable: key.isWritable,
        },
        index,
      ),
    ),
  });
}
function assertExactCanonicalRoles(
  candidate: TransactionInstruction,
  canonical: TransactionInstruction,
): void {
  invariant(candidate.keys.length === canonical.keys.length, "canonical account count drifted");
  for (let index = 0; index < canonical.keys.length; index += 1) {
    const expected = canonical.keys[index]!;
    const actual = candidate.keys[index]!;
    invariant(
      actual.pubkey.equals(expected.pubkey) &&
        actual.isSigner === expected.isSigner &&
        actual.isWritable === expected.isWritable,
      `canonical account role ${index} drifted`,
    );
  }
}
function replaceKey(
  instruction: TransactionInstruction,
  expected: PublicKey,
  actual: PublicKey,
): TransactionInstruction {
  let found = 0;
  const next = cloneInstruction(instruction, (key) => {
    if (!key.pubkey.equals(expected)) return key;
    found += 1;
    return { ...key, pubkey: actual };
  });
  invariant(
    found === 1,
    `outer instruction expected exactly one ${expected.toBase58()} account, found ${found}`,
  );
  return next;
}
function replaceLastInnerData(
  outer: TransactionInstruction,
  canonical: Buffer,
  replacement: Buffer,
): TransactionInstruction {
  invariant(
    canonical.length === replacement.length,
    "inner data replacement must preserve packet shape",
  );
  const hits: number[] = [];
  for (
    let offset = 0;
    offset <= outer.data.length - canonical.length;
    offset += 1
  )
    if (
      outer.data.subarray(offset, offset + canonical.length).equals(canonical)
    )
      hits.push(offset);
  invariant(
    hits.length === 1,
    `expected one serialized canonical inner-data sequence, found ${hits.length}`,
  );
  const data = Buffer.from(outer.data);
  replacement.copy(data, hits[0]!);
  return new TransactionInstruction({
    programId: outer.programId,
    data,
    keys: outer.keys,
  });
}
function compileInner(instruction: TransactionInstruction) {
  const accounts: Array<{
    pubkey: PublicKey;
    isWritable: boolean;
    isSigner: false;
  }> = [];
  const indexOf = (pubkey: PublicKey, writable: boolean) => {
    const old = accounts.findIndex((account) => account.pubkey.equals(pubkey));
    if (old >= 0) {
      accounts[old]!.isWritable ||= writable;
      return old;
    }
    invariant(accounts.length < 255, "inner account table exceeds u8");
    accounts.push({ pubkey, isWritable: writable, isSigner: false });
    return accounts.length - 1;
  };
  const indexes = instruction.keys.map((key) =>
    indexOf(key.pubkey, key.isWritable),
  );
  const programIndex = indexOf(instruction.programId, false);
  const length = Buffer.alloc(2);
  length.writeUInt16LE(instruction.data.length);
  return {
    accounts,
    bytes: Buffer.concat([
      Buffer.from([1, programIndex, indexes.length, ...indexes]),
      length,
      instruction.data,
    ]),
    programIndex,
  } as const;
}
function extraInstructionOuter(
  canonical: TransactionInstruction,
  policy: string,
  delegated: PublicKey,
): TransactionInstruction {
  const compiled = compileInner(canonical);
  // The encoded inner vector declares two instructions while the policy has
  // one constraint index. The synthetic second record uses the existing
  // K-Lend program and no accounts, so Squads rejects on count before CPI.
  const twoInstructions = Buffer.concat([
    Buffer.from([2]),
    compiled.bytes.subarray(1),
    Buffer.from([compiled.programIndex, 0, 0, 0]),
  ]);
  return executePolicyPayloadSync({
    feePayer: delegated,
    policy: new PublicKey(policy),
    accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
    numSigners: 1,
    policyPayload: {
      __kind: "ProgramInteraction",
      fields: [
        {
          instructionConstraintIndices: new Uint8Array([0]),
          transactionPayload: {
            __kind: "SyncTransaction",
            fields: [
              {
                accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
                instructions: twoInstructions,
              },
            ],
          },
        },
      ],
    },
    instruction_accounts: [
      { pubkey: delegated, isSigner: true, isWritable: false },
      ...compiled.accounts,
    ],
    programId: new PublicKey(RWA_MULTIPLY_ROUTE.squads.program),
  });
}
function providerResult(value: unknown): Json {
  const root = object(value, "Helius response");
  invariant(!root.error, `Helius RPC error: ${JSON.stringify(root.error)}`);
  const result = object(root.result, "Helius result");
  const body = object(result.value, "Helius result value");
  return {
    contextSlot: object(result.context ?? {}, "Helius context").slot ?? null,
    summary: body.summary ?? null,
    transactionResults: array(
      body.transactionResults,
      "Helius transactionResults",
    ).map((entry, index) => {
      const row = object(entry, `Helius transaction ${index}`);
      const logs = Array.isArray(row.logs)
        ? row.logs.map((line) => String(line))
        : [];
      return {
        err: row.err ?? null,
        logs,
        logsSha256: sha256(JSON.stringify(logs)),
        preExecutionAccountsSha256: sha256(
          JSON.stringify(row.preExecutionAccounts ?? []),
        ),
        postExecutionAccountsSha256: sha256(
          JSON.stringify(row.postExecutionAccounts ?? []),
        ),
      };
    }),
  };
}
function isSquadsTopLevelCustomError(value: unknown): boolean {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    return false;
  const instructionError = (value as Json).InstructionError;
  if (
    !Array.isArray(instructionError) ||
    instructionError.length !== 2 ||
    instructionError[0] !== 0
  )
    return false;
  const detail = instructionError[1];
  return (
    detail !== null &&
    typeof detail === "object" &&
    !Array.isArray(detail) &&
    typeof (detail as Json).Custom === "number" &&
    integer((detail as Json).Custom, "Squads custom error") >= 0x1770 &&
    integer((detail as Json).Custom, "Squads custom error") <= 0x17ee
  );
}

async function main() {
  invariant(
    !existsSync(OUTPUT),
    `${OUTPUT} already exists; refusing to overwrite evidence`,
  );
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(
    rpcUrl && new URL(rpcUrl).hostname.includes("helius"),
    "Helius SOLANA_RPC_URL is required",
  );
  const artifactBytes = readFileSync(COMPILED);
  invariant(
    sha256(artifactBytes) === EXPECTED_ARTIFACT_SHA256,
    "compiled artifact hash is not the parent-confirmed final artifact",
  );
  const artifact = object(
    JSON.parse(artifactBytes.toString("utf8")),
    "compiled artifact",
  );
  invariant(
    artifact.phase === "phase2" && artifact.broadcast === false,
    "current artifact is not Phase-2 no-broadcast",
  );
  const policies = array(artifact.policies, "compiled policies").map((entry) =>
    object(entry, "compiled policy"),
  );
  const borrow = policies.find((policy) => {
    if (policy.logicalName !== "lane/OnRe/ONyc/USDC") return false;
    const operations = array(policy.operations, "operations");
    return operations.length === 1 && operations[0] === "borrow";
  });
  invariant(
    borrow && text(borrow.seed, "borrow seed") === "77",
    "semantic OnRe/ONyc/USDC borrow policy must be seed 77",
  );
  const primeToUsdc = policies.find(
    (policy) =>
      Array.isArray(policy.swapEdges) &&
      policy.swapEdges.some((value) => {
        const edge = object(value, "swap edge");
        return edge.from === "PRIME" && edge.to === "USDC";
      }),
  );
  invariant(
    primeToUsdc && text(primeToUsdc.seed, "PRIME->USDC seed") === "75",
    "semantic PRIME->USDC packed policy must be seed 75",
  );
  const resolution = object(
    JSON.parse(readFileSync(RESOLUTION, "utf8")),
    "resolution",
  );
  const lanes = resolutionLanes(resolution);
  const onre = lanes.find((lane) => lane.key === "OnRe/ONyc/USDC");
  const prime = lanes.find((lane) => lane.key === "Prime/PRIME/USDC");
  invariant(onre && prime, "OnRe and Prime USDC lanes are required");
  invariant(
    onre.resolved.debtReserve.liquidityMint ===
      prime.resolved.debtReserve.liquidityMint &&
      onre.resolved.debtReserve.address !== prime.resolved.debtReserve.address,
    "same-mint reserve mutation is not meaningful",
  );
  const canonicalOperation = buildPhaseTwoKaminoLaneOperations(onre).find(
    (operation) => operation.operation === "borrow",
  );
  invariant(canonicalOperation, "canonical OnRe borrow is absent");
  const canonicalInner = new TransactionInstruction({
    programId: new PublicKey(canonicalOperation.programId),
    data: Buffer.from(canonicalOperation.dataBase64, "base64"),
    keys: canonicalOperation.accounts.map((account) => ({
      pubkey: new PublicKey(account.address),
      isSigner: account.signer,
      isWritable: account.writable,
    })),
  });
  const admin = Keypair.fromSecretKey(
    (await signingMaterialFromEnvironment("SOLANA_TESTING_PK")).secretKey,
  );
  const delegated = Keypair.fromSecretKey(
    (await signingMaterialFromEnvironment("POLICY_KEYPAIR")).secretKey,
  );
  invariant(
    admin.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin &&
      delegated.publicKey.toBase58() ===
        RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
    "configured signer identity drifted",
  );
  const connection = new Connection(rpcUrl, "confirmed");
  invariant(
    (await connection.getGenesisHash()) === RWA_MULTIPLY_ROUTE.genesisHash,
    "Helius endpoint is not mainnet-beta",
  );
  const latest = await connection.getLatestBlockhashAndContext("confirmed");
  const headers = object(
    JSON.parse(readFileSync(HEADERS, "utf8")),
    "Jupiter headers",
  );
  const primeHeader = array(headers.rows, "Jupiter headers rows")
    .map((entry) => object(entry, "Jupiter header"))
    .find((row) => row.key === "PRIME->USDC");
  invariant(primeHeader, "PRIME->USDC header is absent");
  const canonicalBorrow = buildExactKaminoSquadsExecution({
    compiledPolicy: borrow,
    operation: "borrow",
    innerInstruction: canonicalInner,
    delegatedSigner: delegated.publicKey,
  });
  const jupiter = await buildExactJupiterSquadsExecution({
    connection,
    compiledPolicy: primeToUsdc,
    headerRow: primeHeader,
    delegatedSigner: delegated.publicKey,
  });
  const jupiterWrongPolicy = policies.find(
    (policy) =>
      policy !== primeToUsdc &&
      Array.isArray(policy.swapEdges) &&
      !policy.swapEdges.some((value) => {
        const edge = object(value, "other swap edge");
        return edge.from === "PRIME" && edge.to === "USDC";
      }),
  );
  invariant(
    jupiterWrongPolicy,
    "need a current packed policy that excludes PRIME->USDC",
  );
  const results: Json[] = [];
  for (const name of CASES) {
    const target = name === "unapproved-edge" ? primeToUsdc : borrow;
    const prefixEnd = policies.indexOf(target);
    invariant(prefixEnd >= 0, `${name} target is not in artifact`);
    const prefix = policies.slice(0, prefixEnd + 1);
    invariant(
      prefix.length <= 11 &&
        Number(text(target.seed, `${name} target seed`)) <= 77,
      `${name} prefix exceeds the allowed seed 77 boundary`,
    );
    let finalInstruction: TransactionInstruction;
    let finalPayer = delegated;
    let mutation: Json;
    let preSign: Json | null = null;
    let downstreamProgram: PublicKey;
    if (name === "same-mint-wrong-reserve") {
      finalInstruction = replaceKey(
        canonicalBorrow.outerInstruction,
        new PublicKey(onre.resolved.debtReserve.address),
        new PublicKey(prime.resolved.debtReserve.address),
      );
      mutation = {
        expected: onre.resolved.debtReserve.address,
        actual: prime.resolved.debtReserve.address,
        proof: "same liquidity mint, distinct Kamino reserve",
      };
      downstreamProgram = canonicalInner.programId;
    } else if (name === "cross-lane-obligation") {
      finalInstruction = replaceKey(
        canonicalBorrow.outerInstruction,
        new PublicKey(onre.resolved.obligation),
        new PublicKey(prime.resolved.obligation),
      );
      mutation = {
        expected: onre.resolved.obligation,
        actual: prime.resolved.obligation,
      };
      downstreamProgram = canonicalInner.programId;
    } else if (name === "unapproved-edge") {
      finalInstruction = replaceKey(
        jupiter.outerInstruction,
        new PublicKey(jupiter.policy),
        new PublicKey(text(jupiterWrongPolicy.policy, "wrong packed policy")),
      );
      mutation = {
        canonicalEdge: jupiter.edgeKey,
        canonicalPolicy: jupiter.policy,
        substitutedPolicy: jupiterWrongPolicy.policy,
      };
      downstreamProgram = jupiter.innerInstruction.programId;
    } else if (name === "extra-instruction") {
      finalInstruction = extraInstructionOuter(
        canonicalInner,
        canonicalBorrow.policy,
        delegated.publicKey,
      );
      mutation = { canonicalInstructionCount: 1, actualInstructionCount: 2 };
      downstreamProgram = canonicalInner.programId;
    } else if (name === "amount-cap-breach") {
      const max = buildPhaseTwoKaminoLaneOperations(
        onre,
        1_000_000_000_000n,
      ).find((operation) => operation.operation === "borrow");
      invariant(max, "max canonical borrow is absent");
      const maxData = Buffer.from(max.dataBase64, "base64");
      const breach = Buffer.from(maxData);
      breach.writeBigUInt64LE(1_000_000_000_001n, 8);
      const maxExecution = buildExactKaminoSquadsExecution({
        compiledPolicy: borrow,
        operation: "borrow",
        innerInstruction: new TransactionInstruction({
          programId: new PublicKey(max.programId),
          data: maxData,
          keys: max.accounts.map((account) => ({
            pubkey: new PublicKey(account.address),
            isSigner: account.signer,
            isWritable: account.writable,
          })),
        }),
        delegatedSigner: delegated.publicKey,
      });
      finalInstruction = replaceLastInnerData(
        maxExecution.outerInstruction,
        maxData,
        breach,
      );
      mutation = {
        dataOffset: 8,
        maximum: "1000000000000",
        actual: "1000000000001",
      };
      downstreamProgram = canonicalInner.programId;
    } else if (name === "signer-substitution") {
      finalInstruction = replaceKey(
        canonicalBorrow.outerInstruction,
        delegated.publicKey,
        admin.publicKey,
      );
      finalPayer = admin;
      mutation = {
        expected: delegated.publicKey.toBase58(),
        actual: admin.publicKey.toBase58(),
      };
      downstreamProgram = canonicalInner.programId;
    } else {
      const vault = new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault);
      const writable = canonicalInner.keys.find(
        (key) => key.isWritable && !key.pubkey.equals(vault),
      );
      invariant(writable, "canonical borrow has no non-vault writable role");
      const locallyBad = cloneInstruction(canonicalInner, (key) =>
        key.pubkey.equals(writable.pubkey)
          ? { ...key, isWritable: false }
          : key,
      );
      let message = "";
      try {
        assertExactCanonicalRoles(locallyBad, canonicalInner);
      } catch (error) {
        message = error instanceof Error ? error.message : String(error);
      }
      invariant(
        message.length > 0,
        "canonical builder did not reject writable-role substitution before signing",
      );
      preSign = {
        canonicalBuilderRejected: true,
        messageSha256: sha256(message),
      };
      let changed = 0;
      finalInstruction = cloneInstruction(
        canonicalBorrow.outerInstruction,
        (key) => {
          if (!key.pubkey.equals(writable.pubkey) || !key.isWritable)
            return key;
          changed += 1;
          return { ...key, isWritable: false };
        },
      );
      invariant(
        changed === 1,
        "independently constructed outer writable-role mutation was not exact",
      );
      mutation = {
        account: writable.pubkey.toBase58(),
        canonicalWritable: true,
        mutatedWritable: false,
      };
      downstreamProgram = canonicalInner.programId;
    }
    const prefixWires = prefix.map((policy, index) =>
      sign(
        admin,
        latest.value.blockhash,
        createInstruction(policy.createInstruction),
        `create-seed-${text(policy.seed, "prefix seed")}-${index}`,
      ),
    );
    const finalWire = sign(
      finalPayer,
      latest.value.blockhash,
      finalInstruction,
      `${name}-rejection`,
    );
    const wires = [...prefixWires, finalWire];
    invariant(
      wires.length <= MAX_TRANSACTIONS &&
        wires.every((wire) => wire.packetBytes <= PACKET_LIMIT),
      `${name} violates Helius bundle/packet limits`,
    );
    const simulationInspected = [
      ...new Set([
        RWA_MULTIPLY_ROUTE.squads.settings,
        RWA_MULTIPLY_ROUTE.squads.vault,
        text(target.policy, `${name} target policy`),
        ...canonicalInner.keys.map((key) => key.pubkey.toBase58()),
        ...(name === "unapproved-edge"
          ? jupiter.innerInstruction.keys.map((key) => key.pubkey.toBase58())
          : []),
      ]),
    ];
    const protectedInspected = [
      ...new Set([
        RWA_MULTIPLY_ROUTE.squads.settings,
        RWA_MULTIPLY_ROUTE.squads.vault,
        RWA_MULTIPLY_ROUTE.squads.assetAta,
        text(target.policy, `${name} target policy`),
        onre.resolved.obligation,
        onre.resolved.collateralCustody.address,
        onre.resolved.debtCustody.address,
        ...(name === "unapproved-edge"
          ? [
              jupiter.innerInstruction.keys[3]!.pubkey.toBase58(),
              jupiter.innerInstruction.keys[6]!.pubkey.toBase58(),
            ]
          : []),
      ]),
    ];
    const before = await connection.getMultipleAccountsInfoAndContext(
      protectedInspected.map((address) => new PublicKey(address)),
      { commitment: "confirmed", minContextSlot: latest.context.slot },
    );
    const statusesBefore = await connection.getSignatureStatuses(
      wires.map((wire) => wire.signature),
      { searchTransactionHistory: true },
    );
    invariant(
      statusesBefore.value.every((status) => status === null),
      `${name} signed wire is already present on chain`,
    );
    const response = await fetch(rpcUrl, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: `rwa-phase2-negative-${name}`,
        method: "simulateBundle",
        params: [
          {
            encodedTransactions: wires.map((wire) =>
              Buffer.from(wire.wire).toString("base64"),
            ),
          },
          {
            preExecutionAccountsConfigs: wires.map(() => ({
              addresses: simulationInspected,
              encoding: "base64",
            })),
            postExecutionAccountsConfigs: wires.map(() => ({
              addresses: simulationInspected,
              encoding: "base64",
            })),
            skipSigVerify: false,
            simulationBank: { commitment: { commitment: "confirmed" } },
            transactionEncoding: "base64",
            replaceRecentBlockhash: false,
          },
        ],
      }),
    });
    invariant(response.ok, `${name} Helius HTTP ${response.status}`);
    const provider = providerResult((await response.json()) as unknown);
    const statusesAfter = await connection.getSignatureStatuses(
      wires.map((wire) => wire.signature),
      { searchTransactionHistory: true },
    );
    invariant(
      statusesAfter.value.every((status) => status === null),
      `${name} simulation unexpectedly landed a signed wire`,
    );
    const after = await connection.getMultipleAccountsInfoAndContext(
      protectedInspected.map((address) => new PublicKey(address)),
      { commitment: "confirmed", minContextSlot: before.context.slot },
    );
    invariant(
      stateSha256(before.value) === stateSha256(after.value),
      `${name} changed confirmed chain state`,
    );
    const rows = array(
      provider.transactionResults,
      `${name} provider rows`,
    ).map((entry) => object(entry, `${name} provider row`));
    invariant(
      rows.length === wires.length &&
        rows.slice(0, -1).every((row) => row.err === null),
      `${name} prefix failed before its intended final rejection`,
    );
    const final = rows.at(-1)!;
    invariant(final.err !== null, `${name} final mutation was accepted`);
    invariant(
      final.preExecutionAccountsSha256 === final.postExecutionAccountsSha256,
      `${name} rejected mutation changed a captured account inside the atomic simulation`,
    );
    const logs = array(final.logs, `${name} final logs`).map((line) =>
      text(line, `${name} log`),
    );
    invariant(
      name === "writable-role-substitution" || isSquadsTopLevelCustomError(final.err),
      `${name} did not return a top-level Squads custom error`,
    );
    results.push({
      name,
      accepted: false,
      rejectionLayer:
        name === "writable-role-substitution"
          ? "canonical-go-builder"
          : "Squads policy",
      mutation,
      canonicalPreSign: preSign,
      target: {
        name: target.name,
        logicalName: target.logicalName,
        seed: target.seed,
        policy: target.policy,
      },
      prefixSeeds: prefix.map((policy) => policy.seed),
      downstreamProgramNotInvoked: downstreamProgram.toBase58(),
      simulation: {
        method: "simulateBundle",
        skipSigVerify: false,
        simulationBankCommitment: "confirmed",
        replaceRecentBlockhash: false,
        contextSlot: provider.contextSlot,
        err: final.err,
        logsSha256: final.logsSha256,
        capturedAddresses: simulationInspected,
        atomicPreStateSha256: final.preExecutionAccountsSha256,
        atomicPostStateSha256: final.postExecutionAccountsSha256,
        downstreamBoundary:
          name === "writable-role-substitution"
            ? "runtime rejected independently signed role mutation"
            : "top-level Squads custom error",
      },
      transactions: wires.map((wire) => ({
        role: wire.role,
        signature: wire.signature,
        packetBytes: wire.packetBytes,
        transactionBase64: Buffer.from(wire.wire).toString("base64"),
        transactionSha256: sha256(wire.wire),
      })),
      signatureAbsentOnChain:
        statusesBefore.value.every((status) => status === null) &&
        statusesAfter.value.every((status) => status === null),
      chainPreStateSha256: stateSha256(before.value),
      chainPostStateSha256: stateSha256(after.value),
      confirmedReadbackSlot: after.context.slot,
    });
  }
  writeFileSync(
    OUTPUT,
    `${JSON.stringify({ schema: "loyal-backyard-rwa-phase2-negative-bundles/v1", verdict: "PASS", broadcast: false, signedUnsent: true, cluster: "mainnet-beta", commitment: "confirmed", compiledArtifactSha256: sha256(artifactBytes), hardLimits: { prefixSeedMaximum: 77, heliusBundleTransactionMaximum: MAX_TRANSACTIONS, signedWireMaximumBytes: PACKET_LIMIT }, cases: results, conclusion: "All seven signed-but-unsent mutations were rejected by Squads before their downstream Kamino or Jupiter program, and confirmed readback was unchanged." }, null, 2)}\n`,
    { flag: "wx", mode: 0o600 },
  );
  console.log(
    JSON.stringify({
      verdict: "PASS",
      output: OUTPUT,
      cases: results.map((row) => row.name),
    }),
  );
}
main().catch((error) => {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  const message = error instanceof Error ? error.message : String(error);
  console.error(rpcUrl ? message.replaceAll(rpcUrl, "<rpc>") : message);
  process.exitCode = 1;
});
