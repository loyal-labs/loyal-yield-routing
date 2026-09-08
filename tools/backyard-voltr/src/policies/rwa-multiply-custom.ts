import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { AccountRole, createNoopSigner, type Instruction } from "@solana/kit";
import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE, type RwaMultiplyRouteSpec } from "../domain/rwa-multiply-route-spec.js";
import {
  V2_CUSTOM_POLICY_TARGET,
  type CustomPolicyCaps,
  type CustomPolicySpendingLimit,
  type CustomPolicyTarget,
} from "../domain/custom-policy-target.js";
import {
  buildRwaMultiplyArmReportInstruction,
  buildRwaMultiplyManagerInstructions,
  buildRwaMultiplyWithdrawalStagingInstruction,
  deriveRwaMultiplyVoltrAccounts,
} from "../integrations/rwa-multiply-voltr.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const COMPILER_BIN = "compile-voltr-custom-policy";

type SettingsState = Readonly<{
  policySeed: { toString(): string } | null;
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
}>;

type PolicyState = Readonly<{
  settings: PublicKey;
  seed: { toString(): string };
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
  policyState: Readonly<{ __kind: string; fields?: readonly unknown[] }>;
}>;

const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [SettingsState, number] };
}).Settings;
const Policy = (squadsGenerated as unknown as {
  Policy: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [PolicyState, number] };
}).Policy;

type WireInstruction = Readonly<{
  programId: string;
  accounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
  dataBase64: string;
}>;

export type CustomPolicyArtifact = Readonly<{
  schema: "loyal-voltr-custom-policy-artifact/v5";
  verdict: "VOLTR_CUSTOM_POLICY_ARTIFACT_COMPILED_NOT_DEPLOYED";
  physicalPolicyCount: 4;
  deploymentReady: false;
  sourceSha256: string;
  policies: readonly Readonly<{
    operation: "allocation" | "nav-refresh" | "stage-withdrawal" | "withdraw";
    seed: string;
    policy: string;
    constraintIndex: 0;
    constraintIndices: readonly number[];
    createInstruction: WireInstruction;
  }>[];
}>;

export type CustomPolicyVerificationRow = Readonly<{
  operation: CustomPolicyArtifact["policies"][number]["operation"];
  seed: string;
  policy: string;
  pass: boolean;
  reason?: string;
  dataSha256?: string;
}>;

export type CustomPolicyMutation = Readonly<
  | { kind: "noop" }
  | {
    kind: "create";
    target: CustomPolicyArtifact["policies"][number];
    row: CustomPolicyVerificationRow;
    instructions: readonly [WireInstruction];
  }
>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function wire(instruction: Instruction): WireInstruction {
  return {
    programId: instruction.programAddress,
    accounts: (instruction.accounts ?? []).map((account) => ({
      address: account.address,
      signer: account.role === AccountRole.READONLY_SIGNER || account.role === AccountRole.WRITABLE_SIGNER,
      writable: account.role === AccountRole.WRITABLE || account.role === AccountRole.WRITABLE_SIGNER,
    })),
    dataBase64: Buffer.from(instruction.data ?? []).toString("base64"),
  };
}

function policyAddress(seed: bigint, route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE): string {
  const seedBytes = Buffer.alloc(8);
  seedBytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync([
    Buffer.from("smart_account"),
    Buffer.from("policy"),
    new PublicKey(route.squads.settings).toBuffer(),
    seedBytes,
  ], new PublicKey(route.squads.program))[0].toBase58();
}

function dataValue(value: Readonly<{ __kind?: unknown; fields?: readonly unknown[] }>) {
  const kind = String(value.__kind ?? "");
  const raw = value.fields?.[0];
  if (kind === "U8Slice") {
    const bytes = raw instanceof Uint8Array
      ? raw
      : raw && typeof raw === "object"
        ? Uint8Array.from(Object.values(raw as Record<string, number>))
        : new Uint8Array();
    return { kind, value: Buffer.from(bytes).toString("hex") };
  }
  return { kind, value: typeof raw === "number" ? String(raw) : String((raw as { toString?: () => string })?.toString?.() ?? raw) };
}

