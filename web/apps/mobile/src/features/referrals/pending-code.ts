import type { ReferralClaimRequest } from "@loyal-labs/api";
import * as SecureStore from "expo-secure-store";

import { referralsEnabled } from "./enabled";

export type ReferralSource = ReferralClaimRequest["source"];
export type PendingCode = { code: string; source: ReferralSource };

const STORAGE_KEY = "referrals.pending.v1";
const CODE_PATTERN = /^[0-9A-HJKMNP-TV-Z]{7}$/;
const SOURCES: readonly ReferralSource[] = ["link", "typed", "install_referrer"];

let cached: PendingCode | null | undefined;
const listeners = new Set<() => void>();

export function notifyPending(): void {
  listeners.forEach((listener) => listener());
}

export function parseCode(raw: string): string | null {
  const code = raw.trim().toUpperCase();
  return CODE_PATTERN.test(code) ? code : null;
}

function decode(raw: string | null): PendingCode | null {
  if (!raw) return null;
  try {
    const value = JSON.parse(raw) as Partial<PendingCode>;
    const code = parseCode(value.code ?? "");
    const source = SOURCES.find((s) => s === value.source);
    return code && source ? { code, source } : null;
  } catch {
    return null;
  }
}

export async function readPending(): Promise<PendingCode | null> {
  if (cached !== undefined) return cached;
  let loaded: PendingCode | null = null;
  try {
    loaded = decode(await SecureStore.getItemAsync(STORAGE_KEY));
  } catch {}
  cached ??= loaded;
  return cached;
}

async function write(next: PendingCode | null): Promise<void> {
  cached = next;
  notifyPending();
  try {
    if (next) await SecureStore.setItemAsync(STORAGE_KEY, JSON.stringify(next));
    else await SecureStore.deleteItemAsync(STORAGE_KEY);
  } catch {}
}

export async function storePending(
  raw: string,
  source: ReferralSource,
): Promise<void> {
  if (!referralsEnabled()) return;
  const code = parseCode(raw);
  if (!code) return;
  if (source === "install_referrer" && (await readPending())) return;
  await write({ code, source });
}

export async function clearPending(only?: PendingCode): Promise<void> {
  const current = await readPending();
  if (!current) return;
  if (only && (only.code !== current.code || only.source !== current.source)) {
    return;
  }
  await write(null);
}

export function subscribePending(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
