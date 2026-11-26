import {
  type BridgeOutQuoteResponse,
  type BridgeTransfer,
} from "@loyal-labs/api";
import { getAssociatedTokenAddressSync } from "@solana/spl-token";
import {
  AddressLookupTableAccount,
  type Connection,
  Keypair,
  PublicKey,
  VersionedTransaction,
} from "@solana/web3.js";
import bs58 from "bs58";

import { PrivyEmbeddedSigner } from "@/lib/wallet/privy-signer";
import { LocalKeypairSigner, type Signer } from "@/lib/wallet/signer";

import relay from "../../../../../../../go/workers/internal/bridge/testdata/relay-quote-solana-base-usdc.json";
import snapshot from "../../../../../../packages/wallet-core/src/lib/solana/bridge-out-preflight.testdata.json";
import { usdcForDollars, usdcRaw } from "../amount";
import {
  outgoingReceipt,
  quoteOut,
  reconcileReceipt,
  sendReviewedQuote,
  transferStep,
  type Request,
} from "../transfer";

const payer = Keypair.fromSeed(
  Uint8Array.from({ length: 32 }, (_, i) => i + 1)
);
const wallet = payer.publicKey.toBase58();
const data = relay.steps[0].items[0].data;
const ix = data.instructions[0];
const requestId = relay.steps[0].requestId;
const input = {
  chain: "base",
  asset: "usdc",
  recipient: relay.details.recipient,
  amountRaw: relay.details.currencyIn.amount,
};
const ata = getAssociatedTokenAddressSync(
  new PublicKey(ix.keys[4].pubkey),
  payer.publicKey
).toBase58();
const signal = new AbortController().signal;
const savedFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = savedFetch;
  jest.restoreAllMocks();
});

