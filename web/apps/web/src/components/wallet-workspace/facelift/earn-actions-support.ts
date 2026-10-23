import { EARN_AUTODEPOSIT_DEFAULT_AMOUNT_PER_PERIOD_USDC } from "@loyal-labs/shared";
import type {
  SmartAccountEarnUsdcDepositInput,
  SmartAccountEarnUsdcReserveTargetInput,
  SmartAccountEarnUsdcWithdrawInput,
  SmartAccountPreparedEarnUsdcDeposit,
} from "@loyal-labs/smart-account-vaults";
import {
  AccountLayout,
  getAssociatedTokenAddressSync,
  TOKEN_PROGRAM_ID,
} from "@solana/spl-token";
import { type AccountInfo, type Connection, PublicKey } from "@solana/web3.js";
import { useCallback, useEffect, useRef, useState } from "react";

import {
  type EarnWithdrawDraft,
  type EarnWithdrawSourceOption,
  selectEarnFullExitSources,
} from "@/components/wallet-sidebar/earn-detail-view";
import {
  type EarnExpectedMutationOperation,
  type EarnRealtimeInvalidation,
  resolveEarnRealtimeRefreshPlan,
} from "@/features/earn-realtime";
import { normalizeLifecycleErrorCode } from "@/features/observability/lifecycle-contract";
import {
  resolveSmartAccountMutationRefreshPlan,
  type SmartAccountRefreshPlan,
} from "@/features/smart-accounts/refresh-plan";
import type {
  ActiveEarnPosition,
  ActiveEarnPositionHolding,
} from "@/hooks/use-active-earn-position";
import {
  resolveEarnPositionDisplay,
  resolveEarnTransactionMarketIcon,
} from "@/lib/yield-optimization/earn-position-display";

export type EarnWithdrawVaultsSource = NonNullable<
  SmartAccountEarnUsdcWithdrawInput["source"]
>;
export type EarnWithdrawFullWithdrawalTarget = NonNullable<
  SmartAccountEarnUsdcWithdrawInput["fullWithdrawalTargets"]
>[number];
export type EarnDepositYieldRoutingPolicy = NonNullable<
  SmartAccountEarnUsdcDepositInput["yieldRoutingPolicy"]
>;

export const EARN_SYNC_RESOURCES = {
  earnings: "earn.earnings",
  state: "earn.state",
  position: "earn.position",
  transactions: "earn.transactions",
} as const;
export const EARN_VAULT_ACCOUNT_INDEX = 1;
export const EARN_BALANCE_MUTATION_RESOURCES = [
  EARN_SYNC_RESOURCES.position,
  EARN_SYNC_RESOURCES.transactions,
  EARN_SYNC_RESOURCES.earnings,
] as const;
export const EARN_AUTODEPOSIT_MUTATION_RESOURCES = [
  EARN_SYNC_RESOURCES.state,
  EARN_SYNC_RESOURCES.transactions,
] as const;
export const EARN_CLEANUP_MUTATION_RESOURCES = [
  EARN_SYNC_RESOURCES.state,
  ...EARN_BALANCE_MUTATION_RESOURCES,
] as const;
export const EARN_POLICY_MUTATION_RESOURCES = [
  EARN_SYNC_RESOURCES.state,
] as const;

export const DEFAULT_EARN_AUTODEPOSIT_AMOUNT_LABEL =
  EARN_AUTODEPOSIT_DEFAULT_AMOUNT_PER_PERIOD_USDC.toLocaleString("en-US");

export function resolveEarnMutationSmartAccountPlan(args: {
  operation: EarnExpectedMutationOperation;
  resources: readonly string[];
}): SmartAccountRefreshPlan | null {
  if (
    args.operation === "autodeposit_floor" ||
    args.operation === "autodeposit_toggle"
  ) {
    return null;
  }

  const plan = resolveSmartAccountMutationRefreshPlan({
    kind: "earn",
    operation: args.operation,
    accountIndex: EARN_VAULT_ACCOUNT_INDEX,
  });
  const groups = args.resources.includes(EARN_SYNC_RESOURCES.state)
    ? plan.groups.filter((group) => group !== "earn")
    : plan.groups;

  return groups.length > 0 ? { ...plan, groups } : null;
}

