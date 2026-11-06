import { describe, expect, test } from "bun:test";
import { AccountLayout, TOKEN_PROGRAM_ID } from "@solana/spl-token";
import { Connection, Keypair, type AccountInfo } from "@solana/web3.js";
import { voltrClaimPlan, type VoltrWithdrawalConsent } from "./plans";
import {
  VOLTR_ASSET_MINT,
  VOLTR_IDLE_AUTH,
  VOLTR_PROGRAM_ID,
  VOLTR_VAULT,
} from "./program";

const owner = Keypair.generate().publicKey;
const authority = Keypair.generate().publicKey;
const consent: VoltrWithdrawalConsent = {
  authority: authority.toBase58(),
  cluster: "mainnet-beta",
  destination: owner.toBase58(),
  expiresAt: "4102444800",
  feeBasisHash: "a".repeat(64),
  lpRaw: "100",
  maxUserFeeLamports: "5000",
  minClaimUsdcRaw: "100",
  nonce: "b".repeat(64),
  owner: owner.toBase58(),
  ownerSignature: "",
  program: VOLTR_PROGRAM_ID.toBase58(),
  referenceSlot: "1",
  referenceUsdcRaw: "100",
  requestInstruction: 0,
  requestSignature: "fixture",
  settings: Keypair.generate().publicKey.toBase58(),
  vault: VOLTR_VAULT.toBase58(),
  version: "1",
  policySeed: "0",
};

function fixture(payout: bigint, dust: bigint, hasAta = true) {
  const connection = new Connection("http://fixture.invalid");
  const info = (
    data: Buffer,
    program = TOKEN_PROGRAM_ID
  ): AccountInfo<Buffer> => ({
    data,
    executable: false,
    lamports: 3_000_000,
    owner: program,
    rentEpoch: 0,
  });
  const vault = Buffer.alloc(1024);
  vault.writeBigUInt64LE(payout, 168);
  const mint = Buffer.alloc(82);
  mint.writeBigUInt64LE(BigInt(100), 36);
  const asset = Buffer.alloc(165);
  asset.writeBigUInt64LE(dust, 64);
  const receipt = Buffer.alloc(112);
  receipt.writeBigUInt64LE(BigInt(100), 72);
  receipt.writeBigUInt64LE(BigInt(100) << BigInt(48), 80);
  receipt.writeBigUInt64LE(BigInt(1), 96);
  const idle = Buffer.alloc(165);
  AccountLayout.encode(
    {
      mint: VOLTR_ASSET_MINT,
      owner: VOLTR_IDLE_AUTH,
      amount: BigInt(1000),
      delegateOption: 0,
      delegate: owner,
      state: 1,
      isNativeOption: 0,
      isNative: BigInt(0),
      delegatedAmount: BigInt(0),
      closeAuthorityOption: 0,
      closeAuthority: owner,
    },
    idle
  );
  connection.getMultipleAccountsInfoAndContext = async () => ({
    context: { slot: 1 },
    value: [
      info(vault, VOLTR_PROGRAM_ID),
      info(mint),
      hasAta ? info(asset) : null,
      null,
      info(receipt, VOLTR_PROGRAM_ID),
      null,
      info(Buffer.alloc(0)),
      info(idle),
    ],
  });
  connection.getMinimumBalanceForRentExemption = async () => 2_039_280;
  return connection;
}

describe("protected Voltr claim instruction contract (not runtime qualification)", () => {
  test("existing dust cannot permit an under-floor quote", async () => {
    await expect(
      voltrClaimPlan(
        fixture(BigInt(99), BigInt(1_000_000_000)),
        { authority, owner },
        consent
      )
    ).rejects.toMatchObject({ code: "waiting_price" });
  });
  test("explicit expired and mismatched LP approvals cannot prepare a claim", async () => {
    await expect(
      voltrClaimPlan(
        fixture(BigInt(100), BigInt(0)),
        { authority, owner },
        { ...consent, expiresAt: "1" }
      )
    ).rejects.toMatchObject({ code: "expired" });
    await expect(
      voltrClaimPlan(
        fixture(BigInt(100), BigInt(0)),
        { authority, owner },
        { ...consent, lpRaw: "99" }
      )
    ).rejects.toMatchObject({ code: "needs_consent" });
  });
});

test("public claims need no separate consent and pin the payout and receipt", async () => {
  const plan = await voltrClaimPlan(fixture(100n, 7n), { authority, owner });
  expect(plan.claimProtection).toBeUndefined();
  expect(plan.lifecycleFence).toBe(true);
  expect(plan.withdrawalReceiptBytes?.length).toBe(112);
  expect(plan.vault[1].data.readBigUInt64LE(1)).toBe(106n);
  const smallest = await voltrClaimPlan(fixture(1n, 0n), { authority, owner });
  expect(smallest.vault[1].data.readBigUInt64LE(1)).toBe(1n);
});
