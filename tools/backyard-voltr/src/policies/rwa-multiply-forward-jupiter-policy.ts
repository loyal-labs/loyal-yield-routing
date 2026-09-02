import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX,
  resolveCurrentPhaseOneForwardJupiterHeader,
} from "./rwa-multiply-jupiter-headers.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const CATALOG_PATH = resolve(REPOSITORY_ROOT,
  "crates/loyal-actions/fixtures/backyard_rwa_policy_catalog_v1.json");
const COMPILER = "compile-backyard-rwa-resolved-policies";
const LEGACY_SHARED_HEX = "c1209b3341d69c81";

export const FORWARD_JUPITER_POLICY_SEED_BEFORE = 65n;
export const FORWARD_JUPITER_POLICY_SEED = 66n;
export const FORWARD_JUPITER_DATA_LENGTH = 37;
export const FORWARD_JUPITER_AMOUNT_OFFSET = 18;
export const FORWARD_JUPITER_OUT_AMOUNT_OFFSET = 26;
export const FORWARD_JUPITER_SLIPPAGE_OFFSET = 34;
export const FORWARD_JUPITER_FEE_OFFSET = 36;

type SettingsState = Readonly<{
  policySeed: { toString(): string } | null;
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
}>;

const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [SettingsState, number] };
}).Settings;

export type ForwardJupiterPolicyArtifact = Readonly<{
  schema: "loyal-backyard-rwa-resolved-policy-artifact/v1";
  phase: "phase1-forward-jupiter-rollover";
  verdict: "COMPILED_SIGNED_SIMULATION_REQUIRED";
  broadcast: false;
  physicalPolicyCount: 1;
  policySeedBefore: "65";
  catalogSha256: string;
  resolutionSha256: string;
  sourceSha256: string;
  policies: readonly [Readonly<{
    name: "swap/Prime/USDC/PRIME/forward-rollover";
    seed: "66";
    policy: string;
    semanticEdgeCount: 2;
    constraintCount: 2;
    constraints: ReturnType<typeof forwardJupiterConstraints>;
    createPacketBytes: number;
    updatePacketBytes: number;
    createInstruction: WireInstruction;
    updateInstruction: WireInstruction;
  }>];
}>;

