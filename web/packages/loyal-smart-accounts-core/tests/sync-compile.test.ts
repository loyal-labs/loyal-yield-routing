import { describe, expect, it } from "bun:test";
import { PublicKey, TransactionInstruction } from "@solana/web3.js";
import { compileToSynchronousMessageAndAccountsV2 } from "../src/utils/compileToSynchronousMessageV2";

const vault = new PublicKey("Ch4HE6TezzRk6NDXYCyxwCrmVY6QgrhL6HiXxnM8aEVZ");
const member = new PublicKey("GmaDrppBC7P5ARKV8g3djiwP89vz1jLK23V2GBjuAEGB");
const memo = new PublicKey("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr");

describe("synchronous message compile", () => {
  it("keeps a read-only vault signer read-only", () => {
    const compiled = compileToSynchronousMessageAndAccountsV2({
      vaultPda: vault,
      members: [member],
      instructions: [
        new TransactionInstruction({
          programId: memo,
          keys: [{ pubkey: vault, isSigner: true, isWritable: false }],
          data: Buffer.from("hi"),
        }),
      ],
    });
    expect(
      compiled.accounts.map((a) => [
        a.pubkey.toBase58(),
        a.isSigner,
        a.isWritable,
      ])
    ).toEqual([
      [member.toBase58(), true, false],
      [vault.toBase58(), false, false],
      [memo.toBase58(), false, false],
    ]);
    expect(Buffer.from(compiled.instructions).toString("hex")).toBe(
      "010101000200" + "6869"
    );
  });
});
