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
    });
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
