import { describe, expect, test } from "bun:test";
import { Keypair } from "@solana/web3.js";

import {
  computeSweepAmount,
  parseKeypairSecret,
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
    expect(renderYaml).toContain("runtime: image");
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