export function resolveEarnRealtimeResources(
  event: EarnRealtimeInvalidation
): string[] {
  const refreshPlan = resolveEarnRealtimeRefreshPlan([event]);
  const resources: string[] = [];
  if (refreshPlan.transactions) {
    resources.push(EARN_SYNC_RESOURCES.transactions);
  }
  if (refreshPlan.earnings) {
    resources.push(EARN_SYNC_RESOURCES.earnings);
  }
  if (refreshPlan.earnState) {
    resources.push(EARN_SYNC_RESOURCES.state);
  }
  if (refreshPlan.position) {
    resources.push(EARN_SYNC_RESOURCES.position);
  }
  return resources;
}

export function parseTokenAmountLabelToRaw(
  amountLabel: string,
  decimals: number
): bigint {
  const normalized = amountLabel.replace(/,/g, "").trim();
  if (!/^\d+(\.\d+)?$/.test(normalized)) {
    throw new Error("Enter a valid deposit amount.");
  }

  const [wholePart, fractionPart = ""] = normalized.split(".");
  const fraction = fractionPart.slice(0, decimals).padEnd(decimals, "0");
  return (
    BigInt(wholePart || "0") * BigInt(10) ** BigInt(decimals) +
    BigInt(fraction || "0")
  );
}

export function parseOptionalUnsignedBigInt(
  value: string | null | undefined
): bigint | undefined {
  return value && /^\d+$/.test(value) ? BigInt(value) : undefined;
}

export function isWalletCancellation(error: unknown): boolean {
  const message =
    error instanceof Error
      ? error.message.toLowerCase()
      : String(error).toLowerCase();
  return ["reject", "denied", "declined", "cancelled", "canceled"].some(
    (marker) => message.includes(marker)
  );
}

export function getEarnWithdrawDraftAmountRaw(
  draft: Pick<
    EarnWithdrawDraft,
    "amountLabel" | "isSourceMax" | "mode" | "source" | "tokenDecimals"
  >
): bigint {
  return draft.isSourceMax || draft.mode === "full"
    ? BigInt(draft.source.amountRaw)
    : parseTokenAmountLabelToRaw(draft.amountLabel, draft.tokenDecimals);
}

function parseEarnWithdrawPublicKey(
  value: string | null | undefined,
  label: string
): PublicKey {
  if (!value) {
    throw new Error(`Selected Earn source is missing ${label}.`);
  }

  try {
    return new PublicKey(value);
  } catch {
    throw new Error(`Selected Earn source has an invalid ${label}.`);
  }
}

function parseEarnWithdrawAmountRaw(value: string, label: string): bigint {
  try {
    const amountRaw = BigInt(value);
    if (amountRaw < BigInt(0)) {
      throw new Error("negative amount");
    }
    return amountRaw;
  } catch {
    throw new Error(`Selected Earn source has an invalid ${label}.`);
  }
}

export function toEarnWithdrawVaultsSource(
  source: EarnWithdrawSourceOption
): EarnWithdrawVaultsSource {
  const amountRaw = parseEarnWithdrawAmountRaw(source.amountRaw, "amount");
  const sourceId =
    source.sourceId ||
    source.reserve ||
    source.tokenAccount ||
    source.liquidityMint;

  if (!sourceId) {
    throw new Error("Selected Earn source is missing an identifier.");
  }

  if (source.type === "idle") {
    return {
      amountRaw,
      id: sourceId,
      mint: parseEarnWithdrawPublicKey(source.liquidityMint, "mint"),
      tokenAccount: parseEarnWithdrawPublicKey(
        source.tokenAccount,
        "token account"
      ),
      tokenProgramId: parseEarnWithdrawPublicKey(
        source.tokenProgramId,
        "token program"
      ),
      type: "idle",
    };
  }

  return {
    amountRaw,
    id: sourceId,
    liquidityMint: parseEarnWithdrawPublicKey(
      source.liquidityMint,
      "liquidity mint"
    ),
    market: parseEarnWithdrawPublicKey(source.market, "Kamino market"),
    reserve: parseEarnWithdrawPublicKey(source.reserve, "Kamino reserve"),
    type: "reserve",
  };
}

