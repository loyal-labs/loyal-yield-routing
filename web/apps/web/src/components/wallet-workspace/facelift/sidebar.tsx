"use client";

import { formatEarnMaxApyLabel } from "@loyal-labs/shared";
import { useWallet } from "@solana/wallet-adapter-react";
import { Popover } from "radix-ui";
import {
  type CSSProperties,
  useEffect,
  useRef,
  useState,
} from "react";

import {
  ScrambledPopDigits,
  useBalanceVisibility,
} from "@/components/wallet-workspace/facelift/balance-visibility";
import { copyTextToClipboard } from "@/components/wallet-workspace/facelift/copy-text";
import { InfoTooltip } from "@/components/wallet-workspace/facelift/info-tooltip";
import { PopDigits } from "@/components/wallet-workspace/facelift/pop-digits";
import type { WorkspacePage } from "@/components/wallet-workspace/facelift/shell";
import { SkeletonReveal } from "@/components/wallet-workspace/facelift/skeleton-reveal";
import { TextSwap } from "@/components/wallet-workspace/facelift/text-swap";
import { AddEmailNudge } from "@/components/auth/add-email-nudge";
import { LedgerSignInLink } from "@/components/auth/ledger-sign-in-link";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";
import { useEarnMaxApy } from "@/features/earn-max";
import { useEarnForecastApy } from "@/hooks/use-earn-forecast-apy";
import { useSignInModal } from "@/contexts/sign-in-modal-context";
import { useCherryRuntime } from "@/features/cherry/client/runtime-context";
import { useAuthCapability } from "@/lib/auth/capability";
import { captureBrowserLoadingMetricAfterPaint } from "@/features/observability/client";
import {
  splitUsdBalance,
  useWalletDesktopData,
} from "@/hooks/use-wallet-desktop-data";
import { useTheme } from "@/hooks/use-theme";
import { formatEarnApyLabel } from "@/lib/kamino/earn-forecast.shared";

const ASSET_BASE = "/wallet-workspace/facelift";
const WALLET_ICONS: Readonly<Record<string, string>> = {
  phantom: "wallet-phantom.svg",
  solflare: "wallet-solflare.svg",
  jupiter: "wallet-jupiter.png",
  zerion: "wallet-zerion.svg",
};

export const SIDEBAR_LINKS = [
  {
    href: "https://x.com/loyal_hq",
    icon: "icon-x-social.svg",
    label: "Follow Loyal on X",
  },
  {
    href: "https://docs.askloyal.com",
    icon: "icon-docs.svg",
    label: "Documentation",
  },
  {
    href: "https://tally.so/r/ZjRpev",
    icon: "icon-bug.svg",
    label: "Report a bug",
  },
  {
    href: "https://t.me/loyal_tgchat",
    icon: "icon-support.svg",
    label: "Support",
  },
  {
    href: "https://askloyal.com",
    icon: "icon-globe.svg",
    label: "Visit askloyal.com",
  },
] as const;

export function SplitAmount({
  fraction,
  fractionColor = "var(--tertiary)",
  isHidden = false,
  isRevealed,
  whole,
}: {
  fraction: string;
  fractionColor?: string;
  isHidden?: boolean;
  isRevealed: boolean;
  whole: string;
}) {
  return (
    <p className="whitespace-nowrap font-semibold text-[20px] text-foreground leading-6">
      <SkeletonReveal
        isRevealed={isRevealed}
        skeletonClassName="rounded-md bg-accent-selected"
      >
        {isRevealed ? (
          <ScrambledPopDigits
            isHidden={isHidden}
            segments={[
              { text: whole },
              { color: fractionColor, text: fraction },
            ]}
          />
        ) : (
          `${whole}${fraction}`
        )}
      </SkeletonReveal>
    </p>
  );
}

function ShortcutKey({
  isFlashed,
  letter,
}: {
  isFlashed: boolean;
  letter: string;
}) {
  return (
    <kbd
      className={`flex h-4 min-w-4 items-center justify-center rounded-[5px] px-0.5 font-medium font-sans text-[11px] leading-none ${
        isFlashed
          ? // A keypress lands instantly, so its feedback must too — only the
            "bg-primary text-white opacity-100 transition-none"
          : "bg-accent-selected text-muted-foreground opacity-0 transition-opacity duration-150 group-hover:opacity-100"
      }`}
    >
      {letter}
    </kbd>
  );
}

