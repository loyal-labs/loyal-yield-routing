import { createHash } from "node:crypto";

import { PublicKey } from "@solana/web3.js";
import bs58 from "bs58";

export const V06_SCHEMA = "loyal-backyard-rwa-live-lifecycle/v2";
export const V06_STEP_NAMES = [
  "deposit",
  "allocate",
  "open",
  "nav",
  "withdraw_request",
  "unwind",
  "restore",
  "predeadline_rejection",
  "claim",
  "conservation",
] as const;

export type V06StepName = typeof V06_STEP_NAMES[number];

export type V06RouteBindings = Readonly<{
  routeKey: string;
  genesisHash: string;
  withdrawalWaitSeconds: number;
  targetLtvBps: number;
  maxReportAgeSlots: number;
  programs: Readonly<{
    voltr: string;
    adaptor: string;
    squads: string;
    kamino: string;
    jupiter: string;
    token: string;
    associatedToken: string;
  }>;
  accounts: Readonly<{
    voltrVault: string;
    strategy: string;
    strategyReceipt: string;
    voltrIdleAta: string;
    strategyAta: string;
    squadsUsdcAta: string;
    squadsPrimeAta: string;
    obligation: string;
    collateralReserve: string;
    debtReserve: string;
    squadsSettings: string;
    squadsVault: string;
    reportTicket: string;
  }>;
  mints: Readonly<{ usdc: string; prime: string }>;
}>;

export type V06TokenBalance = Readonly<{
  address: string;
  mint: string;
  owner: string;
  beforeRaw: string;
  afterRaw: string;
}>;

export type V06EvidenceTransaction = Readonly<{
  signature: string;
  tokenBalances: readonly V06TokenBalance[];
}>;

export type V06EvidenceStep = Readonly<{
  name: V06StepName;
  transactions: readonly V06EvidenceTransaction[];
}>;

export type V06FinalAccountEvidence = Readonly<{
  address: string;
  owner: string | null;
  dataSha256: string | null;
}>;

export type V06NAVReportEvidence = Readonly<{
  signature: string;
  sequence: string;
  observedSlot: string;
  navAfterRaw: string;
  snapshotDigest: string;
}>;

export type V06LaunchYieldAttestation = Readonly<{
  schema: "loyal-backyard-rwa-launch-yield/v1";
  routeKey: string;
  strategyKey: "PRIME/USDC";
  method: "manual_external_total_route_yield";
  observedAt: string;
  validUntil: string;
  totalRouteYieldBps: number;
  source: string;
  attestationSha256: string;
}>;

export type V06LifecycleEvidence = Readonly<{
  schema: string;
  routeKey: string;
  genesisHash: string;
  commitment: string;
  broadcast: boolean;
  withdrawalWaitSeconds: number;
  userTransferAuthority: string;
  userUsdcAta: string;
  withdrawalReceipt: string;
  operationalAmountRaw: string;
  requestedWithdrawalRaw: string;
  realizedYieldRaw: string;
  explicitProtocolFeesRaw: string;
  retainedRaw: string;
  steps: readonly V06EvidenceStep[];
  finalAccounts: readonly V06FinalAccountEvidence[];
  navReports: readonly V06NAVReportEvidence[];
  launchYield: V06LaunchYieldAttestation;
}>;

export type V06ChainTransaction = Readonly<{
  signature: string;
  slot: number;
  blockTime: number;
  success: boolean;
  wireBase64: string;
  accountKeys: readonly string[];
  programIds: readonly string[];
  tokenBalances: readonly V06TokenBalance[];
  returnData: Readonly<{ programId: string; dataBase64: string }> | null;
  topLevelInstructions: readonly V06ChainInstruction[];
  innerInstructions: readonly V06ChainInstruction[];
}>;

export type V06ChainInstruction = Readonly<{
  groupIndex: number;
  position: number;
  stackHeight: number | null;
  programId: string;
  accounts: readonly string[];
  dataBase64: string;
}>;

export type V06ChainRead = Readonly<{
  attempted: boolean;
  error: string | null;
  genesisHash: string | null;
  transactions: readonly V06ChainTransaction[];
  finalContextSlot: number | null;
  finalAccounts: readonly V06FinalAccountEvidence[];
  finalAccountData: Readonly<Record<string, string | null>>;
}>;

export type V06DatabaseRow = Readonly<{
  operationId: string;
  cycle: number;
  action: string;
  status: string;
  transactionSignature: string;
  confirmedSlot: number;
  confirmationStatus: string;
  signedWireBase64: string;
  signedWireSha256: string;
  expectedEffects: unknown;
  reconciledEffects: unknown;
  reconciliationSha256: string;
  createdAt: string;
  broadcastIntentAt: string;
}>;

export type V06PositionSnapshot = Readonly<{
  observedSlot: number;
  collateralRaw: string;
  debtRaw: string;
  ltvBps: number;
  valuationSource: string;
}>;

export type V06DatabaseRead = Readonly<{
  attempted: boolean;
  error: string | null;
  rows: readonly V06DatabaseRow[];
  position: V06PositionSnapshot | null;
  nonterminalCount: number | null;
}>;

export type V06Validation = Readonly<{
  pass: boolean;
  checks: Readonly<Record<string, boolean>>;
  details: Readonly<Record<string, unknown>>;
}>;

