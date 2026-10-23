import type {
  AutodepositCloseConfirmRequest,
  AutodepositFloorConfirmRequest,
  AutodepositSetupConfirmRequest,
  AutodepositSweepsExecuteRequest,
  AutodepositSweepsExecuteResponse,
  AutodepositSweepsProgressResponse,
  AutodepositToggleConfirmRequest,
  EarnPolicyRefundPrepareResponse,
  EarnPolicyRefundScanResponse,
  EarnStateAutodeposit,
  EarnStateResponse,
  EarningsBar,
  EarningsRange,
  EarningsRangeSet,
  SolanaWeekQuestProgressResponse,
} from "@loyal-labs/api";
import { env } from "@/config/env";
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
  SmartAccountNativeSolRequirement,
  SmartAccountPreparedEarnUsdcAutodepositClose,
  SmartAccountPreparedEarnUsdcAutodepositSetup,
  WirePreparedLoyalSmartAccountsOperation as WirePreparedOperation,
} from "@loyal-labs/smart-account-vaults";
import {
  fetchWithTimeout,
  FetchTimeoutError,
} from "@/lib/network/fetch-with-timeout";
import type { LifecycleErrorDetail } from "@/services/observability";

import { earnHeaders, earnWalletRead } from "./earn-session";

// A first-ever deposit provisions the smart account inline on the server
// (finalized-creation wait plus reservation-conflict retries), which can
// exceed the platform's default request ceiling — iOS aborts at ~60s. Give
// prepare its own generous deadline; provisioning keeps running server-side,
// so a retry after a timeout lands on the fast path.
const PREPARE_TIMEOUT_MS = 120_000;

export type EarnAuthFields = {
  walletAddress: string;
  signature: string;
  issuedAt: string;
};

export type EarnSessionAuth = { sessionToken: string };

// The serialized prepared deposit. Only the fields mobile needs to sign+send are
// typed; the whole object is echoed back to `confirm` opaquely.
export type WirePreparedEarnDeposit = {
  nativeSolRequirement?: SmartAccountNativeSolRequirement;
  prepared: WirePreparedOperation;
  policySetupPrepared?: WirePreparedOperation | null;
  policyFinalizePrepared?: WirePreparedOperation | null;
};

export type EarnDepositPrepareResponse = {
  cluster: string;
  programId: string;
  settingsPda: string;
  smartAccountAddress: string;
  preparedDeposit: WirePreparedEarnDeposit;
};

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

// Everything the device needs to run the SDK's deposit prepare locally
// (client-side instruction building on the device's own RPC) instead of
// calling `deposit/prepare` — mirrors the autodeposit `/state` prepareContext.
// Serialized by `deposit/prepare-context`.
export type EarnDepositPrepareContext = {
  cluster: string;
  programId: string;
  settingsPda: string;
  smartAccountAddress: string;
  policySigner: string;
  revokeStrayUsdcDelegate: boolean;
  yieldRoutingPolicy: {
    account: string;
    seed: string;
    setupPolicy: { account: string; seed: string } | null;
  } | null;
  target: {
    reserve: string;
    market: string;
    liquidityMint: string;
    liquidityTokenProgram: string;
    supplyApyBps: string | null;
  } | null;
};

