import {
  serializePreparedOperation,
  type SmartAccountPreparedEarnUsdcAutodepositClose,
  type SmartAccountPreparedEarnUsdcAutodepositSetup,
  type SmartAccountPreparedEarnUsdcDeposit,
  type SmartAccountPreparedEarnUsdcWithdraw,
} from "@loyal-labs/smart-account-vaults";
import type {
  AddressLookupTableAccount,
  PublicKey,
  TransactionInstruction,
} from "@solana/web3.js";

export {
  hydratePreparedOperation,
  serializePreparedOperation,
  type WirePreparedLoyalSmartAccountsOperation as WirePreparedOperation,
} from "@loyal-labs/smart-account-vaults";

export type HydratedPreparedOperation = {
  instructions: readonly TransactionInstruction[];
  lookupTableAccounts: readonly AddressLookupTableAccount[];
  payer: PublicKey;
};

// Wire form of a device-prepared autodeposit setup stage, matching the backend
// `serializePreparedEarnUsdcAutodepositSetup` (`earn-autodeposit-prepare-
// contracts.shared.ts`) — `setup/confirm` hydrates this exact shape.
export function serializePreparedEarnAutodepositSetup(
  setup: SmartAccountPreparedEarnUsdcAutodepositSetup,
) {
  return {
    authorityInitializationRequired: setup.authorityInitializationRequired,
    nativeSolRequirement: setup.nativeSolRequirement,
    persistence: setup.persistence,
    policy: {
      account: setup.policy.account?.toBase58() ?? null,
      id: setup.policy.id?.toString() ?? null,
      seed: setup.policy.seed?.toString() ?? null,
    },
    prepared: serializePreparedOperation(setup.prepared),
    stage: setup.stage,
    subscription: {
      amountPerPeriodRaw: setup.subscription.amountPerPeriodRaw.toString(),
      authority: setup.subscription.authority.toBase58(),
      expiryTimestamp: setup.subscription.expiryTimestamp.toString(),
      nonce: setup.subscription.nonce.toString(),
      periodLengthSeconds: setup.subscription.periodLengthSeconds.toString(),
      recurringDelegation: setup.subscription.recurringDelegation.toBase58(),
      startTimestamp: setup.subscription.startTimestamp.toString(),
    },
    vault: {
      accountIndex: setup.vault.accountIndex,
      pubkey: setup.vault.pubkey.toBase58(),
      usdcAta: setup.vault.usdcAta.toBase58(),
    },
  };
}

// Wire form of a device-prepared Earn deposit, matching the backend
// `serializePreparedEarnUsdcDeposit` (`earn-deposit-prepare-contracts.shared.ts`)
// — `deposit/confirm` hydrates this exact shape.
export function serializePreparedEarnUsdcDeposit(
  deposit: SmartAccountPreparedEarnUsdcDeposit,
) {
  return {
    kaminoSetupAccountCount: deposit.kaminoSetupAccountCount,
    kaminoSetupRentLamports: deposit.kaminoSetupRentLamports,
    kaminoSetupRequired: deposit.kaminoSetupRequired,
    nativeSolRequirement: deposit.nativeSolRequirement,
    persistence: deposit.persistence,
    policyFinalizePrepared: deposit.policyFinalizePrepared
      ? serializePreparedOperation(deposit.policyFinalizePrepared)
      : null,
    policy: {
      account: deposit.policy.account.toBase58(),
      id: deposit.policy.id.toString(),
      sameMintInstructionConstraintIndexes:
        deposit.policy.sameMintInstructionConstraintIndexes,
      seed: deposit.policy.seed.toString(),
    },
    ...(deposit.setupPolicy
      ? {
          setupPolicy: {
            account: deposit.setupPolicy.account.toBase58(),
            id: deposit.setupPolicy.id.toString(),
            initObligationInstructionConstraintIndex:
              deposit.setupPolicy.initObligationInstructionConstraintIndex,
            seed: deposit.setupPolicy.seed.toString(),
          },
        }
      : {}),
    policySetupPrepared: deposit.policySetupPrepared
      ? serializePreparedOperation(deposit.policySetupPrepared)
      : null,
    prepared: serializePreparedOperation(deposit.prepared),
    targetReserve: {
      liquidityMint: deposit.targetReserve.liquidityMint.toBase58(),
      liquidityTokenProgram:
        deposit.targetReserve.liquidityTokenProgram.toBase58(),
      market: deposit.targetReserve.market.toBase58(),
      obligation: deposit.targetReserve.obligation.toBase58(),
      reserve: deposit.targetReserve.reserve.toBase58(),
      supplyApyBps: deposit.targetReserve.supplyApyBps?.toString() ?? null,
    },
    vault: {
      accountIndex: deposit.vault.accountIndex,
      collateralAta: deposit.vault.collateralAta?.toBase58() ?? null,
      pubkey: deposit.vault.pubkey.toBase58(),
      usdcAta: deposit.vault.usdcAta.toBase58(),
    },
  };
}

