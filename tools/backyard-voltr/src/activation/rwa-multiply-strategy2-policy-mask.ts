/**
 * Offline byte-mask derivation for the four strategy-two Squads bridge
 * policies (seeds 145-148).
 *
 * The worker pins bridge policy bytes by a *masked* digest: the bytes that the
 * program itself mutates during normal operation - each embedded spending
 * limit's `usage.remainingInPeriod`, `usage.lastReset`, and
 * `timeConstraints.start` - are zeroed before hashing, so a charged or
 * re-windowed limit no longer fails the pin. Everything else stays pinned.
 *
 * This script computes that mask once, offline, instead of asking the worker
 * to decode policy bytes in the send path:
 *   1. fetch the finalized account,
 *   2. decode it with the generated Squads decoder,
 *   3. re-encode it with the generated beet and require a byte-exact
 *      round-trip (any mismatch is a hard stop - the SDK layout has drifted),
 *   4. re-encode copies with each volatile field replaced by a distinct
 *      sentinel and diff against the raw bytes; the union of differing bytes
 *      is the mask,
 *   5. report `maskedByteRanges`, `normalizedDigest` (raw bytes with the mask
 *      zeroed) and `rawDigest`.
 *
 * Read-only: one getMultipleAccounts call per run plus a genesis hash check.
 * Run with SOLANA_RPC_URL set (op run --env-file=.env.1password).
 */
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { Connection, PublicKey } from "@solana/web3.js";
import BN from "bn.js";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";

type PolicyLike = {
  seed: { toString(): string };
  policyState: { __kind: string; fields?: readonly unknown[] };
};

const Policy = (squadsGenerated as unknown as {
  Policy: { fromAccountInfo(info: { data: Buffer; owner: PublicKey; lamports: number; executable: boolean; rentEpoch: number }): readonly [PolicyLike, number] };
}).Policy;
const PolicyBeet = (squadsGenerated as unknown as {
  policyBeet: { toFixedFromValue(value: unknown): { serialize(instance: unknown, byteSize: number): readonly [Buffer, number] } };
}).policyBeet;
const policyDiscriminator = (squadsGenerated as unknown as { policyDiscriminator: readonly number[] }).policyDiscriminator;

/**
 * Two fixed pattern values per target field. Any original byte equals at most
 * one of 0x55 / 0xAA, so every byte of the field differs from the original in
 * at least one of the two encodings; the mask is the union of both diffs.
 */
const MASK_PATTERNS = [new BN("5555555555555555", 16), new BN("aaaaaaaaaaaaaaaa", 16)] as const;

type BridgeBinding = { action: string; seed: number; account: string; normalizedDigest: string; dataSha256Raw: string };

function manifestBindings(): BridgeBinding[] {
  const path = new URL("../../../../go/backyard-rwa-worker/internal/backyardrwa/manifest/backyard-rwa-v2.json", import.meta.url);
  const manifest = JSON.parse(readFileSync(path, "utf8")) as { genesisHash: string; runtimeBindings: { bridgePolicies: BridgeBinding[] } };
  const bindings = manifest.runtimeBindings.bridgePolicies;
  if (!Array.isArray(bindings) || bindings.length !== 4) throw new Error(`expected 4 bridge policy bindings, found ${bindings?.length}`);
  return bindings;
}

/**
 * Copy of the decoded account value with exactly one volatile field of one
 * spending limit replaced by `pattern`. Only the objects on that path are
 * copied (as plain objects); every other value, including the leaves the
 * beet serializer consumes in place, is shared with the original so the
 * mutation can never leak into the baseline encode.
 */
