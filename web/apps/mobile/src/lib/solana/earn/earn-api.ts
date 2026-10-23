import { env } from "@/config/env";
import {
  EARN_AUTODEPOSIT_PROGRESS_STATES,
  type EarnAutodepositProgressState,
  type EarnForecastApyHistorySample,
  type EarnForecastApyHistorySeries,
  type EarnForecastSummaryResponse,
  type EarnTransactionsResponse,
  type LoadedEarnAutodepositScheduledSweep,
  type SerializedEarnTransaction,
} from "@loyal-labs/shared";
import type {
  SmartAccountNativeSolRequirement,
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

function earnAuthParts(auth: EarnAuthFields | EarnSessionAuth): {
  headers: Record<string, string>;
  bodyFields: Record<string, string>;
} {
  return "sessionToken" in auth
    ? {
        bodyFields: {},
        headers: { Authorization: `Bearer ${auth.sessionToken}` },
      }
    : { bodyFields: { ...auth }, headers: {} };
}

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

export type EarnWithdrawSourcesResponse = {
  sources: EarnWithdrawSourceInfo[];
  settingsPda: string | null;
  smartAccountAddress: string | null;
};

export async function fetchEarnWithdrawSources(
  walletAddress: string,
): Promise<EarnWithdrawSourcesResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/withdraw/sources?walletAddress=${encodeURIComponent(
      walletAddress,
    )}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn withdrawal sources.");
  }
  return (await res.json()) as EarnWithdrawSourcesResponse;
}

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

export type EarnAutodepositSetupStage =
  | "initialize_subscription_authority"
  | "approve_token_delegate"
  | "create_policy"
  | "create_recurring_delegation";

export type WirePreparedEarnAutodepositSetup = {
  prepared: WirePreparedOperation;
  stage: EarnAutodepositSetupStage;
  policy: { seed: string | null };
  persistence: { policySeed: string | null };
};

export type WirePreparedEarnAutodepositClose = {
  prepared: WirePreparedOperation;
};

export type EarnAutodepositState = {
  active: boolean;
  status: string;
  policyAccount: string;
  recurringDelegation: string | null;
  walletBalanceFloorRaw: string | null;
  lifecycleStatus: string;
  vaultIndex: number;
  scheduledSweeps?: LoadedEarnAutodepositScheduledSweep[];
  // Resume metadata for the device-side prepare: a half-finished setup must
  // reuse the recorded seed/nonce/window so the SDK returns the missing stage
  // for the SAME policy/delegation pair.
  policySeed: string;
  recurringDelegationNonce: string | null;
  periodLengthSeconds: string | null;
  startTimestamp: string | null;
  recurringDelegationExpiryTimestamp: string | null;
};

// Deployment parameters the device needs to run the SDK's autodeposit prepare
// locally. Null when the deployment isn't configured for it.
export type EarnAutodepositPrepareContext = {
  cluster: string;
  policySigner: string;
  programId: string;
};

export type EarnAutodepositStateResponse = {
  autodeposit: EarnAutodepositState | null;
  prepareContext?: EarnAutodepositPrepareContext | null;
  settingsPda: string | null;
  smartAccountAddress: string | null;
};

export async function fetchEarnAutodepositState(
  walletAddress: string,
): Promise<EarnAutodepositStateResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/autodeposit/state?walletAddress=${encodeURIComponent(
      walletAddress,
    )}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Autodeposit state.");
  }
  return (await res.json()) as EarnAutodepositStateResponse;
}

export async function confirmEarnAutodepositSetup(args: {
  auth: EarnAuthFields;
  preparedSetup: WirePreparedEarnAutodepositSetup;
  setupSignature: string;
  confirmedSlot: string;
  walletBalanceFloorRaw: string;
}): Promise<void> {
  const { auth, ...rest } = args;
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/autodeposit/setup/confirm`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify({ ...auth, ...rest }),
    },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to confirm Autodeposit setup.");
  }
}

export async function updateEarnAutodepositFloor(args: {
  auth: EarnAuthFields | EarnSessionAuth;
  policyAccount: string;
  recurringDelegation: string;
  vaultIndex: number;
  walletBalanceFloorRaw: string;
}): Promise<void> {
  const { auth, ...rest } = args;
  const { headers, bodyFields } = earnAuthParts(auth);
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/autodeposit/floor/confirm`,
    {
      method: "POST",
      headers: { ...earnHeaders(), ...headers },
      body: JSON.stringify({ ...bodyFields, ...rest }),
    },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to update Autodeposit threshold.");
  }
}

