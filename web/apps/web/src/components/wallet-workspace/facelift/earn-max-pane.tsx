"use client";

import {
  earnMaxDailySeries,
  earnMaxPending,
  earnMaxPendingMessage,
  earnMaxPendingSubtitle,
  earnMaxWithdrawalPending,
  formatEarnMaxApyLabel,
} from "@loyal-labs/shared";
import {
  CircleCheck,
  Clock3,
  Infinity as InfinityIcon,
} from "lucide-react";
import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import {
  HistoricalApyChart,
  type HistoricalApySample,
} from "@/components/wallet-sidebar/earn-detail-view";
import {
  ScrambledPopDigits,
  useBalanceVisibility,
} from "@/components/wallet-workspace/facelift/balance-visibility";
import { readCssDurationMs } from "@/components/wallet-workspace/facelift/css-duration";
import {
  type ChartCardCustomContent,
  type ChartTab,
  EarnChartCard,
  EarnChartPane,
} from "@/components/wallet-workspace/facelift/earn-chart-pane";
import {
  EarnMaxDepositPane,
  EarnMaxDualIcon,
  EarnMaxInfoFaqsCard,
  EarnMaxWithdrawPane,
} from "@/components/wallet-workspace/facelift/earn-max-action-panes";
import { EarnEmptyPane } from "@/components/wallet-workspace/facelift/earn-empty-pane";
import {
  EarnMaxTransactionDetailPane,
  earnMaxActivityLabel,
  earnMaxRequestedNote,
  formatEarnMaxUsdcAmount,
  isEarnMaxWithdrawishAction,
  OperationRow,
} from "@/components/wallet-workspace/facelift/earn-max-transaction-detail";
import {
  EarnPositionRow,
  GroupHeader,
} from "@/components/wallet-workspace/facelift/earn-activity-card";
import {
  EarnedBarsChart,
  type EarnedChartBar,
} from "@/components/wallet-workspace/facelift/earned-chart";
import { InfoTooltip } from "@/components/wallet-workspace/facelift/info-tooltip";
import { PendingWithdrawalTooltip } from "@/components/wallet-workspace/facelift/pending-withdrawal-tooltip";
import { useEscapePage } from "@/components/wallet-workspace/facelift/escape-owner";
import { useLastValue } from "@/components/wallet-workspace/facelift/use-last-value";
import {
  MiddlePaneSlide,
  PaneReveal,
} from "@/components/wallet-workspace/facelift/pane-transitions";
import { SheetReveal } from "@/components/wallet-workspace/facelift/sheet-reveal";
import { SkeletonReveal } from "@/components/wallet-workspace/facelift/skeleton-reveal";
import {
  StaggerLine,
  StaggerReveal,
} from "@/components/wallet-workspace/facelift/stagger-reveal";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";
import { useUnderlineTabs } from "@/components/wallet-workspace/facelift/use-underline-tabs";
import type { EarnPositionData } from "@/components/wallet-workspace/facelift/use-earn-position-data";
import {
  EARN_MAX_STRATEGY_NAME,
  type EarnMaxActions,
  type EarnMaxActivityItem,
  type EarnMaxViewModel,
  useEarnMaxApy,
} from "@/features/earn-max";
import type { EarnMaxApy } from "@/features/earn-max/use-earn-max-apy";
import { splitUsdBalance } from "@/hooks/use-wallet-desktop-data";
import { useAuthCapability } from "@/lib/auth/capability";

const ASSET_BASE = "/wallet-workspace/facelift";


function usdcRawLabel(amountRaw: string): string {
  return `${formatEarnMaxUsdcAmount(amountRaw)} USDC`;
}

function GrayInfinityIcon() {
  return (
    <span className="mr-3 flex size-11 shrink-0 items-center justify-center rounded-[11px] bg-accent">
      <InfinityIcon aria-hidden="true" className="size-6 text-foreground" />
    </span>
  );
}

function ApyBadge({
  label,
  muted = false,
}: {
  label: string;
  muted?: boolean;
}) {
  return (
    <span
      className={`inline-flex items-center rounded-md px-1 py-px ${
        muted ? "bg-accent" : "bg-positive/[0.14]"
      }`}
    >
      <span
        className={`whitespace-nowrap pt-px font-medium text-[11px] leading-[13px] tracking-[0.06px] ${
          muted ? "text-muted-foreground" : "text-positive"
        }`}
      >
        {label}
      </span>
    </span>
  );
}

const EARN_MAX_CHART_TABS: readonly ChartTab[] = ["APY", "Earned"];

