import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { createNoopSigner } from "@solana/kit";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, PublicKey } from "@solana/web3.js";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerBuilderRoute,
  partnerStrategyIdentity,
  type PartnerStrategyId,
  routeSpecSha256,
} from "../domain/route-spec.js";
import { loadMainReserveGraph } from "../integrations/solana-compat.js";
import {
  createVoltrRouteBuilder,
  deriveVoltrAccounts,
  type CanonicalInstruction,
} from "../integrations/voltr.js";
import { verifyNonCatalogSquadsPoliciesIsolated } from "../verify/squads.js";

const COMPILER_BIN = "compile-voltr-kamino-runtime-policy";
const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const SQUADS_POLICY_SEED = "policy";
const SQUADS_SMART_ACCOUNT_SEED = "smart_account";
const TRAILING_DATA_CONSTRAINT_LIMITATION = "Squads ProgramInteraction has no instruction-data length comparator: the policy pins the canonical bytes through offset 29 but cannot itself reject appended trailing bytes; exact 30-byte length remains a local canonical-builder and pre-send-verifier invariant";

type DecodedSettings = Readonly<{
  policySeed: { toString(): string } | null;
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{
    key: PublicKey;
    permissions: Readonly<{ mask: number }>;
  }>[];
}>;

const SettingsAccount = (squadsGenerated as unknown as {
  Settings: {
    fromAccountInfo(
      account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>,
    ): readonly [DecodedSettings, number];
  };
}).Settings;

export type RuntimePolicyManifest = Readonly<{
  schemaVersion: 1;
  routeId: string;
  routeSpecSha256: string;
  strategyId?: PartnerStrategyId;
  cluster: "mainnet-beta";
  genesisHash: string;
  ids: Readonly<{
    squadsSettings: string;
    manager: string;
    guardian: string;
    admin: string;
    vault: string;
    reserve: string;
    lendingMarket: string;
    collateralFarm: string;
  }>;
  vaultIndex: number;
  limits: Readonly<{
    maxPerOperationRaw: string;
    solanaPacketBytes: 1232;
  }>;
  policySeeds: Readonly<{
    policySeedBefore: string;
    deposit: string;
    withdraw: string;
  }>;
  instructions: Readonly<{
    deposit: CompilerInstruction;
    withdraw: CompilerInstruction;
  }>;
}>;

type CompilerInstruction = Readonly<{
  programId: string;
  dataHex: string;
  dataBase64: string;
  dataSha256: string;
  dataLength: number;
  accounts: readonly Readonly<{
    index: number;
    label: string;
    address: string;
    signer: boolean;
    writable: boolean;
  }>[];
}>;

export type RuntimePolicyArtifact = Readonly<{
  schemaVersion: 1;
  evidenceType: "backyard-voltr-runtime-policy-artifact";
  verdict: "RUNTIME_POLICY_ARTIFACT_COMPILED_AND_VERIFIED";
  broadcast: false;
  routeId: string;
  routeSpecSha256: string;
  sourceManifestSha256: string;
  runtimePolicyCount: 2 | 8;
  setupPolicyIncluded: false;
  trailingDataConstraintLimitation?: string;
  manager: string;
  policySeedBefore: string;
  policies: readonly RuntimePolicyArtifactEntry[];
  sourceManifest: RuntimePolicyManifest;
  sourceManifests?: readonly RuntimePolicyManifest[];
  artifactSha256: string;
}>;

export type RuntimePolicyArtifactEntry = Readonly<{
  strategyId?: PartnerStrategyId;
  operation: "deposit" | "withdraw";
  seed: string;
  policy: string;
  constrainedAccountIndexes: readonly number[];
  innerInstructionDataSha256: string;
  policyCreate: Readonly<{
    programId: string;
    accounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
    dataLength: number;
    dataBase64: string;
    dataSha256: string;
  }>;
  policyCreatePacketBytes: number;
  managerExecution: Readonly<{
    programId: string;
    accounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
    dataLength: number;
    dataBase64: string;
    dataSha256: string;
  }>;
}>;


function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required for live policy compilation");
  return value;
}

function policyAddress(seed: bigint): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [
      Buffer.from(SQUADS_SMART_ACCOUNT_SEED),
      Buffer.from(SQUADS_POLICY_SEED),
      new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(),
      seedBytes,
    ],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}

