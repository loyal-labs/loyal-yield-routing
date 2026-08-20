import { describe, expect, test } from "bun:test";
import { Keypair, type Connection } from "@solana/web3.js";

import {
  AutodepositEffectAmbiguousError,
  assertEmptyVaultBeforeDirectAutodeposit,
  assertSolBalance,
  autodepositExecutorFailureExitCode,
  buildDirectDepositPositionReconciliationCommand,
  computeSweepAmount,
  isMissingAutodepositTokenDelegateFailure,
  parseKeypairSecret,
  quarantineMissingAutodepositDelegate,
  observeDurableAutodepositAttempt,
  releaseAutodepositLotClaim,
  runAfterFeePayerSolSafety,
  runTopUpWithLookupTableRetry,
  shouldNotifyFailedSweep,
  throwIfAutodepositAttemptRequiresOperator,
} from "./execute-autodeposit-policy";
import type { DurableAutodepositAttempt } from "./durable-autodeposit-confirmation";

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

describe("direct autodeposit vault ownership", () => {
  test("defers a pull until pre-existing idle funds drain", () => {
    expect(() => assertEmptyVaultBeforeDirectAutodeposit(BigInt(1))).toThrow(
      "existing idle vault balance must drain before direct autodeposit"
    );
    expect(() =>
      assertEmptyVaultBeforeDirectAutodeposit(BigInt(0))
    ).not.toThrow();
  });
});

describe("direct autodeposit position reconciliation", () => {
  test("scopes the chain reconciliation to the deposited reserve", () => {
    const command = buildDirectDepositPositionReconciliationCommand({
      reserve: "kamino-reserve",
      rpcUrl: "https://rpc.invalid",
      target: {
        settings: "smart-account-settings",
        vaultIndex: 1,
      },
    });
    const settingsArgument = command.indexOf("--settings");

    expect(command.slice(settingsArgument)).toEqual([
      "--settings",
      "smart-account-settings",
      "--vault-index",
      "1",
      "--reconcile-from-chain",
      "--reconcile-current-positions",
      "--reconcile-reserve",
      "kamino-reserve",
      "--rpc-url",
      "https://rpc.invalid",
    ]);
  });
});

describe("autodeposit top-up alert boundary", () => {
  test("raises a typed operator error only for an ambiguous chain effect", () => {
    expect(() =>
      throwIfAutodepositAttemptRequiresOperator({
        signature: "ambiguous-top-up",
        state: "ambiguous",
      })
    ).toThrow(AutodepositEffectAmbiguousError);
    expect(() =>
      throwIfAutodepositAttemptRequiresOperator({
        signature: "pending-top-up",
        state: "unknown",
      })
    ).not.toThrow();
  });
});