// ponytail: daily "earned" ≈ equity delta minus confirmed same-day flows
// (deposits in, withdrawal requests out) — replace with a dedicated earnings
// feed when the indexer grows one.
// ponytail: mock tooltip copy — real copy comes with the wiring pass
const EARN_MAX_TOOLTIP_TEXT =
  "Higher-yield strategies through your own smart account";

export function isEarnMaxUntouched(view: EarnMaxViewModel): boolean {
  return (
    !view.isLoading &&
    view.balanceUsd <= 0 &&
    view.activity.length === 0 &&
    !view.withdrawal
  );
}

export function buildEarnMaxEarnedBars(view: EarnMaxViewModel): EarnedChartBar[] {
  return earnMaxDailySeries(view).days.map((day) => ({
    apyBps: null,
    earnedUsd: day.earnedUsd,
    endAt: day.date.toISOString(),
    isCurrent: day.isCurrent,
    label: day.date.toLocaleDateString("en-US", {
      day: "numeric",
      month: "short",
      timeZone: "UTC",
    }),
    startAt: day.date.toISOString(),
  }));
}

// Earn MAX rides Earn's animated HistoricalApyChart on the vault's own
// trailing 7-day realized APY series from the public read, one point per
// sample as served. Fewer than two points render the chart's empty state;
// an undefined sample list would plot Earn's own history under the MAX label.
function buildEarnMaxApySamples(
  apy: EarnMaxApy | null
): HistoricalApySample[] {
  const samples = (apy?.history ?? []).map((point) => ({
    apyPercent: point.apyBps / 100,
    observedAtMs: Date.parse(point.observedAt),
  }));
  return samples.length >= 2 ? samples : [];
}

function SmallPill({
  disabled = false,
  label,
  onClick,
  variant,
}: {
  disabled?: boolean;
  label: string;
  onClick: () => void;
  variant: "dark" | "light";
}) {
  return (
    <button
      className={`t-hover min-w-16 rounded-full px-4 py-2.5 text-center font-medium text-[13px] leading-4 disabled:opacity-40 ${
        variant === "dark"
          ? "bg-foreground text-background enabled:hover:bg-foreground/90"
          : "bg-accent text-foreground enabled:hover:bg-accent-active"
      }`}
      disabled={disabled}
      onClick={onClick}
      type="button"
    >
      {label}
    </button>
  );
}

function GroupHeaderWithIcon({
  icon,
  label,
}: {
  icon: "check" | "clock";
  label: string;
}) {
  const Icon = icon === "check" ? CircleCheck : Clock3;
  return (
    <div className="flex w-full items-center gap-1 px-4 pt-4 pb-2">
      <Icon aria-hidden="true" className="size-5 text-tertiary" />
      <p className="min-w-0 flex-1 text-[16px] text-muted-foreground leading-5 tracking-[-0.176px]">
        {label}
      </p>
    </div>
  );
}

const TRANSACTIONS_LIMIT = 5;

function minutesLeftLabel(readyBy: string): string {
  const msLeft = new Date(readyBy).getTime() - Date.now();
  if (!Number.isFinite(msLeft) || msLeft <= 0) {
    return "Finishing up";
  }
  return `~${Math.max(Math.ceil(msLeft / 60_000), 1)} min left`;
}