function withVolatileField(decoded: unknown, limitIndex: number, field: VolatileField, pattern: BN): Record<string, unknown> {
  const outer = decoded as Record<string, unknown>;
  const policyState = outer.policyState as Record<string, unknown>;
  const body = (policyState.fields as unknown[])[0] as Record<string, unknown>;
  const limits = [...(body.spendingLimits as Record<string, unknown>[])];
  const limit = { ...(limits[limitIndex] as Record<string, unknown>) };
  field.apply(limit, pattern);
  limits[limitIndex] = limit;
  return { ...outer, policyState: { ...policyState, fields: [{ ...body, spendingLimits: limits }] } };
}

/**
 * Serializes with the generated beet. `size` is only the writer capacity (the
 * on-chain account length, which includes trailing rent padding); the struct
 * itself is shorter, so the returned buffer is the modeled prefix.
 */
function encode(value: unknown, size: number): Buffer {
  const [buffer, offset] = PolicyBeet.toFixedFromValue(value).serialize(value, size);
  if (offset > size) throw new Error(`beet wrote ${offset} bytes into a ${size}-byte account`);
  return buffer.subarray(0, offset);
}

function sha256(data: Buffer): string {
  return createHash("sha256").update(data).digest("hex");
}

/** Merges sorted, differing byte indices into half-open [start, end) ranges. */
function mergeRanges(indices: number[]): Array<[number, number]> {
  const ranges: Array<[number, number]> = [];
  for (const index of [...indices].sort((a, b) => a - b)) {
    const last = ranges[ranges.length - 1];
    if (last && last[1] >= index) last[1] = Math.max(last[1], index + 1);
    else ranges.push([index, index + 1]);
  }
  return ranges;
}

function differingIndices(raw: Buffer, mutated: Buffer): number[] {
  if (mutated.length !== raw.length) throw new Error(`sentinel encode changed the account size: ${raw.length} -> ${mutated.length}`);
  const indices: number[] = [];
  for (let offset = 0; offset < raw.length; offset += 1) if (raw[offset] !== mutated[offset]) indices.push(offset);
  return indices;
}

type VolatileField = { name: string; apply(limit: Record<string, unknown>, pattern: BN): void };

const volatileFields: readonly VolatileField[] = [
  {
    name: "usage.remainingInPeriod",
    apply: (limit, pattern) => {
      limit.usage = { ...(limit.usage as Record<string, unknown>), remainingInPeriod: pattern };
    },
  },
  {
    name: "usage.lastReset",
    apply: (limit, pattern) => {
      limit.usage = { ...(limit.usage as Record<string, unknown>), lastReset: pattern };
    },
  },
  {
    name: "timeConstraints.start",
    apply: (limit, pattern) => {
      limit.timeConstraints = { ...(limit.timeConstraints as Record<string, unknown>), start: pattern };
    },
  },
];

function spendingLimits(decoded: PolicyLike, seed: number): Record<string, unknown>[] {
  if (decoded.policyState.__kind !== "ProgramInteraction") {
    throw new Error(`policy ${seed} policyState is ${decoded.policyState.__kind}, want ProgramInteraction`);
  }
  const body = decoded.policyState.fields?.[0] as { spendingLimits?: Record<string, unknown>[] } | undefined;
  if (!body || !Array.isArray(body.spendingLimits)) throw new Error(`policy ${seed} has no ProgramInteraction spendingLimits body`);
  return body.spendingLimits;
}

