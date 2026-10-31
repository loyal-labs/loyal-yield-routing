import { TOKEN_MINTS } from "@loyal-labs/wallet-core/constants";
import type { AnalyticsProperties } from "@loyal-labs/shared/analytics";
import { useConnection, useWallet } from "@solana/wallet-adapter-react";
import {
  type ParsedAccountData,
  PublicKey,
  VersionedTransaction,
} from "@solana/web3.js";
import { useCallback, useEffect, useRef, useState } from "react";

import { usePublicEnv } from "@/contexts/public-env-context";
import { createBrowserLifecycleTracker } from "@/features/observability/client";
import {
  type LifecycleErrorCode,
  lifecycleErrorMessage,
  type LifecycleFlowStage,
  type LifecycleTracker,
  normalizeLifecycleWalletProvider,
} from "@/features/observability/lifecycle-contract";
import { trackWalletSwapCompleted } from "@/lib/core/analytics";
import {
  isSwapVisible,
  type SwapBalanceSnapshot,
} from "@/features/observability/swap-visibility";

const logger = {
  debug: (...args: unknown[]) => {
    if (process.env.NODE_ENV === "development") {
      // biome-ignore lint/suspicious/noConsole: Development logging
      console.log(...args);
    }
  },
};

function cleanSolanaErrorMessage(message: string): string {
  const logsIndex = message.indexOf("Logs:");
  if (logsIndex !== -1) {
    return message.slice(0, logsIndex).trim();
  }
  return message;
}

const PERCENTAGE_MULTIPLIER = 100;

const SLIPPAGE_BPS = 50;
const PRIORITY_FEE_MAX_LAMPORTS = 50_000_000;

// Jupiter aggregator custom error 6001 (0x1771) — SlippageToleranceExceeded:
// the route's output fell below the quote's minimum-out by execution time.
function isSlippageExceededError(message: string): boolean {
  return message.includes('"Custom":6001') || message.includes("0x1771");
}

function swapLifecycleErrorCode(message: string): LifecycleErrorCode {
  if (isSlippageExceededError(message)) {
    return "slippage_exceeded";
  }
  if (message.includes("timeout") || message.includes("Timeout")) {
    return "wallet_signing_failed";
  }
  if (message.includes("Jupiter Swap API failed")) {
    return "request_failed";
  }
  if (message.includes("Transaction failed:")) {
    return "chain_confirmation_failed";
  }
  return "unexpected_error";
}

export type SwapQuote = {
  inputAmount: string;
  outputAmount: string;
  inputToken: string;
  outputToken: string;
  priceImpact?: string;
  fee?: string;
};

export type SwapResult = {
  signature?: string;
  success: boolean;
  error?: string;
  status?: "executed" | "proposed";
};

export type SwapExecutionContext = {
  executeTransaction: (
    transaction: VersionedTransaction
  ) => Promise<SwapResult>;
  userPublicKey: PublicKey | string | null;
};

const JUPITER_QUOTE_API_URL = "https://api.jup.ag/swap/v1/quote";
const JUPITER_SWAP_API_URL = "https://api.jup.ag/swap/v1/swap";

const getTokenMint = (symbol: string): string | undefined => {
  const normalizedSymbol = symbol.toUpperCase();
  return TOKEN_MINTS[normalizedSymbol];
};

type JupiterQuoteResponse = {
  inputMint: string;
  inAmount: string;
  outputMint: string;
  outAmount: string;
  otherAmountThreshold: string;
  swapMode: string;
  slippageBps: number;
  platformFee: null | {
    amount: string;
    feeBps: number;
  };
  priceImpactPct: string;
  routePlan: Array<{
    swapInfo: {
      ammKey: string;
      label: string;
      inputMint: string;
      outputMint: string;
      inAmount: string;
      outAmount: string;
      feeAmount: string;
      feeMint: string;
    };
    percent: number;
  }>;
  contextSlot?: number;
  timeTaken?: number;
};

type JupiterSwapResponse = {
  swapTransaction: string;
  lastValidBlockHeight: number;
  prioritizationFeeLamports: number;
};

