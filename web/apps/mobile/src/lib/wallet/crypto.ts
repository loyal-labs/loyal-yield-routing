import { gcm } from "@noble/ciphers/aes.js";
import { pbkdf2Async } from "@noble/hashes/pbkdf2.js";
import { sha256 } from "@noble/hashes/sha256.js";
import { Buffer } from "buffer";

// 10k iterations for pure-JS PBKDF2 on mobile.
// Browser extension uses 600k with native crypto.subtle (hardware-accelerated).
// Combined with AES-256-GCM + hardware-backed SecureStore this is adequate.
const PBKDF2_ITERATIONS = 10_000;

const BLOB_MAGIC = 0x4c;
const LEGACY_JSON_VERSION = 0;
const HEADER_VERSION = 1;
const KDF_PBKDF2_SHA256_10K = 1;
const SALT_BYTES = 16;
const IV_BYTES = 12;
const HEADER_BYTES = 3;

export type ParsedBlob = {
  version: typeof LEGACY_JSON_VERSION | typeof HEADER_VERSION;
  kdfId: typeof KDF_PBKDF2_SHA256_10K;
  salt: Uint8Array;
  iv: Uint8Array;
  data: Uint8Array;
};

function parseLegacyJsonBlob(blob: string): ParsedBlob | null {
  const { salt, iv, data } = JSON.parse(blob);
  if (!Array.isArray(salt) || !Array.isArray(iv) || !Array.isArray(data)) {
    return null;
  }
  return {
    version: LEGACY_JSON_VERSION,
    kdfId: KDF_PBKDF2_SHA256_10K,
    salt: new Uint8Array(salt),
    iv: new Uint8Array(iv),
    data: new Uint8Array(data),
  };
}

function parseHeaderBlob(blob: string): ParsedBlob | null {
  const bytes = new Uint8Array(Buffer.from(blob, "base64"));
  const bodyStart = HEADER_BYTES + SALT_BYTES + IV_BYTES;
  if (bytes.length <= bodyStart || bytes[0] !== BLOB_MAGIC) return null;
  if (bytes[1] !== HEADER_VERSION || bytes[2] !== KDF_PBKDF2_SHA256_10K) {
    return null;
  }
  return {
    version: HEADER_VERSION,
    kdfId: KDF_PBKDF2_SHA256_10K,
    salt: bytes.slice(HEADER_BYTES, HEADER_BYTES + SALT_BYTES),
    iv: bytes.slice(HEADER_BYTES + SALT_BYTES, bodyStart),
    data: bytes.slice(bodyStart),
  };
}

export function parseBlob(blob: string): ParsedBlob | null {
  try {
    return blob.startsWith("{")
      ? parseLegacyJsonBlob(blob)
      : parseHeaderBlob(blob);
  } catch {
    return null;
  }
}

async function deriveKey(
  password: string,
  salt: Uint8Array,
): Promise<Uint8Array> {
  return pbkdf2Async(sha256, new TextEncoder().encode(password), salt, {
    c: PBKDF2_ITERATIONS,
    dkLen: 32,
  });
}

export async function encryptSecret(
  plaintext: string,
  password: string,
): Promise<string> {
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const key = await deriveKey(password, salt);
  const aes = gcm(key, iv);
  const encrypted = aes.encrypt(new TextEncoder().encode(plaintext));
  return JSON.stringify({
    salt: Array.from(salt),
    iv: Array.from(iv),
    data: Array.from(encrypted),
  });
}

export async function decryptSecret(
  ciphertext: string,
  password: string,
): Promise<string | null> {
  const blob = parseBlob(ciphertext);
  if (!blob) return null;
  try {
    const key = await deriveKey(password, blob.salt);
    const aes = gcm(key, blob.iv);
    const decrypted = aes.decrypt(blob.data);
    return new TextDecoder().decode(decrypted);
  } catch {
    return null;
  }
}
