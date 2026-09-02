import assert from "node:assert/strict";
import { describe, test } from "node:test";
import { PublicKey, TransactionInstruction } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { catalogCustodies } from "./rwa-multiply-custodies.js";
import { cachedJupiterRowsByEdge, catalogSwapEdges, measureSignedJupiterPacket, validateJupiterHeader } from "./rwa-multiply-jupiter-headers.js";

describe("RWA Multiply custody and swap catalog", () => {
  test("derives nine unique authority-bound custody accounts", () => {
    const rows = catalogCustodies();
    assert.equal(rows.length, 9);
    assert.equal(new Set(rows.map(({ symbol }) => symbol)).size, 9);
    assert.equal(new Set(rows.map(({ mint }) => mint)).size, 9);
    assert.equal(new Set(rows.map(({ ata }) => ata)).size, 9);
    assert.ok(rows.every(({ lanes }) => lanes.length > 0));
  });

  test("retains exactly the frozen 52 directed edges", () => {
    const rows = catalogSwapEdges();
    assert.equal(rows.length, 52);
    assert.equal(new Set(rows.map(({ key }) => key)).size, 52);
    assert.ok(rows.every(({ source, destination }) => source.symbol !== destination.symbol));
  });

  test("accepts only an exact SharedAccountsRouteV2 authority and custody header", () => {
    const edge = catalogSwapEdges().find(({ key }) => key === "USDG->USDC")!;
    const keys = Array.from({ length: 12 }, () => ({
      pubkey: PublicKey.default, isSigner: false, isWritable: false,
    }));
    keys[1] = { pubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), isSigner: true, isWritable: false };
    keys[2] = { pubkey: new PublicKey(edge.source.ata), isSigner: false, isWritable: true };
    keys[5] = { pubkey: new PublicKey(edge.destination.ata), isSigner: false, isWritable: true };
    keys[6] = { pubkey: new PublicKey(edge.source.mint), isSigner: false, isWritable: false };
    keys[7] = { pubkey: new PublicKey(edge.destination.mint), isSigner: false, isWritable: false };
    keys[8] = { pubkey: new PublicKey(edge.source.tokenProgram), isSigner: false, isWritable: false };
    keys[9] = { pubkey: new PublicKey(edge.destination.tokenProgram), isSigner: false, isWritable: false };
    const data = Buffer.alloc(64);
    Buffer.from([209, 152, 83, 147, 124, 254, 216, 233]).copy(data);
    data.writeUInt16LE(50, 25);
    data[27] = 0;
    data.writeBigUInt64LE(1_000_000n, data.length - 19);
    data.writeBigUInt64LE(990_000n, data.length - 11);
    const instruction = new TransactionInstruction({
      programId: new PublicKey(RWA_MULTIPLY_ROUTE.programs.jupiter), keys, data,
    });
    assert.equal(validateJupiterHeader({ instruction, sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }).dialect, "shared-accounts-route-v2");
    keys[2] = { ...keys[2]!, pubkey: PublicKey.default };
    assert.throws(() => validateJupiterHeader({ instruction: new TransactionInstruction({
      programId: instruction.programId, keys, data }), sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }), /boundary 2 drifted/);
  });

  test("accepts the explicit legacy cross-token-program boundary at index 10", () => {
    const edge = catalogSwapEdges().find(({ key }) => key === "PRIME->USDG")!;
    const keys = Array.from({ length: 11 }, () => ({
      pubkey: PublicKey.default, isSigner: false, isWritable: false,
    }));
    keys[0] = { pubkey: new PublicKey(edge.source.tokenProgram), isSigner: false, isWritable: false };
    keys[2] = { pubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), isSigner: true, isWritable: false };
    keys[3] = { pubkey: new PublicKey(edge.source.ata), isSigner: false, isWritable: true };
    keys[6] = { pubkey: new PublicKey(edge.destination.ata), isSigner: false, isWritable: true };
    keys[7] = { pubkey: new PublicKey(edge.source.mint), isSigner: false, isWritable: false };
    keys[8] = { pubkey: new PublicKey(edge.destination.mint), isSigner: false, isWritable: false };
    keys[10] = { pubkey: new PublicKey(edge.destination.tokenProgram), isSigner: false, isWritable: false };
    const data = Buffer.alloc(48);
    Buffer.from("c1209b3341d69c81", "hex").copy(data);
    data.writeUInt16LE(50, data.length - 3);
    data[data.length - 1] = 0;
    data.writeBigUInt64LE(1_000_000n, data.length - 19);
    data.writeBigUInt64LE(990_000n, data.length - 11);
    const instruction = new TransactionInstruction({
      programId: new PublicKey(RWA_MULTIPLY_ROUTE.programs.jupiter), keys, data,
    });
    assert.equal(validateJupiterHeader({ instruction, sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }).dialect, "shared-accounts-route-cross-program");
    keys[10] = { ...keys[10]!, pubkey: PublicKey.default };
    assert.throws(() => validateJupiterHeader({ instruction: new TransactionInstruction({
      programId: instruction.programId, keys, data }), sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }), /lacks an exact cross-token-program layout/);
  });

  test("accepts the reverse legacy cross-token-program boundary at index 10", () => {
    const edge = catalogSwapEdges().find(({ key }) => key === "USDG->USDC")!;
    const keys = Array.from({ length: 11 }, () => ({
      pubkey: PublicKey.default, isSigner: false, isWritable: false,
    }));
    keys[0] = { pubkey: new PublicKey(edge.destination.tokenProgram), isSigner: false, isWritable: false };
    keys[2] = { pubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), isSigner: true, isWritable: false };
    keys[3] = { pubkey: new PublicKey(edge.source.ata), isSigner: false, isWritable: true };
    keys[6] = { pubkey: new PublicKey(edge.destination.ata), isSigner: false, isWritable: true };
    keys[7] = { pubkey: new PublicKey(edge.source.mint), isSigner: false, isWritable: false };
    keys[8] = { pubkey: new PublicKey(edge.destination.mint), isSigner: false, isWritable: false };
    keys[10] = { pubkey: new PublicKey(edge.source.tokenProgram), isSigner: false, isWritable: false };
    const data = Buffer.alloc(48);
    Buffer.from("c1209b3341d69c81", "hex").copy(data);
    data.writeUInt16LE(50, data.length - 3);
    data[data.length - 1] = 0;
    data.writeBigUInt64LE(1_000_000n, data.length - 19);
    data.writeBigUInt64LE(990_000n, data.length - 11);
    const instruction = new TransactionInstruction({
      programId: new PublicKey(RWA_MULTIPLY_ROUTE.programs.jupiter), keys, data,
    });
    assert.equal(validateJupiterHeader({ instruction, sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }).dialect, "shared-accounts-route-cross-program-reverse");
    keys[10] = { ...keys[10]!, pubkey: PublicKey.default };
    assert.throws(() => validateJupiterHeader({ instruction: new TransactionInstruction({
      programId: instruction.programId, keys, data }), sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }), /lacks an exact cross-token-program layout/);
  });

  test("accepts the explicit legacy Token-2022 shared-program boundary at index 10", () => {
    const edge = catalogSwapEdges().find(({ key }) => key === "USDG->PYUSD")!;
    const keys = Array.from({ length: 11 }, () => ({
      pubkey: PublicKey.default, isSigner: false, isWritable: false,
    }));
    keys[2] = { pubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), isSigner: true, isWritable: false };
    keys[3] = { pubkey: new PublicKey(edge.source.ata), isSigner: false, isWritable: true };
    keys[6] = { pubkey: new PublicKey(edge.destination.ata), isSigner: false, isWritable: true };
    keys[7] = { pubkey: new PublicKey(edge.source.mint), isSigner: false, isWritable: false };
    keys[8] = { pubkey: new PublicKey(edge.destination.mint), isSigner: false, isWritable: false };
    keys[10] = { pubkey: new PublicKey(edge.source.tokenProgram), isSigner: false, isWritable: false };
    const data = Buffer.alloc(48);
    Buffer.from("c1209b3341d69c81", "hex").copy(data);
    data.writeUInt16LE(50, data.length - 3);
    data[data.length - 1] = 0;
    data.writeBigUInt64LE(1_000_000n, data.length - 19);
    data.writeBigUInt64LE(990_000n, data.length - 11);
    const instruction = new TransactionInstruction({
      programId: new PublicKey(RWA_MULTIPLY_ROUTE.programs.jupiter), keys, data,
    });
    assert.equal(validateJupiterHeader({ instruction, sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }).dialect, "shared-accounts-route-token-2022");
    keys[10] = { ...keys[10]!, pubkey: PublicKey.default };
    assert.throws(() => validateJupiterHeader({ instruction: new TransactionInstruction({
      programId: instruction.programId, keys, data }), sourceMint: edge.source.mint,
      destinationMint: edge.destination.mint, sourceAta: edge.source.ata,
      destinationAta: edge.destination.ata, sourceTokenProgram: edge.source.tokenProgram,
      destinationTokenProgram: edge.destination.tokenProgram,
      amountRaw: 1_000_000n, outAmountRaw: 990_000n }), /lacks an exact shared token-program layout/);
  });

  test("measures a partially signed transport packet without a Squads private key", () => {
    const edge = catalogSwapEdges().find(({ key }) => key === "USDG->USDC")!;
    const instruction = new TransactionInstruction({
      programId: new PublicKey(RWA_MULTIPLY_ROUTE.programs.jupiter),
      keys: [{ pubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), isSigner: true, isWritable: false }],
      data: Buffer.from([209, 152, 83, 147, 124, 254, 216, 233]),
    });
    const first = measureSignedJupiterPacket(edge.key, instruction);
    const second = measureSignedJupiterPacket(edge.key, instruction);
    assert.equal(first.broadcastable, false);
    assert.equal(first.squadsPdaSignaturePending, true);
    assert.equal(first.requiredSignatureCount, 2);
    assert.equal(first.signedSignatureCount, 1);
    assert.ok(first.packetBytes > instruction.data.length);
    assert.equal(first.packetSha256, second.packetSha256);
  });

  test("reuses only a structurally revalidated dataBase64 cache row", () => {
    const edge = catalogSwapEdges().find(({ key }) => key === "USDG->USDC")!;
    const keys = Array.from({ length: 12 }, () => ({
      pubkey: PublicKey.default, isSigner: false, isWritable: false,
    }));
    keys[1] = { pubkey: new PublicKey(RWA_MULTIPLY_ROUTE.squads.vault), isSigner: true, isWritable: false };
    keys[2] = { pubkey: new PublicKey(edge.source.ata), isSigner: false, isWritable: true };
    keys[5] = { pubkey: new PublicKey(edge.destination.ata), isSigner: false, isWritable: true };
    keys[6] = { pubkey: new PublicKey(edge.source.mint), isSigner: false, isWritable: false };
    keys[7] = { pubkey: new PublicKey(edge.destination.mint), isSigner: false, isWritable: false };
    keys[8] = { pubkey: new PublicKey(edge.source.tokenProgram), isSigner: false, isWritable: false };
    keys[9] = { pubkey: new PublicKey(edge.destination.tokenProgram), isSigner: false, isWritable: false };
    const data = Buffer.alloc(64);
    Buffer.from([209, 152, 83, 147, 124, 254, 216, 233]).copy(data);
    data.writeUInt16LE(50, 25);
    data[27] = 0;
    data.writeBigUInt64LE(1_000_000n, data.length - 19);
    data.writeBigUInt64LE(990_000n, data.length - 11);
    const cache = {
      schema: "loyal-backyard-rwa-jupiter-header-evidence/v2",
      rows: [{
        key: edge.key,
        pass: true,
        source: edge.source,
        destination: edge.destination,
        quote: { inAmountRaw: "1000000", outAmountRaw: "990000" },
        instruction: {
          programId: RWA_MULTIPLY_ROUTE.programs.jupiter,
          dataBase64: data.toString("base64"),
          accounts: keys.map(({ pubkey, isSigner, isWritable }) => ({ pubkey: pubkey.toBase58(), isSigner, isWritable })),
        },
        lookupTables: [],
      }],
    };
    const reused = cachedJupiterRowsByEdge(cache).get(edge.key);
    assert.ok(reused);
    assert.equal(reused.packet !== undefined, true);
    assert.deepEqual(reused.observation, { source: "validated-cache" });
  });
});
