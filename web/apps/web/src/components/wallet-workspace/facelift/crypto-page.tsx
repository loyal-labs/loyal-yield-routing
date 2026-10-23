"use client";

import { resolveLoyalClusterForSolanaEnv } from "@loyal-labs/actions";
import { resolveSolanaEnv } from "@loyal-labs/solana-rpc";
import { useUnshield } from "@loyal-labs/wallet-core/hooks";
import type { WalletSigner } from "@loyal-labs/wallet-core/types";
import { useWallet } from "@solana/wallet-adapter-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  LOYL_TOKEN,
  swapTokens as fallbackSwapTokens,
} from "@/components/wallet-sidebar/types";
import type {
  SubView,
  SwapToken,
  TokenRow,
} from "@/components/wallet-sidebar/types";
import {
  CryptoPane,
  type CryptoPaneVariant,
  type CryptoRowActions,
} from "@/components/wallet-workspace/facelift/crypto-pane";
import { deriveSwapTokens, selectSwapToken } from "@/components/wallet-sidebar/swap-tokens";
import { useEscapePage } from "@/components/wallet-workspace/facelift/escape-owner";
import {
  MiddlePaneSlide,
  PaneReveal,
} from "@/components/wallet-workspace/facelift/pane-transitions";
import {
  SendPane,
  SendRecipientPane,
  useSendFormActive,
  useSendRecentRecipients,
} from "@/components/wallet-workspace/facelift/send-pane";
import {
  InlineSheetReveal,
  SheetReveal,
} from "@/components/wallet-workspace/facelift/sheet-reveal";
import {
  buildSwapTargetTokens,
  SwapPane,
  SwapTokenSelectPane,
} from "@/components/wallet-workspace/facelift/swap-pane";
import { TokenDetailPane } from "@/components/wallet-workspace/facelift/token-detail-pane";
import { useAbandonActionsOnNavigate } from "@/components/wallet-workspace/facelift/use-abandon-actions-on-navigate";
import { useStablecoinsUsd } from "@/components/wallet-workspace/facelift/use-stablecoins-usd";
import {
  type ShieldedRow,
  toShieldedRow,
  UnshieldPane,
} from "@/components/wallet-workspace/facelift/unshield-pane";
import { useAuthCapability } from "@/lib/auth/capability";
import { usePublicEnv } from "@/contexts/public-env-context";
import { usePopularTokens } from "@/hooks/use-popular-tokens";
import {
  splitUsdBalance,
  useWalletDesktopData,
} from "@/hooks/use-wallet-desktop-data";
import {
  getStablecoinMintSetForSolanaEnv,
  isStablecoinMint,
} from "@/lib/wallet/stablecoin-classification";
import { getEarnProductAssetsForCluster } from "@loyal-labs/smart-account-vaults";

type ActionView = Exclude<SubView, null>;

function viewType(view: SubView) {
  return typeof view === "object" && view !== null ? view.type : view;
}

function tokenRowToSwapToken(token: TokenRow): SwapToken {
  return {
    balance: Number.parseFloat(token.amount.replace(/,/g, "")) || 0,
    icon: token.icon,
    mint: token.id,
    price: Number.parseFloat(token.price.replace(/[$,]/g, "")) || 0,
    symbol: token.symbol,
  };
}