export function FaceliftSidebar({
  activePage,
  onOpenSettings,
  onAssetAction,
  earnPolicyUpdates = 0,
  earnMaxPolicyUpdates = 0,
  earnBalanceUsd,
  earnMaxBalanceUsd,
  flashedShortcut,
  isEarnBalanceLoading,
  isEarnMaxBalanceLoading,
  onSelectPage,
  onUnseenActivityChange,
  showEarnMax,
}: {
  activePage: WorkspacePage;
  onOpenSettings: () => void;
  onAssetAction: (action: "send" | "receive" | "swap" | "bridge") => void;
  earnPolicyUpdates?: number;
  earnMaxPolicyUpdates?: number;
  earnBalanceUsd: number;
  earnMaxBalanceUsd: number;
  flashedShortcut: WorkspacePage | null;
  isEarnBalanceLoading: boolean;
  isEarnMaxBalanceLoading: boolean;
  onSelectPage: (page: WorkspacePage) => void;
  onUnseenActivityChange?: (hasUnseen: boolean) => void;
  showEarnMax: boolean;
}) {
  const data = useWalletDesktopData({});
  const { apy: earnApy, isLoaded: isApyLoaded } = useEarnForecastApy();
  const { isHydrated, isSignedIn } = useAuthCapability();
  const cherryRuntime = useCherryRuntime();
  const { publicKey, wallet } = useWallet();
  const walletIcon =
    publicKey?.toBase58() === data.walletAddress
      ? WALLET_ICONS[wallet?.adapter.name.toLowerCase() ?? ""]
      : undefined;
  const { isBalanceHidden, toggleBalanceHidden } = useBalanceVisibility();
  const { open: openSignIn } = useSignInModal();
  const { isDark, toggleTheme } = useTheme();
  const loadActivity = data.loadActivity;
  useEffect(() => {
    if (data.walletAddress) {
      void loadActivity();
    }
  }, [data.walletAddress, loadActivity]);
  const latestActivityAt = data.allActivityRows[0]?.rawTimestampMs ?? 0;
  const activitySeenKey = data.walletAddress
    ? `loyal:activity-seen:${data.walletAddress}`
    : null;
  const [activitySeenAt, setActivitySeenAt] = useState(0);
  useEffect(() => {
    if (!activitySeenKey) {
      setActivitySeenAt(0);
      return;
    }
    const stored = Number(localStorage.getItem(activitySeenKey) ?? "0");
    setActivitySeenAt(Number.isFinite(stored) ? stored : 0);
  }, [activitySeenKey]);
  useEffect(() => {
    if (activePage === "activity" && activitySeenKey && latestActivityAt > 0) {
      localStorage.setItem(activitySeenKey, String(latestActivityAt));
      setActivitySeenAt(latestActivityAt);
    }
  }, [activePage, activitySeenKey, latestActivityAt]);
  const hasUnseenActivity =
    activePage !== "activity" && latestActivityAt > activitySeenAt;
  useEffect(() => {
    onUnseenActivityChange?.(hasUnseenActivity);
  }, [hasUnseenActivity, onUnseenActivityChange]);

  const assetsBalance = splitUsdBalance(data.totalUsd);
  const earnBalance = splitUsdBalance(earnBalanceUsd);
  const earnMaxBalance = splitUsdBalance(earnMaxBalanceUsd);
  const earnMaxApyBps =
    useEarnMaxApy({ enabled: showEarnMax }).apy?.apyBps ?? null;
  const earnMaxApyLabel =
    earnMaxApyBps === null ? null : formatEarnMaxApyLabel(earnMaxApyBps);
  const totalBalance = splitUsdBalance(
    data.totalUsd + earnBalanceUsd + (showEarnMax ? earnMaxBalanceUsd : 0)
  );

  const addressLabel = data.walletAddress
    ? `${data.walletAddress.slice(0, 4)}…${data.walletAddress.slice(-4)}`
    : "No account";

  const isAddressRevealed =
    isHydrated && (!isSignedIn || data.walletAddress !== null);
  const isWalletDataRevealed =
    isHydrated &&
    (!isSignedIn || (data.walletAddress !== null && !data.isLoading));
  const isEarnBalanceRevealed = !isEarnBalanceLoading;
  const isEarnMaxBalanceRevealed = !isEarnMaxBalanceLoading;
  const isTotalRevealed =
    isWalletDataRevealed &&
    isEarnBalanceRevealed &&
    (!showEarnMax || isEarnMaxBalanceRevealed);
  const hasReportedBalancesReadyRef = useRef(false);
  useEffect(() => {
    if (
      hasReportedBalancesReadyRef.current ||
      !isSignedIn ||
      !isTotalRevealed
    ) {
      return;
    }

    hasReportedBalancesReadyRef.current = true;
    captureBrowserLoadingMetricAfterPaint({
      operation: "page_load",
      phase: "balances_ready",
      startedAtMs: 0,
    });
  }, [isSignedIn, isTotalRevealed]);

  const [isCopied, setIsCopied] = useState(false);
  const copiedTimerRef = useRef<number | null>(null);
  useEffect(
    () => () => {
      if (copiedTimerRef.current !== null) {
        window.clearTimeout(copiedTimerRef.current);
      }
    },
    []
  );
  const showCopied = () => {
    setIsCopied(true);
    if (copiedTimerRef.current !== null) {
      window.clearTimeout(copiedTimerRef.current);
    }
    copiedTimerRef.current = window.setTimeout(() => {
      copiedTimerRef.current = null;
      setIsCopied(false);
    }, 1500);
  };

  const isSignedOutNav = isHydrated && !isSignedIn;

  const handleCopyAddress = () => {
    const address = data.walletAddress;
    if (!address) {
      return;
    }
    void copyTextToClipboard(address).then((didCopy) => {
      if (didCopy) {
        showCopied();
      }
    });
  };

  return (
    <aside className="flex h-full w-[360px] shrink-0 flex-col overflow-clip p-2 max-[795px]:hidden">
      <div className="flex h-[60px] w-full shrink-0 items-center">
        <ThemedIcon
          className="ml-4 h-6 w-14 text-foreground"
          label="Loyal"
          src={`${ASSET_BASE}/logotype.svg`}
        />
        <div className="flex min-w-0 flex-1 items-center justify-end pl-3">
          <span className="group flex items-center gap-1.5">
            <ShortcutKey
              isFlashed={flashedShortcut === "activity"}
              letter="H"
            />
            <button
              aria-label="Activity"
              className={`t-hover relative flex size-11 items-center justify-center rounded-3xl ${
                activePage === "activity" ? "bg-accent" : "hover:bg-accent"
              }`}
              onClick={() => onSelectPage("activity")}
              type="button"
            >
              <ThemedIcon
                className="size-6 text-tertiary"
                src={`${ASSET_BASE}/icon-clock-history.svg`}
              />
              <span
                className="t-badge t-badge-activity"
                data-open={hasUnseenActivity ? "true" : "false"}
              >
                <span className="t-badge-dot size-1.5 rounded-full bg-primary" />
              </span>
            </button>
          </span>
        </div>
      </div>

      <div className="min-h-0 w-full flex-1 overflow-y-auto [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      {isSignedOutNav ? null : (
      <div className="w-full py-2">
        <div className="flex w-full flex-col gap-0.5 px-4 py-2">
          <div className="flex items-center gap-1">
            <p className="whitespace-nowrap text-[16px] leading-5 text-muted-foreground">
              Total balance
            </p>
            <InfoTooltip text="Wallet and Earn balances combined" />
          </div>
          <div className="flex items-center gap-3">
            <p className="whitespace-nowrap font-semibold text-[40px] text-foreground leading-[48px] tracking-[-0.44px]">
              <SkeletonReveal
                isRevealed={isTotalRevealed}
                skeletonClassName="rounded-lg bg-accent-selected"
              >
                {isTotalRevealed ? (
                  <ScrambledPopDigits
                    isHidden={isBalanceHidden}
                    segments={[
                      { text: totalBalance.balanceWhole },
                      {
                        color: "var(--tertiary)",
                        text: totalBalance.balanceFraction,
                      },
                    ]}
                  />
                ) : (
                  `${totalBalance.balanceWhole}${totalBalance.balanceFraction}`
                )}
              </SkeletonReveal>
            </p>
            <button
              aria-label={isBalanceHidden ? "Show balance" : "Hide balance"}
              className="t-hover -m-2.5 flex size-11 shrink-0 items-center justify-center rounded-3xl hover:bg-accent"
              onClick={toggleBalanceHidden}
              type="button"
            >
              <ThemedIcon
                className="size-6 text-tertiary"
                src={`${ASSET_BASE}/icon-eye.svg`}
              />
            </button>
          </div>
        </div>
        <div aria-label="Asset actions" className="flex w-full">
          {(["send", "receive", "swap", "bridge"] as const).map((action) => (
            <button
              className="t-hover flex min-w-0 flex-1 flex-col items-center gap-1 rounded-2xl py-2 enabled:hover:bg-accent disabled:cursor-not-allowed disabled:opacity-40"
              key={action}
              disabled={action === "bridge"}
              title={action === "bridge" ? "Coming soon" : undefined}
              onClick={() => onAssetAction(action)}
              type="button"
            >
              <span className="flex size-11 items-center justify-center rounded-full bg-accent">
                <ThemedIcon className="size-6 text-foreground" src={`${ASSET_BASE}/sidebar/${action}.svg`} />
              </span>
              <span className="text-[13px] leading-4 text-muted-foreground">
                {action[0].toUpperCase() + action.slice(1)}
              </span>
            </button>
          ))}
        </div>
      </div>
      )}

      <div className="w-full px-2">
        <AddEmailNudge />
      </div>

      {isSignedOutNav ? (
        <nav className="flex w-full flex-col gap-2 py-2">
          <button
            className={`t-hover flex w-full items-center rounded-2xl px-4 text-left ${
              activePage === "earn" ? "bg-accent" : "hover:bg-accent"
            }`}
            onClick={() => onSelectPage("earn")}
            type="button"
          >
            <span className="flex items-center py-2 pr-3">
              {/* eslint-disable-next-line @next/next/no-img-element */}
              <img
                alt=""
                aria-hidden="true"
                className="size-11 shrink-0"
                src={`${ASSET_BASE}/earn-icon.svg`}
              />
            </span>
            <span className="flex min-w-0 flex-1 items-center gap-1 py-2">
              <span className="whitespace-nowrap font-medium text-[16px] text-foreground leading-5">
                Earn
              </span>
              <SkeletonReveal
                isRevealed={isApyLoaded}
                skeletonClassName="rounded-md bg-accent-selected"
              >
                <span className="inline-flex items-center rounded-md bg-positive/[0.14] px-1 py-px">
                  <span className="whitespace-nowrap pt-px font-medium text-positive text-[11px] leading-[13px] tracking-[0.06px]">
                    {formatEarnApyLabel(earnApy)}
                  </span>
                </span>
              </SkeletonReveal>
            </span>
          </button>
          <button
            className={`t-hover flex w-full items-center rounded-2xl px-4 text-left ${
              activePage === "earnmax" ? "bg-accent" : "hover:bg-accent"
            }`}
            onClick={() => onSelectPage("earnmax")}
            type="button"
          >
            <span className="flex items-center py-2 pr-3">
              {/* eslint-disable-next-line @next/next/no-img-element */}
              <img
                alt=""
                aria-hidden="true"
                className="size-11 shrink-0"
                src={`${ASSET_BASE}/earn-max-icon.svg`}
              />
            </span>
            <span className="flex min-w-0 flex-1 items-center gap-1 py-2">
              <span className="whitespace-nowrap font-medium text-[16px] text-foreground leading-5">
                Earn MAX
              </span>
              {earnMaxApyLabel ? (
                <span className="inline-flex items-center rounded-md bg-positive/[0.14] px-1 py-px">
                  <span className="whitespace-nowrap pt-px font-medium text-positive text-[11px] leading-[13px] tracking-[0.06px]">
                    {earnMaxApyLabel}
                  </span>
                </span>
              ) : null}
            </span>
          </button>
        </nav>
      ) : (
      <nav className="flex w-full flex-col gap-2 py-2">
        <button
          className={`t-hover group flex h-[60px] w-full items-center rounded-2xl px-4 text-left ${
            activePage === "earn" ? "bg-accent" : "hover:bg-accent"
          }`}
          onClick={() => onSelectPage("earn")}
          type="button"
        >
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img
            alt=""
            aria-hidden="true"
            className="mr-3 size-11 shrink-0"
            src={`${ASSET_BASE}/earn-icon.svg`}
          />
          <span className="flex min-w-0 flex-1 flex-col gap-1 py-2">
            <span className="flex items-center gap-1">
              <span className="whitespace-nowrap text-[13px] leading-4 text-muted-foreground">
                Earn
              </span>
              <SkeletonReveal
                isRevealed={isApyLoaded}
                skeletonClassName="rounded-md bg-accent-selected"
              >
                <span className="inline-flex items-center gap-0.5 rounded-md bg-positive/[0.14] px-1 py-px">
                  {/* eslint-disable-next-line @next/next/no-img-element */}
                  <img
                    alt=""
                    aria-hidden="true"
                    className="h-3 w-2"
                    src="/wallet-workspace/earn-flash.svg"
                  />
                  <span className="whitespace-nowrap pt-px font-medium text-positive text-[11px] leading-[13px] tracking-[0.06px]">
                    <PopDigits
                      segments={[
                        {
                          text: formatEarnApyLabel(earnApy),
                        },
                      ]}
                    />
                  </span>
                </span>
              </SkeletonReveal>
              {earnPolicyUpdates > 0 && <ThemedIcon label="Earn policy update required" className="ml-auto size-4 text-destructive" src={`${ASSET_BASE}/settings/warning-small.svg`} />}
                  <ShortcutKey isFlashed={flashedShortcut === "earn"} letter="E" />
            </span>
            <SplitAmount
              fraction={earnBalance.balanceFraction}
              fractionColor="var(--tertiary)"
              isHidden={isBalanceHidden}
              isRevealed={isEarnBalanceRevealed}
              whole={earnBalance.balanceWhole}
            />
          </span>
        </button>

        {showEarnMax ? (
          <button
            className={`t-hover group flex h-[60px] w-full items-center rounded-2xl px-4 text-left ${
              activePage === "earnmax" ? "bg-accent" : "hover:bg-accent"
            }`}
            onClick={() => onSelectPage("earnmax")}
            type="button"
          >
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img
              alt=""
              aria-hidden="true"
              className="mr-3 size-11 shrink-0"
              src={`${ASSET_BASE}/earn-max-icon.svg`}
            />
            <span className="flex min-w-0 flex-1 flex-col gap-1 py-2">
              <span className="flex items-center gap-1">
                <span className="whitespace-nowrap text-[13px] leading-4 text-muted-foreground">
                  Earn MAX
                </span>
                {earnMaxApyLabel ? (
                  <span className="inline-flex items-center gap-0.5 rounded-md bg-positive/[0.14] px-1 py-px">
                    {/* eslint-disable-next-line @next/next/no-img-element */}
                    <img
                      alt=""
                      aria-hidden="true"
                      className="h-3 w-2"
                      src="/wallet-workspace/earn-flash.svg"
                    />
                    <span className="whitespace-nowrap pt-px font-medium text-positive text-[11px] leading-[13px] tracking-[0.06px]">
                      {earnMaxApyLabel}
                    </span>
                  </span>
                ) : null}
                {earnMaxPolicyUpdates > 0 && <ThemedIcon label="Earn MAX policy update required" className="ml-auto size-4 text-destructive" src={`${ASSET_BASE}/settings/warning-small.svg`} />}
                  <ShortcutKey
                  isFlashed={flashedShortcut === "earnmax"}
                  letter="M"
                />
              </span>
              <SplitAmount
                fraction={earnMaxBalance.balanceFraction}
                isHidden={isBalanceHidden}
                isRevealed={isEarnMaxBalanceRevealed}
                whole={earnMaxBalance.balanceWhole}
              />
            </span>
          </button>
        ) : null}

        <button
          className={`t-hover group flex h-[60px] w-full items-center rounded-2xl px-4 text-left ${
            activePage === "assets" ? "bg-accent" : "hover:bg-accent"
          }`}
          onClick={() => onSelectPage("assets")}
          type="button"
        >
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img
            alt=""
            aria-hidden="true"
            className="mr-3 size-11 shrink-0"
            src={`${ASSET_BASE}/sidebar/assets.svg`}
          />
          <span className="flex min-w-0 flex-1 flex-col gap-1 py-2">
            <span className="flex items-center gap-1">
              <span className="whitespace-nowrap text-[13px] leading-4 text-muted-foreground">
                Assets
              </span>
              <ShortcutKey
                isFlashed={flashedShortcut === "assets"}
                letter="A"
              />
            </span>
            <SplitAmount
              fraction={assetsBalance.balanceFraction}
              isHidden={isBalanceHidden}
              isRevealed={isWalletDataRevealed}
              whole={assetsBalance.balanceWhole}
            />
          </span>
        </button>
      </nav>
      )}

        <button
          className="flex min-h-[60px] w-full cursor-default items-center rounded-2xl px-4 text-left"
          disabled
          type="button"
        >
          <span className="mr-3 flex size-11 shrink-0 items-center justify-center rounded-[11px] bg-primary">
            <ThemedIcon className="size-7 text-white" src={`${ASSET_BASE}/icon-bone.svg`} />
          </span>
          <span className="flex min-w-0 flex-1 items-center gap-2 py-2">
            <span className="font-medium text-[16px] leading-5 text-foreground">Quests</span>
            <span className="rounded-full bg-accent px-2 py-1 text-[11px] leading-[13px] text-muted-foreground">Coming soon</span>
          </span>
        </button>

        <div className="w-full py-2">
          <div className="[@media(max-height:899px)]:hidden">
            <SidebarResourceLinks />
          </div>
          <div className="hidden [@media(max-height:899px)]:block">
            <Popover.Root>
              <Popover.Trigger asChild>
                <button
                  className="t-hover flex min-h-11 w-full items-center rounded-2xl px-4 text-left text-muted-foreground hover:bg-accent"
                  type="button"
                >
                  <span className="mr-3 flex size-11 shrink-0 items-center justify-center">
                    <ThemedIcon
                      className="size-6 text-tertiary"
                      src={`${ASSET_BASE}/icon-question.svg`}
                    />
                  </span>
                  <span className="flex-1 font-medium text-[16px] leading-5">
                    Help &amp; Resources
                  </span>
                  <ThemedIcon
                    className="size-6 text-tertiary"
                    src={`${ASSET_BASE}/icon-chevron-grabber.svg`}
                  />
                </button>
              </Popover.Trigger>
              <Popover.Portal>
                <Popover.Content
                  aria-label="Help and resources"
                  align="start"
                  side="top"
                  sideOffset={4}
                  collisionPadding={8}
                  onEscapeKeyDown={(event) => event.stopPropagation()}
                  className="z-50 max-h-[var(--radix-popover-content-available-height)] w-[var(--radix-popover-trigger-width)] overflow-y-auto rounded-2xl bg-popover/70 p-2 shadow-[0px_0px_2px_0px_rgba(0,0,0,0.08),0px_4px_16px_0px_rgba(0,0,0,0.08)] backdrop-blur-[16px] data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=open]:fade-in-0 data-[state=closed]:fade-out-0 data-[state=open]:zoom-in-95 data-[state=closed]:zoom-out-95 motion-reduce:animate-none"
                >
                  <SidebarResourceLinks />
                </Popover.Content>
              </Popover.Portal>
            </Popover.Root>
          </div>
        </div>

      </div>

      {isHydrated && !isSignedIn ? (
        <div className="flex w-full shrink-0 items-center gap-2">
          <button
            className="t-hover flex h-[60px] items-center rounded-2xl px-4 text-left hover:bg-accent"
            onClick={openSignIn}
            type="button"
          >
            {/* Figma 5429:36740 — plain wallet chip replaces the agent
                avatar on the redesigned logged-out sidebar. */}
            <span className="mr-3 flex size-11 shrink-0 items-center justify-center rounded-[11px] bg-accent">
              <ThemedIcon
                className="size-7 text-tertiary"
                src={`${ASSET_BASE}/icon-wallet-fill.svg`}
              />
            </span>
            <span className="whitespace-nowrap font-medium text-[16px] text-foreground leading-5">
              {cherryRuntime.mode === "cherry_embedded"
                ? "Verify account"
                : "Connect wallet"}
            </span>
          </button>
          {cherryRuntime.mode === "standalone" ? <LedgerSignInLink /> : null}
          <div className="flex min-w-0 flex-1 items-center justify-end pl-3">
            <button
              aria-label={
                isDark ? "Switch to light theme" : "Switch to dark theme"
              }
              className="t-hover flex size-11 items-center justify-center rounded-3xl text-tertiary hover:bg-accent"
              onClick={(event) => {
                const rect = event.currentTarget.getBoundingClientRect();
                toggleTheme({
                  x: rect.left + rect.width / 2,
                  y: rect.top + rect.height / 2,
                });
              }}
              type="button"
            >
              {isDark ? (
                <ThemedIcon className="size-6" src={`${ASSET_BASE}/settings/sun.svg`} />
              ) : (
                <ThemedIcon className="size-6" src={`${ASSET_BASE}/settings/moon.svg`} />
              )}
            </button>
          </div>
        </div>
      ) : (
        <div className="relative flex w-full shrink-0 items-center">
          <button
            aria-label={isCopied ? "Address copied" : "Copy wallet address"}
            className="t-hover flex h-[60px] min-w-0 items-center rounded-2xl px-4 text-left hover:bg-accent"
            onClick={handleCopyAddress}
            type="button"
          >
            {/* eslint-disable-next-line @next/next/no-img-element */}
            <img
              alt=""
              aria-hidden="true"
              className="mr-3 size-11 shrink-0 rounded-[11px]"
              src={`${ASSET_BASE}/${walletIcon ?? "wallet-dog.svg"}`}
            />
            <span className="flex min-w-0 items-center gap-1">
              <span className="whitespace-nowrap text-[16px] text-foreground leading-5">
                <SkeletonReveal
                  isRevealed={isAddressRevealed}
                  skeletonClassName="rounded-md bg-accent-selected"
                >
                  <TextSwap text={addressLabel} />
                </SkeletonReveal>
              </span>
              {/* transitions.dev icon swap: copy ↔ check while isCopied. */}
              <span
                className="t-icon-swap size-6 shrink-0 cursor-pointer"
                data-state={isCopied ? "b" : "a"}
                onClick={(event) => {
                  event.stopPropagation();
                  handleCopyAddress();
                }}
              >
                <span
                  className="t-icon icon-themed size-6 text-tertiary"
                  data-icon="a"
                  role="img"
                  aria-label="Copy address"
                  style={
                    {
                      "--icon": `url("${ASSET_BASE}/icon-copy.svg")`,
                    } as CSSProperties
                  }
                />
                <span
                  aria-hidden="true"
                  className="t-icon icon-themed size-6 text-tertiary"
                  data-icon="b"
                  style={
                    {
                      "--icon": `url("${ASSET_BASE}/icon-check.svg")`,
                    } as CSSProperties
                  }
                />
              </span>
            </span>
          </button>
          <div className="flex min-w-0 flex-1 items-center justify-end pl-3">
            <button
              aria-label={
                isDark ? "Switch to light theme" : "Switch to dark theme"
              }
              className="t-hover flex size-11 items-center justify-center rounded-3xl text-tertiary hover:bg-accent"
              onClick={(event) => {
                // Wipe from the toggle itself (also right for keyboard
                // activation, where click coords are unreliable).
                const rect = event.currentTarget.getBoundingClientRect();
                toggleTheme({
                  x: rect.left + rect.width / 2,
                  y: rect.top + rect.height / 2,
                });
              }}
              type="button"
            >
              {isDark ? (
                <ThemedIcon className="size-6" src={`${ASSET_BASE}/settings/sun.svg`} />
              ) : (
                <ThemedIcon className="size-6" src={`${ASSET_BASE}/settings/moon.svg`} />
              )}
            </button>
            <button aria-label="Settings" className="t-hover relative flex size-11 shrink-0 items-center justify-center rounded-full hover:bg-accent" onClick={onOpenSettings} type="button">
              <ThemedIcon className="size-6 text-tertiary" src={`${ASSET_BASE}/icon-gear.svg`} />
              {earnPolicyUpdates + earnMaxPolicyUpdates > 0 && <span aria-label="Policy updates required" className="absolute right-2 top-2 size-1.5 rounded-full bg-primary" />}
            </button>
          </div>
        </div>
      )}
    </aside>
  );
}

function SidebarResourceLinks() {
  return (
    <nav aria-label="Help and resources" className="flex w-full flex-col">
      {SIDEBAR_LINKS.map((link) => (
        <a
          className="t-hover flex w-full items-center rounded-2xl px-4 hover:bg-accent"
          href={link.href}
          key={link.label}
          rel="noreferrer"
          target="_blank"
        >
          <span className="mr-3 flex size-11 shrink-0 items-center justify-center">
            <ThemedIcon
              className="size-6 text-tertiary"
              src={`${ASSET_BASE}/${link.icon}`}
            />
          </span>
          <span className="py-2 font-medium text-muted-foreground text-[16px] leading-5">
            {link.label}
          </span>
        </a>
      ))}
    </nav>
  );
}
