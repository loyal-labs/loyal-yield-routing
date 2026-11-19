import { createHash } from "node:crypto";
import { expect, test } from "bun:test";
import {
  calculateLpForDepositAmount,
  DEPOSIT_STRATEGY_DISCRIMINATOR,
  DEPOSIT_VAULT_DISCRIMINATOR,
  HARVEST_FEE_DISCRIMINATOR,
  VOLTR_VAULT_PROGRAM_ADDRESS as voltr,
  getDepositStrategyEventEncoder,
  getDepositVaultEventEncoder,
  getHarvestFeeEventEncoder,
} from "@voltr/vault-sdk";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { feeReceipt, type Receipt } from "./earn-max-fee-receipt.js";

const vault = RWA_MULTIPLY_ROUTE.vault.address;
const caller = RWA_MULTIPLY_ROUTE.squads.program;
const sig = "fixture";
function log(name: string, bytes: ArrayLike<number>) {
  const discriminator = createHash("sha256")
    .update("event:" + name)
    .digest()
    .subarray(0, 8);
  return "Program data: " + Buffer.concat([discriminator, Buffer.from(bytes)]).toString("base64");
}

function receipt(data: Uint8Array, event: string): Receipt {
  return {
    slot: 10,
    transaction: {
      signatures: [sig],
      message: {
        accountKeys: [caller, voltr, vault],
        instructions: [{ programIdIndex: 0, accounts: [1, 2], data: "" }],
      },
    },
    meta: {
      err: null,
      innerInstructions: [
        {
          index: 0,
          instructions: [{ programIdIndex: 1, accounts: [2], data: bs58.encode(data), stackHeight: 2 }],
        },
      ],
      logMessages: [
        `Program ${caller} invoke [1]`,
        `Program ${voltr} invoke [2]`,
        event,
        `Program ${voltr} success`,
        `Program ${caller} success`,
      ],
    },
  };
}

function nav(delta: bigint) {
  return receipt(
    DEPOSIT_STRATEGY_DISCRIMINATOR,
    log(
      "DepositStrategyEvent",
      getDepositStrategyEventEncoder().encode({
        manager: vault,
        vault,
        strategy: vault,
        strategyInitReceipt: vault,
        adaptorProgram: caller,
        vaultAssetMint: vault,
        vaultAmountAssetDeposited: 0n,
        vaultAssetTotalValueBefore: 10_000n,
        vaultAssetTotalValueAfter: 10_200n,
        vaultLpSupplyInclFeesBefore: 10_000n,
        vaultLpSupplyInclFeesAfter: 10_000n + delta,
        vaultHighestAssetPerLpDecimalBitsBefore: 1n << 48n,
        vaultHighestAssetPerLpDecimalBitsAfter: (10_200n << 48n) / (10_000n + delta),
        vaultAssetIdleAtaAmountBefore: 0n,
        vaultAssetIdleAtaAmountAfter: 0n,
        strategyPositionValueBefore: 10_000n,
        strategyPositionValueAfter: 10_200n,
        depositedTs: 1n,
      }),
    ),
  );
}

test("positive NAV dilution is an assessment observation, separate from harvesting admin shares", () => {
  const assessed = feeReceipt(nav(29n), sig, ["other", sig]);
  expect(assessed.transactionIndex).toBe(1);
  expect(assessed.events[0]).toMatchObject({
    outer: 0,
    inner: 0,
    effectiveSupplyDelta: 29n,
    strategyCapitalRaw: 0n,
  });
  const harvested = receipt(
    HARVEST_FEE_DISCRIMINATOR,
    log(
      "HarvestFeeEvent",
      getHarvestFeeEventEncoder().encode({
        vault,
        admin: vault,
        manager: vault,
        protocol: caller,
        harvester: vault,
        amountLpAdminFees: 100n,
        amountLpManagerFees: 0n,
        amountLpProtocolFees: 0n,
      }),
    ),
  );
  expect(feeReceipt(harvested, sig, [sig]).events[0]).toMatchObject({ harvestedAdminLp: 100n });
});

test("the observed 40 bps issuance reduces LP minted to the depositor while leaving accumulated fees unchanged", () => {
  const openingSupply = 202_442_907n;
  const accumulated = 73_763n;
  const deadWeight = 1_000n;
  const value = 75_820_513_625n;
  const deposit = 3_534_210_000n;
  const quote = {
    vaultTotalValue: value,
    vaultAccumulatedLpAdminFees: accumulated,
    vaultAccumulatedLpManagerFees: 0n,
    vaultAccumulatedLpProtocolFees: 0n,
    vaultDeadWeight: deadWeight,
    vaultManagementFeeBps: 0,
    vaultLastManagementFeeUpdateTs: 1n,
    lpSupply: openingSupply - accumulated - deadWeight,
    assetAmount: deposit,
    assetDecimals: 6,
    lpDecimals: 6,
    currentTimeSec: 1n,
  };
  expect(calculateLpForDepositAmount({ ...quote, vaultIssuanceFeeBps: 40 })).toBe(9_396_942n);
  expect(calculateLpForDepositAmount({ ...quote, vaultIssuanceFeeBps: 0 })).toBe(9_436_440n);
  const tx = receipt(
    DEPOSIT_VAULT_DISCRIMINATOR,
    log(
      "DepositVaultEvent",
      getDepositVaultEventEncoder().encode({
        user: vault,
        userAmountAssetDeposited: deposit,
        userAmountLpMinted: 9_396_942n,
        vault,
        vaultAssetMint: vault,
        vaultAssetTotalValueBefore: value,
        vaultAssetTotalValueAfter: value + deposit,
        vaultLpSupplyInclFeesBefore: openingSupply,
        vaultLpSupplyInclFeesAfter: 211_839_849n,
        vaultLpTotalAccumulatedFeesBefore: accumulated,
        vaultLpTotalAccumulatedFeesAfter: accumulated,
        vaultLpDeadWeightBefore: deadWeight,
        vaultLpDeadWeightAfter: deadWeight,
        vaultHighestAssetPerLpDecimalBitsBefore: 105_644_333_190_869_013n,
        vaultHighestAssetPerLpDecimalBitsAfter: 105_663_989_144_383_133n,
        depositedTs: 1n,
      }),
    ),
  );
  expect(feeReceipt(tx, sig, [sig]).events[0]).toMatchObject({
    accumulatedFeesBefore: accumulated,
    accumulatedFeesAfter: accumulated,
    mintedUserLp: 211_839_849n - openingSupply,
  });
});

test("drops another program's matching event and refuses incomplete or mismatched receipts", () => {
  const tx = nav(0n);
  const logs = tx.meta!.logMessages!;
  const trueEvent = logs[2]!;
  logs.splice(1, 0, trueEvent);
  expect(feeReceipt(tx, sig, [sig]).events).toHaveLength(1);
  expect(() => feeReceipt(tx, sig, [])).toThrow();
  tx.meta!.innerInstructions![0]!.instructions[0]!.data = bs58.encode(DEPOSIT_VAULT_DISCRIMINATOR);
  expect(() => feeReceipt(tx, sig, [sig])).toThrow();
  tx.meta!.innerInstructions![0]!.instructions[0]!.data = bs58.encode(DEPOSIT_STRATEGY_DISCRIMINATOR);
  logs.pop();
  expect(() => feeReceipt(tx, sig, [sig])).toThrow();
});
