import type { BridgeDestinationToken } from "@loyal-labs/api";

import { fetchWithTimeout } from "@/lib/network/fetch-with-timeout";
import { SOLANA_USDC_MINT_MAINNET } from "@/lib/solana/constants";

export type BridgeEstimate =
  | { kind: "priced"; amountOutRaw: string; amountOutUsd: string | null }
  | { kind: "below-minimum" | "unavailable"; reason: string };

// The Go destination catalog supplies token addresses and decimals, but omits Relay chain IDs.
const chainIds: Readonly<Partial<Record<string, number>>> = {
  ethereum: 1,
  bsc: 56,
  base: 8453,
  arbitrum: 42161,
  optimism: 10,
  polygon: 137,
  monad: 143,
  tempo: 4217,
  robinhood: 4663,
  megaeth: 4326,
  avalanche: 43114,
  plasma: 9745,
  tron: 728126428,
};
const solana = 792703809;
// Matches bridge.OutFeeBps and RELAY_APP_FEE_RECIPIENT in loyal-web.service.
const loyalFeeBps = 40;
const feeRecipient = "0x536a85f63ceca75b12ec588cf37ca371489dd4a3";

function unavailable(reason: string): BridgeEstimate {
  return { kind: "unavailable", reason };
}

function matchesCurrency(
  currency:
    | { chainId?: unknown; address?: unknown; decimals?: unknown }
    | undefined,
  chainId: number,
  address: string,
  decimals: number
) {
  return (
    currency?.chainId === chainId &&
    currency.decimals === decimals &&
    typeof currency.address === "string" &&
    (address.startsWith("0x")
      ? currency.address.toLowerCase() === address.toLowerCase()
      : currency.address === address)
  );
}

function rawAmount(value: unknown): value is string {
  return typeof value === "string" && /^\d{1,78}$/.test(value);
}

export async function fetchBridgeEstimate(
  chain: string,
  token: BridgeDestinationToken,
  amountRaw: bigint,
  signal: AbortSignal
): Promise<BridgeEstimate> {
  const chainId = chainIds[chain];
  if (typeof chainId !== "number")
    return unavailable("Relay estimates are unavailable for this chain.");

  // shortcut: v2 is keyless until February 2027, migrate with the server's authenticated Relay integration.
  const response = await fetchWithTimeout("https://api.relay.link/quote/v2", {
    method: "POST",
    credentials: "omit",
    headers: { "Content-Type": "application/json" },
    signal,
    body: JSON.stringify({
      indicativeQuote: true,
      user: "CbKGgVKLJFb8bBrf58DnAkdryX6ubewVytn7X957YwNr",
      // RelayKit's VM-specific preview addresses stay inside this indicative request.
      recipient:
        chain === "tron"
          ? "THa7BwoPfacfiELa63pbmm3g5PGKYmtJyt"
          : "0x000000000000000000000000000000000000dead",
      originChainId: solana,
      originCurrency: SOLANA_USDC_MINT_MAINNET,
      destinationChainId: chainId,
      destinationCurrency: token.address,
      amount: amountRaw.toString(),
      tradeType: "EXACT_INPUT",
      slippageTolerance: "100",
      appFees: [{ recipient: feeRecipient, fee: String(loyalFeeBps) }],
    }),
  });
  const body = await response.json().catch(() => null);
  if (!response.ok) {
    if (body?.errorCode === "AMOUNT_TOO_LOW")
      return {
        kind: "below-minimum",
        reason: "Try a larger USDC amount for this route.",
      };
    if (response.status === 429)
      return unavailable(
        "Relay is receiving too many requests. Try again shortly."
      );
    if (
      [
        "NO_QUOTES",
        "NO_SWAP_ROUTES_FOUND",
        "INSUFFICIENT_LIQUIDITY",
        "UNSUPPORTED_ROUTE",
      ].includes(body?.errorCode)
    )
      return unavailable("Relay has no route for this token and amount.");
    return unavailable(
      "Relay could not price this route. Enter a recipient for a quote."
    );
  }

  const input = body?.details?.currencyIn;
  const output = body?.details?.currencyOut;
  const fee = body?.fees?.app;
  if (
    !Array.isArray(body?.steps) ||
    body.steps.length !== 0 ||
    !matchesCurrency(input?.currency, solana, SOLANA_USDC_MINT_MAINNET, 6) ||
    input.amount !== amountRaw.toString() ||
    !matchesCurrency(
      output?.currency,
      chainId,
      token.address,
      token.decimals
    ) ||
    !rawAmount(output.amount) ||
    BigInt(output.amount) === BigInt(0) ||
    !matchesCurrency(fee?.currency, solana, SOLANA_USDC_MINT_MAINNET, 6) ||
    !rawAmount(fee.amount)
  )
    return unavailable("Relay did not confirm the selected route and fees.");
  // Relay rounds its USDC fee to an atomic unit, as on the final quote.
  const feeDifference =
    BigInt(fee.amount) * BigInt(10_000) - amountRaw * BigInt(loyalFeeBps);
  if (feeDifference < BigInt(-10_000) || feeDifference > BigInt(10_000))
    return unavailable("Relay did not confirm the Loyal fee.");

  const usd = output.amountUsd;
  return {
    kind: "priced",
    amountOutRaw: output.amount,
    amountOutUsd:
      typeof usd === "string" &&
      /^\d+(?:\.\d+)?$/.test(usd) &&
      Number.isFinite(Number(usd))
        ? usd
        : null,
  };
}
