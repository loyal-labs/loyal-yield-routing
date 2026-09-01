import { createHash } from "node:crypto";

import { getMint } from "@solana/spl-token";
import { Connection, PublicKey, TransactionInstruction } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { readRwaMultiplyCatalog } from "./rwa-multiply-catalog-resolver.js";
import { catalogCustodies } from "./rwa-multiply-custodies.js";

const LEGACY_SHARED = Buffer.from("c1209b3341d69c81", "hex");
const SHARED_V2 = Buffer.from([209, 152, 83, 147, 124, 254, 216, 233]);

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
  return new TransactionInstruction({
    programId: new PublicKey(string(raw.programId, `${label}.programId`)),
    data: Buffer.from(string(raw.data, `${label}.data`), "base64"),
    keys: array(raw.accounts, `${label}.accounts`).map((entry, index) => {
      const account = record(entry, `${label}.accounts[${index}]`);
      invariant(typeof account.isSigner === "boolean" && typeof account.isWritable === "boolean",
        `${label}.accounts[${index}] flags are malformed`);
      return { pubkey: new PublicKey(string(account.pubkey, `${label}.accounts[${index}].pubkey`)),
        isSigner: account.isSigner, isWritable: account.isWritable };
    }),
  });
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
  invariant(legacy || v2, "unsupported Jupiter route discriminator");
  const layout = legacy
    ? { authority: 2, source: 3, destination: 6, sourceMint: 7, destinationMint: 8,
      sourceProgram: 0, destinationProgram: 0, slippage: ix.data.length - 3, platformFee: ix.data.length - 1 }
    : { authority: 1, source: 2, destination: 5, sourceMint: 6, destinationMint: 7,
      sourceProgram: 8, destinationProgram: 9, slippage: 25, platformFee: 27 };
  if (legacy) invariant(input.sourceTokenProgram === input.destinationTokenProgram,
    "legacy SharedAccountsRoute cannot prove two distinct token-program boundaries");
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
  return { dialect: legacy ? "shared-accounts-route" : "shared-accounts-route-v2", accountCount: ix.keys.length } as const;
}

async function resolveOne(connection: Connection, edge: ReturnType<typeof catalogSwapEdges>[number], decimals: Map<string, number>) {
  const amountRaw = 10n ** BigInt(decimals.get(edge.source.mint) ?? 0);
  const params = new URLSearchParams({
    inputMint: edge.source.mint, outputMint: edge.destination.mint,
    amount: amountRaw.toString(), slippageBps: String(RWA_MULTIPLY_ROUTE.assets.maxSlippageBps),
    swapMode: "ExactIn", maxAccounts: "64",
  });
  const quoteResponse = await fetch(`https://lite-api.jup.ag/swap/v1/quote?${params}`, {
    signal: AbortSignal.timeout(20_000),
  });
  const quote = record(await quoteResponse.json(), `${edge.key} quote`);
  invariant(quoteResponse.ok, `${edge.key} quote request failed`);
  invariant(quote.inputMint === edge.source.mint && quote.outputMint === edge.destination.mint
    && quote.inAmount === amountRaw.toString() && quote.swapMode === "ExactIn"
    && BigInt(string(quote.outAmount, `${edge.key}.outAmount`)) > 0n
    && array(quote.routePlan, `${edge.key}.routePlan`).length > 0,
  `${edge.key} quote identity/economics drifted`);
  const response = await fetch("https://lite-api.jup.ag/swap/v1/swap-instructions", {
    method: "POST", headers: { "content-type": "application/json" }, signal: AbortSignal.timeout(20_000),
    body: JSON.stringify({ userPublicKey: RWA_MULTIPLY_ROUTE.squads.vault, quoteResponse: quote,
      wrapAndUnwrapSol: false, useSharedAccounts: true, dynamicComputeUnitLimit: false }),
  });
  const body = record(await response.json(), `${edge.key} swap instructions`);
  invariant(response.ok, `${edge.key} swap-instructions request failed`);
  invariant(array(body.setupInstructions ?? [], `${edge.key}.setupInstructions`).length === 0
    && array(body.otherInstructions ?? [], `${edge.key}.otherInstructions`).length === 0
    && (body.cleanupInstruction === null || body.cleanupInstruction === undefined),
  `${edge.key} requires extra instructions outside the policy contract`);
  const instruction = instructionFromJson(body.swapInstruction, `${edge.key}.swapInstruction`);
  const header = validateJupiterHeader({ instruction, sourceMint: edge.source.mint,
    destinationMint: edge.destination.mint, sourceAta: edge.source.ata, destinationAta: edge.destination.ata,
    sourceTokenProgram: edge.source.tokenProgram, destinationTokenProgram: edge.destination.tokenProgram,
    amountRaw, outAmountRaw: BigInt(string(quote.outAmount, `${edge.key}.outAmount`)) });
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
    lookupTables,
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

export async function resolveCurrentJupiterHeaders(connection: Connection) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const edges = catalogSwapEdges();
  const custodies = catalogCustodies();
  const decimals = new Map<string, number>();
  await boundedMap(custodies, 4, async ({ mint, tokenProgram }) => {
    const state = await getMint(connection, new PublicKey(mint), "confirmed", new PublicKey(tokenProgram));
    decimals.set(mint, state.decimals);
  });
  const rows = await boundedMap(edges, 4, async (edge) => {
    try { return await resolveOne(connection, edge, decimals); }
    catch (error) { return { key: edge.key, pass: false, blocker: error instanceof Error ? error.message : String(error) } as const; }
  });
  const passCount = rows.filter(({ pass }) => pass).length;
  return {
    schema: "loyal-backyard-rwa-jupiter-header-evidence/v1",
    verdict: passCount === 52 ? "PASS_HEADERS_RESOLVED" : "BLOCKED_CURRENT_JUPITER_HEADERS",
    broadcast: false,
    requestedEdgeCount: 52,
    passCount,
    rows,
    resumeCondition: passCount === 52 ? null : "Resolve every reported Jupiter route/header blocker, then rerun all 52 edges at one fresh observation time.",
  } as const;
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
