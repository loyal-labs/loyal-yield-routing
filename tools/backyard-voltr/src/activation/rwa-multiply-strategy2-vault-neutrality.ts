import { Buffer } from "node:buffer";

import { getVaultDecoder, type Vault } from "@voltr/vault-sdk";

/**
 * The only vault field wire C may see move. Voltr stamps `lastUpdatedTs` on
 * every config write (and on initializeStrategy), so the manager round trip
 * BAqg -> initializeStrategy -> ST999 can never leave the account bytes
 * identical; the state-neutrality gate therefore compares the decoded vault
 * with this one field removed. Nested stamps such as
 * `highWaterMark.lastUpdatedTs` are still compared.
 */
export const VAULT_NEUTRALITY_IGNORED_FIELDS = ["lastUpdatedTs"] as const;

export type VaultNeutralityView = Readonly<{
  /** Stable JSON of the decoded vault minus the ignored field. */
  stateJson: string;
  manager: string;
  lastUpdatedTs: string;
}>;

/** Bigints and byte arrays become strings so the JSON survives a journal round trip. */
export function vaultNeutralityStateJson(vault: Vault): string {
  const { lastUpdatedTs: _ignored, ...compared } = vault;
  return JSON.stringify(compared, (_key, entry) => {
    if (typeof entry === "bigint") return entry.toString();
    if (entry instanceof Uint8Array) return Buffer.from(entry).toString("hex");
    return entry;
  });
}

/** Decoded neutrality view of raw vault bytes, or null when they do not decode. */
export function decodeVaultNeutralityView(
  data: Uint8Array | null | undefined,
): VaultNeutralityView | null {
  if (data == null) return null;
  try {
    const vault = getVaultDecoder().decode(data);
    return {
      stateJson: vaultNeutralityStateJson(vault),
      manager: vault.manager,
      lastUpdatedTs: vault.lastUpdatedTs.toString(),
    };
  } catch {
    return null;
  }
}

/**
 * The wire C state-neutrality verdict: every decoded vault field except the
 * ignored stamp is unchanged, the manager was the Squads vault before the wire
 * and is restored to it after, and the stamp never regresses.
 */
export function vaultNeutralityVerdict(input: Readonly<{
  before: VaultNeutralityView;
  after: VaultNeutralityView | null;
  expectedManager: string;
}>): Readonly<{ neutral: boolean; reason: string | null }> {
  if (input.after === null) {
    return { neutral: false, reason: "vault data did not decode as a Voltr Vault" };
  }
  if (input.before.manager !== input.expectedManager) {
    return {
      neutral: false,
      reason: `vault manager was ${input.before.manager} before the wire, expected ${input.expectedManager}`,
    };
  }
  if (input.after.manager !== input.expectedManager) {
    return {
      neutral: false,
      reason: `wire C left the Voltr manager as ${input.after.manager}, expected ${input.expectedManager}`,
    };
  }
  if (BigInt(input.after.lastUpdatedTs) < BigInt(input.before.lastUpdatedTs)) {
    return {
      neutral: false,
      reason: `vault lastUpdatedTs regressed from ${input.before.lastUpdatedTs} to ${input.after.lastUpdatedTs}`,
    };
  }
  if (input.after.stateJson !== input.before.stateJson) {
    return { neutral: false, reason: "a decoded vault field other than lastUpdatedTs changed" };
  }
  return { neutral: true, reason: null };
}
