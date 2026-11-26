import { SOLANA_USDC_MINT_MAINNET } from "@/lib/solana/constants";
import { fetchBridgeEstimate } from "@/services/bridge-estimate";

const token = {
  asset: "rlusd",
  name: "Ripple USD",
  symbol: "RLUSD",
  icon: "rlusd.svg",
  address: "0x8292bb45bf1ee4d140127049757c2e0ff06317ed",
  decimals: 18,
  minUsdc: 0,
};
const source = {
  chainId: 792703809,
  address: SOLANA_USDC_MINT_MAINNET,
  decimals: 6,
};
const destination = {
  chainId: 1,
  address: token.address,
  decimals: token.decimals,
};
const amount = BigInt(2_192_896);
const signal = new AbortController().signal;
const estimate = () => fetchBridgeEstimate("ethereum", token, amount, signal);

// Monetary fields from Relay's indicative Solana USDC → Ethereum RLUSD response.
const input = { currency: source, amount: amount.toString() };
const output = {
  currency: destination,
  amount: "1966622533750915366",
  amountUsd: "1.966296",
};
const fee = { currency: source, amount: "8771" };
const pair = (currencyIn = input, currencyOut = output) => ({
  details: { currencyIn, currencyOut },
});
const price = { steps: [], ...pair(), fees: { app: fee } };

afterEach(() => jest.restoreAllMocks());

test("requests a fee-inclusive, non-executable price and retains only the destination amounts", async () => {
  const fetch = jest.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({
      ...price,
      requestId: "indicative-only",
    })
  );
  expect(await estimate()).toEqual({
    kind: "priced",
    amountOutRaw: output.amount,
    amountOutUsd: output.amountUsd,
  });
  const [url, init] = fetch.mock.calls[0];
  expect(url).toBe("https://api.relay.link/quote/v2");
  expect(JSON.parse(String(init?.body))).toMatchObject({
    indicativeQuote: true,
    tradeType: "EXACT_INPUT",
    amount: input.amount,
    originChainId: source.chainId,
    originCurrency: source.address,
    destinationChainId: destination.chainId,
    destinationCurrency: token.address,
    appFees: [
      { fee: "40", recipient: "0x536a85f63ceca75b12ec588cf37ca371489dd4a3" },
    ],
  });
});

test.each<[string, object]>([
  ["execution steps", { steps: [{ id: "deposit" }] }],
  ["wrong source mint", pair({ ...input, currency: destination })],
  ["wrong input amount", pair({ ...input, amount: "219" })],
  [
    "wrong destination chain",
    pair(input, { ...output, currency: { ...destination, chainId: 8453 } }),
  ],
  [
    "wrong token address",
    pair(input, {
      ...output,
      currency: { ...destination, address: source.address },
    }),
  ],
  [
    "wrong decimals",
    pair(input, { ...output, currency: { ...destination, decimals: 6 } }),
  ],
  ["zero output", pair(input, { ...output, amount: "0" })],
  ["missing fee", { fees: {} }],
  ["wrong fee", { fees: { app: { ...fee, amount: "0" } } }],
])("withholds a price with %s", async (_name, fields) => {
  jest
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(Response.json({ ...price, ...fields }));
  expect((await estimate()).kind).toBe("unavailable");
});

test("distinguishes the provider's amount floor from a route with no liquidity", async () => {
  jest
    .spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(
      Response.json({ errorCode: "AMOUNT_TOO_LOW" }, { status: 400 })
    )
    .mockResolvedValueOnce(
      Response.json({ errorCode: "INSUFFICIENT_LIQUIDITY" }, { status: 400 })
    );
  expect((await estimate()).kind).toBe("below-minimum");
  expect(await estimate()).toMatchObject({
    kind: "unavailable",
    reason: expect.stringContaining("no route"),
  });
});
