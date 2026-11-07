import { expect, test } from "bun:test";
import { Connection } from "@solana/web3.js";

import { confirmSentTransaction } from "./confirm-sent-transaction";

const SIGNATURE = "1".repeat(64);
const BLOCKHASH = "11111111111111111111111111111111";

test("a transaction that lands while the socket is down counts as confirmed", async () => {
  let height = 1000;
  let landed = false;
  const stub = Bun.serve({
    port: 0,
    fetch(request, server) {
      if (server.upgrade(request)) {
        return undefined;
      }
      return request.json().then(({ id, method }) => {
        const result =
          method === "getBlockHeight"
            ? (height += 1)
            : {
                context: { slot: 5 },
                value: [
                  landed
                    ? {
                        confirmationStatus: "confirmed",
                        confirmations: 1,
                        err: null,
                        slot: 5,
                        status: { Ok: null },
                      }
                    : null,
                ],
              };
        return Response.json({ id, jsonrpc: "2.0", result });
      });
    },
    websocket: {
      message(socket, frame) {
        const { id, method } = JSON.parse(String(frame));
        if (method === "ping") {
          return;
        }
        socket.send(JSON.stringify({ id, jsonrpc: "2.0", result: 7 }));
        if (method === "signatureSubscribe" && !landed) {
          setTimeout(() => {
            socket.close(1001);
            landed = true;
          }, 300);
        }
      },
    },
  });
  try {
    const connection = new Connection(`http://127.0.0.1:${stub.port}/`, {
      wsEndpoint: `ws://127.0.0.1:${stub.port}/`,
    });
    await confirmSentTransaction(connection, {
      blockhash: BLOCKHASH,
      lastValidBlockHeight: height + 8,
      signature: SIGNATURE,
    });
  } finally {
    stub.stop(true);
  }
  expect(landed).toBe(true);
}, 30_000);
