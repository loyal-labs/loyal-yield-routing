import "server-only";

import { neon } from "@neondatabase/serverless";
import {
  Connection,
  PublicKey,
  type ConfirmedSignatureInfo,
  type VersionedTransactionResponse,
} from "@solana/web3.js";
import { unstable_cache } from "next/cache";

import { BACKYARD_VAULT } from "../config";
import type {
  ReserveRate,
  VaultApy,
  VaultBalancePoint,
  VaultFlow,
  VaultHistory,
  VaultSnapshot,
} from "../types";

const COMMITMENT = "confirmed" as const;
const HISTORY_DAYS = 30;
const SECONDS_PER_DAY = 86_400;
const SIGNATURE_PAGE_SIZE = 1_000;
const MAX_SIGNATURES = 5_000;
const TRANSACTION_BATCH_SIZE = 50;
const TRANSACTION_BATCH_CONCURRENCY = 4;
const MAX_RATE_AGE_MILLIS = 15 * 60 * 1_000;

const VAULT_DATA_LENGTH = 928;
const VAULT_DISCRIMINATOR = Buffer.from([211, 8, 232, 43, 2, 152, 117, 119]);
const VAULT_ASSET_MINT_OFFSET = 104;
const VAULT_IDLE_ATA_OFFSET = 136;
const VAULT_TOTAL_VALUE_OFFSET = 168;
const VAULT_LP_MINT_OFFSET = 272;
const VAULT_MANAGER_OFFSET = 368;
const VAULT_WITHDRAWAL_WAIT_OFFSET = 456;

const TOKEN_ACCOUNT_DATA_LENGTH = 165;
const TOKEN_MINT_OFFSET = 0;
const TOKEN_OWNER_OFFSET = 32;
const TOKEN_AMOUNT_OFFSET = 64;

const STRATEGY_RECEIPT_DATA_LENGTH = 192;
const STRATEGY_RECEIPT_DISCRIMINATOR = Buffer.from([
  51, 8, 192, 253, 115, 78, 112, 214,
]);
const STRATEGY_RECEIPT_VAULT_OFFSET = 8;
const STRATEGY_RECEIPT_RESERVE_OFFSET = 40;
const STRATEGY_RECEIPT_ADAPTOR_OFFSET = 72;
const STRATEGY_RECEIPT_VALUE_OFFSET = 104;
const STRATEGY_RECEIPT_VERSION_OFFSET = 120;
const STRATEGY_RECEIPT_PADDING_OFFSET = 123;

const DEPOSIT_EVENT_DISCRIMINATOR = Buffer.from([
  0x0b, 0x0f, 0x07, 0x5c, 0x96, 0x64, 0xa5, 0xe8,
]);
const WITHDRAW_EVENT_DISCRIMINATOR = Buffer.from([
  0xc4, 0x7b, 0x4f, 0xd7, 0x04, 0xd6, 0x14, 0xc5,
]);
const INSTANT_WITHDRAW_EVENT_DISCRIMINATOR = Buffer.from([
  0x2e, 0x39, 0x3c, 0x14, 0x06, 0xa0, 0xa4, 0xf7,
]);
const PROGRAM_DATA_PREFIX = "Program data: ";

type HistoryEventLayout = Readonly<{
  kind: VaultFlow["kind"];
  discriminator: Buffer;
  length: number;
  amountOffset: number;
  vaultOffset: number;
  totalBeforeOffset: number;
  totalAfterOffset: number;
  timestampOffset: number;
}>;

const HISTORY_EVENT_LAYOUTS: readonly HistoryEventLayout[] = [
  {
    kind: "deposit",
    discriminator: DEPOSIT_EVENT_DISCRIMINATOR,
    length: 224,
    amountOffset: 40,
    vaultOffset: 56,
    totalBeforeOffset: 120,
    totalAfterOffset: 128,
    timestampOffset: 216,
  },
  {
    kind: "withdrawal",
    discriminator: WITHDRAW_EVENT_DISCRIMINATOR,
    length: 232,
    amountOffset: 40,
    vaultOffset: 56,
    totalBeforeOffset: 128,
    totalAfterOffset: 136,
    timestampOffset: 224,
  },
  {
    kind: "withdrawal",
    discriminator: INSTANT_WITHDRAW_EVENT_DISCRIMINATOR,
    length: 242,
    amountOffset: 50,
    vaultOffset: 66,
    totalBeforeOffset: 138,
    totalAfterOffset: 146,
    timestampOffset: 234,
  },
];

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL?.trim();
  if (!value) throw new Error("SOLANA_RPC_URL is not configured");
  return value;
}

