import { expect, mock, test } from "bun:test";
import { parseHTML } from "linkedom";

const { window, document } = parseHTML("<!doctype html><html><body></body></html>");
Object.assign(globalThis, { window, document, IS_REACT_ACT_ENVIRONMENT: true });

const WALLET = "SoLWaLLet11111111111111111111111111111111111";
const EVM = `0x${"b".repeat(40)}`;
const signer = mock(async () => ({ signature: "sig" }));

mock.module("@privy-io/react-auth", () => ({
  usePrivy: () => ({
    user: {
      linkedAccounts: [
        { type: "wallet", chainType: "solana", walletClientType: "privy", address: WALLET },
      ],
    },
  }),
  useAuthorizationSignature: () => ({ generateAuthorizationSignature: signer }),
}));
mock.module("@/contexts/public-env-context", () => ({
  usePublicEnv: () => ({ privyAppId: "app" }),
}));

const React = await import("react");
const { createRoot } = await import("react-dom/client");
const { useEvmDepositAddress } = await import("./use-evm-deposit-address");

async function load(responses: { status: number; body: unknown }[]) {
  const methods: string[] = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (_url: string, init?: RequestInit) => {
    methods.push(init?.method ?? "GET");
    const next = responses[methods.length - 1];
    return new Response(JSON.stringify(next.body), { status: next.status });
  }) as typeof fetch;
  signer.mockClear();
  let hook!: ReturnType<typeof useEvmDepositAddress>;
  function Probe() {
    hook = useEvmDepositAddress(WALLET);
    return null;
  }
  const root = createRoot(document.createElement("div") as unknown as Element);
  try {
    await React.act(async () => root.render(React.createElement(Probe)));
    await React.act(async () => hook.load());
    return { methods, hook };
  } finally {
    await React.act(async () => root.unmount());
    globalThis.fetch = realFetch;
  }
}

const network = {
  networks: [{ chain: "base", name: "Base", assets: ["usdc"], minUsd: 0 }],
};

test("a relay_signed response without an address never reaches the signer", async () => {
  const { methods, hook } = await load([
    { status: 200, body: { ...network, address: null, kind: "relay_signed" } },
  ]);
  expect(signer).not.toHaveBeenCalled();
  expect(methods).toEqual(["GET"]);
  expect(hook.info).toBeNull();
  expect(hook.error).toBeTruthy();
});

test("a failed response without an error body never reaches the signer", async () => {
  const { methods, hook } = await load([
    { status: 500, body: { title: "Internal Server Error", status: 500 } },
  ]);
  expect(signer).not.toHaveBeenCalled();
  expect(methods).toEqual(["GET"]);
  expect(hook.info).toBeNull();
  expect(hook.error).toBeTruthy();
});

test("a toSign response without a kind signs and posts the signature", async () => {
  const toSign = { version: 1, method: "POST" };
  const { methods, hook } = await load([
    { status: 200, body: { ...network, address: null, toSign, requestExpiry: 123 } },
    { status: 200, body: { ...network, address: EVM } },
  ]);
  expect(signer).toHaveBeenCalledWith(toSign);
  expect(methods).toEqual(["GET", "POST"]);
  expect(hook.info?.address).toBe(EVM);
  expect(hook.error).toBeNull();
});