export async function parseEarnAutodepositExecuteError(response: Response) {
  const payload = (await response.json().catch(() => null)) as {
    error?: { code?: string; message?: string };
  } | null;

  return {
    code: normalizeLifecycleErrorCode(payload?.error?.code),
    message:
      payload?.error?.message ??
      "Failed to request immediate Autodeposit execution.",
  };
}

export function parseEarnAutodepositExecuteResponse(value: unknown): {
  scheduledSlotId: string;
} {
  const slotId =
    typeof value === "object" &&
    value !== null &&
    typeof (value as { sweepRequest?: { slotId?: unknown } }).sweepRequest
      ?.slotId === "string"
      ? (value as { sweepRequest: { slotId: string } }).sweepRequest.slotId
      : null;
  if (!(slotId && /^[1-9]\d{0,19}$/.test(slotId))) {
    throw new Error(
      "Autodeposit execution returned an invalid scheduled slot."
    );
  }
  return { scheduledSlotId: slotId };
}

export function toEarnWithdrawReserveTarget(
  source: EarnWithdrawSourceOption
): EarnWithdrawFullWithdrawalTarget {
  if (source.type !== "reserve") {
    throw new Error("Selected Earn source is not a Kamino reserve.");
  }

  const vaultCollateralAta = source.tokenAccount
    ? parseEarnWithdrawPublicKey(source.tokenAccount, "collateral account")
    : null;

  return {
    amountRaw: parseEarnWithdrawAmountRaw(source.amountRaw, "amount"),
    liquidityMint: parseEarnWithdrawPublicKey(
      source.liquidityMint,
      "liquidity mint"
    ),
    market: parseEarnWithdrawPublicKey(source.market, "Kamino market"),
    reserve: parseEarnWithdrawPublicKey(source.reserve, "Kamino reserve"),
    ...(source.supplyApyBps
      ? { supplyApyBps: parseEarnWithdrawAmountRaw(source.supplyApyBps, "APY") }
      : {}),
    ...(vaultCollateralAta ? { vaultCollateralAta } : {}),
  };
}

export function selectFullExitWithdrawTargets(draft: EarnWithdrawDraft): {
  fullWithdrawalTargets: EarnWithdrawFullWithdrawalTarget[];
  target: EarnWithdrawFullWithdrawalTarget | null;
} {
  return {
    fullWithdrawalTargets: selectEarnFullExitSources(draft).map(
      toEarnWithdrawReserveTarget
    ),
    target:
      draft.source.type === "reserve"
        ? toEarnWithdrawReserveTarget(draft.source)
        : null,
  };
}

type EarnDepositTargetCandidate = {
  amountRaw: string;
  liquidityMint: string;
  market: string | null;
  reserve: string | null;
  supplyApyBps?: string | null;
};

function parseEarnDepositTargetPublicKey(
  value: string | null | undefined,
  label: string
): PublicKey | null {
  if (!value) {
    return null;
  }

  try {
    return new PublicKey(value);
  } catch {
    throw new Error(`Active Earn position has an invalid ${label}.`);
  }
}

function parseOptionalEarnDepositTargetAmountRaw(
  value: string | null | undefined
): bigint | null {
  if (!value) {
    return null;
  }

  try {
    const amountRaw = BigInt(value);
    return amountRaw > BigInt(0) ? amountRaw : null;
  } catch {
    return null;
  }
}

function toEarnDepositReserveTarget(
  candidate: EarnDepositTargetCandidate
): SmartAccountEarnUsdcReserveTargetInput | null {
  if (!parseOptionalEarnDepositTargetAmountRaw(candidate.amountRaw)) {
    return null;
  }

  const market = parseEarnDepositTargetPublicKey(
    candidate.market,
    "Kamino market"
  );
  const reserve = parseEarnDepositTargetPublicKey(
    candidate.reserve,
    "Kamino reserve"
  );

  if (!(market && reserve)) {
    return null;
  }

  const liquidityMint = parseEarnDepositTargetPublicKey(
    candidate.liquidityMint,
    "liquidity mint"
  );

  if (!liquidityMint) {
    throw new Error("Active Earn position is missing its liquidity mint.");
  }

  return {
    liquidityMint,
    market,
    reserve,
    supplyApyBps: parseOptionalEarnDepositTargetAmountRaw(
      candidate.supplyApyBps
    ),
  };
}

