import { requireOptionalNativeModule } from "expo-modules-core";

export type KeyAuth = "device" | "biometry";

export type UnwrapFailure = "canceled" | "invalidated" | "failed";

export type UnwrapResult =
  | { ok: true; plaintext: string }
  | { ok: false; reason: UnwrapFailure };

type NativeHardwareKey = {
  isSupported(): boolean;
  ensureKey(alias: string, auth: KeyAuth): Promise<void>;
  replaceKey(alias: string, auth: KeyAuth): Promise<void>;
  wrap(alias: string, plaintext: string, prompt: string): Promise<string>;
  unwrap(alias: string, wrapped: string, prompt: string): Promise<string>;
  deleteKey(alias: string): Promise<void>;
};

const FAILURE_BY_CODE: Record<string, UnwrapFailure> = {
  ERR_HARDWARE_KEY_CANCELED: "canceled",
  ERR_HARDWARE_KEY_INVALIDATED: "invalidated",
};

let cached: NativeHardwareKey | null | undefined;

function getNative(): NativeHardwareKey | null {
  if (cached === undefined) {
    const native =
      requireOptionalNativeModule<NativeHardwareKey>("ExpoHardwareKey");
    cached = native && native.isSupported() ? native : null;
  }
  return cached;
}

function requireNative(): NativeHardwareKey {
  const native = getNative();
  if (!native) throw new Error("Hardware key is not available on this device");
  return native;
}

export function isAvailable(): boolean {
  return getNative() != null;
}

export async function ensureKey(alias: string, auth: KeyAuth): Promise<void> {
  await requireNative().ensureKey(alias, auth);
}

export async function replaceKey(alias: string, auth: KeyAuth): Promise<void> {
  await requireNative().replaceKey(alias, auth);
}

export async function wrap(
  alias: string,
  plaintext: string,
  prompt: string,
): Promise<string> {
  return requireNative().wrap(alias, plaintext, prompt);
}

export async function unwrap(
  alias: string,
  wrapped: string,
  prompt: string,
): Promise<UnwrapResult> {
  try {
    return {
      ok: true,
      plaintext: await requireNative().unwrap(alias, wrapped, prompt),
    };
  } catch (error) {
    const code = (error as { code?: unknown } | null)?.code;
    const reason =
      typeof code === "string" ? FAILURE_BY_CODE[code] : undefined;
    return { ok: false, reason: reason ?? "failed" };
  }
}

export async function deleteKey(alias: string): Promise<void> {
  const native = getNative();
  if (!native) return;
  await native.deleteKey(alias);
}
