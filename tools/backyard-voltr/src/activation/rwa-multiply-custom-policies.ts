import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { AccountRole, address, type Instruction } from "@solana/kit";
import { Connection, PublicKey } from "@solana/web3.js";

import {
  V2_CUSTOM_POLICY_TARGET,
  type CustomPolicyTarget,
} from "../domain/custom-policy-target.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  deriveStrategyTwoPolicySeeds,
  rwaMultiplyStrategyTwoTarget,
  type StrategyTwoIdentity,
} from "../domain/rwa-multiply-strategy2-route-spec.js";
import { prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  assertStrategyTwoInstallMayCoexist,
  LEGACY_CUSTOM_POLICY_ADDRESSES,
} from "../policies/rwa-multiply-legacy-retirement.js";
import {
  assertStrategyTwoSeedJournalAnchor,
  assertStrategyTwoFirstInvocation,
  parseStrategyTwoSeed,
  readStrategyTwoSeedExpectation,
  strategyTwoSeedStrings,
  STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE,
  STRATEGY_TWO_REPAIR_POLICY_ADDRESS,
  writeStrategyTwoSeedExpectation,
  type StrategyTwoSeedAnchorObservation,
  type StrategyTwoSeedExpectation,
} from "../policies/rwa-multiply-strategy2-seed-journal.js";
import {
  selectCustomPolicyMutation,
  readFinalizedCustomPolicySeed,
  verifyInstalledCustomPolicies,
  type CustomPolicyArtifact,
} from "../policies/rwa-multiply-custom.js";

const PACKET_LIMIT = 1_232;
const MAX_POLICY_COST_LAMPORTS = 20_000_000;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function instruction(value: CustomPolicyArtifact["policies"][number]["createInstruction"]): Instruction {
  return {
    programAddress: address(value.programId),
    accounts: value.accounts.map((account) => ({
      address: address(account.address),
      role: account.signer
        ? account.writable ? AccountRole.WRITABLE_SIGNER : AccountRole.READONLY_SIGNER
        : account.writable ? AccountRole.WRITABLE : AccountRole.READONLY,
    })),
    data: Buffer.from(value.dataBase64, "base64"),
  };
}

