import { PublicKey } from "@solana/web3.js";

import type {
  SmartAccountEarnUsdcAutodepositCloseMetadata,
  SmartAccountEarnUsdcAutodepositSetupMetadata,
  SmartAccountEarnUsdcDepositMetadata,
  SmartAccountEarnUsdcWithdrawMetadata,
  SmartAccountNativeSolRequirement,
  SmartAccountPreparedEarnUsdcAutodepositClose,
  SmartAccountPreparedEarnUsdcAutodepositSetup,
  SmartAccountPreparedEarnUsdcDeposit,
  SmartAccountPreparedEarnUsdcWithdraw,
} from "../types";
import {
  hydratePreparedOperation,
  serializePreparedOperation,
  type WirePreparedLoyalSmartAccountsOperation,
} from "./prepared-operation-wire.shared";

export type WireSmartAccountPreparedEarnUsdcAutodepositSetup = {
  authorityInitializationRequired: boolean;
  nativeSolRequirement: SmartAccountNativeSolRequirement;
  persistence: SmartAccountEarnUsdcAutodepositSetupMetadata;
  policy: {
    account: string | null;
    id: string | null;
    seed: string | null;
  };
  prepared: WirePreparedLoyalSmartAccountsOperation;
  stage: SmartAccountPreparedEarnUsdcAutodepositSetup["stage"];
  subscription: {
    amountPerPeriodRaw: string;
    authority: string;
    expiryTimestamp: string;
    nonce: string;
    periodLengthSeconds: string;
    recurringDelegation: string;
    startTimestamp: string;
  };
  vault: {
    accountIndex: 1;
    pubkey: string;
    usdcAta: string;
  };
};

export type WireSmartAccountPreparedEarnUsdcAutodepositClose = {
  persistence: SmartAccountEarnUsdcAutodepositCloseMetadata;
  policy: {
    account: string;
  };
  prepared: WirePreparedLoyalSmartAccountsOperation;
  subscription: {
    recurringDelegation: string;
  };
  vault: {
    accountIndex: 1;
    pubkey: string;
  };
};

export type WireSmartAccountPreparedEarnUsdcDeposit = {
  kaminoSetupAccountCount: number;
  kaminoSetupRentLamports: string;
  kaminoSetupRequired: boolean;
  nativeSolRequirement: SmartAccountNativeSolRequirement;
  persistence: SmartAccountEarnUsdcDepositMetadata;
  policyFinalizePrepared?: WirePreparedLoyalSmartAccountsOperation | null;
  policy: {
    account: string;
    id: string;
    sameMintInstructionConstraintIndexes: readonly [number, number];
    seed: string;
  };
  setupPolicy?: {
    account: string;
    id: string;
    initObligationInstructionConstraintIndex: 0;
    seed: string;
  };
  policySetupPrepared?: WirePreparedLoyalSmartAccountsOperation | null;
  prepared: WirePreparedLoyalSmartAccountsOperation;
  targetReserve: {
    liquidityMint: string;
    liquidityTokenProgram: string;
    market: string;
    obligation: string;
    reserve: string;
    supplyApyBps: string | null;
  };
  vault: {
    accountIndex: 1;
    collateralAta: string | null;
    pubkey: string;
    usdcAta: string;
  };
};

export type WireSmartAccountPreparedEarnUsdcWithdraw = {
  amountRaw: string;
  autodepositClosePrepared?: WireSmartAccountPreparedEarnUsdcAutodepositClose | null;
  mode: "partial" | "full";
  persistence: SmartAccountEarnUsdcWithdrawMetadata;
  policy: {
    account: string;
    id: string;
    sameMintInstructionConstraintIndexes: readonly [number, number];
    seed: string;
    withdrawInstructionConstraintIndex: 0;
  };
  setupPolicy?: {
    account: string;
    id: string;
    seed: string;
  };
  prepared: WirePreparedLoyalSmartAccountsOperation;
  withdrawSteps?: Array<{
    accountingReserve: {
      liquidityMint: string;
      market: string;
      obligation: string;
      reserve: string;
    } | null;
    amountRaw: string;
    collateralAta: string | null;
    executionReserve: {
      liquidityMint: string;
      market: string;
      reserve: string;
    } | null;
    mode: "partial" | "full";
    persistence: SmartAccountEarnUsdcWithdrawMetadata;
    prepared: WirePreparedLoyalSmartAccountsOperation;
    reserveWithdrawals?: SmartAccountEarnUsdcWithdrawMetadata["reserveWithdrawals"];
    stepCount: number;
    stepIndex: number;
  }>;
  targetReserve: {
    liquidityMint: string;
    liquidityTokenProgram: string;
    market: string;
    obligation: string;
    reserve: string;
  } | null;
  vault: {
    accountIndex: 1;
    collateralAta: string | null;
    pubkey: string;
    usdcAta: string;
  };
};

