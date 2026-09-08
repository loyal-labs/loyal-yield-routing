import { createHash } from "node:crypto";
import { chmodSync, readFileSync, renameSync, writeFileSync } from "node:fs";

import { Connection, PublicKey } from "@solana/web3.js";

import type { RwaMultiplyRouteSpec } from "../domain/rwa-multiply-route-spec.js";
import type { RwaMultiplyVoltrAccounts } from "../integrations/rwa-multiply-voltr.js";

export type StrategyTwoBootstrapPhase = "A" | "B" | "C";

type FinalizedAccount = Readonly<{
  owner: string | { toBase58(): string };
  data: Uint8Array;
}> | null | undefined;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function ownerOf(account: FinalizedAccount): string | null {
  if (account === null || account === undefined) return null;
  return typeof account.owner === "string" ? account.owner : account.owner.toBase58();
}

/**
 * Verify only the post-state. This deliberately has no "must be absent"
 * checks, so a crash after a successful send can be reconciled from a
 * pending journal even though the created account is already present.
 */
export function assertStrategyTwoBootstrapPoststate(input: Readonly<{
  phase: StrategyTwoBootstrapPhase;
  route: RwaMultiplyRouteSpec;
  finalized: readonly FinalizedAccount[];
  expectedConfigSha256?: string | null;
  expectedVaultSha256?: string | null;
}>): void {
  const [config, ticket, custody, vault] = input.finalized;
  if (input.phase === "A") {
    invariant(config !== null && config !== undefined
      && ownerOf(config) === input.route.customAdaptor.program
      && config.data.length === 472,
    "finalized wire A left no 472-byte adaptor config");
  } else if (input.phase === "B") {
    invariant(ticket !== null && ticket !== undefined
      && ownerOf(ticket) === input.route.customAdaptor.program,
    "finalized wire B left no report ticket");
    invariant(config !== null && config !== undefined
      && ownerOf(config) === input.route.customAdaptor.program
      && config.data.length === 472,
    "finalized wire B left no 472-byte adaptor config");
    invariant(input.expectedConfigSha256 !== null
      && input.expectedConfigSha256 !== undefined
      && sha256(config.data) === input.expectedConfigSha256,
    "finalized wire B changed the adaptor config");
  } else {
    invariant(custody !== null && custody !== undefined,
      "finalized wire C left no strategy custody ATA");
    invariant(vault !== null && vault !== undefined
      && ownerOf(vault) === input.route.programs.voltr,
    "finalized wire C left no active Voltr vault");
    invariant(input.expectedVaultSha256 !== null
      && input.expectedVaultSha256 !== undefined
      && sha256(vault.data) === input.expectedVaultSha256,
    "finalized wire C changed the Voltr vault: wire C must be state-neutral");
  }
}

function writePrivate(path: string, value: Record<string, unknown>): void {
  writeFileSync(path, `${JSON.stringify(value, (_key, entry) => typeof entry === "bigint" ? entry.toString() : entry, 2)}\n`, {
    flag: "wx",
    mode: 0o600,
  });
  chmodSync(path, 0o600);
}

type BootstrapReconcileConnection = Pick<Connection, "getSignatureStatuses" | "getMultipleAccountsInfo">;

/**
 * Reconcile one already-sent bootstrap wire before any fresh-state gate or
 * signing path is entered. The connection type intentionally exposes no send
 * method: this operation can only read, finalize, and journal.
 */
export async function reconcileStrategyTwoBootstrap(
  input: Readonly<{
    connection: BootstrapReconcileConnection;
    route: RwaMultiplyRouteSpec;
    accounts: RwaMultiplyVoltrAccounts;
    phase: StrategyTwoBootstrapPhase;
    journal: string;
  }>,
): Promise<Readonly<{ signature: string; finalizedSlot: number | null; finalizedContextSlot: number }>> {
  const pending = JSON.parse(readFileSync(`${input.journal}.pending`, "utf8")) as {
    phase?: unknown;
    config?: unknown;
    transaction?: {
      expectedSignature?: unknown;
      wireSha256?: unknown;
      configSha256Before?: unknown;
      vaultSha256Before?: unknown;
    };
  };
  invariant(pending.phase === input.phase, "pending journal records a different bootstrap wire");
  invariant(pending.config === input.route.customAdaptor.strategyConfig,
    "pending journal records a different strategy-two config identity");
  const signature = String(pending.transaction?.expectedSignature ?? "");
  const wireSha256 = String(pending.transaction?.wireSha256 ?? "");
  invariant(signature.length > 0, "pending journal lacks the expected signature");
  invariant(/^[0-9a-f]{64}$/.test(wireSha256), "pending journal lacks the wire digest");
  const status = await input.connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
  const landed = status.value[0];
  invariant(landed?.err === null && landed.confirmationStatus === "finalized",
    "pending bootstrap signature is not finalized successfully");
  const finalizedAccounts = await input.connection.getMultipleAccountsInfo([
    new PublicKey(input.route.customAdaptor.strategyConfig),
    new PublicKey(input.accounts.reportTicket),
    new PublicKey(input.accounts.strategyAssetAta),
    new PublicKey(input.route.vault.address),
  ], "finalized");
  assertStrategyTwoBootstrapPoststate({
    phase: input.phase,
    route: input.route,
    finalized: finalizedAccounts,
    expectedConfigSha256: input.phase === "B"
      ? String(pending.transaction?.configSha256Before ?? "")
      : null,
    expectedVaultSha256: input.phase === "C"
      ? String(pending.transaction?.vaultSha256Before ?? "")
      : null,
  });
  writePrivate(input.journal, { ...pending, verdict: "FINALIZED_RECONCILED", signature,
    finalizedSlot: landed.slot, finalizedContextSlot: status.context.slot });
  renameSync(`${input.journal}.pending`, `${input.journal}.sent-wire`);
  return {
    signature,
    finalizedSlot: landed.slot,
    finalizedContextSlot: status.context.slot,
  };
}
