import { createHash } from "node:crypto";

import { getRequestWithdrawVaultReceiptDiscriminatorBytes, findRequestWithdrawVaultReceiptPda } from "@voltr/vault-sdk";
import { getTokenDecoder } from "@solana-program/token";
import { getStrategyInitReceiptDecoder } from "@voltr/vault-sdk";
import { address, type Address } from "@solana/kit";
import bs58 from "bs58";

import {
  fourMarketRouteSpecSha256,
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_FOUR_MARKET_STRATEGIES,
  PARTNER_ROUTE,
} from "../domain/route-spec.js";
import type { AccountSnapshot } from "../integrations/solana-compat.js";
import { decodeReceipt, type DecodedWithdrawalReceipt } from "./receipt.js";

const COMMITMENT = "confirmed" as const;
const MAX_CONFIRMED_READ_ATTEMPTS = 3;
const FRACTION_BITS = 48n;
const FRACTION_SCALE = 1n << FRACTION_BITS;

type RpcResponse<T> = Readonly<{
  result?: T;
  error?: Readonly<{ code: number; message: string; data?: unknown }>;
}>;

type ContextValue<T> = Readonly<{
  context: Readonly<{ slot: number }>;
  value: T;
}>;

type ProgramAccount = Readonly<{
  pubkey: string;
  account: Readonly<{
    owner: string;
    lamports: number;
    executable: boolean;
    data: readonly [string, "base64"];
  }>;
}>;

type RawAccount = Readonly<{
  owner: string;
  lamports: number;
  executable: boolean;
  data: readonly [string, "base64"];
}> | null;

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

async function rpc<T>(method: string, params: readonly unknown[]): Promise<T> {
  const response = await fetch(rpcUrl(), {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
  });
  if (!response.ok) throw new Error(`Solana RPC ${method} returned HTTP ${response.status}`);
  const payload = await response.json() as RpcResponse<T>;
  if (payload.error) throw new Error(`Solana RPC ${method} failed: ${payload.error.message}`);
  if (payload.result === undefined) throw new Error(`Solana RPC ${method} returned no result`);
  return payload.result;
}

function contextSlot(value: ContextValue<unknown>, label: string): number {
  const slot = value.context?.slot;
  if (!Number.isSafeInteger(slot) || slot <= 0) throw new Error(`${label} returned an invalid confirmed context slot`);
  return slot;
}

function accountSnapshot(addressValue: string, value: RawAccount): AccountSnapshot | null {
  if (!value) return null;
  if (value.data[1] !== "base64") throw new Error(`unsupported RPC account encoding for ${addressValue}`);
  if (!Number.isSafeInteger(value.lamports) || value.lamports < 0) throw new Error(`invalid lamports for ${addressValue}`);
  return {
    address: addressValue,
    owner: value.owner,
    lamports: value.lamports,
    executable: value.executable,
    data: Buffer.from(value.data[0], "base64"),
  };
}

function sha256(value: string | ArrayLike<number>): string {
  return createHash("sha256")
    .update(typeof value === "string" ? value : Uint8Array.from(value))
    .digest("hex");
}

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.entries(value as Readonly<Record<string, unknown>>)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

function ceilAssetRaw(decimalBits: bigint): bigint {
  return (decimalBits + FRACTION_SCALE - 1n) >> FRACTION_BITS;
}

function canonicalReceipt(receipt: string, decoded: DecodedWithdrawalReceipt): string {
  return JSON.stringify({
    receipt,
    vault: decoded.vault,
    user: decoded.user,
    amountLpEscrowed: decoded.amountLpEscrowed.toString(),
    amountAssetToWithdrawDecimalBits: decoded.amountAssetToWithdrawDecimalBits.toString(),
    withdrawableFromTs: decoded.withdrawableFromTs.toString(),
    bump: decoded.bump,
    version: decoded.version,
  });
}

function assertExactReceipt(receipt: string, decoded: DecodedWithdrawalReceipt): void {
  if (decoded.vault !== PARTNER_ROUTE.vault) throw new Error(`receipt ${receipt} is for an unexpected vault ${decoded.vault}`);
  if (decoded.version !== 0 || decoded.bump < 0 || decoded.bump > 255) throw new Error(`receipt ${receipt} has an unsupported version or bump`);
  if (decoded.amountLpEscrowed <= 0n || decoded.amountAssetToWithdrawDecimalBits <= 0n) throw new Error(`receipt ${receipt} has a non-positive withdrawal amount`);
}

