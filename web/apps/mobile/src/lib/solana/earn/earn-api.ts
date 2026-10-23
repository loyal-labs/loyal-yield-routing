import type {
  AutodepositCloseConfirmRequest,
  AutodepositFloorConfirmRequest,
  AutodepositSetupConfirmRequest,
  AutodepositSweepsExecuteRequest,
  AutodepositSweepsExecuteResponse,
  AutodepositSweepsProgressResponse,
  AutodepositToggleConfirmRequest,
  DprepResponse,
  EarnCleanupConfirmRequest,
  EarnCleanupPrepareRequest,
  EarnCleanupPrepareResponse,
  EarnDepositConfirmRequest,
  EarnPolicyConfirmRequest,
  EarnPolicyPrepareResponse,
  EarnPolicyRefundPrepareResponse,
  EarnWithdrawConfirmRequest,
  EarnWithdrawPrepareResponse,
  EarnPolicyRefundScanResponse,
  EarnStateAutodeposit,
  EarnStateResponse,
  EarningsBar,
  EarningsRange,
  EarningsRangeSet,
  SolanaWeekQuestProgressResponse,
} from "@loyal-labs/api";
import { env } from "@/config/env";
import { FetchTimeoutError } from "@/lib/network/fetch-with-timeout";
import {
  EARN_AUTODEPOSIT_PROGRESS_STATES,
  type EarnAutodepositProgressState,
  type EarnForecastApyHistorySample,
  type EarnForecastApyHistorySeries,
  type EarnForecastSummaryResponse,
  type EarnTransactionsResponse,
  type SerializedEarnTransaction,
} from "@loyal-labs/shared";
import type {
  SmartAccountEarnUsdcReserveWithdrawalMetadata,
  SmartAccountPreparedEarnUsdcAutodepositClose,
  SmartAccountPreparedEarnUsdcAutodepositSetup,
} from "@loyal-labs/smart-account-vaults";
import type { LifecycleErrorDetail } from "@/services/observability";

import { earnHeaders, earnWalletRead } from "./earn-session";

export type EarnAuthFields = {
  walletAddress: string;
  signature: string;
  issuedAt: string;
};

export type EarnSessionAuth = { sessionToken: string };

export class EarnApiError extends Error {
  readonly code?: string;
  readonly status?: number;
  readonly detail?: LifecycleErrorDetail;

  constructor(
    message: string,
    code?: string,
    status?: number,
    detail?: LifecycleErrorDetail,
  ) {
    super(message);
    this.name = "EarnApiError";
    this.code = code;
    this.status = status;
    this.detail = detail;
  }
}

const PREPARE_TIMEOUT_MS = 120_000;

export function earnNetworkError(
  message: string,
  detail?: LifecycleErrorDetail,
): EarnApiError {
  return new EarnApiError(message, undefined, undefined, detail);
}

async function throwEarnError(res: Response, fallback: string): Promise<never> {
  const payload = (await res.json().catch(() => null)) as {
    error?: { code?: string; message?: string };
  } | null;
  throw new EarnApiError(
    payload?.error?.message ?? fallback,
    payload?.error?.code,
    res.status,
  );
}

export async function prepareEarnDeposit(args: {
  walletAddress: string;
  amountRaw: string;
  mint: string;
  flowId?: string;
}): Promise<DprepResponse> {
  let res: Response;
  try {
    res = await earnWalletRead(
      args.walletAddress,
      `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/deposits/prepare`,
      {
        body: JSON.stringify({ amountRaw: args.amountRaw, mint: args.mint }),
        flowId: args.flowId,
        method: "POST",
        timeoutMs: PREPARE_TIMEOUT_MS,
      },
    );
  } catch (error) {
    if (error instanceof FetchTimeoutError) {
      throw earnNetworkError(
        "Setting up your Earn account is taking longer than usual. It finishes in the background — try again in a minute.",
        "request_timeout",
      );
    }
    throw error;
  }
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare Earn deposit.");
  }
  return (await res.json()) as DprepResponse;
}

export async function prepareEarnPolicy(
  walletAddress: string,
): Promise<EarnPolicyPrepareResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/policies/prepare`,
    { method: "POST" },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare Earn policy.");
  }
  return (await res.json()) as EarnPolicyPrepareResponse;
}

export async function confirmEarnPolicy(args: {
  walletAddress: string;
  body: EarnPolicyConfirmRequest;
}): Promise<void> {
  const res = await earnWalletRead(
    args.walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/policies/confirm`,
    { body: JSON.stringify(args.body), method: "POST" },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to confirm Earn policy.");
  }
}