export function resolveActiveEarnDepositTarget(
  position: ActiveEarnPosition | null
): SmartAccountEarnUsdcReserveTargetInput | null {
  if (!position) {
    return null;
  }

  const currentTarget = toEarnDepositReserveTarget({
    amountRaw: position.currentHolding.amountRaw,
    liquidityMint: position.currentHolding.liquidityMint,
    market: position.currentHolding.market,
    reserve: position.currentHolding.reserve,
    supplyApyBps: position.currentSupplyApyBps,
  });
  if (currentTarget) {
    return currentTarget;
  }

  const positiveKaminoHolding = position.holdings?.find(
    (holding) =>
      holding.kind === "kamino" &&
      Boolean(
        parseOptionalEarnDepositTargetAmountRaw(holding.amountRaw) &&
          holding.market &&
          holding.reserve
      )
  );

  return positiveKaminoHolding
    ? toEarnDepositReserveTarget(positiveKaminoHolding)
    : null;
}

function earnHoldingMatchesWithdrawSource(
  holding: ActiveEarnPositionHolding,
  source: EarnWithdrawSourceOption
): boolean {
  if (source.type === "idle") {
    const tokenAccount =
      typeof holding.provenance.tokenAccount === "string"
        ? holding.provenance.tokenAccount
        : null;
    return (
      holding.kind === "idle" &&
      (tokenAccount === source.tokenAccount ||
        holding.liquidityMint === source.liquidityMint)
    );
  }

  return (
    holding.kind === "kamino" &&
    holding.reserve === source.reserve &&
    holding.market === source.market &&
    holding.liquidityMint === source.liquidityMint
  );
}

export function applySubmittedEarnWithdrawToPosition(args: {
  amountRaw: bigint;
  current: ActiveEarnPosition | null;
  draft: EarnWithdrawDraft;
}): ActiveEarnPosition | null {
  const { amountRaw, current, draft } = args;
  if (!current) {
    return current;
  }

  const currentHoldings = current.holdings ?? [];
  if (currentHoldings.length === 0) {
    const currentTotalAmountRaw = BigInt(current.currentTotalAmountRaw);
    const nextCurrentTotal =
      currentTotalAmountRaw > amountRaw
        ? currentTotalAmountRaw - amountRaw
        : BigInt(0);
    const currentPrincipal = BigInt(current.principalAmountRaw);
    const nextPrincipal =
      draft.source.type === "idle"
        ? nextCurrentTotal > BigInt(0)
          ? currentPrincipal
          : BigInt(0)
        : currentPrincipal > amountRaw
        ? currentPrincipal - amountRaw
        : BigInt(0);

    return nextCurrentTotal > BigInt(0)
      ? {
          ...current,
          currentHolding: {
            ...current.currentHolding,
            amountRaw: nextCurrentTotal.toString(),
          },
          currentTotalAmountRaw: nextCurrentTotal.toString(),
          principalAmountRaw: nextPrincipal.toString(),
          status: "active",
        }
      : null;
  }

  let remainingWithdrawalRaw =
    draft.mode === "full"
      ? parseEarnWithdrawAmountRaw(draft.source.amountRaw, "amount")
      : amountRaw;
  const nextHoldings = currentHoldings.flatMap((holding) => {
    if (!earnHoldingMatchesWithdrawSource(holding, draft.source)) {
      return [holding];
    }

    const holdingAmountRaw = BigInt(holding.amountRaw);
    const sourceWithdrawalRaw =
      remainingWithdrawalRaw > holdingAmountRaw
        ? holdingAmountRaw
        : remainingWithdrawalRaw;
    remainingWithdrawalRaw -= sourceWithdrawalRaw;
    const nextHoldingAmountRaw = holdingAmountRaw - sourceWithdrawalRaw;

    return nextHoldingAmountRaw > BigInt(0)
      ? [{ ...holding, amountRaw: nextHoldingAmountRaw.toString() }]
      : [];
  });
  const nextCurrentTotal = nextHoldings.reduce(
    (total, holding) => total + BigInt(holding.amountRaw),
    BigInt(0)
  );
  if (nextCurrentTotal <= BigInt(0)) {
    return null;
  }

  const nextPrimaryHolding =
    nextHoldings.find((holding) => holding.kind === "kamino") ??
    nextHoldings[0];
  const currentPrincipal = BigInt(current.principalAmountRaw);
  const nextPrincipal =
    draft.source.type === "idle"
      ? currentPrincipal
      : currentPrincipal > amountRaw
      ? currentPrincipal - amountRaw
      : BigInt(0);

  return {
    ...current,
    currentHolding: nextPrimaryHolding
      ? {
          amountRaw: nextPrimaryHolding.amountRaw,
          liquidityMint: nextPrimaryHolding.liquidityMint,
          market: nextPrimaryHolding.market,
          observedAt: nextPrimaryHolding.observedAt,
          observedSlot: nextPrimaryHolding.observedSlot,
          provenance: current.currentHolding.provenance,
          reserve: nextPrimaryHolding.reserve ?? "",
        }
      : current.currentHolding,
    currentTotalAmountRaw: nextCurrentTotal.toString(),
    display: nextPrimaryHolding
      ? {
          label: nextPrimaryHolding.label,
          marketName: nextPrimaryHolding.marketName,
          mintSymbol: "USDC",
        }
      : current.display,
    holdings: nextHoldings,
    principalAmountRaw: nextPrincipal.toString(),
    status: "active",
  };
}

