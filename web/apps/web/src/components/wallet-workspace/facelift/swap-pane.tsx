"use client";

import { Search } from "lucide-react";
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
} from "react";

import { sanitizeBucksAmountInput } from "@/components/wallet-sidebar/earn-detail-view";
import { SWAP_QUOTE_DEBOUNCE_MS } from "@/components/wallet-sidebar/swap-content";
import {
  TOKEN_SEARCH_DEBOUNCE_MS,
  TOKEN_SEARCH_MIN_LENGTH,
} from "@/components/wallet-sidebar/token-select-view";
import type { SwapToken } from "@/components/wallet-sidebar/types";
import {
  ActionErrorBody,
  ActionProcessingBody,
  ActionSuccessBody,
  formatTokenAmount,
  formatUsdAmount,
  SOL_FEE_RESERVE,
} from "@/components/wallet-workspace/facelift/action-screens";
import {
  ScrambleText,
  useBalanceVisibility,
} from "@/components/wallet-workspace/facelift/balance-visibility";
import { PaneReveal } from "@/components/wallet-workspace/facelift/pane-transitions";
import { TextSwap } from "@/components/wallet-workspace/facelift/text-swap";
import { ThemedIcon } from "@/components/wallet-workspace/facelift/themed-icon";
import type { SwapBalanceSnapshot } from "@/features/observability/swap-visibility";
import { usePublicEnv } from "@/contexts/public-env-context";
import { useSwap } from "@/hooks/use-swap";
import { splitUsdBalance } from "@/hooks/use-wallet-desktop-data";
import { trackWalletSwapPressed } from "@/lib/core/analytics";
import {
  getCachedTrendingTokens,
  prefetchTrendingTokens,
  rankSearchResults,
  searchTokens,
  type TokenSearchResult,
} from "@/lib/market/token-search.client";
import { getTokenIconUrl } from "@/lib/token-icon";

const ASSET_BASE = "/wallet-workspace/facelift";

// $63.83 / $0.99866 — two decimals above a dollar, significant digits below.
function formatTokenPrice(price: number): string {
  if (price >= 1) {
    return formatUsdAmount(price);
  }
  return price.toLocaleString("en-US", { maximumSignificantDigits: 5 });
}

function TokenIconSwap({ src }: { src: string }) {
  const [slots, setSlots] = useState<{
    a: string;
    b: string;
    active: "a" | "b";
  }>({ a: src, b: src, active: "a" });
  const activeSrc = slots.active === "a" ? slots.a : slots.b;
  if (src !== activeSrc) {
    setSlots((current) =>
      current.active === "a"
        ? { ...current, b: src, active: "b" }
        : { ...current, a: src, active: "a" }
    );
  }
  return (
    <span className="t-icon-swap size-9 shrink-0" data-state={slots.active}>
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img
        alt=""
        aria-hidden="true"
        className="t-icon size-9 rounded-full object-cover"
        data-icon="a"
        src={slots.a}
      />
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img
        alt=""
        aria-hidden="true"
        className="t-icon size-9 rounded-full object-cover"
        data-icon="b"
        src={slots.b}
      />
    </span>
  );
}

function SwapTokenPill({
  isActive,
  onClick,
  token,
}: {
  isActive: boolean;
  onClick: () => void;
  token: SwapToken;
}) {
  return (
    <button
      className={`t-hover flex shrink-0 items-center rounded-full py-1 pl-1 pr-2.5 ${
        isActive ? "bg-accent-active" : "bg-accent hover:bg-accent-active"
      }`}
      onClick={onClick}
      type="button"
    >
      <TokenIconSwap src={token.icon || getTokenIconUrl(token.symbol)} />
      <span className="px-2 font-medium text-[16px] text-foreground leading-5">
        <TextSwap text={token.symbol} />
      </span>
      <ThemedIcon
        className="size-6 text-tertiary"
        src={`${ASSET_BASE}/icon-chevron-right.svg`}
      />
    </button>
  );
}

