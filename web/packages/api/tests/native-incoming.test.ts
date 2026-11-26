// bun test web/packages/api/tests/native-incoming.test.ts
import { afterEach, beforeEach, expect, mock, test } from "bun:test";
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { parseHTML } from "linkedom";
import type { BridgeCall, BridgeQuoteResponse, BridgeTransfer } from "../src";

const app = resolve(import.meta.dir, "../../../apps/mobile");
const mobile = createRequire(resolve(app, "package.json"));
const React: typeof import("react") = mobile("react");
const { createRoot } = mobile("react-dom/client") as typeof import("react-dom/client");
const sdk = mobile("@privy-io/js-sdk-core");
const { window, document } = parseHTML("<html><body></body></html>");
Object.assign(globalThis, { window, document, IS_REACT_ACT_ENVIRONMENT: true });

const solana = "11111111111111111111111111111111";
const evm = "0x" + "b".repeat(40);
const hash = "0x" + "c".repeat(64);
const requestId = "0x" + "d".repeat(64);
const call: BridgeCall = { data: "0x1234", to: "0x" + "a".repeat(40), value: "0" };
const quote: BridgeQuoteResponse = {
  requestId,
  chainId: 8453,
  calls: [call, call],
  amountOutRaw: "4900000",
  amountOutMinRaw: "4800000",
  relayerFeeUsd: "0.10",
  etaSeconds: 30,
};
const linkedEvm = {
  type: "wallet",
  chain_type: "ethereum",
  connector_type: "embedded",
  recovery_method: "privy-v2",
  wallet_client: "privy",
  wallet_client_type: "privy",
  id: "fixture-wallet",
  address: evm,
  wallet_index: 0,
};
const linkedSol = {
  type: "wallet",
  chain_type: "solana",
  wallet_client: "unknown",
  wallet_client_type: "phantom",
  address: solana,
};
const owner = { id: "did:privy:fixture", linked_accounts: [linkedSol, linkedEvm] };
const network = {
  chain: "base",
  route: "evm",
  shortName: "Base",
  minUsd: 0,
  tokens: [{ asset: "usdc", symbol: "USDC", decimals: 6 }],
};
const confirmed: BridgeTransfer = {
  requestId,
  direction: "in",
  state: "quoted",
  chain: "base",
  asset: "usdc",
  amountInRaw: "5000000",
  amountOutRaw: "4900000",
  inFlight: true,
  confirmedAt: "now",
  quotedAt: "now",
  originTx: null,
  destinationTx: null,
  refundTx: null,
  submittedAt: null,
  settledAt: null,
};
let cluster = "mainnet";
let activeSolana = solana;
let currentUser = owner;
let routeAddress = evm;
let routeFailure: unknown;
let balanceFailure = false;
let storageFailure = false;
let confirmBlocker = false;
let renewExternal: (() => Promise<void>) | undefined;
let amount = "5000000";
let transfers: BridgeTransfer[] = [];
let rpcFailure: unknown;
let onSigning: (() => void) | undefined;
let onReceipt: (() => Promise<unknown>) | undefined;
let sponsorCalls: Record<string, any>[] = [];
let signed: Record<string, any>[] = [];
let events: string[] = [];
const storage = new Map<string, string>();
const listeners = new Set<() => void>();
const foregroundListeners = new Set<(next: string) => void>();
const changeCluster = (value: string) => {
  cluster = value;
  for (const f of listeners) f();
};
const signer = { kind: "mwa", publicKey: { toBase58: () => activeSolana } };
const wallet = {
  address: evm,
  getProvider: async () => ({
    request: async ({ method }: { method: string }) => {
      if (method === "eth_accounts") return [evm];
      if (method === "eth_chainId") return "0x2105";
      if (method === "wallet_switchEthereumChain") return null;
      if (method === "eth_getTransactionReceipt") {
        events.push("receipt");
        return onReceipt ? onReceipt() : { status: "0x1" };
      }
      throw Error("Unexpected provider method " + method);
    },
  }),
};
const client = {
  user: { refreshUser: async () => ({ user: currentUser }) },
  app: { appId: "fixture" },
  getCompiledPath: (r: { path: string }, o: { params: { wallet_id: string } }) =>
    "https://fixture.invalid" + r.path.replace(":wallet_id", o.params.wallet_id),
  embeddedWallet: {
    signWithUserSigner: async ({ message }: { message: string }) => {
      signed.push(JSON.parse(Buffer.from(message, "base64").toString()));
      onSigning?.();
      return { signature: "fixture-only" };
    },
  },
  fetchPrivyRoute: async (
    r: { path: string },
    o: { params: { wallet_id: string }; body: Record<string, unknown>; headers: Record<string, string> },
  ) => {
    sponsorCalls.push({ ...o, path: r.path });
    events.push("sponsored");
    if (rpcFailure) throw rpcFailure;
    return { method: "eth_sendTransaction", data: { caip2: o.body.caip2, hash } };
  },
};
const privy = {
  ...sdk,
  usePrivy: () => ({ isReady: true, user: currentUser }),
  usePrivyClient: () => client,
  useEmbeddedEthereumWallet: () => ({ wallets: [wallet] }),
};
function nativeModule(name: string, value: object) {
  mock.module(mobile.resolve(name), () => value);
  mock.module(name, () => value);
}
nativeModule("@privy-io/expo", privy);
nativeModule("@react-navigation/native", { useIsFocused: () => true });
nativeModule("react-native-safe-area-context", { useSafeAreaInsets: () => ({ bottom: 0 }) });
const host =
  (tag: string) =>
  ({ children }: any) =>
    React.createElement(tag, null, children);
