import { afterEach, expect, test } from "bun:test";
import { Connection, type TransactionError } from "@solana/web3.js";

import { settleSentTransaction } from "./confirm-sent-transaction";

const SIGNATURE = "1".repeat(64);
const SLIPPAGE: TransactionError = { InstructionError: [3, { Custom: 6001 }] };

type Status = { confirmationStatus: string; err: TransactionError | null };

let stopStub = () => {};
afterEach(() => stopStub());

function stubRpc(statusAt: (elapsedMs: number) => Status | null | 429) {
  const startedAt = Date.now();
  const elapsed = () => Date.now() - startedAt;
  const server = Bun.serve({
    port: 0,
    fetch(request, bun) {
      if (bun.upgrade(request)) {
        return;
      }
      return request.json().then(({ id, method }) => {
        if (method === "getBlockHeight") {
          return Response.json({
            id,
            jsonrpc: "2.0",
            result: 1000 + Math.floor(elapsed() / 400),
          });
        }
        const status = statusAt(elapsed());
        if (status === 429) {
          return new Response("rate limited", { status: 429 });
        }
        return Response.json({
          id,
          jsonrpc: "2.0",
          result: {
            context: { slot: 5 },
            value: [status && { ...status, confirmations: 1, slot: 5 }],
          },
        });
      });
    },
    websocket: {
      message(socket, frame) {
        const { id, method } = JSON.parse(String(frame));
        if (method === "signatureSubscribe") {
          socket.send(JSON.stringify({ id, jsonrpc: "2.0", result: 7 }));
          setTimeout(() => socket.close(1001), 200);
        }
      },
    },
  });
  stopStub = () => {
    server.stop(true);
  };
  return new Connection(`http://127.0.0.1:${server.port}/`, {
    commitment: "confirmed",
    disableRetryOnRateLimit: true,
    wsEndpoint: `ws://127.0.0.1:${server.port}/`,
  });
}

test("a transaction that lands behind a dropped socket and 429 status reads reads as landed", async () => {
  const connection = stubRpc((elapsedMs) =>
    elapsedMs < 3000 ? 429 : { confirmationStatus: "confirmed", err: null }
  );
  expect(
    await settleSentTransaction(connection, {
      lastValidBlockHeight: 1030,
      signature: SIGNATURE,
    })
  ).toEqual({ kind: "landed" });
}, 20_000);

test("a failure first seen at processed waits for confirmed and reads as failed", async () => {
  const connection = stubRpc((elapsedMs) => ({
    confirmationStatus: elapsedMs < 1000 ? "processed" : "confirmed",
    err: SLIPPAGE,
  }));
  expect(
    await settleSentTransaction(connection, {
      lastValidBlockHeight: 1030,
      signature: SIGNATURE,
    })
  ).toEqual({ err: SLIPPAGE, kind: "failed" });
}, 20_000);