function connection(): Connection {
  return new Connection(rpcUrl(), {
    commitment: COMMITMENT,
    disableRetryOnRateLimit: true,
  });
}

function publicKeyAt(data: Buffer, offset: number): PublicKey {
  if (offset < 0 || offset + 32 > data.length) {
    throw new Error(`public key offset ${offset} is outside ${data.length} bytes`);
  }
  return new PublicKey(data.subarray(offset, offset + 32));
}

function u64At(data: Buffer, offset: number): bigint {
  if (offset < 0 || offset + 8 > data.length) {
    throw new Error(`u64 offset ${offset} is outside ${data.length} bytes`);
  }
  return data.readBigUInt64LE(offset);
}

function assertPublicKey(actual: PublicKey, expected: string, field: string): void {
  if (actual.toBase58() !== expected) {
    throw new Error(`${field} identity drifted`);
  }
}

export async function loadVaultSnapshot(): Promise<VaultSnapshot> {
  const rpc = connection();
  const addresses = [
    BACKYARD_VAULT.address,
    BACKYARD_VAULT.idleAta,
    BACKYARD_VAULT.lpMint,
    ...BACKYARD_VAULT.strategies.map((strategy) => strategy.receipt),
  ].map((value) => new PublicKey(value));
  const response = await rpc.getMultipleAccountsInfoAndContext(addresses, {
    commitment: COMMITMENT,
  });
  const accounts = response.value;
  if (accounts.some((account) => account === null)) {
    throw new Error("one or more required vault accounts are absent");
  }

  const vaultAccount = accounts[0]!;
  const vaultData = Buffer.from(vaultAccount.data);
  if (
    vaultAccount.owner.toBase58() !== BACKYARD_VAULT.programs.voltr ||
    vaultData.length !== VAULT_DATA_LENGTH ||
    !vaultData.subarray(0, 8).equals(VAULT_DISCRIMINATOR)
  ) {
    throw new Error("vault owner or account layout drifted");
  }
  assertPublicKey(
    publicKeyAt(vaultData, VAULT_ASSET_MINT_OFFSET),
    BACKYARD_VAULT.assetMint,
    "vault asset mint",
  );
  assertPublicKey(
    publicKeyAt(vaultData, VAULT_IDLE_ATA_OFFSET),
    BACKYARD_VAULT.idleAta,
    "vault idle ATA",
  );
  assertPublicKey(
    publicKeyAt(vaultData, VAULT_LP_MINT_OFFSET),
    BACKYARD_VAULT.lpMint,
    "vault LP mint",
  );
  assertPublicKey(
    publicKeyAt(vaultData, VAULT_MANAGER_OFFSET),
    BACKYARD_VAULT.manager,
    "vault manager",
  );
  if (u64At(vaultData, VAULT_WITHDRAWAL_WAIT_OFFSET) !== 600n) {
    throw new Error("vault withdrawal wait is not the approved ten minutes");
  }
  const totalValueRaw = u64At(vaultData, VAULT_TOTAL_VALUE_OFFSET);

  const idleAccount = accounts[1]!;
  const idleData = Buffer.from(idleAccount.data);
  if (
    idleAccount.owner.toBase58() !== BACKYARD_VAULT.programs.token ||
    idleData.length !== TOKEN_ACCOUNT_DATA_LENGTH
  ) {
    throw new Error("vault idle token account layout drifted");
  }
  assertPublicKey(
    publicKeyAt(idleData, TOKEN_MINT_OFFSET),
    BACKYARD_VAULT.assetMint,
    "idle token mint",
  );
  assertPublicKey(
    publicKeyAt(idleData, TOKEN_OWNER_OFFSET),
    BACKYARD_VAULT.idleAuthority,
    "idle token authority",
  );
  const idleRaw = u64At(idleData, TOKEN_AMOUNT_OFFSET);

  const lpMintAccount = accounts[2]!;
  const lpMintData = Buffer.from(lpMintAccount.data);
  if (
    lpMintAccount.owner.toBase58() !== BACKYARD_VAULT.programs.token ||
    lpMintData.length !== 82
  ) {
    throw new Error("vault LP mint layout drifted");
  }
  const lpSupplyRaw = u64At(lpMintData, 36);

  const positions = BACKYARD_VAULT.strategies.map((strategy, index) => {
    const account = accounts[index + 3]!;
    const data = Buffer.from(account.data);
    if (
      account.owner.toBase58() !== BACKYARD_VAULT.programs.voltr ||
      data.length !== STRATEGY_RECEIPT_DATA_LENGTH ||
      !data.subarray(0, 8).equals(STRATEGY_RECEIPT_DISCRIMINATOR) ||
      data[STRATEGY_RECEIPT_VERSION_OFFSET] !== 1 ||
      data.subarray(STRATEGY_RECEIPT_PADDING_OFFSET).some((byte) => byte !== 0)
    ) {
      throw new Error(`${strategy.label} strategy receipt layout drifted`);
    }
    assertPublicKey(
      publicKeyAt(data, STRATEGY_RECEIPT_VAULT_OFFSET),
      BACKYARD_VAULT.address,
      `${strategy.label} strategy vault`,
    );
    assertPublicKey(
      publicKeyAt(data, STRATEGY_RECEIPT_RESERVE_OFFSET),
      strategy.reserve,
      `${strategy.label} reserve`,
    );
    assertPublicKey(
      publicKeyAt(data, STRATEGY_RECEIPT_ADAPTOR_OFFSET),
      BACKYARD_VAULT.programs.kaminoAdaptor,
      `${strategy.label} adaptor`,
    );
    return {
      id: strategy.id,
      label: strategy.label,
      reserve: strategy.reserve,
      valueRaw: u64At(data, STRATEGY_RECEIPT_VALUE_OFFSET),
    };
  });
  const accountedRaw = positions.reduce(
    (total, position) => total + position.valueRaw,
    idleRaw,
  );
  if (accountedRaw !== totalValueRaw) {
    throw new Error("vault total value does not equal idle plus strategy positions");
  }

  return {
    contextSlot: response.context.slot,
    observedAt: new Date().toISOString(),
    totalValueRaw,
    idleRaw,
    lpSupplyRaw,
    positions,
  };
}

