import {
  createSmartAccountVaultsClient,
  prepareEarnMaxVoltr,
  voltrClaimPlan,
  voltrDepositPlan,
  voltrRequestWithdrawalPlan,
  type VoltrPlanBuilder,
} from "@loyal-labs/smart-account-vaults";
import { PublicKey } from "@solana/web3.js";

import { env } from "@/config/env";
import { getConnection } from "@/lib/solana/rpc/connection";
import type { Signer } from "@/lib/wallet/signer";

import type * as NextEarnMax from "../../../../../web/src/features/earn-max/types";
import { fetchEarnState } from "./earn-api";
import { earnHeaders, earnWalletRead, ensureEarnSession } from "./earn-session";
import {
  signAndSendPreparedOperation,
  type SentTransaction,
} from "./send-prepared";

export type EarnMaxSummary = NextEarnMax.EarnMaxSummary;
export type EarnMaxActivityResponse = NextEarnMax.EarnMaxActivityResponse;
export type EarnMaxSummaryResponse = Omit<
  NextEarnMax.EarnMaxSummaryResponse,
  "config"
> & {
  config: NextEarnMax.EarnMaxSummaryResponse["config"] & {
    settingsPda?: string;
    walletAddress?: string;
  };
};
export type EarnMaxInviteResponse = { redeemed: boolean };

export const EARN_MAX_CONTRACT = "earn-max-v4";

const EARN_MAX_PATH = "/api/smart-accounts/earn-max";

export type EarnMaxErrorCode =
  | "contract_mismatch"
  | "invite_required"
  | "unauthenticated"
  | "request_failed";

export class EarnMaxError extends Error {
  readonly code: EarnMaxErrorCode;
  readonly status: number | null;

  constructor(code: EarnMaxErrorCode, message: string, status: number | null) {
    super(message);
    this.name = "EarnMaxError";
    this.code = code;
    this.status = status;
  }
}

async function errorCodeOf(res: Response): Promise<string | undefined> {
  const payload = (await res.json().catch(() => null)) as {
    error?: { code?: string };
  } | null;
  return payload?.error?.code;
}

async function readEarnMax<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const code = await errorCodeOf(res);
    if (res.status === 403 && code === "invite_required") {
      throw new EarnMaxError("invite_required", "Earn MAX needs an invite.", 403);
    }
    if (res.status === 401) {
      throw new EarnMaxError(
        "unauthenticated",
        "Sign in again to use Earn MAX.",
        401,
      );
    }
    throw new EarnMaxError(
      "request_failed",
      `Earn MAX request failed (${res.status}${code ? ` ${code}` : ""}).`,
      res.status,
    );
  }
  const contract = res.headers.get("x-loyal-earn-max-contract");
  if (contract !== EARN_MAX_CONTRACT) {
    throw new EarnMaxError(
      "contract_mismatch",
      `Earn MAX server speaks ${contract ?? "no contract"}, this app needs ${EARN_MAX_CONTRACT}. Update the app.`,
      res.status,
    );
  }
  return (await res.json()) as T;
}

function readPath<T>(walletAddress: string, path: string): Promise<T> {
  return earnWalletRead(
    walletAddress,
    `${env.earnApiBaseUrl}${EARN_MAX_PATH}/${path}`,
  ).then((res) => readEarnMax<T>(res));
}

export function fetchEarnMaxSummary(
  walletAddress: string,
): Promise<EarnMaxSummaryResponse> {
  return readPath(walletAddress, "summary");
}

export function fetchEarnMaxActivity(
  walletAddress: string,
): Promise<EarnMaxActivityResponse> {
  return readPath(walletAddress, "activity");
}

export function fetchEarnMaxInvite(
  walletAddress: string,
): Promise<EarnMaxInviteResponse> {
  return readPath(walletAddress, "invite");
}

export async function redeemEarnMaxInvite(
  signer: Signer,
  code: string,
): Promise<"redeemed" | "invalid"> {
  const token = await ensureEarnSession(signer);
  if (!token) {
    throw new EarnMaxError(
      "unauthenticated",
      "Sign in again to use Earn MAX.",
      null,
    );
  }
  const res = await fetch(`${env.earnApiBaseUrl}${EARN_MAX_PATH}/invite`, {
    method: "POST",
    headers: { ...earnHeaders(), Authorization: `Bearer ${token}` },
    body: JSON.stringify({ code }),
  });
  if (res.status === 400 || res.status === 409) {
    return "invalid";
  }
  await readEarnMax<EarnMaxInviteResponse>(res);
  return "redeemed";
}

export type EarnMaxAccount = { programId: string; settingsPda: string };

export async function resolveEarnMaxAccount(
  walletAddress: string,
  config: EarnMaxSummaryResponse["config"],
): Promise<EarnMaxAccount> {
  if (config.walletAddress && config.walletAddress !== walletAddress) {
    throw new EarnMaxError(
      "unauthenticated",
      "Earn MAX answered for a different wallet.",
      null,
    );
  }
  const settingsPda =
    config.settingsPda ?? (await fetchEarnState(walletAddress)).settingsPda;
  if (!settingsPda) {
    throw new EarnMaxError(
      "request_failed",
      "This wallet has no smart account for Earn MAX yet.",
      null,
    );
  }
  return { programId: config.programId, settingsPda };
}

export type EarnMaxVoltrAction =
  | { kind: "deposit"; amountRaw: bigint }
  | { kind: "request_withdrawal"; amountRaw: bigint | "max" }
  | { kind: "claim" };

function planFor(action: EarnMaxVoltrAction): VoltrPlanBuilder {
  const connection = getConnection();
  switch (action.kind) {
    case "deposit":
      return async (context) => voltrDepositPlan(context, action.amountRaw);
    case "request_withdrawal":
      return (context) =>
        voltrRequestWithdrawalPlan(connection, context, action.amountRaw);
    case "claim":
      return (context) => voltrClaimPlan(connection, context);
  }
}

export class EarnMaxWalletChangedError extends Error {
  constructor() {
    super("The active wallet changed. Earn MAX action cancelled.");
    this.name = "EarnMaxWalletChangedError";
  }
}

export async function sendEarnMaxVoltr(args: {
  account: EarnMaxAccount;
  action: EarnMaxVoltrAction;
  signer: Signer;
  assertActive?: () => void;
}): Promise<SentTransaction> {
  const connection = getConnection();
  const programId = new PublicKey(args.account.programId);
  args.assertActive?.();
  const operation = await prepareEarnMaxVoltr({
    build: planFor(args.action),
    client: createSmartAccountVaultsClient({ connection, programId }),
    owner: args.signer.publicKey,
    programId,
    settingsPda: new PublicKey(args.account.settingsPda),
  });
  args.assertActive?.();
  return signAndSendPreparedOperation({
    connection,
    operation,
    signer: args.signer,
  });
}
