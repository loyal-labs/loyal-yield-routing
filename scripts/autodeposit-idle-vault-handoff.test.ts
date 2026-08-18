import { describe, expect, test } from "bun:test";

import {
  OncePerKeyAlertLatch,
  historicalIdleVaultRecoveryAction,
  idleVaultRecoveryAlert,
  observeIdleVaultAtOrAfter,
  projectIdleVaultBalance,
} from "./autodeposit-idle-vault-handoff";

describe("autodeposit idle-vault handoff", () => {
  test("waits for an observation at or after the pull slot", async () => {
    const slots = [40n, 41n];
    const observation = await observeIdleVaultAtOrAfter({
      minimumSlot: 41n,
      maxAttempts: 2,
      pollIntervalMs: 0,
      read: async () => ({
        amountRaw: 5n,
        observedSlot: slots.shift() ?? 41n,
      }),
      sleep: async () => undefined,
    });
    expect(observation.observedSlot).toBe(41n);
  });

  test("keeps a newer projection when an older worker races it", () => {
    const newer = {
      amountRaw: 9n,
      mint: "USDC",
      observedSlot: 50n,
      owner: "vault",
      tokenAccount: "ata",
      vaultId: 1n,
    };
    expect(
      projectIdleVaultBalance(newer, {
        ...newer,
        amountRaw: 8n,
        observedSlot: 49n,
      }),
    ).toEqual(newer);
  });

  test("claims a recovery alert only after a terminal condition", () => {
    const latch = new OncePerKeyAlertLatch();
    expect(
      idleVaultRecoveryAlert({
        alertAlreadyClaimed: !latch.claim("vault:USDC"),
        handoffPersistenceFailed: false,
        idleSinceMs: 0,
        nowMs: 1_000,
        recoverySlaMs: 1_000,
      }),
    ).toBe("recovery_sla_exceeded");
    expect(latch.claim("vault:USDC")).toBeFalse();
  });

  test("historical recovery projects only unresolved positive balances", () => {
    expect(
      historicalIdleVaultRecoveryAction({
        alreadyRecovered: false,
        amountRaw: 1n,
        executionId: 1n,
      }),
    ).toBe("project");
    expect(
      historicalIdleVaultRecoveryAction({
        alreadyRecovered: false,
        amountRaw: 0n,
        executionId: 2n,
      }),
    ).toBe("skip_zero");
  });
});
