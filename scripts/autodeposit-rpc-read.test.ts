import { describe, expect, test } from "bun:test";
import { PublicKey, type Connection } from "@solana/web3.js";
import {
  AutodepositRpcReadError,
  readAutodepositPreSendBalance,
  type AutodepositRpcFetch,
} from "./autodeposit-rpc-read";
import { runAutodepositExecutorWithFailureBoundary } from "./execute-autodeposit-policy";

const context = {
  executorStage: "read_wallet_balance" as const,
  targetId: "123",
  scheduledSlotId: "456",
};
const account = new PublicKey("11111111111111111111111111111111");
const args = {
  rpcUrl: "https://rpc.invalid/private-test-credential",
  context,
  read: async (connection: Connection) => {
    const balance = await connection.getTokenAccountBalance(account, "confirmed");
    return BigInt(balance.value.amount);
  },
};

function replies(statuses: number[]) {
  let calls = 0;
  const fetch: AutodepositRpcFetch = async (_url, init) => {
    const request = JSON.parse(String(init?.body));
    // This test exercises the actual web3 RPC request, never a transaction send.
    expect(request.method).toBe("getTokenAccountBalance");
    expect(request.params[1]).toEqual({ commitment: "confirmed" });
    const status = statuses[calls++] ?? statuses.at(-1)!;
    if (status !== 200) {
      return new Response("provider echoed https://rpc.invalid/private-test-credential", { status });
    }
    return Response.json({
      jsonrpc: "2.0",
      id: request.id,
      result: { context: { slot: 1 }, value: { amount: "42", decimals: 6, uiAmount: 0.000042 } },
    });
  };
  return { fetch, calls: () => calls };
}

async function failureOf(promise: Promise<unknown>): Promise<AutodepositRpcReadError> {
  try {
    await promise;
  } catch (error) {
    expect(error).toBeInstanceOf(AutodepositRpcReadError);
    return error as AutodepositRpcReadError;
  }
  throw new Error("Expected failure");
}