function hasEarnPositionObservedConfirmedSlot(
  position: ActiveEarnPosition,
  confirmedSlot: string | undefined
): boolean {
  if (!confirmedSlot) {
    return false;
  }

  try {
    return (
      BigInt(position.currentHolding.observedSlot) >= BigInt(confirmedSlot)
    );
  } catch {
    return false;
  }
}

function upsertPostDepositEarnHolding(args: {
  amountRaw: bigint;
  currentHoldings: ActiveEarnPositionHolding[] | undefined;
  depositedHolding: ActiveEarnPositionHolding;
}): ActiveEarnPositionHolding[] {
  const holdings = args.currentHoldings ?? [];
  const existingIndex = holdings.findIndex(
    (holding) =>
      holding.kind === "kamino" &&
      holding.reserve === args.depositedHolding.reserve
  );

  if (existingIndex === -1) {
    return [...holdings, args.depositedHolding];
  }

  return holdings.map((holding, index) => {
    if (index !== existingIndex) {
      return holding;
    }

    return {
      ...holding,
      amountRaw: (BigInt(holding.amountRaw) + args.amountRaw).toString(),
      observedAt: args.depositedHolding.observedAt,
      observedSlot: args.depositedHolding.observedSlot,
      supplyApyBps: holding.supplyApyBps ?? args.depositedHolding.supplyApyBps,
    };
  });
}

