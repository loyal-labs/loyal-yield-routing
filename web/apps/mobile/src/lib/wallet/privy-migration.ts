import { Buffer } from "buffer";

import { mmkv } from "@/lib/storage";

import { isWalletRejection } from "./rejection";
import type { Signer } from "./signer";

export const PRIVY_MIGRATION_DONE_KEY = "loyal:privy-migration-done";
// Domain/uri are echoed into the SIWS message; Privy validates the domain
// against the app's allowed origins.
export const SIWS_DOMAIN = "askloyal.com";
export const SIWS_URI = "https://askloyal.com/mobile";

export type SiwsLogin<User = unknown> = {
  generateMessage(args: {
    wallet: { address: string };
    from: { domain: string; uri: string };
  }): Promise<{ message: string }>;
  login(args: { message: string; signature: string }): Promise<User>;
};

export async function signInWithSigner<User>(
  signer: Signer,
  siws: SiwsLogin<User>,
  signal?: AbortSignal,
): Promise<User> {
  const { message } = await siws.generateMessage({
    wallet: { address: signer.publicKey.toBase58() },
    from: { domain: SIWS_DOMAIN, uri: SIWS_URI },
  });
  if (signal?.aborted) throw new Error("Sign-in was cancelled.");
  const signature = await signer.signMessage(Buffer.from(message, "utf8"));
  if (signal?.aborted) throw new Error("Sign-in was cancelled.");
  return siws.login({ message, signature: Buffer.from(signature).toString("base64") });
}

type DoneFlag = {
  getBoolean(key: string): boolean | undefined;
  setBoolean(key: string, value: boolean): void;
};

export type PrivyMigrationResult = "done" | "skipped" | "declined" | "failed";

export async function migrateSignerToPrivy(
  signer: Signer,
  siws: SiwsLogin,
  storage: DoneFlag = mmkv,
): Promise<PrivyMigrationResult> {
  if (storage.getBoolean(PRIVY_MIGRATION_DONE_KEY)) return "skipped";
  try {
    await signInWithSigner(signer, siws);
    storage.setBoolean(PRIVY_MIGRATION_DONE_KEY, true);
    return "done";
  } catch (error) {
    if (isWalletRejection(error)) return "declined";
    console.warn("[privy] migration failed", error);
    return "failed";
  }
}