export function serializePreparedEarnUsdcAutodepositSetup(
  preparedSetup: SmartAccountPreparedEarnUsdcAutodepositSetup
): WireSmartAccountPreparedEarnUsdcAutodepositSetup {
  return {
    authorityInitializationRequired:
      preparedSetup.authorityInitializationRequired,
    nativeSolRequirement: preparedSetup.nativeSolRequirement,
    persistence: preparedSetup.persistence,
    policy: {
      account: preparedSetup.policy.account?.toBase58() ?? null,
      id: preparedSetup.policy.id?.toString() ?? null,
      seed: preparedSetup.policy.seed?.toString() ?? null,
    },
    prepared: serializePreparedOperation(preparedSetup.prepared),
    stage: preparedSetup.stage,
    subscription: {
      amountPerPeriodRaw:
        preparedSetup.subscription.amountPerPeriodRaw.toString(),
      authority: preparedSetup.subscription.authority.toBase58(),
      expiryTimestamp: preparedSetup.subscription.expiryTimestamp.toString(),
      nonce: preparedSetup.subscription.nonce.toString(),
      periodLengthSeconds:
        preparedSetup.subscription.periodLengthSeconds.toString(),
      recurringDelegation:
        preparedSetup.subscription.recurringDelegation.toBase58(),
      startTimestamp: preparedSetup.subscription.startTimestamp.toString(),
    },
    vault: {
      accountIndex: preparedSetup.vault.accountIndex,
      pubkey: preparedSetup.vault.pubkey.toBase58(),
      usdcAta: preparedSetup.vault.usdcAta.toBase58(),
    },
  };
}

export function hydratePreparedEarnUsdcAutodepositSetup(
  wire: WireSmartAccountPreparedEarnUsdcAutodepositSetup
): SmartAccountPreparedEarnUsdcAutodepositSetup {
  return {
    authorityInitializationRequired: wire.authorityInitializationRequired,
    nativeSolRequirement: wire.nativeSolRequirement,
    persistence: wire.persistence,
    policy: {
      account: wire.policy.account ? new PublicKey(wire.policy.account) : null,
      id: wire.policy.id ? BigInt(wire.policy.id) : null,
      seed: wire.policy.seed ? BigInt(wire.policy.seed) : null,
    },
    prepared: hydratePreparedOperation(wire.prepared),
    stage: wire.stage,
    subscription: {
      amountPerPeriodRaw: BigInt(wire.subscription.amountPerPeriodRaw),
      authority: new PublicKey(wire.subscription.authority),
      expiryTimestamp: BigInt(wire.subscription.expiryTimestamp),
      nonce: BigInt(wire.subscription.nonce),
      periodLengthSeconds: BigInt(wire.subscription.periodLengthSeconds),
      recurringDelegation: new PublicKey(wire.subscription.recurringDelegation),
      startTimestamp: BigInt(wire.subscription.startTimestamp),
    },
    vault: {
      accountIndex: wire.vault.accountIndex,
      pubkey: new PublicKey(wire.vault.pubkey),
      usdcAta: new PublicKey(wire.vault.usdcAta),
    },
  };
}

export function serializePreparedEarnUsdcAutodepositClose(
  preparedClose: SmartAccountPreparedEarnUsdcAutodepositClose
): WireSmartAccountPreparedEarnUsdcAutodepositClose {
  return {
    persistence: preparedClose.persistence,
    policy: {
      account: preparedClose.policy.account.toBase58(),
    },
    prepared: serializePreparedOperation(preparedClose.prepared),
    subscription: {
      recurringDelegation:
        preparedClose.subscription.recurringDelegation.toBase58(),
    },
    vault: {
      accountIndex: preparedClose.vault.accountIndex,
      pubkey: preparedClose.vault.pubkey.toBase58(),
    },
  };
}

