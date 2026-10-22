import bs58 from "bs58";
import { Buffer } from "buffer";

import { env } from "@/config/env";
import { getWalletSigner } from "@/lib/solana/wallet/wallet-details";
import type { Signer } from "@/lib/wallet/signer";

import type { EarnAuthFields } from "./earn-api";
import type { EarnAuthPurpose } from "./earn-auth";

const STORAGE_KEY = "earn.sessions.v2";
const LEGACY_STORAGE_KEY = "earn.session.v1";
const EXPIRY_SAFETY_MS = 5 * 60 * 1000;
const FAILED_MINT_COOLDOWN_MS = 60 * 1000;
const SESSION_PURPOSE: EarnAuthPurpose = "earn-max";
const SILENT_SIGNERS: ReadonlySet<Signer["kind"]> = new Set(["privy", "local"]);

type StoredEarnSession = { token: string; expiresAt: string };
type LegacyRecord = StoredEarnSession & { walletAddress: string };

let cached: Map<string, StoredEarnSession> | undefined;
let generation = 0;
const mints = new Map<string, Promise<string | null>>();
const failedMintAt = new Map<string, number>();

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

async function loadStored(): Promise<Map<string, StoredEarnSession>> {
  if (cached) {
    return cached;
  }
  const loaded = new Map<string, StoredEarnSession>();
  try {
    const store = await secureStore();
    const raw = await store.getItemAsync(STORAGE_KEY);
    if (raw) {
      for (const [wallet, entry] of Object.entries(
        JSON.parse(raw) as Record<string, StoredEarnSession>,
      )) {
        loaded.set(wallet, entry);
      }
    } else {
      const legacy = await store.getItemAsync(LEGACY_STORAGE_KEY);
      if (legacy) {
        const { walletAddress, ...entry } = JSON.parse(legacy) as LegacyRecord;
        loaded.set(walletAddress, entry);
      }
    }
  } catch {}
  cached ??= loaded;
  return cached;
}

async function persist(sessions: Map<string, StoredEarnSession>) {
  const snapshot = JSON.stringify(Object.fromEntries(sessions));
  try {
    const store = await secureStore();
    await store.setItemAsync(STORAGE_KEY, snapshot);
    await store.deleteItemAsync(LEGACY_STORAGE_KEY);
  } catch {}
}

function isUsable(session: StoredEarnSession): boolean {
  const expiresAtMs = Date.parse(session.expiresAt);
  return (
    Number.isFinite(expiresAtMs) && expiresAtMs - EXPIRY_SAFETY_MS > Date.now()
  );
}

export async function getEarnSessionToken(
  walletAddress: string,
): Promise<string | null> {
  const stored = (await loadStored()).get(walletAddress);
  return stored && isUsable(stored) ? stored.token : null;
}

export async function clearEarnSession(): Promise<void> {
  generation += 1;
  cached = new Map();
  mints.clear();
  failedMintAt.clear();
  try {
    const store = await secureStore();
    await store.deleteItemAsync(STORAGE_KEY);
    await store.deleteItemAsync(LEGACY_STORAGE_KEY);
  } catch {}
}

export async function dropRejectedToken(
  walletAddress: string,
  token: string,
): Promise<void> {
  const startedAt = generation;
  const sessions = await loadStored();
  if (startedAt === generation && sessions.get(walletAddress)?.token === token) {
    sessions.delete(walletAddress);
    await persist(sessions);
  }
}

async function mint(
  auth: EarnAuthFields,
  startedAt: number,
): Promise<string | null> {
  const res = await fetch(
    `${env.apiBaseUrl}/api/smart-accounts/mobile/earn/session`,
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
  if (!payload.token || !payload.expiresAt || startedAt !== generation) {
    return null;
  }
  const sessions = await loadStored();
  if (startedAt !== generation) {
    return null;
  }
  sessions.set(auth.walletAddress, {
    expiresAt: payload.expiresAt,
    token: payload.token,
  });
  await persist(sessions);
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
  const startedAt = generation;
  const minting: Promise<string | null> = (async () => {
    const existing = await getEarnSessionToken(walletAddress);
    if (existing) {
      return existing;
    }
    const auth = await sign();
    return startedAt === generation ? await mint(auth, startedAt) : null;
  })()
    .catch(() => null)
    .then((token) => {
      if (!token && startedAt === generation) {
        failedMintAt.set(walletAddress, Date.now());
      }
      return token;
    })
    .finally(() => {
      if (mints.get(walletAddress) === minting) {
        mints.delete(walletAddress);
      }
    });
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

function coolingDown(walletAddress: string): boolean {
  const failedAt = failedMintAt.get(walletAddress);
  return (
    failedAt !== undefined && Date.now() - failedAt < FAILED_MINT_COOLDOWN_MS
  );
}

export async function deliberateSession(
  signer: Signer,
): Promise<string | null> {
  return coolingDown(signer.publicKey.toBase58())
    ? null
    : ensureEarnSession(signer);
}

export async function silentSession(signer: Signer): Promise<string | null> {
  return SILENT_SIGNERS.has(signer.kind) ? deliberateSession(signer) : null;
}

export async function silentEarnToken(
  walletAddress: string,
): Promise<string | null> {
  const token = await getEarnSessionToken(walletAddress);
  if (token) {
    return token;
  }
  const signer = await activeSignerFor(walletAddress);
  return signer ? silentSession(signer) : null;
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
  const startedAt = generation;
  const token = await silentEarnToken(walletAddress);
  const res = await getWithBearer(url, token);
  if (!token || !(await isInvalidSession(res))) {
    return res;
  }
  await dropRejectedToken(walletAddress, token);
  const signer = await activeSignerFor(walletAddress);
  const fresh = signer ? await silentSession(signer) : null;
  if (!fresh) {
    return res;
  }
  const retried = await getWithBearer(url, fresh);
  const rejected = await isInvalidSession(retried);
  if (rejected && startedAt === generation) {
    failedMintAt.set(walletAddress, Date.now());
  }
  return retried;
}
