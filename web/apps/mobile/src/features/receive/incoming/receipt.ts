import type { BridgeTransfer } from "@loyal-labs/api";

export type PendingReceipt = {
  requestId: string;
  hash: string;
  chainId: number;
  chain: string;
  asset: string;
  symbol: string;
};

type Store = {
  getString: (key: string) => string | undefined;
  setString: (key: string, value: string) => void;
  delete: (key: string) => void;
};

// The hash is public. A receipt belongs to the exact SOL recipient and EVM signing account.
export function receiptStore(store: Store, solana: string, sender: string) {
  const key = `loyal:incoming:${solana}:${sender.toLowerCase()}`;
  function read(): PendingReceipt[] {
    try {
      const rows: unknown = JSON.parse(store.getString(key) ?? "[]");
      return Array.isArray(rows)
        ? rows.filter(
            (r) =>
              r &&
              typeof r.requestId === "string" &&
              /^0x[0-9a-f]{64}$/i.test(r.hash) &&
              Number.isSafeInteger(r.chainId) &&
              r.chainId > 0 &&
              typeof r.chain === "string" &&
              typeof r.asset === "string" &&
              typeof r.symbol === "string",
          )
        : [];
    } catch {
      return [];
    }
  }
  function write(receipts: PendingReceipt[]) {
    try {
      if (receipts.length) store.setString(key, JSON.stringify(receipts));
      else store.delete(key);
    } catch {
      // A device storage failure must still let /submit record the hash; Go also reconciles confirmed quotes.
    }
  }
  return {
    read,
    remember(receipt: PendingReceipt) {
      write([...read().filter((r) => r.requestId !== receipt.requestId), receipt]);
    },
    forget(requestId: string) {
      write(read().filter((r) => r.requestId !== requestId));
    },
  };
}

export function recordedReceipt(transfers: BridgeTransfer[], requestId: string) {
  return transfers.some(
    (t) => t.requestId === requestId && t.direction === "in" && t.state !== "quoted",
  );
}