export type EarnWithdrawMode = "partial" | "full";

export async function prepareEarnWithdraw(args: {
  walletAddress: string;
  amountRaw: string;
  flowId?: string;
  sourceId: string;
}): Promise<EarnWithdrawPrepareResponse> {
  const res = await earnWalletRead(
    args.walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/withdrawals/prepare`,
    {
      body: JSON.stringify({
        amountRaw: args.amountRaw,
        sourceId: args.sourceId,
      }),
      flowId: args.flowId,
      method: "POST",
    },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare Earn withdrawal.");
  }
  return (await res.json()) as EarnWithdrawPrepareResponse;
}

export async function prepareEarnWithdrawCleanup(args: {
  walletAddress: string;
  minContextSlot?: string;
  flowId?: string;
}): Promise<EarnCleanupPrepareResponse> {
  const body: EarnCleanupPrepareRequest = args.minContextSlot
    ? { minContextSlot: args.minContextSlot }
    : {};
  const res = await earnWalletRead(
    args.walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/withdrawals/cleanup/prepare`,
    {
      body: JSON.stringify(body),
      flowId: args.flowId,
      method: "POST",
    },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare Earn account cleanup.");
  }
  return (await res.json()) as EarnCleanupPrepareResponse;
}

export async function confirmEarnWithdrawCleanup(args: {
  walletAddress: string;
  body: EarnCleanupConfirmRequest;
}): Promise<void> {
  const res = await earnWalletRead(
    args.walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/withdrawals/cleanup/confirm`,
    { body: JSON.stringify(args.body), method: "POST" },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to confirm Earn account cleanup.");
  }
}

export type EarnWithdrawConfirmBody = Omit<
  EarnWithdrawConfirmRequest,
  "reserveWithdrawals"
> & {
  reserveWithdrawals?: SmartAccountEarnUsdcReserveWithdrawalMetadata[];
};

export async function confirmEarnWithdraw(args: {
  walletAddress: string;
  body: EarnWithdrawConfirmBody;
}): Promise<void> {
  const res = await earnWalletRead(
    args.walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/withdrawals/confirm`,
    { body: JSON.stringify(args.body), method: "POST" },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to confirm Earn withdrawal.");
  }
}

export type EarnWithdrawSourceInfo = {
  type: "reserve" | "idle";
  id: string;
  sourceId?: string;
  label: string;
  amountRaw: string;
  liquidityMint: string;
  market: string | null;
  reserve: string | null;
  tokenAccount: string | null;
};

export type EarnAutodepositState = EarnStateAutodeposit & {
  vaultIndex: number;
};

export type EarnAutodepositStateResponse = {
  autodeposit: EarnAutodepositState | null;
  policySigner: string;
  programId: string;
  settingsPda: string;
};

export async function fetchEarnAutodepositState(
  walletAddress: string,
): Promise<EarnAutodepositStateResponse> {
  const state = await fetchEarnState(walletAddress);
  const autodeposit = state.autodeposit as EarnStateAutodeposit | null;
  return {
    autodeposit: autodeposit
      ? { ...autodeposit, vaultIndex: state.vault.accountIndex }
      : null,
    policySigner: state.policySignerPublicKey,
    programId: state.programId,
    settingsPda: state.settingsPda,
  };
}