function compilerInstruction(instruction: CanonicalInstruction): CompilerInstruction {
  return {
    programId: instruction.programId,
    dataHex: Buffer.from(instruction.data).toString("hex"),
    dataBase64: instruction.dataBase64,
    dataSha256: instruction.dataSha256,
    dataLength: instruction.dataLength,
    accounts: instruction.accounts,
  };
}

function compilerArgs(...args: string[]): string[] {
  return [
    "run",
    "--quiet",
    "-p",
    "loyal-actions",
    "--bin",
    COMPILER_BIN,
    "--",
    ...args,
  ];
}

function runCompiler(args: readonly string[], input?: string): unknown {
  const result = spawnSync("cargo", compilerArgs(...args), {
    cwd: REPOSITORY_ROOT,
    encoding: "utf8",
    input,
    maxBuffer: 16 * 1024 * 1024,
    env: process.env,
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    const detail = result.stderr.trim() || result.stdout.trim() || `exit ${result.status}`;
    throw new Error(`runtime policy compiler refused input: ${detail}`);
  }
  try {
    return JSON.parse(result.stdout);
  } catch {
    throw new Error("runtime policy compiler returned non-JSON output");
  }
}

function assertArtifact(value: unknown): asserts value is RuntimePolicyArtifact {
  if (!value || typeof value !== "object") throw new Error("runtime policy artifact is not an object");
  const artifact = value as Partial<RuntimePolicyArtifact>;
  const fourMarket = artifact.runtimePolicyCount === 8;
  const expectedPolicies = fourMarket
    ? PARTNER_FOUR_MARKET_ROUTE.strategies.flatMap((strategy, strategyIndex) => [
      {
        strategyId: strategy.id,
        operation: "deposit" as const,
        seed: (PARTNER_ROUTE.squads.policySeedBefore + BigInt(1 + strategyIndex * 2)).toString(),
      },
      {
        strategyId: strategy.id,
        operation: "withdraw" as const,
        seed: (PARTNER_ROUTE.squads.policySeedBefore + BigInt(2 + strategyIndex * 2)).toString(),
      },
    ])
    : [
      {
        operation: "deposit" as const,
        seed: PARTNER_ROUTE.squads.depositPolicySeed.toString(),
      },
      {
        operation: "withdraw" as const,
        seed: PARTNER_ROUTE.squads.withdrawPolicySeed.toString(),
      },
    ];
  const exactPolicies = Array.isArray(artifact.policies)
    && artifact.policies.length === (fourMarket ? 8 : 2)
    && artifact.policies.every((policy, index) => {
      const expected = expectedPolicies[index]!;
      return policy.operation === expected.operation
        && policy.seed === expected.seed
        && (!fourMarket || ("strategyId" in expected && policy.strategyId === expected.strategyId))
        && policy.policy === policyAddress(BigInt(expected.seed))
        && /^[1-9][0-9]*$/.test(policy.seed ?? "")
        && typeof policy.policy === "string"
        && typeof policy.policyCreate?.dataSha256 === "string"
        && typeof policy.managerExecution?.programId === "string"
        && Array.isArray(policy.managerExecution?.accounts)
        && typeof policy.managerExecution?.dataLength === "number"
        && typeof policy.managerExecution?.dataBase64 === "string"
        && typeof policy.managerExecution?.dataSha256 === "string"
        && policy.policyCreatePacketBytes > 0
        && policy.policyCreatePacketBytes <= 1232;
    });
  const exactSourceManifests = !fourMarket || (
    Array.isArray(artifact.sourceManifests)
    && artifact.sourceManifests.length === PARTNER_FOUR_MARKET_ROUTE.strategies.length
    && artifact.sourceManifests.every((manifest, index) => {
      const expected = PARTNER_FOUR_MARKET_ROUTE.strategies[index]!;
      return manifest.strategyId === expected.id
        && manifest.routeId === PARTNER_FOUR_MARKET_ROUTE.id
        && manifest.routeSpecSha256 === fourMarketRouteSpecSha256()
        && manifest.ids.squadsSettings === PARTNER_ROUTE.squads.settings
        && manifest.ids.manager === PARTNER_ROUTE.squads.manager
        && manifest.ids.guardian === PARTNER_ROUTE.squads.guardian
        && manifest.ids.admin === PARTNER_ROUTE.setupAdmin
        && manifest.ids.vault === PARTNER_ROUTE.vault
        && manifest.ids.reserve === expected.reserve
        && manifest.ids.lendingMarket === expected.graph.lendingMarket
        && manifest.ids.collateralFarm === expected.graph.reserveFarmState;
    })
    && JSON.stringify(artifact.sourceManifest) === JSON.stringify(artifact.sourceManifests[0])
  );
  if (
    artifact.schemaVersion !== 1
    || artifact.evidenceType !== "backyard-voltr-runtime-policy-artifact"
    || artifact.verdict !== "RUNTIME_POLICY_ARTIFACT_COMPILED_AND_VERIFIED"
    || artifact.broadcast !== false
    || artifact.routeId !== (fourMarket ? PARTNER_FOUR_MARKET_ROUTE.id : PARTNER_ROUTE.id)
    || artifact.routeSpecSha256 !== (fourMarket ? fourMarketRouteSpecSha256() : routeSpecSha256(PARTNER_ROUTE))
    || (artifact.runtimePolicyCount !== 2 && artifact.runtimePolicyCount !== 8)
    || artifact.setupPolicyIncluded !== false
    || (fourMarket && artifact.trailingDataConstraintLimitation !== TRAILING_DATA_CONSTRAINT_LIMITATION)
    || artifact.manager !== PARTNER_ROUTE.squads.manager
    || artifact.policySeedBefore !== PARTNER_ROUTE.squads.policySeedBefore.toString()
    || !exactSourceManifests
    || !/^[0-9a-f]{64}$/.test(artifact.sourceManifestSha256 ?? "")
    || !/^[0-9a-f]{64}$/.test(artifact.artifactSha256 ?? "")
    || !exactPolicies
  ) {
    throw new Error("runtime policy compiler output escaped the exact approved policy catalog boundary");
  }
}

async function loadExactSettings(connection: Connection) {
  const response = await connection.getAccountInfoAndContext(
    new PublicKey(PARTNER_ROUTE.squads.settings),
    { commitment: "finalized" },
  );
  const account = response.value;
  if (!account) throw new Error("Squads Settings account is absent");
  if (!account.owner.equals(new PublicKey(PARTNER_ROUTE.squads.program))) {
    throw new Error("Squads Settings owner is not the approved Squads program");
  }
  const [settings] = SettingsAccount.fromAccountInfo(account);
  const currentPolicySeed = BigInt(settings.policySeed?.toString() ?? "0");
  const signers = settings.signers.map((signer) => ({
    address: signer.key.toBase58(),
    permissionsMask: signer.permissions.mask,
  }));
  if (
    (currentPolicySeed !== PARTNER_ROUTE.squads.policySeedBefore
      && currentPolicySeed !== PARTNER_ROUTE.squads.withdrawPolicySeed)
    || settings.threshold !== PARTNER_ROUTE.squads.threshold
    || settings.timeLock !== 0
    || signers.length !== 1
    || signers[0]?.address !== PARTNER_ROUTE.setupAdmin
    || signers[0]?.permissionsMask !== 7
  ) {
    throw new Error("Squads Settings is not the exact finalized pre-compilation authority state");
  }
  return { contextSlot: response.context.slot, currentPolicySeed } as const;
}

export async function compileRuntimePolicyArtifact(): Promise<RuntimePolicyArtifact> {
  const route = PARTNER_ROUTE;
  const connection = new Connection(rpcUrl(), "finalized");
  const genesisHash = await connection.getGenesisHash();
  if (genesisHash !== route.genesisHash) {
    throw new Error(`refusing non-mainnet genesis ${genesisHash}`);
  }
  const settings = await loadExactSettings(connection);
  const isolation = await verifyNonCatalogSquadsPoliciesIsolated(
    rpcUrl(),
    17n,
    24n,
    settings.contextSlot,
    "finalized",
  );
  if (isolation.verdict !== "PARTNER_NON_CATALOG_SQUADS_ISOLATION_PASS") {
    throw new Error("shared Squads non-catalog boundary is not isolated from the Voltr manager route");
  }
  const policyAddresses = Array.from({ length: 8 }, (_, index) =>
    policyAddress(route.squads.policySeedBefore + BigInt(index + 1)));
  const policyState = await connection.getMultipleAccountsInfoAndContext(
    policyAddresses.map((value) => new PublicKey(value)),
    { commitment: "finalized", minContextSlot: settings.contextSlot },
  );
  const expectedPolicyPresence = settings.currentPolicySeed === route.squads.withdrawPolicySeed;
  if (
    policyState.value.slice(0, 2).some((account) => (account !== null) !== expectedPolicyPresence)
    || policyState.value.slice(2).some((account) => account !== null)
    || policyState.value.some((account) =>
      account !== null
      && !account.owner.equals(new PublicKey(route.squads.program))
    )
  ) {
    throw new Error("runtime policy PDA occupancy does not match the exact seed sequence state");
  }

  const manifests = await Promise.all(PARTNER_FOUR_MARKET_ROUTE.strategies.map(async (strategy, index) => {
    const strategyId = strategy.id as PartnerStrategyId;
    const strategyRoute = partnerBuilderRoute(strategyId);
    const accounts = await deriveVoltrAccounts(strategyRoute);
    const reserve = await loadMainReserveGraph(rpcUrl(), strategyRoute, accounts.strategyAuth);
    const manager = createNoopSigner(strategyRoute.squads.manager);
    const builder = await createVoltrRouteBuilder(strategyRoute, reserve.graph);
    const [deposit, withdraw] = await Promise.all([
      builder.strategy.deposit(manager, strategyRoute.asset.proofAmountRaw),
      builder.strategy.withdraw(manager, strategyRoute.asset.proofAmountRaw),
    ]);
    const depositSeed = route.squads.policySeedBefore + BigInt(1 + index * 2);
    return {
      schemaVersion: 1,
      routeId: PARTNER_FOUR_MARKET_ROUTE.id,
      routeSpecSha256: fourMarketRouteSpecSha256(),
      strategyId,
      cluster: strategyRoute.cluster,
      genesisHash,
      ids: {
        squadsSettings: strategyRoute.squads.settings,
        manager: strategyRoute.squads.manager,
        guardian: strategyRoute.squads.guardian,
        admin: strategyRoute.setupAdmin,
        vault: strategyRoute.vault,
        reserve: strategy.reserve,
        lendingMarket: strategy.graph.lendingMarket,
        collateralFarm: strategy.graph.reserveFarmState,
      },
      vaultIndex: strategyRoute.squads.vaultIndex,
      limits: {
        maxPerOperationRaw: strategyRoute.asset.maxManagerOperationRaw.toString(),
        solanaPacketBytes: 1232,
      },
      policySeeds: {
        policySeedBefore: route.squads.policySeedBefore.toString(),
        deposit: depositSeed.toString(),
        withdraw: (depositSeed + 1n).toString(),
      },
      instructions: {
        deposit: compilerInstruction(deposit.canonical),
        withdraw: compilerInstruction(withdraw.canonical),
      },
    } satisfies RuntimePolicyManifest;
  }));
  const tempDir = mkdtempSync(resolve("/tmp", "backyard-voltr-policy-manifests-"));
  const manifestPaths = manifests.map((manifest, index) => {
    const path = resolve(tempDir, `${index}-${manifest.strategyId}.json`);
    writeFileSync(path, JSON.stringify(manifest));
    return path;
  });
  let artifact: unknown;
  try {
    artifact = runCompiler(manifestPaths.flatMap((path) => ["--manifest", path]));
  } finally {
    rmSync(tempDir, { recursive: true, force: true });
  }
  assertArtifact(artifact);
  return artifact;
}

export function verifyRuntimePolicyArtifact(path: string) {
  const report = runCompiler(["--verify-artifact", resolve(path)]);
  const value = report as Record<string, unknown>;
  const fourMarket = value.runtimePolicyCount === 8;
  if (
    value.verdict !== "RUNTIME_POLICY_ARTIFACT_VERIFIED"
    || value.broadcast !== false
    || value.routeSpecSha256 !== (fourMarket ? fourMarketRouteSpecSha256() : routeSpecSha256(PARTNER_ROUTE))
    || (value.runtimePolicyCount !== 2 && value.runtimePolicyCount !== 8)
    || value.setupPolicyIncluded !== false
    || typeof value.artifactSha256 !== "string"
  ) {
    throw new Error("runtime policy artifact verifier returned an invalid verdict");
  }
  return report;
}

export function loadRuntimePolicyArtifact(path: string): Readonly<{
  path: string;
  fileSha256: string;
  artifact: RuntimePolicyArtifact;
}> {
  const absolutePath = resolve(path);
  const bytes = readFileSync(absolutePath);
  const parsed: unknown = JSON.parse(bytes.toString("utf8"));
  assertArtifact(parsed);
  verifyRuntimePolicyArtifact(absolutePath);
  return {
    path: absolutePath,
    fileSha256: createHash("sha256").update(bytes).digest("hex"),
    artifact: parsed,
  };
}
