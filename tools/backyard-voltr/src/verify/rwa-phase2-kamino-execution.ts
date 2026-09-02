/** Exact fail-closed Squads wrapper for one compiled Phase-2 K-Lend operation. */
import { createHash } from "node:crypto";

import { executePolicyPayloadSync } from "@loyal-labs/loyal-smart-accounts-core/internal";
import { PublicKey, TransactionInstruction } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

type Json = Record<string, unknown>;
export type ExactKaminoSquadsExecution = Readonly<{
  policy: string;
  policySeed: string;
  operation: string;
  innerInstruction: TransactionInstruction;
  outerInstruction: TransactionInstruction;
  compiledPayloadSha256: string;
}>;

function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function array(value: unknown, label: string): unknown[] { invariant(Array.isArray(value), `${label} is not an array`); return value; }
function text(value: unknown, label: string): string { invariant(typeof value === "string" && value.length > 0, `${label} is missing`); return value; }
function integer(value: unknown, label: string): number { invariant(typeof value === "number" && Number.isSafeInteger(value) && value >= 0, `${label} is not a non-negative integer`); return value; }
function sha256(value: Uint8Array): string { return createHash("sha256").update(value).digest("hex"); }

function policyAddress(seed: bigint): string {
  const bytes = Buffer.alloc(8); bytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync([Buffer.from("smart_account"), Buffer.from("policy"), new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(), bytes], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0].toBase58();
}

function compileInner(instruction: TransactionInstruction) {
  const accounts: Array<{ pubkey: PublicKey; isWritable: boolean; isSigner: false }> = [];
  const indexOf = (pubkey: PublicKey, writable: boolean): number => {
    const previous = accounts.findIndex((account) => account.pubkey.equals(pubkey));
    if (previous >= 0) { const account = accounts[previous]; invariant(account, "inner account disappeared"); account.isWritable ||= writable; return previous; }
    invariant(accounts.length < 255, "inner account table exceeds Squads u8 limit");
    accounts.push({ pubkey, isWritable: writable, isSigner: false }); return accounts.length - 1;
  };
  invariant(instruction.keys.every((key) => !key.isSigner || key.pubkey.toBase58() === RWA_MULTIPLY_ROUTE.squads.vault), "inner K-Lend instruction has a signer other than the Squads vault");
  const indexes = instruction.keys.map((key) => indexOf(key.pubkey, key.isWritable));
  invariant(instruction.data.length <= 65_535, "inner K-Lend data exceeds Squads u16 limit");
  const length = Buffer.alloc(2); length.writeUInt16LE(instruction.data.length);
  return { accounts, bytes: Buffer.concat([Buffer.from([1, indexOf(instruction.programId, false), indexes.length, ...indexes]), length, instruction.data]) } as const;
}

function matchesConstraint(value: unknown, instruction: TransactionInstruction): boolean {
  try {
    const constraint = object(value, "K-Lend constraint");
    if (text(constraint.programId, "K-Lend constraint program") !== instruction.programId.toBase58()) return false;
    const accountConstraints = array(constraint.accountPubkeys, "K-Lend account constraints");
    for (let index = 0; index < instruction.keys.length; index += 1) {
      const key = instruction.keys[index]!;
      const match = accountConstraints.find((entry) => {
        const row = object(entry, "K-Lend account constraint");
        const pubkeys = array(row.pubkeys, "K-Lend constraint pubkeys");
        return integer(row.index, "K-Lend constraint account index") === index && pubkeys.length === 1 && text(pubkeys[0], "K-Lend constraint pubkey") === key.pubkey.toBase58();
      });
      if (!match) return false;
    }
    const dataConstraints = array(constraint.data, "K-Lend data constraints");
    for (const value of dataConstraints) {
      const row = object(value, "K-Lend data constraint"); const offset = integer(row.offset, "K-Lend data constraint offset"); const kind = text(row.kind, "K-Lend data constraint kind");
      if (kind === "slice-equals") { const expected = Buffer.from(text(row.valueHex, "K-Lend slice value"), "hex"); if (expected.length === 0 || !instruction.data.subarray(offset, offset + expected.length).equals(expected)) return false; }
      else if (kind === "u64-less-than-or-equal") { if (offset + 8 > instruction.data.length || instruction.data.readBigUInt64LE(offset) > BigInt(integer(row.value, "K-Lend u64 cap"))) return false; }
      else return false;
    }
    return true;
  } catch { return false; }
}

export function buildExactKaminoSquadsExecution(input: Readonly<{
  compiledPolicy: unknown;
  operation: string;
  innerInstruction: TransactionInstruction;
  delegatedSigner: PublicKey;
}>): ExactKaminoSquadsExecution {
  const policy = object(input.compiledPolicy, "compiled K-Lend policy");
  const operations = array(policy.operations, "compiled K-Lend operations");
  invariant(operations.length === 1 && operations[0] === input.operation, `compiled K-Lend policy is not exact ${input.operation}`);
  const policySeed = text(policy.seed, "compiled K-Lend seed");
  invariant(/^[1-9][0-9]*$/.test(policySeed), "compiled K-Lend seed is malformed");
  const policyText = text(policy.policy, "compiled K-Lend policy PDA");
  invariant(policyAddress(BigInt(policySeed)) === policyText, "compiled K-Lend policy PDA does not derive from Settings and seed");
  const constraints = array(policy.constraints, "compiled K-Lend constraints");
  invariant(constraints.length === 1 && constraints.length === integer(policy.constraintCount, "compiled K-Lend constraint count"), "compiled K-Lend policy is not a one-operation physical policy");
  invariant(matchesConstraint(constraints[0], input.innerInstruction), "compiled K-Lend constraint does not exactly match the canonical operation");
  const compiled = compileInner(input.innerInstruction);
  const outerInstruction = executePolicyPayloadSync({
    feePayer: input.delegatedSigner, policy: new PublicKey(policyText), accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex, numSigners: 1,
    policyPayload: { __kind: "ProgramInteraction", fields: [{ instructionConstraintIndices: new Uint8Array([0]), transactionPayload: { __kind: "SyncTransaction", fields: [{ accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex, instructions: compiled.bytes }] } }] },
    instruction_accounts: [{ pubkey: input.delegatedSigner, isSigner: true, isWritable: false }, ...compiled.accounts], programId: new PublicKey(RWA_MULTIPLY_ROUTE.squads.program),
  });
  return { policy: policyText, policySeed, operation: input.operation, innerInstruction: input.innerInstruction, outerInstruction, compiledPayloadSha256: sha256(compiled.bytes) };
}