async function expectedReceiptPda(user: Address): Promise<Address> {
  const [receipt] = await findRequestWithdrawVaultReceiptPda({
    vault: PARTNER_ROUTE.vault,
    userTransferAuthority: user,
  }, { programAddress: PARTNER_ROUTE.programs.voltrVault });
  return receipt;
}

async function fetchReceipts(minContextSlot?: number): Promise<Readonly<{
  contextSlot: number;
  accounts: readonly ProgramAccount[];
  rawQuery: Readonly<{ method: "getProgramAccounts"; params: readonly [string, Readonly<Record<string, unknown>>] }>;
}>> {
  const discriminator = bs58.encode(Buffer.from(getRequestWithdrawVaultReceiptDiscriminatorBytes()));
  const config = {
    commitment: COMMITMENT,
    encoding: "base64",
    withContext: true,
    ...(minContextSlot === undefined ? {} : { minContextSlot }),
    filters: [
      { memcmp: { offset: 0, bytes: discriminator } },
      { memcmp: { offset: 8, bytes: PARTNER_ROUTE.vault } },
    ],
  } as const;
  const rawQuery = {
    method: "getProgramAccounts" as const,
    params: [PARTNER_ROUTE.programs.voltrVault, config] as const,
  };
  const result = await rpc<ContextValue<readonly ProgramAccount[]>>(rawQuery.method, rawQuery.params);
  const accounts = result.value;
  const seen = new Set<string>();
  for (const entry of accounts) {
    if (seen.has(entry.pubkey)) throw new Error(`confirmed receipt scan returned duplicate account ${entry.pubkey}`);
    seen.add(entry.pubkey);
  }
  return { contextSlot: contextSlot(result, "getProgramAccounts"), accounts, rawQuery };
}

async function fetchIdleAta(minContextSlot?: number): Promise<Readonly<{ contextSlot: number; snapshot: AccountSnapshot | null }>> {
  const config = {
    commitment: COMMITMENT,
    encoding: "base64",
    ...(minContextSlot === undefined ? {} : { minContextSlot }),
  } as const;
  const result = await rpc<ContextValue<readonly RawAccount[]>>("getMultipleAccounts", [
    [PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta],
    config,
  ]);
  const slot = contextSlot(result, "getMultipleAccounts");
  return {
    contextSlot: slot,
    snapshot: accountSnapshot(PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta, result.value[0] ?? null),
  };
}

function decodeIdleRaw(snapshot: AccountSnapshot | null): bigint {
  if (!snapshot || snapshot.owner !== PARTNER_ROUTE.asset.tokenProgram || snapshot.data.length !== 165) {
    throw new Error("confirmed idle ATA is absent or is not an SPL Token account");
  }
  const decoded = getTokenDecoder().decode(snapshot.data);
  if (decoded.mint !== PARTNER_ROUTE.asset.mint || decoded.owner !== PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAuth) {
    throw new Error("confirmed idle ATA mint or authority does not match the four-market route");
  }
  return decoded.amount;
}