function decodeFlowLine(line: string, signature: string): VaultFlow | null {
  if (!line.startsWith(PROGRAM_DATA_PREFIX)) return null;
  let data: Buffer;
  try {
    data = Buffer.from(line.slice(PROGRAM_DATA_PREFIX.length), "base64");
  } catch {
    return null;
  }
  const layout = HISTORY_EVENT_LAYOUTS.find(
    (candidate) =>
      data.length === candidate.length &&
      data.subarray(0, 8).equals(candidate.discriminator),
  );
  if (!layout) return null;
  if (publicKeyAt(data, layout.vaultOffset).toBase58() !== BACKYARD_VAULT.address) {
    return null;
  }
  const timestamp = Number(u64At(data, layout.timestampOffset));
  if (!Number.isSafeInteger(timestamp) || timestamp <= 0) return null;
  return {
    kind: layout.kind,
    signature,
    timestamp,
    amountRaw: u64At(data, layout.amountOffset),
    totalValueBeforeRaw: u64At(data, layout.totalBeforeOffset),
    totalValueAfterRaw: u64At(data, layout.totalAfterOffset),
  };
}

function decodeTransactionFlows(
  transaction: VersionedTransactionResponse,
  signature: string,
): VaultFlow[] {
  if (transaction.meta?.err || !transaction.meta?.logMessages) return [];
  return transaction.meta.logMessages.flatMap((line) => {
    const flow = decodeFlowLine(line, signature);
    return flow ? [flow] : [];
  });
}

async function signaturesWithinWindow(
  rpc: Connection,
  cutoffTimestamp: number,
): Promise<ConfirmedSignatureInfo[]> {
  const address = new PublicKey(BACKYARD_VAULT.address);
  const signatures: ConfirmedSignatureInfo[] = [];
  let before: string | undefined;
  while (signatures.length < MAX_SIGNATURES) {
    const page = await rpc.getSignaturesForAddress(
      address,
      { before, limit: SIGNATURE_PAGE_SIZE },
      COMMITMENT,
    );
    if (page.length === 0) break;
    for (const signature of page) {
      if (
        typeof signature.blockTime === "number" &&
        signature.blockTime < cutoffTimestamp
      ) {
        return signatures;
      }
      if (signature.err === null) signatures.push(signature);
    }
    if (page.length < SIGNATURE_PAGE_SIZE) break;
    before = page.at(-1)?.signature;
  }
  if (signatures.length >= MAX_SIGNATURES) {
    throw new Error(`vault history exceeded the ${MAX_SIGNATURES}-signature safety bound`);
  }
  return signatures;
}