export function hydratePreparedEarnUsdcAutodepositClose(
  wire: WireSmartAccountPreparedEarnUsdcAutodepositClose
): SmartAccountPreparedEarnUsdcAutodepositClose {
  return {
    persistence: wire.persistence,
    policy: {
      account: new PublicKey(wire.policy.account),
    },
    prepared: hydratePreparedOperation(wire.prepared),
    subscription: {
      recurringDelegation: new PublicKey(wire.subscription.recurringDelegation),
    },
    vault: {
      accountIndex: wire.vault.accountIndex,
      pubkey: new PublicKey(wire.vault.pubkey),
    },
  };
}

export function serializePreparedEarnUsdcDeposit(
  preparedDeposit: SmartAccountPreparedEarnUsdcDeposit
): WireSmartAccountPreparedEarnUsdcDeposit {
  return {
    kaminoSetupAccountCount: preparedDeposit.kaminoSetupAccountCount,
    kaminoSetupRentLamports: preparedDeposit.kaminoSetupRentLamports,
    kaminoSetupRequired: preparedDeposit.kaminoSetupRequired,
    nativeSolRequirement: preparedDeposit.nativeSolRequirement,
    persistence: preparedDeposit.persistence,
    policyFinalizePrepared: preparedDeposit.policyFinalizePrepared
      ? serializePreparedOperation(preparedDeposit.policyFinalizePrepared)
      : null,
    policy: {
      account: preparedDeposit.policy.account.toBase58(),
      id: preparedDeposit.policy.id.toString(),
      sameMintInstructionConstraintIndexes:
        preparedDeposit.policy.sameMintInstructionConstraintIndexes,
      seed: preparedDeposit.policy.seed.toString(),
    },
    ...(preparedDeposit.setupPolicy
      ? {
          setupPolicy: {
            account: preparedDeposit.setupPolicy.account.toBase58(),
            id: preparedDeposit.setupPolicy.id.toString(),
            initObligationInstructionConstraintIndex:
              preparedDeposit.setupPolicy
                .initObligationInstructionConstraintIndex,
            seed: preparedDeposit.setupPolicy.seed.toString(),
          },
        }
      : {}),
    policySetupPrepared: preparedDeposit.policySetupPrepared
      ? serializePreparedOperation(preparedDeposit.policySetupPrepared)
      : null,
    prepared: serializePreparedOperation(preparedDeposit.prepared),
    targetReserve: {
      liquidityMint: preparedDeposit.targetReserve.liquidityMint.toBase58(),
      liquidityTokenProgram:
        preparedDeposit.targetReserve.liquidityTokenProgram.toBase58(),
      market: preparedDeposit.targetReserve.market.toBase58(),
      obligation: preparedDeposit.targetReserve.obligation.toBase58(),
      reserve: preparedDeposit.targetReserve.reserve.toBase58(),
      supplyApyBps:
        preparedDeposit.targetReserve.supplyApyBps?.toString() ?? null,
    },
    vault: {
      accountIndex: preparedDeposit.vault.accountIndex,
      collateralAta: preparedDeposit.vault.collateralAta?.toBase58() ?? null,
      pubkey: preparedDeposit.vault.pubkey.toBase58(),
      usdcAta: preparedDeposit.vault.usdcAta.toBase58(),
    },
  };
}