// Swap screen (Figma 4819:414629 wide / 4813:404479 downsized): the middle
// pane form. Quote/execute logic is the OG SwapContent orchestration ported
// onto useSwap — debounced Jupiter quote, price-ratio fallback, MAX with the
// SOL fee reserve, flip keeps the USD value. Confirm walks the step screens:
// processing (4813:405472) → success (4813:406464) or error (4813:407564).
export function SwapPane({
  activeSelectSide,
  fromToken,
  onBack,
  onDone,
  onFlipTokens,
  onRequestSelect,
  onSuccess,
  visibleBalances,
  toToken,
}: {
  activeSelectSide: "from" | "to" | null;
  fromToken: SwapToken;
  onBack: () => void;
  onDone: () => void;
  onFlipTokens: () => void;
  onRequestSelect: (side: "from" | "to") => void;
  visibleBalances: SwapBalanceSnapshot;
  onSuccess: () => void;
  toToken: SwapToken;
}) {
  const publicEnv = usePublicEnv();
  const { isBalanceHidden } = useBalanceVisibility();
  const {
    getQuote,
    executeSwap,
    resetQuote,
    quote,
    isAvailable,
    unavailableReason,
    error: swapError,
  } = useSwap(visibleBalances);

  const [amountInput, setAmountInput] = useState("");
  const [isUsdInput, setIsUsdInput] = useState(false);
  const [isQuoting, setIsQuoting] = useState(false);
  const [isSwapping, setIsSwapping] = useState(false);
  const [errorMessage, setErrorMessage] = useState<string | null>(null);
  const [step, setStep] = useState<"form" | "processing" | "success" | "error">(
    "form"
  );
  const [txSignature, setTxSignature] = useState<string | null>(null);
  const quoteTimerRef = useRef<number | null>(null);

  const numericInput = Number.parseFloat(amountInput.replace(/,/g, "")) || 0;
  const tokenAmount = isUsdInput
    ? fromToken.price > 0
      ? numericInput / fromToken.price
      : 0
    : numericInput;
  const fromUsd = tokenAmount * fromToken.price;
  const hasAmount = tokenAmount > 0;
  const insufficientFunds = tokenAmount > fromToken.balance;

  useEffect(() => {
    if (quoteTimerRef.current !== null) {
      window.clearTimeout(quoteTimerRef.current);
    }
    if (!hasAmount || insufficientFunds || isSwapping) {
      resetQuote();
      setIsQuoting(false);
      return;
    }
    setIsQuoting(true);
    quoteTimerRef.current = window.setTimeout(() => {
      void getQuote(
        fromToken.symbol,
        toToken.symbol,
        String(tokenAmount),
        fromToken.mint,
        undefined,
        undefined,
        toToken.mint
      ).finally(() => setIsQuoting(false));
    }, SWAP_QUOTE_DEBOUNCE_MS);
    return () => {
      if (quoteTimerRef.current !== null) {
        window.clearTimeout(quoteTimerRef.current);
      }
    };
  }, [
    fromToken.mint,
    fromToken.symbol,
    getQuote,
    hasAmount,
    insufficientFunds,
    isSwapping,
    resetQuote,
    toToken.mint,
    toToken.symbol,
    tokenAmount,
  ]);

  const toAmount = useMemo(() => {
    if (!hasAmount) {
      return 0;
    }
    if (quote?.outputAmount) {
      return Number.parseFloat(quote.outputAmount);
    }
    if (toToken.price <= 0) {
      return 0;
    }
    return (tokenAmount * fromToken.price) / toToken.price;
  }, [hasAmount, quote, tokenAmount, fromToken.price, toToken.price]);
  const toUsd = toAmount * toToken.price;

  const rate =
    toAmount > 0 && hasAmount
      ? tokenAmount / toAmount
      : fromToken.price > 0
      ? toToken.price / fromToken.price
      : 0;

  const handleAmountChange = (rawValue: string) => {
    const sanitized = sanitizeBucksAmountInput(rawValue, amountInput);
    if (sanitized !== null) {
      setAmountInput(sanitized);
      setErrorMessage(null);
    }
  };

  const handleToggleCurrency = () => {
    if (numericInput > 0 && fromToken.price > 0) {
      setAmountInput(
        isUsdInput
          ? String(Number(tokenAmount.toFixed(6)))
          : String(Number(fromUsd.toFixed(2)))
      );
    }
    setIsUsdInput(!isUsdInput);
  };

  const handleMax = () => {
    let value = fromToken.balance;
    if (fromToken.symbol.toUpperCase() === "SOL") {
      value = Math.max(0, value - SOL_FEE_RESERVE);
    }
    if (value <= 0) {
      setAmountInput("");
      return;
    }
    setAmountInput(
      isUsdInput
        ? String(Number((value * fromToken.price).toFixed(2)))
        : String(Number(value.toFixed(6)))
    );
  };

  const handleFlip = () => {
    if (!isUsdInput && tokenAmount > 0) {
      setAmountInput(
        toToken.price > 0
          ? String(Number((fromUsd / toToken.price).toFixed(6)))
          : ""
      );
    }
    onFlipTokens();
  };

  const handleConfirm = async () => {
    if (!quote || isSwapping) {
      return;
    }
    const completedAmount = Number(toAmount.toFixed(6)).toString();
    const completedUsd = `$${formatUsdAmount(toUsd)}`;
    trackWalletSwapPressed(publicEnv, {
      source: "swap_confirm",
      interaction: "confirm",
      from_symbol: fromToken.symbol,
      from_mint: fromToken.mint,
      to_symbol: toToken.symbol,
      to_mint: toToken.mint,
      from_amount: String(tokenAmount),
      to_amount: completedAmount,
      usd_value: completedUsd,
    });
    setIsSwapping(true);
    setErrorMessage(null);
    setTxSignature(null);
    if (activeSelectSide) {
      onRequestSelect(activeSelectSide);
    }
    setStep("processing");
    const result = await executeSwap({
      from_symbol: fromToken.symbol,
      from_mint: fromToken.mint,
      to_symbol: toToken.symbol,
      to_mint: toToken.mint,
      from_amount: String(tokenAmount),
      to_amount: completedAmount,
      usd_value: completedUsd,
    });
    setIsSwapping(false);
    if (result.success) {
      setTxSignature(result.signature ?? null);
      setAmountInput("");
      resetQuote();
      onSuccess();
      setStep("success");
      return;
    }
    setErrorMessage(result.error ?? "Swap failed. Please try again.");
    setStep("error");
  };

  const ctaLabel = !isAvailable
    ? unavailableReason ?? "Swap unavailable"
    : !hasAmount
    ? "Enter amount"
    : insufficientFunds
    ? "Insufficient balance"
    : isSwapping
    ? "Swapping…"
    : isQuoting || !quote
    ? "Getting quote…"
    : "Swap now";
  const ctaDisabled =
    !isAvailable ||
    !hasAmount ||
    insufficientFunds ||
    isQuoting ||
    !quote ||
    isSwapping;
  const isCtaInvalid = isAvailable && (!hasAmount || insufficientFunds);
  const visibleError =
    errorMessage ??
    (hasAmount && !insufficientFunds && !isQuoting && !quote
      ? swapError
      : null);

  return (
    /* The result screens unclip the pane on desktop so their dog can drop
        into the shell's 8px gap and sit flush with the viewport bottom (same
        escape the earn empty pane uses); the form keeps the clip. */
    <section
      className={`flex h-full w-full min-w-0 flex-1 flex-col rounded-3xl bg-card max-[795px]:rounded-none ${
        step === "success" || step === "error"
          ? "max-[795px]:overflow-clip"
          : "overflow-clip"
      }`}
    >
      <header className="flex w-full shrink-0 items-center p-2">
        <div className="flex shrink-0 items-center pr-3">
          <button
            aria-label="Back"
            className="t-hover flex size-11 items-center justify-center rounded-3xl hover:bg-accent"
            onClick={onBack}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${ASSET_BASE}/icon-arrow-left.svg`}
            />
          </button>
        </div>
        <div className="flex min-w-0 flex-1 items-center py-2">
          <h1 className="truncate whitespace-nowrap font-semibold text-[20px] text-foreground leading-6">
            Swap
          </h1>
        </div>
      </header>

      <PaneReveal key={step}>
        {step === "form" ? (
          <>
            <div className="flex min-h-0 w-full flex-1 flex-col overflow-y-auto">
              <div className="flex w-full shrink-0 flex-col p-2">
                <div className="flex w-full flex-col gap-0.5 px-4 py-2">
                  <p className="whitespace-nowrap text-[16px] leading-5 text-muted-foreground">
                    You swap
                  </p>
                  <div className="flex w-full items-center">
                    <div className="flex h-12 min-w-0 flex-1 items-baseline">
                      {isUsdInput ? (
                        <span className="font-semibold text-[40px] text-foreground leading-[48px]">
                          $
                        </span>
                      ) : null}
                      <input
                        autoFocus
                        className="w-full min-w-0 bg-transparent font-semibold text-[40px] text-foreground leading-[48px] outline-none placeholder:text-tertiary"
                        inputMode="decimal"
                        onChange={(event) =>
                          handleAmountChange(event.target.value)
                        }
                        placeholder="0"
                        type="text"
                        value={amountInput}
                      />
                    </div>
                    <div className="flex shrink-0 items-center pl-3">
                      <SwapTokenPill
                        isActive={activeSelectSide === "from"}
                        onClick={() => onRequestSelect("from")}
                        token={fromToken}
                      />
                    </div>
                  </div>
                  <div className="flex w-full items-center justify-between">
                    <button
                      aria-label="Switch between token and dollar amounts"
                      className="t-hover flex items-center gap-1 rounded-lg text-left hover:opacity-70"
                      onClick={handleToggleCurrency}
                      type="button"
                    >
                      <span className="whitespace-nowrap text-[16px] leading-5 text-muted-foreground">
                        {isUsdInput
                          ? `${formatTokenAmount(tokenAmount)} ${
                              fromToken.symbol
                            }`
                          : `$${formatUsdAmount(fromUsd)}`}
                      </span>
                      <ThemedIcon
                        className="size-5 text-tertiary"
                        src={`${ASSET_BASE}/icon-arrow-rotate.svg`}
                      />
                    </button>
                    <p className="flex items-center gap-1 whitespace-nowrap text-[16px] leading-5">
                      <button
                        className="t-hover text-foreground hover:opacity-70"
                        onClick={handleMax}
                        type="button"
                      >
                        MAX
                      </button>
                      <span className="text-muted-foreground">
                        <ScrambleText
                          isHidden={isBalanceHidden}
                          text={formatTokenAmount(fromToken.balance)}
                        />
                      </span>
                    </p>
                  </div>
                </div>

                <div className="relative flex h-11 w-full items-center justify-center">
                  <span
                    aria-hidden="true"
                    className="absolute inset-x-6 top-1/2 h-px bg-border"
                  />
                  <button
                    aria-label="Swap tokens"
                    className="t-hover relative flex size-11 items-center justify-center rounded-3xl bg-secondary hover:bg-accent-active"
                    onClick={handleFlip}
                    type="button"
                  >
                    <ThemedIcon
                      className="size-6 text-tertiary"
                      src={`${ASSET_BASE}/icon-arrow-top-bottom.svg`}
                    />
                  </button>
                </div>

                <div className="flex w-full flex-col gap-0.5 px-4 py-2">
                  <p className="whitespace-nowrap text-[16px] leading-5 text-muted-foreground">
                    You receive
                  </p>
                  <div className="flex w-full items-center">
                    <div className="flex h-12 min-w-0 flex-1 items-baseline overflow-hidden">
                      <p
                        className={`truncate font-semibold text-[40px] leading-[48px] ${
                          toAmount > 0 ? "text-foreground" : "text-tertiary"
                        }`}
                      >
                        {toAmount > 0 ? formatTokenAmount(toAmount) : "0"}
                      </p>
                    </div>
                    <div className="flex shrink-0 items-center pl-3">
                      <SwapTokenPill
                        isActive={activeSelectSide === "to"}
                        onClick={() => onRequestSelect("to")}
                        token={toToken}
                      />
                    </div>
                  </div>
                  <div className="flex w-full items-center justify-between">
                    <p className="whitespace-nowrap text-[16px] leading-5 text-muted-foreground">
                      ~${formatUsdAmount(toUsd)}
                    </p>
                    <p className="whitespace-nowrap text-[16px] leading-5 text-muted-foreground">
                      {"Balance: "}
                      <ScrambleText
                        isHidden={isBalanceHidden}
                        text={formatTokenAmount(toToken.balance)}
                      />
                    </p>
                  </div>
                </div>
              </div>

              <div className="w-full shrink-0 p-2">
                <div className="flex w-full flex-col rounded-2xl bg-accent">
                  <div className="flex w-full items-center justify-between px-4 py-2.5 text-[13px] leading-4">
                    <span className="text-muted-foreground">Rate</span>
                    <span className="pl-3 text-right text-foreground">
                      {rate > 0
                        ? `1 ${toToken.symbol} ~ ${
                            rate >= 1
                              ? formatUsdAmount(rate)
                              : rate.toLocaleString("en-US", {
                                  maximumSignificantDigits: 5,
                                })
                          } ${fromToken.symbol}`
                        : "—"}
                    </span>
                  </div>
                  <div className="flex w-full items-center justify-between px-4 py-2.5 text-[13px] leading-4">
                    <span className="text-muted-foreground">Slippage</span>
                    {/* Same static figures the OG swap form shows — actual
                  slippage/priority fee are Jupiter-side. */}
                    <span className="text-foreground">1%</span>
                  </div>
                  <div className="flex w-full items-center justify-between px-4 py-2.5 text-[13px] leading-4">
                    <span className="text-muted-foreground">Network Fee</span>
                    <span className="pl-3 text-right text-foreground">
                      {"0.00005 SOL ~ <$0.01"}
                    </span>
                  </div>
                </div>
              </div>
            </div>

            <div className="w-full shrink-0 bg-card px-4 pt-2 pb-4">
              {visibleError ? (
                <p className="pb-2 text-[13px] text-destructive leading-4">
                  {visibleError}
                </p>
              ) : null}
              <button
                className={`flex h-12 w-full items-center justify-center rounded-full font-medium text-[16px] leading-5 transition-colors ${
                  isCtaInvalid
                    ? "bg-destructive/[0.08] text-destructive"
                    : ctaDisabled
                    ? "bg-foreground text-background opacity-60"
                    : "t-hover bg-foreground text-background hover:-translate-y-0.5 hover:bg-foreground/90 active:translate-y-0"
                }`}
                disabled={ctaDisabled}
                onClick={() => void handleConfirm()}
                type="button"
              >
                <TextSwap text={ctaLabel} />
              </button>
            </div>
          </>
        ) : step === "processing" ? (
          <ActionProcessingBody
            icons={
              <>
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img
                  alt=""
                  aria-hidden="true"
                  className="-mr-4 size-16 rounded-full object-cover"
                  src={fromToken.icon || getTokenIconUrl(fromToken.symbol)}
                />
                {/* eslint-disable-next-line @next/next/no-img-element */}
                <img
                  alt=""
                  aria-hidden="true"
                  className="size-16 rounded-full object-cover"
                  src={toToken.icon || getTokenIconUrl(toToken.symbol)}
                />
              </>
            }
            label="Swapping"
          />
        ) : step === "success" ? (
          <ActionSuccessBody
            label="Swapped"
            onDone={onDone}
            signature={txSignature}
            source="swap_success"
          />
        ) : (
          <ActionErrorBody
            message={errorMessage}
            onBack={() => {
              setErrorMessage(null);
              setStep("form");
            }}
          />
        )}
      </PaneReveal>
    </section>
  );
}

// `rankSearchResults` tiers on the richer `TokenSearchResult` shape, so held
// tokens are lifted into it: `mcap: null` keeps their own list order within a
// tier, and a missing mint becomes "" so it can never mint-match.
function toSelectorToken(
  token: SwapToken,
  nameByMint: Record<string, string>
): TokenSearchResult {
  return {
    balance: token.balance,
    icon: token.icon,
    isVerified: true,
    mint: token.mint ?? "",
    mcap: null,
    name: (token.mint ? nameByMint[token.mint] : undefined) ?? token.symbol,
    price: token.price,
    symbol: token.symbol,
  };
}

export function SwapTokenSelectPane({
  earnEligibleMints,
  nameByMint,
  onClose,
  onSearch,
  onSelect,
  side,
  title,
  tokens,
}: {
  earnEligibleMints?: ReadonlySet<string>;
  nameByMint: Record<string, string>;
  onClose: () => void;
  /**
   * Opt-in to the remote search and the Trending section. Presence is the
   * switch: the request lifecycle (debounce, abort, stale guarding) is owned
   * here and calls `searchTokens` directly, so the callback itself is never
   * invoked — it stays accepted for callers passing `usePopularTokens().search`.
   * Selectors that pass none (Send's asset picker, swap's from side) stay
   * holdings-only.
   */
  onSearch?: (query: string) => Promise<SwapToken[]>;
  onSelect: (token: SwapToken) => void;
  side: "from" | "to";
  title?: string;
  tokens: SwapToken[];
}) {
  const [search, setSearch] = useState("");
  const [searchResults, setSearchResults] = useState<TokenSearchResult[]>([]);
  const [trending, setTrending] = useState<TokenSearchResult[]>(() =>
    onSearch ? getCachedTrendingTokens() ?? [] : []
  );
  const [activeIndex, setActiveIndex] = useState(0);
  const searchAbortRef = useRef<AbortController | null>(null);
  const { isBalanceHidden } = useBalanceVisibility();

  // Raw (case-preserved) for the remote search and ranking — base58 mints are
  // case-sensitive — lowercased for the local symbol/name filter.
  const rawQuery = search.trim();
  const query = rawQuery.toLowerCase();
  const isRemoteSearchEnabled = Boolean(onSearch);
  const latestQueryRef = useRef(rawQuery);
  latestQueryRef.current = rawQuery;

  const localFiltered = useMemo(
    () =>
      tokens.filter((token) => {
        if (!query) {
          return true;
        }
        const name = token.mint ? nameByMint[token.mint] : undefined;
        return (
          token.symbol.toLowerCase().includes(query) ||
          (name?.toLowerCase().includes(query) ?? false) ||
          token.mint?.toLowerCase() === query
        );
      }),
    [nameByMint, query, tokens]
  );

  const heldMints = useMemo(
    () => new Set(tokens.map((token) => token.mint).filter(Boolean)),
    [tokens]
  );
  const ownedTokens = useMemo(
    () => localFiltered.map((token) => toSelectorToken(token, nameByMint)),
    [localFiltered, nameByMint]
  );
  const remoteTokens = useMemo(
    () => searchResults.filter((token) => !heldMints.has(token.mint)),
    [heldMints, searchResults]
  );
  const trendingRows = useMemo(() => {
    if (!isRemoteSearchEnabled || query) {
      return [];
    }
    return trending.filter((token) => !heldMints.has(token.mint));
  }, [heldMints, isRemoteSearchEnabled, query, trending]);

  const list = useMemo(() => {
    if (!query) {
      return [...trendingRows, ...ownedTokens];
    }
    return rankSearchResults(rawQuery, [...ownedTokens, ...remoteTokens]);
  }, [ownedTokens, query, rawQuery, remoteTokens, trendingRows]);

  const activeSuggestion =
    query.length >= TOKEN_SEARCH_MIN_LENGTH && list.length > 0
      ? Math.min(activeIndex, list.length - 1)
      : null;
  const activeToken = activeSuggestion === null ? null : list[activeSuggestion];

  const refreshTrending = useCallback(() => {
    if (!isRemoteSearchEnabled) {
      return;
    }
    const cached = getCachedTrendingTokens();
    if (cached) {
      setTrending(cached);
    }
    void prefetchTrendingTokens()
      .then((result) => setTrending(result))
      .catch(() => {});
  }, [isRemoteSearchEnabled]);

  useEffect(() => {
    refreshTrending();
  }, [refreshTrending]);

  // Debounced remote search. Each keystroke aborts the request before it; a
  // failed or aborted one leaves the previous results on screen.
  useEffect(() => {
    if (!isRemoteSearchEnabled || rawQuery.length < TOKEN_SEARCH_MIN_LENGTH) {
      searchAbortRef.current?.abort();
      searchAbortRef.current = null;
      setSearchResults([]);
      return;
    }
    let isCurrent = true;
    const timer = window.setTimeout(() => {
      const controller = new AbortController();
      searchAbortRef.current = controller;
      void searchTokens(rawQuery, controller.signal)
        .then((results) => {
          if (!isCurrent || latestQueryRef.current !== rawQuery) {
            return;
          }
          setSearchResults(results.filter((token) => token.isVerified));
        })
        .catch(() => {});
    }, TOKEN_SEARCH_DEBOUNCE_MS);
    return () => {
      isCurrent = false;
      window.clearTimeout(timer);
      searchAbortRef.current?.abort();
    };
  }, [isRemoteSearchEnabled, rawQuery]);

  const handleSearchKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.nativeEvent.isComposing || activeSuggestion === null) {
      return;
    }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      setActiveIndex((current) =>
        Math.max(
          0,
          Math.min(
            list.length - 1,
            current + (event.key === "ArrowDown" ? 1 : -1)
          )
        )
      );
      return;
    }
    if (event.key === "Enter" || event.key === "Tab") {
      event.preventDefault();
      onSelect(list[activeSuggestion]);
    }
  };

  const renderTokenRow = (token: TokenSearchResult) => (
    <button
      className={`t-hover flex w-full shrink-0 items-center rounded-2xl px-4 text-left hover:bg-accent ${
        token === activeToken ? "bg-accent" : ""
      }`}
      key={token.mint || token.symbol}
      onClick={() => onSelect(token)}
      type="button"
    >
      <span className="flex shrink-0 items-center py-2 pr-3">
        {/* eslint-disable-next-line @next/next/no-img-element */}
        <img
          alt=""
          className="size-11 rounded-full object-cover"
          src={token.icon || getTokenIconUrl(token.symbol)}
        />
      </span>
      <span className="flex min-w-0 flex-1 flex-col gap-0.5 py-[11px]">
        <span className="flex min-w-0 items-center gap-1.5">
          <span className="truncate font-medium text-[16px] text-foreground leading-5 tracking-[-0.176px]">
            {(token.mint ? nameByMint[token.mint] : undefined) ?? token.symbol}
          </span>
          {side === "to" && token.mint && earnEligibleMints?.has(token.mint) ? (
            <span className="shrink-0 rounded-md bg-positive/[0.14] px-1.5 py-0.5 font-medium text-[11px] text-positive leading-3">
              Can earn
            </span>
          ) : null}
        </span>
        <span className="whitespace-nowrap text-[13px] leading-4 text-muted-foreground">
          {token.symbol}
        </span>
      </span>
      {side === "from" ? (
        <span className="flex shrink-0 flex-col items-end justify-center gap-0.5 py-[11px] pl-3">
          <span className="whitespace-nowrap text-right font-medium text-[16px] text-foreground leading-5">
            <ScrambleText
              isHidden={isBalanceHidden}
              text={splitUsdBalance(token.balance * token.price).balanceWhole}
            />
            <span className="text-tertiary">
              <ScrambleText
                isHidden={isBalanceHidden}
                text={
                  splitUsdBalance(token.balance * token.price).balanceFraction
                }
              />
            </span>
          </span>
          <span className="whitespace-nowrap text-right text-[13px] leading-4 text-muted-foreground">
            <ScrambleText
              isHidden={isBalanceHidden}
              text={formatTokenAmount(token.balance)}
            />
          </span>
        </span>
      ) : (
        <span className="flex shrink-0 items-center justify-end pl-3">
          <span className="whitespace-nowrap text-right font-medium text-[16px] text-foreground leading-5">
            ${formatTokenPrice(token.price)}
          </span>
        </span>
      )}
    </button>
  );

  const renderSectionLabel = (label: string) => (
    <p className="px-4 pb-1 pt-2 text-[13px] leading-4 text-muted-foreground">
      {label}
    </p>
  );

  return (
    <div className="flex h-full w-full min-w-0 flex-col">
      <header className="flex w-full shrink-0 items-center p-2">
        <div className="flex min-w-0 flex-1 items-center py-2.5 pl-4 max-[795px]:pl-2">
          <h2 className="truncate whitespace-nowrap font-semibold text-[20px] text-foreground leading-6">
            {title ?? (side === "from" ? "You swap" : "You receive")}
          </h2>
        </div>
        <div className="flex shrink-0 items-center pl-3">
          <button
            aria-label="Close"
            className="t-hover flex size-11 items-center justify-center rounded-3xl hover:bg-accent"
            onClick={onClose}
            type="button"
          >
            <ThemedIcon
              className="size-6 text-muted-foreground"
              src={`${ASSET_BASE}/icon-cross.svg`}
            />
          </button>
        </div>
      </header>
      <div className="w-full shrink-0 px-4 pb-2">
        <div className="flex w-full items-center rounded-full bg-accent px-3">
          <Search
            className="mr-3 shrink-0 text-muted-foreground"
            size={24}
            strokeWidth={2}
          />
          <input
            className="min-w-0 flex-1 bg-transparent py-3 text-[16px] text-foreground leading-5 outline-none placeholder:text-muted-foreground"
            onChange={(event) => {
              setSearch(event.target.value);
              setActiveIndex(0);
            }}
            onFocus={refreshTrending}
            onKeyDown={handleSearchKeyDown}
            placeholder="Search assets"
            type="text"
            value={search}
          />
        </div>
      </div>
      <div className="flex min-h-0 w-full flex-1 flex-col overflow-y-auto p-2">
        {list.length === 0 ? (
          <p className="px-4 py-8 text-center text-[14px] leading-5 text-muted-foreground">
            No tokens found
          </p>
        ) : query ? (
          list.map(renderTokenRow)
        ) : trendingRows.length > 0 ? (
          <>
            {renderSectionLabel("Trending")}
            {trendingRows.map(renderTokenRow)}
            {renderSectionLabel("Your assets")}
            {ownedTokens.map(renderTokenRow)}
          </>
        ) : (
          ownedTokens.map(renderTokenRow)
        )}
      </div>
    </div>
  );
}