describe("pull fee-payer SOL safety", () => {
  test("rejects a pull payer below 50,000,000 lamports with its role", async () => {
    const feePayer = Keypair.generate().publicKey;
    const connection = {
      getBalance: async (address: typeof feePayer, commitment: string) => {
        expect(address.toBase58()).toBe(feePayer.toBase58());
        expect(commitment).toBe("confirmed");
        return 49_999_999;
      },
    } as unknown as Pick<Connection, "getBalance">;

    await expect(
      assertSolBalance({
        connection,
        feePayer,
        minimumLamports: 50_000_000,
        role: "Autodeposit pull fee payer",
      })
    ).rejects.toThrow(
      `Autodeposit pull fee payer ${feePayer.toBase58()} has 49999999 lamports; 50000000 required.`
    );
  });

  test("checks the pull payer before preparing or simulating the pull", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();
    const balanceCheck = source.indexOf("await assertSolBalance({");
    const preparePull = source.indexOf(
      "await client.prepareEarnUsdcAutodepositPull({"
    );
    const simulatePull = source.indexOf(
      "? await simulatePreparedOperation({"
    );

    expect(balanceCheck).toBeGreaterThan(-1);
    expect(balanceCheck).toBeLessThan(preparePull);
    expect(preparePull).toBeLessThan(simulatePull);
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

describe("durable pull signature observations", () => {
  const durableAttempt: DurableAutodepositAttempt = {
    id: "1",
    claimToken: "claim-1",
    operationKind: "pull",
    executionId: null,
    amountRaw: BigInt(10),
    sourcePreBalanceRaw: BigInt(110),
    destinationPreBalanceRaw: BigInt(20),
    signature: "signature-1",
    signedTransactionBase64: "d2lyZQ==",
    signedTransactionSha256: "a".repeat(64),
    blockhash: "blockhash-1",
    lastValidBlockHeight: BigInt(100),
    state: "submitted",
    broadcastCount: 1,
    confirmedSlot: null,
  };

  test("classifies confirmed history as landed", async () => {
    const connection = {
      getSignatureStatuses: async () => ({
        context: { apiVersion: "test", slot: 43 },
        value: [
          {
            confirmationStatus: "finalized",
            confirmations: null,
            err: null,
            slot: 42,
            status: { Ok: null },
          },
        ],
      }),
      getBlockHeight: async () => 101,
    } as unknown as Connection;

    await expect(
      observeDurableAutodepositAttempt({
        connection,
        attempt: durableAttempt,
      })
    ).resolves.toEqual({
      state: "confirmed",
      confirmedSlot: BigInt(42),
      error: null,
    });
  });

  test("classifies a missing expired signature as safe to requeue", async () => {
    const connection = {
      getSignatureStatuses: async () => ({ value: [null] }),
      getBlockHeight: async () => 101,
    } as unknown as Connection;

    await expect(
      observeDurableAutodepositAttempt({
        connection,
        attempt: durableAttempt,
      })
    ).resolves.toEqual({
      state: "expired",
      confirmedSlot: null,
      error: null,
    });
  });

  test("holds a processed fork after expiry as ambiguous", async () => {
    const connection = {
      getSignatureStatuses: async () => ({
        value: [
          {
            confirmationStatus: "processed",
            confirmations: 0,
            err: null,
            slot: 42,
            status: { Ok: null },
          },
        ],
      }),
      getBlockHeight: async () => 101,
    } as unknown as Connection;

    const observation = await observeDurableAutodepositAttempt({
      connection,
      attempt: durableAttempt,
    });
    expect(observation.state).toBe("ambiguous");
  });

  test("keeps a missing unexpired signature pending", async () => {
    const connection = {
      getSignatureStatuses: async () => ({ value: [null] }),
      getBlockHeight: async () => 100,
    } as unknown as Connection;

    const observation = await observeDurableAutodepositAttempt({
      connection,
      attempt: durableAttempt,
    });
    expect(observation.state).toBe("unknown");
  });
});

describe("top-up retry identity", () => {
  test("retries only the top-up tied to the recorded pull execution and amount", async () => {
    const contexts: Array<{
      attempt: number;
      executionId: string;
      amountRaw: bigint;
    }> = [];
    const result = await runTopUpWithLookupTableRetry({
      attempts: 2,
      executionId: "execution-9",
      amountRaw: BigInt(10),
      delayMs: 0,
      sleep: async () => {},
      attempt: async (context) => {
        contexts.push(context);
        if (context.attempt === 1) {
          throw new Error(
            "reusable lookup-table coverage is incomplete or the exact simulation failure"
          );
        }
        return {
          command: ["same-mint-reserve-swap"],
          exitCode: 0,
          stdout: "{}",
          stderr: "",
          json: {},
        };
      },
    });

    expect(result.exitCode).toBe(0);
    expect(contexts).toEqual([
      { attempt: 1, executionId: "execution-9", amountRaw: BigInt(10) },
      { attempt: 2, executionId: "execution-9", amountRaw: BigInt(10) },
    ]);
  });
});

describe("autodeposit token delegate failures", () => {
  test("recognizes the exact pull simulation owner mismatch", () => {
    expect(
      isMissingAutodepositTokenDelegateFailure(
        new Error(
          "Autodeposit pull simulation failed; refusing to execute. Program log: Error: owner does not match"
        )
      )
    ).toBe(true);
    expect(
      isMissingAutodepositTokenDelegateFailure(
        new Error("Kamino top-up failed: owner does not match")
      )
    ).toBe(false);
  });

  test("reports every transition made by the atomic claim release", async () => {
    const neon = (() => {
      return async () => [
        {
          claim_released: true,
          slot_released: true,
          target_paused: true,
        },
      ];
    }) as Parameters<typeof releaseAutodepositLotClaim>[0]["neon"];

    await expect(
      releaseAutodepositLotClaim({
        neon,
        databaseUrl: "test-database",
        claimToken: "test-claim",
        lastError: "owner does not match",
        pauseTargetForMissingDelegate: true,
      })
    ).resolves.toEqual({
      claimReleased: true,
      slotReleased: true,
      targetPaused: true,
    });
  });

  test("suppresses the alert only after a fully proven quarantine", async () => {
    let exitCode = 1;
    const events: unknown[] = [];
    let releaseReturned = false;

    const result = await quarantineMissingAutodepositDelegate({
      releaseClaim: async () => {
        releaseReturned = true;
        return {
          claimReleased: true,
          slotReleased: true,
          targetPaused: true,
        };
      },
      targetId: BigInt(41),
      scheduledSlotId: BigInt(73),
      onQuarantined: (event) => {
        expect(releaseReturned).toBe(true);
        exitCode = autodepositExecutorFailureExitCode("not_actionable", {
          AUTODEPOSIT_NOT_ACTIONABLE_EXIT_CODE: "23",
        });
        events.push(event);
      },
    });

    expect(exitCode).toBe(23);
    expect(result.status).toBe("quarantined");
    expect(events).toEqual([
      {
        status: "autodeposit_target_paused_missing_delegate",
        targetId: "41",
        scheduledSlotId: "73",
        recoveryOwner: "user",
        recoveryAction: "repair_autodeposit_token_delegate",
        retryable: false,
      },
    ]);
    const serializedEvent = JSON.stringify(events[0]);
    for (const forbiddenField of [
      "wallet",
      "signer",
      "claimToken",
      "secret",
      "rpc",
      "database",
    ]) {
      expect(serializedEvent).not.toContain(forbiddenField);
    }
  });

  test("keeps incomplete quarantine results on the generic failure exit", async () => {
    const incompleteResults = [
      { claimReleased: false, slotReleased: true, targetPaused: true },
      { claimReleased: true, slotReleased: false, targetPaused: true },
      { claimReleased: true, slotReleased: true, targetPaused: false },
    ];

    for (const release of incompleteResults) {
      let exitCode = 1;
      let lifecycleEvents = 0;
      const result = await quarantineMissingAutodepositDelegate({
        releaseClaim: async () => release,
        targetId: BigInt(41),
        scheduledSlotId: BigInt(73),
        onQuarantined: () => {
          exitCode = 23;
          lifecycleEvents += 1;
        },
      });

      expect(result).toEqual({ status: "unproven", release });
      expect(exitCode).toBe(1);
      expect(lifecycleEvents).toBe(0);
    }
  });

  test("keeps a thrown claim release on the generic failure exit", async () => {
    let exitCode = 1;
    let lifecycleEvents = 0;

    await expect(
      quarantineMissingAutodepositDelegate({
        releaseClaim: async () => {
          throw new Error("claim release failed");
        },
        targetId: BigInt(41),
        scheduledSlotId: BigInt(73),
        onQuarantined: () => {
          exitCode = 23;
          lifecycleEvents += 1;
        },
      })
    ).rejects.toThrow("claim release failed");
    expect(exitCode).toBe(1);
    expect(lifecycleEvents).toBe(0);
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
    expect(source).not.toContain("ORDER BY rp.last_seen_slot DESC, rp.id DESC\n      LIMIT 1");
  });

  test("uses same-mint reserve-swap for Kamino top-up instead of Earn deposit prep", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();

    expect(source).toContain("prepareSameMintReserveTopUp");
    expect(source).not.toContain("runSameMintReserveTopUp");
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

  test("caps every lot release at its original amount", async () => {
    const executorSource = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();
    const triggerSource = await Bun.file(
      new URL(
        "../crates/balance-sweep-autodeposit-trigger/src/main.rs",
        import.meta.url
      )
    ).text();

    expect(
      executorSource.match(
        /LEAST\(\s+l\.original_amount_raw,\s+l\.remaining_amount_raw \+ i\.amount_raw\s+\)/g
      )
    ).toHaveLength(1);
    expect(
      triggerSource.match(
        /LEAST\(\s+lot\.original_amount_raw,\s+lot\.remaining_amount_raw \+ item\.amount_raw\s+\)/g
      )
    ).toHaveLength(2);
  });

  test("compare-and-sets missing delegates without gating claim release", async () => {
    const source = await Bun.file(
      new URL("./execute-autodeposit-policy.ts", import.meta.url)
    ).text();
    const releaseSql = source.slice(
      source.indexOf("async function releaseAutodepositLotClaim"),
      source.indexOf("async function markScheduledSlotFailed")
    );
    const pausedTargetSql = releaseSql.match(
      /paused_target AS \([\s\S]*?\n    \),\n    updated_claim AS \(/
    )?.[0];
    const updatedClaimSql = releaseSql.match(
      /updated_claim AS \([\s\S]*?\n    \),\n    -- The replacement slot/
    )?.[0];

    expect(pausedTargetSql).toContain("AND t.active");
    expect(pausedTargetSql).toContain("AND t.lifecycle_status = 'active'");
    expect(pausedTargetSql).toContain("pauseTargetForMissingDelegate");
    expect(updatedClaimSql).toContain("EXISTS (SELECT 1 FROM restored)");
    expect(updatedClaimSql).not.toContain("paused_target");
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
    expect(source).toContain("slot.requested_at DESC NULLS LAST");
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

describe("shouldNotifyFailedSweep", () => {
  test("wakes the user only for a fleet-wide fee payer outage", () => {
    expect(shouldNotifyFailedSweep("preflight_blocked")).toBe(false);
    expect(shouldNotifyFailedSweep("fee_payer_exhausted")).toBe(true);
    // Nothing to sweep, deposit already landed, or a transient error the next
    // cycle clears — pushing for these is noise.
    expect(shouldNotifyFailedSweep("not_actionable")).toBe(false);
    expect(shouldNotifyFailedSweep("yield_persistence_failed")).toBe(false);
    expect(shouldNotifyFailedSweep(null)).toBe(false);
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