async function main(): Promise<void> {
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
  const bindings = manifestBindings();
  const connection = new Connection(rpcUrl, { commitment: "finalized" });

  const genesisHash = await connection.getGenesisHash();
  if (genesisHash !== "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d") {
    throw new Error(`RPC is not mainnet-beta (genesis ${genesisHash})`);
  }

  const accounts = await connection.getMultipleAccountsInfo(bindings.map((binding) => new PublicKey(binding.account)), "finalized");
  const report: Array<Record<string, unknown>> = [];
  for (const [index, binding] of bindings.entries()) {
    const info = accounts[index];
    if (!info) throw new Error(`policy ${binding.seed} account ${binding.account} is absent`);
    if (!info.data.subarray(0, policyDiscriminator.length).equals(Buffer.from(policyDiscriminator))) {
      throw new Error(`policy ${binding.seed} discriminator does not match the generated Policy discriminator`);
    }
    const raw = Buffer.from(info.data);
    const [decoded] = Policy.fromAccountInfo({ data: raw, owner: info.owner, lamports: info.lamports, executable: info.executable, rentEpoch: info.rentEpoch });

    // The SDK layout must reproduce the on-chain bytes exactly, or every mask
    // derived below would be a guess instead of a fact. The live accounts are
    // created with extra zero-filled rent padding after the modeled struct, so
    // the round-trip is: serialized struct == raw prefix, and the raw suffix
    // is all zeros.
    const struct = encode({ ...decoded, accountDiscriminator: [...policyDiscriminator] }, raw.length);
    const padding = raw.subarray(struct.length);
    if (!raw.subarray(0, struct.length).equals(struct)) {
      throw new Error(`policy ${binding.seed} decode/re-encode does not round-trip the modeled ${struct.length}-byte prefix`);
    }
    if (padding.some((byte) => byte !== 0)) {
      throw new Error(`policy ${binding.seed} has ${padding.length} unmodeled trailing bytes that are not zero padding`);
    }

    // Diff the sentinels against the modeled prefix; its offsets are raw
    // account offsets because the struct is the prefix.
    const modeled = raw.subarray(0, struct.length);

    const indices: number[] = [];
    const limits = spendingLimits(decoded, binding.seed);
    for (let limitIndex = 0; limitIndex < limits.length; limitIndex += 1) {
      for (const field of volatileFields) {
        const fieldIndices: number[] = [];
        for (const pattern of MASK_PATTERNS) {
          const mutated = withVolatileField(decoded, limitIndex, field, pattern);
          fieldIndices.push(...differingIndices(modeled, encode({ ...mutated, accountDiscriminator: [...policyDiscriminator] }, modeled.length)));
        }
        // One volatile field must resolve to exactly its own 8 bytes: a
        // wider or split union would mean the encode leaked into a
        // neighbouring field, so the mask would be a guess.
        const fieldRange = mergeRanges(fieldIndices);
        if (fieldRange.length !== 1 || fieldRange[0]![1] - fieldRange[0]![0] !== 8) {
          throw new Error(`policy ${binding.seed} limit ${limitIndex} ${field.name} mask is ${JSON.stringify(fieldRange)}, want one 8-byte range`);
        }
        indices.push(...fieldIndices);
      }
    }
    const maskedByteRanges = mergeRanges(indices);
    const maskedTotal = maskedByteRanges.reduce((sum, [start, end]) => sum + (end - start), 0);
    if (maskedTotal > 64) throw new Error(`policy ${binding.seed} mask covers ${maskedTotal} bytes, worker caps at 64`);

    const masked = Buffer.from(raw);
    for (const [start, end] of maskedByteRanges) masked.fill(0, start, end);
    const normalizedDigest = sha256(masked);
    const rawDigest = sha256(raw);
    // The raw digest drifts as the program charges the limit; the masked
    // normalized digest is the pin and must reproduce exactly.
    if (normalizedDigest !== binding.normalizedDigest) {
      throw new Error(`policy ${binding.seed} normalized digest ${normalizedDigest} does not match the manifest pin ${binding.normalizedDigest}`);
    }
    report.push({
      policy: binding.action,
      seed: binding.seed,
      account: binding.account,
      dataSize: raw.length,
      spendingLimitCount: limits.length,
      maskedByteRanges,
      maskedByteTotal: maskedTotal,
      normalizedDigest,
      rawDigest,
    });
  }
  console.log(JSON.stringify({ schema: "loyal-strategy2-policy-mask/v1", commitment: "finalized", policies: report }, null, 2));
}

main().catch((error: unknown) => {
  console.error(`[policy-mask] ${(error as Error).stack ?? (error as Error).message}`);
  process.exit(1);
});