nativeModule("react-native", {
  AppState: {
    currentState: "active",
    addEventListener: (_: string, f: (next: string) => void) => {
      foregroundListeners.add(f);
      return { remove: () => foregroundListeners.delete(f) };
    },
  },
  StyleSheet: { create: (x: object) => x },
  Text: host("span"),
  View: host("div"),
  ScrollView: host("div"),
  Modal: host("section"),
  Linking: { openURL: async () => {} },
  Pressable: ({ children, disabled, onPress }: any) =>
    React.createElement("button", { disabled, onClick: onPress }, children),
});
const stub = (path: string, value: object) => mock.module(resolve(app, "src", path), () => value);
stub("components/wallet/PrivyProviderRoot.tsx", { isPrivyConfigured: () => true });
stub("lib/wallet/wallet-provider.tsx", {
  isWalletUnlocked: () => true,
  useWallet: () => ({ state: "unlocked", signer, publicKey: activeSolana }),
});
stub("lib/solana/wallet/wallet-details.ts", { getWalletSigner: async () => signer });
stub("lib/solana/rpc/connection.ts", {
  getSolanaEnv: () => cluster,
  onSolanaEnvChange: (f: () => void) => {
    listeners.add(f);
    return () => listeners.delete(f);
  },
});
stub("lib/storage.ts", {
  mmkv: {
    getString: (key: string) => storage.get(key),
    setString: (k: string, v: string) => {
      if (storageFailure) throw Error("fixture disk full");
      storage.set(k, v);
    },
    delete: (k: string) => storage.delete(k),
  },
});
stub("features/receive/funding-api.ts", {
  fundingRequest: async (
    sol: string,
    path: string,
    { body, interactive }: { body?: { originTx?: string; reason?: string }; interactive?: boolean } = {},
  ) => {
    expect(sol).toBe(solana);
    const step = path.split("/").pop()!;
    events.push(step);
    switch (step) {
      case "launch":
        return { launched: true };
      case "evm-deposit-address":
        if (interactive && renewExternal) await renewExternal();
        if (routeFailure) throw routeFailure;
        return { kind: "relay_signed", address: routeAddress, networks: [network] };
      case "balances":
        if (balanceFailure) throw Error("fixture balances unavailable");
        return { balances: [{ chain: "base", asset: "usdc", amountRaw: amount, decimals: 6 }] };
      case "transfers":
        return { transfers };
      case "quote":
        return quote;
      case "confirm":
        if (confirmBlocker) {
          transfers = [{ ...confirmed, requestId: "other-client" }];
          throw Object.assign(Error("Already in flight"), {
            code: "bridge_in_flight",
            requestId: "other-client",
            status: 409,
          });
        }
        transfers = [confirmed];
        return confirmed;
      case "release":
        events.push("released-" + body?.reason);
        transfers = [];
        return confirmed;
      case "sponsor-refusal":
        return {};
      case "submit":
        expect(body?.originTx).toBe(hash);
        transfers = [{ ...confirmed, state: "submitted", originTx: hash }];
        return transfers[0];
      default:
        throw Error("Unexpected funding path " + path);
    }
  },
});

const { IncomingBridge } = await import(
  "../../../apps/mobile/src/features/receive/incoming/IncomingBridge"
);
const { sendSponsoredCall } = await import(
  "../../../apps/mobile/src/features/receive/incoming/privy-transfer"
);
const container = document.createElement("div");
let root: ReturnType<typeof createRoot>;
const paint = () =>
  React.act(async () => root.render(React.createElement(IncomingBridge, { walletAddress: solana })));
const click = (label: string, twice = false) =>
  React.act(async () => {
    const button = [...container.querySelectorAll("button")].find((b) => b.textContent === label);
    expect(button, label).toBeTruthy();
    button!.click();
    if (twice) button!.click();
  });