async function postAutodeposit(
  walletAddress: string,
  path: string,
  body: object,
  fallback: string,
  flowId?: string,
): Promise<Response> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}${path}`,
    { body: JSON.stringify(body), flowId, method: "POST" },
  );
  if (!res.ok) {
    return throwEarnError(res, fallback);
  }
  return res;
}

function requirePersisted(value: string | null, key: string): string {
  if (!value) {
    throw new Error(`prepared autodeposit metadata is missing ${key}.`);
  }
  return value;
}

export async function confirmEarnAutodepositSetup(args: {
  confirmedSlot: string;
  preparedSetup: SmartAccountPreparedEarnUsdcAutodepositSetup;
  setupSignature: string;
  walletBalanceFloorRaw: string;
}): Promise<void> {
  const { persistence, stage } = args.preparedSetup;
  const body: AutodepositSetupConfirmRequest = {
    amountPerPeriodRaw: persistence.amountPerPeriodRaw,
    cluster: persistence.cluster,
    confirmedSlot: args.confirmedSlot,
    delegatedSigner: persistence.delegatedSigner,
    expiryTimestamp: persistence.expiryTimestamp,
    liquidityMint: persistence.liquidityMint,
    nonce: persistence.nonce,
    periodLengthSeconds: persistence.periodLengthSeconds,
    policyAccount: requirePersisted(persistence.policyAccount, "policyAccount"),
    policyId: requirePersisted(persistence.policyId, "policyId"),
    policySeed: requirePersisted(persistence.policySeed, "policySeed"),
    recurringDelegation: persistence.recurringDelegation,
    settings: persistence.settings,
    setupSignature: args.setupSignature,
    setupStage: stage,
    startTimestamp: persistence.startTimestamp,
    subscriptionAuthority: persistence.subscriptionAuthority,
    subscriptionAuthorityInitialization:
      persistence.subscriptionAuthorityInitialization,
    subscriptionDelegatee: persistence.subscriptionDelegatee,
    vaultIndex: persistence.vaultIndex,
    vaultPubkey: persistence.vaultPubkey,
    vaultUsdcAta: persistence.vaultUsdcAta,
    walletAddress: persistence.walletAddress,
    walletBalanceFloorRaw: args.walletBalanceFloorRaw,
    walletUsdcAta: persistence.walletUsdcAta,
  };
  await postAutodeposit(
    persistence.walletAddress,
    "/api/smart-accounts/yield-optimization/autodeposit/setup/confirm",
    body,
    "Failed to confirm Autodeposit setup.",
  );
}

export async function updateEarnAutodepositFloor(args: {
  walletAddress: string;
  policyAccount: string;
  recurringDelegation: string;
  vaultIndex: number;
  walletBalanceFloorRaw: string;
}): Promise<void> {
  const { walletAddress, ...body } = args;
  await postAutodeposit(
    walletAddress,
    "/api/smart-accounts/yield-optimization/autodeposit/floor/confirm",
    body satisfies AutodepositFloorConfirmRequest,
    "Failed to update Autodeposit threshold.",
  );
}

export async function toggleEarnAutodeposit(args: {
  walletAddress: string;
  flowId?: string;
  active: boolean;
  policyAccount: string;
  recurringDelegation: string;
  vaultIndex: number;
}): Promise<void> {
  const { walletAddress, flowId, ...body } = args;
  await postAutodeposit(
    walletAddress,
    "/api/smart-accounts/yield-optimization/autodeposit/toggle/confirm",
    body satisfies AutodepositToggleConfirmRequest,
    "Failed to update Autodeposit on/off state.",
    flowId,
  );
}

export async function confirmEarnAutodepositClose(args: {
  preparedClose: SmartAccountPreparedEarnUsdcAutodepositClose;
  closeSignature: string;
  confirmedSlot: string;
}): Promise<void> {
  const { persistence } = args.preparedClose;
  const body: AutodepositCloseConfirmRequest = {
    ...persistence,
    closeSignature: args.closeSignature,
    confirmedSlot: args.confirmedSlot,
  };
  await postAutodeposit(
    persistence.walletAddress,
    "/api/smart-accounts/yield-optimization/autodeposit/close/confirm",
    body,
    "Failed to confirm Autodeposit removal.",
  );
}

export async function requestEarnAutodepositSweepExecute(
  walletAddress: string,
): Promise<AutodepositSweepsExecuteResponse> {
  const res = await postAutodeposit(
    walletAddress,
    "/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute",
    {} satisfies AutodepositSweepsExecuteRequest,
    "Failed to execute Autodeposit sweep now.",
  );
  return (await res.json()) as AutodepositSweepsExecuteResponse;
}

const EARN_SWEEP_PROGRESS_STATES = new Set<string>(
  EARN_AUTODEPOSIT_PROGRESS_STATES,
);

export type EarnAutodepositSweepProgress = {
  scheduledSlotId: string;
  state: EarnAutodepositProgressState;
  failureCode?: string;
  occurredAt?: string;
};

// Returns null on ANY failure, so callers can quietly fall back to the coarse
// state polling.
export async function fetchEarnAutodepositSweepProgress(
  walletAddress: string,
  slotId: string,
): Promise<EarnAutodepositSweepProgress | null> {
  try {
    const res = await earnWalletRead(
      walletAddress,
      `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/autodeposit/sweeps/execute?slotId=${encodeURIComponent(
        slotId,
      )}`,
    );
    if (!res.ok) {
      return null;
    }
    const value = (await res.json()) as AutodepositSweepsProgressResponse | null;
    if (
      !value ||
      value.scheduledSlotId !== slotId ||
      !EARN_SWEEP_PROGRESS_STATES.has(value.state)
    ) {
      return null;
    }
    return {
      scheduledSlotId: slotId,
      state: value.state as EarnAutodepositProgressState,
      failureCode: value.failureCode,
      occurredAt: value.occurredAt,
    };
  } catch {
    return null;
  }
}

export type { EarnStateResponse };

export async function fetchEarnState(
  walletAddress: string,
): Promise<EarnStateResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/earn-state`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn state.");
  }
  return (await res.json()) as EarnStateResponse;
}