type WireInstruction = Readonly<{
  programId: string;
  accounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
  dataBase64: string;
  dataSha256: string;
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

export function forwardJupiterPolicyAddress(): string {
  const seed = Buffer.alloc(8);
  seed.writeBigUInt64LE(FORWARD_JUPITER_POLICY_SEED);
  return PublicKey.findProgramAddressSync([
    Buffer.from("smart_account"), Buffer.from("policy"),
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(), seed,
  ], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0].toBase58();
}

export function forwardJupiterConstraints() {
  const constraint = (routePlanPrefixHex: typeof PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX[number]) => ({
    programId: RWA_MULTIPLY_ROUTE.programs.jupiter,
    accountPubkeys: [
      { index: 0, pubkeys: [RWA_MULTIPLY_ROUTE.assets.tokenProgram] },
      { index: 2, pubkeys: [RWA_MULTIPLY_ROUTE.squads.vault] },
      { index: 3, pubkeys: [RWA_MULTIPLY_ROUTE.squads.assetAta] },
      { index: 6, pubkeys: [RWA_MULTIPLY_ROUTE.squads.collateralAta] },
      { index: 7, pubkeys: [RWA_MULTIPLY_ROUTE.assets.assetMint] },
      { index: 8, pubkeys: [RWA_MULTIPLY_ROUTE.assets.collateralMint] },
    ],
    data: [
      { kind: "slice-equals", offset: 0, valueHex: LEGACY_SHARED_HEX },
      { kind: "slice-equals", offset: 8, valueHex: routePlanPrefixHex },
      { kind: "u64-less-than-or-equal", offset: FORWARD_JUPITER_AMOUNT_OFFSET,
        value: Number(RWA_MULTIPLY_ROUTE.vault.capRaw) },
      { kind: "u16-less-than-or-equal", offset: FORWARD_JUPITER_SLIPPAGE_OFFSET,
        value: RWA_MULTIPLY_ROUTE.assets.maxSlippageBps },
      { kind: "u8-equals", offset: FORWARD_JUPITER_FEE_OFFSET, value: 0 },
    ],
  } as const);
  return [
    constraint(PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX[0]),
    constraint(PHASE_ONE_FORWARD_ROUTE_PREFIX_HEX[1]),
  ] as const;
}

function parseArtifact(value: unknown): ForwardJupiterPolicyArtifact {
  invariant(value !== null && typeof value === "object" && !Array.isArray(value),
    "forward Jupiter compiler returned a non-object");
  const artifact = value as Partial<ForwardJupiterPolicyArtifact>;
  const policy = artifact.policies?.[0];
  const expectedCreateAccounts = [
    { address: RWA_MULTIPLY_ROUTE.squads.settings, signer: false, writable: true },
    { address: RWA_MULTIPLY_ROUTE.setupAdmin, signer: true, writable: true },
    { address: "11111111111111111111111111111111", signer: false, writable: false },
    { address: RWA_MULTIPLY_ROUTE.squads.program, signer: false, writable: false },
    { address: RWA_MULTIPLY_ROUTE.setupAdmin, signer: true, writable: false },
    { address: forwardJupiterPolicyAddress(), signer: false, writable: true },
  ];
  invariant(artifact.schema === "loyal-backyard-rwa-resolved-policy-artifact/v1"
    && artifact.phase === "phase1-forward-jupiter-rollover"
    && artifact.verdict === "COMPILED_SIGNED_SIMULATION_REQUIRED"
    && artifact.broadcast === false
    && artifact.physicalPolicyCount === 1
    && artifact.policySeedBefore === "65"
    && artifact.policies?.length === 1
    && policy?.name === "swap/Prime/USDC/PRIME/forward-rollover"
    && policy.seed === "66"
    && policy.policy === forwardJupiterPolicyAddress()
    && policy.semanticEdgeCount === 2
    && policy.constraintCount === 2
    && JSON.stringify(policy.constraints) === JSON.stringify(forwardJupiterConstraints())
    && policy.createPacketBytes > 0 && policy.createPacketBytes <= 1_232
    && policy.createInstruction.programId === RWA_MULTIPLY_ROUTE.squads.program
    && JSON.stringify(policy.createInstruction.accounts) === JSON.stringify(expectedCreateAccounts)
    && /^[0-9a-f]{64}$/.test(policy.createInstruction.dataSha256),
  "forward Jupiter compiler escaped the exact one-policy boundary");
  return artifact as ForwardJupiterPolicyArtifact;
}

export async function compileCurrentForwardJupiterPolicy(connection: Connection) {
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash,
    "RPC is not mainnet-beta");
  const settingsRead = await connection.getAccountInfoAndContext(
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), { commitment: "finalized" });
  invariant(settingsRead.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program,
    "Squads Settings is absent or has the wrong owner");
  const [settings] = Settings.fromAccountInfo(settingsRead.value);
  invariant(BigInt(settings.policySeed?.toString() ?? "0") === FORWARD_JUPITER_POLICY_SEED_BEFORE
    && settings.threshold === 1 && settings.timeLock === 0 && settings.signers.length === 1
    && settings.signers[0]?.key.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin
    && settings.signers[0]?.permissions.mask === 7,
  "Squads Settings is not the exact seed-65 activation boundary");
  const targetRead = await connection.getAccountInfoAndContext(
    new PublicKey(forwardJupiterPolicyAddress()), {
      commitment: "finalized", minContextSlot: settingsRead.context.slot,
    });
  invariant(targetRead.value === null, "forward Jupiter seed-66 policy already exists");
  const header = await resolveCurrentPhaseOneForwardJupiterHeader(connection);
  const compilerInput = {
    schema: "loyal-backyard-rwa-policy-compiler-input/v1",
    addressesResolved: true,
    swapHeadersResolved: true,
    catalogSha256: sha256(readFileSync(CATALOG_PATH)),
    resolutionSha256: sha256(JSON.stringify(header)),
    settings: RWA_MULTIPLY_ROUTE.squads.settings,
    authority: RWA_MULTIPLY_ROUTE.setupAdmin,
    delegatedSigner: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
    accountIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
    policySeedBefore: FORWARD_JUPITER_POLICY_SEED_BEFORE.toString(),
    policies: [{
      name: "swap/Prime/USDC/PRIME/forward-rollover",
      semanticEdgeCount: 2,
      constraints: forwardJupiterConstraints(),
    }],
  } as const;
  const source = JSON.stringify(compilerInput);
  const result = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin",
    COMPILER, "--", "--phase1-forward-jupiter-rollover"], {
    cwd: REPOSITORY_ROOT, input: source, encoding: "utf8", maxBuffer: 8 * 1024 * 1024,
  });
  invariant(result.status === 0,
    `forward Jupiter policy compiler failed: ${(result.stderr || result.stdout).trim()}`);
  return {
    schema: "loyal-backyard-rwa-forward-jupiter-policy-bindings/v1",
    verdict: "COMPILED_SIGNED_SIMULATION_REQUIRED",
    broadcast: false,
    settingsSlot: settingsRead.context.slot,
    header,
    compilerInput,
    artifact: parseArtifact(JSON.parse(result.stdout)),
  } as const;
}