const reopen = async () => {
  await React.act(async () => root.unmount());
  root = createRoot(container as unknown as Element);
  await paint();
};
beforeEach(() => {
  root = createRoot(container as unknown as Element);
  currentUser = owner;
  cluster = "mainnet";
  activeSolana = solana;
  routeAddress = evm;
  routeFailure = undefined;
  confirmBlocker = false;
  balanceFailure = false;
  storageFailure = false;
  renewExternal = undefined;
  amount = "5000000";
  transfers = [];
  rpcFailure = undefined;
  onSigning = undefined;
  onReceipt = undefined;
  signed = [];
  sponsorCalls = [];
  events = [];
  storage.clear();
});
afterEach(async () => {
  await React.act(async () => root.unmount());
});

test("review, cancel, review again and double tap approval: one sponsored allowance and deposit, then a reopened receipt", async () => {
  await paint();
  expect(container.textContent).toContain("5 USDC from Base");
  await click("Review transfer");
  expect(container.textContent).toContain(evm);
  expect(container.textContent).toContain(solana);
  expect(container.textContent).toContain("4.8 USDC");
  expect(container.textContent).toContain("$0.10");
  expect(events).not.toContain("confirm");
  await click("Cancel");
  expect(signed).toHaveLength(0);
  await click("Review transfer");
  await click("Approve and send", true);
  expect(events.filter((s) => ["confirm", "sponsored", "receipt", "submit"].includes(s))).toEqual([
    "confirm",
    "sponsored",
    "receipt",
    "sponsored",
    "receipt",
    "submit",
  ]);
  for (let i = 0; i < 2; i++) {
    const wire = sponsorCalls[i];
    expect(wire.path).toContain("/wallets/:wallet_id/rpc");
    expect(wire.params.wallet_id).toBe(linkedEvm.id);
    expect(wire.headers["privy-authorization-signature"]).toBe("fixture-only");
    expect(signed[i].body).toEqual(wire.body);
    expect(wire.body).toMatchObject({
      caip2: "eip155:8453",
      method: "eth_sendTransaction",
      sponsor: true,
      params: { transaction: { from: evm, to: call.to, data: call.data, value: "0x0" } },
    });
  }
  expect(storage.size).toBe(0);
  amount = "0";
  transfers = [{ ...confirmed, state: "filled", inFlight: false, destinationTx: "fixture-sol-tx" }];
  await React.act(async () => {
    for (const f of foregroundListeners) f("active");
  });
  expect(container.textContent).toContain("USDC arrived on Solana");
  await reopen();
  expect(container.textContent).toContain("USDC arrived on Solana");
  expect(container.textContent).toContain("View receipt");
  expect(sponsorCalls).toHaveLength(2);
});

test("devnet and unrelated SOL/EVM accounts do not discover balances or sign", async () => {
  cluster = "devnet";
  await paint();
  expect(events).toEqual([]);
  cluster = "mainnet";
  currentUser = { ...owner, linked_accounts: [linkedEvm, { ...linkedSol, address: solana.slice(1) }] };
  await paint();
  expect(events).toEqual([]);
  currentUser = owner;
  routeAddress = "0x" + "a".repeat(40);
  await paint();
  expect(events).not.toContain("balances");
  expect(signed).toHaveLength(0);
});

test("a changed balance requires a fresh review and a chain switch during authorization cannot reach transport", async () => {
  await paint();
  await click("Review transfer");
  amount = "6000000";
  await click("Approve and send");
  expect(container.textContent).toContain("Your balance changed");
  expect(events).not.toContain("confirm");
  amount = "5000000";
  await click("Review transfer");
  onSigning = () => changeCluster("devnet");
  await click("Approve and send");
  expect(sponsorCalls).toHaveLength(0);
  expect(events).toContain("released-aborted");
});

test("an in-flight move from another client wins over the reviewed quote", async () => {
  await paint();
  await click("Review transfer");
  transfers = [confirmed];
  await click("Approve and send");
  expect(events).not.toContain("confirm");
  expect(signed).toHaveLength(0);
  expect(container.textContent).toContain("Transfer in progress");
});

test("an ambiguous deposit remains server-locked across reopening", async () => {
  await paint();
  await click("Review transfer");
  onReceipt = async () => {
    rpcFailure = Error("server disconnected after handoff");
    return { status: "0x1" };
  };
  await click("Approve and send");
  expect(sponsorCalls).toHaveLength(2);
  expect(events).not.toContain("release");
  await reopen();
  expect(container.textContent).toContain("Transfer in progress");
  expect(container.textContent).not.toContain("Review transfer");
  expect(sponsorCalls).toHaveLength(2);
});