export async function scanWithdrawalDemand(
  requestSignature?: string,
  requestEventIndex = 0,
  requestReceipt?: string,
  minimumContextSlot?: number,
) {
  if (requestSignature !== undefined && requestSignature.trim() === "") throw new Error("withdrawal scan request signature must be nonempty");
  if (!Number.isSafeInteger(requestEventIndex) || requestEventIndex < 0) throw new Error("withdrawal scan request event index must be a non-negative safe integer");
  if (minimumContextSlot !== undefined && (!Number.isSafeInteger(minimumContextSlot) || minimumContextSlot <= 0)) {
    throw new Error("withdrawal scan minimum context slot must be a positive safe integer");
  }
  let receipts: Awaited<ReturnType<typeof fetchReceipts>> | null = null;
  let idle: Readonly<{ contextSlot: number; snapshot: AccountSnapshot | null }> | null = null;
  let minContextSlot = minimumContextSlot;
  for (let attempt = 1; attempt <= MAX_CONFIRMED_READ_ATTEMPTS; attempt += 1) {
    const [nextReceipts, nextIdle] = await Promise.all([
      fetchReceipts(minContextSlot),
      fetchIdleAta(minContextSlot),
    ]);
    receipts = nextReceipts;
    idle = nextIdle;
    if (nextReceipts.contextSlot === nextIdle.contextSlot) break;
    if (attempt === MAX_CONFIRMED_READ_ATTEMPTS) {
      throw new Error(`confirmed withdrawal scan could not align receipt slot ${nextReceipts.contextSlot} with idle slot ${nextIdle.contextSlot}`);
    }
    minContextSlot = Math.max(nextReceipts.contextSlot, nextIdle.contextSlot);
  }
  if (!receipts || !idle || receipts.contextSlot !== idle.contextSlot) {
    throw new Error("confirmed withdrawal scan did not produce an aligned account snapshot");
  }
  const receiptRows = await Promise.all(receipts.accounts
    .slice()
    .sort((left, right) => left.pubkey.localeCompare(right.pubkey))
    .map(async (entry) => {
      const snapshot = accountSnapshot(entry.pubkey, entry.account);
      const decoded = decodeReceipt(snapshot);
      if (!decoded) throw new Error(`confirmed account ${entry.pubkey} failed strict withdrawal-receipt decoding`);
      assertExactReceipt(entry.pubkey, decoded);
      const expectedPda = await expectedReceiptPda(address(decoded.user));
      if (expectedPda !== entry.pubkey) throw new Error(`receipt ${entry.pubkey} is not the canonical PDA for ${decoded.user}`);
      const upperBoundAssetRaw = ceilAssetRaw(decoded.amountAssetToWithdrawDecimalBits);
      if (upperBoundAssetRaw > PARTNER_ROUTE.asset.vaultCapRaw) throw new Error(`receipt ${entry.pubkey} exceeds the exact vault cap`);
      const canonical = canonicalReceipt(entry.pubkey, decoded);
      const data = snapshot!.data;
      return {
        receipt: entry.pubkey,
        owner: snapshot!.owner,
        lamports: snapshot!.lamports,
        dataBase64: Buffer.from(data).toString("base64"),
        dataSha256: sha256(data),
        vault: decoded.vault,
        user: decoded.user,
        amountLpEscrowed: decoded.amountLpEscrowed,
        amountAssetToWithdrawDecimalBits: decoded.amountAssetToWithdrawDecimalBits,
        upperBoundAssetRaw,
        withdrawableFromTs: decoded.withdrawableFromTs,
        bump: decoded.bump,
        version: decoded.version,
        observedContextSlot: receipts.contextSlot,
        generationFingerprint: sha256(canonical),
      };
    }));
  const confirmedIdleRaw = decodeIdleRaw(idle.snapshot);
  const configuredIdleFloorRaw = PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw;
  const pendingWithdrawalUpperBoundRaw = receiptRows.reduce((sum, row) => sum + row.upperBoundAssetRaw, 0n);
  if (configuredIdleFloorRaw + pendingWithdrawalUpperBoundRaw > PARTNER_ROUTE.asset.vaultCapRaw) {
    throw new Error("confirmed withdrawal demand exceeds the exact vault cap");
  }
  const requiredIdleRaw = configuredIdleFloorRaw + pendingWithdrawalUpperBoundRaw;
  const idleShortfallRaw = requiredIdleRaw > confirmedIdleRaw ? requiredIdleRaw - confirmedIdleRaw : 0n;
  const observationContextSlot = receipts.contextSlot;
  const generationFingerprint = sha256(JSON.stringify({
    routeSpecSha256: fourMarketRouteSpecSha256(),
    vault: PARTNER_ROUTE.vault,
    receiptContextSlot: receipts.contextSlot,
    idleContextSlot: idle.contextSlot,
    receipts: receiptRows.map(({ receipt, generationFingerprint: fingerprint }) => ({ receipt, fingerprint })),
    confirmedIdleRaw: confirmedIdleRaw.toString(),
  }));
  const rawQuery = receipts.rawQuery;
  const queryConfig = rawQuery.params[1];
  const selectedReceipt = requestReceipt === undefined
    ? receiptRows.length === 1 ? receiptRows[0]! : null
    : receiptRows.find(({ receipt }) => receipt === requestReceipt) ?? null;
  if (requestSignature !== undefined && selectedReceipt === null) {
    throw new Error("withdrawal scan request origin does not identify exactly one active receipt");
  }
  const requestOriginBase = requestSignature === undefined || selectedReceipt === null ? null : {
    signature: requestSignature,
    eventIndex: requestEventIndex,
    receipt: selectedReceipt.receipt,
    rawAccountSha256: selectedReceipt.dataSha256,
  };
  const requestOrigin = requestOriginBase === null ? null : {
    ...requestOriginBase,
    generationFingerprint: sha256(canonicalJson(requestOriginBase)),
  };
  return {
    verdict: "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS",
    broadcast: false,
    signerLoaded: false,
    commitment: COMMITMENT,
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    vault: PARTNER_ROUTE.vault,
    receiptProgram: PARTNER_ROUTE.programs.voltrVault,
    observationContextSlot,
    receiptContextSlot: receipts.contextSlot,
    idleContextSlot: idle.contextSlot,
    contextSlotsAligned: true,
    rawQuery,
    rawQuerySha256: sha256(canonicalJson(rawQuery)),
    queryConfigSha256: sha256(canonicalJson(queryConfig)),
    requestOrigin,
    generationFingerprint,
    receipts: receiptRows,
    demand: {
      configuredIdleFloorRaw,
      confirmedIdleRaw,
      pendingWithdrawalUpperBoundRaw,
      requiredIdleRaw,
      idleShortfallRaw,
      rounding: "each receipt amountAssetToWithdrawDecimalBits is rounded up to raw asset units before summing",
    },
  } as const;
}