function fixture() {
  const quote: BridgeOutQuoteResponse = {
    requestId,
    instructions: [
      {
        ...ix,
        keys: ix.keys.map((k, i) => ({
          ...k,
          pubkey: i === 1 || i === 2 ? wallet : i === 5 ? ata : k.pubkey,
        })),
      },
    ],
    lookupTables: data.addressLookupTableAddresses,
    amountOutRaw: relay.details.currencyOut.amount,
    amountOutMinRaw: relay.details.currencyOut.minimumAmount,
    rate: "0.995",
    amountInUsd: relay.details.currencyIn.amountUsd,
    amountOutUsd: relay.details.currencyOut.amountUsd,
    networkFeeUsd: relay.fees.gas.amountUsd,
    loyalFeeRaw: relay.fees.app.amount,
    loyalFeeBps: 40,
    slippageBps: 100,
    etaSeconds: 30,
  };
  const confirmed: BridgeTransfer = {
    requestId,
    ...input,
    amountInRaw: input.amountRaw,
    amountOutRaw: quote.amountOutRaw,
    direction: "out",
    state: "quoted",
    inFlight: true,
    quotedAt: new Date().toISOString(),
    confirmedAt: new Date().toISOString(),
    originTx: null,
    destinationTx: null,
    refundTx: null,
    submittedAt: null,
    settledAt: null,
  };
  const ledger = new Map<string, string>();
  const store = {
    getString: (k: string) => ledger.get(k),
    setString: (k: string, v: string) => {
      ledger.set(k, v);
    },
    delete: (k: string) => {
      ledger.delete(k);
    },
  };
  const receipts = outgoingReceipt(store, wallet);
  const events: string[] = [];
  const rows: BridgeTransfer[] = [];
  const request: Request = async (path, options) => {
    if (path.endsWith("/out/quote")) {
      expect(options?.body).toEqual(input);
      return quote;
    }
    if (path.endsWith("?direction=out")) return { transfers: rows };
    if (path.endsWith(requestId + "/confirm")) {
      events.push("confirm");
      return confirmed;
    }
    if (path.endsWith(requestId + "/submit")) {
      const { originTx } = options?.body as { originTx: string };
      expect(originTx).toBe(receipts.read()?.signature);
      events.push("submit");
      return { ...confirmed, state: "submitted", originTx };
    }
    throw new Error(path);
  };
  let simulatedMessage: Buffer;
  let badSimulation = false;
  let status: null | { confirmationStatus: string; err: unknown } = null;
  let finalizedHeight = 80;
  let rpcSlot = 123;
  const [oldWallet, oldAta, vaultAta] = Object.keys(snapshot.pre);
  function accounts(addresses: string[], after: boolean) {
    const source: Record<
      string,
      { lamports: number; data: string; owner: string }
    > = after ? snapshot.post : snapshot.pre;
    return addresses.map((address) => {
      const account =
        source[
          address === wallet ? oldWallet : address === ata ? oldAta : address
        ];
      const bytes = Buffer.from(account.data, "base64");
      if (address === ata) {
        bytes.set(payer.publicKey.toBytes(), 32);
        bytes.writeBigUInt64LE(
          BigInt(100_000_000) - (after ? BigInt(input.amountRaw) : BigInt(0)),
          64
        );
      }
      if (address === vaultAta)
        bytes.writeBigUInt64LE(
          BigInt(91_000_000) + (after ? BigInt(input.amountRaw) : BigInt(0)),
          64
        );
      return { ...account, data: [bytes.toString("base64"), "base64"] };
    });
  }
  globalThis.fetch = Object.assign(
    async (url: unknown, init?: { body?: unknown }) => {
      expect(url).toBe("https://synthetic.invalid");
      const [pre, sim] = JSON.parse(String(init?.body));
      const simulated = VersionedTransaction.deserialize(
        Buffer.from(sim.params[0], "base64")
      );
      expect(
        simulated.message.addressTableLookups[0].accountKey.toBase58()
      ).toBe(quote.lookupTables![0]);
      expect(simulated.signatures[0]).toEqual(new Uint8Array(64));
      simulatedMessage = Buffer.from(simulated.message.serialize());
      events.push("simulate");
      return new Response(
        JSON.stringify([
          {
            id: pre.id,
            result: {
              context: { slot: 62 },
              value: accounts(pre.params[0], false),
            },
          },
          {
            id: sim.id,
            result: {
              context: { slot: 62 },
              value: {
                err: badSimulation ? "InsufficientFundsForFee" : null,
                accounts: accounts(sim.params[1].accounts.addresses, true),
              },
            },
          },
        ])
      );
    },
    { preconnect: savedFetch.preconnect }
  );
  const table = new AddressLookupTableAccount({
    key: new PublicKey(quote.lookupTables![0]),
    state: {
      deactivationSlot: BigInt("18446744073709551615"),
      lastExtendedSlot: 1,
      lastExtendedSlotStartIndex: 0,
      addresses: ix.keys
        .filter((_, i) => i !== 1 && i !== 2 && i !== 5)
        .map((k) => new PublicKey(k.pubkey)),
    },
  });
  const connection = {
    rpcEndpoint: "https://synthetic.invalid",
    getLatestBlockhash: async () => ({
      blockhash: Keypair.generate().publicKey.toBase58(),
      lastValidBlockHeight: 90,
    }),
    getAddressLookupTable: async (key: PublicKey) => {
      expect(key.equals(table.key)).toBe(true);
      return { value: table };
    },
    sendRawTransaction: jest.fn(async (wire: Uint8Array) => {
      const signed = VersionedTransaction.deserialize(wire);
      expect(Buffer.from(signed.message.serialize())).toEqual(simulatedMessage);
      expect(receipts.read()).toEqual({
        requestId,
        signature: bs58.encode(signed.signatures[0]),
        lastValidBlockHeight: 90,
      });
      events.push("broadcast");
      throw new Error("lost RPC response");
    }),
    getEpochInfo: async () => ({
      blockHeight: finalizedHeight,
      absoluteSlot: 122,
    }),
    getSignatureStatuses: async () => ({
      context: { slot: rpcSlot },
      value: [status],
    }),
  };
  const activeSigner = new LocalKeypairSigner(payer);
  const sign = jest.spyOn(activeSigner, "signTransaction");
  const options = {
    request,
    connection: connection as unknown as Connection,
    currentSigner: async (): Promise<Signer> => activeSigner,
    assertCurrent: () => {},
    receipts,
    signal,
    ignored: new Set<string>(),
  };
  const review = () => quoteOut(request, wallet, input, signal);
  const send = async () =>
    sendReviewedQuote({ ...options, review: await review() });
  return {
    quote,
    confirmed,
    rows,
    connection,
    options,
    store,
    receipts,
    events,
    sign,
    review,
    send,
    corruptSimulation: () => {
      badSimulation = true;
    },
    confirmSignature: () => {
      status = { confirmationStatus: "confirmed", err: null };
    },
    expire: (slot: number) => {
      finalizedHeight = 91;
      rpcSlot = slot;
    },
  };
}

