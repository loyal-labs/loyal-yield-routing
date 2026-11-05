export type PendingCode = { code: string; source: "link" | "typed" };

const COOKIE_NAME = "loyal_ref";
const COOKIE_MAX_AGE_SECONDS = 30 * 24 * 60 * 60;
const PARENT_DOMAIN = "askloyal.com";
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

function cookieAttributes(maxAge: number): string {
  const { hostname, protocol } = window.location;
  const shared =
    hostname === PARENT_DOMAIN || hostname.endsWith(`.${PARENT_DOMAIN}`);
  return [
    "Path=/",
    `Max-Age=${maxAge}`,
    "SameSite=Lax",
    protocol === "https:" && "Secure",
    shared && `Domain=${PARENT_DOMAIN}`,
  ]
    .filter(Boolean)
    .join("; ");
}

export function readPendingCode(): PendingCode | null {
  if (typeof document === "undefined") return null;
  const entry = document.cookie
    .split("; ")
    .find((pair) => pair.startsWith(`${COOKIE_NAME}=`));
  if (!entry) return null;
  const [source, raw] = entry.slice(COOKIE_NAME.length + 1).split(":");
  const code = normalizeCode(raw ?? "");
  return code && (source === "link" || source === "typed")
    ? { code, source }
    : null;
}

export function writePendingCode(pending: PendingCode): void {
  if (typeof document === "undefined") return;
  document.cookie = `${COOKIE_NAME}=${pending.source}:${
    pending.code
  }; ${cookieAttributes(COOKIE_MAX_AGE_SECONDS)}`;
}

export function clearPendingCode(): void {
  if (typeof document === "undefined") return;
  document.cookie = `${COOKIE_NAME}=; ${cookieAttributes(0)}`;
}

export function storeLinkCode(raw: string | null | undefined): boolean {
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
