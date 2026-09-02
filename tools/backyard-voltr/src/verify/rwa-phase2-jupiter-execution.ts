/**
 * Exact, fail-closed construction of a Jupiter v0 Squads policy execution.
 *
 * This is deliberately not a Jupiter route builder.  It consumes the frozen
 * Phase-2 compiler row and the matching resolved-header row, proves that they
 * describe the same constrained instruction, then wraps that exact inner
 * instruction in Squads `executePolicyPayloadSync`.
 */
import { createHash } from "node:crypto";

import { executePolicyPayloadSync } from "@loyal-labs/loyal-smart-accounts-core/internal";
import {
  AddressLookupTableAccount,
  Connection,
  Keypair,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

const PACKET_LIMIT = 1_232;

type Json = Record<string, unknown>;
type HeaderBoundary = Readonly<{
  authority: number;
  source: number;
  destination: number;
  sourceMint: number;
  destinationMint: number;
  sourceProgram: number;
  destinationProgram: number;
  slippage: number;
  platformFee: number;
}>;
type ParsedHeader = Readonly<{
  key: string;
  from: string;
  to: string;
  lookupTables: readonly string[];
  indexes: HeaderBoundary;
  instruction: TransactionInstruction;
}>;
type ParsedEdge = Readonly<{
  from: string;
  to: string;
  constraintIndex: number;
  authorityIndex: number;
  sourceIndex: number;
  destinationIndex: number;
  sourceMintIndex: number;
  destinationMintIndex: number;
  sourceTokenProgramIndex: number;
  destinationTokenProgramIndex: number;
  authority: string;
  sourceCustody: string;
  destinationCustody: string;
  sourceMint: string;
  destinationMint: string;
  sourceTokenProgram: string;
  destinationTokenProgram: string;
}>;

export type ExactJupiterSquadsExecution = Readonly<{
  edgeKey: string;
  policy: string;
  policySeed: string;
  constraintIndex: number;
  innerInstruction: TransactionInstruction;
  outerInstruction: TransactionInstruction;
  lookupTables: readonly AddressLookupTableAccount[];
  lookupTableAddresses: readonly string[];
  compiledPayloadSha256: string;
  compiledPayloadBase64: string;
  compiledInnerAccounts: readonly Readonly<{ address: string; writable: boolean; signer: false }>[];
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function object(value: unknown, label: string): Json {
  invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`);
  return value as Json;
}

function array(value: unknown, label: string): unknown[] {
  invariant(Array.isArray(value), `${label} is not an array`);
  return value;
}

function text(value: unknown, label: string): string {
  invariant(typeof value === "string" && value.length > 0, `${label} is missing`);
  return value;
}

function integer(value: unknown, label: string): number {
  invariant(typeof value === "number" && Number.isSafeInteger(value) && value >= 0, `${label} is not a non-negative integer`);
  return value;
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function base64(value: unknown, label: string): Buffer {
  const encoded = text(value, label);
  const decoded = Buffer.from(encoded, "base64");
  invariant(decoded.length > 0 && decoded.toString("base64") === encoded, `${label} is not canonical base64`);
  return decoded;
}

function index(value: Json, key: keyof HeaderBoundary, label: string): number {
  return integer(value[key], `${label}.${key}`);
}

function parseHeader(value: unknown): ParsedHeader {
  const row = object(value, "Jupiter header row");
  const key = text(row.key, "Jupiter header key");
  const [from, to, ...extra] = key.split("->");
  invariant(from && to && extra.length === 0, `Jupiter header key ${key} is malformed`);
  invariant(row.pass === true, `${key} header is not accepted`);
  const source = object(row.source, `${key}.source`);
  const destination = object(row.destination, `${key}.destination`);
  invariant(text(source.symbol, `${key}.source.symbol`) === from && text(destination.symbol, `${key}.destination.symbol`) === to,
    `${key} source/destination symbols drifted`);
  const header = object(row.header, `${key}.header`);
  const indexes = object(header.indexes, `${key}.header.indexes`);
  const boundary: HeaderBoundary = {
    authority: index(indexes, "authority", `${key}.header.indexes`),
    source: index(indexes, "source", `${key}.header.indexes`),
    destination: index(indexes, "destination", `${key}.header.indexes`),
    sourceMint: index(indexes, "sourceMint", `${key}.header.indexes`),
    destinationMint: index(indexes, "destinationMint", `${key}.header.indexes`),
    sourceProgram: index(indexes, "sourceProgram", `${key}.header.indexes`),
    destinationProgram: index(indexes, "destinationProgram", `${key}.header.indexes`),
    slippage: index(indexes, "slippage", `${key}.header.indexes`),
    platformFee: index(indexes, "platformFee", `${key}.header.indexes`),
  };
  const instruction = object(row.instruction, `${key}.instruction`);
  const data = base64(instruction.dataBase64, `${key}.instruction.dataBase64`);
  invariant(sha256(data) === text(instruction.dataSha256, `${key}.instruction.dataSha256`), `${key} instruction data hash drifted`);
  invariant(text(instruction.programId, `${key}.instruction.programId`) === RWA_MULTIPLY_ROUTE.programs.jupiter,
    `${key} is not a Jupiter instruction`);
  const keys = array(instruction.accounts, `${key}.instruction.accounts`).map((entry, entryIndex) => {
    const account = object(entry, `${key}.instruction.accounts[${entryIndex}]`);
    invariant(typeof account.isSigner === "boolean" && typeof account.isWritable === "boolean",
      `${key} account ${entryIndex} roles are malformed`);
    return {
      pubkey: new PublicKey(text(account.pubkey, `${key}.instruction.accounts[${entryIndex}].pubkey`)),
      isSigner: account.isSigner,
      isWritable: account.isWritable,
    };
  });
  invariant(keys.length === integer(header.accountCount, `${key}.header.accountCount`), `${key} header account count drifted`);
  const at = (position: number, label: string) => {
    const account = keys[position];
    invariant(account, `${key} ${label} index ${position} is outside the instruction`);
    return account;
  };
  const sourceMint = text(source.mint, `${key}.source.mint`);
  const destinationMint = text(destination.mint, `${key}.destination.mint`);
  const sourceProgram = text(source.tokenProgram, `${key}.source.tokenProgram`);
  const destinationProgram = text(destination.tokenProgram, `${key}.destination.tokenProgram`);
  const sourceAta = text(source.ata, `${key}.source.ata`);
  const destinationAta = text(destination.ata, `${key}.destination.ata`);
  const requireRole = (position: number, expected: string, signer: boolean, writable: boolean, label: string) => {
    const account = at(position, label);
    invariant(account.pubkey.toBase58() === expected && account.isSigner === signer && account.isWritable === writable,
      `${key} ${label} boundary drifted`);
  };
  requireRole(boundary.authority, RWA_MULTIPLY_ROUTE.squads.vault, true, false, "authority");
  requireRole(boundary.source, sourceAta, false, true, "source custody");
  requireRole(boundary.destination, destinationAta, false, true, "destination custody");
  requireRole(boundary.sourceMint, sourceMint, false, false, "source mint");
  requireRole(boundary.destinationMint, destinationMint, false, false, "destination mint");
  requireRole(boundary.sourceProgram, sourceProgram, false, false, "source token program");
  requireRole(boundary.destinationProgram, destinationProgram, false, false, "destination token program");
  invariant(keys.filter((entry) => entry.isSigner).length === 1, `${key} has a signer besides the Squads vault`);
  invariant(data.length > boundary.platformFee && data.length >= boundary.slippage + 2,
    `${key} Jupiter data does not contain constrained slippage/fee fields`);
  invariant(data.readUInt16LE(boundary.slippage) <= RWA_MULTIPLY_ROUTE.assets.maxSlippageBps && data[boundary.platformFee] === 0,
    `${key} slippage or platform-fee boundary drifted`);
  const lookupTables = array(row.lookupTables, `${key}.lookupTables`).map((entry, entryIndex) => text(entry, `${key}.lookupTables[${entryIndex}]`));
  invariant(lookupTables.length > 0 && new Set(lookupTables).size === lookupTables.length, `${key} lookup tables are incomplete or duplicate`);
  return { key, from, to, lookupTables, indexes: boundary, instruction: new TransactionInstruction({
    programId: new PublicKey(RWA_MULTIPLY_ROUTE.programs.jupiter), data, keys,
  }) };
}

function parseEdge(value: unknown, label: string): ParsedEdge {
  const edge = object(value, label);
  return {
    from: text(edge.from, `${label}.from`), to: text(edge.to, `${label}.to`),
    constraintIndex: integer(edge.constraintIndex, `${label}.constraintIndex`),
    authorityIndex: integer(edge.authorityIndex, `${label}.authorityIndex`),
    sourceIndex: integer(edge.sourceIndex, `${label}.sourceIndex`),
    destinationIndex: integer(edge.destinationIndex, `${label}.destinationIndex`),
    sourceMintIndex: integer(edge.sourceMintIndex, `${label}.sourceMintIndex`),
    destinationMintIndex: integer(edge.destinationMintIndex, `${label}.destinationMintIndex`),
    sourceTokenProgramIndex: integer(edge.sourceTokenProgramIndex, `${label}.sourceTokenProgramIndex`),
    destinationTokenProgramIndex: integer(edge.destinationTokenProgramIndex, `${label}.destinationTokenProgramIndex`),
    authority: text(edge.authority, `${label}.authority`),
    sourceCustody: text(edge.sourceCustody, `${label}.sourceCustody`),
    destinationCustody: text(edge.destinationCustody, `${label}.destinationCustody`),
    sourceMint: text(edge.sourceMint, `${label}.sourceMint`),
    destinationMint: text(edge.destinationMint, `${label}.destinationMint`),
    sourceTokenProgram: text(edge.sourceTokenProgram, `${label}.sourceTokenProgram`),
    destinationTokenProgram: text(edge.destinationTokenProgram, `${label}.destinationTokenProgram`),
  };
}

function policyAddress(seed: bigint): string {
  const bytes = Buffer.alloc(8);
  bytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync([
    Buffer.from("smart_account"), Buffer.from("policy"),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(), bytes,
  ], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0].toBase58();
}

function constraintMatchesHeader(constraintValue: unknown, header: ParsedHeader, edge: ParsedEdge): boolean {
  try {
    const constraint = object(constraintValue, `${header.key} constraint`);
    if (text(constraint.programId, `${header.key} constraint.programId`) !== header.instruction.programId.toBase58()) return false;
    const expected = [
      [edge.authorityIndex, edge.authority], [edge.sourceIndex, edge.sourceCustody],
      [edge.destinationIndex, edge.destinationCustody], [edge.sourceMintIndex, edge.sourceMint],
      [edge.destinationMintIndex, edge.destinationMint], [edge.sourceTokenProgramIndex, edge.sourceTokenProgram],
      [edge.destinationTokenProgramIndex, edge.destinationTokenProgram],
    ] as const;
    const accountConstraints = array(constraint.accountPubkeys, `${header.key} constraint.accountPubkeys`);
    for (const [accountIndex, expectedPubkey] of expected) {
      const found = accountConstraints.find((entry) => {
        const row = object(entry, `${header.key} constraint account`);
        const pubkeys = array(row.pubkeys, `${header.key} constraint account pubkeys`);
        return integer(row.index, `${header.key} constraint account index`) === accountIndex
          && pubkeys.length === 1 && text(pubkeys[0], `${header.key} constraint account pubkey`) === expectedPubkey;
      });
      if (!found) return false;
    }
    const dataConstraints = array(constraint.data, `${header.key} constraint.data`);
    for (const entry of dataConstraints) {
      const data = object(entry, `${header.key} constraint.data item`);
      const kind = text(data.kind, `${header.key} constraint.data kind`);
      const offset = integer(data.offset, `${header.key} constraint.data offset`);
      if (kind === "slice-equals") {
        const expectedBytes = Buffer.from(text(data.valueHex, `${header.key} slice value`), "hex");
        if (expectedBytes.length === 0 || !header.instruction.data.subarray(offset, offset + expectedBytes.length).equals(expectedBytes)) return false;
      } else if (kind === "u64-less-than-or-equal") {
        if (offset + 8 > header.instruction.data.length || header.instruction.data.readBigUInt64LE(offset) > BigInt(integer(data.value, `${header.key} u64 value`))) return false;
      } else if (kind === "u16-less-than-or-equal") {
        if (offset + 2 > header.instruction.data.length || header.instruction.data.readUInt16LE(offset) > integer(data.value, `${header.key} u16 value`)) return false;
      } else if (kind === "u8-equals") {
        if (offset >= header.instruction.data.length || header.instruction.data[offset] !== integer(data.value, `${header.key} u8 value`)) return false;
      } else return false;
    }
    return true;
  } catch {
    return false;
  }
}

function compileInner(instruction: TransactionInstruction) {
  const accounts: Array<{ pubkey: PublicKey; isWritable: boolean; isSigner: false }> = [];
  const indexOf = (pubkey: PublicKey, writable: boolean): number => {
    const prior = accounts.findIndex((account) => account.pubkey.equals(pubkey));
    if (prior >= 0) {
      const account = accounts[prior];
      invariant(account, "inner account disappeared");
      account.isWritable ||= writable;
      return prior;
    }
    invariant(accounts.length < 255, "inner account table exceeds Squads u8 limit");
    accounts.push({ pubkey, isWritable: writable, isSigner: false });
    return accounts.length - 1;
  };
  invariant(instruction.keys.every((key) => !key.isSigner || key.pubkey.toBase58() === RWA_MULTIPLY_ROUTE.squads.vault),
    "inner Jupiter instruction has a signer other than the Squads vault");
  const indexes = instruction.keys.map((key) => indexOf(key.pubkey, key.isWritable));
  invariant(instruction.data.length <= 65_535, "inner Jupiter instruction data exceeds Squads u16 limit");
  const dataLength = Buffer.alloc(2);
  dataLength.writeUInt16LE(instruction.data.length);
  return {
    accounts,
    // The sync executor consumes `numSigners` before resolving the compiled
    // inner-account table. Indices are therefore relative to `accounts`, not
    // to the outer remaining-account slice that also carries the executor.
    bytes: Buffer.concat([
      Buffer.from([1, indexOf(instruction.programId, false), indexes.length, ...indexes]), dataLength, instruction.data,
    ]),
  } as const;
}

/**
 * Build the exact Squads wrapper and resolve every declared ALT.  The caller
 * owns signing and simulation so it can place this transaction in a sequential
 * signed-unsent Helius bundle with its prerequisite PolicyCreate transaction.
 */
export async function buildExactJupiterSquadsExecution(input: Readonly<{
  connection: Connection;
  compiledPolicy: unknown;
  headerRow: unknown;
  delegatedSigner: PublicKey;
}>): Promise<ExactJupiterSquadsExecution> {
  const policy = object(input.compiledPolicy, "compiled packed Jupiter policy");
  const header = parseHeader(input.headerRow);
  invariant(text(policy.logicalName, "compiled policy logicalName").startsWith("swap/"), "compiled policy is not a Jupiter policy");
  const policySeed = text(policy.seed, "compiled policy seed");
  invariant(/^[1-9][0-9]*$/.test(policySeed), "compiled policy seed is malformed");
  const policyAddressText = text(policy.policy, "compiled policy PDA");
  invariant(policyAddress(BigInt(policySeed)) === policyAddressText, "compiled policy PDA does not derive from the RWA Settings and seed");
  const edges = array(policy.swapEdges, "compiled policy swapEdges").map((entry, entryIndex) => parseEdge(entry, `compiled policy swapEdges[${entryIndex}]`));
  const matches = edges.filter((edge) => edge.from === header.from && edge.to === header.to);
  invariant(matches.length === 1, `${header.key} is not carried exactly once by the packed policy`);
  const edge = matches[0]!;
  invariant(edge.authorityIndex === header.indexes.authority && edge.sourceIndex === header.indexes.source
    && edge.destinationIndex === header.indexes.destination && edge.sourceMintIndex === header.indexes.sourceMint
    && edge.destinationMintIndex === header.indexes.destinationMint && edge.sourceTokenProgramIndex === header.indexes.sourceProgram
    && edge.destinationTokenProgramIndex === header.indexes.destinationProgram,
  `${header.key} compiled edge indexes disagree with the resolved header: ${JSON.stringify({ edge, header: header.indexes })}`);
  const constraints = array(policy.constraints, "compiled policy constraints");
  invariant(constraints.length === integer(policy.constraintCount, "compiled policy constraintCount"), "compiled policy constraint count drifted");
  const semanticMatches = constraints.map((constraint, constraintIndex) => ({ constraintIndex, exact: constraintMatchesHeader(constraint, header, edge) }))
    .filter(({ exact }) => exact);
  invariant(semanticMatches.length === 1, `${header.key} does not have exactly one matching packed policy constraint`);
  const constraintIndex = semanticMatches[0]!.constraintIndex;
  invariant(edge.constraintIndex === constraintIndex,
    `${header.key} edge constraintIndex ${edge.constraintIndex} does not name its exact packed constraint ${constraintIndex}`);
  const lookupTables = await Promise.all(header.lookupTables.map(async (tableAddress) => {
    const response = await input.connection.getAddressLookupTable(new PublicKey(tableAddress), { commitment: "confirmed" });
    invariant(response.value, `${header.key} lookup table ${tableAddress} is absent`);
    invariant(response.value.key.toBase58() === tableAddress && response.value.state.addresses.length > 0,
      `${header.key} lookup table ${tableAddress} is malformed`);
    return response.value;
  }));
  const compiled = compileInner(header.instruction);
  const outerInstruction = executePolicyPayloadSync({
    feePayer: input.delegatedSigner,
    policy: new PublicKey(policyAddressText),
    accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
    numSigners: 1,
    policyPayload: {
      __kind: "ProgramInteraction",
      fields: [{
        instructionConstraintIndices: new Uint8Array([constraintIndex]),
        transactionPayload: {
          __kind: "SyncTransaction",
          fields: [{ accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex, instructions: compiled.bytes }],
        },
      }],
    },
    // The delegated executor is the first required remaining signer; Squads
    // removes that signer prefix before resolving the compiled indices.
    instruction_accounts: [{ pubkey: input.delegatedSigner, isSigner: true, isWritable: false }, ...compiled.accounts],
    programId: new PublicKey(RWA_MULTIPLY_ROUTE.squads.program),
  });
  return {
    edgeKey: header.key,
    policy: policyAddressText,
    policySeed,
    constraintIndex,
    innerInstruction: header.instruction,
    outerInstruction,
    lookupTables,
    lookupTableAddresses: header.lookupTables,
    compiledPayloadSha256: sha256(compiled.bytes),
    compiledPayloadBase64: Buffer.from(compiled.bytes).toString("base64"),
    compiledInnerAccounts: compiled.accounts.map((account) => ({ address: account.pubkey.toBase58(), writable: account.isWritable, signer: false as const })),
  };
}

/** Sign the constructed v0 execution and enforce that its declared ALTs are used. */
export function signExactJupiterSquadsExecution(input: Readonly<{
  execution: ExactJupiterSquadsExecution;
  payer: Keypair;
  recentBlockhash: string;
}>): Readonly<{ transaction: VersionedTransaction; wire: Uint8Array; packetBytes: number }> {
  const message = new TransactionMessage({
    payerKey: input.payer.publicKey,
    recentBlockhash: input.recentBlockhash,
    instructions: [input.execution.outerInstruction],
  }).compileToV0Message([...input.execution.lookupTables]);
  const used = new Set(message.addressTableLookups.map((lookup) => lookup.accountKey.toBase58()));
  for (const tableAddress of input.execution.lookupTableAddresses) {
    invariant(used.has(tableAddress), `${input.execution.edgeKey} did not use declared lookup table ${tableAddress}`);
  }
  const transaction = new VersionedTransaction(message);
  transaction.sign([input.payer]);
  const wire = transaction.serialize();
  invariant(wire.length <= PACKET_LIMIT, `${input.execution.edgeKey} Squads Jupiter packet is ${wire.length} bytes; limit is ${PACKET_LIMIT}`);
  return { transaction, wire, packetBytes: wire.length };
}
