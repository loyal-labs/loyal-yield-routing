import { referralsEnabled } from "./enabled";

export type PendingCode = { code: string; source: "link" | "typed" };

const STORAGE_KEY = "loyal.referral.pending";
export const CODE_LENGTH = 7;
const CODE_PATTERN = new RegExp(`^[0-9A-HJKMNP-TV-Z]{${CODE_LENGTH}}$`);

export function normalizeCode(raw: string): string | null {
  const code = raw
    .replace(/\s/g, "")
    .toUpperCase()
    .replace(/O/g, "0")
    .replace(/[IL]/g, "1");
  return CODE_PATTERN.test(code) ? code : null;
}

function storage(): Storage | null {
  if (typeof window === "undefined") return null;
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

export function readPendingCode(): PendingCode | null {
  if (!referralsEnabled()) return null;
  try {
    const raw = storage()?.getItem(STORAGE_KEY);
    if (!raw) return null;
    const value: unknown = JSON.parse(raw);
    if (typeof value !== "object" || value === null) return null;
    const { code, source } = value as Record<string, unknown>;
    if (typeof code !== "string") return null;
    return source === "link" || source === "typed" ? { code, source } : null;
  } catch {
    return null;
  }
}

export function writePendingCode(pending: PendingCode): void {
  try {
    storage()?.setItem(STORAGE_KEY, JSON.stringify(pending));
  } catch {}
}

export function clearPendingCode(): void {
  try {
    storage()?.removeItem(STORAGE_KEY);
  } catch {}
}

export function storeLinkCode(raw: string | null | undefined): boolean {
  if (!referralsEnabled()) return false;
  const code = normalizeCode(raw ?? "");
  if (!code) return false;
  writePendingCode({ code, source: "link" });
  return true;
}

export function withPendingLinkRef(href: string): string {
  const pending = readPendingCode();
  if (pending?.source !== "link") return href;
  try {
    const url = new URL(href);
    url.searchParams.set("ref", pending.code);
    return url.toString();
  } catch {
    return href;
  }
}