export function hydratePreparedEarnUsdcDeposit(
  wire: WireSmartAccountPreparedEarnUsdcDeposit
): SmartAccountPreparedEarnUsdcDeposit {
  return {
    kaminoSetupAccountCount: wire.kaminoSetupAccountCount,
    kaminoSetupRentLamports: wire.kaminoSetupRentLamports,
    kaminoSetupRequired: wire.kaminoSetupRequired,
    nativeSolRequirement: wire.nativeSolRequirement,
    persistence: wire.persistence,
    policyFinalizePrepared: wire.policyFinalizePrepared
      ? hydratePreparedOperation(wire.policyFinalizePrepared)
      : null,
    policy: {
      account: new PublicKey(wire.policy.account),
      id: BigInt(wire.policy.id),
      sameMintInstructionConstraintIndexes:
        wire.policy.sameMintInstructionConstraintIndexes,
      seed: BigInt(wire.policy.seed),
    },
    ...(wire.setupPolicy
      ? {
          setupPolicy: {
            account: new PublicKey(wire.setupPolicy.account),
            id: BigInt(wire.setupPolicy.id),
            initObligationInstructionConstraintIndex:
              wire.setupPolicy.initObligationInstructionConstraintIndex,
            seed: BigInt(wire.setupPolicy.seed),
          },
        }
      : {}),
    policySetupPrepared: wire.policySetupPrepared
      ? hydratePreparedOperation(wire.policySetupPrepared)
      : null,
    prepared: hydratePreparedOperation(wire.prepared),
    targetReserve: {
      liquidityMint: new PublicKey(wire.targetReserve.liquidityMint),
      liquidityTokenProgram: new PublicKey(
        wire.targetReserve.liquidityTokenProgram
      ),
      market: new PublicKey(wire.targetReserve.market),
      obligation: new PublicKey(wire.targetReserve.obligation),
      reserve: new PublicKey(wire.targetReserve.reserve),
      supplyApyBps: wire.targetReserve.supplyApyBps
        ? BigInt(wire.targetReserve.supplyApyBps)
        : null,
    },
    vault: {
      accountIndex: wire.vault.accountIndex,
      collateralAta: wire.vault.collateralAta
        ? new PublicKey(wire.vault.collateralAta)
        : null,
      pubkey: new PublicKey(wire.vault.pubkey),
      usdcAta: new PublicKey(wire.vault.usdcAta),
    },
  };
}

