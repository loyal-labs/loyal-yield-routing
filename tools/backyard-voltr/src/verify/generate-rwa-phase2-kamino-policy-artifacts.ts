/**
 * Compile only the resolved K-Lend half of Phase 2.
 *
 * Jupiter remains intentionally absent until all 52 exact current headers are
 * available.  The compiler signs every attempted PolicyCreate rung with the
 * approved Settings authority, but against a confirmed blockhash at least 512
 * slots old.  The resulting wire proves real signing and packet size while
 * being expired before it is persisted; this command never sends a packet.
 */
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, PublicKey, type AccountInfo } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  buildPhaseTwoKaminoLaneOperations,
  resolutionLanes,
  type ResolvedLane,
} from "../policies/rwa-multiply-phase2-kamino.js";

type Json = Record<string, unknown>;
type SettingsState = Readonly<{
  policySeed: { toString(): string } | null;
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{
    key: PublicKey;
    permissions: Readonly<{ mask: number }>;
  }>[];
}>;
type Artifact = Readonly<{
  schema: string;
  phase: string;
  verdict: string;
  broadcast: boolean;
  packing: Readonly<{
    selectedRung: string;
    attemptedRungs: readonly unknown[];
    activationPrefix?: readonly string[];
    comparativeCandidates?: readonly Json[];
    exactSwapPackingProof?: Json;
  }>;
  packetMeasurements: readonly Json[];
}>;

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const RESOLUTION_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const KAMINO_PROBE_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-kamino-family-probes-v1.json");
const CATALOG_PATH = resolve(ROOT, "crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json");
const COMPILED_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json");
const PACKETS_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-packets-v1.json");
const JUPITER_HEADERS_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json");
const COMPILER = "compile-backyard-rwa-resolved-policies";
const MAX_OPERATION_RAW = 1_000_000_000_000;
const EXPIRED_BLOCKHASH_GAP = 512;

const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: AccountInfo<Buffer>): readonly [SettingsState, number] };
}).Settings;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function readJson(path: string): Json {
  return JSON.parse(readFileSync(path, "utf8")) as Json;
}

function exactKaminoConstraint(operation: Json): Json {
  const data = Buffer.from(String(operation.dataBase64), "base64");
  const accounts = operation.accounts;
  invariant(typeof operation.operation === "string", "Kamino operation name is missing");
  invariant(typeof operation.programId === "string" && operation.programId === RWA_MULTIPLY_ROUTE.kamino.program,
    "Kamino program boundary drifted");
  invariant(data.length === 16 && typeof operation.dataSha256 === "string" && sha256(data) === operation.dataSha256,
    "Kamino operation data is not exact");
  invariant(Array.isArray(accounts) && accounts.length === operation.accountCount,
    "Kamino operation account count drifted");
  return {
    operation: operation.operation,
    programId: operation.programId,
    accountPubkeys: accounts.map((account, index) => {
      const row = account as Json;
      invariant(typeof row.address === "string", `Kamino account ${index} is missing`);
      return { index, pubkeys: [row.address] };
    }),
    data: [
      { kind: "slice-equals", offset: 0, valueHex: data.subarray(0, 8).toString("hex") },
      { kind: "u64-less-than-or-equal", offset: 8, value: MAX_OPERATION_RAW },
    ],
  };
}

function expectedLanes(value: unknown): ResolvedLane[] {
  const root = value as Json;
  invariant(root.schema === "loyal-backyard-rwa-policy-resolution/v1"
    && root.commitment === "confirmed" && root.laneGraphExact === true
    && Array.isArray(root.lanes) && root.lanes.length === 11,
  "confirmed 11-lane resolution is incomplete");
  return resolutionLanes(root);
}

function expectedSwapEdgeKeys(): string[] {
  const stable = ["USDC", "USDG", "USDS", "PYUSD"];
  const rwa = ["ONyc", "PRIME", "syrupUSDC", "AUTO", "USDe"];
  return [
    ...stable.flatMap((from) => rwa.map((to) => `${from}->${to}`)),
    ...rwa.flatMap((from) => stable.map((to) => `${from}->${to}`)),
    ...stable.flatMap((from) => stable.filter((to) => to !== from).map((to) => `${from}->${to}`)),
  ];
}