function decodedConstraint(value: unknown) {
  const constraint = value as {
    accountIndex?: number;
    accountConstraint?: { __kind?: unknown; fields?: readonly unknown[] };
    dataOffset?: { toString(): string } | number;
    dataValue?: { __kind?: unknown; fields?: readonly unknown[] };
    operator?: { __kind?: unknown } | number;
  };
  if (constraint.accountConstraint) {
    const keys = constraint.accountConstraint.fields?.[0] as readonly PublicKey[] | undefined;
    return {
      index: constraint.accountIndex,
      kind: String(constraint.accountConstraint.__kind ?? ""),
      keys: keys?.map((key) => key.toBase58()) ?? [],
    };
  }
  return {
    offset: String((constraint.dataOffset as { toString?: () => string })?.toString?.() ?? constraint.dataOffset),
    operator: typeof constraint.operator === "number"
      ? (["Equals", "NotEquals", "GreaterThan", "GreaterThanOrEqualTo", "LessThan", "LessThanOrEqualTo"] as const)[constraint.operator] ?? `Unknown(${constraint.operator})`
      : String(constraint.operator?.__kind ?? ""),
    ...dataValue(constraint.dataValue ?? {}),
  };
}

function integerString(value: unknown): string | null {
  if (value === null || value === undefined) return null;
  if (typeof value === "bigint") return value.toString();
  if (typeof value === "number") return Number.isInteger(value) ? String(value) : null;
  if (typeof value === "object" && value !== null && "toString" in value) {
    const toString = (value as { toString?: unknown }).toString;
    if (typeof toString === "function") return toString.call(value);
  }
  return null;
}

/**
 * Compare the generated SDK's finalized SpendingLimitV2 shape with the
 * install target. The period is nested under timeConstraints in account
 * state; accepting a top-level period would let a failed reconciliation pass
 * while the ordered policy journal advances.
 */
export function spendingLimitsMatch(
  limits: readonly unknown[],
  expected: CustomPolicySpendingLimit | null,
): boolean {
  if (expected === null) return limits.length === 0;
  if (limits.length !== 1) return false;
  const shaped = limits[0] as {
    mint?: { toBase58?: () => string };
    timeConstraints?: {
      start?: unknown;
      expiration?: unknown;
      period?: { __kind?: unknown };
      accumulateUnused?: unknown;
    };
    quantityConstraints?: { maxPerPeriod?: unknown };
  };
  const timeConstraints = shaped.timeConstraints;
  const observedExpiration = timeConstraints?.expiration === null
    ? null
    : timeConstraints?.expiration === undefined
      ? undefined
      : integerString(timeConstraints.expiration);
  const expectedExpiration = expected.expiration === null
    ? null
    : expected.expiration.toString();
  return shaped.mint?.toBase58?.() === expected.mint
    && integerString(timeConstraints?.start) === expected.start.toString()
    && observedExpiration !== undefined
    && observedExpiration === expectedExpiration
    && timeConstraints?.period?.__kind === expected.period
    && timeConstraints?.accumulateUnused === expected.accumulateUnused
    && integerString(shaped.quantityConstraints?.maxPerPeriod) === expected.maxPerPeriodRaw.toString();
}

/**
 * Expected capital-wire constraints for one custom policy set. `navRaw` adds
 * the `nav_after_raw <= cap` bound at capital offset 51; `null` (v2) leaves
 * the reported NAV unconstrained there.
 */
function expectedData(
  operation: CustomPolicyArtifact["policies"][number]["operation"],
  caps: CustomPolicyCaps,
) {
  const deposit = "f65239e283defdf9";
  const withdraw = "1f2da205c1d986bc";
  const depositEnvelope = "0108000000f223c68952e1f2b6013900000001";
  const withdrawEnvelope = "0108000000b712469c946da122013900000001";
  const cap = caps.amountRaw!.toString();
  const navCap = caps.reportNavRaw === null ? [] : [
    { offset: "51", operator: "LessThanOrEqualTo", kind: "U64Le", value: caps.reportNavRaw.toString() },
  ];
  if (operation === "allocation") return [
    { offset: "0", operator: "Equals", kind: "U8Slice", value: deposit },
    { offset: "8", operator: "GreaterThan", kind: "U64Le", value: "0" },
    { offset: "8", operator: "LessThanOrEqualTo", kind: "U64Le", value: cap },
    ...navCap,
    { offset: "16", operator: "Equals", kind: "U8Slice", value: depositEnvelope },
  ];
  if (operation === "nav-refresh") return [
    { offset: "0", operator: "Equals", kind: "U8Slice", value: deposit },
    { offset: "8", operator: "Equals", kind: "U64Le", value: "0" },
    ...navCap,
    { offset: "16", operator: "Equals", kind: "U8Slice", value: depositEnvelope },
  ];
  if (operation === "stage-withdrawal") return [
    { offset: "0", operator: "Equals", kind: "U8", value: "12" },
    { offset: "1", operator: "GreaterThan", kind: "U64Le", value: "0" },
    { offset: "1", operator: "LessThanOrEqualTo", kind: "U64Le", value: cap },
    { offset: "9", operator: "Equals", kind: "U8", value: String(RWA_MULTIPLY_ROUTE.assets.decimals) },
  ];
  return [
    { offset: "0", operator: "Equals", kind: "U8Slice", value: withdraw },
    { offset: "8", operator: "GreaterThan", kind: "U64Le", value: "0" },
    { offset: "8", operator: "LessThanOrEqualTo", kind: "U64Le", value: cap },
    ...navCap,
    { offset: "16", operator: "Equals", kind: "U8Slice", value: withdrawEnvelope },
  ];
}