describe("bounded pre-send autodeposit RPC balance reads", () => {
  test.each([500, 502, 503, 504])("retries HTTP %s then returns the actual balance", async (status) => {
    const transport = replies([status, 200]);
    expect(await readAutodepositPreSendBalance(args, { ...transport, retryDelayMs: 0 })).toBe(42n);
    expect(transport.calls()).toBe(2);
  });

  test("does not retry a successful read", async () => {
    const transport = replies([200]);
    expect(await readAutodepositPreSendBalance(args, transport)).toBe(42n);
    expect(transport.calls()).toBe(1);
  });

  test.each([400, 401, 403, 404, 429, 501])("does not retry HTTP %s, including SDK-hidden 429 retries", async (status) => {
    const transport = replies([status]);
    const error = await failureOf(readAutodepositPreSendBalance(args, { ...transport, retryDelayMs: 0 }));
    expect(transport.calls()).toBe(1);
    expect(error.diagnostics).toMatchObject({ httpStatus: status, attempts: 1, retries: 0, deadlineExceeded: false });
    expect(JSON.stringify(error)).not.toContain("private-test-credential");
  });

  test("does not retry a transport error or leak its URL", async () => {
    let calls = 0;
    const error = await failureOf(readAutodepositPreSendBalance(args, {
      fetch: async () => { calls++; throw new Error(args.rpcUrl); },
      retryDelayMs: 0,
    }));
    expect(calls).toBe(1);
    expect(error.diagnostics.httpStatus).toBeNull();
    expect(String(error)).not.toContain("private-test-credential");
    expect(JSON.stringify(error)).not.toContain("private-test-credential");
    expect(error.cause).toBeUndefined();
  });

  test("does not retry JSON-RPC missing-account errors or reinterpret them as zero", async () => {
    let calls = 0;
    const error = await failureOf(readAutodepositPreSendBalance(args, {
      fetch: async (_url, init) => {
        calls++;
        return Response.json({
          jsonrpc: "2.0", id: JSON.parse(String(init?.body)).id,
          error: { code: -32602, message: "Invalid param: could not find account" },
        });
      },
      retryDelayMs: 0,
    }));
    expect(calls).toBe(1);
    expect(error.diagnostics).toMatchObject({ httpStatus: 200, deadlineExceeded: false });
  });

  test("preserves the caller's pre-send missing-account policy without adding one", async () => {
    let calls = 0;
    const balance = await readAutodepositPreSendBalance({
      ...args,
      read: async (connection) => {
        try {
          return await args.read(connection);
        } catch (error) {
          // Same existing policy as getTokenBalanceRaw, before any pull only.
          if (error instanceof Error && error.message.includes("could not find account")) return 0n;
          throw error;
        }
      },
    }, {
      fetch: async (_url, init) => {
        calls++;
        return Response.json({
          jsonrpc: "2.0", id: JSON.parse(String(init?.body)).id,
          error: { code: -32602, message: "Invalid param: could not find account" },
        });
      },
    });
    expect(balance).toBe(0n);
    expect(calls).toBe(1);
  });

  test("retries only balance reads over the real local HTTP transport", async () => {
    let calls = 0;
    const server = Bun.serve({
      hostname: "127.0.0.1", port: 0,
      async fetch(request) {
        const body = await request.json() as { method: string; id: string };
        expect(body.method).toBe("getTokenAccountBalance");
        calls++;
        if (calls < 3) return new Response("untrusted provider response", { status: 503 });
        return Response.json({
          jsonrpc: "2.0", id: body.id,
          result: { context: { slot: 1 }, value: { amount: "42", decimals: 6, uiAmount: 0.000042 } },
        });
      },
    });
    try {
      expect(await readAutodepositPreSendBalance({ ...args, rpcUrl: server.url.toString() }, { retryDelayMs: 0 })).toBe(42n);
      expect(calls).toBe(3);
    } finally {
      await server.stop(true);
    }
  });

  test("exhaustion preserves the paging exit27 contract with sanitized diagnosis", async () => {
    const transport = replies([500, 502, 503]);
    let exitCode: number | undefined;
    let record: Record<string, unknown> | undefined;
    await runAutodepositExecutorWithFailureBoundary(async (recordStage) => {
      recordStage(context.executorStage);
      await readAutodepositPreSendBalance(args, { ...transport, retryDelayMs: 0 });
    }, {
      environment: { AUTODEPOSIT_DEPENDENCY_UNAVAILABLE_EXIT_CODE: "27" },
      getExitCode: () => exitCode,
      setExitCode: (code) => { exitCode = code; },
      reportFailure: (failure) => { record = failure; },
    });
    expect(transport.calls()).toBe(3);
    expect(exitCode).toBe(27);
    expect(record).toMatchObject({
      ...context, status: "error", failureCode: "dependency_unavailable", exitCode: 27,
      errorKind: "retryable_http_server_error", operation: "getTokenAccountBalance",
      httpStatus: 503, attempts: 3, retries: 2, deadlineExceeded: false,
    });
    expect(JSON.stringify(record)).not.toContain("private-test-credential");
  });

  test("does not sleep or issue another request when backoff exceeds the remaining budget", async () => {
    const transport = replies([503]);
    const error = await failureOf(readAutodepositPreSendBalance(args, {
      ...transport, totalBudgetMs: 20, retryDelayMs: 100,
    }));
    expect(error.diagnostics).toMatchObject({ httpStatus: 503, attempts: 1, deadlineExceeded: true });
    await Bun.sleep(40);
    expect(transport.calls()).toBe(1);
  });

  test("aborts a hung request by the whole-sequence deadline and still exits27", async () => {
    let calls = 0;
    let signal: AbortSignal | undefined;
    let exitCode: number | undefined;
    let record: Record<string, unknown> | undefined;
    await runAutodepositExecutorWithFailureBoundary(async (recordStage) => {
      recordStage("read_vault_balance");
      await readAutodepositPreSendBalance({ ...args, context: { ...context, executorStage: "read_vault_balance" } }, {
        totalBudgetMs: 20,
        fetch: async (_url, init) => {
          calls++;
          signal = init?.signal ?? undefined;
          return await new Promise<Response>((_, reject) => {
            signal!.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
          });
        },
      });
    }, {
      environment: { AUTODEPOSIT_DEPENDENCY_UNAVAILABLE_EXIT_CODE: "27" },
      getExitCode: () => exitCode,
      setExitCode: (code) => { exitCode = code; },
      reportFailure: (failure) => { record = failure; },
    });
    expect(exitCode).toBe(27);
    expect(signal?.aborted).toBe(true);
    expect(record).toMatchObject({
      executorStage: "read_vault_balance", errorKind: "rpc_read_deadline_exceeded",
      httpStatus: null, attempts: 1, retries: 0, deadlineExceeded: true,
    });
    await Bun.sleep(40);
    expect(calls).toBe(1);
  });

  test("bounds response body parsing, not just receipt of HTTP headers", async () => {
    let signal: AbortSignal | undefined;
    const error = await failureOf(readAutodepositPreSendBalance(args, {
      totalBudgetMs: 20,
      fetch: async (_url, init) => {
        signal = init?.signal ?? undefined;
        return new Response(new ReadableStream({
          start(controller) {
            signal!.addEventListener("abort", () => controller.error(new Error("aborted")), { once: true });
          },
        }));
      },
    }));
    expect(signal?.aborted).toBe(true);
    expect(error.diagnostics).toMatchObject({ httpStatus: 200, attempts: 1, deadlineExceeded: true });
  });
});