test("a reopened deposit hash is recorded even when balance discovery is down, without signing again", async () => {
  balanceFailure = true;
  transfers = [confirmed];
  amount = "0";
  storage.set(
    "loyal:incoming:" + solana + ":" + evm,
    JSON.stringify([{ requestId, hash, chainId: 8453, chain: "base", asset: "usdc", symbol: "USDC" }]),
  );
  onReceipt = async () => {
    throw Error("fixture RPC unavailable");
  };
  await paint();
  expect(events).toContain("submit");
  expect(storage.size).toBe(0);
  expect(signed).toHaveLength(0);
});

test("rejection releases the backend claim; sponsorship refusal is reported without an unsponsored fallback", async () => {
  await paint();
  await click("Review transfer");
  rpcFailure = Object.assign(Error("fixture refusal"), { code: 4001 });
  await click("Approve and send");
  expect(events).toContain("released-rejected");
  await click("Review transfer");
  rpcFailure = Object.assign(Error("fixture refused by sponsor"), { code: "policy_violation" });
  await click("Approve and send");
  expect(events).toContain("released-refused");
  expect(events).toContain("sponsor-refusal");
  expect(sponsorCalls.every((w) => w.body.sponsor === true)).toBe(true);
});

test("the server launch gate hides the card; a locked external session needs a deliberate retry", async () => {
  routeFailure = Object.assign(Error("closed"), { code: "not_open", status: 403 });
  await paint();
  expect(container.textContent).toBe("");
  expect(events).not.toContain("balances");
  routeFailure = Object.assign(Error("expired session"), { status: 401 });
  await reopen();
  expect(container.textContent).toContain("Authorize your wallet");
  routeFailure = undefined;
  await click("Retry incoming transfers");
  expect(container.textContent).toContain("Review transfer");
  expect(signed).toHaveLength(0);
});

test("a changed linked Privy EVM wallet after authorization cannot submit signed bytes", async () => {
  await paint();
  await click("Review transfer");
  onSigning = () => {
    currentUser = { ...owner, linked_accounts: [linkedSol, { ...linkedEvm, id: "another-id" }] };
  };
  await click("Approve and send");
  expect(sponsorCalls).toHaveLength(0);
  expect(events).toContain("released-aborted");
});

test("Phantom can renew a deliberate session in the background and return to one unsigned review", async () => {
  await paint();
  let returnFromPhantom!: () => void;
  renewExternal = () =>
    new Promise<void>((resolve) => {
      for (const f of foregroundListeners) {
        f("inactive");
        f("background");
      }
      returnFromPhantom = resolve;
    });
  await click("Review transfer");
  expect(container.textContent).toContain("Getting your quote");
  await React.act(async () => {
    returnFromPhantom();
    for (const f of foregroundListeners) f("active");
  });
  expect(container.textContent).toContain("Review incoming transfer");
  expect(events.filter((e) => e === "quote")).toHaveLength(1);
  expect(events).not.toContain("confirm");
  expect(signed).toHaveLength(0);
});

test("a conflicting confirm adopts the other request without signing or releasing it", async () => {
  await paint();
  await click("Review transfer");
  confirmBlocker = true;
  await click("Approve and send");
  expect(events).not.toContain("release");
  expect(signed).toHaveLength(0);
  expect(container.textContent).toContain("Transfer in progress");
});

test("an approval revert stops the deposit; a refunded balance retries through a fresh review", async () => {
  await paint();
  await click("Review transfer");
  onReceipt = async () => ({ status: "0x0" });
  await click("Approve and send");
  expect(sponsorCalls).toHaveLength(1);
  expect(events).toContain("released-reverted");
  expect(events).not.toContain("submit");
  transfers = [{ ...confirmed, state: "refunded", inFlight: false }];
  await reopen();
  expect(container.textContent).toContain("Transfer refunded");
  await click("Retry transfer");
  expect(container.textContent).toContain("Approve and send");
  expect(sponsorCalls).toHaveLength(1);
});

test("a local receipt storage failure still records the actual deposit hash with the backend", async () => {
  await paint();
  await click("Review transfer");
  storageFailure = true;
  await click("Approve and send");
  expect(events.filter((e) => e === "submit")).toHaveLength(1);
  expect(sponsorCalls).toHaveLength(2);
});

test("Tempo sponsorship signs a type-118 transaction with its quoted call", async () => {
  await sendSponsoredCall(
    client as never,
    { id: linkedEvm.id, address: evm },
    4217,
    call,
    async () => {},
  );
  expect(signed[0].body.params.transaction).toMatchObject({
    type: 118,
    from: evm,
    calls: [{ to: call.to, data: call.data, value: "0x0" }],
  });
  expect(sponsorCalls[0].body.caip2).toBe("eip155:4217");
});
