import "server-only";

import type { SolanaEnv } from "@loyal-labs/solana-rpc";

import { resolveLoyalWebSolanaEnvFromEnv } from "@/lib/core/config/solana-env-override";
import {
  EarnDepositConfirmError,
  pollRpcRead,
  resolvePolicyCreationSignatureFromChain,
  type EarnDepositConfirmPrincipal,
} from "@/lib/yield-optimization/earn-deposit-confirm.server";
import {
  findActiveYieldRoutePolicyPair,
  type ConfirmedYieldDepositInput,
  type RoutePolicyRecord,
} from "@/lib/yield-optimization/yield-deposit-repository.server";

type PolicyCitation = { signature: string; slot: bigint };

async function citePolicy(args: {
  policyAccount: string;
  recorded: RoutePolicyRecord | null | undefined;
  solanaEnv: SolanaEnv;
}): Promise<PolicyCitation | null> {
  if (args.recorded?.policyAccount === args.policyAccount) {
    return {
      signature: args.recorded.lastSeenSignature,
      slot: args.recorded.lastSeenSlot,
    };
  }
  const creation = await pollRpcRead(() =>
    resolvePolicyCreationSignatureFromChain({
      cluster: args.solanaEnv,
      policyAccount: args.policyAccount,
    })
  );
  return creation
    ? { signature: creation.signature, slot: BigInt(creation.slot) }
    : null;
}

export async function citeEarnDepositPolicies(args: {
  input: ConfirmedYieldDepositInput;
  principal: EarnDepositConfirmPrincipal;
}): Promise<ConfirmedYieldDepositInput> {
  const { input, principal } = args;
  const solanaEnv = resolveLoyalWebSolanaEnvFromEnv(process.env);
  const pair = await findActiveYieldRoutePolicyPair({
    authority: principal.walletAddress,
    cluster: input.cluster,
    settings: principal.settingsPda,
    vaultIndex: input.vaultIndex,
    vaultPubkey: input.vaultPubkey,
  });
  const route = await citePolicy({
    policyAccount: input.policyAccount,
    recorded: pair?.routePolicy,
    solanaEnv,
  });
  if (!route) {
    console.error("[earn-deposit-confirm] policy citation unresolved", {
      depositSignature: input.depositSignature,
      policyAccount: input.policyAccount,
      policyInitialization: input.policyInitialization,
      walletAddress: principal.walletAddress,
    });
    throw new EarnDepositConfirmError({
      code: "policy_signature_unresolved",
      message:
        "We couldn't verify your Earn policy on-chain. Your deposit is safe — please try again in a moment.",
      status: 400,
    });
  }
  const setup = input.setupPolicyAccount
    ? await citePolicy({
        policyAccount: input.setupPolicyAccount,
        recorded: pair?.setupPolicy,
        solanaEnv,
      })
    : null;

  return {
    ...input,
    policyConfirmedSlot: route.slot,
    policySignature: route.signature,
    setupPolicyConfirmedSlot: setup?.slot ?? null,
    setupPolicySignature: setup?.signature ?? null,
  };
}
