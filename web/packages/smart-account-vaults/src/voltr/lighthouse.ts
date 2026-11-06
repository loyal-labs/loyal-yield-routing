import { PublicKey, TransactionInstruction } from "@solana/web3.js";
import { Buffer } from "buffer";

// Lighthouse (github.com/Jac0xb/lighthouse programs/lighthouse/src) is
// immutable on mainnet: its ProgramData has no upgrade authority.
export const LIGHTHOUSE_PROGRAM_ID = new PublicKey(
  "L2TExMFKdjpN9kozasaurPirfHy9P8sbXoAN1qA3S95",
);

const Tag = {
  AssertAccountData: 2,
  AssertAccountDataMulti: 3,
  AssertMintAccount: 7,
  AssertTokenAccount: 9,
  AssertTokenAccountMulti: 10,
  AssertSysvarClock: 15,
} as const;
const SILENT = 0;
const DATA_U64 = 7;
const DATA_BYTES = 11;
const MINT_SUPPLY = 1;
const TOKEN_AMOUNT = 2;
const CLOCK_SLOT = 0;
const CLOCK_UNIX_TIMESTAMP = 4;
const EQUAL = 0;

export const IntegerOperator = {
  equal: 0,
  greaterThanOrEqual: 4,
  lessThanOrEqual: 5,
} as const;
export type IntegerOperator =
  (typeof IntegerOperator)[keyof typeof IntegerOperator];

export type U64Bound = { operator: IntegerOperator; value: bigint };
export type BytePin = { offset: number; bytes: Uint8Array };

function leb128(value: number): number[] {
  const out: number[] = [];
  let rest = value;
  do {
    const byte = rest & 0x7f;
    rest = Math.floor(rest / 128);
    out.push(rest > 0 ? byte | 0x80 : byte);
  } while (rest > 0);
  return out;
}

const MAX_U64 = (BigInt(1) << BigInt(64)) - BigInt(1);

function u64(value: bigint): Buffer {
  if (value < BigInt(0) || value > MAX_U64)
    throw new RangeError("Lighthouse u64 out of range");
  const out = Buffer.alloc(8);
  out.writeBigUInt64LE(value);
  return out;
}

function i64(value: bigint): Buffer {
  const out = Buffer.alloc(8);
  out.writeBigInt64LE(value);
  return out;
}

function assertion(
  data: (number | Buffer | Uint8Array)[],
  target?: PublicKey,
): TransactionInstruction {
  return new TransactionInstruction({
    data: Buffer.concat(
      data.map((part) =>
        typeof part === "number" ? Buffer.of(part) : Buffer.from(part),
      ),
    ),
    keys: target
      ? [{ isSigner: false, isWritable: false, pubkey: target }]
      : [],
    programId: LIGHTHOUSE_PROGRAM_ID,
  });
}

export function assertAccountBytes(
  target: PublicKey,
  pins: readonly BytePin[],
): TransactionInstruction {
  return assertion(
    [
      Tag.AssertAccountDataMulti,
      SILENT,
      Buffer.from(leb128(pins.length)),
      ...pins.flatMap((pin) => [
        Buffer.from(leb128(pin.offset)),
        DATA_BYTES,
        Buffer.from(leb128(pin.bytes.length)),
        pin.bytes,
        EQUAL,
      ]),
    ],
    target,
  );
}

export function assertAccountU64(
  target: PublicKey,
  offset: number,
  bound: U64Bound,
): TransactionInstruction {
  return assertion(
    [
      Tag.AssertAccountData,
      SILENT,
      Buffer.from(leb128(offset)),
      DATA_U64,
      u64(bound.value),
      bound.operator,
    ],
    target,
  );
}

export function assertMintSupply(
  mint: PublicKey,
  bound: U64Bound,
): TransactionInstruction {
  return assertion(
    [
      Tag.AssertMintAccount,
      SILENT,
      MINT_SUPPLY,
      u64(bound.value),
      bound.operator,
    ],
    mint,
  );
}

export function assertTokenAmount(
  account: PublicKey,
  bounds: readonly U64Bound[],
): TransactionInstruction {
  const encoded = bounds.flatMap((bound) => [
    TOKEN_AMOUNT,
    u64(bound.value),
    bound.operator,
  ]);
  return bounds.length === 1
    ? assertion([Tag.AssertTokenAccount, SILENT, ...encoded], account)
    : assertion(
        [
          Tag.AssertTokenAccountMulti,
          SILENT,
          Buffer.from(leb128(bounds.length)),
          ...encoded,
        ],
        account,
      );
}

export function assertClockSlot(bound: U64Bound): TransactionInstruction {
  return assertion([
    Tag.AssertSysvarClock,
    SILENT,
    CLOCK_SLOT,
    u64(bound.value),
    bound.operator,
  ]);
}

export function assertClockUnixTimestamp(
  bound: U64Bound,
): TransactionInstruction {
  return assertion([
    Tag.AssertSysvarClock,
    SILENT,
    CLOCK_UNIX_TIMESTAMP,
    i64(bound.value),
    bound.operator,
  ]);
}