export function CryptoPage({
  navigationNonce,
  onBack,
  onEarn,
  page,
}: {
  navigationNonce: number;
  onBack: () => void;
  onEarn: (sourceMint?: string | null) => void;
  page: CryptoPaneVariant;
}) {
  const publicEnv = usePublicEnv();
  const { isHydrated, isSignedIn } = useAuthCapability();

  const data = useWalletDesktopData({});
  const swapVisibleBalances = useMemo(
    () => ({
      owner: data.walletAddress,
      balances: data.positions.map((position) => ({
        mint: position.asset.mint,
        amount: position.publicBalance,
      })),
    }),
    [data.walletAddress, data.positions]
  );

  const wallet = useWallet();
  const unshieldSigner = useMemo<WalletSigner | null>(
    () =>
      wallet.publicKey && wallet.signTransaction && wallet.signAllTransactions
        ? {
            publicKey: wallet.publicKey,
            signTransaction: wallet.signTransaction,
            signAllTransactions: wallet.signAllTransactions,
            signMessage: wallet.signMessage,
          }
        : null,
    [
      wallet.publicKey,
      wallet.signAllTransactions,
      wallet.signMessage,
      wallet.signTransaction,
    ]
  );
  const { balances: shieldedBalances, executeUnshield } = useUnshield(
    unshieldSigner,
    publicEnv.solanaEnv
  );
  const [isUnshieldOpen, setIsUnshieldOpen] = useState(false);
  const [unshieldMint, setUnshieldMint] = useState<string | undefined>();

  const stablecoinMints = useMemo(
    () => getStablecoinMintSetForSolanaEnv(publicEnv.solanaEnv),
    [publicEnv.solanaEnv]
  );
  const shieldedRows = useMemo<ShieldedRow[]>(
    () =>
      shieldedBalances
        .map((balance) => toShieldedRow(balance, data.positions))
        .filter(
          (row) =>
            isStablecoinMint(row.mint, stablecoinMints) === (page === "stables")
        ),
    [data.positions, page, shieldedBalances, stablecoinMints]
  );
  const stablecoinsUsd = useStablecoinsUsd(data.positions);
  const pageBalance = splitUsdBalance(
    page === "stables"
      ? stablecoinsUsd
      : Math.max(data.totalUsd - stablecoinsUsd, 0)
  );
  const isWalletDataRevealed =
    isHydrated &&
    (!isSignedIn || (data.walletAddress !== null && !data.isLoading));

  const [viewStack, setViewStack] = useState<ActionView[]>([]);
  const [sendToken, setSendToken] = useState<SwapToken>(fallbackSwapTokens[0]);
  const [swapFromToken, setSwapFromToken] = useState<SwapToken>(
    fallbackSwapTokens[0]
  );
  const [swapToToken, setSwapToToken] = useState<SwapToken>(LOYL_TOKEN);
  const [shouldLoadPopularTokens, setShouldLoadPopularTokens] = useState(false);
  const { tokens: popularTokens } = usePopularTokens({
    enabled: shouldLoadPopularTokens,
  });

  const [sendRecipient, setSendRecipient] = useState<string | null>(null);
  const [sendSelect, setSendSelect] = useState<"asset" | "recipient" | null>(
    null
  );
  const sendSelectRef = useRef<"asset" | "recipient">("recipient");
  if (sendSelect) {
    sendSelectRef.current = sendSelect;
  }
  const overlaySendSelect = sendSelect ?? sendSelectRef.current;
  const closeSendSelect = useCallback(() => setSendSelect(null), []);
  const {
    handleFormActiveChange: handleSendFormActiveChange,
    isFormActive: isSendFormActive,
    reopenForm: reopenSendForm,
  } = useSendFormActive(closeSendSelect);
  const { recents: sendRecents, recordSend: recordSendRecent } =
    useSendRecentRecipients(data.walletAddress);

  const [isSwapOpen, setIsSwapOpen] = useState(false);
  const [swapSelectSide, setSwapSelectSide] = useState<"from" | "to" | null>(
    null
  );
  const swapSelectSideRef = useRef<"from" | "to">("to");
  if (swapSelectSide) {
    swapSelectSideRef.current = swapSelectSide;
  }
  const overlaySelectSide = swapSelectSide ?? swapSelectSideRef.current;

  const [detailToken, setDetailToken] = useState<SwapToken | null>(null);
  const [isDetailSheetOpen, setIsDetailSheetOpen] = useState(false);
  useEffect(() => {
    setDetailToken(null);
    setIsDetailSheetOpen(false);
  }, [page]);

  const derivedTokens = useMemo(
    () => deriveSwapTokens(data.positions),
    [data.positions]
  );
  const earnProductAssets = useMemo(
    () =>
      getEarnProductAssetsForCluster(
        resolveLoyalClusterForSolanaEnv(resolveSolanaEnv(publicEnv.solanaEnv))
      ).map((asset) => ({
        mint: asset.mint.toBase58(),
        symbol: asset.symbol,
      })),
    [publicEnv.solanaEnv]
  );
  const earnEligibleMints = useMemo(
    () => new Set(earnProductAssets.map((asset) => asset.mint)),
    [earnProductAssets]
  );
  const swapTargetTokens = useMemo<SwapToken[]>(
    () =>
      buildSwapTargetTokens({
        derivedTokens,
        earnEligibleMints,
        earnProductAssets,
        popularTokens,
      }),
    [derivedTokens, earnEligibleMints, earnProductAssets, popularTokens]
  );

  const prevHadTokensRef = useRef(false);
  useEffect(() => {
    const firstToken = derivedTokens[0];
    const hasTokens = derivedTokens.length > 0 && !!firstToken?.mint;

    if (hasTokens && !prevHadTokensRef.current && firstToken) {
      setSendToken(firstToken);
      setSwapFromToken(firstToken);
      setSwapToToken(
        derivedTokens.find((token) => token.mint === LOYL_TOKEN.mint) ??
          LOYL_TOKEN
      );
    }

    prevHadTokensRef.current = hasTokens;
  }, [derivedTokens]);

  const closeAction = useCallback(() => {
    setViewStack([]);
  }, []);

  const openAction = useCallback((view: ActionView) => {
    setViewStack([view]);
  }, []);

  const openSend = useCallback(
    (token?: SwapToken) => {
      if (token) {
        setSendToken(token);
      }
      setSendRecipient(null);
      setSendSelect(null);
      reopenSendForm();
      setIsUnshieldOpen(false);
      openAction({ type: "sendPanel" });
    },
    [openAction, reopenSendForm]
  );

  const swapFromTokenRef = useRef(swapFromToken);
  swapFromTokenRef.current = swapFromToken;
  const swapToTokenRef = useRef(swapToToken);
  swapToTokenRef.current = swapToToken;

  const openSwap = useCallback((from?: SwapToken) => {
    setShouldLoadPopularTokens(true);
    if (from) {
      setSwapFromToken(from);
      setSwapToToken((current) =>
        current.symbol === from.symbol ? swapFromTokenRef.current : current
      );
    }
    setViewStack([]);
    setSwapSelectSide(null);
    setIsUnshieldOpen(false);
    setIsSwapOpen(true);
  }, []);

  const closeSwap = useCallback(() => {
    setIsSwapOpen(false);
    setSwapSelectSide(null);
  }, []);

  const openUnshield = useCallback(
    (mint?: string) => {
      setViewStack([]);
      closeSwap();
      setUnshieldMint(mint);
      setIsUnshieldOpen(true);
    },
    [closeSwap]
  );
  const closeUnshield = useCallback(() => setIsUnshieldOpen(false), []);

  useAbandonActionsOnNavigate(navigationNonce, () => {
    setViewStack([]);
    setSendSelect(null);
    closeSwap();
    closeUnshield();
  });

  useEscapePage(
    viewStack.length > 0 || isSwapOpen || isUnshieldOpen,
    () => {
      if (sendSelect !== null) {
        setSendSelect(null);
        return;
      }
      if (swapSelectSide !== null) {
        setSwapSelectSide(null);
        return;
      }
      setViewStack([]);
      closeSwap();
      closeUnshield();
    }
  );

  const handleSwapTokenSelect = useCallback(
    (side: "from" | "to", token: SwapToken) => {
      const next = selectSwapToken(side, token, {
        from: swapFromTokenRef.current,
        to: swapToTokenRef.current,
      });
      setSwapFromToken(next.from);
      setSwapToToken(next.to);
      setSwapSelectSide(null);
    },
    []
  );

  const tokenNameByMint = useMemo(() => {
    const map: Record<string, string> = {};
    for (const position of data.positions) {
      map[position.asset.mint] = position.asset.name;
    }
    return map;
  }, [data.positions]);

  // Wrapped so flow callbacks' own arguments never leak into refresh's
  // isCurrent parameter.
  const refreshWalletData = data.refresh;
  const refreshWallet = useCallback(
    () => refreshWalletData(),
    [refreshWalletData]
  );

  const pageTokenRows =
    page === "stables" ? data.cashTokenRows : data.investmentTokenRows;
  const firstPageRow = pageTokenRows[0];
  const detailDefault =
    (page === "stables"
      ? derivedTokens.find((token) => token.symbol === "USDC")
      : derivedTokens.find((token) => token.mint === LOYL_TOKEN.mint)) ??
    (firstPageRow ? tokenRowToSwapToken(firstPageRow) : null);
  const detailBase = detailToken ?? detailDefault;
  const detailMint = detailBase?.mint ?? null;
  const detailPosition = detailMint
    ? data.positions.find((position) => position.asset.mint === detailMint) ??
      null
    : null;
  const detailPublicBalance =
    detailPosition?.publicBalance ?? detailBase?.balance ?? 0;
  const detailPrice = detailPosition?.priceUsd ?? detailBase?.price ?? 0;
  const openDetailSwap = () => {
    if (!detailBase) {
      return;
    }
    setIsDetailSheetOpen(false);
    openSwap({ ...detailBase, balance: detailPublicBalance });
  };
  const openDetailSend = () => {
    if (!detailBase) {
      return;
    }
    setIsDetailSheetOpen(false);
    openSend({
      ...detailBase,
      balance: detailPublicBalance,
    });
  };

  const selectRowDetail = (row: TokenRow) =>
    setDetailToken(tokenRowToSwapToken(row));

  const rowActions: CryptoRowActions = {
    onEarn: (row) => {
      selectRowDetail(row);
      onEarn(row.id);
    },
    onSelect: (row) => {
      setDetailToken(tokenRowToSwapToken(row));
      setIsDetailSheetOpen(true);
    },
    onSend: (row) => {
      selectRowDetail(row);
      openSend(tokenRowToSwapToken(row));
    },
    onSwap: (row) => {
      selectRowDetail(row);
      const base =
        derivedTokens.find((token) => token.mint === row.id) ??
        tokenRowToSwapToken(row);
      openSwap(base);
    },
  };

  const actionView = viewStack[viewStack.length - 1] ?? null;
  const actionType = actionView === null ? null : viewType(actionView);
  const isSendOpen = actionType === "sendPanel";

  const renderSendSelectPane = () =>
    overlaySendSelect === "asset" ? (
      <SwapTokenSelectPane
        nameByMint={tokenNameByMint}
        onClose={() => setSendSelect(null)}
        onSelect={(token) => {
          setSendToken(token);
          setSendSelect(null);
        }}
        side="from"
        title="Select asset"
        tokens={derivedTokens}
      />
    ) : (
      <SendRecipientPane
        onClose={() => setSendSelect(null)}
        onSelect={(address) => {
          setSendRecipient(address);
          setSendSelect(null);
        }}
        recents={sendRecents}
      />
    );

  return (
    <>
      <div className="flex h-full min-h-0 min-w-0 flex-1 gap-2 p-2 max-[795px]:gap-0 max-[795px]:p-0">
        <MiddlePaneSlide
          actionPane={
            isSendOpen ? (
              <SendPane
                onBack={closeAction}
                onDone={closeAction}
                onFormActiveChange={handleSendFormActiveChange}
                onOpenAssetSelect={() =>
                  setSendSelect((current) =>
                    current === "asset" ? null : "asset"
                  )
                }
                onOpenRecipientSelect={() =>
                  setSendSelect((current) =>
                    current === "recipient" ? null : "recipient"
                  )
                }
                onSuccess={refreshWallet}
                recipient={sendRecipient}
                recordRecent={recordSendRecent}
                token={sendToken}
              />
            ) : isSwapOpen ? (
              <SwapPane
                visibleBalances={swapVisibleBalances}
                activeSelectSide={swapSelectSide}
                fromToken={swapFromToken}
                onBack={closeSwap}
                onDone={closeSwap}
                onFlipTokens={() => {
                  setSwapFromToken(swapToToken);
                  setSwapToToken(swapFromToken);
                }}
                onRequestSelect={(side) =>
                  setSwapSelectSide((current) =>
                    current === side ? null : side
                  )
                }
                onSuccess={refreshWallet}
                toToken={swapToToken}
              />
            ) : isUnshieldOpen ? (
              <UnshieldPane
                executeUnshield={executeUnshield}
                initialMint={unshieldMint}
                onBack={closeUnshield}
                onDone={closeUnshield}
                onSuccess={refreshWallet}
                rows={shieldedBalances.map((balance) =>
                  toShieldedRow(balance, data.positions)
                )}
              />
            ) : null
          }
        >
          <PaneReveal>
            <CryptoPane
              balanceFraction={pageBalance.balanceFraction}
              balanceWhole={pageBalance.balanceWhole}
              isBalanceRevealed={isWalletDataRevealed}
              onBack={onBack}
              onEarn={() => onEarn()}
              onSend={() => openSend()}
              onSwap={() => openSwap()}
              onUnshield={
                shieldedBalances.length > 0 ? openUnshield : undefined
              }
              rowActions={rowActions}
              shieldedRows={shieldedRows}
              tokenRows={
                page === "stables"
                  ? data.cashTokenRows
                  : data.investmentTokenRows
              }
              variant={page}
            />
          </PaneReveal>
        </MiddlePaneSlide>
        {isSendOpen ? (
          <div className="hidden h-full w-[400px] shrink-0 min-[1204px]:block">
            <InlineSheetReveal
              className="flex h-full w-full min-w-0 flex-col overflow-clip rounded-3xl bg-card"
              isOpen={sendSelect !== null && isSendFormActive}
            >
              <PaneReveal key={overlaySendSelect}>
                {renderSendSelectPane()}
              </PaneReveal>
            </InlineSheetReveal>
          </div>
        ) : actionView !== null ? null : isUnshieldOpen ? (
          <aside className="hidden h-full w-[400px] shrink-0 rounded-3xl bg-card min-[1204px]:block" />
        ) : isSwapOpen ? (
          <div className="hidden h-full w-[400px] shrink-0 min-[1204px]:block">
            <InlineSheetReveal
              className="flex h-full w-full min-w-0 flex-col overflow-clip rounded-3xl bg-card"
              isOpen={swapSelectSide !== null}
            >
              <PaneReveal key={overlaySelectSide}>
                <SwapTokenSelectPane
                  earnEligibleMints={earnEligibleMints}
                  nameByMint={tokenNameByMint}
                  onClose={() => setSwapSelectSide(null)}
                  remoteSearch={overlaySelectSide === "to"}
                  onSelect={(token) =>
                    handleSwapTokenSelect(overlaySelectSide, token)
                  }
                  side={overlaySelectSide}
                  tokens={
                    overlaySelectSide === "from"
                      ? derivedTokens
                      : swapTargetTokens
                  }
                />
              </PaneReveal>
            </InlineSheetReveal>
          </div>
        ) : detailBase && detailMint ? (
          <aside className="hidden h-full w-[400px] shrink-0 min-[1204px]:block">
            <PaneReveal key={detailMint}>
              <div className="flex h-full w-full min-w-0 flex-col overflow-clip rounded-3xl bg-card">
                <TokenDetailPane
                  hideChart={page === "stables"}
                  icon={detailBase.icon}
                  mint={detailMint}
                  name={detailPosition?.asset.name ?? detailBase.symbol}
                  onSend={openDetailSend}
                  onSwap={openDetailSwap}
                  price={detailPrice}
                  publicBalance={detailPublicBalance}
                  symbol={detailBase.symbol}
                />
              </div>
            </PaneReveal>
          </aside>
        ) : (
          <aside className="hidden h-full w-[400px] shrink-0 rounded-3xl bg-card min-[1204px]:block" />
        )}
      </div>
      <SheetReveal
        isOpen={isSwapOpen && swapSelectSide !== null}
        onClose={() => setSwapSelectSide(null)}
        scrimClassName="fixed inset-0 z-50 flex bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8 min-[1204px]:hidden"
        sheetClassName="ml-auto flex h-full w-[392px] min-w-0 max-w-full flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
      >
        <PaneReveal key={overlaySelectSide}>
          <SwapTokenSelectPane
            earnEligibleMints={earnEligibleMints}
            nameByMint={tokenNameByMint}
            onClose={() => setSwapSelectSide(null)}
            remoteSearch={overlaySelectSide === "to"}
            onSelect={(token) =>
              handleSwapTokenSelect(overlaySelectSide, token)
            }
            side={overlaySelectSide}
            tokens={
              overlaySelectSide === "from" ? derivedTokens : swapTargetTokens
            }
          />
        </PaneReveal>
      </SheetReveal>
      <SheetReveal
        isOpen={isSendOpen && sendSelect !== null && isSendFormActive}
        onClose={() => setSendSelect(null)}
        scrimClassName="fixed inset-0 z-50 flex bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8 min-[1204px]:hidden"
        sheetClassName="ml-auto flex h-full w-[392px] min-w-0 max-w-full flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
      >
        <PaneReveal key={overlaySendSelect}>
          {renderSendSelectPane()}
        </PaneReveal>
      </SheetReveal>
      {detailBase && detailMint ? (
        <SheetReveal
          isOpen={isDetailSheetOpen}
          onClose={() => setIsDetailSheetOpen(false)}
          scrimClassName="fixed inset-0 z-50 flex bg-black/20 p-2 backdrop-blur-[4px] max-[795px]:bg-white/60 max-[795px]:p-0 max-[795px]:pt-8 min-[1204px]:hidden"
          sheetClassName="ml-auto flex h-full w-[392px] min-w-0 max-w-full flex-col overflow-clip rounded-3xl bg-card max-[795px]:w-full max-[795px]:rounded-b-none max-[795px]:shadow-[0px_-10px_40px_-10px_rgba(0,0,0,0.2)]"
        >
          <TokenDetailPane
            hideChart={page === "stables"}
            icon={detailBase.icon}
            mint={detailMint}
            name={detailPosition?.asset.name ?? detailBase.symbol}
            onClose={() => setIsDetailSheetOpen(false)}
            onSend={openDetailSend}
            onSwap={openDetailSwap}
            price={detailPrice}
            publicBalance={detailPublicBalance}
            symbol={detailBase.symbol}
          />
        </SheetReveal>
      ) : null}
    </>
  );
}
