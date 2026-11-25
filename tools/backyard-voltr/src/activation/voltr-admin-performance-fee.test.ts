import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { test } from "node:test";

import { findAssociatedTokenPda } from "@solana-program/token";
import { address, createKeyPairSignerFromBytes } from "@solana/kit";
import { Keypair, PublicKey, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import { findProtocolPda, findVaultLpMintPda } from "@voltr/vault-sdk";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE as route } from "../domain/rwa-multiply-route-spec.js";
import { toWeb3Instruction } from "../integrations/solana-compat.js";
import { verifyPendingSignedWire } from "./pending-signed-wire.js";
import { buildInstructions } from "./voltr-admin-performance-fee.js";

const looping = address("GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1");
const cases = [
  { vault: route.vault.address, bps: 1500, feeBytes: [0xdc, 0x05] },
  { vault: looping, bps: 1500, feeBytes: [0xdc, 0x05] },
  { vault: looping, bps: 0, feeBytes: [0, 0] },
];

for (const { vault, bps, feeBytes } of cases) {
  test(`sign and verify the ${bps} bps fee update wire for ${vault}`, async () => {
    const keypair = Keypair.generate();
    const admin = await createKeyPairSignerFromBytes(keypair.secretKey);
    const [lpMint] = await findVaultLpMintPda({ vault });
    const [ata] = await findAssociatedTokenPda({ owner: admin.address, mint: lpMint, tokenProgram: route.assets.tokenProgram });
    const [protocol] = await findProtocolPda();
    const instructions = await buildInstructions(admin, vault, lpMint, ata, bps);
    const transaction = new VersionedTransaction(new TransactionMessage({
      payerKey: keypair.publicKey,
      recentBlockhash: PublicKey.default.toBase58(),
      instructions: instructions.map(toWeb3Instruction),
    }).compileToV0Message());
    transaction.sign([keypair]);
    const wire = Buffer.from(transaction.serialize());
    const record = {
      schema: "fee-wire-test/v1",
      verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND",
      broadcast: true,
      phase: "fee-update",
      signedWireBase64: wire.toString("base64"),
      transaction: {
        expectedSignature: bs58.encode(transaction.signatures[0]!),
        wireSha256: createHash("sha256").update(wire).digest("hex"),
      },
    };
    const persisted = verifyPendingSignedWire(record, record.schema, [record.phase]);
    const signed = VersionedTransaction.deserialize(persisted.wire);
    assert.equal(signed.message.header.numRequiredSignatures, 1);
    assert.equal(await crypto.subtle.verify("Ed25519", admin.keyPair.publicKey,
      signed.signatures[0]!, signed.message.serialize()), true);
    const decoded = TransactionMessage.decompile(signed.message);
    assert.equal(decoded.payerKey.toBase58(), admin.address);
    assert.deepEqual(decoded.instructions.map(ix => ix.programId.toBase58()), [route.assets.associatedTokenProgram, route.programs.voltr]);
    const [createAta, setFee] = decoded.instructions;
    assert.deepEqual(createAta!.keys.map(key => key.pubkey.toBase58()),
      [admin.address, ata, admin.address, lpMint, route.programs.system, route.assets.tokenProgram]);
    assert.deepEqual([...createAta!.data], [1]);
    assert.deepEqual(setFee!.keys.map(key => key.pubkey.toBase58()),
      [admin.address, protocol, vault, "SysvarRent111111111111111111111111111111111"]);
    assert.ok(setFee!.keys[0]!.isSigner && setFee!.keys[2]!.isWritable);
    // Voltr update_vault_config discriminator, AdminPerformanceFee field, two-byte LE payload.
    assert.deepEqual([...setFee!.data], [122, 3, 21, 222, 158, 255, 238, 157, 5, 2, 0, 0, 0, ...feeBytes]);
  });
}