export async function toggleEarnAutodeposit(args: {
  auth: EarnAuthFields | EarnSessionAuth;
  flowId?: string;
  active: boolean;
  policyAccount: string;
  recurringDelegation: string;
  vaultIndex: number;
}): Promise<void> {
  const { auth, flowId, ...rest } = args;
  const { headers, bodyFields } = earnAuthParts(auth);
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/autodeposit/toggle/confirm`,
    {
      method: "POST",
      headers: { ...earnHeaders(flowId), ...headers },
      body: JSON.stringify({ ...bodyFields, ...rest }),
    },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to update Autodeposit on/off state.");
  }
}

export async function confirmEarnAutodepositClose(args: {
  auth: EarnAuthFields;
  preparedClose: WirePreparedEarnAutodepositClose;
  closeSignature: string;
  confirmedSlot: string;
}): Promise<void> {
  const { auth, ...rest } = args;
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/autodeposit/close/confirm`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify({ ...auth, ...rest }),
    },
  );
  if (!res.ok) {
    await throwEarnError(res, "Failed to confirm Autodeposit removal.");
  }
}

export type EarnAutodepositSweepExecuteResponse = {
  status: string;
  sweepRequest: {
    acceleratedAmountRaw: string;
    acceleratedLotCount: number;
    eligibleAfter: string;
    slotId: string;
    targetId: string;
  };
  target: {
    active: boolean;
    balanceSweepPolicyId: string | null;
    id: string;
    lifecycleStatus: string;
    policyAccount: string;
    recurringDelegation: string | null;
    walletBalanceFloorRaw: string | null;
  };
};

