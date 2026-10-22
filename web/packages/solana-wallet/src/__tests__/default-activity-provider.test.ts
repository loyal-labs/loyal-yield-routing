import { describe, expect, test } from "bun:test";
import { type Connection, PublicKey } from "@solana/web3.js";

import { solTransferFixture } from "../__fixtures__/activity-fixtures";
import { WALLET_ADDRESS } from "../__fixtures__/asset-fixtures";
import { createRpcActivityProvider } from "../providers/default-activity-provider";

function transactionWith(signature: string, slot: number) {
  const tx = solTransferFixture();
  tx.slot = slot;
  tx.transaction.signatures = [signature];
  return tx;
}

describe("createRpcActivityProvider.getActivity", () => {
  test("keys each transaction by its own signature when the batch comes back reordered", async () => {
    const requested = ["sigNew", "sigOld"];
    let pages = 0;
    const connection = {
      getSignaturesForAddress: async () =>
        pages++ === 0 ? requested.map((signature) => ({ signature })) : [],
      getParsedTransactions: async () => [
        transactionWith("sigOld", 100),
        transactionWith("sigUnrequested", 300),
        transactionWith("sigNew", 200),
      ],
    } as unknown as Connection;
    const provider = createRpcActivityProvider({
      rpcEndpoint: "http://rpc.test",
      websocketEndpoint: "ws://rpc.test",
      commitment: "confirmed",
      config: { createRpcConnection: () => connection },
    });

    const { activities } = await provider.getActivity(
      new PublicKey(WALLET_ADDRESS)
    );

    expect(activities.map((a) => [a.signature, a.slot])).toEqual([
      ["sigNew", 200],
      ["sigOld", 100],
    ]);
  });
});