/**
 * Expected ArmReport-wire constraints. The reported NAV rides at arm offset
 * 39, so a capped set carries the same ceiling there before the envelope tail.
 */
function expectedArmData(
  operation: CustomPolicyArtifact["policies"][number]["operation"],
  caps: CustomPolicyCaps,
) {
  const operationTag = operation === "withdraw" ? "01" : "00";
  const cap = caps.amountRaw!.toString();
  const navCap = caps.reportNavRaw === null ? [] : [
    { offset: "39", operator: "LessThanOrEqualTo", kind: "U64Le", value: caps.reportNavRaw.toString() },
  ];
  const constraints = [
    { offset: "0", operator: "Equals", kind: "U8Slice", value: `a4aff629b28c2303${operationTag}` },
  ];
  if (operation === "nav-refresh") {
    constraints.push({ offset: "9", operator: "Equals", kind: "U64Le", value: "0" });
  } else {
    constraints.push(
      { offset: "9", operator: "GreaterThan", kind: "U64Le", value: "0" },
      { offset: "9", operator: "LessThanOrEqualTo", kind: "U64Le", value: cap },
    );
  }
  constraints.push(...navCap);
  constraints.push({ offset: "17", operator: "Equals", kind: "U8Slice", value: "013900000001" });
  return constraints;
}

function parseArtifact(
  value: unknown,
  expectedSeeds: readonly bigint[],
  route: RwaMultiplyRouteSpec = RWA_MULTIPLY_ROUTE,
): CustomPolicyArtifact {
  invariant(value && typeof value === "object", "custom policy compiler returned a non-object");
  const artifact = value as Partial<CustomPolicyArtifact>;
  invariant(artifact.schema === "loyal-voltr-custom-policy-artifact/v5"
    && artifact.verdict === "VOLTR_CUSTOM_POLICY_ARTIFACT_COMPILED_NOT_DEPLOYED"
    && artifact.physicalPolicyCount === 4
    && artifact.deploymentReady === false
    && /^[0-9a-f]{64}$/.test(artifact.sourceSha256 ?? "")
    && Array.isArray(artifact.policies)
    && artifact.policies.length === 4,
  "custom policy compiler escaped its artifact contract");
  const operations = ["allocation", "nav-refresh", "stage-withdrawal", "withdraw"];
  const indexes = [[0, 1], [0, 1], [0], [0, 1]] as const;
  artifact.policies.forEach((policy, index) => {
    const seed = expectedSeeds[index]!;
    invariant(policy.operation === operations[index]
      && policy.seed === seed.toString()
      && policy.policy === policyAddress(seed, route)
      && policy.constraintIndex === 0
      && JSON.stringify(policy.constraintIndices) === JSON.stringify(indexes[index])
      && policy.createInstruction?.programId === route.squads.program
      && policy.createInstruction.accounts.length === 6
      && policy.createInstruction.dataBase64.length > 0,
    `custom ${operations[index]} policy artifact drifted`);
  });
  return artifact as CustomPolicyArtifact;
}