export function buildPostDepositEarnPosition(args: {
  amountRaw: bigint;
  confirmedSlot?: string;
  current: ActiveEarnPosition | null;
  preparedDeposit: SmartAccountPreparedEarnUsdcDeposit;
}): ActiveEarnPosition {
  const current = args.current;
  if (
    current &&
    hasEarnPositionObservedConfirmedSlot(current, args.confirmedSlot)
  ) {
    return current;
  }

  const amountRawString = args.amountRaw.toString();
  const currentTotalAmountRaw = (
    BigInt(current?.currentTotalAmountRaw ?? "0") + args.amountRaw
  ).toString();
  const principalAmountRaw = (
    BigInt(current?.principalAmountRaw ?? "0") + args.amountRaw
  ).toString();
  const liquidityMint =
    args.preparedDeposit.targetReserve.liquidityMint.toBase58();
  const market = args.preparedDeposit.targetReserve.market.toBase58();
  const reserve = args.preparedDeposit.targetReserve.reserve.toBase58();
  const display = resolveEarnPositionDisplay({ liquidityMint, market });
  const nowIso = new Date().toISOString();
  const observedSlot = args.confirmedSlot ?? "0";
  const supplyApyBps =
    args.preparedDeposit.targetReserve.supplyApyBps?.toString() ?? null;
  const depositedHolding: ActiveEarnPositionHolding = {
    amountRaw: amountRawString,
    kind: "kamino",
    label: display.label,
    liquidityMint,
    market,
    marketName: display.marketName,
    observedAt: nowIso,
    observedSlot,
    provenance: {
      source: "earn_deposit_confirmation",
      vaultUsdcAta: args.preparedDeposit.vault.usdcAta.toBase58(),
    },
    reserve,
    sourceId: `reserve:${reserve}`,
    supplyApyBps,
    tokenProgramId:
      args.preparedDeposit.targetReserve.liquidityTokenProgram?.toBase58() ??
      TOKEN_PROGRAM_ID.toBase58(),
  };
  const holdings = upsertPostDepositEarnHolding({
    amountRaw: args.amountRaw,
    currentHoldings: current?.holdings,
    depositedHolding,
  });
  const currentHoldingAmountRaw =
    holdings.find(
      (holding) => holding.kind === "kamino" && holding.reserve === reserve
    )?.amountRaw ?? amountRawString;

  return {
    currentHolding: {
      amountRaw: currentHoldingAmountRaw,
      liquidityMint,
      market,
      observedAt: nowIso,
      observedSlot,
      provenance: {
        lastHoldingEventId: null,
        lastRebalanceDecisionId: null,
      },
      reserve,
    },
    currentSupplyApyBps: current?.currentSupplyApyBps ?? supplyApyBps,
    display,
    initialHolding: current?.initialHolding ?? {
      liquidityMint,
      market,
      reserve,
      supplyApyBps,
    },
    holdings,
    currentTotalAmountRaw,
    principalAmountRaw,
    status: "active",
  };
}

export function getWithdrawSourceKeyForHolding(
  holding: ActiveEarnPositionHolding
): string {
  if ("sourceId" in holding && typeof holding.sourceId === "string") {
    return holding.sourceId;
  }
  const tokenAccount =
    typeof holding.provenance.tokenAccount === "string"
      ? holding.provenance.tokenAccount
      : null;
  const sourceId =
    holding.kind === "idle"
      ? tokenAccount ?? holding.liquidityMint
      : holding.reserve ?? holding.liquidityMint;
  return `${holding.kind}:${sourceId}`;
}

export function createWithdrawSourceOptions(
  holdings: ActiveEarnPositionHolding[] | undefined
): EarnWithdrawSourceOption[] {
  const positiveHoldings =
    holdings?.filter((holding) => {
      try {
        return BigInt(holding.amountRaw) > BigInt(0);
      } catch {
        return false;
      }
    }) ?? [];
  return positiveHoldings.map((holding): EarnWithdrawSourceOption => {
    const tokenAccount =
      typeof holding.provenance.tokenAccount === "string"
        ? holding.provenance.tokenAccount
        : null;
    const sourceId = getWithdrawSourceKeyForHolding(holding);
    return {
      amountRaw: holding.amountRaw,
      balance: Number(BigInt(holding.amountRaw)) / 1_000_000,
      id: sourceId,
      icon: resolveEarnTransactionMarketIcon({ market: holding.market }),
      label:
        holding.kind === "idle"
          ? `Idle vault ${holding.marketName}`
          : `${holding.marketName} reserve`,
      liquidityMint: holding.liquidityMint,
      market: holding.market,
      reserve: holding.reserve,
      sourceId,
      supplyApyBps: holding.supplyApyBps,
      tokenAccount,
      tokenProgramId: holding.tokenProgramId,
      type: holding.kind === "idle" ? "idle" : "reserve",
    };
  });
}

function decodeWalletUsdcAmountRaw(
  account: AccountInfo<Buffer> | null,
  usdcMint: PublicKey,
  owner: PublicKey
): bigint {
  if (!(account && account.owner.equals(TOKEN_PROGRAM_ID))) {
    return BigInt(0);
  }
  const decoded = AccountLayout.decode(account.data);
  if (!(decoded.mint.equals(usdcMint) && decoded.owner.equals(owner))) {
    return BigInt(0);
  }
  return BigInt(decoded.amount.toString());
}