export async function requestEarnAutodepositSweepExecute(args: {
  auth: EarnAuthFields | EarnSessionAuth;
}): Promise<EarnAutodepositSweepExecuteResponse> {
  const { headers, bodyFields } = earnAuthParts(args.auth);
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/autodeposit/sweeps/execute`,
    {
      method: "POST",
      headers: { ...earnHeaders(), ...headers },
      body: JSON.stringify(bodyFields),
    },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to execute Autodeposit sweep now.");
  }
  return (await res.json()) as EarnAutodepositSweepExecuteResponse;
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

// Read-only progress of one scheduled sweep, keyed by wallet address (no
// signature) — the polled twin of the contract the web receives over SSE.
// Returns null on ANY failure, so callers can quietly fall back to the coarse
// state polling.
export async function fetchEarnAutodepositSweepProgress(
  walletAddress: string,
  slotId: string,
): Promise<EarnAutodepositSweepProgress | null> {
  try {
    const res = await earnWalletRead(
      walletAddress,
      `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/autodeposit/sweeps/execute?walletAddress=${encodeURIComponent(
        walletAddress,
      )}&slotId=${encodeURIComponent(slotId)}`,
    );
    if (!res.ok) {
      return null;
    }
    const value = (await res.json()) as Record<string, unknown> | null;
    if (
      !value ||
      value.scheduledSlotId !== slotId ||
      typeof value.state !== "string" ||
      !EARN_SWEEP_PROGRESS_STATES.has(value.state)
    ) {
      return null;
    }
    return {
      scheduledSlotId: slotId,
      state: value.state as EarnAutodepositProgressState,
      failureCode:
        typeof value.failureCode === "string" ? value.failureCode : undefined,
      occurredAt:
        typeof value.occurredAt === "string" ? value.occurredAt : undefined,
    };
  } catch {
    return null;
  }
}

// Current on-chain Earn position read-model (balance + live APY). All amounts
// are USDC base units (6 decimals) as strings; APY is in basis points.
export type EarnPosition = {
  currentAmountRaw: string;
  currentSupplyApyBps: string | null;
  principalAmountRaw: string;
  status: string;
};

export type EarnStateResponse = {
  position: EarnPosition | null;
  settingsPda: string | null;
  smartAccountAddress: string | null;
};

export async function fetchEarnState(
  walletAddress: string,
): Promise<EarnStateResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/state?walletAddress=${encodeURIComponent(
      walletAddress,
    )}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn state.");
  }
  return (await res.json()) as EarnStateResponse;
}

// Live on-chain Earn holdings snapshot — the same read the web does for its
// headline balance (the vault's Kamino obligations + idle USDC, summed live via
// RPC). Used to override `fetchEarnState`'s `currentAmountRaw`, which reads a DB
// read-model that lags the chain and omits non-idle venue holdings (so the
// native balance showed stale/low values). All amounts are USDC base units
// (6 decimals) as strings. Wallet-keyed, read-only, no signature — like `state`.
export type EarnHoldingItem = {
  kind: "kamino" | "idle";
  label: string;
  amountRaw: string;
  liquidityMint: string;
  market: string | null;
  marketName: string | null;
  reserve: string | null;
};

export type EarnHoldingsResponse = {
  currentTotalAmountRaw: string;
  holdings: EarnHoldingItem[];
  observedAt: string | null;
  observedSlot: string | null;
  settingsPda: string | null;
  smartAccountAddress: string | null;
};

export async function fetchEarnHoldings(
  walletAddress: string,
): Promise<EarnHoldingsResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/holdings?walletAddress=${encodeURIComponent(
      walletAddress,
    )}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn holdings.");
  }
  return (await res.json()) as EarnHoldingsResponse;
}

export type EarnEarningsBar = {
  apyBps: number | null;
  avgPrincipalUsd: number;
  earnedUsd: number;
  endAt: string;
  isCurrent: boolean;
  label: string;
  principalAmountRaw: string;
  principalUsd: number;
  startAt: string;
};

export type EarnEarningsResponse = {
  bars: EarnEarningsBar[];
  currentApyBps: number | null;
  lastDepositAt: string | null;
  lifetimeEarnedUsd: number;
  principalAmountRaw: string;
  principalUsd: number;
  rangeEarnedUsd: number;
  sinceLastDepositEarnedUsd: number;
  todayEarnedUsd: number;
};

// The backend returns every range in one payload (7D/30D/1Y/ALL); the mobile
// chart plots the 30-day daily range.
export type EarnEarningsRangeSetResponse = {
  ranges: Record<"7D" | "30D" | "1Y" | "ALL", EarnEarningsResponse>;
};

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
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/earnings?walletAddress=${encodeURIComponent(
      walletAddress,
    )}${timezone ? `&timezone=${encodeURIComponent(timezone)}` : ""}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn earnings.");
  }
  const payload = (await res.json()) as EarnEarningsRangeSetResponse;
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
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/transactions?walletAddress=${encodeURIComponent(
      walletAddress,
    )}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load Earn transactions.");
  }
  return (await res.json()) as EarnTransactionsResponse;
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

export type SolanaWeekQuestStatus =
  | "reported"
  | "pending"
  | "failed"
  | "not_started";

export type SolanaWeekQuestProgressItem = {
  kind: SolanaWeekQuestKind;
  status: SolanaWeekQuestStatus;
  solanaStatus: string | null;
  reportedAt: string | null;
  attempts: number;
};

export type SolanaWeekQuestProgressResponse = {
  walletAddress: string;
  quests: SolanaWeekQuestProgressItem[];
};

export async function fetchSolanaWeekQuestProgress(
  walletAddress: string,
): Promise<SolanaWeekQuestProgressResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/solana-week/progress?walletAddress=${encodeURIComponent(
      walletAddress,
    )}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to load quest progress.");
  }
  return (await res.json()) as SolanaWeekQuestProgressResponse;
}

export type EarnRefundScanItem = {
  account: string;
  blockedReason: string | null;
  canRefund: boolean;
  lamports: number | null;
};

export type EarnRefundScanVault = EarnRefundScanItem & {
  totalRefundableLamports: number;
};

export type EarnRefundScanResponse = {
  scan: {
    policies: EarnRefundScanItem[];
    recurringDelegations: EarnRefundScanItem[];
    vault: EarnRefundScanVault | null;
  } | null;
};

export async function fetchEarnRefundScan(
  walletAddress: string,
): Promise<EarnRefundScanResponse> {
  const res = await earnWalletRead(
    walletAddress,
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/policy-refunds/scan?walletAddress=${encodeURIComponent(
      walletAddress,
    )}`,
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to scan for refunds.");
  }
  return (await res.json()) as EarnRefundScanResponse;
}

export type EarnRefundPrepareRequest =
  | { kind: "policy"; policyAccount: string }
  | { kind: "recurring_delegation"; recurringDelegation: string }
  | { kind: "vault" };

type WirePreparedEarnRefund = {
  estimatedRefundLamports: number | null;
  prepared: WirePreparedOperation;
};

export type EarnRefundPrepareResponse = {
  preparedRefund?: WirePreparedEarnRefund;
  preparedRecurringDelegationRefund?: WirePreparedEarnRefund;
  preparedVaultRefund?: WirePreparedEarnRefund;
};

export async function prepareEarnRefund(args: {
  auth: EarnAuthFields;
  request: EarnRefundPrepareRequest;
}): Promise<EarnRefundPrepareResponse> {
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/policy-refunds/prepare`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify({ ...args.auth, ...args.request }),
    },
  );
  if (!res.ok) {
    return throwEarnError(res, "Failed to prepare the refund.");
  }
  return (await res.json()) as EarnRefundPrepareResponse;
}
