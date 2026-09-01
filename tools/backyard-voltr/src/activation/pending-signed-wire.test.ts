import { createHash } from "node:crypto";
import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { Keypair, PublicKey, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import { verifyPendingSignedWire } from "./pending-signed-wire.js";

function fixture() {
  const signer = Keypair.generate();
  const message = new TransactionMessage({
    payerKey: signer.publicKey,
    recentBlockhash: new PublicKey(new Uint8Array(32).fill(7)).toBase58(),
    instructions: [],
  }).compileToV0Message();
  const transaction = new VersionedTransaction(message);
  transaction.sign([signer]);
  const wire = transaction.serialize();
  return {
    schema: "activation/test-v1",
    verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND",
    broadcast: true,
    phase: "initialize",
    transaction: {
      expectedSignature: bs58.encode(transaction.signatures[0]!),
      wireSha256: createHash("sha256").update(wire).digest("hex"),
    },
    signedWireBase64: Buffer.from(wire).toString("base64"),
  };
}

describe("verifyPendingSignedWire", () => {
  test("binds schema, phase, hash, and signature to one canonical signed wire", () => {
    const pending = fixture();
    const verified = verifyPendingSignedWire(pending, pending.schema, [pending.phase]);
    assert.equal(verified.expectedSignature, pending.transaction.expectedSignature);
    assert.equal(verified.wireSha256, pending.transaction.wireSha256);
  });

  test("rejects a journal whose wire hash drifted", () => {
    const pending = fixture();
    pending.transaction.wireSha256 = "0".repeat(64);
    assert.throws(
      () => verifyPendingSignedWire(pending, pending.schema, [pending.phase]),
      /signed wire hash drifted/,
    );
  });

  test("rejects a journal whose expected signature is not the wire signature", () => {
    const pending = fixture();
    pending.transaction.expectedSignature = bs58.encode(new Uint8Array(64).fill(1));
    assert.throws(
      () => verifyPendingSignedWire(pending, pending.schema, [pending.phase]),
      /expected signature does not match/,
    );
  });
});