export function useMainAccountUsdcBalance(args: {
  connection: Connection;
  mint: string | null | undefined;
  walletAddress: string | null | undefined;
}): {
  amount: number | null;
  amountRaw: bigint | null;
  refresh: (isCurrent?: () => boolean) => Promise<void>;
} {
  const { connection, mint, walletAddress } = args;
  const [amountRaw, setAmountRaw] = useState<bigint | null>(null);
  const balanceScope = `${walletAddress ?? "no-wallet"}:${mint ?? "no-mint"}`;
  const activeBalanceScopeRef = useRef(balanceScope);
  activeBalanceScopeRef.current = balanceScope;
  const newestSlotRef = useRef({ scope: balanceScope, slot: -1 });

  const commitAmountRaw = useCallback(
    (scope: string, nextAmountRaw: bigint | null, slot: number) => {
      if (activeBalanceScopeRef.current !== scope) {
        return;
      }
      const newest = newestSlotRef.current;
      if (newest.scope === scope && slot < newest.slot) {
        return;
      }
      newestSlotRef.current = { scope, slot };
      setAmountRaw(nextAmountRaw);
    },
    []
  );

  const readAmountRaw = useCallback(async (): Promise<{
    amountRaw: bigint | null;
    slot: number;
  }> => {
    if (!(walletAddress && mint)) {
      return { amountRaw: null, slot: -1 };
    }

    try {
      const owner = new PublicKey(walletAddress);
      const usdcMint = new PublicKey(mint);
      const usdcAta = getAssociatedTokenAddressSync(
        usdcMint,
        owner,
        false,
        TOKEN_PROGRAM_ID
      );

      const { context, value } = await connection.getAccountInfoAndContext(
        usdcAta,
        "confirmed"
      );
      return {
        amountRaw: decodeWalletUsdcAmountRaw(value, usdcMint, owner),
        slot: context.slot,
      };
    } catch (error) {
      console.warn("[earn-deposit] failed to load wallet USDC ATA", error);
      return { amountRaw: null, slot: -1 };
    }
  }, [connection, mint, walletAddress]);

  const refresh = useCallback(
    async (isCurrent: () => boolean = () => true) => {
      if (!isCurrent()) {
        return;
      }
      const requestedScope = balanceScope;
      const next = await readAmountRaw();
      if (isCurrent()) {
        commitAmountRaw(requestedScope, next.amountRaw, next.slot);
      }
    },
    [balanceScope, commitAmountRaw, readAmountRaw]
  );

  useEffect(() => {
    let cancelled = false;

    const catchUp = () => {
      void readAmountRaw().then((next) => {
        if (!cancelled) {
          commitAmountRaw(balanceScope, next.amountRaw, next.slot);
        }
      });
    };
    const catchUpWhenVisible = () => {
      if (document.visibilityState === "visible") {
        catchUp();
      }
    };
    catchUp();
    window.addEventListener("online", catchUp);
    document.addEventListener("visibilitychange", catchUpWhenVisible);

    return () => {
      cancelled = true;
      window.removeEventListener("online", catchUp);
      document.removeEventListener("visibilitychange", catchUpWhenVisible);
    };
  }, [balanceScope, commitAmountRaw, readAmountRaw]);

  useEffect(() => {
    if (!(walletAddress && mint)) {
      return;
    }
    let owner: PublicKey;
    let usdcMint: PublicKey;
    try {
      owner = new PublicKey(walletAddress);
      usdcMint = new PublicKey(mint);
    } catch {
      return;
    }
    const subscribedScope = balanceScope;
    const subscriptionId = connection.onAccountChange(
      getAssociatedTokenAddressSync(usdcMint, owner, false, TOKEN_PROGRAM_ID),
      (account, context) => {
        try {
          commitAmountRaw(
            subscribedScope,
            decodeWalletUsdcAmountRaw(account, usdcMint, owner),
            context.slot
          );
        } catch (error) {
          console.warn(
            "[earn-deposit] failed to decode wallet USDC ATA",
            error
          );
        }
      },
      "confirmed"
    );
    return () => {
      void connection.removeAccountChangeListener(subscriptionId);
    };
  }, [balanceScope, commitAmountRaw, connection, mint, walletAddress]);

  return {
    amount: amountRaw === null ? null : Number(amountRaw) / 1_000_000,
    amountRaw,
    refresh,
  };
}
