import type { Get, Post, ReferralClaimRequest } from "@loyal-labs/api";

import type { ClaimFailure } from "./copy";

export type ClaimOutcome =
  | { kind: "bound" }
  | { kind: "failed"; failure: ClaimFailure; answered: boolean };

export async function claimCode(
  request: ReferralClaimRequest
): Promise<ClaimOutcome> {
  let response: Response;
  try {
    response = await fetch("/api/referrals/claim", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
  } catch {
    return { kind: "failed", failure: "retry", answered: false };
  }
  if (response.status === 400) {
    return { kind: "failed", failure: "malformed", answered: true };
  }
  if (response.status === 429) {
    return { kind: "failed", failure: "rate_limited", answered: false };
  }
  const body = response.ok
    ? ((await response.json().catch(() => null)) as
        | Post["/api/referrals/claim"]
        | null)
    : null;
  if (body?.bound) return { kind: "bound" };
  if (body?.refusal) {
    return { kind: "failed", failure: body.refusal, answered: true };
  }
  return { kind: "failed", failure: "retry", answered: false };
}

export async function readClaimWindow(): Promise<string | null> {
  try {
    const response = await fetch("/api/referrals", { credentials: "include" });
    if (!response.ok) return null;
    const body = (await response.json()) as Get["/api/referrals"];
    return body.can_claim ? body.closes_at : null;
  } catch {
    return null;
  }
}

export async function codeExists(code: string): Promise<boolean | null> {
  try {
    const response = await fetch(`/api/referrals/codes/${code}`);
    if (!response.ok) return null;
    const body = (await response.json()) as Get["/api/referrals/codes/{code}"];
    return body.valid;
  } catch {
    return null;
  }
}
