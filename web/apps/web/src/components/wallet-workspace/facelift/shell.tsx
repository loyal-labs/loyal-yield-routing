"use client";

import { useEffect, useMemo, useRef, useState } from "react";

import { WalletReconnectPrompt } from "@/components/auth/wallet-reconnect-prompt";
import { ActivityPage } from "@/components/wallet-workspace/facelift/activity-page";
import { AutodepositPane } from "@/components/wallet-workspace/facelift/autodeposit-pane";
import { AutoswapPane } from "@/components/wallet-workspace/facelift/autoswap-pane";
import {
  BalanceVisibilityProvider,
  HiddenBalanceFilterDefs,
} from "@/components/wallet-workspace/facelift/balance-visibility";
import { CryptoPage } from "@/components/wallet-workspace/facelift/crypto-page";
import { DepositPane } from "@/components/wallet-workspace/facelift/deposit-pane";
import { EarnApprovalSheet } from "@/components/wallet-workspace/facelift/earn-approval-sheet";
import { EarnBanners } from "@/components/wallet-workspace/facelift/earn-banner";
import {
  EarnChartPane,
  type ChartTab,
} from "@/components/wallet-workspace/facelift/earn-chart-pane";
import { startEarnEarningsPrefetch } from "@/components/wallet-workspace/facelift/earn-earnings-prefetch";
import { EarnExplainerCard } from "@/components/wallet-workspace/facelift/earn-explainer";
import { EarnEmptyPane } from "@/components/wallet-workspace/facelift/earn-empty-pane";
import {
  buildEarnMaxEarnedBars,
  EarnMaxWorkspace,
  isEarnMaxUntouched,
} from "@/components/wallet-workspace/facelift/earn-max-pane";
import { useEarnEarnedData } from "@/components/wallet-workspace/facelift/earned-chart";
import { EarnToastHost } from "@/components/wallet-workspace/facelift/earn-toast";
import { EarnStatsPanel } from "@/components/wallet-workspace/facelift/earn-stats-panel";
import {
  AUTOSWAP_HIDDEN,
  EarnPositionPane,
} from "@/components/wallet-workspace/facelift/earn-position-pane";
import { EarnTransactionDetailPane } from "@/components/wallet-workspace/facelift/transaction-detail-pane";
import { SettingsSheet } from "@/components/wallet-workspace/facelift/settings-sheet";
import {
  MiddlePaneSlide,
  PaneReveal,
} from "@/components/wallet-workspace/facelift/pane-transitions";
import { SheetReveal } from "@/components/wallet-workspace/facelift/sheet-reveal";
import { isTypingTarget } from "@/components/wallet-workspace/facelift/keyboard";
import { LifecycleOnboardingHost } from "@/components/wallet-workspace/facelift/lifecycle-onboarding";
import { FaceliftSidebar } from "@/components/wallet-workspace/facelift/sidebar";
import { useEarnPositionData } from "@/components/wallet-workspace/facelift/use-earn-position-data";
import {
  dispatchEscape,
  useEscapeOverlay,
  useEscapePage,
} from "@/components/wallet-workspace/facelift/escape-owner";
import { useLastValue } from "@/components/wallet-workspace/facelift/use-last-value";
import { useIsNarrowViewport } from "@/components/wallet-workspace/facelift/use-is-narrow-viewport";
import { WalletHomePage } from "@/components/wallet-workspace/facelift/wallet-home-page";
import { WithdrawPane } from "@/components/wallet-workspace/facelift/withdraw-pane";
import {
  WorkspacePaneSkeleton,
  WorkspaceSkeleton,
} from "@/components/wallet-workspace/facelift/workspace-skeleton";
import { usePublicEnv } from "@/contexts/public-env-context";
import { useSignInModal } from "@/contexts/sign-in-modal-context";
import { useEarnMax } from "@/features/earn-max";
import { useAuthCapability } from "@/lib/auth/capability";
import type { EarnTransactionItem } from "@/lib/yield-optimization/earn-transactions.client";

export type WorkspacePage =
  | "crypto"
  | "stables"
  | "activity"
  | "earn"
  | "earnmax"
  | "wallet";

type MiddleView = "earn" | "deposit" | "withdraw" | "autodeposit" | "autoswap";

const PAGE_STORAGE_KEY = "loyal:workspace-page";
const EARN_MAX_VISIBLE = true;
const WORKSPACE_PAGES: WorkspacePage[] = [
  "crypto",
  "stables",
  "activity",
  "earn",
  "earnmax",
  "wallet",
];