export function serializePreparedEarnUsdcWithdraw(
  preparedWithdraw: SmartAccountPreparedEarnUsdcWithdraw
): WireSmartAccountPreparedEarnUsdcWithdraw {
  return {
    amountRaw: preparedWithdraw.amountRaw.toString(),
    autodepositClosePrepared: preparedWithdraw.autodepositClosePrepared
      ? serializePreparedEarnUsdcAutodepositClose(
          preparedWithdraw.autodepositClosePrepared
        )
      : null,
    mode: preparedWithdraw.mode,
    persistence: preparedWithdraw.persistence,
    policy: {
      account: preparedWithdraw.policy.account.toBase58(),
      id: preparedWithdraw.policy.id.toString(),
      sameMintInstructionConstraintIndexes:
        preparedWithdraw.policy.sameMintInstructionConstraintIndexes,
      seed: preparedWithdraw.policy.seed.toString(),
      withdrawInstructionConstraintIndex:
        preparedWithdraw.policy.withdrawInstructionConstraintIndex,
    },
    ...(preparedWithdraw.setupPolicy
      ? {
          setupPolicy: {
            account: preparedWithdraw.setupPolicy.account.toBase58(),
            id: preparedWithdraw.setupPolicy.id.toString(),
            seed: preparedWithdraw.setupPolicy.seed.toString(),
          },
        }
      : {}),
    prepared: serializePreparedOperation(preparedWithdraw.prepared),
    withdrawSteps: preparedWithdraw.withdrawSteps.map((step) => ({
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
    targetReserve: preparedWithdraw.targetReserve
      ? {
          liquidityMint:
            preparedWithdraw.targetReserve.liquidityMint.toBase58(),
          liquidityTokenProgram:
            preparedWithdraw.targetReserve.liquidityTokenProgram.toBase58(),
          market: preparedWithdraw.targetReserve.market.toBase58(),
          obligation: preparedWithdraw.targetReserve.obligation.toBase58(),
          reserve: preparedWithdraw.targetReserve.reserve.toBase58(),
        }
      : null,
    vault: {
      accountIndex: preparedWithdraw.vault.accountIndex,
      collateralAta: preparedWithdraw.vault.collateralAta?.toBase58() ?? null,
      pubkey: preparedWithdraw.vault.pubkey.toBase58(),
      usdcAta: preparedWithdraw.vault.usdcAta.toBase58(),
    },
  };
}

export function hydratePreparedEarnUsdcWithdraw(
  wire: WireSmartAccountPreparedEarnUsdcWithdraw
): SmartAccountPreparedEarnUsdcWithdraw {
  const fallbackStep = {
    accountingReserve: wire.targetReserve
      ? {
          liquidityMint: new PublicKey(wire.targetReserve.liquidityMint),
          market: new PublicKey(wire.targetReserve.market),
          obligation: new PublicKey(wire.targetReserve.obligation),
          reserve: new PublicKey(wire.targetReserve.reserve),
        }
      : null,
    amountRaw: BigInt(wire.amountRaw),
    collateralAta: wire.vault.collateralAta
      ? new PublicKey(wire.vault.collateralAta)
      : null,
    executionReserve: wire.targetReserve
      ? {
          liquidityMint: new PublicKey(wire.targetReserve.liquidityMint),
          market: new PublicKey(wire.targetReserve.market),
          reserve: new PublicKey(wire.targetReserve.reserve),
        }
      : null,
    mode: wire.mode,
    persistence: wire.persistence,
    prepared: hydratePreparedOperation(wire.prepared),
    reserveWithdrawals: wire.persistence.reserveWithdrawals ?? [],
    stepCount: 1,
    stepIndex: 0,
  };

  return {
    amountRaw: BigInt(wire.amountRaw),
    autodepositClosePrepared: wire.autodepositClosePrepared
      ? hydratePreparedEarnUsdcAutodepositClose(wire.autodepositClosePrepared)
      : null,
    mode: wire.mode,
    persistence: wire.persistence,
    policy: {
      account: new PublicKey(wire.policy.account),
      id: BigInt(wire.policy.id),
      sameMintInstructionConstraintIndexes:
        wire.policy.sameMintInstructionConstraintIndexes,
      seed: BigInt(wire.policy.seed),
      withdrawInstructionConstraintIndex:
        wire.policy.withdrawInstructionConstraintIndex,
    },
    ...(wire.setupPolicy
      ? {
          setupPolicy: {
            account: new PublicKey(wire.setupPolicy.account),
            id: BigInt(wire.setupPolicy.id),
            seed: BigInt(wire.setupPolicy.seed),
          },
        }
      : {}),
    prepared: hydratePreparedOperation(wire.prepared),
    withdrawSteps: wire.withdrawSteps?.map((step) => ({
      accountingReserve: step.accountingReserve
        ? {
            liquidityMint: new PublicKey(step.accountingReserve.liquidityMint),
            market: new PublicKey(step.accountingReserve.market),
            obligation: new PublicKey(step.accountingReserve.obligation),
            reserve: new PublicKey(step.accountingReserve.reserve),
          }
        : null,
      amountRaw: BigInt(step.amountRaw),
      collateralAta: step.collateralAta
        ? new PublicKey(step.collateralAta)
        : null,
      executionReserve: step.executionReserve
        ? {
            liquidityMint: new PublicKey(step.executionReserve.liquidityMint),
            market: new PublicKey(step.executionReserve.market),
            reserve: new PublicKey(step.executionReserve.reserve),
          }
        : null,
      mode: step.mode,
      persistence: step.persistence,
      prepared: hydratePreparedOperation(step.prepared),
      reserveWithdrawals:
        step.reserveWithdrawals ?? step.persistence.reserveWithdrawals ?? [],
      stepCount: step.stepCount,
      stepIndex: step.stepIndex,
    })) ?? [fallbackStep],
    targetReserve: wire.targetReserve
      ? {
          liquidityMint: new PublicKey(wire.targetReserve.liquidityMint),
          liquidityTokenProgram: new PublicKey(
            wire.targetReserve.liquidityTokenProgram
          ),
          market: new PublicKey(wire.targetReserve.market),
          obligation: new PublicKey(wire.targetReserve.obligation),
          reserve: new PublicKey(wire.targetReserve.reserve),
        }
      : null,
    vault: {
      accountIndex: wire.vault.accountIndex,
      collateralAta: wire.vault.collateralAta
        ? new PublicKey(wire.vault.collateralAta)
        : null,
      pubkey: new PublicKey(wire.vault.pubkey),
      usdcAta: new PublicKey(wire.vault.usdcAta),
    },
  };
}
