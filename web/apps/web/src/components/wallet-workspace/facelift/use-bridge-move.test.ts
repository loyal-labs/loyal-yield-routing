import { expect, mock, test } from "bun:test";
import { parseHTML } from "linkedom";

const { window, document } = parseHTML("<!doctype html><html><body></body></html>");
Object.assign(globalThis, { window, document, IS_REACT_ACT_ENVIRONMENT: true });

const WALLET = "SoLWaLLet11111111111111111111111111111111111";
const EVM = `0x${"b".repeat(40)}`;
const sendTransaction = mock(async (): Promise<{ hash: string }> => ({ hash: "0x1" }));
const wallets = [
  {
    address: EVM,
    switchChain: async () => undefined,
    getEthereumProvider: async () => ({
      request: async () => ({ status: "0x1" }),
    }),
  },
];

mock.module("@privy-io/react-auth", () => ({
  usePrivy: () => ({ ready: true, authenticated: true }),
  useUser: () => ({
    user: {
      linkedAccounts: [
        { type: "wallet", chainType: "solana", address: WALLET, walletClientType: "phantom" },
      ],
    },
    refreshUser: async () => ({}),
  }),
  useCreateWallet: () => ({ createWallet: async () => ({}) }),
  getIdentityToken: async () => "token",
  useAuthorizationSignature: () => ({ generateAuthorizationSignature: async () => ({}) }),
  useSendTransaction: () => ({ sendTransaction }),
  useWallets: () => ({ wallets }),
}));

const React = await import("react");
const { createRoot } = await import("react-dom/client");
const { classifySendError, moveIncoming, settleWithin, useIncomingTransfers } = await import(
  "./use-bridge-move"
);

const NETWORKS = [
  {
    chain: "base",
    name: "Base",
    icon: null,
    route: "evm",
    shortName: "Base",
    minUsd: 0,
    tokens: [{ asset: "0xusdc", symbol: "USDC", name: "USD Coin", icon: null, decimals: 6 }],
  },
];
const CALL = { to: EVM, data: "0x", value: "0" };
const QUOTE = {
  requestId: "0xreq",
  chainId: 8453,
  amountOutRaw: "990000",
  amountOutMinRaw: "980000",
  relayerFeeUsd: "0.01",
  etaSeconds: 30,
};
const waitFor = (ms: number) =>
  React.act(async () => {
    await new Promise((resolve) => setTimeout(resolve, ms));
  });

test("one open reads the balances once, however often the sheet renders", async () => {
  const paths: string[] = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (url: string) => {
    paths.push(url);
    if (paths.length > 12) return new Promise(() => undefined);
    return new Response(
      JSON.stringify(
        url.endsWith("/evm-deposit-address")
          ? { address: EVM, kind: "relay_signed", networks: NETWORKS }
          : url.endsWith("/balances")
          ? { balances: [{ chain: "base", asset: "0xusdc", decimals: 6, amountRaw: "1" }] }
          : { transfers: [] }
      )
    );
  }) as typeof fetch;
  let moves!: ReturnType<typeof useIncomingTransfers>;
  function Probe() {
    moves = useIncomingTransfers(WALLET);
    return null;
  }
  const root = createRoot(document.createElement("div") as unknown as Element);
  try {
    await React.act(async () => root.render(React.createElement(Probe)));
    await waitFor(50);
    await React.act(async () => root.render(React.createElement(Probe)));
    await waitFor(50);
    expect(paths.filter((p) => p.endsWith("/balances"))).toHaveLength(1);
    expect(moves.items).toHaveLength(1);
  } finally {
    await React.act(async () => root.unmount());
    globalThis.fetch = realFetch;
  }
});

async function failSend(failure: unknown, calls = [CALL], confirmStatus = 200) {
  const posts: string[] = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (url: string) => {
    posts.push(url);
    if (url.endsWith("/balances"))
      return new Response(
        JSON.stringify({
          balances: [{ chain: "base", asset: "0xusdc", decimals: 6, amountRaw: "5000000" }],
        })
      );
    if (url.endsWith("/transfers")) return new Response(JSON.stringify({ transfers: [] }));
    return new Response(JSON.stringify({ ...QUOTE, calls }), {
      status: url.endsWith("/confirm") ? confirmStatus : 200,
    });
  }) as typeof fetch;
  sendTransaction.mockReset();
  for (let i = 1; i < calls.length; i++) {
    sendTransaction.mockImplementationOnce(async () => ({ hash: "0xapprove" }));
  }
  sendTransaction.mockImplementationOnce(() => Promise.reject(failure));
  try {
    const outcome = await moveIncoming({
      sender: EVM,
      chain: "base",
      asset: "0xusdc",
      symbol: "USDC",
      wallet: wallets[0] as never,
      sendTransaction: sendTransaction as never,
      onRead: () => undefined,
      onQuoted: () => undefined,
      onSent: () => undefined,
    });
    return {
      step: outcome.kind,
      message: outcome.kind === "failed" ? outcome.message : null,
      refusalPosts: posts.filter((p) => p.endsWith("/sponsor-refusal")).length,
      sends: sendTransaction.mock.calls.length,
    };
  } finally {
    globalThis.fetch = realFetch;
  }
}

test("a rejection is read from the code, not the wording", async () => {
  const result = await failSend(Object.assign(new Error("nope"), { code: 4001 }));
  expect(result).toEqual({ step: "rejected", message: null, refusalPosts: 0, sends: 1 });
});

test("a failure that names rejection in its text is not a rejection", async () => {
  const result = await failSend(new Error("the RPC rejected the batch"));
  expect(result.step).toBe("failed");
  expect(result.refusalPosts).toBe(0);
});

test("only a sponsorship code reaches the refusal route and its copy", async () => {
  const refused = await failSend(
    Object.assign(new Error("Insufficient gas credits balance"), {
      code: "insufficient_funds",
    })
  );
  expect(refused.refusalPosts).toBe(1);
  expect(refused.message).toBe("Not sponsored. USDC stays in your wallet.");

  const other = await failSend(
    Object.assign(new Error("rpc down"), { code: "transaction_broadcast_failure" })
  );
  expect(other.refusalPosts).toBe(0);
  expect(other.message).toBe("The move did not finish. Please try again.");
});

test("once a transaction went out the copy never says the funds stayed", async () => {
  const { message } = await failSend(new Error("rpc down"), [CALL, CALL]);
  expect(message).toBe("The move did not finish. Check your wallet before trying again.");
});

test("a confirm the server refused sends nothing and shows the failure", async () => {
  const result = await failSend(new Error("unreached"), [CALL], 503);
  expect(result.sends).toBe(0);
  expect(result.step).toBe("failed");
});

test("a wallet call that never settles times out as its own failure", async () => {
  const error = await settleWithin(new Promise(() => undefined), 5).catch((e) => e);
  expect(classifySendError(error)).toBe("timeout");
});
