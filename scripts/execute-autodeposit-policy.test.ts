import { describe, expect, test } from "bun:test";
import { Keypair, type Connection } from "@solana/web3.js";

import {
  assertDurableExecuteIdentity,
  computeSweepAmount,
  parseKeypairSecret,
  redactSensitiveText,
  reconcilePersistedAttempt,
  runAfterExecutablePreflight,
  runAfterFeePayerSolSafety,
  type SameMintTopUpResult,
} from "./execute-autodeposit-policy";

function hex(bytes: Uint8Array): string {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join(
    ""
  );
}

describe("computeSweepAmount", () => {
  test("no-ops when the wallet balance is at or below the floor", () => {
    expect(
      computeSweepAmount({
        walletBalanceRaw: BigInt(200),
        walletBalanceFloorRaw: BigInt(200),
        maxAmountPerPeriodRaw: BigInt(50),
      })
    ).toEqual({ kind: "no_excess", excessRaw: BigInt(0) });

    expect(
      computeSweepAmount({
        walletBalanceRaw: BigInt(199),
        walletBalanceFloorRaw: BigInt(200),
        maxAmountPerPeriodRaw: null,
      })
    ).toEqual({ kind: "no_excess", excessRaw: BigInt(-1) });
  });

  test("sweeps the full excess when the cap is absent or above excess", () => {
    expect(
      computeSweepAmount({
        walletBalanceRaw: BigInt(350),
        walletBalanceFloorRaw: BigInt(200),
        maxAmountPerPeriodRaw: null,
      })
    ).toEqual({
      kind: "sweep",
      amountRaw: BigInt(150),
      excessRaw: BigInt(150),
      capped: false,
      cappedByMaxPerPeriod: false,
      cappedByRemainingAllowance: false,
    });
  });

  test("caps the sweep by max amount per period", () => {
    expect(
      computeSweepAmount({
        walletBalanceRaw: BigInt(350),
        walletBalanceFloorRaw: BigInt(200),
        maxAmountPerPeriodRaw: BigInt(80),
      })
    ).toEqual({
      kind: "sweep",
      amountRaw: BigInt(80),
      excessRaw: BigInt(150),
      capped: true,
      cappedByMaxPerPeriod: true,
      cappedByRemainingAllowance: false,
    });
  });

  test("caps the sweep by remaining subscription allowance", () => {
    expect(
      computeSweepAmount({
        walletBalanceRaw: BigInt(350),
        walletBalanceFloorRaw: BigInt(200),
        maxAmountPerPeriodRaw: BigInt(200),
        remainingAllowanceRaw: BigInt(60),
      })
    ).toEqual({
      kind: "sweep",
      amountRaw: BigInt(60),
      excessRaw: BigInt(150),
      capped: true,
      cappedByMaxPerPeriod: false,
      cappedByRemainingAllowance: true,
    });
  });

  test("no-ops when subscription allowance is exhausted", () => {
    expect(
      computeSweepAmount({
        walletBalanceRaw: BigInt(350),
        walletBalanceFloorRaw: BigInt(200),
        maxAmountPerPeriodRaw: BigInt(200),
        remainingAllowanceRaw: BigInt(0),
      })
    ).toEqual({
      kind: "allowance_exhausted",
      excessRaw: BigInt(150),
      remainingAllowanceRaw: BigInt(0),
    });
  });
});

describe("top-up fee-payer SOL safety", () => {
  test("rejects before invoking the pull when the fee payer is below the minimum", async () => {
    const feePayer = Keypair.generate().publicKey;
    let pullCalls = 0;
    const connection = {
      getBalance: async (address: typeof feePayer, commitment: string) => {
        expect(address.toBase58()).toBe(feePayer.toBase58());
        expect(commitment).toBe("confirmed");
        return 49_999_999;
      },
    } as unknown as Pick<Connection, "getBalance">;

    await expect(
      runAfterFeePayerSolSafety({
        connection,
        feePayer,
        run: async () => {
          pullCalls += 1;
          return "pull-sent";
        },
      })
    ).rejects.toThrow("Refusing to pull user funds");
    expect(pullCalls).toBe(0);
  });

  test("invokes the pull exactly once at the configured minimum", async () => {
    const feePayer = Keypair.generate().publicKey;
    let pullCalls = 0;
    const connection = {
      getBalance: async () => 50_000_000,
    } as unknown as Pick<Connection, "getBalance">;

    const result = await runAfterFeePayerSolSafety({
      connection,
      feePayer,
      run: async () => {
        pullCalls += 1;
        return "pull-sent";
      },
    });

    expect(pullCalls).toBe(1);
    expect(result).toEqual({
      result: "pull-sent",
      safety: {
        feePayer: feePayer.toBase58(),
        balanceLamports: 50_000_000,
        minimumLamports: 50_000_000,
        commitment: "confirmed",
        checked: true,
      },
    });
  });
});