const REQUIRED_ACTIONS = [
  "VOLTR_ALLOCATE_TO_SQUADS",
  "SWAP_USDC_TO_PRIME_STEP",
  "OPEN_PRIME_USDC_STEP",
  "REPORT_NAV",
  "DELEVER_PRIME_USDC_STEP",
  "SWAP_PRIME_TO_USDC_STEP",
  "STAGE_SQUADS_TO_VOLTR",
  "VOLTR_RESTORE_IDLE",
] as const;

const RISK_INCREASING_ACTIONS = new Set(["SWAP_USDC_TO_PRIME_STEP", "OPEN_PRIME_USDC_STEP"]);
const BRIDGE_REPORT_ACTIONS = new Set([
  "VOLTR_ALLOCATE_TO_SQUADS",
  "REPORT_NAV",
  "VOLTR_RESTORE_IDLE",
]);

const SQUADS_SYNC_DISCRIMINATOR = Buffer.from([90, 81, 187, 81, 39, 70, 128, 78]);
const ARM_REPORT_DISCRIMINATOR = Buffer.from([0xa4, 0xaf, 0xf6, 0x29, 0xb2, 0x8c, 0x23, 0x03]);
const ADAPTOR_DEPOSIT_DISCRIMINATOR = Buffer.from([242, 35, 198, 137, 82, 225, 242, 182]);
const ADAPTOR_WITHDRAW_DISCRIMINATOR = Buffer.from([183, 18, 70, 156, 148, 109, 161, 34]);
const VOLTR_DEPOSIT_DISCRIMINATOR = Buffer.from([246, 82, 57, 226, 131, 222, 253, 249]);
const VOLTR_WITHDRAW_DISCRIMINATOR = Buffer.from([31, 45, 162, 5, 193, 217, 134, 188]);
const REPORT_TICKET_DISCRIMINATOR = Buffer.from([0xf5, 0x68, 0xb6, 0xc5, 0x3a, 0xe7, 0x74, 0xed]);

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function record(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

function validAddress(value: unknown): value is string {
  if (typeof value !== "string") return false;
  try {
    return bs58.decode(value).length === 32;
  } catch {
    return false;
  }
}

function validSignature(value: unknown): value is string {
  if (typeof value !== "string") return false;
  try {
    return bs58.decode(value).length === 64;
  } catch {
    return false;
  }
}

function validHash(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
}

function canonicalBase64(value: unknown): Buffer | null {
  if (typeof value !== "string" || value.length === 0) return null;
  const decoded = Buffer.from(value, "base64");
  return decoded.length > 0 && decoded.toString("base64") === value ? decoded : null;
}

function derivedUserBindings(evidence: V06LifecycleEvidence, route: V06RouteBindings): boolean {
  try {
    const user = new PublicKey(evidence.userTransferAuthority);
    const vault = new PublicKey(route.accounts.voltrVault);
    const mint = new PublicKey(route.mints.usdc);
    const token = new PublicKey(route.programs.token);
    const receipt = PublicKey.findProgramAddressSync([
      Buffer.from("request_withdraw_vault_receipt"),
      vault.toBuffer(),
      user.toBuffer(),
    ], new PublicKey(route.programs.voltr))[0];
    const ata = PublicKey.findProgramAddressSync([
      user.toBuffer(),
      token.toBuffer(),
      mint.toBuffer(),
    ], new PublicKey(route.programs.associatedToken))[0];
    return receipt.toBase58() === evidence.withdrawalReceipt && ata.toBase58() === evidence.userUsdcAta;
  } catch {
    return false;
  }
}

function unsigned(value: unknown): bigint | null {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value)) return null;
  try {
    const parsed = BigInt(value);
    return parsed <= 18_446_744_073_709_551_615n ? parsed : null;
  } catch {
    return null;
  }
}

function canonicalTokenBalances(rows: readonly V06TokenBalance[]): string | null {
  const canonical: string[] = [];
  const seen = new Set<string>();
  for (const row of rows) {
    if (!validAddress(row.address) || !validAddress(row.mint) || !validAddress(row.owner)
      || unsigned(row.beforeRaw) === null || unsigned(row.afterRaw) === null || seen.has(row.address)) {
      return null;
    }
    seen.add(row.address);
    canonical.push(`${row.address}:${row.mint}:${row.owner}:${row.beforeRaw}:${row.afterRaw}`);
  }
  return canonical.sort().join("|");
}

function exactStrings(observed: readonly string[], expected: readonly string[]): boolean {
  return observed.length === expected.length && observed.every((value, index) => value === expected[index]);
}

function stepMap(evidence: V06LifecycleEvidence): Map<V06StepName, V06EvidenceStep> | null {
  if (!Array.isArray(evidence.steps) || evidence.steps.length !== V06_STEP_NAMES.length) return null;
  const map = new Map<V06StepName, V06EvidenceStep>();
  for (const step of evidence.steps) {
    if (!V06_STEP_NAMES.includes(step.name) || map.has(step.name) || !Array.isArray(step.transactions)) return null;
    map.set(step.name, step);
  }
  return exactStrings(evidence.steps.map(({ name }) => name), V06_STEP_NAMES) ? map : null;
}

function allEvidenceTransactions(evidence: V06LifecycleEvidence): V06EvidenceTransaction[] {
  return evidence.steps.flatMap(({ transactions }) => transactions);
}

