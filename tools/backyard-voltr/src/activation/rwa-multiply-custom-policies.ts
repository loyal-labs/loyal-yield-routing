import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { AccountRole, address, type Instruction } from "@solana/kit";
import { Connection, PublicKey, SystemProgram } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  selectCustomPolicyMutation,
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

function assertAtomicReplacementInstruction(value: Instruction, target: CustomPolicyArtifact["policies"][number]) {
  const data = Buffer.from(value.data ?? []);
  const accounts = value.accounts ?? [];
  invariant(value.programAddress === RWA_MULTIPLY_ROUTE.squads.program
    && accounts.length === 6
    && accounts[0]?.address === RWA_MULTIPLY_ROUTE.squads.settings
    && accounts[0]?.role === AccountRole.WRITABLE
    && accounts[1]?.address === RWA_MULTIPLY_ROUTE.setupAdmin
    && accounts[1]?.role === AccountRole.WRITABLE_SIGNER
    && accounts[2]?.address === SystemProgram.programId.toBase58()
    && accounts[2]?.role === AccountRole.READONLY
    && accounts[3]?.address === RWA_MULTIPLY_ROUTE.squads.program
    && accounts[3]?.role === AccountRole.READONLY
    && accounts[4]?.address === RWA_MULTIPLY_ROUTE.setupAdmin
    && accounts[4]?.role === AccountRole.READONLY_SIGNER
    && accounts[5]?.address === target.policy
    && accounts[5]?.role === AccountRole.WRITABLE,
  "replacement escaped the exact Settings/policy authority boundary");
  invariant(data.length > 55 && data[8] === 1 && data.readUInt32LE(9) === 2
    && data[13] === 9
    && new PublicKey(data.subarray(14, 46)).toBase58() === target.policy
    && data[46] === 7
    && data.readBigUInt64LE(47) === BigInt(target.seed),
  "replacement is not exactly PolicyRemove then same-seed PolicyCreate");
}

function writePrivate(path: string, value: Record<string, unknown>, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, (_key, entry) => typeof entry === "bigint" ? entry.toString() : entry, 2)}\n`, {
    flag,
    mode: 0o600,
  });
  chmodSync(path, 0o600);
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
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not mainnet-beta");
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "setup admin signer drifted");

  const before = await verifyInstalledCustomPolicies(connection);
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
    };
    const signature = String(pending.transaction?.expectedSignature ?? "");
    const policyAddress = String(pending.policy ?? "");
    invariant(pending.mutation === "create" || pending.mutation === "replace",
      "pending journal lacks a valid policy mutation kind");
    invariant(signature.length > 0 && policyAddress.length > 0, "pending journal lacks signature or policy identity");
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
    invariant(finalizedPolicyDataSha256 === pending.transaction?.projectedPolicyDataSha256,
      "finalized policy bytes differ from the signed simulation projection");
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
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, policies: before.rows }, null, 2));
    return;
  }
  const target = mutation.target;
  const targetIndex = before.artifact.policies.findIndex(({ policy }) => policy === target.policy);
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
  if (mutation.kind === "create") {
    invariant(targetBefore === null, "custom policy appeared after finalized absence inspection");
  } else {
    invariant(targetBefore?.owner.toBase58() === route.squads.program,
      "custom policy disappeared or changed owner before replacement simulation");
    invariant(sha256(targetBefore.data) === mutation.row.dataSha256,
      "custom policy bytes changed after finalized replacement inspection");
  }
  const selectedInstruction = instruction(mutation.instruction);
  if (mutation.kind === "replace") assertAtomicReplacementInstruction(selectedInstruction, target);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    commitment: "finalized",
    minimumContextSlot: before.contextSlot,
    instructions: [selectedInstruction],
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
  if (mutation.kind === "replace") {
    invariant(sha256(postSettings.data) === settingsBeforeSha256,
      "same-seed atomic replacement changed Settings bytes");
  }
  invariant(postPrevious !== null && postPrevious !== undefined, "simulation omitted the protected Backyard vault");
  invariant(postVoltr !== null && postVoltr !== undefined, "simulation omitted the active Voltr vault");
  invariant(postAdmin !== null && postAdmin !== undefined, "simulation omitted the setup admin");
  const projectedRentDeltaLamports = Math.max(0, postPolicy.lamports - (targetBefore?.lamports ?? 0));
  const projectedCostLamports = projectedRentDeltaLamports + prepared.feeLamports;
  invariant(projectedCostLamports >= 0 && projectedCostLamports <= MAX_POLICY_COST_LAMPORTS,
    `projected custom policy cost ${projectedCostLamports} exceeds bound`);
  invariant(sha256(postPrevious.data) === protectedPreviousSha256,
    "simulation changed the protected Backyard vault");
  invariant(sha256(postVoltr.data) === protectedVoltrSha256,
    "simulation changed the active Voltr vault");
  const plan = {
    schema: "loyal-rwa-multiply-custom-policy-activation/v2",
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    routeSpecSha256: (await import("../domain/rwa-multiply-route-spec.js")).rwaMultiplyRouteSpecSha256(),
    sourceSha256: before.sourceSha256,
    mutation: mutation.kind,
    operation: target.operation,
    seed: target.seed,
    policy: target.policy,
    transaction: {
      packetBytes: prepared.packetBytes,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      projectedCostLamports,
      projectedRentDeltaLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      projectedPolicyDataSha256: sha256(postPolicy.data),
      projectedSettingsDataSha256: sha256(postSettings.data),
      previousPolicyDataSha256: targetBefore ? sha256(targetBefore.data) : null,
      previousSettingsDataSha256: settingsBeforeSha256,
      protectedPreviousVaultSha256: protectedPreviousSha256,
      protectedVoltrVaultSha256: protectedVoltrSha256,
    },
  };
  if (!execute) {
    console.log(JSON.stringify(plan, null, 2));
    return;
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
  const after = await verifyInstalledCustomPolicies(connection);
  const installed = after.rows[targetIndex];
  invariant(installed?.pass === true, `finalized custom ${target.operation} policy did not reconcile exactly`);
  const [finalizedPolicy, finalizedSettings, finalizedPrevious, finalizedVoltr] =
    await connection.getMultipleAccountsInfo([
      new PublicKey(target.policy),
      new PublicKey(route.squads.settings),
      new PublicKey(route.previousBackyardVault),
      new PublicKey(route.vault.address),
    ], "finalized");
  invariant(finalizedPolicy != null && sha256(finalizedPolicy.data) === plan.transaction.projectedPolicyDataSha256,
    "finalized policy bytes differ from the signed simulation projection");
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
