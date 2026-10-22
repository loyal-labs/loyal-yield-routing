import type { SmartAccountPreparedEarnUsdcDeposit } from "@loyal-labs/smart-account-vaults";

export type EarnDepositReviewStage = "deposit" | "policy" | "policy-finalize";

export function getEarnDepositReviewStages(args: {
  preparedDeposit?: SmartAccountPreparedEarnUsdcDeposit | null;
  requiresPolicySetup?: boolean;
}): EarnDepositReviewStage[] {
  const preparedDeposit = args.preparedDeposit ?? null;
  if (!preparedDeposit) {
    return args.requiresPolicySetup ? ["policy", "deposit"] : ["deposit"];
  }

  return [
    ...(preparedDeposit.policySetupPrepared ? (["policy"] as const) : []),
    ...(preparedDeposit.policyFinalizePrepared
      ? (["policy-finalize"] as const)
      : []),
    "deposit",
  ];
}

export function getFirstEarnDepositReviewStage(args: {
  preparedDeposit?: SmartAccountPreparedEarnUsdcDeposit | null;
  requiresPolicySetup?: boolean;
}): EarnDepositReviewStage {
  return getEarnDepositReviewStages(args)[0] ?? "deposit";
}

export function getNextEarnDepositReviewStage(args: {
  currentStage: EarnDepositReviewStage;
  preparedDeposit?: SmartAccountPreparedEarnUsdcDeposit | null;
  requiresPolicySetup?: boolean;
}): EarnDepositReviewStage | null {
  const stages = getEarnDepositReviewStages(args);
  const index = stages.indexOf(args.currentStage);
  if (index < 0) {
    return stages[0] ?? null;
  }

  return stages[index + 1] ?? null;
}

export function getEarnDepositReviewStagePosition(args: {
  stage: EarnDepositReviewStage;
  preparedDeposit?: SmartAccountPreparedEarnUsdcDeposit | null;
  requiresPolicySetup?: boolean;
}): { index: number; total: number } {
  const stages = getEarnDepositReviewStages(args);
  const index = stages.indexOf(args.stage);

  return {
    index: index >= 0 ? index + 1 : 1,
    total: stages.length || 1,
  };
}