test("the reviewed deposit is the simulated and signed v0 wire, saved before its first broadcast and recovered without signing again", async () => {
  const f = fixture();
  await expect(f.send()).resolves.toEqual({ kind: "bridging", requestId });
  const old = f.receipts.read()!;
  expect(f.events).toEqual(["simulate", "confirm", "broadcast"]);
  const reopened = outgoingReceipt(f.store, wallet);
  expect(reopened.read()).toEqual(old);
  expect(
    outgoingReceipt(f.store, Keypair.generate().publicKey.toBase58()).read()
  ).toBeNull();
  const review = await f.review();
  await sendReviewedQuote({ ...f.options, receipts: reopened, review });
  const recover = (request = f.options.request) =>
    reconcileReceipt(reopened, old, [], request, f.options.connection, signal);
  await recover();
  expect(f.events).not.toContain("submit");
  f.confirmSignature();
  await expect(
    recover(async () => {
      throw Error("API offline");
    })
  ).rejects.toThrow("offline");
  expect(reopened.read()).toEqual(old);
  await expect(recover()).resolves.toEqual({ kind: "bridging", requestId });
  expect(reopened.read()).toEqual(old);
  const afterFill = await reconcileReceipt(
    reopened,
    old,
    [
      {
        ...f.confirmed,
        state: "filled",
        inFlight: false,
        originTx: old.signature,
      },
    ],
    f.options.request,
    f.options.connection,
    signal
  );
  expect(afterFill).toEqual({ kind: "filled", requestId });
  expect(reopened.read()).toBeNull();
  expect(f.sign).toHaveBeenCalledTimes(1);
  expect(f.events).toEqual(["simulate", "confirm", "broadcast", "submit"]);
});

test("a tracked unsigned approval stops showing progress when its reservation expires", () => {
  const { confirmed } = fixture();
  expect(transferStep(confirmed).kind).toBe("bridging");
  expect(transferStep({ ...confirmed, inFlight: false })).toMatchObject({
    kind: "failed",
    requestId,
  });
});

test("a crash between saving and sending never records a nonexistent transaction or unlocks on a lagging RPC", async () => {
  const f = fixture();
  const row = {
    requestId,
    signature: bs58.encode(Uint8Array.from({ length: 64 }, () => 7)),
    lastValidBlockHeight: 90,
  };
  f.receipts.remember(row);
  const recover = () =>
    reconcileReceipt(
      f.receipts,
      row,
      [],
      f.options.request,
      f.options.connection,
      signal
    );
  f.expire(121);
  expect((await recover()).kind).toBe("bridging");
  expect(f.receipts.read()).toEqual(row);
  f.expire(122);
  expect((await recover()).kind).toBe("failed");
  expect(f.receipts.read()).toBeNull();
  expect(f.events).toEqual([]);
});

test("a modified deposit, unknown program, signer, or ALT cannot reach signing", async () => {
  for (const alter of [
    (q: BridgeOutQuoteResponse) => {
      q.instructions![0].programId = "11111111111111111111111111111111";
    },
    (q: BridgeOutQuoteResponse) => {
      q.instructions![0].data = q.instructions![0].data.replace(
        /^(\w{16})\w{2}/,
        "$1ff"
      );
    },
    (q: BridgeOutQuoteResponse) => {
      q.instructions![0].keys![5].pubkey = wallet;
    },
    (q: BridgeOutQuoteResponse) => {
      q.lookupTables = [wallet];
    },
  ]) {
    const f = fixture();
    alter(f.quote);
    await expect(f.send()).rejects.toThrow("does not match");
    expect(f.sign).not.toHaveBeenCalled();
    expect(f.events).toEqual([]);
  }
});

test("insufficient gas, a wrong confirm response, and an in-flight send cannot reach signing", async () => {
  const sol = fixture();
  sol.corruptSimulation();
  await expect(sol.send()).rejects.toThrow("Add SOL");
  expect(sol.sign).not.toHaveBeenCalled();
  const wrong = fixture();
  wrong.confirmed.asset = "usdt";
  await expect(wrong.send()).rejects.toThrow("did not confirm");
  expect(wrong.sign).not.toHaveBeenCalled();
  const other = fixture();
  other.rows.push({
    ...other.confirmed,
    requestId: "previous",
    state: "submitted",
  });
  expect(await other.send()).toEqual({
    kind: "bridging",
    requestId: "previous",
  });
  expect(other.sign).not.toHaveBeenCalled();
});