async function loadVaultHistoryUncached(): Promise<VaultHistory> {
  const rpc = connection();
  const cutoffTimestamp = Math.floor(Date.now() / 1_000) - HISTORY_DAYS * SECONDS_PER_DAY;
  const signatures = await signaturesWithinWindow(rpc, cutoffTimestamp);
  const flows: VaultFlow[] = [];
  const batches = Array.from(
    { length: Math.ceil(signatures.length / TRANSACTION_BATCH_SIZE) },
    (_, index) =>
      signatures.slice(
        index * TRANSACTION_BATCH_SIZE,
        (index + 1) * TRANSACTION_BATCH_SIZE,
      ),
  );
  for (
    let groupIndex = 0;
    groupIndex < batches.length;
    groupIndex += TRANSACTION_BATCH_CONCURRENCY
  ) {
    const group = batches.slice(groupIndex, groupIndex + TRANSACTION_BATCH_CONCURRENCY);
    const results = await Promise.all(
      group.map(async (batch) => ({
        batch,
        transactions: await rpc.getTransactions(
          batch.map((entry) => entry.signature),
          { commitment: COMMITMENT, maxSupportedTransactionVersion: 0 },
        ),
      })),
    );
    for (const { batch, transactions } of results) {
      for (let index = 0; index < transactions.length; index++) {
        const transaction = transactions[index];
        if (!transaction) {
          throw new Error(`confirmed transaction ${batch[index].signature} is unavailable`);
        }
        flows.push(...decodeTransactionFlows(transaction, batch[index].signature));
      }
    }
  }
  const withinWindow = flows
    .filter((flow) => flow.timestamp >= cutoffTimestamp)
    .sort((left, right) => left.timestamp - right.timestamp);
  return {
    cutoffTimestamp,
    depositsRaw: withinWindow.reduce(
      (total, flow) => total + (flow.kind === "deposit" ? flow.amountRaw : 0n),
      0n,
    ),
    withdrawalsRaw: withinWindow.reduce(
      (total, flow) => total + (flow.kind === "withdrawal" ? flow.amountRaw : 0n),
      0n,
    ),
    flows: withinWindow,
    scannedSignatureCount: signatures.length,
  };
}

type CachedVaultHistory = Readonly<{
  cutoffTimestamp: number;
  depositsRaw: string;
  withdrawalsRaw: string;
  flows: readonly Readonly<{
    kind: VaultFlow["kind"];
    signature: string;
    timestamp: number;
    amountRaw: string;
    totalValueBeforeRaw: string;
    totalValueAfterRaw: string;
  }>[];
  scannedSignatureCount: number;
}>;

const loadCachedVaultHistory = unstable_cache(
  async (): Promise<CachedVaultHistory> => {
    const history = await loadVaultHistoryUncached();
    return {
      ...history,
      depositsRaw: history.depositsRaw.toString(),
      withdrawalsRaw: history.withdrawalsRaw.toString(),
      flows: history.flows.map((flow) => ({
        ...flow,
        amountRaw: flow.amountRaw.toString(),
        totalValueBeforeRaw: flow.totalValueBeforeRaw.toString(),
        totalValueAfterRaw: flow.totalValueAfterRaw.toString(),
      })),
    };
  },
  ["backyard-vault-history-v2"],
  { revalidate: 300, tags: ["backyard-vault-history"] },
);

export async function loadVaultHistory(): Promise<VaultHistory> {
  const history = await loadCachedVaultHistory();
  return {
    ...history,
    depositsRaw: BigInt(history.depositsRaw),
    withdrawalsRaw: BigInt(history.withdrawalsRaw),
    flows: history.flows.map((flow) => ({
      ...flow,
      amountRaw: BigInt(flow.amountRaw),
      totalValueBeforeRaw: BigInt(flow.totalValueBeforeRaw),
      totalValueAfterRaw: BigInt(flow.totalValueAfterRaw),
    })),
  };
}

