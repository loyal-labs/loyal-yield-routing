import {
  withRequestLifecycle,
  type RequestLifecycle,
} from "@/features/observability/lifecycle.server";
import { NextResponse } from "next/server";

import { resolveAuthenticatedPrincipalFromRequest } from "@/features/identity/server/auth-session";
import { parseEarnDepositConfirmRequestBody } from "@/lib/yield-optimization/earn-confirm-contracts.shared";
import {
  EarnDepositConfirmError,
  verifyConfirmedEarnDeposit,
} from "@/lib/yield-optimization/earn-deposit-confirm.server";
import { citeEarnDepositPolicies } from "@/lib/yield-optimization/earn-deposit-policy-citation.server";
import type { ConfirmedYieldDepositInput } from "@/lib/yield-optimization/yield-deposit-repository.server";

function jsonError(
  status: number,
  code: string,
  message: string
): NextResponse {
  return NextResponse.json({ error: { code, message } }, { status });
}

export async function POST(request: Request) {
  return withRequestLifecycle({
    request,
    flowName: "earn.deposit",
    flowVariant: "initial",
    stage: "backend_confirm",
    success: { chainState: "confirmed", persistenceState: "recorded" },
    run: (lifecycle) => handleRequest(request, lifecycle),
  });
}

async function handleRequest(
  request: Request,
  lifecycle: RequestLifecycle | null
) {
  const principal = await resolveAuthenticatedPrincipalFromRequest(request);

  if (!principal) {
    return jsonError(401, "unauthenticated", "No active auth session.");
  }

  lifecycle?.setVerifiedWallet(principal.walletAddress);

  let input: ConfirmedYieldDepositInput;
  try {
    input = parseEarnDepositConfirmRequestBody(await request.json());
  } catch (error) {
    return jsonError(
      400,
      "invalid_request",
      error instanceof Error ? error.message : "Invalid request body."
    );
  }

  const confirmPrincipal = {
    walletAddress: principal.walletAddress,
    smartAccountAddress: principal.smartAccountAddress,
    settingsPda: principal.settingsPda,
  };
  try {
    const position = await verifyConfirmedEarnDeposit({
      principal: confirmPrincipal,
      input: await citeEarnDepositPolicies({
        input,
        principal: confirmPrincipal,
      }),
    });
    return NextResponse.json({ position });
  } catch (error) {
    if (error instanceof EarnDepositConfirmError) {
      return jsonError(error.status, error.code, error.message);
    }
    throw error;
  }
}