// Resolves the on-device prepare context (auth + provisioning + DB reads only
// — no instruction building server-side). `mint` selects the Earn product
// stablecoin.
export async function fetchEarnDepositPrepareContext(args: {
  auth: EarnAuthFields;
  amountRaw: string;
  mint: string;
  flowId?: string;
}): Promise<EarnDepositPrepareContext> {
  let res: Response;
  try {
    res = await fetchWithTimeout(
      `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/deposit/prepare-context`,
      {
        method: "POST",
        headers: earnHeaders(args.flowId),
        body: JSON.stringify({
          ...args.auth,
          amountRaw: args.amountRaw,
          mint: args.mint,
        }),
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
  return (await res.json()) as EarnDepositPrepareContext;
}

export type EarnWithdrawMode = "partial" | "full";

export type EarnWithdrawSource = {
  type: "reserve" | "idle";
  id: string;
  amountRaw?: string;
  liquidityMint?: string;
  market?: string | null;
  mint?: string;
  reserve?: string;
  tokenAccount?: string;
} | null;

export type WirePreparedEarnWithdrawStep = {
  prepared: WirePreparedOperation;
};

export type WireEarnWithdrawAutodepositClose = {
  policy: { account: string };
  subscription: { recurringDelegation: string };
};

export type WirePreparedEarnWithdraw = {
  prepared: WirePreparedOperation;
  withdrawSteps?: WirePreparedEarnWithdrawStep[];
  autodepositClosePrepared?: WireEarnWithdrawAutodepositClose | null;
};

export type EarnWithdrawPrepareResponse = {
  cluster: string;
  programId: string;
  settingsPda: string;
  smartAccountAddress: string;
  preparedWithdraw: WirePreparedEarnWithdraw;
};

export async function prepareEarnWithdraw(args: {
  auth: EarnAuthFields;
  amountRaw: string;
  mode: EarnWithdrawMode;
  source?: EarnWithdrawSource;
}): Promise<EarnWithdrawPrepareResponse> {
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/withdraw/prepare`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify({
        ...args.auth,
        amountRaw: args.amountRaw,
        mode: args.mode,
        source: args.source ?? null,
      }),
    },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare Earn withdrawal.");
  }
  return (await res.json()) as EarnWithdrawPrepareResponse;
}

// The resolved SDK input for an ON-DEVICE withdraw prepare, serialized by
// `withdraw/prepare-context` (`earn-withdraw-input-resolution.server.ts`) —
// the server keeps source selection/reconcile; the device builds the
// transactions. Hydrated in `withdraw.ts`.
export type EarnWithdrawPrepareContext = {
  cluster: string;
  programId: string;
  settingsPda: string;
  smartAccountAddress: string;
  withdrawInput: {
    amountRaw: string;
    mode: EarnWithdrawMode;
    closePoliciesOnFullWithdrawal: boolean;
    policySigner: string;
    source:
      | {
          type: "reserve";
          id: string;
          amountRaw: string;
          liquidityMint: string;
          market: string;
          reserve: string;
        }
      | {
          type: "idle";
          id: string;
          amountRaw: string;
          mint: string;
          tokenAccount: string;
          tokenProgramId: string;
        }
      | null;
    target: {
      reserve: string;
      market: string;
      liquidityMint: string;
      liquidityTokenProgram?: string | null;
      supplyApyBps: string | null;
    } | null;
    fullWithdrawalTargets:
      | {
          amountRaw: string | null;
          liquidityMint: string;
          liquidityTokenProgram?: string | null;
          market: string;
          reserve: string;
          reserveCollateralMint: string | null;
          reserveLiquiditySupply: string | null;
          supplyApyBps: string | null;
          vaultCollateralAta: string | null;
        }[]
      | null;
    yieldRoutingPolicy: {
      account: string;
      seed: string;
      setupPolicy: { account: string; seed: string } | null;
    };
    autodepositClose: {
      policy: string;
      recurringDelegation: string;
    } | null;
  };
};

// Resolves the on-device withdraw prepare context (auth + source selection
// only — no instruction building server-side).
export async function fetchEarnWithdrawPrepareContext(args: {
  auth: EarnAuthFields;
  amountRaw: string;
  mode: EarnWithdrawMode;
  source?: EarnWithdrawSource;
  flowId?: string;
}): Promise<EarnWithdrawPrepareContext> {
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/withdraw/prepare-context`,
    {
      method: "POST",
      headers: earnHeaders(args.flowId),
      body: JSON.stringify({
        ...args.auth,
        amountRaw: args.amountRaw,
        mode: args.mode,
        source: args.source ?? null,
      }),
    },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare Earn withdrawal.");
  }
  return (await res.json()) as EarnWithdrawPrepareContext;
}

// Fresh post-withdraw inputs for the device-side cleanup prepare. The backend
// verifies that no Kamino holding remains at or after `minContextSlot` and
// returns the full vault token-account inventory (any product mint, either
// token program); it does not build a transaction.
export type EarnWithdrawCleanupPrepareContext = {
  cluster: string;
  programId: string;
  settingsPda: string;
  cleanupInput: {
    policySigner: string;
    vaultTokenAccounts: {
      address: string;
      amountRaw: string;
      decimals: number;
      mint: string;
      tokenProgramId: string;
    }[];
    yieldRoutingPolicy: {
      account: string;
      seed: string;
      setupPolicy: { account: string; seed: string } | null;
    };
  };
};

export async function fetchEarnWithdrawCleanupPrepareContext(args: {
  auth: EarnAuthFields;
  minContextSlot: string;
  flowId?: string;
}): Promise<EarnWithdrawCleanupPrepareContext> {
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/withdraw/cleanup/prepare-context`,
    {
      method: "POST",
      headers: earnHeaders(args.flowId),
      body: JSON.stringify({
        ...args.auth,
        minContextSlot: args.minContextSlot,
      }),
    },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare Earn account cleanup.");
  }
  return (await res.json()) as EarnWithdrawCleanupPrepareContext;
}

export type EarnWithdrawCleanupConfirmArgs = {
  auth: EarnAuthFields;
  cleanupSignature: string;
  confirmedSlot: string;
};

export async function confirmEarnWithdrawCleanup(
  args: EarnWithdrawCleanupConfirmArgs,
): Promise<void> {
  const { auth, ...rest } = args;
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/withdraw/cleanup/confirm`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify({ ...auth, ...rest }),
    },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to confirm Earn account cleanup.");
  }
}

export type EarnWithdrawConfirmArgs = {
  auth: EarnAuthFields;
  preparedWithdraw: WirePreparedEarnWithdraw;
  stepIndex?: number;
  withdrawalSignature: string;
  confirmedSlot: string;
  autodepositCloseSignature?: string;
  autodepositCloseConfirmedSlot?: string;
};

export async function confirmEarnWithdraw(
  args: EarnWithdrawConfirmArgs,
): Promise<void> {
  const { auth, ...rest } = args;
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/withdraw/confirm`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify({ ...auth, ...rest }),
    },
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

export function toWithdrawPrepareSource(
  info: EarnWithdrawSourceInfo,
): EarnWithdrawSource {
  return {
    type: info.type,
    id: info.id,
    amountRaw: info.amountRaw,
    liquidityMint: info.liquidityMint,
    market: info.market,
    reserve: info.reserve ?? undefined,
    tokenAccount: info.tokenAccount ?? undefined,
    mint: info.type === "idle" ? info.liquidityMint : undefined,
  };
}

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

export type EarnDepositConfirmArgs = {
  auth: EarnAuthFields;
  preparedDeposit: WirePreparedEarnDeposit;
  depositSignature: string;
  confirmedSlot: string;
};

export async function confirmEarnDeposit(
  args: EarnDepositConfirmArgs,
): Promise<void> {
  const { auth, ...rest } = args;
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/deposit/confirm`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify({ ...auth, ...rest }),
    },
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