// Wire form of a device-prepared autodeposit close, matching the backend
// `serializePreparedEarnUsdcAutodepositClose` — `close/confirm` hydrates it.
export function serializePreparedEarnAutodepositClose(
  close: SmartAccountPreparedEarnUsdcAutodepositClose,
) {
  return {
    persistence: close.persistence,
    policy: {
      account: close.policy.account.toBase58(),
    },
    prepared: serializePreparedOperation(close.prepared),
    subscription: {
      recurringDelegation: close.subscription.recurringDelegation.toBase58(),
    },
    vault: {
      accountIndex: close.vault.accountIndex,
      pubkey: close.vault.pubkey.toBase58(),
    },
  };
}

// Wire form of a device-prepared Earn withdrawal, matching the backend
// `serializePreparedEarnUsdcWithdraw` (`earn-withdraw-prepare-contracts.shared.ts`)
// — `withdraw/confirm` hydrates this exact shape.
export function serializePreparedEarnUsdcWithdraw(
  withdraw: SmartAccountPreparedEarnUsdcWithdraw,
) {
  return {
    amountRaw: withdraw.amountRaw.toString(),
    autodepositClosePrepared: withdraw.autodepositClosePrepared
      ? serializePreparedEarnAutodepositClose(withdraw.autodepositClosePrepared)
      : null,
    mode: withdraw.mode,
    persistence: withdraw.persistence,
    policy: {
      account: withdraw.policy.account.toBase58(),
      id: withdraw.policy.id.toString(),
      sameMintInstructionConstraintIndexes:
        withdraw.policy.sameMintInstructionConstraintIndexes,
      seed: withdraw.policy.seed.toString(),
      withdrawInstructionConstraintIndex:
        withdraw.policy.withdrawInstructionConstraintIndex,
    },
    ...(withdraw.setupPolicy
      ? {
          setupPolicy: {
            account: withdraw.setupPolicy.account.toBase58(),
            id: withdraw.setupPolicy.id.toString(),
            seed: withdraw.setupPolicy.seed.toString(),
          },
        }
      : {}),
    prepared: serializePreparedOperation(withdraw.prepared),
    withdrawSteps: withdraw.withdrawSteps.map((step) => ({
      // Reserve/collateral step fields are null for idle-only withdrawals
      // (vault stablecoin balance with no Kamino position).
      accountingReserve: step.accountingReserve
        ? {
            liquidityMint: step.accountingReserve.liquidityMint.toBase58(),
            market: step.accountingReserve.market.toBase58(),
            obligation: step.accountingReserve.obligation.toBase58(),
            reserve: step.accountingReserve.reserve.toBase58(),
          }
        : null,
      amountRaw: step.amountRaw.toString(),
      collateralAta: step.collateralAta?.toBase58() ?? null,
      executionReserve: step.executionReserve
        ? {
            liquidityMint: step.executionReserve.liquidityMint.toBase58(),
            market: step.executionReserve.market.toBase58(),
            reserve: step.executionReserve.reserve.toBase58(),
          }
        : null,
      mode: step.mode,
      persistence: step.persistence,
      prepared: serializePreparedOperation(step.prepared),
      reserveWithdrawals: step.reserveWithdrawals,
      stepCount: step.stepCount,
      stepIndex: step.stepIndex,
    })),
    targetReserve: withdraw.targetReserve
      ? {
          liquidityMint: withdraw.targetReserve.liquidityMint.toBase58(),
          liquidityTokenProgram:
            withdraw.targetReserve.liquidityTokenProgram.toBase58(),
          market: withdraw.targetReserve.market.toBase58(),
          obligation: withdraw.targetReserve.obligation.toBase58(),
          reserve: withdraw.targetReserve.reserve.toBase58(),
        }
      : null,
    vault: {
      accountIndex: withdraw.vault.accountIndex,
      collateralAta: withdraw.vault.collateralAta?.toBase58() ?? null,
      pubkey: withdraw.vault.pubkey.toBase58(),
      usdcAta: withdraw.vault.usdcAta.toBase58(),
    },
  };
}