/**
 * Read the four strategy receipts and asset ATAs from one confirmed bank
 * context.  The caller supplies only a minimum slot; reserve and account
 * identities always come from the frozen four-market route catalog.
 *
 * This is deliberately a separate read from the withdrawal scanner.  A
 * restoration plan must prove that every source position was observed at or
 * after the demand snapshot, rather than accepting a caller-provided reserve
 * list or mixing snapshots from different banks.
 */
export async function loadFourMarketRestorationSources(minimumContextSlot: number) {
  if (!Number.isSafeInteger(minimumContextSlot) || minimumContextSlot <= 0) {
    throw new Error("four-market position evidence requires a positive minimum context slot");
  }
  const addresses = PARTNER_FOUR_MARKET_STRATEGIES.flatMap((strategy) => [
    strategy.voltr.strategyInitReceipt,
    strategy.voltr.strategyAssetAta,
  ]);
  const result = await rpc<ContextValue<readonly RawAccount[]>>("getMultipleAccounts", [
    addresses,
    { commitment: COMMITMENT, encoding: "base64", minContextSlot: minimumContextSlot },
  ]);
  const slot = contextSlot(result, "four-market strategy position getMultipleAccounts");
  if (slot < minimumContextSlot) throw new Error(`four-market position slot ${slot} predates scan slot ${minimumContextSlot}`);
  const sources = PARTNER_FOUR_MARKET_STRATEGIES.map((strategy, index) => {
    const receipt = accountSnapshot(strategy.voltr.strategyInitReceipt, result.value[index * 2] ?? null);
    const assetAta = accountSnapshot(strategy.voltr.strategyAssetAta, result.value[index * 2 + 1] ?? null);
    if (!receipt || receipt.owner !== PARTNER_ROUTE.programs.voltrVault) throw new Error(`${strategy.id} strategy receipt is absent or has an unexpected owner`);
    if (!assetAta || assetAta.owner !== PARTNER_ROUTE.asset.tokenProgram || assetAta.data.length !== 165) throw new Error(`${strategy.id} strategy asset ATA is absent or has an unexpected owner/layout`);
    const assetToken = getTokenDecoder().decode(assetAta.data);
    if (assetToken.mint !== PARTNER_ROUTE.asset.mint || assetToken.owner !== strategy.voltr.strategyAuth) throw new Error(`${strategy.id} strategy asset ATA mint or authority is not frozen route identity`);
    let positionValue: bigint;
    try {
      positionValue = getStrategyInitReceiptDecoder().decode(receipt.data).positionValue;
    } catch (error) {
      throw new Error(`${strategy.id} strategy receipt failed strict decoding: ${error instanceof Error ? error.message : String(error)}`);
    }
    if (positionValue < 0n) throw new Error(`${strategy.id} strategy position is negative`);
    const positionFingerprint = sha256(JSON.stringify({
      strategyId: strategy.id,
      reserve: strategy.reserve,
      strategyReceipt: strategy.voltr.strategyInitReceipt,
      strategyAssetAta: strategy.voltr.strategyAssetAta,
      receiptDataSha256: sha256(Buffer.from(receipt.data).toString("base64")),
      assetAtaDataSha256: sha256(Buffer.from(assetAta.data).toString("base64")),
      contextSlot: slot,
    }));
    return {
      strategyId: strategy.id,
      reserve: strategy.reserve,
      availableRaw: positionValue,
      netYieldLossBps: 0n,
      unwindCostLamports: 0n,
      observedContextSlot: slot,
      positionFingerprint,
      strategyReceipt: strategy.voltr.strategyInitReceipt,
      strategyAssetAta: strategy.voltr.strategyAssetAta,
    };
  });
  return {
    verdict: "PARTNER_FOUR_MARKET_POSITION_EVIDENCE_PASS" as const,
    broadcast: false,
    signerLoaded: false,
    commitment: COMMITMENT,
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    vault: PARTNER_ROUTE.vault,
    observationContextSlot: slot,
    minimumContextSlot,
    sources,
  };
}