export async function compileCustomPolicyArtifact(
  policySeedBefore: bigint,
  target: CustomPolicyTarget = V2_CUSTOM_POLICY_TARGET,
): Promise<CustomPolicyArtifact> {
  invariant(policySeedBefore >= 0n && policySeedBefore < (1n << 64n) - 4n,
    "current Squads policy seed is outside the supported range");
  const route = target.route;
  const manager = createNoopSigner(route.squads.vault);
  const report = {
    sequence: 1n,
    observedSlot: 1n,
    navAfterRaw: 0n,
    snapshotDigest: new Uint8Array(32).fill(1),
  } as const;
  const [positive, zero, stage, allocationArm, navRefreshArm, withdrawArm] = await Promise.all([
    buildRwaMultiplyManagerInstructions(manager, route.vault.proofAmountRaw, report, route),
    buildRwaMultiplyManagerInstructions(manager, 0n, report, route),
    buildRwaMultiplyWithdrawalStagingInstruction(manager, route.vault.proofAmountRaw, route),
    buildRwaMultiplyArmReportInstruction(manager, "deposit", route.vault.proofAmountRaw, report, route),
    buildRwaMultiplyArmReportInstruction(manager, "deposit", 0n, report, route),
    buildRwaMultiplyArmReportInstruction(manager, "withdraw", route.vault.proofAmountRaw, report, route),
  ]);
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const seeds = target.seeds;
  invariant(target.caps.amountRaw !== null, "custom policy amount cap is required");
  invariant(seeds.navRefresh === seeds.allocation + 1n
    && seeds.stageWithdrawal === seeds.allocation + 2n
    && seeds.withdraw === seeds.allocation + 3n,
  "custom policy seeds are not the fixed four-policy packet-fit split");
  const input = {
    identity: {
      settings: route.squads.settings,
      authority: route.setupAdmin,
      delegatedSigner: route.squads.delegatedExecutor,
      manager: route.squads.vault,
      squadsProgram: route.squads.program,
      vaultIndex: route.squads.vaultIndex,
      vault: route.vault.address,
      strategy: route.customAdaptor.strategyConfig,
      voltrProgram: route.programs.voltr,
      adaptorProgram: route.customAdaptor.program,
      tokenProgram: route.assets.tokenProgram,
      assetMint: route.assets.assetMint,
      squadsAssetAta: route.squads.assetAta,
      strategyAssetAta: accounts.strategyAssetAta,
      reportTicket: accounts.reportTicket,
      maxAmountRaw: target.caps.amountRaw!.toString(),
      navCapRaw: target.caps.reportNavRaw === null ? null : target.caps.reportNavRaw.toString(),
      dailySpendingLimit: target.caps.dailySpendingLimit === null ? null : {
        mint: target.caps.dailySpendingLimit.mint,
        maxPerPeriodRaw: target.caps.dailySpendingLimit.maxPerPeriodRaw.toString(),
      },
      assetDecimals: route.assets.decimals,
      seeds: {
        allocation: seeds.allocation.toString(),
        navRefresh: seeds.navRefresh.toString(),
        stageWithdrawal: seeds.stageWithdrawal.toString(),
        withdraw: seeds.withdraw.toString(),
      },
    },
    instructions: {
      allocationArm: wire(allocationArm),
      allocation: wire(positive.deposit),
      navRefreshArm: wire(navRefreshArm),
      navRefresh: wire(zero.deposit),
      stageWithdrawal: wire(stage),
      withdrawArm: wire(withdrawArm),
      withdraw: wire(positive.withdraw),
    },
  };
  const source = JSON.stringify(input);
  const result = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin", COMPILER_BIN], {
    cwd: REPOSITORY_ROOT,
    input: source,
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
  });
  if (result.error) throw result.error;
  invariant(result.status === 0,
    `custom policy compiler failed (status ${result.status}, signal ${result.signal}): ${
      result.stderr.trim() || result.stdout.trim() || "<no output>"}`);
  const artifact = parseArtifact(JSON.parse(result.stdout), [
    seeds.allocation, seeds.navRefresh, seeds.stageWithdrawal, seeds.withdraw,
  ], route);
  invariant(artifact.sourceSha256 === createHash("sha256").update(source).digest("hex"),
    "custom policy compiler source hash drifted");
  return artifact;
}

export async function readFinalizedCustomPolicySeed(connection: Connection) {
  const response = await connection.getAccountInfoAndContext(
    new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings),
    { commitment: "finalized" },
  );
  invariant(response.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program,
    "Squads Settings is absent or has the wrong owner");
  const [settings] = Settings.fromAccountInfo(response.value);
  invariant(settings.threshold === 1 && settings.timeLock === 0
    && settings.signers.length === 1
    && settings.signers[0]?.key.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin
    && settings.signers[0]?.permissions.mask === 7,
  "Squads Settings authority boundary drifted");
  const policySeedBefore = BigInt(settings.policySeed?.toString() ?? "0");
  return { contextSlot: response.context.slot, policySeedBefore };
}