export type EarnEarningsResponse = EarningsRange;

export type EarnEarningsBar = EarningsBar;

// The server buckets bars by calendar day in this zone. Omitting it buckets in
// UTC, which puts the wrong earnings in "today" for anyone far from it — and
// disagrees with web, which sends the browser's zone.
function deviceTimezone(): string | null {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || null;
  } catch {
    return null;
  }
}

export async function fetchEarnEarnings(
  walletAddress: string,
): Promise<EarnEarningsResponse> {
  const timezone = deviceTimezone();
  const query = new URLSearchParams(timezone ? { timezone } : {});
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/earnings?${query}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn earnings.");
  }
  const payload = (await res.json()) as EarningsRangeSet;
  return payload.ranges["30D"];
}

export type { EarnTransactionAccount } from "@loyal-labs/shared";

export type EarnTransactionItem = SerializedEarnTransaction;

export type { EarnTransactionsResponse };

export async function fetchEarnTransactions(
  walletAddress: string,
): Promise<EarnTransactionsResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/earn-transactions`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn transactions.");
  }
  const payload = (await res.json()) as {
    transactions: EarnTransactionItem[] | null;
  };
  return { transactions: payload.transactions ?? [] };
}

export type EarnApySample = EarnForecastApyHistorySample;

export type EarnApySeries = EarnForecastApyHistorySeries;

export type EarnForecastSummary = EarnForecastSummaryResponse;

export async function fetchEarnForecastSummary(): Promise<EarnForecastSummary> {
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/earn-forecast/summary`,
    { method: "GET", headers: earnHeaders() },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn forecast.");
  }
  return (await res.json()) as EarnForecastSummary;
}

export async function confirmEarnDeposit(args: {
  walletAddress: string;
  body: EarnDepositConfirmRequest;
  flowId?: string;
}): Promise<void> {
  const res = await earnWalletRead(
    args.walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/deposits/confirm`,
    { body: JSON.stringify(args.body), flowId: args.flowId, method: "POST" },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to confirm Earn deposit.");
  }
}

export type SolanaWeekQuestKind = "earn_deposit" | "first_autodeposit_sweep";

export async function fetchSolanaWeekQuestProgress(
  walletAddress: string,
): Promise<SolanaWeekQuestProgressResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/solana-week/progress`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load quest progress.");
  }
  return (await res.json()) as SolanaWeekQuestProgressResponse;
}

export async function fetchEarnRefundScan(
  walletAddress: string,
): Promise<EarnPolicyRefundScanResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/policy-refunds/scan`,
    { method: "POST" },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to scan for refunds.");
  }
  return (await res.json()) as EarnPolicyRefundScanResponse;
}

export type EarnRefundPrepareRequest =
  | { kind: "policy"; policyAccount: string }
  | { kind: "recurring_delegation"; recurringDelegation: string }
  | { kind: "vault" };

export async function prepareEarnRefund(args: {
  walletAddress: string;
  request: EarnRefundPrepareRequest;
}): Promise<EarnPolicyRefundPrepareResponse> {
  const res = await earnWalletRead(
    args.walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/yield-optimization/policy-refunds/prepare`,
    { body: JSON.stringify(args.request), method: "POST" },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare the refund.");
  }
  return (await res.json()) as EarnPolicyRefundPrepareResponse;
}
