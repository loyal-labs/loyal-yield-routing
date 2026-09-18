import { createHash } from "node:crypto";
import { PublicKey, TransactionInstruction } from "@solana/web3.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { validateJupiterHeader } from "./rwa-multiply-jupiter-headers.js";

type Json = Record<string, unknown>;
const MAX_OPERATION_RAW = 1_000_000_000_000; // Existing catalogue policy limit, not runtime spend approval.
function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function sha256(value: Uint8Array): string { return createHash("sha256").update(value).digest("hex"); }

// Shared by the existing catalogue compiler and scoped forward-repair probes.
// V2 preserves asset/custody/economic constraints while allowing route length
// variation. Neither this pure function nor its output installs any authority.
export function exactJupiterConstraint(row: Json): Readonly<{ constraint: Json; edge: Json }> {
  const source = row.source as Json;
  const destination = row.destination as Json;
  const header = row.header as Json;
  const indexes = header.indexes as Json;
  const instruction = row.instruction as Json;
  const accounts = instruction.accounts;
  const data = Buffer.from(String(instruction.dataBase64), "base64");
  invariant(row.pass === true && typeof row.key === "string", "Jupiter edge is not an accepted header");
  invariant(typeof source?.symbol === "string" && typeof source.mint === "string"
    && typeof source.tokenProgram === "string" && typeof source.ata === "string"
    && typeof destination?.symbol === "string" && typeof destination.mint === "string"
    && typeof destination.tokenProgram === "string" && typeof destination.ata === "string",
  `${String(row.key)} Jupiter asset boundary is incomplete`);
  invariant(typeof instruction.programId === "string" && instruction.programId === RWA_MULTIPLY_ROUTE.programs.jupiter
    && typeof instruction.dataSha256 === "string" && sha256(data) === instruction.dataSha256
    && Array.isArray(accounts) && data.length >= 28,
  `${String(row.key)} Jupiter instruction is incomplete`);
  const index = (name: string) => {
    const value = indexes?.[name];
    invariant(typeof value === "number" && Number.isSafeInteger(value) && value >= 0,
      `${String(row.key)} ${name} index is invalid`);
    return value;
  };
  const positions = [
    [index("authority"), RWA_MULTIPLY_ROUTE.squads.vault, true, false],
    [index("source"), source.ata, false, true],
    [index("destination"), destination.ata, false, true],
    [index("sourceMint"), source.mint, false, false],
    [index("destinationMint"), destination.mint, false, false],
    [index("sourceProgram"), source.tokenProgram, false, false],
    [index("destinationProgram"), destination.tokenProgram, false, false],
  ] as const;
  const constraints = new Map<number, string>();
  for (const [accountIndex, pubkey, signer, writable] of positions) {
    const account = accounts[accountIndex] as Json | undefined;
    invariant(account?.pubkey === pubkey && account.isSigner === signer && account.isWritable === writable,
      `${String(row.key)} Jupiter account boundary ${accountIndex} drifted`);
    const previous = constraints.get(accountIndex);
    invariant(previous === undefined || previous === pubkey,
      `${String(row.key)} Jupiter account index is assigned conflicting exact keys`);
    constraints.set(accountIndex, pubkey);
  }
  const ix = new TransactionInstruction({
    programId: new PublicKey(String(instruction.programId)), data,
    keys: (accounts as Json[]).map(a => ({pubkey:new PublicKey(String(a.pubkey)),isSigner:a.isSigner===true,isWritable:a.isWritable===true})),
  });
  const quote = row.quote as Json;
  const verified = validateJupiterHeader({instruction:ix,
    sourceMint:String(source.mint), destinationMint:String(destination.mint),
    sourceAta:String(source.ata), destinationAta:String(destination.ata),
    sourceTokenProgram:String(source.tokenProgram), destinationTokenProgram:String(destination.tokenProgram),
    amountRaw:BigInt(String(quote.inAmountRaw)),outAmountRaw:BigInt(String(quote.outAmountRaw)),
  });
  invariant(JSON.stringify(verified.indexes) === JSON.stringify(indexes),
    "Jupiter compiler header differs from independently validated byte layout");
  const v2 = verified.dialect === "shared-accounts-route-v2";
  const slippage = index("slippage");
  const platformFee = index("platformFee");
  invariant(data.readUInt16LE(slippage) <= RWA_MULTIPLY_ROUTE.assets.maxSlippageBps && data[platformFee] === 0,
    `${String(row.key)} Jupiter data boundary drifted`);
  return {
    constraint: {
      programId: instruction.programId,
      accountPubkeys: [...constraints.entries()].sort(([left], [right]) => left - right)
        .map(([accountIndex, pubkey]) => ({ index: accountIndex, pubkeys: [pubkey] })),
      data: [
        { kind: "slice-equals", offset: 0, valueHex: data.subarray(0, 8).toString("hex") },
        { kind: "u64-less-than-or-equal", offset: v2 ? 9 : data.length - 19, value: MAX_OPERATION_RAW },
        { kind: "u16-less-than-or-equal", offset: slippage, value: RWA_MULTIPLY_ROUTE.assets.maxSlippageBps },
        ...(v2 ? [{ kind: "slice-equals", offset: 27, valueHex: "00000000" }] : [{ kind: "u8-equals", offset: platformFee, value: 0 }]),
      ],
    },
    edge: {
      from: source.symbol, to: destination.symbol, constraintIndex: 0,
      authorityIndex: index("authority"), sourceIndex: index("source"), destinationIndex: index("destination"),
      sourceMintIndex: index("sourceMint"), destinationMintIndex: index("destinationMint"),
      sourceTokenProgramIndex: index("sourceProgram"), destinationTokenProgramIndex: index("destinationProgram"),
      authority: RWA_MULTIPLY_ROUTE.squads.vault,
      sourceCustody: source.ata, destinationCustody: destination.ata,
      sourceMint: source.mint, destinationMint: destination.mint,
      sourceTokenProgram: source.tokenProgram, destinationTokenProgram: destination.tokenProgram,
    },
  };
}
