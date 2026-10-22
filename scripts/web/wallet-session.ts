import { createPrivateKey, createPublicKey, sign } from "node:crypto";

const ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
const PKCS8_ED25519_PREFIX = Buffer.from("302e020100300506032b657004220420", "hex");

export const MOBILE_SESSION_PATH = "/api/smart-accounts/mobile/earn/session";

const secrets = new Set<string>();

export function register(secret: string | undefined): string {
  if (secret && secret.length >= 8) secrets.add(secret);
  return secret ?? "";
}

export function scrub(text: string): string {
  let out = text;
  for (const secret of secrets) out = out.split(secret).join("[redacted]");
  return out;
}

export function base58Encode(bytes: Uint8Array): string {
  let value = 0n;
  for (const byte of bytes) value = (value << 8n) | BigInt(byte);
  let out = "";
  while (value > 0n) {
    out = ALPHABET[Number(value % 58n)] + out;
    value /= 58n;
  }
  for (const byte of bytes) {
    if (byte !== 0) break;
    out = "1" + out;
  }
  return out;
}

export function base58Decode(text: string): Uint8Array {
  let value = 0n;
  for (const char of text) {
    const digit = ALPHABET.indexOf(char);
    if (digit < 0) throw new Error("not base58");
    value = value * 58n + BigInt(digit);
  }
  const bytes: number[] = [];
  while (value > 0n) {
    bytes.unshift(Number(value & 0xffn));
    value >>= 8n;
  }
  for (const char of text) {
    if (char !== "1") break;
    bytes.unshift(0);
  }
  return Uint8Array.from(bytes);
}

function secretBytes(raw: string): Uint8Array {
  const trimmed = raw.trim();
  const bytes = trimmed.startsWith("[")
    ? Uint8Array.from(JSON.parse(trimmed) as number[])
    : /^[0-9a-fA-F]+$/.test(trimmed) && (trimmed.length === 64 || trimmed.length === 128)
      ? Uint8Array.from(Buffer.from(trimmed, "hex"))
      : base58Decode(trimmed);
  if (bytes.length !== 32 && bytes.length !== 64) {
    throw new Error(`POLICY_KEYPAIR decodes to ${bytes.length} bytes, need a 32-byte seed or 64-byte keypair`);
  }
  return bytes;
}

export type WalletKey = {
  address: string;
  publicKey: Uint8Array;
  sign: (message: string) => string;
};

export function walletKeyFromEnv(name = "POLICY_KEYPAIR"): WalletKey {
  const raw = process.env[name];
  if (!raw) {
    throw new Error(`${name} is unset. Run under: op run --env-file=.env.1password -- bun <script>`);
  }
  let seed: Uint8Array;
  try {
    seed = secretBytes(raw).subarray(0, 32);
  } catch (error) {
    throw new Error(scrub(error instanceof Error ? error.message : `${name} is not a valid key`));
  }
  register(raw);
  const privateKey = createPrivateKey({
    key: Buffer.concat([PKCS8_ED25519_PREFIX, seed]),
    format: "der",
    type: "pkcs8",
  });
  const jwk = createPublicKey(privateKey).export({ format: "jwk" });
  const publicKey = Uint8Array.from(Buffer.from(jwk.x as string, "base64url"));
  return {
    address: base58Encode(publicKey),
    publicKey,
    sign: (message) => base58Encode(sign(null, Buffer.from(message, "utf8"), privateKey)),
  };
}

export function mobileAuthMessage(walletAddress: string, issuedAt: string): string {
  return ["Loyal Mobile Earn", "purpose: earn-max", `wallet: ${walletAddress}`, `issuedAt: ${issuedAt}`].join("\n");
}

export async function mintMobileSession(
  origin: string,
  key: WalletKey,
): Promise<{ token: string; expiresAt: string }> {
  const issuedAt = new Date().toISOString();
  const response = await fetch(new URL(MOBILE_SESSION_PATH, origin), {
    method: "POST",
    redirect: "error",
    signal: AbortSignal.timeout(15_000),
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      walletAddress: key.address,
      signature: key.sign(mobileAuthMessage(key.address, issuedAt)),
      issuedAt,
    }),
  });
  const body = (await response.json().catch(() => null)) as {
    token?: unknown;
    expiresAt?: unknown;
    error?: { code?: unknown };
  } | null;
  if (!response.ok || typeof body?.token !== "string" || typeof body.expiresAt !== "string") {
    const code = typeof body?.error?.code === "string" ? ` ${body.error.code}` : "";
    throw new Error(`session mint at ${origin} failed: HTTP ${response.status}${code}`);
  }
  register(body.token);
  return { token: body.token, expiresAt: body.expiresAt };
}

export function tamper(token: string): string {
  const parts = token.split(".");
  const last = parts.length - 1;
  const first = parts[last]![0] === "A" ? "B" : "A";
  parts[last] = first + parts[last]!.slice(1);
  return parts.join(".");
}