function EarnMaxActivityCard({
  actions,
  onSelectTransaction,
  onViewAllActivity,
  onWithdraw,
  selectedTransactionId,
  view,
}: {
  actions: EarnMaxActions;
  onSelectTransaction: (item: EarnMaxActivityItem) => void;
  onViewAllActivity: () => void;
  onWithdraw: () => void;
  selectedTransactionId: string | null;
  view: EarnMaxViewModel;
}) {
  const { isBalanceHidden } = useBalanceVisibility();
  const [tab, setTab] = useState<"Positions" | "Transactions">(
    "Transactions"
  );

  const tabContentRef = useRef<HTMLDivElement>(null);
  const outgoingHeightRef = useRef<number | null>(null);
  const resizeTimerRef = useRef<number | null>(null);
  const selectTab = (next: "Positions" | "Transactions") => {
    if (next !== tab) {
      outgoingHeightRef.current = tabContentRef.current?.offsetHeight ?? null;
      setTab(next);
    }
  };
  useLayoutEffect(() => {
    const el = tabContentRef.current;
    const fromHeight = outgoingHeightRef.current;
    outgoingHeightRef.current = null;
    if (!el || fromHeight === null) {
      return;
    }
    el.style.height = "";
    const toHeight = el.offsetHeight;
    if (fromHeight === toHeight) {
      return;
    }
    if (resizeTimerRef.current !== null) {
      window.clearTimeout(resizeTimerRef.current);
    }
    el.style.height = `${fromHeight}px`;
    void el.offsetHeight;
    el.style.height = `${toHeight}px`;
    const dur = readCssDurationMs("--resize-dur", 300);
    resizeTimerRef.current = window.setTimeout(() => {
      resizeTimerRef.current = null;
      el.style.height = "";
    }, dur);
  }, [tab]);
  useEffect(
    () => () => {
      if (resizeTimerRef.current !== null) {
        window.clearTimeout(resizeTimerRef.current);
      }
    },
    []
  );
  const { tabBarRef, underlineRef } = useUnderlineTabs(tab);

  const withdrawal = view.withdrawal;
  const pending = earnMaxPending(withdrawal);
  const needsAttention = pending === "operator_attention";
  const groups: { items: EarnMaxActivityItem[]; label: string }[] = [];
  for (const item of view.activity.slice(0, TRANSACTIONS_LIMIT)) {
    const label = item.timestamp
      ? new Date(item.timestamp).toLocaleDateString("en-US", {
          day: "numeric",
          month: "short",
        })
      : "Confirming";
    const group = groups.at(-1);
    if (group && group.label === label) {
      group.items.push(item);
    } else {
      groups.push({ items: [item], label });
    }
  }
  const balance = splitUsdBalance(view.balanceUsd);
  return (
    <div className="flex w-full shrink-0 flex-col overflow-clip rounded-3xl bg-card max-[795px]:rounded-none">
      <div
        className="relative flex w-full items-center px-2 pt-2"
        ref={tabBarRef}
        role="tablist"
      >
        <span
          aria-hidden="true"
          className="t-tabs-underline"
          ref={underlineRef}
        />
        {(["Transactions", "Positions"] as const).map((key) => {
          const isActive = tab === key;
          return (
            <button
              aria-selected={isActive}
              className={`t-hover relative flex h-11 items-center justify-center px-4 font-medium text-[16px] leading-5 tracking-[-0.176px] ${
                isActive
                  ? "text-foreground"
                  : "rounded-3xl text-muted-foreground hover:bg-accent hover:text-foreground"
              }`}
              key={key}
              onClick={() => selectTab(key)}
              role="tab"
              type="button"
            >
              {key}
            </button>
          );
        })}
      </div>
      <div className="t-resize w-full overflow-clip" ref={tabContentRef}>
      <div className="flex w-full flex-col px-2 pb-2">
        {tab === "Positions" ? (
          view.balanceUsd > 0 ? (
            <StaggerReveal className="flex w-full flex-col">
              <StaggerLine index={0}>
                <EarnPositionRow
                  action={
                    <PendingWithdrawalTooltip pending={earnMaxWithdrawalPending(withdrawal)}>
                      <SmallPill
                        disabled={!view.canWithdraw}
                        label="Withdraw"
                        onClick={onWithdraw}
                        variant="light"
                      />
                    </PendingWithdrawalTooltip>
                  }
                  actionWrapClassName="pointer-events-none flex pl-3 opacity-0 transition-opacity duration-150 group-hover:pointer-events-auto group-hover:opacity-100 group-hover:delay-100 group-focus-within:pointer-events-auto group-focus-within:opacity-100"
                  amountFraction={balance.balanceFraction}
                  amountWhole={balance.balanceWhole}
                  icon={<EarnMaxDualIcon />}
                  isHidden={isBalanceHidden}
                  label={`${EARN_MAX_STRATEGY_NAME} USDC`}
                />
              </StaggerLine>
            </StaggerReveal>
          ) : (
            <p className="px-4 py-3 text-[13px] text-muted-foreground leading-4">
              No positions.
            </p>
          )
        ) : view.isLoading && groups.length === 0 && !withdrawal ? (
          <div className="t-skel-rows flex flex-col gap-2 px-4 py-3">
            {[0, 1, 2].map((index) => (
              <div className="h-[60px] w-full rounded-2xl bg-accent" key={index} />
            ))}
          </div>
        ) : (
          <StaggerReveal className="flex w-full flex-col">
            {(() => {
              let lineIndex = 0;
              return (
                <>
                  {withdrawal?.canClaim ? (
                    <StaggerLine index={lineIndex++}>
                      <GroupHeaderWithIcon icon="check" label="Ready to claim" />
                      <div className="flex w-full flex-col rounded-2xl">
                        <OperationRow
                          amountLabel={usdcRawLabel(withdrawal.amountRaw)}
                          isWithdraw
                          subtitle="Ready"
                          title="Withdraw"
                        />
                        <div className="flex w-full items-start gap-2 px-4 pt-1 pb-2">
                          <SmallPill
                            disabled={view.isBusy}
                            label="Claim withdrawal"
                            onClick={() => void actions.claim()}
                            variant="dark"
                          />
                          {withdrawal.canCancel ? (
                            <SmallPill
                              disabled={view.isBusy}
                              label="Cancel"
                              onClick={() => void actions.cancelWithdrawal()}
                              variant="light"
                            />
                          ) : null}
                        </div>
                      </div>
                    </StaggerLine>
                  ) : null}
                  {withdrawal &&
                  !withdrawal.canClaim &&
                  withdrawal.status !== "claimed" ? (
                    <StaggerLine index={lineIndex++}>
                      <GroupHeaderWithIcon icon="clock" label={needsAttention ? "Withdrawal delayed" : "Pending"} />
                      <div className="flex w-full flex-col rounded-2xl">
                        <OperationRow
                          amountLabel={usdcRawLabel(withdrawal.amountRaw)}
                          isWithdraw
                          subtitle={
                            pending
                              ? earnMaxPendingSubtitle(pending)
                              : minutesLeftLabel(withdrawal.readyBy)
                          }
                          title="Withdraw"
                        />
                        {pending ? (
                          <div className="flex flex-col items-start gap-2 px-4 pt-1 pb-2">
                            <p className="text-[13px] text-muted-foreground leading-4">
                              {earnMaxPendingMessage(pending)}
                            </p>
                            <SmallPill
                              disabled={view.isBusy}
                              label="Check status"
                              onClick={() => void actions.refresh()}
                              variant="light"
                            />
                          </div>
                        ) : null}
                        {withdrawal.canCancel ? (
                          <div className="flex w-full items-start gap-2 px-4 pt-1 pb-2">
                            <SmallPill
                              disabled={view.isBusy}
                              label="Cancel"
                              onClick={() => void actions.cancelWithdrawal()}
                              variant="light"
                            />
                          </div>
                        ) : null}
                      </div>
                    </StaggerLine>
                  ) : null}
                  {groups.length === 0 && !withdrawal ? (
                    <p className="px-4 py-3 text-[13px] text-muted-foreground leading-4">
                      No transactions yet.
                    </p>
                  ) : (
                    <>
                      {groups.map((group) => (
                        <div className="flex w-full flex-col" key={group.label}>
                          <StaggerLine index={lineIndex++}>
                            <GroupHeader label={group.label} />
                          </StaggerLine>
                          {group.items.map((item) => (
                            <StaggerLine index={lineIndex++} key={item.id}>
                              <OperationRow
                                amountLabel={
                                  item.amountRaw
                                    ? usdcRawLabel(item.amountRaw)
                                    : null
                                }
                                isSelected={selectedTransactionId === item.id}
                                isWithdraw={isEarnMaxWithdrawishAction(
                                  item.action
                                )}
                                onSelect={() => onSelectTransaction(item)}
                                subtitle={[
                                  item.timestamp
                                    ? new Date(
                                        item.timestamp
                                      ).toLocaleTimeString([], {
                                        hour: "2-digit",
                                        minute: "2-digit",
                                      })
                                    : "Confirming",
                                  earnMaxRequestedNote(item),
                                ]
                                  .filter(Boolean)
                                  .join(" · ")}
                                title={earnMaxActivityLabel(item)}
                              />
                            </StaggerLine>
                          ))}
                        </div>
                      ))}
                      <StaggerLine index={lineIndex}>
                        <button
                          className="t-hover flex h-11 w-full items-center justify-center rounded-2xl font-medium text-[14px] text-foreground leading-5 hover:bg-accent"
                          onClick={onViewAllActivity}
                          type="button"
                        >
                          View all activity
                        </button>
                      </StaggerLine>
                    </>
                  )}
                </>
              );
            })()}
          </StaggerReveal>
        )}
      </div>
      </div>
    </div>
  );
}