export function useSwap(visibleBalances: SwapBalanceSnapshot) {
  const { connection } = useConnection();
  const {
    publicKey,
    connected: isConnected,
    sendTransaction,
    wallet,
  } = useWallet();
  const publicEnv = usePublicEnv();
  const { swap: swapConfig } = publicEnv;
  const visibleBalancesRef = useRef(visibleBalances);
  visibleBalancesRef.current = visibleBalances;
  const pendingVisibility = useRef(
    new Map<
      string,
      {
        tracker: LifecycleTracker;
        before: SwapBalanceSnapshot;
        atConfirmation: SwapBalanceSnapshot;
        inputMint: string;
        outputMint: string;
        timer: ReturnType<typeof setTimeout>;
        successProperties?: AnalyticsProperties;
        signature?: string;
      }
    >()
  );
  useEffect(() => {
    const pending = pendingVisibility.current;
    return () => {
      for (const item of pending.values()) clearTimeout(item.timer);
      pending.clear();
    };
  }, [visibleBalances.owner]);
  useEffect(() => {
    for (const [id, item] of pendingVisibility.current) {
      if (
        visibleBalances === item.atConfirmation ||
        !isSwapVisible(
          item.before,
          visibleBalances,
          item.inputMint,
          item.outputMint
        )
      )
        continue;
      item.tracker.complete("balance_visible", { chainState: "confirmed" });
      clearTimeout(item.timer);
      pendingVisibility.current.delete(id);
      if (item.successProperties)
        trackWalletSwapCompleted(publicEnv, {
          ...item.successProperties,
          signature: item.signature,
        });
    }
  }, [visibleBalances, publicEnv]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [quote, setQuote] = useState<SwapQuote | null>(null);
  const [quoteResponse, setQuoteResponse] =
    useState<JupiterQuoteResponse | null>(null);
  const quoteRequestId = useRef(0);
  const getTokenDecimals = useCallback(
    async (mintAddress: string): Promise<number> => {
      const mintPublicKey = new PublicKey(mintAddress);
      const accountInfo = await connection.getParsedAccountInfo(mintPublicKey);
      const data = accountInfo.value?.data;

      if (data && typeof data === "object" && "parsed" in data) {
        const parsedData = data as ParsedAccountData;
        const decimals = parsedData.parsed?.info?.decimals;
        if (typeof decimals === "number") {
          return decimals;
        }
      }

      throw new Error(
        `Unable to determine token decimals for mint ${mintAddress}`
      );
    },
    [connection]
  );

  const getQuote = useCallback(
    async (
      fromToken: string,
      toToken: string,
      amount: string,
      fromTokenMint?: string,
      fromTokenDecimals?: number,
      toTokenDecimals?: number,
      toTokenMint?: string
    ): Promise<SwapQuote | null> => {
      const requestId = ++quoteRequestId.current;
      setQuote(null);
      setQuoteResponse(null);
      try {
        setError(null);

        if (swapConfig.mode === "disabled") {
          throw new Error(swapConfig.reason);
        }

        const inputMint = fromTokenMint || getTokenMint(fromToken);
        const outputMint = toTokenMint || getTokenMint(toToken);

        if (!inputMint) {
          throw new Error(
            `Unknown token: ${fromToken}. Please provide token mint address.`
          );
        }
        if (!outputMint) {
          throw new Error(`Unknown token: ${toToken}`);
        }

        const [inputDecimals, outputDecimals] = await Promise.all([
          fromTokenDecimals ?? getTokenDecimals(inputMint),
          toTokenDecimals ?? getTokenDecimals(outputMint),
        ]);
        const amountInSmallestUnit = Math.floor(
          Number.parseFloat(amount) * 10 ** inputDecimals
        ).toString();

        logger.debug("Token conversion:", {
          fromToken,
          inputMint,
          toToken,
          outputMint,
          amount,
          amountInSmallestUnit,
          decimals: inputDecimals,
        });

        const url = `${JUPITER_QUOTE_API_URL}?inputMint=${inputMint}&outputMint=${outputMint}&amount=${amountInSmallestUnit}&slippageBps=${SLIPPAGE_BPS}`;
        logger.debug("Fetching quote from Jupiter API:", url);

        const response = await fetch(url, {
          signal: AbortSignal.timeout(15_000),
          headers: {
            "x-api-key": swapConfig.apiKey,
          },
        });

        if (!response.ok) {
          const errorText = await response.text();
          logger.debug("Quote API error:", errorText);
          throw new Error(`Failed to get quote: ${response.statusText}`);
        }

        const data: JupiterQuoteResponse = await response.json();
        logger.debug("Jupiter Quote response:", data);

        const outputAmount = (
          Number.parseInt(data.outAmount, 10) /
          10 ** outputDecimals
        ).toFixed(outputDecimals);

        const priceImpact = `${(
          Number.parseFloat(data.priceImpactPct) * PERCENTAGE_MULTIPLIER
        ).toFixed(2)}%`;

        const quoteData: SwapQuote = {
          inputAmount: amount,
          outputAmount,
          inputToken: fromToken,
          outputToken: toToken,
          priceImpact,
          fee: undefined,
        };

        logger.debug("Parsed quote data:", quoteData);
        if (requestId !== quoteRequestId.current) return null;
        setQuoteResponse(data);
        setQuote(quoteData);
        return quoteData;
      } catch (err) {
        if (requestId !== quoteRequestId.current) return null;
        const errorMessage =
          err instanceof TypeError ||
          (err instanceof Error && err.name === "TimeoutError")
            ? "Unable to get a quote. Check your connection and try again."
            : err instanceof Error
            ? err.message
            : "Failed to get quote";
        setError(errorMessage);
        logger.debug("Quote error:", err);
        return null;
      }
    },
    [getTokenDecimals, swapConfig]
  );

  const executeSwap = useCallback(
    async (
      successTrackingProperties?: AnalyticsProperties,
      executionContext?: SwapExecutionContext
    ): Promise<SwapResult> => {
      if (swapConfig.mode === "disabled") {
        setError(swapConfig.reason);
        return { success: false, error: swapConfig.reason };
      }

      const swapUserPublicKey = executionContext?.userPublicKey
        ? typeof executionContext.userPublicKey === "string"
          ? new PublicKey(executionContext.userPublicKey)
          : executionContext.userPublicKey
        : publicKey;

      if (!swapUserPublicKey || (!executionContext && !isConnected)) {
        const errorMsg = "Wallet not connected";
        setError(errorMsg);
        return { success: false, error: errorMsg };
      }

      if (!quoteResponse) {
        const errorMsg = "No quote available. Please get a quote first.";
        setError(errorMsg);
        return { success: false, error: errorMsg };
      }

      setLoading(true);
      setError(null);

      const tracker = createBrowserLifecycleTracker({
        flowName: "wallet.swap",
        flowVariant: executionContext ? "smart_account" : "wallet_adapter",
      });
      const walletProvider = normalizeLifecycleWalletProvider(
        wallet?.adapter.name
      );
      let lifecycleStage: LifecycleFlowStage<"wallet.swap"> = "intent";
      tracker.start("intent");
      const before = visibleBalancesRef.current;

      try {
        logger.debug("Executing swap with quote:", quoteResponse);

        // The stored quote is as old as the user's review pause + wallet
        // approval, so its minimum-out can trail the live market. Refresh it
        // right before building the transaction; on failure fall back to the
        // quote we already have rather than blocking the swap.
        const refreshQuoteResponse =
          async (): Promise<JupiterQuoteResponse | null> => {
            try {
              const url = `${JUPITER_QUOTE_API_URL}?inputMint=${quoteResponse.inputMint}&outputMint=${quoteResponse.outputMint}&amount=${quoteResponse.inAmount}&slippageBps=${SLIPPAGE_BPS}`;
              const response = await fetch(url, {
                headers: { "x-api-key": swapConfig.apiKey },
              });
              if (!response.ok) {
                return null;
              }
              return (await response.json()) as JupiterQuoteResponse;
            } catch {
              return null;
            }
          };

        // Price can still move between signing and landing — retry exactly
        // once with another fresh quote when the route slips (Jupiter 6001).
        let activeQuote = quoteResponse;
        for (let attempt = 0; ; attempt++) {
          activeQuote = (await refreshQuoteResponse()) ?? activeQuote;
          lifecycleStage = "quote_refresh";
          tracker.observe("quote_refresh", { stageIndex: attempt });
          try {
            logger.debug("Calling Jupiter Swap API...");
            lifecycleStage = "build";

            const swapResponse = await fetch(JUPITER_SWAP_API_URL, {
              method: "POST",
              headers: {
                "Content-Type": "application/json",
                "x-api-key": swapConfig.apiKey,
              },
              body: JSON.stringify({
                userPublicKey: swapUserPublicKey.toBase58(),
                quoteResponse: activeQuote,
                wrapAndUnwrapSol: true,
                dynamicComputeUnitLimit: true,
                prioritizationFeeLamports: {
                  priorityLevelWithMaxLamports: {
                    priorityLevel: "veryHigh",
                    maxLamports: PRIORITY_FEE_MAX_LAMPORTS,
                    global: true, // Use global fee market
                  },
                },
              }),
            });

            if (!swapResponse.ok) {
              const errorText = await swapResponse.text();
              logger.debug("Jupiter Swap API error:", errorText);
              throw new Error(
                `Jupiter Swap API failed: ${swapResponse.statusText}`
              );
            }

            const swapData: JupiterSwapResponse = await swapResponse.json();
            logger.debug("Jupiter Swap transaction response:", swapData);

            const { swapTransaction: serializedTx } = swapData;
            if (!serializedTx) {
              throw new Error("No transaction returned from Jupiter Swap API");
            }

            const txBuffer = Buffer.from(serializedTx, "base64");
            const transaction = VersionedTransaction.deserialize(txBuffer);
            tracker.observe("build", { stageIndex: attempt });

            logger.debug("Signing and sending transaction...");
            lifecycleStage = "wallet_submit_confirm";
            const executionResult = executionContext
              ? await executionContext.executeTransaction(transaction)
              : {
                  signature: await sendTransaction(transaction, connection),
                  success: true,
                  status: "executed" as const,
                };

            if (!executionResult.success) {
              throw new Error(executionResult.error ?? "Swap execution failed");
            }

            const signature = executionResult.signature;

            logger.debug("Transaction sent:", signature);
            logger.debug(
              `View transaction: https://orbmarkets.io/tx/${signature}?tab=summary`
            );

            if (!executionContext && signature) {
              logger.debug("Confirming transaction...");
              const latestBlockhash = await connection.getLatestBlockhash(
                "confirmed"
              );
              const confirmation = await connection.confirmTransaction(
                {
                  signature,
                  blockhash: latestBlockhash.blockhash,
                  lastValidBlockHeight: latestBlockhash.lastValidBlockHeight,
                },
                "confirmed"
              );

              if (confirmation.value.err) {
                throw new Error(
                  `Transaction failed: ${JSON.stringify(
                    confirmation.value.err
                  )}`
                );
              }
            }

            logger.debug("Transaction confirmed!");
            tracker.observe("wallet_submit_confirm", {
              chainState:
                executionResult.status === "proposed"
                  ? "submitted"
                  : "confirmed",
            });
            setLoading(false);
            if (executionResult.status === "proposed") {
              tracker.observe("ui_commit", { chainState: "submitted" });
            } else {
              tracker.observe("ui_commit", { chainState: "confirmed" });
              if (
                before.owner === swapUserPublicKey.toBase58() &&
                visibleBalancesRef.current.owner === before.owner
              ) {
                pendingVisibility.current.set(tracker.flowId, {
                  tracker,
                  before,
                  atConfirmation: visibleBalancesRef.current,
                  inputMint: activeQuote.inputMint,
                  outputMint: activeQuote.outputMint,
                  signature,
                  successProperties: successTrackingProperties,
                  timer: setTimeout(() => {
                    if (pendingVisibility.current.has(tracker.flowId))
                      tracker.timeout("balance_visible", {
                        chainState: "confirmed",
                        errorCode: "realtime_unresolved",
                      });
                  }, 60_000),
                });
              }
            }
            return {
              signature,
              success: true,
              status: executionResult.status,
            };
          } catch (attemptError) {
            const attemptMessage =
              attemptError instanceof Error ? attemptError.message : "";
            if (attempt === 0 && isSlippageExceededError(attemptMessage)) {
              logger.debug("Slippage exceeded; retrying with a fresh quote");
              tracker.retry("wallet_submit_confirm", {
                errorCode: "slippage_exceeded",
                stageIndex: attempt,
              });
              continue;
            }
            throw attemptError;
          }
        }
      } catch (err) {
        let errorMessage = "Swap execution failed";
        const rawMessage = err instanceof Error ? err.message : "";

        if (rawMessage.includes("User rejected")) {
          tracker.cancel(lifecycleStage, {
            errorCode: "wallet_rejected",
            ...(walletProvider ? { walletProvider } : {}),
          });
        } else {
          tracker.fail(lifecycleStage, {
            errorCode: swapLifecycleErrorCode(rawMessage),
            errorMessage: lifecycleErrorMessage(err),
            ...(walletProvider ? { walletProvider } : {}),
          });
        }

        if (err instanceof Error) {
          if (
            err.message.includes("timeout") ||
            err.message.includes("Timeout")
          ) {
            errorMessage =
              "Transaction signing timed out. Please try again and approve the transaction in your wallet promptly.";
          } else if (err.message.includes("User rejected")) {
            errorMessage = "Transaction was rejected in your wallet.";
          } else if (isSlippageExceededError(err.message)) {
            errorMessage =
              "The price moved while the swap was executing, so nothing was swapped. Please try again.";
          } else {
            errorMessage = cleanSolanaErrorMessage(err.message);
          }
        }

        setError(errorMessage);
        logger.debug("Swap execution error:", err);
        setLoading(false);
        return { success: false, error: errorMessage };
      }
    },
    [
      connection,
      isConnected,
      publicKey,
      quoteResponse,
      sendTransaction,
      swapConfig,
      wallet,
    ]
  );

  const resetQuote = useCallback(() => {
    quoteRequestId.current++;
    setQuote(null);
    setQuoteResponse(null);
    setError(null);
  }, []);

  return {
    getQuote,
    executeSwap,
    resetQuote,
    quote,
    loading,
    error,
    isAvailable: swapConfig.mode === "enabled",
    unavailableReason:
      swapConfig.mode === "disabled" ? swapConfig.reason : null,
  };
}
