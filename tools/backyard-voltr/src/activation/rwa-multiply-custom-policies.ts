import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { AccountRole, address, type Instruction } from "@solana/kit";
import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { verifyInstalledCustomPolicies, type CustomPolicyArtifact } from "../policies/rwa-multiply-custom.js";

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
      seed?: unknown;
      policy?: unknown;
      transaction?: { expectedSignature?: unknown; projectedPolicyDataSha256?: unknown; protectedPreviousVaultSha256?: unknown };
    };
    const signature = String(pending.transaction?.expectedSignature ?? "");
    const policyAddress = String(pending.policy ?? "");
    invariant(signature.length > 0 && policyAddress.length > 0, "pending journal lacks signature or policy identity");
    const status = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    const landed = status.value[0];
    invariant(landed?.err === null && landed.confirmationStatus === "finalized", "pending signature is not finalized successfully");
    const installed = before.rows.find((row) => row.policy === policyAddress);
    invariant(installed?.pass === true && installed.operation === pending.operation && installed.seed === String(pending.seed),
      "pending policy does not decode to the exact finalized contract");
    const [policyInfo, protectedPrevious] = await connection.getMultipleAccountsInfo([
      new PublicKey(policyAddress),
      new PublicKey(route.previousBackyardVault),
    ], "finalized");
    invariant(policyInfo != null, "finalized policy account is absent");
    const finalizedPolicyDataSha256 = sha256(policyInfo.data);
    invariant(protectedPrevious && sha256(protectedPrevious.data) === pending.transaction?.protectedPreviousVaultSha256,
      "protected Backyard vault changed across policy activation");
    writePrivate(journal, { ...pending, verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: landed.slot, finalizedContextSlot: status.context.slot,
      finalizedPolicyDataSha256,
      note: "Squads policy account bytes include bank-dependent fields; exact decoded semantics and the signed wire are authoritative.",
      installed }, "wx");
    renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: landed.slot, finalizedContextSlot: status.context.slot,
      finalizedPolicyDataSha256, installed, journal }, null, 2));
    return;
  }
  const badExisting = before.rows.find((row) => row.reason !== "absent" && !row.pass);
  invariant(!badExisting, `existing custom policy ${badExisting?.operation ?? "unknown"} is inexact`);
  const firstAbsent = before.rows.findIndex((row) => !row.pass);
  if (firstAbsent < 0) {
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, policies: before.rows }, null, 2));
    return;
  }
  invariant(before.rows.slice(0, firstAbsent).every(({ pass }) => pass), "custom policy prefix is not exact");
  invariant(before.rows.slice(firstAbsent).every(({ reason }) => reason === "absent"), "custom policy installation has a gap");
  const target = before.artifact.policies[firstAbsent]!;
  invariant(BigInt(target.seed) === before.policySeedBefore + 1n,
    `next custom policy seed ${target.seed} does not follow finalized Settings seed ${before.policySeedBefore}`);
  const protectedPrevious = await connection.getAccountInfo(new PublicKey(route.previousBackyardVault), "finalized");
  invariant(protectedPrevious?.owner.toBase58() === route.programs.voltr, "protected Backyard vault is absent or inexact");
  const protectedPreviousSha256 = sha256(protectedPrevious.data);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    commitment: "finalized",
    minimumContextSlot: before.contextSlot,
    instructions: [instruction(target.createInstruction)],
    inspectedAddresses: [target.policy, route.squads.settings, route.previousBackyardVault, route.setupAdmin],
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT, `custom ${target.operation} policy packet exceeds ${PACKET_LIMIT} bytes`);
  invariant(prepared.simulation.err === null,
    `custom ${target.operation} policy simulation failed: ${JSON.stringify(prepared.simulation.err)}`);
  const [postPolicy, postSettings, postPrevious, postAdmin] = prepared.simulation.postAccounts;
  invariant(postPolicy?.owner === route.squads.program, "simulation did not create the exact policy account");
  invariant(postSettings?.owner === route.squads.program, "simulation changed Settings ownership");
  invariant(postPrevious !== null && postPrevious !== undefined, "simulation omitted the protected Backyard vault");
  invariant(postAdmin !== null && postAdmin !== undefined, "simulation omitted the setup admin");
  const projectedCostLamports = postPolicy.lamports + prepared.feeLamports;
  invariant(projectedCostLamports >= 0 && projectedCostLamports <= MAX_POLICY_COST_LAMPORTS,
    `projected custom policy cost ${projectedCostLamports} exceeds bound`);
  invariant(sha256(postPrevious.data) === protectedPreviousSha256,
    "simulation changed the protected Backyard vault");
  const plan = {
    schema: "loyal-rwa-multiply-custom-policy-activation/v1",
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    routeSpecSha256: (await import("../domain/rwa-multiply-route-spec.js")).rwaMultiplyRouteSpecSha256(),
    sourceSha256: before.sourceSha256,
    operation: target.operation,
    seed: target.seed,
    policy: target.policy,
    transaction: {
      packetBytes: prepared.packetBytes,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      projectedCostLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      projectedPolicyDataSha256: sha256(postPolicy.data),
      protectedPreviousVaultSha256: protectedPreviousSha256,
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
  const installed = after.rows[firstAbsent];
  invariant(installed?.pass === true, `finalized custom ${target.operation} policy did not reconcile exactly`);
  writePrivate(journal, { ...plan, verdict: "FINALIZED_RECONCILED", signature: returned,
    finalizedContextSlot: confirmation.context.slot, installed }, "wx");
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
