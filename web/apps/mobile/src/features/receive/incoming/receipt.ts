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
    return JSON.parse(store.getString(key) ?? "[]") as PendingReceipt[];
  }
  function write(receipts: PendingReceipt[]) {
    if (receipts.length) store.setString(key, JSON.stringify(receipts));
    else store.delete(key);
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