function readStoredPage(): WorkspacePage | null {
  if (typeof window === "undefined") {
    return null;
  }
  const stored = localStorage.getItem(PAGE_STORAGE_KEY);
  return WORKSPACE_PAGES.find((page) => page === stored) ?? null;
}

const SHORTCUT_PAGES: Partial<Record<string, WorkspacePage>> = {
  a: "activity",
  c: "crypto",
  e: "earn",
  m: "earnmax",
  s: "stables",
};

// Figma 4693:64818 — fixed 3-pane workspace: 360px sidebar on the gray shell,
// fluid middle panel, 400px right panel. Panes are intentionally not resizable.
// Below 1204px the right pane would force the dog under its 420px natural size,
// so it hides (Figma 4693:65423) and the middle header shows a chart button
// that opens the expanded chart overlay instead. Below 796px the sidebar goes
// too: screens run full-bleed with a bottom tab bar (Figma 4693:69948 et al.),
// and only the Earn screen with a position keeps the gray card background
// (Figma 4693:70364).
export function WorkspaceFaceliftShell() {
  const publicEnv = usePublicEnv();
  const [isChartExpanded, setIsChartExpanded] = useState(false);
  const [isSettingsOpen, setIsSettingsOpen] = useState(false);
  const [chartTab, setChartTab] = useState<ChartTab | null>(null);
  const [activePage, setActivePage] = useState<WorkspacePage>("earn");
  const { isHydrated, isSignedIn } = useAuthCapability();
  const { open: openSignIn } = useSignInModal();
  const [storedPage] = useState(readStoredPage);
  const hasRestoredPageRef = useRef(false);
  useEffect(() => {
    if (!isHydrated || hasRestoredPageRef.current) {
      return;
    }
    hasRestoredPageRef.current = true;
    if (storedPage === null || storedPage === "earn") {
      return;
    }
    if (!isSignedIn && storedPage !== "earnmax") {
      return;
    }
    setActivePage(storedPage);
  }, [isHydrated, isSignedIn, storedPage]);
  useEffect(() => {
    localStorage.setItem(PAGE_STORAGE_KEY, activePage);
  }, [activePage]);
  useEffect(() => {
    if (
      isHydrated &&
      !isSignedIn &&
      activePage !== "earn" &&
      activePage !== "earnmax"
    ) {
      setActivePage("earn");
    }
  }, [activePage, isHydrated, isSignedIn]);
  const isNarrowViewport = useIsNarrowViewport();
  useEffect(() => {
    if (!isNarrowViewport && activePage === "wallet") {
      setActivePage("earn");
    }
  }, [activePage, isNarrowViewport]);
  const [middleView, setMiddleView] = useState<MiddleView>("earn");
  const [selectedEarnTransaction, setSelectedEarnTransaction] =
    useState<EarnTransactionItem | null>(null);
  const earnTransactionDetail = useLastValue(selectedEarnTransaction);
  const [withdrawSourceKey, setWithdrawSourceKey] = useState<string | null>(
    null
  );
  const [depositSourceKey, setDepositSourceKey] = useState<string | null>(null);
  // Mirrored from the sidebar (which owns the unseen-activity computation)
  // so the mobile tab bar's clock shows the same badge.
  const [hasUnseenActivity, setHasUnseenActivity] = useState(false);
  const earnData = useEarnPositionData();
  const earnMaxAvailable = EARN_MAX_VISIBLE && isSignedIn;
  const { actions: earnMaxRawActions, view: earnMaxView } = useEarnMax({
    settingsPda: earnMaxAvailable ? earnData.settingsPda : undefined,
    walletAddress: earnMaxAvailable ? earnData.walletAddress : null,
  });
  const { ensureCanSignAccountAction } = earnData.actions;
  const earnMax = useMemo(() => {
    const signed =
      <A extends unknown[]>(run: (...args: A) => Promise<boolean>) =>
      (...args: A) =>
        ensureCanSignAccountAction() ? run(...args) : Promise.resolve(false);
    const actions = earnMaxRawActions;
    return {
      view: earnMaxView,
      actions: {
        ...actions,
        claim: signed(actions.claim),
        deposit: signed(actions.deposit),
        requestWithdrawal: signed(actions.requestWithdrawal),
      },
    };
  }, [earnMaxRawActions, earnMaxView, ensureCanSignAccountAction]);
  const earnEarned = useEarnEarnedData(earnData);
  const earnEarned30dUsd =
    earnEarned.showEarningsLoader || earnEarned.earningsUnavailable
      ? null
      : earnEarned.dailyBars.reduce((sum, bar) => sum + bar.earnedUsd, 0);
  const earnMaxEarned30dUsd = useMemo(
    () =>
      earnMax.view.isLoading
        ? null
        : buildEarnMaxEarnedBars(earnMax.view).reduce(
            (sum, bar) => sum + bar.earnedUsd,
            0
          ),
    [earnMax.view]
  );
  const [navigationNonce, setNavigationNonce] = useState(0);
  useEffect(() => {
    const walletAddress = earnData.walletAddress;
    const settingsPda = earnData.settingsPda;
    if (!(walletAddress && settingsPda)) {
      return;
    }

    startEarnEarningsPrefetch({
      enabled:
        isHydrated &&
        isSignedIn &&
        earnData.hasResolvedPosition &&
        earnData.hasPosition &&
        earnData.earnBalanceUsd > 0,
      revalidationKey: earnData.position?.principalAmountRaw ?? "0",
      settingsPda,
      solanaEnv: publicEnv.solanaEnv,
      timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC",
      walletAddress,
    });
  }, [
    earnData.earnBalanceUsd,
    earnData.hasPosition,
    earnData.hasResolvedPosition,
    earnData.position?.principalAmountRaw,
    earnData.settingsPda,
    earnData.walletAddress,
    isHydrated,
    isSignedIn,
    publicEnv.solanaEnv,
  ]);
  const handleSelectPage = (page: WorkspacePage) => {
    if (!EARN_MAX_VISIBLE && page === "earnmax") {
      return;
    }
    if (!isSignedIn && page !== "earn" && page !== "earnmax") {
      openSignIn();
      return;
    }
    setActivePage(
      page === "activity" && activePage === "activity" ? "earn" : page
    );
    setMiddleView("earn");
    setSelectedEarnTransaction(null);
    setNavigationNonce((nonce) => nonce + 1);
  };
  const [flashedShortcut, setFlashedShortcut] = useState<WorkspacePage | null>(
    null
  );
  const flashTimerRef = useRef<number | null>(null);
  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        dispatchEscape(event);
        return;
      }
      if (
        event.metaKey ||
        event.ctrlKey ||
        event.altKey ||
        isNarrowViewport
      ) {
        return;
      }
      if (!isSignedIn || isTypingTarget(event.target)) {
        return;
      }
      const page = SHORTCUT_PAGES[event.key.toLowerCase()];
      if (!page) {
        return;
      }
      handleSelectPage(page);
      setFlashedShortcut(page);
      if (flashTimerRef.current !== null) {
        window.clearTimeout(flashTimerRef.current);
      }
      flashTimerRef.current = window.setTimeout(
        () => setFlashedShortcut(null),
        800
      );
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  });
  const activeMiddleView: MiddleView = isSignedIn ? middleView : "earn";
  const { isReconnectPromptOpen, pendingApproval } = earnData.actions;
  const isShellEscapeActive =
    activePage === "activity" ||
    (activePage === "earn" &&
      activeMiddleView !== "earn" &&
      !pendingApproval &&
      !isReconnectPromptOpen);
  useEscapePage(isShellEscapeActive, (event) => {
    if (event.metaKey || event.ctrlKey || event.altKey || isNarrowViewport) {
      return;
    }
    if (activePage === "activity") {
      setActivePage("earn");
    } else {
      setMiddleView("earn");
    }
  });

  useEscapeOverlay(selectedEarnTransaction !== null, () => {
    if (document.activeElement instanceof HTMLElement) {
      document.activeElement.blur();
    }
    setSelectedEarnTransaction(null);
  });

  useEffect(() => {
    if (
      selectedEarnTransaction &&
      (activePage !== "earn" || activeMiddleView !== "earn")
    ) {
      setSelectedEarnTransaction(null);
    }
  }, [activeMiddleView, activePage, selectedEarnTransaction]);

  const isPositionLoading =
    !isHydrated || (isSignedIn && !earnData.hasResolvedPosition);
  const isEarnRootView = activeMiddleView === "earn";
  const isMobileGrayBackground =
    (activePage === "earn" &&
      isEarnRootView &&
      earnData.hasPosition &&
      !isPositionLoading) ||
    (EARN_MAX_VISIBLE &&
      activePage === "earnmax" &&
      isSignedIn &&
      !earnMax.view.isLoading &&
      !isEarnMaxUntouched(earnMax.view));

  if (!isHydrated) return <WorkspaceSkeleton />;

  return (
    <BalanceVisibilityProvider>
      {/* facelift-shell marks the workspace for globals.css's sign-in modal
          optical centering (the shell's side rails are asymmetric). */}
      <div className="cherry-mobile-workspace facelift-shell flex h-dvh w-full overflow-clip bg-muted text-foreground">
        <HiddenBalanceFilterDefs />
        <WalletReconnectPrompt
          expectedWalletAddress={earnData.actions.authenticatedWalletAddress}
          onClose={earnData.actions.closeReconnectPrompt}
          onReady={earnData.actions.closeReconnectPrompt}
          open={earnData.actions.isReconnectPromptOpen}
        />
        <EarnApprovalSheet approval={earnData.actions.pendingApproval} />
        <EarnToastHost />
        <SettingsSheet
          isOpen={isSettingsOpen}
          onClose={() => setIsSettingsOpen(false)}
        />
        <LifecycleOnboardingHost
          hasAutodeposit={earnData.autodepositConfig !== null}
          hasPosition={earnData.hasPosition}
          isAtEarnRoot={
            activePage === "earn" &&
            activeMiddleView === "earn" &&
            !isChartExpanded
          }
          isReady={isSignedIn && !isPositionLoading}
          onOpenAutodeposit={() => {
            setActivePage("earn");
            setMiddleView("autodeposit");
          }}
          onOpenDeposit={() => {
            setDepositSourceKey(null);
            setActivePage("earn");
            setMiddleView("deposit");
          }}
          walletAddress={earnData.walletAddress ?? null}
        />
        <FaceliftSidebar
          activePage={activePage}
          earnBalanceUsd={earnData.earnBalanceUsd}
          earnMaxBalanceUsd={earnMax.view.balanceUsd}
          earnMaxForecastApyBps={earnMax.view.forecastApyBps}
          flashedShortcut={flashedShortcut}
          isEarnBalanceLoading={isPositionLoading}
          isEarnMaxBalanceLoading={earnMax.view.isLoading}
          onSelectPage={handleSelectPage}
          onUnseenActivityChange={setHasUnseenActivity}
          showEarnMax={EARN_MAX_VISIBLE}
        />
        <div
          className={`flex h-full min-w-0 flex-1 flex-col ${
            isMobileGrayBackground ? "" : "max-[795px]:bg-card"
          }`}
        >
          {activePage === "wallet" ? (
            <WalletHomePage
              earnBalanceUsd={earnData.earnBalanceUsd}
              earnEarned30dUsd={earnEarned30dUsd}
              earnMaxBalanceUsd={earnMax.view.balanceUsd}
              earnMaxEarned30dUsd={earnMaxEarned30dUsd}
              earnMaxForecastApyBps={earnMax.view.forecastApyBps}
              isEarnBalanceLoading={isPositionLoading}
              isEarnMaxBalanceLoading={earnMax.view.isLoading}
              onSelectPage={handleSelectPage}
              onSetUpAutodeposit={() => {
                setActivePage("earn");
                setMiddleView("autodeposit");
              }}
              onOpenSettings={() => setIsSettingsOpen(true)}
              showActivityBadge={hasUnseenActivity}
              showEarnMax={EARN_MAX_VISIBLE}
            />
          ) : activePage === "activity" ? (
            <ActivityPage
              earnMaxActivity={earnMax.view.activity}
              onSelectPage={handleSelectPage}
              refreshKey={earnData.actions.earnTransactionsRefreshKey}
              settingsPda={earnData.settingsPda}
              walletAddress={earnData.walletAddress}
            />
          ) : EARN_MAX_VISIBLE && activePage === "earnmax" ? (
            <EarnMaxWorkspace
              earnData={earnData}
              earnMax={earnMax}
              onBack={() => handleSelectPage("wallet")}
              onOpenSettings={() => setIsSettingsOpen(true)}
              onViewAllActivity={() => handleSelectPage("activity")}
            />
          ) : activePage !== "earn" && activePage !== "earnmax" ? (
            <CryptoPage
              navigationNonce={navigationNonce}
              onBack={() => handleSelectPage("wallet")}
              onEarn={(sourceMint) => {
                setDepositSourceKey(sourceMint ?? null);
                setActivePage("earn");
                setMiddleView("deposit");
              }}
              page={activePage}
            />
          ) : (
            <>
              <div className="flex h-full min-h-0 min-w-0 flex-1 gap-2 p-2 max-[795px]:gap-0 max-[795px]:p-0">
                <MiddlePaneSlide
                  actionPane={
                    activeMiddleView === "withdraw" ? (
                      <WithdrawPane
                        data={earnData}
                        initialSourceKey={withdrawSourceKey}
                        onBack={() => setMiddleView("earn")}
                      />
                    ) : activeMiddleView === "autodeposit" ? (
                      <AutodepositPane
                        data={earnData}
                        onBack={() => setMiddleView("earn")}
                      />
                    ) : activeMiddleView === "autoswap" ? (
                      <AutoswapPane
                        data={earnData}
                        onBack={() => setMiddleView("earn")}
                      />
                    ) : activeMiddleView === "deposit" ? (
                      <DepositPane
                        data={earnData}
                        initialSourceKey={depositSourceKey}
                        onBack={() => setMiddleView("earn")}
                        onOpenChart={() => setIsChartExpanded(true)}
                      />
                    ) : null
                  }
                >
                  {isPositionLoading ? (
                    <WorkspacePaneSkeleton />
                  ) : earnData.hasPosition ? (
                    <PaneReveal>
                      <EarnPositionPane
                        data={earnData}
                        onBack={() => handleSelectPage("wallet")}
                        onDeposit={() => {
                          setDepositSourceKey(null);
                          setMiddleView("deposit");
                        }}
                        onOpenAutodeposit={() => setMiddleView("autodeposit")}
                        onOpenAutoswap={() => setMiddleView("autoswap")}
                        onOpenChart={() => setIsChartExpanded(true)}
                        onSelectChartTab={setChartTab}
                        onSelectTransaction={setSelectedEarnTransaction}
                        onViewAllActivity={() => handleSelectPage("activity")}
                        onWithdraw={(sourceKey) => {
                          setWithdrawSourceKey(sourceKey ?? null);
                          setMiddleView("withdraw");
                        }}
                        selectedChartTab={chartTab}
                        selectedTransactionId={
                          selectedEarnTransaction?.id ?? null
                        }
                      />
                    </PaneReveal>
                  ) : (
                    <PaneReveal>
                      <EarnEmptyPane
                        onBack={() => handleSelectPage("wallet")}
                        onDeposit={() => {
                          setDepositSourceKey(null);
                          setMiddleView("deposit");
                        }}
                        onOpenSettings={() => setIsSettingsOpen(true)}
                        onManageAutoswap={
                          earnData.autoswapConfig && !AUTOSWAP_HIDDEN
                            ? () => setMiddleView("autoswap")
                            : undefined
                        }
                        onOpenChart={() => setIsChartExpanded(true)}
                      />
                    </PaneReveal>
                  )}
                </MiddlePaneSlide>
                <EarnChartPane
                  banner={
                    <EarnBanners
                      onSetUpAutodeposit={() => setMiddleView("autodeposit")}
                    />
                  }
                  earnData={earnData}
                  isExpanded={isChartExpanded}
                  isLoggedOut={isHydrated && !isSignedIn}
                  onExpandedChange={setIsChartExpanded}
                  onSelectTab={setChartTab}
                  selectedTab={chartTab}
                  statsPanel={
                    <>
                      <EarnExplainerCard />
                      <EarnStatsPanel />
                    </>
                  }
                />
              </div>
              {earnData.hasPosition ? (
                <SheetReveal
                  isOpen={selectedEarnTransaction !== null && isEarnRootView}
                  onClose={() => setSelectedEarnTransaction(null)}
                  scrimClassName="fixed inset-0 z-50 flex bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8"
                  sheetClassName="ml-auto flex h-full w-[400px] min-w-0 flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
                >
                  {earnTransactionDetail ? (
                    <EarnTransactionDetailPane
                      item={earnTransactionDetail}
                      key={earnTransactionDetail.id}
                      onClose={() => setSelectedEarnTransaction(null)}
                      walletAddress={earnData.walletAddress}
                    />
                  ) : null}
                </SheetReveal>
              ) : null}
            </>
          )}
        </div>
      </div>
    </BalanceVisibilityProvider>
  );
}
