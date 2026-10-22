import bs58 from "bs58";
import { Buffer } from "buffer";

import { env } from "@/config/env";
import { getWalletSigner } from "@/lib/solana/wallet/wallet-details";
import type { Signer } from "@/lib/wallet/signer";

import type { EarnAuthFields } from "./earn-api";
import type { EarnAuthPurpose } from "./earn-auth";

const STORAGE_KEY = "earn.session.v1";
const EXPIRY_SAFETY_MS = 5 * 60 * 1000;
const SESSION_PURPOSE: EarnAuthPurpose = "earn-max";
const PROMPT_FREE_SIGNERS: ReadonlySet<Signer["kind"]> = new Set([
  "privy",
  "local",
]);

type StoredEarnSession = {
  walletAddress: string;
  token: string;
  expiresAt: string;
};

let cached: StoredEarnSession | null | undefined;
const mints = new Map<string, Promise<string | null>>();

export function earnHeaders(flowId?: string): Record<string, string> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
  };
  if (env.vercelProtectionBypass) {
    headers["x-vercel-protection-bypass"] = env.vercelProtectionBypass;
  }
  if (flowId) {
    headers["x-loyal-flow-id"] = flowId;
  }
  return headers;
}

export async function signEarnAuthFields(
  signer: Signer,
  purpose: EarnAuthPurpose,
): Promise<EarnAuthFields> {
  const walletAddress = signer.publicKey.toBase58();
  const issuedAt = new Date().toISOString();
  const message = [
    "Loyal Mobile Earn",
    `purpose: ${purpose}`,
    `wallet: ${walletAddress}`,
    `issuedAt: ${issuedAt}`,
  ].join("\n");
  const signature = await signer.signMessage(Buffer.from(message, "utf8"));
  return { walletAddress, signature: bs58.encode(signature), issuedAt };
}

async function secureStore() {
  return await import("expo-secure-store");
}

async function loadStored(): Promise<StoredEarnSession | null> {
  if (cached !== undefined) {
    return cached;
  }
  try {
    const raw = await (await secureStore()).getItemAsync(STORAGE_KEY);
    cached = raw ? (JSON.parse(raw) as StoredEarnSession) : null;
  } catch {
    cached = null;
  }
  return cached;
}

function isUsable(
  session: StoredEarnSession,
  walletAddress: string,
): boolean {
  const expiresAtMs = Date.parse(session.expiresAt);
  return (
    session.walletAddress === walletAddress &&
    Number.isFinite(expiresAtMs) &&
    expiresAtMs - EXPIRY_SAFETY_MS > Date.now()
  );
}

export async function getEarnSessionToken(
  walletAddress: string,
): Promise<string | null> {
  const stored = await loadStored();
  return stored && isUsable(stored, walletAddress) ? stored.token : null;
}

export async function clearEarnSession(): Promise<void> {
  cached = null;
  try {
    await (await secureStore()).deleteItemAsync(STORAGE_KEY);
  } catch {}
}

async function dropRejectedToken(token: string): Promise<void> {
  if ((await loadStored())?.token === token) {
    await clearEarnSession();
  }
}

async function mint(auth: EarnAuthFields): Promise<string | null> {
  const res = await fetch(
    `${env.earnApiBaseUrl}/api/smart-accounts/mobile/earn/session`,
    {
      method: "POST",
      headers: earnHeaders(),
      body: JSON.stringify(auth),
    },
  );
  if (!res.ok) {
    return null;
  }
  const payload = (await res.json()) as {
    token?: string;
    expiresAt?: string;
  };
  if (!payload.token || !payload.expiresAt) {
    return null;
  }
  cached = {
    expiresAt: payload.expiresAt,
    token: payload.token,
    walletAddress: auth.walletAddress,
  };
  await (
    await secureStore()
  ).setItemAsync(STORAGE_KEY, JSON.stringify(cached));
  return payload.token;
}

function mintOnce(
  walletAddress: string,
  sign: () => Promise<EarnAuthFields>,
): Promise<string | null> {
  const inFlight = mints.get(walletAddress);
  if (inFlight) {
    return inFlight;
  }
  const minting = (async () =>
    (await getEarnSessionToken(walletAddress)) ?? mint(await sign()))()
    .catch(() => null)
    .finally(() => mints.delete(walletAddress));
  mints.set(walletAddress, minting);
  return minting;
}

export function ensureEarnSession(signer: Signer): Promise<string | null> {
  return mintOnce(signer.publicKey.toBase58(), () =>
    signEarnAuthFields(signer, SESSION_PURPOSE),
  );
}

export function maybeMintEarnSession(auth: EarnAuthFields): void {
  void mintOnce(auth.walletAddress, async () => auth);
}

async function activeSignerFor(walletAddress: string): Promise<Signer | null> {
  try {
    const signer = await getWalletSigner();
    return signer.publicKey.toBase58() === walletAddress ? signer : null;
  } catch {
    return null;
  }
}

async function readToken(walletAddress: string): Promise<string | null> {
  const token = await getEarnSessionToken(walletAddress);
  if (token) {
    return token;
  }
  const signer = await activeSignerFor(walletAddress);
  return signer && PROMPT_FREE_SIGNERS.has(signer.kind)
    ? ensureEarnSession(signer)
    : null;
}

function getWithBearer(url: string, token: string | null): Promise<Response> {
  return fetch(url, {
    method: "GET",
    headers: token
      ? { ...earnHeaders(), Authorization: `Bearer ${token}` }
      : earnHeaders(),
  });
}

async function isInvalidSession(res: Response): Promise<boolean> {
  if (res.status !== 401) {
    return false;
  }
  const payload = (await res
    .clone()
    .json()
    .catch(() => null)) as { error?: { code?: string } } | null;
  return payload?.error?.code === "invalid_mobile_session";
}

export async function earnWalletRead(
  walletAddress: string,
  url: string,
): Promise<Response> {
  const token = await readToken(walletAddress);
  const res = await getWithBearer(url, token);
  if (!token || !(await isInvalidSession(res))) {
    return res;
  }
  await dropRejectedToken(token);
  const signer = await activeSignerFor(walletAddress);
  const fresh = signer ? await ensureEarnSession(signer) : null;
  return fresh ? getWithBearer(url, fresh) : res;
}
