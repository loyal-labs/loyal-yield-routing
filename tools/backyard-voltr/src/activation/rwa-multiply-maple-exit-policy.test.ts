import { describe, expect, test } from "bun:test";
import { MAPLE_EXIT, validateFreshMapleExitHeader } from "./rwa-multiply-maple-exit-policy.js";

function row(data: Buffer) {
  return {
    instruction: { programId: MAPLE_EXIT.program, dataBase64: data.toString("base64"), accounts: Array.from({ length: 28 }, (_, index) => ({ pubkey: (MAPLE_EXIT.accountPins as Record<number, string>)[index] ?? "11111111111111111111111111111111", isSigner: index === 2 })) },
    header: { dialect: "shared-accounts-route", accountCount: 28 },
  };
}

describe("Maple seed-139 exit header", () => {
  test("accepts only the exact 37-byte SharedAccountsRoute boundary", () => {
    const data = Buffer.alloc(37);
    Buffer.from(MAPLE_EXIT.discriminatorHex, "hex").copy(data, 0);
    data.writeBigUInt64LE(1_000_000n, 18);
    data.writeUInt16LE(50, 34);
    expect(validateFreshMapleExitHeader(row(data)).amountRaw).toBe("1000000");
  });

  test("rejects an amount above the hard cap", () => {
    const data = Buffer.alloc(37);
    Buffer.from(MAPLE_EXIT.discriminatorHex, "hex").copy(data, 0);
    data.writeBigUInt64LE(1_000_001n, 18);
    expect(() => validateFreshMapleExitHeader(row(data))).toThrow();
  });
});
