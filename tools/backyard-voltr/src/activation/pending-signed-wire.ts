import { createHash } from "node:crypto";

import { VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

export type VerifiedPendingSignedWire = Readonly<{
  record: Record<string, unknown>;
  phase: string;
  expectedSignature: string;
  wireSha256: string;
  wire: Uint8Array;
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function record(value: unknown, label: string): Record<string, unknown> {
  invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`);
  return value as Record<string, unknown>;
}

export function verifyPendingSignedWire(
  input: unknown,
  expectedSchema: string,
  allowedPhases: readonly string[],
): VerifiedPendingSignedWire {
  const pending = record(input, "pending journal");
  invariant(pending.schema === expectedSchema, "pending journal schema drifted");
  invariant(pending.verdict === "SIGNED_SIMULATION_PASS_PENDING_SEND", "pending journal verdict drifted");
  invariant(pending.broadcast === true, "pending journal was not created by an execute path");
  const phase = String(pending.phase ?? "");
  invariant(allowedPhases.includes(phase), "pending journal phase drifted");
  const transaction = record(pending.transaction, "pending journal transaction");
  const expectedSignature = String(transaction.expectedSignature ?? "");
  const wireSha256 = String(transaction.wireSha256 ?? "");
  const wireBase64 = String(pending.signedWireBase64 ?? "");
  invariant(expectedSignature.length > 0, "pending journal lacks the expected signature");
  invariant(/^[0-9a-f]{64}$/.test(wireSha256), "pending journal wire hash is malformed");
  invariant(wireBase64.length > 0, "pending journal lacks the signed wire");
  const wire = Buffer.from(wireBase64, "base64");
  invariant(wire.length > 0 && wire.toString("base64") === wireBase64, "pending journal signed wire is not canonical base64");
  invariant(createHash("sha256").update(wire).digest("hex") === wireSha256, "pending journal signed wire hash drifted");
  let decoded: VersionedTransaction;
  try {
    decoded = VersionedTransaction.deserialize(wire);
  } catch {
    throw new Error("pending journal signed wire is not a versioned transaction");
  }
  invariant(decoded.signatures.length > 0, "pending journal signed wire has no signature");
  invariant(bs58.encode(decoded.signatures[0]!) === expectedSignature,
    "pending journal expected signature does not match its signed wire");
  return { record: pending, phase, expectedSignature, wireSha256, wire };
}
