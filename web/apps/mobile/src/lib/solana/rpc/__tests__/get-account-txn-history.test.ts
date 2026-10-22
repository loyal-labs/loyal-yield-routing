import { PublicKey } from "@solana/web3.js";

import { getAccountTransactionHistory } from "../get-account-txn-history";

const mockGetConnection = jest.fn();

jest.mock("../connection", () => ({
  getConnection: () => mockGetConnection(),
}));

jest.mock("@loyal-labs/shared", () => ({
  isDustSolTransfer: () => false,
  isDustTokenTransfer: () => false,
  SOL_DUST_THRESHOLD_LAMPORTS: 0,
}));

const WALLET = "11111111111111111111111111111112";
const COUNTERPARTY = "7vfCXTUXx5pPAtF5NCz7L3A5kZZgwyUrKvYzUKGwEuUq";

function transactionWith(signature: string, slot: number) {
  return {
    blockTime: 1_700_000_000,
    slot,
    meta: {
      err: null,
      fee: 5_000,
      preBalances: [2_000_000_000, 1_000_000_000],
      postBalances: [1_500_000_000, 1_500_000_000],
      preTokenBalances: [],
      postTokenBalances: [],
      innerInstructions: [],
    },
    transaction: {
      signatures: [signature],
      message: {
        accountKeys: [
          { pubkey: WALLET, signer: true, writable: true },
          { pubkey: COUNTERPARTY, signer: false, writable: true },
        ],
        instructions: [
          {
            program: "system",
            parsed: {
              type: "transfer",
              info: { source: WALLET, destination: COUNTERPARTY },
            },
          },
        ],
      },
    },
  };
}

describe("getAccountTransactionHistory", () => {
  test("keys each transaction by its own signature when the batch comes back reordered", async () => {
    mockGetConnection.mockReturnValue({
      getSignaturesForAddress: async () => [
        { signature: "sigNew" },
        { signature: "sigOld" },
      ],
      getParsedTransactions: async () => [
        transactionWith("sigOld", 100),
        transactionWith("sigUnrequested", 300),
        transactionWith("sigNew", 200),
      ],
    });

    const { transfers } = await getAccountTransactionHistory(
      new PublicKey(WALLET),
    );

    expect(transfers.map((t) => [t.signature, t.slot])).toEqual([
      ["sigNew", 200],
      ["sigOld", 100],
    ]);
  });
});