function expectedFinalAccountOwners(route: V06RouteBindings, withdrawalReceipt: string): Map<string, string | null> {
  return new Map([
    [route.accounts.voltrVault, route.programs.voltr],
    [route.accounts.strategy, route.programs.adaptor],
    [route.accounts.strategyReceipt, route.programs.voltr],
    [route.accounts.voltrIdleAta, route.programs.token],
    [route.accounts.strategyAta, route.programs.token],
    [route.accounts.squadsUsdcAta, route.programs.token],
    [route.accounts.squadsPrimeAta, route.programs.token],
    [route.accounts.obligation, route.programs.kamino],
    [route.accounts.collateralReserve, route.programs.kamino],
    [route.accounts.debtReserve, route.programs.kamino],
    [route.accounts.reportTicket, route.programs.adaptor],
    [withdrawalReceipt, null],
  ]);
}

function exactFinalAccounts(evidence: V06LifecycleEvidence, route: V06RouteBindings, chain: V06ChainRead): boolean {
  const expected = expectedFinalAccountOwners(route, evidence.withdrawalReceipt);
  if (evidence.finalAccounts.length !== expected.size || chain.finalAccounts.length !== expected.size) return false;
  const evidenceByAddress = new Map(evidence.finalAccounts.map((row) => [row.address, row]));
  const chainByAddress = new Map(chain.finalAccounts.map((row) => [row.address, row]));
  for (const [address, owner] of expected) {
    const declared = evidenceByAddress.get(address);
    const observed = chainByAddress.get(address);
    if (!declared || !observed || declared.owner !== owner || observed.owner !== owner
      || declared.dataSha256 !== observed.dataSha256
      || (owner === null ? declared.dataSha256 !== null : !validHash(declared.dataSha256))) return false;
  }
  return true;
}

function transactionMap(chain: V06ChainRead): Map<string, V06ChainTransaction> | null {
  const map = new Map<string, V06ChainTransaction>();
  for (const transaction of chain.transactions) {
    if (!validSignature(transaction.signature) || map.has(transaction.signature)) return null;
    map.set(transaction.signature, transaction);
  }
  return map;
}

function includesAll(values: readonly string[], expected: readonly string[]): boolean {
  const set = new Set(values);
  return expected.every((value) => set.has(value));
}

function stepTopology(
  steps: Map<V06StepName, V06EvidenceStep>,
  transactions: Map<string, V06ChainTransaction>,
  route: V06RouteBindings,
  evidence: V06LifecycleEvidence,
): boolean {
  const combined = (name: V06StepName, field: "accountKeys" | "programIds") =>
    steps.get(name)!.transactions.flatMap(({ signature }) => transactions.get(signature)?.[field] ?? []);
  const success = (name: V06StepName) => steps.get(name)!.transactions.every(({ signature }) => transactions.get(signature)?.success === true);
  const nonempty = V06_STEP_NAMES.filter((name) => name !== "conservation")
    .every((name) => (steps.get(name)?.transactions.length ?? 0) > 0);
  const conservationEmpty = steps.get("conservation")?.transactions.length === 0;
  if (!nonempty || !conservationEmpty) return false;
  if (!["deposit", "allocate", "open", "nav", "withdraw_request", "unwind", "restore", "claim"].every((name) => success(name as V06StepName))) return false;
  if (!steps.get("predeadline_rejection")!.transactions.every(({ signature }) => transactions.get(signature)?.success === false)) return false;
  const accountRequirements: Readonly<Record<string, readonly string[]>> = {
    deposit: [route.accounts.voltrVault, route.accounts.voltrIdleAta, evidence.userUsdcAta],
    allocate: [route.accounts.voltrVault, route.accounts.strategy, route.accounts.strategyReceipt,
      route.accounts.voltrIdleAta, route.accounts.strategyAta, route.accounts.squadsUsdcAta],
    open: [route.accounts.obligation, route.accounts.collateralReserve, route.accounts.debtReserve,
      route.accounts.squadsUsdcAta, route.accounts.squadsPrimeAta],
    nav: [route.accounts.strategy, route.accounts.strategyReceipt],
    withdraw_request: [route.accounts.voltrVault, evidence.withdrawalReceipt],
    unwind: [route.accounts.obligation, route.accounts.collateralReserve, route.accounts.debtReserve,
      route.accounts.squadsUsdcAta, route.accounts.squadsPrimeAta],
    restore: [route.accounts.voltrVault, route.accounts.strategy, route.accounts.strategyReceipt,
      route.accounts.voltrIdleAta, route.accounts.strategyAta, route.accounts.squadsUsdcAta],
    predeadline_rejection: [route.accounts.voltrVault, evidence.withdrawalReceipt],
    claim: [route.accounts.voltrVault, route.accounts.voltrIdleAta, evidence.userUsdcAta, evidence.withdrawalReceipt],
  };
  for (const [name, addresses] of Object.entries(accountRequirements)) {
    if (!includesAll(combined(name as V06StepName, "accountKeys"), addresses)) return false;
  }
  const programRequirements: Readonly<Record<string, readonly string[]>> = {
    deposit: [route.programs.voltr],
    allocate: [route.programs.squads, route.programs.voltr, route.programs.adaptor],
    open: [route.programs.squads, route.programs.kamino, route.programs.jupiter],
    nav: [route.programs.squads, route.programs.voltr, route.programs.adaptor],
    withdraw_request: [route.programs.voltr],
    unwind: [route.programs.squads, route.programs.kamino, route.programs.jupiter],
    restore: [route.programs.squads, route.programs.voltr, route.programs.adaptor],
    predeadline_rejection: [route.programs.voltr],
    claim: [route.programs.voltr],
  };
  for (const [name, programs] of Object.entries(programRequirements)) {
    if (!includesAll(combined(name as V06StepName, "programIds"), programs)) return false;
  }
  return true;
}