export async function compileCurrentCustomPolicyArtifact(
  connection: Connection,
  target: CustomPolicyTarget = V2_CUSTOM_POLICY_TARGET,
) {
  const { contextSlot, policySeedBefore } = await readFinalizedCustomPolicySeed(connection);
  const fixed = target.seeds;
  invariant(fixed.navRefresh === fixed.allocation + 1n
    && fixed.stageWithdrawal === fixed.allocation + 2n
    && fixed.withdraw === fixed.allocation + 3n,
  "custom policy seeds are not the fixed four-policy packet-fit split");
  return {
    contextSlot,
    policySeedBefore,
    artifact: await compileCustomPolicyArtifact(policySeedBefore, target),
  };
}

export async function verifyInstalledCustomPolicies(
  connection: Connection,
  target: CustomPolicyTarget = V2_CUSTOM_POLICY_TARGET,
) {
  const compiled = await compileCurrentCustomPolicyArtifact(connection, target);
  const route = target.route;
  const manager = createNoopSigner(route.squads.vault);
  const report = {
    sequence: 1n,
    observedSlot: 1n,
    navAfterRaw: 0n,
    snapshotDigest: new Uint8Array(32).fill(1),
  } as const;
  const [positive, zero, stage, allocationArm, navRefreshArm, withdrawArm] = await Promise.all([
    buildRwaMultiplyManagerInstructions(manager, route.vault.proofAmountRaw, report, route),
    buildRwaMultiplyManagerInstructions(manager, 0n, report, route),
    buildRwaMultiplyWithdrawalStagingInstruction(manager, route.vault.proofAmountRaw, route),
    buildRwaMultiplyArmReportInstruction(manager, "deposit", route.vault.proofAmountRaw, report, route),
    buildRwaMultiplyArmReportInstruction(manager, "deposit", 0n, report, route),
    buildRwaMultiplyArmReportInstruction(manager, "withdraw", route.vault.proofAmountRaw, report, route),
  ]);
  const templates = [
    [allocationArm, positive.deposit],
    [navRefreshArm, zero.deposit],
    [stage],
    [withdrawArm, positive.withdraw],
  ] as const;
  const selectedIndexes = [
    [[0, 1], [0, 2, 3, 8, 11, 12, 13, 14, 15, 16, 17]],
    [[0, 1], [0, 2, 3, 8, 11, 12, 13, 14, 15, 16, 17]],
    [[0, 1, 2, 3]],
    [[0, 1], [0, 2, 5, 6, 9, 12, 13, 14, 15, 16, 17]],
  ] as const;
  const expectedPrograms = [
    [route.customAdaptor.program, route.programs.voltr],
    [route.customAdaptor.program, route.programs.voltr],
    [route.assets.tokenProgram],
    [route.customAdaptor.program, route.programs.voltr],
  ] as const;
  const expectedDataSets = [
    [expectedArmData("allocation", target.caps), expectedData("allocation", target.caps)],
    [expectedArmData("nav-refresh", target.caps), expectedData("nav-refresh", target.caps)],
    [expectedData("stage-withdrawal", target.caps)],
    [expectedArmData("withdraw", target.caps), expectedData("withdraw", target.caps)],
  ] as const;
  const response = await connection.getMultipleAccountsInfoAndContext(
    compiled.artifact.policies.map(({ policy }) => new PublicKey(policy)),
    { commitment: "finalized", minContextSlot: compiled.contextSlot },
  );
  const rows = compiled.artifact.policies.map((expected, index) => {
    const info = response.value[index];
    if (!info) return {
      operation: expected.operation,
      seed: expected.seed,
      policy: expected.policy,
      pass: false,
      reason: "absent",
    } satisfies CustomPolicyVerificationRow;
    if (!info.owner.equals(new PublicKey(route.squads.program))) {
      return {
        operation: expected.operation,
        seed: expected.seed,
        policy: expected.policy,
        pass: false,
        reason: "wrong owner",
        dataSha256: createHash("sha256").update(info.data).digest("hex"),
      } satisfies CustomPolicyVerificationRow;
    }
    let policy: PolicyState;
    try {
      [policy] = Policy.fromAccountInfo(info);
    } catch {
      return {
        operation: expected.operation,
        seed: expected.seed,
        policy: expected.policy,
        pass: false,
        reason: "undecodable policy account",
        dataSha256: createHash("sha256").update(info.data).digest("hex"),
      } satisfies CustomPolicyVerificationRow;
    }
    const body = policy.policyState.fields?.[0] as {
      accountIndex?: number;
      preHook?: unknown;
      postHook?: unknown;
      spendingLimits?: readonly unknown[];
      instructionsConstraints?: readonly Readonly<{
        programId: PublicKey;
        accountConstraints?: readonly unknown[];
        dataConstraints?: readonly unknown[];
      }>[];
    } | undefined;
    const observedConstraints = expected.constraintIndices.map((constraintIndex, innerIndex) => {
      const constraint = body?.instructionsConstraints?.[constraintIndex];
      const template = templates[index]![innerIndex]!;
      const expectedAccounts = selectedIndexes[index]![innerIndex]!.map((accountIndex) => ({
        index: accountIndex,
        kind: "Pubkey",
        keys: [template.accounts?.[accountIndex]?.address ?? ""],
      }));
      const observedAccounts = constraint?.accountConstraints?.map(decodedConstraint) ?? [];
      const observedData = constraint?.dataConstraints?.map(decodedConstraint) ?? [];
      return {
        pass: Boolean(constraint?.programId.equals(new PublicKey(expectedPrograms[index]![innerIndex]!))
          && JSON.stringify(observedAccounts) === JSON.stringify(expectedAccounts)
          && JSON.stringify(observedData) === JSON.stringify(expectedDataSets[index]![innerIndex])),
        program: constraint?.programId.toBase58() ?? null,
        accountConstraints: observedAccounts,
        dataConstraints: observedData,
      };
    });
    const authorityBoundaryPass = Boolean(policy.settings.equals(new PublicKey(route.squads.settings))
      && policy.seed.toString() === expected.seed
      && policy.threshold === 1
      && policy.timeLock === 0
      && policy.signers.length === 1
      && policy.signers[0]?.key.equals(new PublicKey(route.squads.delegatedExecutor))
      && policy.signers[0]?.permissions.mask === 7);
    // Every policy in a limited set must carry exactly the configured daily
    // limit (Squads charges it only on decreases, so deposit-side lanes are
    // unaffected); the uncapped v2 set must carry none.
    const expectedLimit = target.caps.dailySpendingLimit === null
      ? null
      : target.caps.dailySpendingLimit;
    const spendingLimitsPass = spendingLimitsMatch(body?.spendingLimits ?? [], expectedLimit);
    const pass = Boolean(authorityBoundaryPass
      && policy.policyState.__kind === "ProgramInteraction"
      && body?.accountIndex === route.squads.vaultIndex
      && body.preHook == null
      && body.postHook == null
      && spendingLimitsPass
      && body.instructionsConstraints?.length === expected.constraintIndices.length
      && observedConstraints.every(({ pass }) => pass));
    return {
      operation: expected.operation,
      seed: expected.seed,
      policy: expected.policy,
      pass,
      ...(pass ? {} : { reason: authorityBoundaryPass ? "inexact policy payload" : "authority boundary mismatch" }),
      dataSha256: createHash("sha256").update(info.data).digest("hex"),
      owner: info.owner.toBase58(),
      accountIndex: body?.accountIndex ?? null,
      constraints: observedConstraints,
    };
  });
  return {
    contextSlot: response.context.slot,
    policySeedBefore: compiled.policySeedBefore,
    sourceSha256: compiled.artifact.sourceSha256,
    pass: rows.every(({ pass }) => pass),
    rows,
    artifact: compiled.artifact,
  };
}

export function selectCustomPolicyMutation(input: Readonly<{
  policySeedBefore: bigint;
  rows: readonly CustomPolicyVerificationRow[];
  artifact: CustomPolicyArtifact;
}>): CustomPolicyMutation {
  invariant(input.rows.length === input.artifact.policies.length,
    "custom policy inspection row count drifted");
  const index = input.rows.findIndex(({ pass }) => !pass);
  if (index < 0) return { kind: "noop" };
  const row = input.rows[index]!;
  const target = input.artifact.policies[index]!;
  invariant(row.operation === target.operation && row.seed === target.seed && row.policy === target.policy,
    "custom policy inspection identity drifted");
  if (row.reason === "absent") {
    invariant(BigInt(target.seed) === input.policySeedBefore + 1n,
      `absent custom policy seed ${target.seed} is not the next finalized Settings seed`);
    return { kind: "create", target, row, instructions: [target.createInstruction] };
  }
  throw new Error(
    `existing custom ${target.operation} policy is inexact; policy seeds are monotonic and require a fresh-seed rollover`,
  );
}