async function loadReserveRatesUncached(): Promise<readonly ReserveRate[]> {
  const databaseUrl = process.env.TIMESCALEDB_URL?.trim();
  if (!databaseUrl) throw new Error("TIMESCALEDB_URL is not configured");
  const sql = neon(databaseUrl);
  const reserves = BACKYARD_VAULT.strategies.map((strategy) => strategy.reserve);
  const rows = await sql.query(
    `SELECT reserve, supply_apy, observed_at::text, slot::text
       FROM kamino.latest_reserve_updates
      WHERE reserve = ANY($1::text[])
        AND source_commitment IN ('confirmed', 'finalized')`,
    [reserves],
  );
  const rates = rows.map((row) => {
    const record = row as Record<string, unknown>;
    const reserve = String(record.reserve);
    const supplyApy = Number(record.supply_apy);
    const observedAt = String(record.observed_at);
    const slot = Number(record.slot);
    if (
      !reserves.includes(reserve as (typeof reserves)[number]) ||
      !Number.isFinite(supplyApy) ||
      supplyApy < 0 ||
      supplyApy >= 0.5 ||
      !Number.isSafeInteger(slot) ||
      !Number.isFinite(Date.parse(observedAt))
    ) {
      throw new Error(`invalid Kamino rate row for ${reserve}`);
    }
    return { reserve, supplyApy, observedAt, slot };
  });
  if (rates.length !== reserves.length || new Set(rates.map((rate) => rate.reserve)).size !== reserves.length) {
    throw new Error("latest Kamino rate coverage is incomplete");
  }
  const oldestRate = Math.min(...rates.map((rate) => Date.parse(rate.observedAt)));
  if (Date.now() - oldestRate > MAX_RATE_AGE_MILLIS) {
    throw new Error("latest Kamino rates are stale");
  }
  return rates;
}

export const loadReserveRates = unstable_cache(
  loadReserveRatesUncached,
  ["backyard-vault-reserve-rates-v1"],
  { revalidate: 300, tags: ["backyard-vault-reserve-rates"] },
);

export function calculateVaultApy(
  snapshot: VaultSnapshot,
  rates: readonly ReserveRate[],
): VaultApy {
  const rateByReserve = new Map(rates.map((rate) => [rate.reserve, rate]));
  let weightedApy = 0;
  for (const position of snapshot.positions) {
    const rate = rateByReserve.get(position.reserve);
    if (!rate) throw new Error(`missing APY for ${position.label}`);
    weightedApy +=
      (Number(position.valueRaw) / Number(snapshot.totalValueRaw || 1n)) * rate.supplyApy;
  }
  const feeFraction = BACKYARD_VAULT.performanceFeeBps / 10_000;
  return {
    grossSupplyApy: weightedApy,
    netSupplyApy: weightedApy * (1 - feeFraction),
    performanceFeeBps: BACKYARD_VAULT.performanceFeeBps,
    observedAt: rates
      .map((rate) => rate.observedAt)
      .sort()
      .at(0)!,
  };
}

function utcDay(timestampSeconds: number): string {
  return new Date(timestampSeconds * 1_000).toISOString().slice(0, 10);
}

export function buildBalanceSeries(
  history: VaultHistory,
  currentBalanceRaw: bigint,
  nowTimestamp = Math.floor(Date.now() / 1_000),
): readonly VaultBalancePoint[] {
  const points: VaultBalancePoint[] = [];
  const firstDayStart =
    Math.floor((nowTimestamp - (HISTORY_DAYS - 1) * SECONDS_PER_DAY) / SECONDS_PER_DAY) *
    SECONDS_PER_DAY;
  let runningBalance = history.flows.at(0)?.totalValueBeforeRaw ?? currentBalanceRaw;
  let flowIndex = 0;
  for (let day = 0; day < HISTORY_DAYS; day++) {
    const dayStart = firstDayStart + day * SECONDS_PER_DAY;
    const dayEnd = dayStart + SECONDS_PER_DAY;
    let depositsRaw = 0n;
    let withdrawalsRaw = 0n;
    while (flowIndex < history.flows.length && history.flows[flowIndex].timestamp < dayEnd) {
      const flow = history.flows[flowIndex++];
      runningBalance = flow.totalValueAfterRaw;
      if (flow.kind === "deposit") depositsRaw += flow.amountRaw;
      else withdrawalsRaw += flow.amountRaw;
    }
    if (day === HISTORY_DAYS - 1) runningBalance = currentBalanceRaw;
    points.push({
      date: utcDay(dayStart),
      balanceRaw: runningBalance,
      depositsRaw,
      withdrawalsRaw,
    });
  }
  return points;
}
