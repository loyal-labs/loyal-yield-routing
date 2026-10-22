import type { Get, Post, RouteError } from "@loyal-labs/api";
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

import { earnHeaders, earnWalletRead, ensureEarnSession } from "./earn-session";
import {
  signAndSendPreparedOperation,
  type SentTransaction,
} from "./send-prepared";

export const EARN_MAX_CONTRACT = "earn-max-v4";

export type EarnMaxErrorCode =
  | "contract_mismatch"
  | "invite_required"
  | "unauthenticated"
  | "session_required"
  | "smart_account_required"
  | "unsupported_cluster"
  | "rate_limited"
  | "unavailable"
  | "request_failed";

const REFUSALS: Readonly<
  Record<string, { code: EarnMaxErrorCode; message: string }>
> = {
  unauthenticated: {
    code: "session_required",
    message: "Sign in to use Earn MAX.",
  },
  invalid_mobile_session: {
    code: "session_required",
    message: "Sign in to use Earn MAX.",
  },
  wallet_required: {
    code: "unauthenticated",
    message: "Sign in with a Solana wallet to use Earn MAX.",
  },
  smart_account_required: {
    code: "smart_account_required",
    message: "Set up your smart account to use Earn MAX.",
  },
  invite_required: {
    code: "invite_required",
    message: "Earn MAX needs an invite.",
  },
  unsupported_cluster: {
    code: "unsupported_cluster",
    message: "Earn MAX is only available on mainnet.",
  },
  rate_limited: {
    code: "rate_limited",
    message: "Too many attempts. Try again in a minute.",
  },
  earn_max_unavailable: {
    code: "unavailable",
    message: "Earn MAX is temporarily unavailable. Try again soon.",
  },
};

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
  const payload = (await res.json().catch(() => null)) as Partial<RouteError> | null;
  return payload?.error?.code;
}

async function readEarnMax<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const code = await errorCodeOf(res);
    const refusal = code ? REFUSALS[code] : undefined;
    if (refusal) {
      throw new EarnMaxError(refusal.code, refusal.message, res.status);
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

type EarnMaxReadPath = Extract<
  keyof Get,
  `/api/smart-accounts/earn-max/${string}`
>;

function readPath<P extends EarnMaxReadPath>(
  walletAddress: string,
  path: P,
): Promise<Get[P]> {
  return earnWalletRead(walletAddress, `${env.apiBaseUrl}${path}`).then((res) =>
    readEarnMax<Get[P]>(res),
  );
}

export function fetchEarnMaxSummary(walletAddress: string) {
  return readPath(walletAddress, "/api/smart-accounts/earn-max/summary");
}

export function fetchEarnMaxActivity(walletAddress: string) {
  return readPath(walletAddress, "/api/smart-accounts/earn-max/activity");
}

export function fetchEarnMaxInvite(walletAddress: string) {
  return readPath(walletAddress, "/api/smart-accounts/earn-max/invite");
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
  const res = await fetch(`${env.apiBaseUrl}/api/smart-accounts/earn-max/invite`, {
    method: "POST",
    headers: { ...earnHeaders(), Authorization: `Bearer ${token}` },
    body: JSON.stringify({ code }),
  });
  if (!res.ok) {
    const code = await errorCodeOf(res.clone());
    if (code === "invalid_code" || code === "code_used") {
      return "invalid";
    }
  }
  await readEarnMax<Post["/api/smart-accounts/earn-max/invite"]>(res);
  return "redeemed";
}

export type EarnMaxAccount = { programId: string; settingsPda: string };

export async function resolveEarnMaxAccount(
  walletAddress: string,
  config: Get["/api/smart-accounts/earn-max/summary"]["config"],
): Promise<EarnMaxAccount> {
  if (config.walletAddress !== walletAddress) {
    throw new EarnMaxError(
      "unauthenticated",
      "Earn MAX answered for a different wallet.",
      null,
    );
  }
  return { programId: config.programId, settingsPda: config.settingsPda };
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