function writePrivate(path: string, value: Record<string, unknown>, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, (_key, entry) => typeof entry === "bigint" ? entry.toString() : entry, 2)}\n`, {
    flag,
    mode: 0o600,
  });
  chmodSync(path, 0o600);
}

function flagValue(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

async function readStrategyTwoSeedAnchor(
  connection: Connection,
  settingsAtStart: Readonly<{ contextSlot: number; policySeedBefore: bigint }>,
  target: CustomPolicyTarget,
  genesisHash: string,
): Promise<StrategyTwoSeedAnchorObservation> {
  const response = await connection.getAccountInfoAndContext(
    new PublicKey(STRATEGY_TWO_REPAIR_POLICY_ADDRESS),
    { commitment: "finalized", minContextSlot: settingsAtStart.contextSlot },
  );
  const repair = response.value;
  return {
    settingsAddress: target.route.squads.settings,
    genesisHash,
    strategyTwoConfig: target.route.customAdaptor.strategyConfig,
    delegatedSigner: target.route.squads.delegatedExecutor,
    repairPolicy: STRATEGY_TWO_REPAIR_POLICY_ADDRESS,
    repairPolicyPresent: repair !== null
      && repair.owner.toBase58() === target.route.squads.program
      && repair.data.length > 0,
    repairPolicyDataSha256: repair === null ? null : sha256(repair.data),
    observationSlot: Math.max(settingsAtStart.contextSlot, response.context.slot),
    policySeedBefore: settingsAtStart.policySeedBefore,
  };
}

/**
 * Which policy set this run installs. `v2` is the live set at seeds 62-65;
 * `strategy-two` compiles against the rotated identities supplied on the
 * command line and installs at the four seeds derived from the finalized
 * Settings counter. The target choice must be
 * explicit so a strategy-two run can never silently compare against the v2
 * contract.
 */
async function resolveInstallTarget(
  policySeedBefore: bigint,
): Promise<CustomPolicyTarget> {
  const kind = flagValue("--target") ?? "v2";
  invariant(kind === "v2" || kind === "strategy-two", "--target must be v2 or strategy-two");
  if (kind === "v2") return V2_CUSTOM_POLICY_TARGET;
  const config = flagValue("--config");
  const delegatedSigner = flagValue("--delegated");
  invariant(config !== undefined && delegatedSigner !== undefined,
    "--target strategy-two requires --config <configAddress> --delegated <executorAddress>");
  const identity: StrategyTwoIdentity = { config: address(config), delegatedSigner: address(delegatedSigner) };
  return rwaMultiplyStrategyTwoTarget(identity, policySeedBefore);
}

/**
 * Refuses while any legacy policy at seeds 62-65 still exists at finalized
 * commitment. This is the standalone strategy-two cutover gate: it runs
 * keyless as the worker-start preflight (`--assert-legacy-retired` without
 * --execute). Installation deliberately does not call it because legacy
 * policies must coexist until the replacement set is finalized and verified.
 */
async function assertLegacyRetired(connection: Connection, target: CustomPolicyTarget) {
  const legacy = await connection.getMultipleAccountsInfo(
    LEGACY_CUSTOM_POLICY_ADDRESSES.map((value) => new PublicKey(value)), "finalized");
  const surviving = LEGACY_CUSTOM_POLICY_ADDRESSES.filter((_value, index) => legacy[index] !== null);
  invariant(surviving.length === 0,
    `legacy policies still installed at seeds 62-65, refusing to touch the v2 set: ${surviving.join(",")}`);
  return {
    checkedSeeds: [62, 63, 64, 65],
    legacyPolicyAddresses: LEGACY_CUSTOM_POLICY_ADDRESSES,
    expectedStrategyTwoSeeds: strategyTwoSeedStrings(target.seeds),
  };
}

async function main() {
  const execute = process.argv.includes("--execute");
  const reconcile = process.argv.includes("--reconcile");
  const journalIndex = process.argv.indexOf("--journal");
  const journal = journalIndex >= 0 ? resolve(process.argv[journalIndex + 1] ?? "") : "";
  invariant(!(execute && reconcile), "--execute and --reconcile are mutually exclusive");
  invariant(!execute || process.env.CONFIRM_MAINNET === "1", "--execute requires CONFIRM_MAINNET=1");
  invariant(!(execute || reconcile) || (journal.endsWith(".json") && existsSync(dirname(journal))),
    "--execute/--reconcile requires --journal PATH under an existing directory");
  invariant(!execute || (!existsSync(journal) && !existsSync(`${journal}.pending`)),
    "journal replay barrier already exists");
  invariant(!reconcile || (!existsSync(journal) && existsSync(`${journal}.pending`)),
    "--reconcile requires one pending journal and no finalized journal");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const route = RWA_MULTIPLY_ROUTE;
  const connection = new Connection(rpcUrl, "finalized");
  const genesisHash = await connection.getGenesisHash();
  invariant(genesisHash === route.genesisHash, "RPC is not mainnet-beta");
  const targetKind = flagValue("--target") ?? "v2";
  invariant(targetKind === "v2" || targetKind === "strategy-two", "--target must be v2 or strategy-two");
  const strategyTwo = targetKind === "strategy-two";
  const seedJournalArg = flagValue("--seed-journal");
  const seedJournal = seedJournalArg === undefined ? "" : resolve(seedJournalArg);
  invariant(!seedJournalArg || (seedJournal.endsWith(".json") && existsSync(dirname(seedJournal))),
    "--seed-journal PATH.json must be under an existing directory");
  invariant(!strategyTwo || seedJournal.length > 0,
    "--target strategy-two requires --seed-journal PATH.json so the four expected seeds are reused across ordered invocations");
  const settingsAtStart = await readFinalizedCustomPolicySeed(connection);
  const seedJournalExists = strategyTwo && seedJournal.length > 0 && existsSync(seedJournal);
  const journaledExpectation = strategyTwo && seedJournalExists
    ? readStrategyTwoSeedExpectation(seedJournal)
    : null;
  const installTarget = await resolveInstallTarget(
    journaledExpectation?.policySeedBefore ?? settingsAtStart.policySeedBefore,
  );
  let seedExpectation: StrategyTwoSeedExpectation | null = journaledExpectation;
  if (strategyTwo) {
    const liveAnchor = await readStrategyTwoSeedAnchor(
      connection, settingsAtStart, installTarget, genesisHash);
    if (seedExpectation === null) {
      assertStrategyTwoFirstInvocation(
        settingsAtStart.policySeedBefore,
        liveAnchor.repairPolicyPresent,
      );
      invariant(liveAnchor.repairPolicyDataSha256 !== null,
        "first strategy-two invocation could not read the finalized repair policy bytes");
      seedExpectation = {
        policySeedBefore: settingsAtStart.policySeedBefore,
        expectedSeeds: Object.values(deriveStrategyTwoPolicySeeds(settingsAtStart.policySeedBefore)),
        settingsAddress: liveAnchor.settingsAddress,
        genesisHash: liveAnchor.genesisHash,
        strategyTwoConfig: liveAnchor.strategyTwoConfig,
        delegatedSigner: liveAnchor.delegatedSigner,
        repairPolicy: liveAnchor.repairPolicy,
        repairPolicyDataSha256: liveAnchor.repairPolicyDataSha256,
        observationSlot: liveAnchor.observationSlot,
      };
    }
    assertStrategyTwoSeedJournalAnchor(seedExpectation, liveAnchor);
  }
  if (seedExpectation !== null) {
    invariant(settingsAtStart.policySeedBefore >= seedExpectation.policySeedBefore
      && settingsAtStart.policySeedBefore <= seedExpectation.policySeedBefore + 4n,
    `finalized Settings counter ${settingsAtStart.policySeedBefore} escaped the journaled strategy-two seed range ${seedExpectation.policySeedBefore + 1n}-${seedExpectation.policySeedBefore + 4n}; refusing to reuse seeds`);
  }
  if (process.argv.includes("--assert-legacy-retired") && !execute && !reconcile) {
    // Standalone keyless gate: this is the preflight the runbook requires
    // before the strategy-two worker image is allowed to start.
    const gate = await assertLegacyRetired(connection, installTarget);
    console.log(JSON.stringify({ verdict: "PASS_LEGACY_RETIRED", broadcast: false,
      ...gate, seedJournal: seedJournal || null }, null, 2));
    return;
  }
  if (execute && !reconcile && strategyTwo) {
    // The legacy set is expected to coexist during installation. Keep a
    // coherent four-account read as an explicit cutover invariant, but do not
    // require absence; the standalone worker-start gate handles that later.
    const legacy = await connection.getMultipleAccountsInfo(
      LEGACY_CUSTOM_POLICY_ADDRESSES.map((value) => new PublicKey(value)), "finalized");
    assertStrategyTwoInstallMayCoexist(legacy);
  }
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "setup admin signer drifted");

  const before = await verifyInstalledCustomPolicies(connection, installTarget);
  if (reconcile) {
    const pending = JSON.parse(readFileSync(`${journal}.pending`, "utf8")) as {
      operation?: unknown;
      mutation?: unknown;
      seed?: unknown;
      policy?: unknown;
      transaction?: {
        expectedSignature?: unknown;
        projectedPolicyDataSha256?: unknown;
        projectedSettingsDataSha256?: unknown;
        protectedPreviousVaultSha256?: unknown;
        protectedVoltrVaultSha256?: unknown;
      };
      policySeedBefore?: unknown;
      expectedPolicySeeds?: unknown;
      seedExpectationJournal?: unknown;
    };
    const signature = String(pending.transaction?.expectedSignature ?? "");
    const policyAddress = String(pending.policy ?? "");
    invariant(pending.mutation === "create", "pending journal lacks the create-only mutation kind");
    invariant(signature.length > 0 && policyAddress.length > 0, "pending journal lacks signature or policy identity");
    if (strategyTwo) {
      const pendingPolicySeedBefore = parseStrategyTwoSeed(
        pending.policySeedBefore,
        "pending strategy-two policySeedBefore",
      );
      invariant(pending.seedExpectationJournal === seedJournal,
        "pending journal records a different strategy-two seed journal");
      invariant(pendingPolicySeedBefore + 1n === before.policySeedBefore
        && JSON.stringify(pending.expectedPolicySeeds) === JSON.stringify(strategyTwoSeedStrings(installTarget.seeds)),
      "pending journal strategy-two seed expectation did not advance exactly once");
    }
    const status = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    const landed = status.value[0];
    invariant(landed?.err === null && landed.confirmationStatus === "finalized", "pending signature is not finalized successfully");
    const installed = before.rows.find((row) => row.policy === policyAddress);
    invariant(installed?.pass === true && installed.operation === pending.operation && installed.seed === String(pending.seed),
      "pending policy does not decode to the exact finalized contract");
    const [policyInfo, settingsInfo, protectedPrevious, protectedVoltr] = await connection.getMultipleAccountsInfo([
      new PublicKey(policyAddress),
      new PublicKey(route.squads.settings),
      new PublicKey(route.previousBackyardVault),
      new PublicKey(route.vault.address),
    ], "finalized");
    invariant(policyInfo != null, "finalized policy account is absent");
    const finalizedPolicyDataSha256 = sha256(policyInfo.data);
    invariant(protectedPrevious && sha256(protectedPrevious.data) === pending.transaction?.protectedPreviousVaultSha256,
      "protected Backyard vault changed across policy activation");
    invariant(protectedVoltr && sha256(protectedVoltr.data) === pending.transaction?.protectedVoltrVaultSha256,
      "active Voltr vault changed across policy activation");
    invariant(settingsInfo && sha256(settingsInfo.data) === pending.transaction?.projectedSettingsDataSha256,
      "finalized Settings bytes differ from the signed simulation projection");
    writePrivate(journal, { ...pending, verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: landed.slot, finalizedContextSlot: status.context.slot,
      finalizedPolicyDataSha256,
      installed }, "wx");
    renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: landed.slot, finalizedContextSlot: status.context.slot,
      finalizedPolicyDataSha256, installed, journal }, null, 2));
    return;
  }
  const mutation = selectCustomPolicyMutation(before);
  if (mutation.kind === "noop") {
    console.log(JSON.stringify({
      verdict: "PASS_ALREADY_FINALIZED",
      broadcast: false,
      ...(strategyTwo ? {
        policySeedBefore: before.policySeedBefore.toString(),
        expectedPolicySeeds: strategyTwoSeedStrings(installTarget.seeds),
        seedExpectationJournal: seedJournal,
      } : {}),
      policies: before.rows,
    }, null, 2));
    return;
  }
  const target = mutation.target;
  const targetIndex = before.artifact.policies.findIndex(({ policy }) => policy === target.policy);
  if (strategyTwo && mutation.kind === "create") {
    // Ordered install: the runbook sends one invocation per seed; seed N+1
    // refuses until seed N is finalized and verified, so the set can never
    // land half-installed in a different order than the runbook documents.
    const seedOrder = Object.values(installTarget.seeds).map(String);
    for (const earlierSeed of seedOrder.slice(0, seedOrder.indexOf(target.seed))) {
      const earlierRow = before.rows.find((row) => row.seed === earlierSeed);
      invariant(earlierRow?.pass === true,
        `strategy-two seed ${earlierSeed} is not finalized and verified; refusing to install seed ${target.seed} out of order`);
    }
  }
  invariant(targetIndex >= 0, "selected custom policy is absent from the compiled artifact");
  const [settingsBefore, protectedPrevious, protectedVoltr] = await connection.getMultipleAccountsInfo([
    new PublicKey(route.squads.settings),
    new PublicKey(route.previousBackyardVault),
    new PublicKey(route.vault.address),
  ], "finalized");
  invariant(settingsBefore?.owner.toBase58() === route.squads.program, "Squads Settings is absent or inexact");
  invariant(protectedPrevious?.owner.toBase58() === route.programs.voltr, "protected Backyard vault is absent or inexact");
  invariant(protectedVoltr?.owner.toBase58() === route.programs.voltr, "active Voltr vault is absent or inexact");
  const settingsBeforeSha256 = sha256(settingsBefore.data);
  const protectedPreviousSha256 = sha256(protectedPrevious.data);
  const protectedVoltrSha256 = sha256(protectedVoltr.data);
  const targetBeforeResponse = await connection.getAccountInfoAndContext(new PublicKey(target.policy), {
    commitment: "finalized",
    minContextSlot: before.contextSlot,
  });
  const targetBefore = targetBeforeResponse.value;
  invariant(targetBefore === null, "custom policy appeared after finalized absence inspection");
  const selectedInstructions = mutation.instructions.map(instruction);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    commitment: "finalized",
    minimumContextSlot: before.contextSlot,
    instructions: selectedInstructions,
    inspectedAddresses: [target.policy, route.squads.settings, route.previousBackyardVault,
      route.vault.address, route.setupAdmin],
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT,
    `custom ${target.operation} policy ${mutation.kind} packet exceeds ${PACKET_LIMIT} bytes`);
  invariant(prepared.simulation.err === null,
    `custom ${target.operation} policy simulation failed: ${JSON.stringify({
      err: prepared.simulation.err,
      logs: prepared.simulation.logs,
    })}`);
  const [postPolicy, postSettings, postPrevious, postVoltr, postAdmin] = prepared.simulation.postAccounts;
  invariant(postPolicy?.owner === route.squads.program, "simulation did not project the exact policy owner");
  invariant(postSettings?.owner === route.squads.program, "simulation changed Settings ownership");
  invariant(postPrevious !== null && postPrevious !== undefined, "simulation omitted the protected Backyard vault");
  invariant(postVoltr !== null && postVoltr !== undefined, "simulation omitted the active Voltr vault");
  invariant(postAdmin !== null && postAdmin !== undefined, "simulation omitted the setup admin");
  const projectedRentDeltaLamports = postPolicy.lamports;
  const projectedCostLamports = projectedRentDeltaLamports + prepared.feeLamports;
  invariant(projectedCostLamports >= 0 && projectedCostLamports <= MAX_POLICY_COST_LAMPORTS,
    `projected custom policy cost ${projectedCostLamports} exceeds bound`);
  invariant(sha256(postPrevious.data) === protectedPreviousSha256,
    "simulation changed the protected Backyard vault");
  invariant(sha256(postVoltr.data) === protectedVoltrSha256,
    "simulation changed the active Voltr vault");
  const plan = {
    schema: "loyal-rwa-multiply-custom-policy-activation/v5",
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    routeSpecSha256: (await import("../domain/rwa-multiply-route-spec.js")).rwaMultiplyRouteSpecSha256(),
    sourceSha256: before.sourceSha256,
    mutation: mutation.kind,
    operation: target.operation,
    seed: target.seed,
    policy: target.policy,
    policySeedBefore: before.policySeedBefore.toString(),
    expectedPolicySeeds: strategyTwoSeedStrings(installTarget.seeds),
    seedExpectationJournal: strategyTwo ? seedJournal : null,
    transaction: {
      packetBytes: prepared.packetBytes,
      instructionCount: selectedInstructions.length,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      projectedCostLamports,
      projectedRentDeltaLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      projectedPolicyDataSha256: sha256(postPolicy.data),
      policyProjectionMode: "semantic_dynamic_create_fields",
      projectedSettingsDataSha256: sha256(postSettings.data),
      previousPolicyDataSha256: null,
      previousSettingsDataSha256: settingsBeforeSha256,
      protectedPreviousVaultSha256: protectedPreviousSha256,
      protectedVoltrVaultSha256: protectedVoltrSha256,
    },
  };
  if (!execute) {
    console.log(JSON.stringify(plan, null, 2));
    return;
  }
  const expectedCounterAtSend = strategyTwo ? BigInt(target.seed) - 1n : before.policySeedBefore;
  const settingsAtSend = await readFinalizedCustomPolicySeed(connection);
  if (strategyTwo && seedExpectation !== null) {
    const anchorAtSend = await readStrategyTwoSeedAnchor(
      connection, settingsAtSend, installTarget, genesisHash);
    assertStrategyTwoSeedJournalAnchor(seedExpectation, anchorAtSend);
    invariant(before.policySeedBefore === expectedCounterAtSend,
      `finalized Settings counter ${before.policySeedBefore} does not match the journaled strategy-two seed expectation for policy ${target.seed}; aborting before send`);
  }
  invariant(settingsAtSend.policySeedBefore === expectedCounterAtSend,
    `finalized Settings counter ${settingsAtSend.policySeedBefore} differs from the journaled send expectation ${expectedCounterAtSend}; aborting before send`);
  if (strategyTwo && seedExpectation !== null && !existsSync(seedJournal)) {
    writeStrategyTwoSeedExpectation(seedJournal, seedExpectation);
  }
  writePrivate(`${journal}.pending`, {
    ...plan,
    signedWireBase64: Buffer.from(prepared.serializedTransaction).toString("base64"),
  }, "wx");
  const returned = await connection.sendRawTransaction(prepared.serializedTransaction, {
    skipPreflight: false,
    preflightCommitment: "finalized",
    maxRetries: 0,
    minContextSlot: prepared.simulationSlot,
  });
  invariant(returned === prepared.expectedSignature, "RPC returned a signature different from the persisted wire");
  const confirmation = await connection.confirmTransaction({ signature: returned, ...prepared.latestBlockhash }, "finalized");
  invariant(confirmation.value.err === null, `policy transaction finalized with ${JSON.stringify(confirmation.value.err)}`);
  const after = await verifyInstalledCustomPolicies(connection, installTarget);
  const installed = after.rows[targetIndex];
  invariant(installed?.pass === true, `finalized custom ${target.operation} policy did not reconcile exactly`);
  const [finalizedPolicy, finalizedSettings, finalizedPrevious, finalizedVoltr] =
    await connection.getMultipleAccountsInfo([
      new PublicKey(target.policy),
      new PublicKey(route.squads.settings),
      new PublicKey(route.previousBackyardVault),
      new PublicKey(route.vault.address),
    ], "finalized");
  invariant(finalizedPolicy != null, "finalized policy account is absent");
  invariant(finalizedSettings != null
    && sha256(finalizedSettings.data) === plan.transaction.projectedSettingsDataSha256,
  "finalized Settings bytes differ from the signed simulation projection");
  invariant(finalizedPrevious != null
    && sha256(finalizedPrevious.data) === plan.transaction.protectedPreviousVaultSha256,
  "protected Backyard vault changed across direct policy activation");
  invariant(finalizedVoltr != null
    && sha256(finalizedVoltr.data) === plan.transaction.protectedVoltrVaultSha256,
  "active Voltr vault changed across direct policy activation");
  writePrivate(journal, { ...plan, verdict: "FINALIZED_RECONCILED", signature: returned,
    finalizedContextSlot: confirmation.context.slot,
    finalizedPolicyDataSha256: sha256(finalizedPolicy.data), installed }, "wx");
  renameSync(`${journal}.pending`, `${journal}.sent-wire`);
  console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature: returned,
    finalizedContextSlot: confirmation.context.slot, installed, journal }, null, 2));
}

try {
  await main();
} catch (error) {
  console.error(JSON.stringify({
    verdict: "BLOCKED",
    blocker: error instanceof Error ? error.message.replace(process.env.SOLANA_RPC_URL ?? "", "<rpc>") : String(error),
  }));
  process.exitCode = 1;
}