function EarnMaxMainPane({
  actions,
  chartCustom,
  earnData,
  onBack,
  onDeposit,
  onOpenChart,
  onSelectChartTab,
  onSelectTransaction,
  onViewAllActivity,
  onWithdraw,
  selectedChartTab,
  selectedTransactionId,
  view,
}: {
  actions: EarnMaxActions;
  chartCustom: ChartCardCustomContent;
  earnData: EarnPositionData;
  onBack: () => void;
  onDeposit: () => void;
  onOpenChart: () => void;
  onSelectChartTab: (tab: ChartTab) => void;
  onSelectTransaction: (item: EarnMaxActivityItem) => void;
  onViewAllActivity: () => void;
  onWithdraw: () => void;
  selectedChartTab: ChartTab | null;
  selectedTransactionId: string | null;
  view: EarnMaxViewModel;
}) {
  const { isBalanceHidden } = useBalanceVisibility();
  const balance = splitUsdBalance(view.balanceUsd);
  const earnedCents =
    view.earnedUsd === null ? null : Math.round(view.earnedUsd * 100);
  const earnedLabel =
    earnedCents === null
      ? null
      : `${earnedCents < 0 ? "-" : "+"}$${(
          Math.abs(earnedCents) / 100
        ).toLocaleString("en-US", {
          maximumFractionDigits: 2,
          minimumFractionDigits: 2,
        })}`;
  const hasPendingWithdrawal = earnMaxWithdrawalPending(view.withdrawal);
  const canWithdraw = view.canWithdraw;
  return (
    <section className="flex h-full min-w-0 flex-1 flex-col">
      <div className="flex min-h-0 w-full flex-1 flex-col gap-2 overflow-y-auto">
      <div className="flex w-full shrink-0 flex-col rounded-3xl bg-card max-[795px]:rounded-t-none">
        <header className="flex w-full items-center p-2">
          <button
            aria-label="Back"
            className="t-hover hidden size-11 shrink-0 items-center justify-center rounded-3xl hover:bg-accent max-[795px]:flex"
            onClick={onBack}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${ASSET_BASE}/icon-arrow-left.svg`}
            />
          </button>
          <div className="flex min-w-0 flex-1 items-center gap-2 py-2 pl-4 max-[795px]:pl-1">
            <h1 className="whitespace-nowrap font-semibold text-[24px] text-foreground leading-7">
              Earn MAX
            </h1>
            <span className="max-[795px]:hidden">
              <InfoTooltip
                iconClassName="size-6"
                placement="bottom"
                text={EARN_MAX_TOOLTIP_TEXT}
              />
            </span>
          </div>
          <button
            aria-label="Expand chart"
            className="t-hover hidden size-11 shrink-0 items-center justify-center rounded-3xl hover:bg-accent max-[795px]:flex"
            onClick={onOpenChart}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${ASSET_BASE}/icon-chart.svg`}
            />
          </button>
          <span className="hidden size-11 shrink-0 items-center justify-center max-[795px]:flex">
            <InfoTooltip
              iconClassName="size-6"
              placement="bottom"
              text={EARN_MAX_TOOLTIP_TEXT}
            />
          </span>
          <div className="flex shrink-0 items-start gap-2 pl-3 max-[795px]:hidden">
            <PendingWithdrawalTooltip pending={hasPendingWithdrawal}>
<button
              className="t-hover flex items-center justify-center gap-2 rounded-full bg-accent p-2.5 enabled:hover:-translate-y-0.5 enabled:hover:bg-accent-active enabled:active:translate-y-0 disabled:opacity-40"
              disabled={!canWithdraw}
              onClick={onWithdraw}
              type="button"
            >
              <ThemedIcon
                className="size-6 text-muted-foreground"
                src={`${ASSET_BASE}/icon-withdraw-arrow.svg`}
              />
              <span className="whitespace-nowrap pr-2.5 font-medium text-[16px] text-foreground leading-5">
                Withdraw
              </span>
            </button>
</PendingWithdrawalTooltip>
            <button
              className="t-hover flex items-center justify-center gap-2 rounded-full bg-foreground p-2.5 hover:-translate-y-0.5 hover:bg-foreground/90 active:translate-y-0"
              onClick={onDeposit}
              type="button"
            >
              <ThemedIcon
                className="size-6 text-background"
                src={`${ASSET_BASE}/icon-plus.svg`}
              />
              <span className="whitespace-nowrap pr-2.5 font-medium text-[16px] text-background leading-5">
                Deposit
              </span>
            </button>
          </div>
        </header>
        <div className="flex w-full flex-col p-2 pt-0">
          <div className="flex w-full flex-col gap-0.5 px-4 py-2">
            <span className="flex items-center gap-1">
              <span className="whitespace-nowrap text-[16px] text-muted-foreground leading-5">
                Balance
              </span>
              <InfoTooltip text="Current equity of your Earn MAX position" />
            </span>
            <p className="whitespace-nowrap font-semibold text-[40px] text-foreground leading-[48px] tracking-[-0.44px]">
              <SkeletonReveal
                isRevealed={!view.isLoading}
                skeletonClassName="rounded-lg bg-accent-selected"
              >
                {view.isLoading ? (
                  `${balance.balanceWhole}${balance.balanceFraction}`
                ) : (
                  <ScrambledPopDigits
                    isHidden={isBalanceHidden}
                    segments={[
                      { text: balance.balanceWhole },
                      {
                        color: "var(--tertiary)",
                        text: balance.balanceFraction,
                      },
                    ]}
                  />
                )}
              </SkeletonReveal>
            </p>
          </div>
        </div>
        <div className="flex w-full flex-col p-2 pt-0">
          <div className="group flex w-full items-center rounded-2xl px-4 transition-colors duration-150 hover:bg-accent">
            <span className="flex items-center py-2">
              <GrayInfinityIcon />
            </span>
            <span className="flex min-w-0 flex-1 items-center gap-1 py-2">
              <span className="whitespace-nowrap font-medium text-[16px] text-foreground leading-5 tracking-[-0.176px]">
                {EARN_MAX_STRATEGY_NAME}
              </span>
              {view.currentApyBps !== null ? (
                <>
                  <ApyBadge label={formatEarnMaxApyLabel(view.currentApyBps)} />
                  {view.forecastApyBps !== null ? (
                    <ApyBadge
                      label={`${(view.forecastApyBps / 100).toFixed(2)}% 7d`}
                      muted
                    />
                  ) : null}
                </>
              ) : (
                <ApyBadge label={formatEarnMaxApyLabel(view.forecastApyBps)} />
              )}
              <InfoTooltip
                placement="bottom"
                text={`Earn steady, real-world yield by looping between tokenized real-world assets (RWAs) and stablecoins. Designed for consistent returns with reduced volatility.${
                  view.currentApyBps !== null
                    ? " Green: net APY of the position right now. Gray: what the vault actually earned over the last 7 days, annualized, after all costs."
                    : ""
                }`}
              />
            </span>
            <div className="relative flex shrink-0 items-center justify-end pl-3">
              <div className="group-hover:pointer-events-none flex flex-col items-end justify-center gap-0.5 py-[11px] transition-opacity duration-150 group-hover:opacity-0">
                <span className="whitespace-nowrap text-right font-medium text-[16px] text-foreground leading-5">
                  <ScrambledPopDigits
                    isHidden={isBalanceHidden}
                    segments={[
                      { text: balance.balanceWhole },
                      {
                        color: "var(--tertiary)",
                        text: balance.balanceFraction,
                      },
                    ]}
                  />
                </span>
                {earnedLabel ? (
                  <span
                    className={`whitespace-nowrap text-[13px] leading-4 ${
                      earnedCents !== null && earnedCents < 0
                        ? "text-destructive"
                        : "text-positive"
                    }`}
                  >
                    {earnedLabel}
                  </span>
                ) : null}
              </div>
              <div className="pointer-events-none absolute right-0 flex items-center gap-2 rounded-[40px] bg-secondary opacity-0 transition-opacity duration-150 group-hover:pointer-events-auto group-hover:opacity-100 group-hover:delay-100 group-focus-within:pointer-events-auto group-focus-within:opacity-100">
                <PendingWithdrawalTooltip pending={hasPendingWithdrawal}>
                  <SmallPill
                    disabled={!canWithdraw}
                    label="Withdraw"
                    onClick={onWithdraw}
                    variant="light"
                  />
                </PendingWithdrawalTooltip>
                <SmallPill label="Deposit" onClick={onDeposit} variant="dark" />
              </div>
            </div>
          </div>
          <div className="flex w-full items-center rounded-2xl px-4 opacity-40">
            <span className="flex items-center py-2">
              <span className="mr-3 flex size-11 shrink-0 items-center justify-center rounded-[11px] bg-accent">
                <ThemedIcon
                  className="size-6 text-foreground"
                  src={`${ASSET_BASE}/icon-clock.svg`}
                />
              </span>
            </span>
            <span className="flex min-w-0 flex-1 flex-col gap-0.5 py-[11px]">
              <span className="whitespace-nowrap font-medium text-[16px] text-foreground leading-5 tracking-[-0.176px]">
                Delta neutral
              </span>
              <span className="whitespace-nowrap text-[13px] text-foreground leading-4">
                Coming soon
              </span>
            </span>
          </div>
          {view.error ? (
            <p className="px-4 py-2 text-[13px] text-destructive leading-4">
              {view.error}
            </p>
          ) : null}
          {view.balanceUsd === 0 && view.status === "claimed" ? (
            <button
              className="t-hover self-start rounded-full px-4 py-2 font-medium text-[14px] text-muted-foreground hover:bg-accent hover:text-foreground"
              disabled={view.isBusy}
              onClick={() => void actions.close()}
              type="button"
            >
              Close Earn MAX accounts &amp; reclaim rent
            </button>
          ) : null}
        </div>
      </div>
      <EarnChartCard
        actionAriaLabel="Expand chart"
        actionIconSrc={`${ASSET_BASE}/icon-expand.svg`}
        custom={chartCustom}
        earnData={earnData}
        onAction={onOpenChart}
        onSelectTab={onSelectChartTab}
        sectionClassName="hidden h-[406px] w-full shrink-0 flex-col overflow-clip rounded-3xl bg-card max-[795px]:flex"
        selectedTab={selectedChartTab}
      />
      <EarnMaxActivityCard
        actions={actions}
        onSelectTransaction={onSelectTransaction}
        onViewAllActivity={onViewAllActivity}
        onWithdraw={onWithdraw}
        selectedTransactionId={selectedTransactionId}
        view={view}
      />
      </div>

      <div className="hidden w-full shrink-0 bg-card px-4 py-2 max-[795px]:block">
        <div className="flex w-full gap-2">
          <button
            className="t-hover flex h-12 min-w-0 flex-1 items-center justify-center gap-2 rounded-full bg-foreground hover:bg-foreground/90"
            onClick={onDeposit}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-background"
              src={`${ASSET_BASE}/icon-plus.svg`}
            />
            <span className="whitespace-nowrap pr-2.5 font-medium text-[16px] text-background leading-5">
              Deposit
            </span>
          </button>
          <PendingWithdrawalTooltip pending={hasPendingWithdrawal} className="min-w-0 flex-1">
<button
            className="t-hover flex h-12 w-full min-w-0 flex-1 items-center justify-center gap-2 rounded-full bg-accent enabled:hover:bg-accent-active disabled:opacity-40"
            disabled={!canWithdraw}
            onClick={onWithdraw}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${ASSET_BASE}/icon-withdraw-arrow.svg`}
            />
            <span className="whitespace-nowrap pr-2.5 font-medium text-[16px] text-foreground leading-5">
              Withdraw
            </span>
          </button>
</PendingWithdrawalTooltip>
        </div>
      </div>
    </section>
  );
}

export function EarnMaxWorkspace({
  earnData,
  earnMax,
  onBack,
  onOpenSettings,
  onViewAllActivity,
}: {
  earnData: EarnPositionData;
  earnMax: { actions: EarnMaxActions; view: EarnMaxViewModel };
  onBack: () => void;
  onOpenSettings: () => void;
  onViewAllActivity: () => void;
}) {
  const { isHydrated, isSignedIn } = useAuthCapability();
  const [screen, setScreen] = useState<"deposit" | "main" | "withdraw">("main");
  const [chartTab, setChartTab] = useState<ChartTab | null>(null);
  const [isChartExpanded, setIsChartExpanded] = useState(false);
  const [selectedTransaction, setSelectedTransaction] =
    useState<EarnMaxActivityItem | null>(null);
  const transactionDetail = useLastValue(selectedTransaction);
  const { view, actions } = earnMax;
  const { apy: publicApy, isLoaded: isPublicApyLoaded } = useEarnMaxApy();
  const isVirgin = isEarnMaxUntouched(view);
  useEffect(() => {
    if (isHydrated && !isSignedIn && screen !== "main") {
      setScreen("main");
    }
  }, [isHydrated, isSignedIn, screen]);
  useEffect(() => {
    if ((screen !== "main" || !isSignedIn) && selectedTransaction) {
      setSelectedTransaction(null);
    }
  }, [isSignedIn, screen, selectedTransaction]);
  useEscapePage(screen !== "main" || selectedTransaction !== null, () => {
    if (selectedTransaction) {
      setSelectedTransaction(null);
    } else {
      setScreen("main");
    }
  });
  const chartCustom: ChartCardCustomContent = {
    defaultTab: isSignedIn && !isVirgin ? "Earned" : "APY",
    loggedOutTooltip:
      "Historical APY of the Earn MAX strategy over the last 30 days.",
    renderBody: (activeTab) =>
      activeTab === "Earned" ? (
        <EarnedBarsChart
          bars={buildEarnMaxEarnedBars(view)}
          isLoading={view.isLoading}
          isStale={false}
          isUnavailable={false}
          lifetimeEarnedUsd={view.earnedUsd ?? 0}
          note="Can dip for short periods as asset prices and rates move."
        />
      ) : !isPublicApyLoaded ? (
        <div aria-hidden="true" className="min-h-0 w-full flex-1" />
      ) : (
        <HistoricalApyChart
          apyDataRevealed
          axisTickCount={2}
          primaryLabel="Earn MAX"
          primarySamples={buildEarnMaxApySamples(publicApy)}
          rangeId="30D"
        />
      ),
    tabs: EARN_MAX_CHART_TABS,
  };
  return (
    <div className="flex h-full min-h-0 min-w-0 flex-1 gap-2 p-2 max-[795px]:gap-0 max-[795px]:p-0">
      <MiddlePaneSlide
        actionPane={
          screen === "deposit" ? (
            <EarnMaxDepositPane
              actions={actions}
              data={earnData}
              onBack={() => setScreen("main")}
              view={view}
            />
          ) : screen === "withdraw" ? (
            <EarnMaxWithdrawPane
              actions={actions}
              data={earnData}
              onBack={() => setScreen("main")}
              view={view}
            />
          ) : null
        }
      >
        {isHydrated && !isSignedIn ? (
          <PaneReveal>
            <EarnEmptyPane
              onOpenSettings={onOpenSettings}
              staticHeadline
              title="Earn MAX"
              tooltipText={EARN_MAX_TOOLTIP_TEXT}
            />
          </PaneReveal>
        ) : !isHydrated ? (
          <section className="flex h-full min-w-0 flex-1 rounded-3xl bg-card max-[795px]:rounded-none" />
        ) : isVirgin ? (
          <PaneReveal>
            <EarnEmptyPane
              apyBadgeLabel={formatEarnMaxApyLabel(view.forecastApyBps)}
              onBack={onBack}
              onDeposit={() => setScreen("deposit")}
              title="Earn MAX"
              tooltipText={EARN_MAX_TOOLTIP_TEXT}
            />
          </PaneReveal>
        ) : (
          <PaneReveal>
            <EarnMaxMainPane
              actions={actions}
              chartCustom={chartCustom}
              earnData={earnData}
              onBack={onBack}
              onDeposit={() => setScreen("deposit")}
              onOpenChart={() => setIsChartExpanded(true)}
              onSelectChartTab={setChartTab}
              selectedChartTab={chartTab}
              onSelectTransaction={(item) =>
                setSelectedTransaction((current) =>
                  current?.id === item.id ? null : item
                )
              }
              onViewAllActivity={onViewAllActivity}
              onWithdraw={() => {
                if (view.canWithdraw) {
                  setScreen("withdraw");
                }
              }}
              selectedTransactionId={selectedTransaction?.id ?? null}
              view={view}
            />
          </PaneReveal>
        )}
      </MiddlePaneSlide>
      <SheetReveal
        isOpen={selectedTransaction !== null && screen === "main"}
        onClose={() => setSelectedTransaction(null)}
        scrimClassName="fixed inset-0 z-50 flex bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8 min-[1204px]:hidden"
        sheetClassName="ml-auto flex h-full w-[400px] min-w-0 flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
      >
        {transactionDetail ? (
          <EarnMaxTransactionDetailPane
            item={transactionDetail}
            key={transactionDetail.id}
            onClose={() => setSelectedTransaction(null)}
            walletAddress={earnData.walletAddress ?? null}
          />
        ) : null}
      </SheetReveal>
      {screen !== "main" ? null : selectedTransaction && transactionDetail ? (
        <aside className="hidden h-full w-[400px] shrink-0 flex-col overflow-clip rounded-3xl bg-card min-[1204px]:flex">
          <PaneReveal key={transactionDetail.id}>
            <EarnMaxTransactionDetailPane
              item={transactionDetail}
              onClose={() => setSelectedTransaction(null)}
              walletAddress={earnData.walletAddress ?? null}
            />
          </PaneReveal>
        </aside>
      ) : (
        <EarnChartPane
          custom={chartCustom}
          earnData={earnData}
          isExpanded={isChartExpanded}
          isLoggedOut={isHydrated && !isSignedIn}
          onExpandedChange={setIsChartExpanded}
          onSelectTab={setChartTab}
          selectedTab={chartTab}
          statsPanel={
            <EarnMaxInfoFaqsCard className="flex min-h-[240px] w-full flex-1" />
          }
        />
      )}
    </div>
  );
}