describe("pre-pull Kamino obligation gate", () => {
  test("a missing obligation sends no pull and a later ready invocation completes with one pull", async () => {
    const pullSimulation = { err: null, logs: [], unitsConsumed: 1 };
    const missingObligation: SameMintTopUpResult = {
      command: ["same-mint-reserve-swap"],
      exitCode: 0,
      stdout: "",
      stderr: "",
      json: {
        preflightBlockers: ["deposit obligation is missing"],
        missingObligationSetup: { obligation: "missing-obligation" },
        policyDepositTransaction: {
          simulationError: null,
          simulationSkippedReason: "init obligation must land before deposit simulation",
        },
      },
    };
    let pullCalls = 0;

    await expect(
      runAfterExecutablePreflight({
        pullSimulation,
        topUpDryRun: missingObligation,
        run: async () => {
          pullCalls += 1;
          return "completed";
        },
      })
    ).rejects.toThrow("refusing to pull user funds");
    expect(pullCalls).toBe(0);

    const readyTopUp: SameMintTopUpResult = {
      ...missingObligation,
      json: {
        preflightBlockers: [],
        missingObligationSetup: null,
        policyDepositTransaction: {
          simulationError: null,
          simulationSkippedReason: null,
        },
      },
    };
    const result = await runAfterExecutablePreflight({
      pullSimulation,
      topUpDryRun: readyTopUp,
      run: async () => {
        pullCalls += 1;
        return "completed";
      },
    });

    expect(result).toBe("completed");
    expect(pullCalls).toBe(1);
  });
});

describe("persisted signature reconciliation", () => {
  test("keeps a processed signature unknown after its blockhash expires", async () => {
    const connection = {
      getSignatureStatuses: async () => ({
        value: [
          {
            err: null,
            confirmationStatus: "processed",
            slot: 123,
          },
        ],
      }),
      getBlockHeight: async () => 999,
    } as unknown as Connection;

    await expect(
      reconcilePersistedAttempt({
        attempt: {
          id: "1",
          executionId: "1",
          operationKind: "top_up",
          attemptNumber: 1,
          signature: "processed-signature",
          blockhash: "old-blockhash",
          lastValidBlockHeight: BigInt(100),
          signedTransactionBase64: "signed-bytes",
          classification: "unknown",
          broadcastAt: null,
        },
        connection,
        waitForConfirmation: false,
      }),
    ).resolves.toEqual({ classification: "unknown", error: null });
  });

  test("expires only a signature absent from status history", async () => {
    const connection = {
      getSignatureStatuses: async () => ({ value: [null] }),
      getBlockHeight: async () => 101,
    } as unknown as Connection;

    await expect(
      reconcilePersistedAttempt({
        attempt: {
          id: "1",
          executionId: "1",
          operationKind: "top_up",
          attemptNumber: 1,
          signature: "absent-signature",
          blockhash: "old-blockhash",
          lastValidBlockHeight: BigInt(100),
          signedTransactionBase64: "signed-bytes",
          classification: "unknown",
          broadcastAt: null,
        },
        connection,
        waitForConfirmation: false,
      }),
    ).resolves.toEqual({ classification: "expired_not_landed", error: null });
  });
});

describe("durable execution identity", () => {
  test("refuses live execution without a real claim and scheduled slot", () => {
    expect(() =>
      assertDurableExecuteIdentity({
        execute: true,
        requireLotClaim: false,
        claimToken: null,
        scheduledSlotId: null,
      })
    ).toThrow("for durable recovery");
  });

  test("allows planning without durable ownership and execution with it", () => {
    expect(() =>
      assertDurableExecuteIdentity({
        execute: false,
        requireLotClaim: false,
        claimToken: null,
        scheduledSlotId: null,
      })
    ).not.toThrow();
    expect(() =>
      assertDurableExecuteIdentity({
        execute: true,
        requireLotClaim: true,
        claimToken: "claim",
        scheduledSlotId: BigInt(1),
      })
    ).not.toThrow();
  });
});

describe("durable execution log redaction", () => {
  test("redacts RPC credentials and replayable signed bytes", () => {
    const previousRpcUrl = process.env.SOLANA_RPC_URL;
    process.env.SOLANA_RPC_URL = "https://rpc.example.test/?api-key=secret-value";
    try {
      const redacted = redactSensitiveText(
        '{"rpc":"https://rpc.example.test/?api-key=secret-value","signedTransactionBase64":"replayable"}'
      );
      expect(redacted).not.toContain("secret-value");
      expect(redacted).not.toContain("replayable");
      expect(redacted).toContain("[redacted SOLANA_RPC_URL]");
      expect(redacted).toContain("[redacted signed transaction]");
    } finally {
      if (previousRpcUrl === undefined) {
        delete process.env.SOLANA_RPC_URL;
      } else {
        process.env.SOLANA_RPC_URL = previousRpcUrl;
      }
    }
  });
});