test("expired approval, wallet changes and an external wallet's modified returned message cannot broadcast", async () => {
  const expired = fixture();
  const review = await expired.review();
  review.requestedAt -= 30_000;
  await expect(
    sendReviewedQuote({ ...expired.options, review })
  ).rejects.toThrow("quote expired");
  expect(expired.sign).not.toHaveBeenCalled();
  const switched = fixture();
  const oldOwner = await switched.options.currentSigner();
  let active = oldOwner;
  switched.options.currentSigner = async () => active;
  switched.sign.mockImplementationOnce(async (tx) => {
    (tx as VersionedTransaction).sign([payer]);
    active = new LocalKeypairSigner(Keypair.generate());
    return tx;
  });
  await expect(switched.send()).rejects.toThrow("wallet changed");
  expect(switched.connection.sendRawTransaction).not.toHaveBeenCalled();
  const changed = fixture();
  changed.sign.mockImplementationOnce(async (tx) => {
    (tx as VersionedTransaction).message.recentBlockhash =
      Keypair.generate().publicKey.toBase58();
    (tx as VersionedTransaction).sign([payer]);
    return tx;
  });
  await expect(changed.send()).rejects.toThrow(
    "wallet changed the checked transaction"
  );
  expect(changed.connection.sendRawTransaction).not.toHaveBeenCalled();
  const persistence = fixture();
  persistence.store.setString = () => {
    throw Error("disk full");
  };
  await expect(persistence.send()).rejects.toThrow("disk full");
  expect(persistence.connection.sendRawTransaction).not.toHaveBeenCalled();
});

test("input keeps six exact USDC decimals, including amounts above JavaScript's safe integer range", () => {
  expect(usdcRaw("9007199254.740993")).toBe(BigInt("9007199254740993"));
  expect(usdcRaw(".000001")).toBe(BigInt(1));
  for (const value of [
    "0.0000001",
    "1.2.3",
    "0x10",
    "1e2",
    "-1",
    "18446744073709.551616",
  ])
    expect(usdcRaw(value)).toBe(BigInt(0));
});

test("Privy's separately returned signed wire is verified, and a missing wallet signature prevents any broadcast", async () => {
  const f = fixture();
  const provider = {
    async request({
      method,
      params,
    }: {
      method: string;
      params: { transaction: VersionedTransaction };
    }) {
      expect(method).toBe("signTransaction");
      const signedTransaction = VersionedTransaction.deserialize(
        params.transaction.serialize()
      );
      signedTransaction.sign([payer]);
      return { signedTransaction };
    },
  };
  const privy = new PrivyEmbeddedSigner(
    provider as ConstructorParameters<typeof PrivyEmbeddedSigner>[0],
    wallet
  );
  f.options.currentSigner = async () => privy;
  expect(await f.send()).toEqual({ kind: "bridging", requestId });
  expect(f.connection.sendRawTransaction).toHaveBeenCalledTimes(1);
  const unsigned = fixture();
  unsigned.sign.mockImplementationOnce(async (tx) => tx);
  await expect(unsigned.send()).rejects.toThrow(/invalid signature/);
  expect(unsigned.connection.sendRawTransaction).not.toHaveBeenCalled();
});

test("a route edit while the external wallet is approving invalidates the signed transaction", async () => {
  const f = fixture();
  let sameRoute = true;
  f.options.assertCurrent = () => {
    if (!sameRoute) throw new Error("recipient changed");
  };
  f.sign.mockImplementationOnce(async (tx) => {
    (tx as VersionedTransaction).sign([payer]);
    sameRoute = false;
    return tx;
  });
  await expect(f.send()).rejects.toThrow("recipient changed");
  expect(f.receipts.read()).toBeNull();
  expect(f.connection.sendRawTransaction).not.toHaveBeenCalled();
});

test("a dollar amount uses exact decimal division through the wire amount", () => {
  expect(usdcForDollars("9007199254.740993", 1)).toBe("9007199254.740993");
  expect(usdcForDollars("10", 0.99)).toBe("10.10101");
  expect(usdcForDollars("1.000001", 2)).toBe("0.500001");
  expect(usdcForDollars(".01", null)).toBe("");
});
