import { createHash } from "node:crypto";

import { getMint } from "@solana/spl-token";
import { Connection, Keypair, PublicKey, Transaction, TransactionInstruction } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { readRwaMultiplyCatalog } from "./rwa-multiply-catalog-resolver.js";
import { catalogCustodies } from "./rwa-multiply-custodies.js";

const LEGACY_SHARED = Buffer.from("c1209b3341d69c81", "hex");
const SHARED_V2 = Buffer.from([209, 152, 83, 147, 124, 254, 216, 233]);
const ROUTE = Buffer.from("e517cb977ae3ad2a", "hex");
const DEFAULT_MIN_REQUEST_INTERVAL_MS = 1_250;
const DEFAULT_MAX_NETWORK_EDGES = 4;
const DEFAULT_RATE_LIMIT_COOLDOWN_MS = 60_000;
export const PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX = [
  "01010000007400640001",
  "02010000007400640001",
] as const;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function record(value: unknown, label: string): Record<string, unknown> {
  invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`);
  return value as Record<string, unknown>;
}

function string(value: unknown, label: string): string {
  invariant(typeof value === "string", `${label} is not a string`);
  return value;
}

function array(value: unknown, label: string): unknown[] {
  invariant(Array.isArray(value), `${label} is not an array`);
  return value;
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function integer(value: unknown, label: string): number {
  invariant(typeof value === "number" && Number.isSafeInteger(value) && value >= 0,
    `${label} is not a non-negative safe integer`);
  return value;
}

function edgeKeysSha256(keys: readonly string[]): string {
  return sha256(Buffer.from(JSON.stringify([...keys].sort())));
}

class JupiterHttpError extends Error {
  readonly status: number;
  readonly retryAfterMs: number | null;
  readonly stage: "quote" | "swap-instructions";

  constructor(input: Readonly<{ edge: string; stage: "quote" | "swap-instructions"; status: number; body: string; retryAfterMs: number | null }>) {
    super(`${input.edge} ${input.stage} request failed with HTTP ${input.status}: ${input.body.slice(0, 160)}`);
    this.name = "JupiterHttpError";
    this.status = input.status;
    this.retryAfterMs = input.retryAfterMs;
    this.stage = input.stage;
  }
}

function retryAfterMs(value: string | null): number | null {
  if (!value) return null;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) return Math.ceil(seconds * 1_000);
  const date = Date.parse(value);
  return Number.isFinite(date) ? Math.max(0, date - Date.now()) : null;
}

/**
 * Measures the exact Jupiter instruction inside a transaction that has an
 * actual signature, without needing the Squads PDA's private key. The second
 * required signature slot is deliberately left for the PDA, so the result is
 * a transport-size measurement, never a broadcastable transaction.
 */
export function measureSignedJupiterPacket(edgeKey: string, instruction: TransactionInstruction) {
  const payer = Keypair.fromSeed(createHash("sha256").update(`backyard-rwa-jupiter-measure:${edgeKey}`).digest());
  const transaction = new Transaction({
    feePayer: payer.publicKey,
    recentBlockhash: PublicKey.default.toBase58(),
  }).add(instruction);
  transaction.setSigners(payer.publicKey, new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault));
  transaction.partialSign(payer);
  const message = transaction.serializeMessage();
  const signatures = transaction.signatures.map(({ signature }) => signature ?? Buffer.alloc(64));
  const shortvec = (value: number) => {
    const bytes: number[] = [];
    let remainder = value;
    while (true) {
      const next = remainder & 0x7f;
      remainder >>>= 7;
      bytes.push(remainder === 0 ? next : next | 0x80);
      if (remainder === 0) return Buffer.from(bytes);
    }
  };
  // web3's Transaction.serialize deliberately refuses packets over 1232
  // bytes. The resolver needs to *measure and reject* those packets, so build
  // the same shortvec/signature/message wire directly after partial signing.
  const bytes = Buffer.concat([shortvec(signatures.length), ...signatures, message]);
  const signedSignatureCount = transaction.signatures.filter(({ signature }) => signature !== null).length;
  return {
    kind: "ephemeral-fee-payer-partially-signed-transport-measurement" as const,
    packetBytes: bytes.length,
    requiredSignatureCount: transaction.signatures.length,
    signedSignatureCount,
    squadsPdaSignaturePending: true,
    broadcastable: false,
    packetSha256: sha256(bytes),
  };
}

export function catalogSwapEdges() {
  const assets = new Map(catalogCustodies().map((value) => [value.symbol, value]));
  const edges = readRwaMultiplyCatalog().swapEdges.map(({ from, to }) => {
    const source = assets.get(from);
    const destination = assets.get(to);
    invariant(source && destination, `swap edge ${from}->${to} references an unknown asset`);
    return { key: `${from}->${to}`, from, to, source, destination };
  });
  invariant(edges.length === 52 && new Set(edges.map(({ key }) => key)).size === 52,
    "swap catalog is not exactly 52 unique directed edges");
  return edges;
}

export function phaseOnePrimeUsdcSwapEdges() {
  const wanted = new Set(["USDC->PRIME", "PRIME->USDC"]);
  const edges = catalogSwapEdges().filter(({ key }) => wanted.has(key));
  invariant(edges.length === 2 && edges.every(({ key }) => wanted.delete(key)) && wanted.size === 0,
    "Phase 1 PRIME/USDC swap graph is not exactly bidirectional");
  return edges;
}

function instructionFromJson(value: unknown, label: string): TransactionInstruction {
  const raw = record(value, label);
  const data = typeof raw.data === "string"
    ? raw.data
    : string(raw.dataBase64, `${label}.dataBase64`);
  return new TransactionInstruction({
    programId: new PublicKey(string(raw.programId, `${label}.programId`)),
    data: Buffer.from(data, "base64"),
    keys: array(raw.accounts, `${label}.accounts`).map((entry, index) => {
      const account = record(entry, `${label}.accounts[${index}]`);
      invariant(typeof account.isSigner === "boolean" && typeof account.isWritable === "boolean",
        `${label}.accounts[${index}] flags are malformed`);
      return { pubkey: new PublicKey(string(account.pubkey, `${label}.accounts[${index}].pubkey`)),
        isSigner: account.isSigner, isWritable: account.isWritable };
    }),
  });
}

type SwapEdge = ReturnType<typeof catalogSwapEdges>[number];
type JupiterRow = Record<string, unknown>;

type SanitizedInstructionShape = Readonly<{
  programId: string;
  discriminatorHex: string;
  dataBase64: string;
  dataSha256: string;
  dataBytes: number;
  accounts: readonly Readonly<{ index: number; pubkey: string; isSigner: boolean; isWritable: boolean }>[];
}>;

function sanitizeInstructionShape(value: unknown, label: string): SanitizedInstructionShape {
  const instruction = instructionFromJson(value, label);
  return {
    programId: instruction.programId.toBase58(),
    discriminatorHex: Buffer.from(instruction.data.subarray(0, 8)).toString("hex"),
    // Instruction data and account metas are public transaction payload, but
    // deliberately omit Jupiter's free-form quote/response fields.
    dataBase64: Buffer.from(instruction.data).toString("base64"),
    dataSha256: sha256(instruction.data),
    dataBytes: instruction.data.length,
    accounts: instruction.keys.map(({ pubkey, isSigner, isWritable }, index) =>
      ({ index, pubkey: pubkey.toBase58(), isSigner, isWritable })),
  };
}

function exactBoundaryPositions(shape: SanitizedInstructionShape, edge: SwapEdge) {
  const matching = (value: string) => shape.accounts
    .filter(({ pubkey }) => pubkey === value)
    .map(({ index, isSigner, isWritable }) => ({ index, isSigner, isWritable }));
  return {
    sourceMint: matching(edge.source.mint),
    sourceTokenProgram: matching(edge.source.tokenProgram),
    destinationMint: matching(edge.destination.mint),
    destinationTokenProgram: matching(edge.destination.tokenProgram),
    authority: matching(RWA_MULTIPLY_ROUTE.squads.vault),
    sourceAta: matching(edge.source.ata),
    destinationAta: matching(edge.destination.ata),
  } as const;
}

function sanitizeRejectedResponseShape(body: Record<string, unknown>, edge: SwapEdge) {
  const optionalInstruction = (value: unknown, label: string) => value === null || value === undefined
    ? null : sanitizeInstructionShape(value, label);
  const swapInstruction = sanitizeInstructionShape(body.swapInstruction, `${edge.key}.swapInstruction`);
  return {
    swapInstruction,
    routeBoundaryPositions: exactBoundaryPositions(swapInstruction, edge),
    setupInstructions: array(body.setupInstructions ?? [], `${edge.key}.setupInstructions`)
      .map((value, index) => sanitizeInstructionShape(value, `${edge.key}.setupInstructions[${index}]`)),
    otherInstructions: array(body.otherInstructions ?? [], `${edge.key}.otherInstructions`)
      .map((value, index) => sanitizeInstructionShape(value, `${edge.key}.otherInstructions[${index}]`)),
    cleanupInstruction: optionalInstruction(body.cleanupInstruction, `${edge.key}.cleanupInstruction`),
  } as const;
}

function validateAuxiliaryInstructionBoundary(body: Record<string, unknown>, edge: SwapEdge) {
  const setupInstructionCount = array(body.setupInstructions ?? [], `${edge.key}.setupInstructions`).length;
  const otherInstructionCount = array(body.otherInstructions ?? [], `${edge.key}.otherInstructions`).length;
  const cleanupInstructionPresent = body.cleanupInstruction !== null && body.cleanupInstruction !== undefined;
  // There is intentionally no allowlist yet. Adding one must be an explicit
  // policy-family change, not an accidental acceptance of a Jupiter response.
  invariant(setupInstructionCount === 0 && otherInstructionCount === 0 && !cleanupInstructionPresent,
    `${edge.key} has unapproved auxiliary instructions: setup=${setupInstructionCount}, other=${otherInstructionCount}, cleanup=${cleanupInstructionPresent}`);
  return { setupInstructionCount, otherInstructionCount, cleanupInstructionPresent, explicitlyConstrained: false } as const;
}

function cachedRowForEdge(value: unknown, edge: SwapEdge): JupiterRow | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  const row = value as JupiterRow;
  if (row.pass !== true || row.key !== edge.key) return null;
  try {
    const source = record(row.source, `${edge.key}.cached.source`);
    const destination = record(row.destination, `${edge.key}.cached.destination`);
    invariant(source.symbol === edge.source.symbol && source.mint === edge.source.mint
      && source.tokenProgram === edge.source.tokenProgram && source.ata === edge.source.ata,
    `${edge.key} cached source boundary drifted`);
    invariant(destination.symbol === edge.destination.symbol && destination.mint === edge.destination.mint
      && destination.tokenProgram === edge.destination.tokenProgram && destination.ata === edge.destination.ata,
    `${edge.key} cached destination boundary drifted`);
    const quote = record(row.quote, `${edge.key}.cached.quote`);
    const instruction = instructionFromJson(row.instruction, `${edge.key}.cached.instruction`);
    const header = validateJupiterHeader({
      instruction,
      sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint,
      sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata,
      sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: BigInt(string(quote.inAmountRaw, `${edge.key}.cached.quote.inAmountRaw`)),
      outAmountRaw: BigInt(string(quote.outAmountRaw, `${edge.key}.cached.quote.outAmountRaw`)),
    });
    const lookupTables = array(row.lookupTables, `${edge.key}.cached.lookupTables`)
      .map((candidate, index) => string(candidate, `${edge.key}.cached.lookupTables[${index}]`));
    return {
      ...row,
      header,
      packet: measureSignedJupiterPacket(edge.key, instruction),
      lookupTables,
      auxiliaryInstructions: { setupInstructionCount: 0, otherInstructionCount: 0,
        cleanupInstructionPresent: false, explicitlyConstrained: false },
      observation: { source: "validated-cache" },
    };
  } catch {
    return null;
  }
}

export function cachedJupiterRowsByEdge(value: unknown): ReadonlyMap<string, JupiterRow> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return new Map();
  const evidence = value as Record<string, unknown>;
  if ((evidence.schema !== "loyal-backyard-rwa-jupiter-header-evidence/v1"
    && evidence.schema !== "loyal-backyard-rwa-jupiter-header-evidence/v2")
    || !Array.isArray(evidence.rows)) {
    return new Map();
  }
  const expected = catalogSwapEdges();
  const expectedKeys = new Set(expected.map(({ key }) => key));
  const rows = new Map<string, JupiterRow>();
  for (const value of evidence.rows) {
    if (value === null || typeof value !== "object" || Array.isArray(value)) continue;
    const row = value as JupiterRow;
    if (typeof row.key !== "string" || !expectedKeys.has(row.key) || rows.has(row.key)) continue;
    const edge = expected.find(({ key }) => key === row.key)!;
    const cached = cachedRowForEdge(row, edge);
    if (cached) rows.set(edge.key, cached);
  }
  return rows;
}

export function validateJupiterHeader(input: Readonly<{
  instruction: TransactionInstruction;
  sourceMint: string;
  destinationMint: string;
  sourceAta: string;
  destinationAta: string;
  sourceTokenProgram: string;
  destinationTokenProgram: string;
  amountRaw: bigint;
  outAmountRaw: bigint;
}>) {
  const ix = input.instruction;
  invariant(ix.programId.toBase58() === RWA_MULTIPLY_ROUTE.programs.jupiter,
    "swap instruction is not Jupiter v6");
  invariant(ix.data.length >= 28, "Jupiter instruction data is too short");
  const legacy = ix.data.subarray(0, 8).equals(LEGACY_SHARED);
  const v2 = ix.data.subarray(0, 8).equals(SHARED_V2);
  const route = ix.data.subarray(0, 8).equals(ROUTE);
  invariant(legacy || v2 || route, "unsupported Jupiter route discriminator");
  const sourceMintIndex = route
    ? ix.keys.findIndex(({ pubkey }, index) => index >= 9 && pubkey.toBase58() === input.sourceMint)
    : legacy ? 7 : 6;
  const sourceProgramIndex = route
    ? ix.keys.findIndex(({ pubkey }, index) => index >= 9 && pubkey.toBase58() === input.sourceTokenProgram)
    : legacy ? 0 : 8;
  invariant(sourceMintIndex >= 0 && sourceProgramIndex >= 0,
    "Jupiter Route does not expose the exact source mint/token-program boundary");
  // Jupiter's legacy SharedAccountsRoute normally uses account 0 for both
  // token programs. Distinct-program routes expose the pair at fixed accounts
  // 0 and 10. Their direction matters, so both variants are exact families,
  // not a search or a weakened boundary.
  const legacyCrossProgram = legacy && input.sourceTokenProgram !== input.destinationTokenProgram;
  const legacyCrossProgramForward = legacyCrossProgram
    && ix.keys[0]?.pubkey.toBase58() === input.sourceTokenProgram
    && ix.keys[10]?.pubkey.toBase58() === input.destinationTokenProgram;
  const legacyCrossProgramReverse = legacyCrossProgram
    && ix.keys[10]?.pubkey.toBase58() === input.sourceTokenProgram
    && ix.keys[0]?.pubkey.toBase58() === input.destinationTokenProgram;
  if (legacyCrossProgram) invariant(legacyCrossProgramForward || legacyCrossProgramReverse,
    "legacy SharedAccountsRoute lacks an exact cross-token-program layout");
  const legacySharedTokenProgram = legacy && !legacyCrossProgram;
  const legacySharedProgramAtZero = legacySharedTokenProgram
    && ix.keys[0]?.pubkey.toBase58() === input.sourceTokenProgram;
  const legacySharedToken2022AtTen = legacySharedTokenProgram
    && ix.keys[10]?.pubkey.toBase58() === input.sourceTokenProgram;
  if (legacySharedTokenProgram) invariant(legacySharedProgramAtZero || legacySharedToken2022AtTen,
    "legacy SharedAccountsRoute lacks an exact shared token-program layout");
  const layout = legacy
    ? { authority: 2, source: 3, destination: 6, sourceMint: 7, destinationMint: 8,
      sourceProgram: legacyCrossProgramReverse || legacySharedToken2022AtTen ? 10 : 0,
      destinationProgram: legacyCrossProgramForward || legacySharedToken2022AtTen ? 10 : 0,
      slippage: ix.data.length - 3, platformFee: ix.data.length - 1 }
    : v2 ? { authority: 1, source: 2, destination: 5, sourceMint: 6, destinationMint: 7,
      sourceProgram: 8, destinationProgram: 9, slippage: 25, platformFee: 27 }
    : { authority: 1, source: 2, destination: 3, sourceMint: sourceMintIndex, destinationMint: 5,
      sourceProgram: sourceProgramIndex, destinationProgram: 0,
      slippage: ix.data.length - 3, platformFee: ix.data.length - 1 };
  const expected = [
    [layout.authority, RWA_MULTIPLY_ROUTE.squads.vault, true, false],
    [layout.source, input.sourceAta, false, true],
    [layout.destination, input.destinationAta, false, true],
    [layout.sourceMint, input.sourceMint, false, false],
    [layout.destinationMint, input.destinationMint, false, false],
    [layout.sourceProgram, input.sourceTokenProgram, false, false],
    [layout.destinationProgram, input.destinationTokenProgram, false, false],
  ] as const;
  for (const [index, pubkey, signer, writable] of expected) {
    const key = ix.keys[index];
    invariant(key?.pubkey.toBase58() === pubkey && key.isSigner === signer && key.isWritable === writable,
      `Jupiter account boundary ${index} drifted`);
  }
  invariant(ix.keys.every((key, index) => !key.isSigner || index === layout.authority),
    "Jupiter instruction has an unexpected signer");
  invariant(!ix.keys.some(({ pubkey }) => pubkey.toBase58() === RWA_MULTIPLY_ROUTE.previousBackyardVault),
    "Jupiter instruction references the previous Backyard vault");
  invariant(ix.data.readBigUInt64LE(ix.data.length - 19) === input.amountRaw,
    "Jupiter input amount tail drifted");
  invariant(ix.data.readBigUInt64LE(ix.data.length - 11) === input.outAmountRaw,
    "Jupiter quoted output tail drifted");
  invariant(ix.data.readUInt16LE(layout.slippage) <= RWA_MULTIPLY_ROUTE.assets.maxSlippageBps,
    "Jupiter slippage exceeds the route boundary");
  invariant(ix.data[layout.platformFee] === 0, "Jupiter platform fee is not zero");
  return {
    dialect: legacy ? legacyCrossProgramForward ? "shared-accounts-route-cross-program"
      : legacyCrossProgramReverse ? "shared-accounts-route-cross-program-reverse"
      : legacySharedToken2022AtTen ? "shared-accounts-route-token-2022" : "shared-accounts-route"
      : v2 ? "shared-accounts-route-v2" : "route",
    accountCount: ix.keys.length,
    indexes: layout,
  } as const;
}

async function resolveOne(connection: Connection, edge: ReturnType<typeof catalogSwapEdges>[number],
  decimals: Map<string, number>, maxAccounts = "64") {
  const apiKey = process.env.JUPITER_API_KEY?.trim();
  const apiBase = apiKey ? "https://api.jup.ag/swap/v1" : "https://lite-api.jup.ag/swap/v1";
  const headers = apiKey ? { "x-api-key": apiKey } : undefined;
  const amountRaw = 10n ** BigInt(decimals.get(edge.source.mint) ?? 0);
  const params = new URLSearchParams({
    inputMint: edge.source.mint, outputMint: edge.destination.mint,
    amount: amountRaw.toString(), slippageBps: String(RWA_MULTIPLY_ROUTE.assets.maxSlippageBps),
    swapMode: "ExactIn", maxAccounts,
  });
  const quoteResponse = await fetch(`${apiBase}/quote?${params}`, {
    ...(headers ? { headers } : {}),
    signal: AbortSignal.timeout(20_000),
  });
  const quoteBody = await quoteResponse.text();
  if (!quoteResponse.ok) {
    throw new JupiterHttpError({ edge: edge.key, stage: "quote", status: quoteResponse.status,
      body: quoteBody, retryAfterMs: retryAfterMs(quoteResponse.headers.get("retry-after")) });
  }
  const quote = record(JSON.parse(quoteBody), `${edge.key} quote`);
  invariant(quote.inputMint === edge.source.mint && quote.outputMint === edge.destination.mint
    && quote.inAmount === amountRaw.toString() && quote.swapMode === "ExactIn"
    && BigInt(string(quote.outAmount, `${edge.key}.outAmount`)) > 0n
    && array(quote.routePlan, `${edge.key}.routePlan`).length > 0,
  `${edge.key} quote identity/economics drifted`);
  const instructionFamily = async (useSharedAccounts: boolean) => {
    const response = await fetch(`${apiBase}/swap-instructions`, {
      method: "POST", headers: { "content-type": "application/json", ...(headers ?? {}) }, signal: AbortSignal.timeout(20_000),
      body: JSON.stringify({ userPublicKey: RWA_MULTIPLY_ROUTE.squads.vault, quoteResponse: quote,
        wrapAndUnwrapSol: false, useSharedAccounts, dynamicComputeUnitLimit: false }),
    });
    const responseBody = await response.text();
    if (!response.ok) {
      throw new JupiterHttpError({ edge: edge.key, stage: "swap-instructions", status: response.status,
        body: responseBody, retryAfterMs: retryAfterMs(response.headers.get("retry-after")) });
    }
    const body = record(JSON.parse(responseBody), `${edge.key} swap instructions`);
    const auxiliaryInstructions = validateAuxiliaryInstructionBoundary(body, edge);
    const instruction = instructionFromJson(body.swapInstruction, `${edge.key}.swapInstruction`);
    const header = validateJupiterHeader({ instruction, sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata, destinationAta: edge.destination.ata,
      sourceTokenProgram: edge.source.tokenProgram, destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw, outAmountRaw: BigInt(string(quote.outAmount, `${edge.key}.outAmount`)) });
    return { body, instruction, header, auxiliaryInstructions };
  };
  let family: Awaited<ReturnType<typeof instructionFamily>>;
  try {
    // USDG is Token-2022 and Jupiter's shared-account cross-program shape has
    // repeatedly failed before CPI on the exact signed Squads wrapper.  Use
    // Jupiter's ordinary Route family for this edge so every token account is
    // explicit and the policy can constrain the same source/destination
    // boundary without adding permissions.
    family = await instructionFamily(edge.key !== "USDC->USDG");
  } catch (error) {
    if (!(edge.source.tokenProgram !== edge.destination.tokenProgram
      && error instanceof Error
      && error.message === "legacy SharedAccountsRoute cannot prove two distinct token-program boundaries")) {
      // Preserve HTTP and auxiliary-instruction errors so the outer bounded
      // scheduler can stop globally on a 429 or semantic boundary failure.
      throw error;
    }
    family = await instructionFamily(false);
    invariant(family.header.dialect === "route", `${edge.key} cross-program fallback is not Jupiter Route`);
  }
  const { body, instruction, header, auxiliaryInstructions } = family;
  const lookupTables = Array.isArray(body.addressLookupTableAddresses)
    ? body.addressLookupTableAddresses.map((value, index) => string(value, `${edge.key}.ALT[${index}]`)) : [];
  const tableRead = await Promise.all(lookupTables.map((value) =>
    connection.getAddressLookupTable(new PublicKey(value), { commitment: "confirmed" })));
  invariant(tableRead.every(({ value }) => value !== null), `${edge.key} has an unresolved lookup table`);
  return {
    key: edge.key, pass: true, source: edge.source, destination: edge.destination,
    quote: { inAmountRaw: quote.inAmount, outAmountRaw: quote.outAmount,
      otherAmountThresholdRaw: quote.otherAmountThreshold, routePlanLength: (quote.routePlan as unknown[]).length },
    header,
    instruction: { programId: instruction.programId.toBase58(), dataBase64: Buffer.from(instruction.data).toString("base64"),
      dataSha256: sha256(instruction.data), accounts: instruction.keys.map(({ pubkey, isSigner, isWritable }) =>
        ({ pubkey: pubkey.toBase58(), isSigner, isWritable })) },
    packet: measureSignedJupiterPacket(edge.key, instruction),
    auxiliaryInstructions,
    lookupTables,
  } as const;
}

/**
 * One-shot, read-only discovery for a previously rejected edge. It captures
 * only the public instruction shapes needed to decide whether a narrower,
 * exact policy family can be defined; it never signs, sends, or accepts one.
 */
export async function diagnoseRejectedJupiterEdge(connection: Connection, edgeKey: string) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const edge = catalogSwapEdges().find(({ key }) => key === edgeKey);
  invariant(edge, `unknown Jupiter edge ${edgeKey}`);
  const apiKey = process.env.JUPITER_API_KEY?.trim();
  const apiBase = apiKey ? "https://api.jup.ag/swap/v1" : "https://lite-api.jup.ag/swap/v1";
  const headers = apiKey ? { "x-api-key": apiKey } : undefined;
  const sourceMint = await getMint(connection, new PublicKey(edge.source.mint), "confirmed",
    new PublicKey(edge.source.tokenProgram));
  const amountRaw = 10n ** BigInt(sourceMint.decimals);
  const params = new URLSearchParams({
    inputMint: edge.source.mint, outputMint: edge.destination.mint,
    amount: amountRaw.toString(), slippageBps: String(RWA_MULTIPLY_ROUTE.assets.maxSlippageBps),
    swapMode: "ExactIn", maxAccounts: "64",
  });
  const quoteResponse = await fetch(`${apiBase}/quote?${params}`, {
    ...(headers ? { headers } : {}), signal: AbortSignal.timeout(20_000),
  });
  const quoteBody = await quoteResponse.text();
  if (!quoteResponse.ok) {
    throw new JupiterHttpError({ edge: edge.key, stage: "quote", status: quoteResponse.status,
      body: quoteBody, retryAfterMs: retryAfterMs(quoteResponse.headers.get("retry-after")) });
  }
  const quote = record(JSON.parse(quoteBody), `${edge.key} diagnostic quote`);
  invariant(quote.inputMint === edge.source.mint && quote.outputMint === edge.destination.mint
    && quote.inAmount === amountRaw.toString() && quote.swapMode === "ExactIn"
    && BigInt(string(quote.outAmount, `${edge.key}.diagnostic.outAmount`)) > 0n
    && array(quote.routePlan, `${edge.key}.diagnostic.routePlan`).length > 0,
  `${edge.key} diagnostic quote identity/economics drifted`);
  // One quote, then one request for each explicit Jupiter account dialect.
  // This is discovery only: it does not turn either layout into an accepted
  // policy family. Keeping the quote shared makes the comparison meaningful.
  const layouts = [];
  for (const useSharedAccounts of [true, false]) {
    const response = await fetch(`${apiBase}/swap-instructions`, {
      method: "POST", headers: { "content-type": "application/json", ...(headers ?? {}) }, signal: AbortSignal.timeout(20_000),
      body: JSON.stringify({ userPublicKey: RWA_MULTIPLY_ROUTE.squads.vault, quoteResponse: quote,
        wrapAndUnwrapSol: false, useSharedAccounts, dynamicComputeUnitLimit: false }),
    });
    const responseBody = await response.text();
    if (!response.ok) {
      throw new JupiterHttpError({ edge: edge.key, stage: "swap-instructions", status: response.status,
        body: responseBody, retryAfterMs: retryAfterMs(response.headers.get("retry-after")) });
    }
    const body = record(JSON.parse(responseBody), `${edge.key} diagnostic swap instructions`);
    const lookupTables = Array.isArray(body.addressLookupTableAddresses)
      ? body.addressLookupTableAddresses.map((value, index) => string(value, `${edge.key}.diagnostic.ALT[${index}]`)) : [];
    layouts.push({ useSharedAccounts, lookupTables, shape: sanitizeRejectedResponseShape(body, edge) });
  }
  return {
    schema: "loyal-backyard-rwa-jupiter-rejected-shape/v1",
    edge: edge.key,
    broadcast: false,
    commitment: "confirmed",
    // Quote amounts are needed to reproduce exact instruction-tail validation;
    // the full Jupiter quote body is intentionally not persisted.
    quote: { inAmountRaw: string(quote.inAmount, `${edge.key}.diagnostic.inAmount`),
      outAmountRaw: string(quote.outAmount, `${edge.key}.diagnostic.outAmount`),
      routePlanLength: array(quote.routePlan, `${edge.key}.diagnostic.routePlan`).length },
    layouts,
  } as const;
}

async function boundedMap<T, R>(values: readonly T[], concurrency: number, task: (value: T) => Promise<R>): Promise<R[]> {
  const result = new Array<R>(values.length);
  let cursor = 0;
  await Promise.all(Array.from({ length: Math.min(concurrency, values.length) }, async () => {
    while (cursor < values.length) {
      const index = cursor++;
      result[index] = await task(values[index]!);
    }
  }));
  return result;
}

export type JupiterHeaderResolveOptions = Readonly<{
  /** A prior artifact may supply only structurally revalidated PASS rows. */
  cachedEvidence?: unknown;
  /** Bounds fresh Jupiter edge retrieval on each resume run. */
  maxNetworkEdges?: number;
  /** Serializes both quote and swap-instructions calls. */
  minRequestIntervalMs?: number;
  /** Revalidate exactly these failed edges while preserving every other prior row. */
  targetEdgeKeys?: readonly string[];
}>;

function boundedOption(value: number | undefined, fallback: number, label: string, maximum: number): number {
  if (value === undefined) return fallback;
  invariant(Number.isSafeInteger(value) && value >= 0 && value <= maximum, `${label} is outside its safe bound`);
  return value;
}

function failureRow(edge: SwapEdge, blocker: string, code: string) {
  return { key: edge.key, pass: false, code, blocker } as const;
}

function priorRowsByEdge(value: unknown, expectedKeys: readonly string[]): ReadonlyMap<string, JupiterRow> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return new Map();
  const rows = (value as Record<string, unknown>).rows;
  if (!Array.isArray(rows)) return new Map();
  const expected = new Set(expectedKeys);
  const result = new Map<string, JupiterRow>();
  for (const candidate of rows) {
    if (candidate === null || typeof candidate !== "object" || Array.isArray(candidate)) continue;
    const row = candidate as JupiterRow;
    if (typeof row.key !== "string" || !expected.has(row.key) || result.has(row.key)) continue;
    result.set(row.key, row);
  }
  return result;
}

function familySummary(rows: readonly JupiterRow[]) {
  const counts = new Map<string, { dialect: string; discriminatorHex: string; dataBytes: number; accountCount: number; edgeCount: number }>();
  for (const row of rows) {
    if (row.pass !== true) continue;
    const header = record(row.header, "Jupiter header");
    const instruction = record(row.instruction, "Jupiter instruction");
    const data = Buffer.from(string(instruction.dataBase64, "Jupiter instruction data"), "base64");
    const dialect = string(header.dialect, "Jupiter header dialect");
    const accountCount = integer(header.accountCount, "Jupiter header account count");
    const key = `${dialect}:${data.subarray(0, 8).toString("hex")}:${data.length}:${accountCount}`;
    const prior = counts.get(key);
    counts.set(key, prior
      ? { ...prior, edgeCount: prior.edgeCount + 1 }
      : { dialect, discriminatorHex: data.subarray(0, 8).toString("hex"), dataBytes: data.length, accountCount, edgeCount: 1 });
  }
  return [...counts.values()].sort((left, right) => left.dialect.localeCompare(right.dialect)
    || left.discriminatorHex.localeCompare(right.discriminatorHex) || left.accountCount - right.accountCount);
}

export async function resolveCurrentJupiterHeaders(connection: Connection, options: JupiterHeaderResolveOptions = {}) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const edges = catalogSwapEdges();
  const expectedKeys = edges.map(({ key }) => key);
  invariant(expectedKeys.length === 52 && new Set(expectedKeys).size === 52,
    "Jupiter resolver input is not an exact 52-edge bijection");
  const custodies = catalogCustodies();
  const decimals = new Map<string, number>();
  await boundedMap(custodies, 4, async ({ mint, tokenProgram }) => {
    const state = await getMint(connection, new PublicKey(mint), "confirmed", new PublicKey(tokenProgram));
    decimals.set(mint, state.decimals);
  });
  const cached = cachedJupiterRowsByEdge(options.cachedEvidence);
  const priorRows = priorRowsByEdge(options.cachedEvidence, expectedKeys);
  const maxNetworkEdges = boundedOption(options.maxNetworkEdges, DEFAULT_MAX_NETWORK_EDGES,
    "maxNetworkEdges", 52);
  const minRequestIntervalMs = boundedOption(options.minRequestIntervalMs, DEFAULT_MIN_REQUEST_INTERVAL_MS,
    "minRequestIntervalMs", 60_000);
  const targetEdgeKeys = options.targetEdgeKeys === undefined ? null : new Set(options.targetEdgeKeys);
  if (targetEdgeKeys) {
    invariant(targetEdgeKeys.size > 0 && [...targetEdgeKeys].every((key) => expectedKeys.includes(key)),
      "targetEdgeKeys must be a non-empty subset of the exact 52-edge catalog");
  }
  let priorRequestAt = 0;
  let attemptedEdges = 0;
  let rateLimited = false;
  let rateLimitCooldownMs: number | null = null;
  let terminalFailure: string | null = null;
  const rows: JupiterRow[] = [];
  for (const edge of edges) {
    if (targetEdgeKeys && !targetEdgeKeys.has(edge.key)) {
      const prior = priorRows.get(edge.key);
      rows.push(prior ?? failureRow(edge, "deferred outside the explicit one-edge revalidation scope", "DEFERRED_TARGET_SCOPE"));
      continue;
    }
    const retained = cached.get(edge.key);
    if (retained) {
      rows.push(retained);
      continue;
    }
    if (terminalFailure) {
      rows.push(failureRow(edge, `deferred after first semantic boundary failure on ${terminalFailure}`,
        "DEFERRED_TERMINAL_FAILURE"));
      continue;
    }
    if (rateLimited) {
      rows.push(failureRow(edge,
        `deferred after Jupiter HTTP 429; wait at least ${rateLimitCooldownMs ?? DEFAULT_RATE_LIMIT_COOLDOWN_MS}ms before retrying`,
        "DEFERRED_RATE_LIMIT"));
      continue;
    }
    if (attemptedEdges >= maxNetworkEdges) {
      rows.push(failureRow(edge, `deferred by bounded resume budget of ${maxNetworkEdges} fresh edges`,
        "DEFERRED_REQUEST_BUDGET"));
      continue;
    }
    const delay = Math.max(0, minRequestIntervalMs - (Date.now() - priorRequestAt));
    if (delay > 0) await new Promise((resume) => setTimeout(resume, delay));
    priorRequestAt = Date.now();
    attemptedEdges += 1;
    try {
      rows.push(await resolveOne(connection, edge, decimals));
    } catch (error) {
      if (error instanceof JupiterHttpError && error.status === 429) {
        rateLimited = true;
        rateLimitCooldownMs = Math.max(error.retryAfterMs ?? 0, DEFAULT_RATE_LIMIT_COOLDOWN_MS);
      } else {
        terminalFailure = edge.key;
      }
      rows.push(failureRow(edge, error instanceof Error ? error.message : String(error),
        error instanceof JupiterHttpError && error.status === 429 ? "JUPITER_HTTP_429" : "JUPITER_EDGE_REJECTED"));
    }
  }
  invariant(rows.length === 52 && new Set(rows.map(({ key }) => String(key))).size === 52
    && rows.every(({ key }) => expectedKeys.includes(String(key))),
  "Jupiter resolver output does not preserve the exact 52-edge bijection");
  const passCount = rows.filter(({ pass }) => pass).length;
  const failedEdges = rows.filter(({ pass }) => !pass).map(({ key, code, blocker }) => ({
    key: String(key), code: typeof code === "string" ? code : "UNKNOWN", blocker: String(blocker),
  }));
  return {
    schema: "loyal-backyard-rwa-jupiter-header-evidence/v2",
    generatedAt: new Date().toISOString(),
    verdict: passCount === 52 ? "PASS_HEADERS_RESOLVED" : "BLOCKED_CURRENT_JUPITER_HEADERS",
    broadcast: false,
    commitment: "confirmed",
    requestedEdgeCount: 52,
    passCount,
    edgeBijection: {
      expectedEdgeCount: 52,
      observedEdgeCount: rows.length,
      expectedKeysSha256: edgeKeysSha256(expectedKeys),
      observedKeysSha256: edgeKeysSha256(rows.map(({ key }) => String(key))),
      exact: true,
    },
    actualInstructionFamilies: familySummary(rows),
    auxiliaryInstructionPolicy: "reject unless an explicit policy-family allowlist is added",
    requestBudget: {
      cachedPassCount: cached.size,
      attemptedEdges,
      maxNetworkEdges,
      minRequestIntervalMs,
      rateLimited,
      rateLimitCooldownMs,
    },
    rows,
    failedEdges,
    resumeCondition: passCount === 52 ? null
      : "Reuse the validated PASS cache, wait out any reported Jupiter rate limit, and resume only the listed unresolved edges; do not refetch successful edges.",
  } as const;
}

export async function resolveRepresentativeJupiterFamilies(connection: Connection) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const wanted = ["USDC->ONyc", "ONyc->USDC", "USDC->USDG"] as const;
  const byKey = new Map(catalogSwapEdges().map((edge) => [edge.key, edge]));
  const edges = wanted.map((key) => {
    const edge = byKey.get(key);
    invariant(edge, `representative Jupiter edge ${key} is absent`);
    return edge;
  });
  const custodies = catalogCustodies();
  const decimals = new Map<string, number>();
  await boundedMap(custodies, 4, async ({ mint, tokenProgram }) => {
    const state = await getMint(connection, new PublicKey(mint), "confirmed", new PublicKey(tokenProgram));
    decimals.set(mint, state.decimals);
  });
  const rows = await boundedMap(edges, 3, async (edge) => {
    try { return await resolveOne(connection, edge, decimals); }
    catch (error) { return { key: edge.key, pass: false,
      blocker: error instanceof Error ? error.message : String(error) } as const; }
  });
  const families = ["stable-to-rwa", "rwa-to-stable", "stable-to-stable"] as const;
  return {
    schema: "loyal-backyard-rwa-jupiter-family-probe/v1",
    verdict: rows.every(({ pass }) => pass) ? "PASS_FAMILIES_PROBED" : "FAIL_FAMILY_PROBE",
    broadcast: false,
    commitment: "confirmed",
    families: families.map((family, index) => ({ family, edge: wanted[index], result: rows[index] })),
  } as const;
}

/**
 * Resolve one fresh current instruction for a bounded signed-unsent execution.
 * The caller must still bind it to the compiler's exact policy constraint;
 * this helper intentionally bypasses the cached 52-edge ledger.
 */
export async function resolveFreshJupiterEdge(connection: Connection, edgeKey: string) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const edge = catalogSwapEdges().find((candidate) => candidate.key === edgeKey);
  invariant(edge, `fresh Jupiter edge ${edgeKey} is not in the exact catalog`);
  const decimals = new Map<string, number>();
  for (const asset of [edge.source, edge.destination]) {
    if (decimals.has(asset.mint)) continue;
    const mint = await getMint(connection, new PublicKey(asset.mint), "confirmed", new PublicKey(asset.tokenProgram));
    decimals.set(asset.mint, mint.decimals);
  }
  return resolveOne(connection, edge, decimals);
}


export async function resolveCurrentPhaseOnePrimeUsdcJupiterHeaders(connection: Connection) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const edges = phaseOnePrimeUsdcSwapEdges();
  const custodies = catalogCustodies().filter(({ symbol }) => symbol === "USDC" || symbol === "PRIME");
  invariant(custodies.length === 2, "Phase 1 custody set is not exactly USDC and PRIME");
  const decimals = new Map<string, number>();
  await boundedMap(custodies, 2, async ({ mint, tokenProgram }) => {
    const state = await getMint(connection, new PublicKey(mint), "confirmed", new PublicKey(tokenProgram));
    decimals.set(mint, state.decimals);
  });
  const rows = await boundedMap(edges, 2, async (edge) => {
    try { return await resolveOne(connection, edge, decimals); }
    catch (error) { return { key: edge.key, pass: false,
      blocker: error instanceof Error ? error.message : String(error) } as const; }
  });
  const passCount = rows.filter(({ pass }) => pass).length;
  return {
    schema: "loyal-backyard-rwa-phase1-jupiter-header-evidence/v1",
    verdict: passCount === 2 ? "PASS_HEADERS_RESOLVED" : "BLOCKED_CURRENT_JUPITER_HEADERS",
    broadcast: false,
    requestedEdgeCount: 2,
    passCount,
    rows,
    resumeCondition: passCount === 2 ? null
      : "Resolve both current PRIME/USDC Jupiter route/header blockers, then recompile Phase 1 from fresh quotes.",
  } as const;
}

export async function resolveCurrentPhaseOneForwardJupiterHeader(connection: Connection) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const edge = phaseOnePrimeUsdcSwapEdges().find(({ key }) => key === "USDC->PRIME");
  invariant(edge !== undefined, "Phase 1 forward Jupiter edge is absent");
  const decimals = new Map<string, number>();
  await boundedMap([edge.source, edge.destination], 2, async ({ mint, tokenProgram }) => {
    const state = await getMint(connection, new PublicKey(mint), "confirmed", new PublicKey(tokenProgram));
    decimals.set(mint, state.decimals);
  });
  const observations = [];
  for (let attempt = 0; attempt < 5; attempt += 1) {
    const row = await resolveOne(connection, edge, decimals, "32");
    const data = Buffer.from(row.instruction.dataBase64, "base64");
    const uniqueAccounts = new Set(row.instruction.accounts.map(({ pubkey }) => pubkey));
    const observed = {
      dataLength: data.length,
      discriminatorHex: data.subarray(0, 8).toString("hex"),
      routePlanPrefixHex: data.subarray(8, Math.min(18, data.length)).toString("hex"),
      amountOffset: data.length - 19,
      slippageOffset: data.length - 3,
      platformFeeOffset: data.length - 1,
      accountCount: row.instruction.accounts.length,
      uniqueAccountCount: uniqueAccounts.size,
    };
    observations.push(observed);
    const approved = row.header.dialect === "shared-accounts-route"
      && data.length === 37
      && data.subarray(0, 8).equals(LEGACY_SHARED)
      && PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX.includes(
        data.subarray(8, 18).toString("hex") as typeof PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX[number])
      && data.readBigUInt64LE(18) === BigInt(row.quote.inAmountRaw)
      && data.readBigUInt64LE(26) === BigInt(String(row.quote.outAmountRaw))
      && data.readUInt16LE(34) <= RWA_MULTIPLY_ROUTE.assets.maxSlippageBps
      && data[36] === 0
      && row.instruction.accounts.length === 28
      && uniqueAccounts.size === 18;
    if (approved) return {
      schema: "loyal-backyard-rwa-phase1-forward-jupiter-header-evidence/v1",
      verdict: "PASS_FORWARD_HEADER_RESOLVED",
      broadcast: false,
      observationAttempt: attempt + 1,
      amountOffset: 18,
      outAmountOffset: 26,
      slippageOffset: 34,
      platformFeeOffset: 36,
      instructionDataLength: 37,
      accountCount: 28,
      uniqueAccountCount: 18,
      row,
    } as const;
  }
  throw new Error(`current USDC->PRIME Jupiter header did not enter the approved legacy len37 allowlist in five bounded observations: ${JSON.stringify(observations)}`);
}