function exactChainRows(evidence: V06LifecycleEvidence, chain: V06ChainRead): boolean {
  const declared = allEvidenceTransactions(evidence);
  const signatures = declared.map(({ signature }) => signature);
  if (new Set(signatures).size !== signatures.length || !signatures.every(validSignature)) return false;
  const observed = transactionMap(chain);
  if (observed === null || observed.size !== signatures.length) return false;
  return declared.every((row) => {
    const transaction = observed.get(row.signature);
    return transaction !== undefined
      && canonicalTokenBalances(row.tokenBalances) !== null
      && canonicalTokenBalances(row.tokenBalances) === canonicalTokenBalances(transaction.tokenBalances)
      && canonicalBase64(transaction.wireBase64) !== null;
  });
}

function orderedAndTimed(
  steps: Map<V06StepName, V06EvidenceStep>,
  transactions: Map<string, V06ChainTransaction>,
  waitSeconds: number,
): boolean {
  let priorSlot = 0;
  for (const name of V06_STEP_NAMES) {
    for (const row of steps.get(name)!.transactions) {
      const transaction = transactions.get(row.signature);
      if (!transaction || transaction.slot < priorSlot || transaction.blockTime <= 0) return false;
      priorSlot = transaction.slot;
    }
  }
  const request = transactions.get(steps.get("withdraw_request")!.transactions.at(-1)!.signature)!;
  const rejected = transactions.get(steps.get("predeadline_rejection")!.transactions.at(-1)!.signature)!;
  const claim = transactions.get(steps.get("claim")!.transactions.at(-1)!.signature)!;
  return rejected.blockTime >= request.blockTime
    && rejected.blockTime < request.blockTime + waitSeconds
    && claim.blockTime >= request.blockTime + waitSeconds;
}

function balanceDelta(transaction: V06ChainTransaction, address: string): bigint | null {
  const row = transaction.tokenBalances.find((balance) => balance.address === address);
  if (!row) return null;
  const before = unsigned(row.beforeRaw);
  const after = unsigned(row.afterRaw);
  return before === null || after === null ? null : after - before;
}

function amountsConserve(
  evidence: V06LifecycleEvidence,
  steps: Map<V06StepName, V06EvidenceStep>,
  transactions: Map<string, V06ChainTransaction>,
  route: V06RouteBindings,
): boolean {
  const operational = unsigned(evidence.operationalAmountRaw);
  const requested = unsigned(evidence.requestedWithdrawalRaw);
  const yieldRaw = unsigned(evidence.realizedYieldRaw);
  const fees = unsigned(evidence.explicitProtocolFeesRaw);
  const retained = unsigned(evidence.retainedRaw);
  if (operational === null || operational === 0n || requested === null || requested === 0n
    || yieldRaw === null || fees === null || retained === null) return false;
  const deposit = transactions.get(steps.get("deposit")!.transactions.at(-1)!.signature)!;
  const restore = transactions.get(steps.get("restore")!.transactions.at(-1)!.signature)!;
  const claim = transactions.get(steps.get("claim")!.transactions.at(-1)!.signature)!;
  return balanceDelta(deposit, evidence.userUsdcAta) === -operational
    && balanceDelta(deposit, route.accounts.voltrIdleAta) === operational
    && balanceDelta(restore, route.accounts.voltrIdleAta) === requested
    && balanceDelta(claim, route.accounts.voltrIdleAta) === -requested
    && balanceDelta(claim, evidence.userUsdcAta) === requested
    && operational + yieldRaw === requested + fees + retained;
}