function swapSlice(from: string, to: string): string {
  const stable = ["USDC", "USDG", "USDS", "PYUSD"];
  const rwa = ["ONyc", "PRIME", "syrupUSDC", "AUTO", "USDe"];
  if (stable.includes(from) && rwa.includes(to)) return "stable-to-rwa";
  if (rwa.includes(from) && stable.includes(to)) return "rwa-to-stable";
  if (stable.includes(from) && stable.includes(to) && from !== to) return "stable-to-stable";
  throw new Error(`unsupported exact Jupiter edge ${from}->${to}`);
}

function exactJupiterConstraint(row: Json): Readonly<{ constraint: Json; edge: Json }> {
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
        { kind: "u64-less-than-or-equal", offset: data.length - 19, value: MAX_OPERATION_RAW },
        { kind: "u16-less-than-or-equal", offset: slippage, value: RWA_MULTIPLY_ROUTE.assets.maxSlippageBps },
        { kind: "u8-equals", offset: platformFee, value: 0 },
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

async function main() {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
  const resolutionBytes = readFileSync(RESOLUTION_PATH);
  const resolution = readJson(RESOLUTION_PATH);
  const headers = readJson(JUPITER_HEADERS_PATH);
  const probe = readJson(KAMINO_PROBE_PATH);
  invariant(probe.schema === "loyal-backyard-rwa-kamino-family-probes/v1"
    && probe.verdict === "PASS_KAMINO_FAMILIES_PROBED" && probe.broadcast === false
    && Array.isArray(probe.allLaneOperations) && probe.allLaneOperations.length === 11,
  "all-lane Kamino compiler probe evidence is absent or incomplete");
  const lanes = expectedLanes(resolution);
  invariant(headers.schema === "loyal-backyard-rwa-jupiter-header-evidence/v2"
    && headers.verdict === "PASS_HEADERS_RESOLVED" && headers.broadcast === false
    && headers.requestedEdgeCount === 52 && headers.passCount === 52 && Array.isArray(headers.rows),
  "exact v2 Jupiter header evidence is incomplete");
  const headerRows = headers.rows as Json[];
  const expectedEdges = expectedSwapEdgeKeys();
  invariant(headerRows.length === expectedEdges.length
    && new Set(headerRows.map((row) => String(row.key))).size === expectedEdges.length
    && headerRows.every((row) => expectedEdges.includes(String(row.key))),
  "Jupiter header evidence does not have the exact 52-edge bijection");
  const swapPolicies = headerRows.map(exactJupiterConstraint);
  const allOperations = lanes.map((lane) => ({
    key: lane.key,
    operations: buildPhaseTwoKaminoLaneOperations(lane),
  }));
  invariant(allOperations.every(({ operations }) => operations.length === 4),
    "every resolved lane must carry all four exact K-Lend operations");

  const connection = new Connection(rpcUrl, "confirmed");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash,
    "RPC is not mainnet-beta");
  const settingsRead = await connection.getAccountInfoAndContext(
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), { commitment: "confirmed" });
  invariant(settingsRead.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program,
    "Squads Settings is absent or has the wrong owner");
  const [settings] = Settings.fromAccountInfo(settingsRead.value);
  invariant(settings.threshold === 1 && settings.timeLock === 0 && settings.signers.length === 1
    && settings.signers[0]?.key.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin
    && settings.signers[0]?.permissions.mask === 7,
  "Squads Settings signing boundary drifted");
  const policySeedBefore = BigInt(settings.policySeed?.toString() ?? "0");
  const measurementBlockhashSlot = settingsRead.context.slot - EXPIRED_BLOCKHASH_GAP;
  invariant(measurementBlockhashSlot > 0, "Settings slot is too early for an expired measurement blockhash");
  const block = await connection.getBlock(measurementBlockhashSlot, {
    commitment: "confirmed", maxSupportedTransactionVersion: 0,
  });
  invariant(block?.blockhash, "confirmed expired measurement blockhash is unavailable");

  const compilerInput = {
    schema: "loyal-backyard-rwa-policy-compiler-input/v1",
    addressesResolved: true,
    swapHeadersResolved: true,
    catalogSha256: sha256(readFileSync(CATALOG_PATH)),
    resolutionSha256: sha256(resolutionBytes),
    kaminoProbeSha256: sha256(readFileSync(KAMINO_PROBE_PATH)),
    jupiterHeadersSha256: sha256(readFileSync(JUPITER_HEADERS_PATH)),
    settings: RWA_MULTIPLY_ROUTE.squads.settings,
    authority: RWA_MULTIPLY_ROUTE.setupAdmin,
    delegatedSigner: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
    accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
    policySeedBefore: policySeedBefore.toString(),
    settingsContextSlot: settingsRead.context.slot,
    settingsDataSha256: sha256(settingsRead.value.data),
    measurementBlockhash: block.blockhash,
    measurementBlockhashSlot,
    policies: ([...allOperations.map(({ key, operations }) => ({
      name: `lane/${key}`,
      semanticEdgeCount: 4,
      constraints: operations.map((operation) => exactKaminoConstraint(operation as unknown as Json)),
      swapEdges: [],
    })), ...swapPolicies.map(({ constraint, edge }) => ({
      name: `swap/${swapSlice(String((edge as Json).from), String((edge as Json).to))}/${String((edge as Json).from)}->${String((edge as Json).to)}`,
      semanticEdgeCount: 1,
      constraints: [constraint],
      swapEdges: [edge],
    }))] as Json[]),
  };
  const compilation = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin", COMPILER], {
    cwd: ROOT,
    input: JSON.stringify(compilerInput),
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
  });
  invariant(compilation.status === 0,
    `Phase-2 Kamino policy compiler failed: ${(compilation.stderr || compilation.stdout).trim()}`);
  const artifact = JSON.parse(compilation.stdout) as Artifact;
  invariant(artifact.schema === "loyal-backyard-rwa-resolved-policy-artifact/v1"
    && artifact.phase === "phase2" && artifact.verdict === "COMPILED_SIGNED_SIMULATION_REQUIRED"
    && artifact.broadcast === false && artifact.packing.attemptedRungs.length > 0
    && artifact.packetMeasurements.length > 0,
  "Phase-2 compiler did not emit complete 52-edge signed packet evidence");
  const compiledBytes = Buffer.from(`${JSON.stringify(artifact, null, 2)}\n`);
  const packetEvidence = {
    schema: "loyal-backyard-rwa-policy-packet-evidence/v1",
    verdict: "SIGNED_PACKETS_SIMULATION_REQUIRED",
    broadcast: false,
    signed: true,
    cryptographicSignaturesVerified: true,
    measurementBlockhash: {
      value: block.blockhash,
      slot: measurementBlockhashSlot,
      settingsContextSlot: settingsRead.context.slot,
      minimumExpiredGapSlots: EXPIRED_BLOCKHASH_GAP,
    },
    compiledArtifactSha256: sha256(compiledBytes),
    selectedRung: artifact.packing.selectedRung,
    packing: {
      activationPrefix: artifact.packing.activationPrefix ?? [],
      comparativeCandidates: artifact.packing.comparativeCandidates ?? [],
      exactSwapPackingProof: artifact.packing.exactSwapPackingProof ?? null,
    },
    measurements: artifact.packetMeasurements,
    swap: {
      status: "COMPILED_52_EDGES",
      requiredEdgeCount: 52,
      resolvedEdgeCount: 52,
    },
  };
  writeFileSync(COMPILED_PATH, compiledBytes, { flag: "w", mode: 0o600 });
  writeFileSync(PACKETS_PATH, `${JSON.stringify(packetEvidence, null, 2)}\n`, { flag: "w", mode: 0o600 });
  console.log(JSON.stringify({
    compiled: COMPILED_PATH,
    packets: PACKETS_PATH,
    verdict: artifact.verdict,
    selectedRung: artifact.packing.selectedRung,
    attemptedRungs: artifact.packing.attemptedRungs.length,
    signedCreateMeasurements: artifact.packetMeasurements.length,
    policySeedBefore: policySeedBefore.toString(),
    settingsContextSlot: settingsRead.context.slot,
    measurementBlockhashSlot,
  }));
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
});
