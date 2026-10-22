import { env } from "@/config/env";
import { earnHeaders, ensureEarnSession } from "@/lib/solana/earn/earn-session";
import type { Signer } from "@/lib/wallet/signer";

export class PrivySessionError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "PrivySessionError";
  }
}

export async function exchangePrivySession(
  identityToken: string,
  signer: Signer,
): Promise<void> {
  const res = await fetch(`${env.earnApiBaseUrl}/api/auth/privy/complete`, {
    method: "POST",
    headers: { ...earnHeaders(), "privy-id-token": identityToken },
    body: JSON.stringify({ walletAddress: signer.publicKey.toBase58() }),
  });
  if (res.ok) {
    await ensureEarnSession(signer);
    return;
  }
  const body = (await res.json().catch(() => null)) as {
    error?: { code?: string; message?: string };
  } | null;
  throw new PrivySessionError(
    body?.error?.code ?? `http_${res.status}`,
    body?.error?.message ?? "Sign-in failed.",
  );
}