describe("runtime dependency boundary", () => {
  test("executor imports packages instead of sibling loyal-apps paths", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();

    expect(source).not.toContain("loyal-apps");
    expect(source).not.toContain("LOYAL_APPS_ROOT");
    expect(source).toContain('import("@loyal-labs/smart-account-vaults")');
    expect(source).toContain('import("@loyal-labs/loyal-smart-accounts-core")');
    expect(source).toContain('import("@loyal/actions")');
  });

  test("selects the same-mint Kamino route policy instead of newest active policy", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();

    expect(source).toContain('const SAME_MINT_ROUTE_MODE = "same_mint_kamino"');
    expect(source).toContain("= ANY(rp.route_modes)");
    expect(source).toContain("mv.active_policy_id = rp.id");
    expect(source).toContain("COALESCE(recovery.top_up_policy_account, rp.policy_account)");
    expect(source).toContain('"--route-policy-account"');
    expect(source).toContain('"--emit-prepared-transaction"');
    expect(source).not.toContain("ORDER BY rp.last_seen_slot DESC, rp.id DESC\n      LIMIT 1");
  });

  test("uses same-mint reserve-swap for Kamino top-up instead of Earn deposit prep", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();

    expect(source).toContain("runSameMintReserveTopUp");
    expect(source).toContain('"--deposit-reserve"');
    expect(source).toContain("same-mint:swap");
    expect(source).not.toContain("prepareEarnUsdcDeposit");
  });

  test("marks scheduled slot failures when an active route policy is missing", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();

    expect(source).toContain("MissingActiveEarnRoutePolicyError");
    expect(source).toContain("markScheduledSlotFailed");
    expect(source).toContain("missing_active_earn_route_policy");
    expect(source).toContain("status = 'failed'");
    expect(source).toContain("last_error = ${args.lastError}");
  });

  test("smart-account-vaults package exposes autodeposit pull helper", async () => {
    const { PublicKey } = await import("@solana/web3.js");
    const { createSmartAccountVaultsClient } = await import(
      "@loyal-labs/smart-account-vaults"
    );

    const client = createSmartAccountVaultsClient({
      connection: {} as never,
      programId: PublicKey.default,
    });

    expect(typeof client.prepareEarnUsdcAutodepositPull).toBe("function");
  });

  test("render light worker keeps pinned image executor boundary", async () => {
    const renderYaml = await Bun.file(
      new URL("../render.yaml", import.meta.url)
    ).text();

    expect(renderYaml).toContain("loyal-balance-sweep-autodeposit-trigger");
    expect(renderYaml).toContain("loyal-yield-realtime");
    expect(renderYaml).toContain("runtime: image");
    expect(renderYaml).toContain("/usr/local/bin/loyal-yield-realtime");
    expect(renderYaml).toContain("healthCheckPath: /healthz");
    expect(renderYaml).toContain("REALTIME_AUTH_SECRET");
    expect(renderYaml).toContain(
      "ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-"
    );
    expect(renderYaml).toContain(
      "ghcr.io/loyal-labs/loyal-yield-routing/laserstream-workers:sha-"
    );
    expect(renderYaml).toContain(
      "BALANCE_SWEEP_EXECUTOR_COMMAND"
    );
    expect(renderYaml).toContain(
      "bun scripts/execute-autodeposit-policy.ts --require-lot-claim"
    );
    expect(renderYaml).not.toContain(":latest");
    expect(renderYaml).not.toContain("dockerfilePath: Dockerfile.light-workers");
  });

  test("autodeposit trigger prioritizes newest execute-now slots first", async () => {
    const source = await Bun.file(
      new URL(
        "../crates/balance-sweep-autodeposit-trigger/src/main.rs",
        import.meta.url
      )
    ).text();

    expect(source).toContain(
      "CASE WHEN slot.status = 'requested' THEN 0 ELSE 1 END"
    );
    expect(source).toContain("requested_at DESC NULLS LAST");
    expect(source).toContain("slot.eligible_after ASC");
    expect(source).not.toContain("slot.requested_at ASC NULLS LAST");
  });

  test("sends Solana Week wallet addresses as base58 public keys", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();

    expect(source).not.toContain("encodePublicKeyBase64");
    expect(source).toContain(
      "new args.PublicKeyCtor(\n    args.ownerWalletAddress\n  ).toBase58()"
    );
  });
});

describe("parseKeypairSecret", () => {
  test("accepts hex-encoded Solana keypair bytes", () => {
    const keypair = Keypair.generate();
    const parsed = parseKeypairSecret(hex(keypair.secretKey));

    expect(parsed.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
  });

  test("accepts JSON byte-array Solana keypair bytes", () => {
    const keypair = Keypair.generate();
    const parsed = parseKeypairSecret(JSON.stringify(Array.from(keypair.secretKey)));

    expect(parsed.publicKey.toBase58()).toBe(keypair.publicKey.toBase58());
  });
});
