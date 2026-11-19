import { signature } from "@solana/kit";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { feeReceipt, type Receipt } from "./earn-max-fee-receipt.js";

async function main(signatures: string[]) {
  if (!signatures.length || signatures.length > 32 || new Set(signatures).size !== signatures.length) {
    throw Error("Supply 1 to 32 unique Earn Max transaction signatures");
  }
  for (const value of signatures) signature(value);
  const endpoint = process.env.SOLANA_RPC_URL;
  if (!endpoint) throw Error("Set SOLANA_RPC_URL to a mainnet archive RPC");

  async function read<T>(method: string, params: unknown[]): Promise<T> {
    try {
      const response = await fetch(endpoint!, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
        signal: AbortSignal.timeout(15_000),
      });
      if (!response.ok) throw Error();
      const payload = (await response.json()) as { result?: T; error?: unknown };
      if (payload.result == null || payload.error) throw Error();
      return payload.result;
    } catch {
      throw Error(`Missing finalized RPC evidence (${method})`);
    }
  }

  if ((await read<string>("getGenesisHash", [])) !== RWA_MULTIPLY_ROUTE.genesisHash) {
    throw Error("Expected Solana mainnet");
  }
  const finalized = { commitment: "finalized", maxSupportedTransactionVersion: 1 };
  const blocks = new Map<number, string[]>();
  const receipts = [];
  for (const sig of signatures) {
    const tx = await read<Receipt>("getTransaction", [sig, { ...finalized, encoding: "json" }]);
    if (!Number.isSafeInteger(tx.slot) || tx.slot <= 0) throw Error("Invalid finalized transaction slot");
    let signaturesInBlock = blocks.get(tx.slot);
    if (!signaturesInBlock) {
      const block = await read<{ signatures: string[] }>("getBlock", [
        tx.slot,
        { ...finalized, transactionDetails: "signatures", rewards: false },
      ]);
      signaturesInBlock = block.signatures;
      blocks.set(tx.slot, signaturesInBlock);
    }
    receipts.push(feeReceipt(tx, sig, signaturesInBlock));
  }
  receipts.sort((a, b) => a.slot - b.slot || a.transactionIndex - b.transactionIndex);
  console.log(
    JSON.stringify(
      { vault: RWA_MULTIPLY_ROUTE.vault.address, receipts },
      (_, value: unknown) => (typeof value === "bigint" ? value.toString() : value),
      2,
    ),
  );
}

main(process.argv.slice(2)).catch((error: Error) => {
  console.error(error.message);
  process.exitCode = 1;
});
