import type { PublicKey } from "@solana/web3.js";

import type { SmartAccountVaultsClient } from "../client";
import type { VoltrPlan } from "./plans";
import {
  deriveEarnMaxVoltrAuthority,
  EARN_MAX_VOLTR_ACCOUNT_INDEX,
} from "./program";

export type EarnMaxVoltrPrepareErrorCode =
  | "multi_signer_unsupported"
  | "transaction_too_large";

const PREPARE_ERROR_MESSAGES: Record<EarnMaxVoltrPrepareErrorCode, string> = {
  multi_signer_unsupported:
    "Earn MAX needs a single-signer smart account without a time lock.",
  transaction_too_large: "Earn MAX transaction is too large.",
};

export class EarnMaxVoltrPrepareError extends Error {
  readonly code: EarnMaxVoltrPrepareErrorCode;

  constructor(code: EarnMaxVoltrPrepareErrorCode) {
    super(PREPARE_ERROR_MESSAGES[code]);
    this.name = "EarnMaxVoltrPrepareError";
    this.code = code;
  }
}

export type VoltrPlanBuilder = (context: {
  authority: PublicKey;
  owner: PublicKey;
}) => Promise<VoltrPlan>;

export async function prepareEarnMaxVoltr(input: {
  build: VoltrPlanBuilder;
  client: SmartAccountVaultsClient;
  owner: PublicKey;
  programId: PublicKey;
  settingsPda: PublicKey;
}) {
  const { build, client, owner, programId, settingsPda } = input;
  const settings =
    await client.sdk.smartAccounts.queries.fetchSettings(settingsPda);
  if (Number(settings.threshold) > 1 || Number(settings.timeLock) > 0) {
    throw new EarnMaxVoltrPrepareError("multi_signer_unsupported");
  }
  const authority = deriveEarnMaxVoltrAuthority(settingsPda, programId);
  const { outer, vault } = await build({ authority, owner });
  const prepared = await client.prepareCustomInstructionSync({
    accountIndex: EARN_MAX_VOLTR_ACCOUNT_INDEX,
    feePayer: owner,
    instructions: vault,
    settingsPda,
    signer: owner,
  });
  if (!prepared) {
    throw new EarnMaxVoltrPrepareError("transaction_too_large");
  }
  return {
    ...prepared,
    instructions: [...outer, ...prepared.instructions],
  };
}
