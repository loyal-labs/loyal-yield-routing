import { describe, expect, test } from "bun:test";
import { Keypair, type Connection } from "@solana/web3.js";

import {
  assertSolBalance,
  computeSweepAmount,
  isMissingAutodepositTokenDelegateFailure,
  parseKeypairSecret,
  runAfterFeePayerSolSafety,
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
      "const pullSimulation = await simulatePreparedOperation({"
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

    // Both hand-back paths clamp: the retryable release and the terminal
    // cancel taken when the route policy is gone from the chain. Whitespace is
    // tolerant so the clamp can be written inline or across lines.
    expect(
      executorSource.match(
        /LEAST\(\s*l\.original_amount_raw,\s*l\.remaining_amount_raw \+ i\.amount_raw\s*\)/g
      )
    ).toHaveLength(2);
    expect(
      triggerSource.match(
        /LEAST\(\s*lot\.original_amount_raw,\s*lot\.remaining_amount_raw \+ item\.amount_raw\s*\)/g
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
      /updated_claim AS \([\s\S]*?\n    \)\n    UPDATE loyal_yield\.balance_sweep_scheduled_slots/
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