function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value !== null && typeof value === "object") {
    return `{${Object.entries(value as Record<string, unknown>)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function reconciledEffectsExact(row: V06DatabaseRow): boolean {
  const effects = record(row.reconciledEffects);
  if (effects?.schema !== "loyal-backyard-rwa-reconciled-effects/v1"
    || effects.slot !== row.confirmedSlot || !Array.isArray(effects.accounts)
    || !effects.accounts.every((value) => typeof value === "string")) return false;
  // Go encoding/json sorts map keys. PostgreSQL JSONB does not preserve the
  // original bytes, so reconstruct those exact bytes before checking the
  // independently persisted reconciliation digest.
  const goBytes = JSON.stringify({
    accounts: effects.accounts,
    schema: effects.schema,
    slot: effects.slot,
  });
  return validHash(row.reconciliationSha256) && sha256(goBytes) === row.reconciliationSha256;
}

function expectedEffectsExact(row: V06DatabaseRow, transaction: V06ChainTransaction, route: V06RouteBindings): boolean {
  const envelope = record(row.expectedEffects);
  const effects = record(envelope?.expectedEffects);
  if (envelope?.schema !== "loyal-backyard-rwa-operation-evidence/v1"
    || effects?.schema !== "loyal-backyard-rwa-expected-effects/v1" || !Array.isArray(effects.accounts)) return false;
  const chainByAddress = new Map(transaction.tokenBalances.map((value) => [value.address, value]));
  return effects.accounts.length > 0 && effects.accounts.every((value) => {
    const effect = record(value);
    const actual = typeof effect?.address === "string" ? chainByAddress.get(effect.address) : undefined;
    const after = unsigned(effect?.afterRaw);
    const minimum = effect?.minimumAfterRaw === undefined ? null : unsigned(effect.minimumAfterRaw);
    return actual !== undefined && effect?.owner === route.programs.token && effect?.mint === actual.mint
      && effect?.authority === actual.owner && unsigned(effect?.beforeRaw) === unsigned(actual.beforeRaw)
      && after !== null && (minimum === null ? after === unsigned(actual.afterRaw) : unsigned(actual.afterRaw)! >= minimum);
  });
}

function reportBytes(report: V06NAVReportEvidence): Buffer | null {
  const sequence = unsigned(report.sequence);
  const slot = unsigned(report.observedSlot);
  const nav = unsigned(report.navAfterRaw);
  if (sequence === null || sequence === 0n || slot === null || sequence !== slot || nav === null || !validHash(report.snapshotDigest)) return null;
  const output = Buffer.alloc(57);
  output[0] = 1;
  output.writeBigUInt64LE(sequence, 1);
  output.writeBigUInt64LE(slot, 9);
  output.writeBigUInt64LE(nav, 17);
  Buffer.from(report.snapshotDigest, "hex").copy(output, 25);
  return output;
}

export function validateV06ReportIdentity(report: V06NAVReportEvidence): boolean {
  return reportBytes(report) !== null;
}

export function validateV06ReturnDataNAV(
  transaction: Pick<V06ChainTransaction, "returnData">,
  report: V06NAVReportEvidence,
  route: Pick<V06RouteBindings, "programs">,
): boolean {
  const returned = transaction.returnData;
  if (returned === null || ![route.programs.adaptor, route.programs.voltr].includes(returned.programId)) return false;
  const returnBytes = canonicalBase64(returned.dataBase64);
  return returnBytes !== null && returnBytes.length === 8
    && returnBytes.readBigUInt64LE(0) === unsigned(report.navAfterRaw);
}

type DecodedSquadsInstruction = Readonly<{
  programId: string;
  accounts: readonly string[];
  data: Buffer;
}>;

function decodeExactSquadsPayload(transaction: V06ChainTransaction, route: V06RouteBindings): DecodedSquadsInstruction[] | null {
  const outer = transaction.topLevelInstructions.filter(({ programId }) => programId === route.programs.squads);
  if (transaction.topLevelInstructions.length !== 1 || outer.length !== 1) return null;
  const data = canonicalBase64(outer[0]!.dataBase64);
  if (data === null || data.length < 24 || !data.subarray(0, 8).equals(SQUADS_SYNC_DISCRIMINATOR)
    || !data.subarray(8, 13).equals(Buffer.from([0, 1, 1, 1, 1]))) return null;
  let offset = 13;
  const constraintCount = data.readUInt32LE(offset);
  offset += 4;
  if (constraintCount !== 2 || offset + constraintCount + 6 > data.length) return null;
  const constraintIndexes = data.subarray(offset, offset + constraintCount);
  offset += constraintCount;
  if (!constraintIndexes.equals(Buffer.from([0, 1])) || data[offset++] !== 1 || data[offset++] !== 0) return null;
  const compiledLength = data.readUInt32LE(offset);
  offset += 4;
  if (compiledLength < 1 || offset + compiledLength !== data.length) return null;
  const end = offset + compiledLength;
  const count = data[offset++]!;
  if (count !== 2) return null;
  const decoded: DecodedSquadsInstruction[] = [];
  for (let position = 0; position < count; position++) {
    if (offset + 2 > end) return null;
    const programIndex = data[offset++]!;
    const accountCount = data[offset++]!;
    if (offset + accountCount + 2 > end) return null;
    const accountIndexes = [...data.subarray(offset, offset + accountCount)];
    offset += accountCount;
    const dataLength = data.readUInt16LE(offset);
    offset += 2;
    if (offset + dataLength > end) return null;
    const localAccounts = outer[0]!.accounts.slice(3);
    const programId = localAccounts[programIndex];
    const accounts = accountIndexes.map((index) => localAccounts[index]);
    if (!programId || accounts.some((address) => !address)) return null;
    decoded.push({ programId, accounts: accounts as string[], data: data.subarray(offset, offset + dataLength) });
    offset += dataLength;
  }
  return offset === end ? decoded : null;
}

export function validateV06TicketedTransaction(
  transaction: V06ChainTransaction,
  report: V06NAVReportEvidence,
  route: V06RouteBindings,
  action: string,
): boolean {
  const decoded = decodeExactSquadsPayload(transaction, route);
  if (decoded === null || decoded.length !== 2) return false;
  const [arm, capital] = decoded;
  const withdraw = action === "VOLTR_RESTORE_IDLE";
  if (!withdraw && action !== "VOLTR_ALLOCATE_TO_SQUADS" && action !== "REPORT_NAV") return false;
  const voltrDiscriminator = withdraw ? VOLTR_WITHDRAW_DISCRIMINATOR : VOLTR_DEPOSIT_DISCRIMINATOR;
  const adaptorDiscriminator = withdraw ? ADAPTOR_WITHDRAW_DISCRIMINATOR : ADAPTOR_DEPOSIT_DISCRIMINATOR;
  const encodedReport = reportBytes(report);
  if (encodedReport === null || arm!.programId !== route.programs.adaptor || capital!.programId !== route.programs.voltr
    || arm!.accounts.length !== 5 || capital!.accounts.length !== 18
    || !exactStrings(arm!.accounts, [route.accounts.strategy, route.accounts.reportTicket,
      route.accounts.squadsSettings, route.accounts.squadsVault, route.programs.squads])
    || capital!.accounts[17] !== route.accounts.reportTicket
    || arm!.data.length !== 79 || !arm!.data.subarray(0, 8).equals(ARM_REPORT_DISCRIMINATOR)
    || arm!.data[8] !== (withdraw ? 1 : 0)
    || capital!.data.length !== 91 || !capital!.data.subarray(0, 8).equals(voltrDiscriminator)
    || capital!.data[16] !== 1 || capital!.data.readUInt32LE(17) !== 8
    || !capital!.data.subarray(21, 29).equals(adaptorDiscriminator)
    || capital!.data[29] !== 1 || capital!.data.readUInt32LE(30) !== 57
    || !capital!.data.subarray(34).equals(encodedReport)) return false;
  const expectedTail = Buffer.concat([capital!.data.subarray(8, 16), capital!.data.subarray(29)]);
  if (!arm!.data.subarray(9).equals(expectedTail)) return false;

  const trace = transaction.innerInstructions;
  const armTrace = trace.findIndex((instruction) => instruction.programId === route.programs.adaptor
    && canonicalBase64(instruction.dataBase64)?.equals(arm!.data) === true
    && exactStrings(instruction.accounts, arm!.accounts));
  const voltrTrace = trace.findIndex((instruction, index) => index > armTrace
    && instruction.programId === route.programs.voltr
    && canonicalBase64(instruction.dataBase64)?.equals(capital!.data) === true
    && exactStrings(instruction.accounts, capital!.accounts));
  const consumeTrace = trace.findIndex((instruction, index) => index > voltrTrace
    && instruction.programId === route.programs.adaptor
    && canonicalBase64(instruction.dataBase64)?.subarray(0, 8).equals(adaptorDiscriminator) === true
    && instruction.accounts.length > 8 && instruction.accounts[8] === route.accounts.reportTicket);
  return armTrace >= 0 && voltrTrace > armTrace && consumeTrace > voltrTrace;
}

export function validateV06FinalTicket(
  dataBase64: string | null | undefined,
  route: V06RouteBindings,
  lastSequence: string,
): boolean {
  if (typeof dataBase64 !== "string") return false;
  const data = canonicalBase64(dataBase64);
  const expectedSequence = unsigned(lastSequence);
  if (data === null || data.length !== 96 || expectedSequence === null || expectedSequence === 0n
    || !data.subarray(0, 8).equals(REPORT_TICKET_DISCRIMINATOR)
    || data[8] !== 1 || data[9] !== 254 || data[10] !== 0
    || !data.subarray(11, 16).every((value) => value === 0)
    || !data.subarray(16, 48).equals(new PublicKey(route.accounts.strategy).toBuffer())
    || data.readBigUInt64LE(48) !== expectedSequence || data.readBigUInt64LE(56) !== 0n
    || !data.subarray(64, 96).every((value) => value === 0)) return false;
  return PublicKey.findProgramAddressSync([
    Buffer.from("report_ticket"), new PublicKey(route.accounts.strategy).toBuffer(),
  ], new PublicKey(route.programs.adaptor))[0].toBase58() === route.accounts.reportTicket;
}

function reportsExact(
  evidence: V06LifecycleEvidence,
  rows: readonly V06DatabaseRow[],
  transactions: Map<string, V06ChainTransaction>,
  chain: V06ChainRead,
  route: V06RouteBindings,
): boolean {
  const bridgeRows = rows.filter(({ action }) => BRIDGE_REPORT_ACTIONS.has(action));
  if (evidence.navReports.length !== bridgeRows.length) return false;
  const reports = new Map(evidence.navReports.map((report) => [report.signature, report]));
  if (reports.size !== evidence.navReports.length) return false;
  let priorObservedSlot = 0n;
  for (const row of bridgeRows) {
    const report = reports.get(row.transactionSignature);
    const transaction = transactions.get(row.transactionSignature);
    const encoded = report ? reportBytes(report) : null;
    const envelope = record(row.expectedEffects);
    const decision = record(envelope?.decision);
    const observationSlot = decision?.observationSlot;
    if (!report || !transaction || encoded === null
      || typeof observationSlot !== "number" || !Number.isSafeInteger(observationSlot) || observationSlot <= 0
      || report.observedSlot !== String(observationSlot) || observationSlot > row.confirmedSlot
      || row.confirmedSlot - observationSlot > route.maxReportAgeSlots) return false;
    if (!validateV06ReturnDataNAV(transaction, report, route)) return false;
    if (!validateV06TicketedTransaction(transaction, report, route, row.action)) return false;
    const sequence = unsigned(report.sequence)!;
    const observedSlot = unsigned(report.observedSlot)!;
    if (sequence !== observedSlot || observedSlot <= priorObservedSlot) return false;
    priorObservedSlot = observedSlot;
    const wire = canonicalBase64(transaction.wireBase64);
    if (wire === null) return false;
    if (wire.indexOf(encoded) < 0 || wire.indexOf(encoded, wire.indexOf(encoded) + 1) >= 0) return false;
  }
  const latest = [...evidence.navReports].sort((left, right) => {
    const a = unsigned(left.sequence)!;
    const b = unsigned(right.sequence)!;
    return a < b ? -1 : a > b ? 1 : 0;
  }).at(-1);
  if (!latest) return false;
  const configBase64 = chain.finalAccountData[route.accounts.strategy];
  const receiptBase64 = chain.finalAccountData[route.accounts.strategyReceipt];
  if (typeof configBase64 !== "string" || typeof receiptBase64 !== "string") return false;
  const config = Buffer.from(configBase64, "base64");
  const receipt = Buffer.from(receiptBase64, "base64");
  return config.length === 472 && receipt.length === 192
    && config.subarray(416, 472).every((value) => value === 0)
    && receipt.readBigUInt64LE(104) === unsigned(latest.navAfterRaw)
    && validateV06FinalTicket(chain.finalAccountData[route.accounts.reportTicket], route, latest.sequence);
}

function databaseExact(
  database: V06DatabaseRead,
  transactions: Map<string, V06ChainTransaction>,
  requestSlot: number,
  route: V06RouteBindings,
): boolean {
  if (!database.attempted || database.error !== null || database.nonterminalCount !== 0 || database.rows.length === 0) return false;
  const signatures = new Set<string>();
  const actionCounts = new Map<string, number>();
  let lifecycleCycle: number | null = null;
  let priorSlot = 0;
  for (const row of database.rows) {
    const transaction = transactions.get(row.transactionSignature);
    const wire = canonicalBase64(row.signedWireBase64);
    const chainWire = transaction === undefined ? null : canonicalBase64(transaction.wireBase64);
    if (!transaction || signatures.has(row.transactionSignature) || !Number.isSafeInteger(row.cycle) || row.cycle <= 0
      || (lifecycleCycle !== null && row.cycle !== lifecycleCycle) || row.confirmedSlot < priorSlot
      || row.status !== "reconciled" || row.confirmedSlot !== transaction.slot
      || !["confirmed", "finalized"].includes(row.confirmationStatus)
      || wire === null || chainWire === null || !validHash(row.signedWireSha256) || sha256(wire) !== row.signedWireSha256
      || !wire.equals(chainWire)
      || !validHash(row.reconciliationSha256) || !reconciledEffectsExact(row)
      || !expectedEffectsExact(row, transaction, route)
      || !Number.isFinite(Date.parse(row.createdAt)) || !Number.isFinite(Date.parse(row.broadcastIntentAt))
      || Date.parse(row.createdAt) >= Date.parse(row.broadcastIntentAt)) return false;
    signatures.add(row.transactionSignature);
    actionCounts.set(row.action, (actionCounts.get(row.action) ?? 0) + 1);
    lifecycleCycle = row.cycle;
    priorSlot = row.confirmedSlot;
    if (RISK_INCREASING_ACTIONS.has(row.action) && row.confirmedSlot > requestSlot) return false;
  }
  return validateV06ActionCoverage([...actionCounts.entries()]);
}

export function validateV06ActionCoverage(counts: readonly (readonly [string, number])[]): boolean {
  const actionCounts = new Map(counts);
  return counts.length === actionCounts.size
    && counts.every(([action, count]) => REQUIRED_ACTIONS.includes(action as typeof REQUIRED_ACTIONS[number])
      && Number.isSafeInteger(count) && count > 0)
    && (actionCounts.get("VOLTR_ALLOCATE_TO_SQUADS") ?? 0) === 1
    && (actionCounts.get("SWAP_USDC_TO_PRIME_STEP") ?? 0) === 2
    && (actionCounts.get("OPEN_PRIME_USDC_STEP") ?? 0) === 3
    && (actionCounts.get("REPORT_NAV") ?? 0) >= 1
    && (actionCounts.get("SWAP_PRIME_TO_USDC_STEP") ?? 0) >= 1
    && (actionCounts.get("DELEVER_PRIME_USDC_STEP") ?? 0) >= 3
    && (actionCounts.get("STAGE_SQUADS_TO_VOLTR") ?? 0) === 1
    && (actionCounts.get("VOLTR_RESTORE_IDLE") ?? 0) === 1;
}

export function validateV06PositionSnapshot(
  position: V06PositionSnapshot | null,
  completedLoopSlot: number,
  requestSlot: number,
  targetLtvBps: number,
): boolean {
  return position !== null && position.valuationSource === "backyard_rwa_v1_onchain_position_only"
    && position.observedSlot >= completedLoopSlot && position.observedSlot <= requestSlot
    && unsigned(position.collateralRaw) !== null && unsigned(position.collateralRaw)! > 0n
    && unsigned(position.debtRaw) !== null && unsigned(position.debtRaw)! > 0n
    && Number.isInteger(position.ltvBps) && position.ltvBps >= 0 && position.ltvBps <= targetLtvBps;
}

function launchYieldPayload(attestation: V06LaunchYieldAttestation): Readonly<Record<string, unknown>> {
  return {
    schema: attestation.schema,
    routeKey: attestation.routeKey,
    strategyKey: attestation.strategyKey,
    method: attestation.method,
    observedAt: attestation.observedAt,
    validUntil: attestation.validUntil,
    totalRouteYieldBps: attestation.totalRouteYieldBps,
    source: attestation.source,
  };
}

export function v06LaunchYieldAttestationHash(attestation: V06LaunchYieldAttestation): string {
  return sha256(canonicalJson(launchYieldPayload(attestation)));
}

export function validateV06LaunchYieldAttestation(
  attestationValue: unknown,
  routeKey: string,
  launchBlockTimeSeconds: number,
): boolean {
  const attestation = record(attestationValue) as V06LaunchYieldAttestation | null;
  if (attestation === null || attestation.schema !== "loyal-backyard-rwa-launch-yield/v1"
    || attestation.routeKey !== routeKey || attestation.strategyKey !== "PRIME/USDC"
    || attestation.method !== "manual_external_total_route_yield"
    || !Number.isInteger(attestation.totalRouteYieldBps) || attestation.totalRouteYieldBps <= 0
    || typeof attestation.source !== "string" || attestation.source.trim().length === 0
    || attestation.source.length > 2_048 || !validHash(attestation.attestationSha256)
    || !Number.isSafeInteger(launchBlockTimeSeconds) || launchBlockTimeSeconds <= 0) return false;
  const observedAt = Date.parse(attestation.observedAt);
  const validUntil = Date.parse(attestation.validUntil);
  const launchAt = launchBlockTimeSeconds * 1_000;
  const maximumWindowMS = 24 * 60 * 60 * 1_000;
  return Number.isFinite(observedAt) && Number.isFinite(validUntil)
    && observedAt <= launchAt && launchAt <= validUntil
    && launchAt - observedAt <= maximumWindowMS && validUntil - observedAt <= maximumWindowMS
    && v06LaunchYieldAttestationHash(attestation) === attestation.attestationSha256;
}

function positionExact(database: V06DatabaseRead, openSlot: number, requestSlot: number, route: V06RouteBindings): boolean {
  const position = database.position;
  const completedLoopSlot = database.rows
    .filter(({ action }) => action === "OPEN_PRIME_USDC_STEP")
    .reduce((maximum, { confirmedSlot }) => Math.max(maximum, confirmedSlot), openSlot);
  return validateV06PositionSnapshot(position, completedLoopSlot, requestSlot, route.targetLtvBps);
}

export function validateV06Lifecycle(
  evidenceValue: unknown,
  route: V06RouteBindings,
  chain: V06ChainRead,
  database: V06DatabaseRead,
): V06Validation {
  const evidence = record(evidenceValue) as V06LifecycleEvidence | null;
  const steps = evidence === null ? null : stepMap(evidence);
  const transactions = transactionMap(chain);
  const shape = evidence !== null && steps !== null
    && evidence.schema === V06_SCHEMA && evidence.routeKey === route.routeKey
    && evidence.genesisHash === route.genesisHash && evidence.commitment === "confirmed"
    && evidence.broadcast === true && evidence.withdrawalWaitSeconds === route.withdrawalWaitSeconds
    && validAddress(evidence.userTransferAuthority) && validAddress(evidence.userUsdcAta)
    && validAddress(evidence.withdrawalReceipt) && derivedUserBindings(evidence, route);
  const chainExact = shape && chain.attempted && chain.error === null && chain.genesisHash === route.genesisHash
    && exactChainRows(evidence!, chain) && transactions !== null;
  const topology = chainExact && stepTopology(steps!, transactions!, route, evidence!);
  const timing = topology && orderedAndTimed(steps!, transactions!, route.withdrawalWaitSeconds);
  const conservation = topology && amountsConserve(evidence!, steps!, transactions!, route);
  const finalAccounts = shape && chain.attempted && chain.error === null && exactFinalAccounts(evidence!, route, chain);
  const requestSlot = topology
    ? transactions!.get(steps!.get("withdraw_request")!.transactions.at(-1)!.signature)!.slot
    : 0;
  const openSlot = topology
    ? transactions!.get(steps!.get("open")!.transactions[0]!.signature)!.slot
    : 0;
  const openBlockTime = topology
    ? transactions!.get(steps!.get("open")!.transactions[0]!.signature)!.blockTime
    : 0;
  const durableReconciliation = topology && databaseExact(database, transactions!, requestSlot, route);
  const safePosition = durableReconciliation && positionExact(database, openSlot, requestSlot, route);
  const positiveExternalYield = shape
    && validateV06LaunchYieldAttestation(evidence!.launchYield, route.routeKey, openBlockTime);
  const authenticatedNAV = durableReconciliation && finalAccounts
    && reportsExact(evidence!, database.rows, transactions!, chain, route);
  const checks = {
    evidenceShape: shape,
    independentConfirmedTransactions: chainExact,
    exactProgramAndAccountTopology: topology,
    withdrawalLockAndOrdering: timing,
    exactDepositRestoreClaimAndConservation: conservation,
    exactFinalAccounts: finalAccounts,
    persistedBeforeSendAndReconciled: durableReconciliation,
    targetLtvPosition: safePosition,
    positiveExternalLaunchYield: positiveExternalYield,
    authenticatedNavSequenceAndReceipt: authenticatedNAV,
  };
  return {
    pass: Object.values(checks).every(Boolean),
    checks,
    details: {
      transactionCount: chain.transactions.length,
      databaseRowCount: database.rows.length,
      finalContextSlot: chain.finalContextSlot,
      requestSlot: requestSlot || null,
      positionSlot: database.position?.observedSlot ?? null,
      launchYieldMethod: evidence?.launchYield?.method ?? null,
      launchYieldBps: evidence?.launchYield?.totalRouteYieldBps ?? null,
      evidenceCanonicalSha256: evidence === null ? null : sha256(canonicalJson(evidence)),
    },
  };
}
