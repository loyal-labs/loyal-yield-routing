import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { getVaultDecoder } from "@voltr/vault-sdk";
import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE, rwaMultiplyRouteSpecSha256 } from "../domain/rwa-multiply-route-spec.js";
import { prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import {
  deriveRwaMultiplyStrategySigningMaterial,
  deriveRwaMultiplyVaultSigningMaterial,
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";
import { buildRwaMultiplyVoltrSetup, deriveRwaMultiplyVoltrAccounts } from "../integrations/rwa-multiply-voltr.js";

const PACKET_LIMIT = 1_232;
const MAX_COST_LAMPORTS = 10_000_000;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function customError(error: unknown): number | null {
  if (!error || typeof error !== "object" || !("InstructionError" in error)) return null;
  const detail = (error as { InstructionError?: unknown }).InstructionError;
  if (!Array.isArray(detail) || !detail[1] || typeof detail[1] !== "object") return null;
  const value = (detail[1] as { Custom?: unknown }).Custom;
  return typeof value === "number" ? value : null;
}

function writePrivate(path: string, value: Record<string, unknown>, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag, mode: 0o600 });
  chmodSync(path, 0o600);
}

async function readState(connection: Connection, minimumContextSlot?: number) {
  const route = RWA_MULTIPLY_ROUTE;
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const response = await connection.getMultipleAccountsInfoAndContext([
    new PublicKey(route.vault.address),
    new PublicKey(route.customAdaptor.strategyConfig),
    new PublicKey(accounts.adaptorAddReceipt),
    new PublicKey(route.previousBackyardVault),
  ], { commitment: "finalized", ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }) });
  const [vaultInfo, configInfo, receiptInfo, previousInfo] = response.value;
  invariant(vaultInfo?.owner.toBase58() === route.programs.voltr, "isolated Voltr vault is absent or inexact");
  invariant(configInfo?.owner.toBase58() === route.customAdaptor.program, "immutable adaptor config is absent or inexact");
  invariant(previousInfo?.owner.toBase58() === route.programs.voltr, "protected Backyard vault is absent or inexact");
  const vault = getVaultDecoder().decode(vaultInfo.data);
  invariant(vault.admin === route.setupAdmin && vault.allowAnyAdaptor === 0,
    "vault admin or allowAnyAdaptor boundary drifted");
  return {
    slot: response.context.slot,
    vault: { manager: vault.manager, admin: vault.admin, allowAnyAdaptor: vault.allowAnyAdaptor, dataSha256: sha256(vaultInfo.data) },
    config: { owner: configInfo.owner.toBase58(), dataSha256: sha256(configInfo.data) },
    receipt: receiptInfo ? { owner: receiptInfo.owner.toBase58(), dataSha256: sha256(receiptInfo.data) } : null,
    previous: { owner: previousInfo.owner.toBase58(), dataSha256: sha256(previousInfo.data) },
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
  invariant(!execute || (!existsSync(journal) && !existsSync(`${journal}.pending`)), "journal replay barrier exists");
  invariant(!reconcile || (!existsSync(journal) && existsSync(`${journal}.pending`)),
    "--reconcile requires one pending journal and no finalized journal");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const route = RWA_MULTIPLY_ROUTE;
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not mainnet-beta");
  const before = await readState(connection);
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  if (reconcile) {
    const pending = JSON.parse(readFileSync(`${journal}.pending`, "utf8")) as { transaction?: { expectedSignature?: unknown } };
    const signature = String(pending.transaction?.expectedSignature ?? "");
    const status = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    const landed = status.value[0];
    invariant(landed?.err === null && landed.confirmationStatus === "finalized", "pending add_adaptor signature is not finalized");
    invariant(before.receipt?.owner === route.programs.voltr, "finalized add_adaptor receipt is absent or inexact");
    invariant(before.previous.dataSha256 === route.previousBackyardVaultDataSha256, "protected Backyard vault changed");
    writePrivate(journal, { ...pending, verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: landed.slot, finalizedContextSlot: status.context.slot, state: before }, "wx");
    renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature, state: before, journal }, null, 2));
    return;
  }
  if (before.receipt) {
    invariant(before.receipt.owner === route.programs.voltr, "existing adaptor receipt owner drifted");
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, state: before }, null, 2));
    return;
  }
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "setup admin signer drifted");
  const setup = await buildRwaMultiplyVoltrSetup({
    admin: admin.signer,
    vault: (await deriveRwaMultiplyVaultSigningMaterial(admin)).signer,
    strategyConfig: (await deriveRwaMultiplyStrategySigningMaterial(admin)).signer,
  });
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    commitment: "finalized",
    minimumContextSlot: before.slot,
    instructions: [setup.instructions.addAdaptor],
    inspectedAddresses: [accounts.adaptorAddReceipt, route.vault.address, route.previousBackyardVault],
  });
  const errorCode = customError(prepared.simulation.err);
  if (errorCode === 6016) {
    console.log(JSON.stringify({
      schema: "loyal-rwa-multiply-custom-add-adaptor/v1",
      verdict: "BLOCKED_TEAM_ADMISSION",
      broadcast: false,
      routeSpecSha256: rwaMultiplyRouteSpecSha256(route),
      programId: route.customAdaptor.program,
      adaptorAddReceipt: accounts.adaptorAddReceipt,
      signedSimulation: { packetBytes: prepared.packetBytes, unitsConsumed: prepared.simulation.unitsConsumed,
        expectedSignature: prepared.expectedSignature, wireSha256: sha256(prepared.serializedTransaction), errorCode },
      resumeCondition: "Voltr makes the exact program pass its deployed add_adaptor gate with allowAnyAdaptor still 0; rerun this same command.",
    }, null, 2));
    return;
  }
  invariant(prepared.simulation.err === null, `signed add_adaptor simulation failed: ${JSON.stringify(prepared.simulation.err)}`);
  invariant(prepared.packetBytes <= PACKET_LIMIT, "add_adaptor packet exceeds Solana limit");
  const [postReceipt, postVault, postPrevious] = prepared.simulation.postAccounts;
  invariant(postReceipt?.owner === route.programs.voltr, "simulation did not create the exact adaptor receipt");
  invariant(postVault?.owner === route.programs.voltr, "simulation lost the isolated Voltr vault");
  invariant(postPrevious && sha256(postPrevious.data) === before.previous.dataSha256,
    "simulation changed the protected Backyard vault");
  const projectedCostLamports = postReceipt.lamports + prepared.feeLamports;
  invariant(projectedCostLamports <= MAX_COST_LAMPORTS, "add_adaptor projected cost exceeds bound");
  const plan = {
    schema: "loyal-rwa-multiply-custom-add-adaptor/v1",
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    routeSpecSha256: rwaMultiplyRouteSpecSha256(route),
    programId: route.customAdaptor.program,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    transaction: { packetBytes: prepared.packetBytes, unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports, projectedCostLamports,
      expectedSignature: prepared.expectedSignature, wireSha256: sha256(prepared.serializedTransaction) },
  };
  if (!execute) { console.log(JSON.stringify(plan, null, 2)); return; }
  writePrivate(`${journal}.pending`, { ...plan,
    signedWireBase64: Buffer.from(prepared.serializedTransaction).toString("base64") }, "wx");
  const returned = await connection.sendRawTransaction(prepared.serializedTransaction, {
    skipPreflight: false, preflightCommitment: "finalized", maxRetries: 0, minContextSlot: prepared.simulationSlot,
  });
  invariant(returned === prepared.expectedSignature, "RPC returned a different signature");
  const confirmation = await connection.confirmTransaction({ signature: returned, ...prepared.latestBlockhash }, "finalized");
  invariant(confirmation.value.err === null, `add_adaptor finalized with ${JSON.stringify(confirmation.value.err)}`);
  const after = await readState(connection, confirmation.context.slot);
  invariant(after.receipt?.owner === route.programs.voltr, "adaptor receipt did not reconcile");
  invariant(after.previous.dataSha256 === before.previous.dataSha256, "protected Backyard vault changed");
  writePrivate(journal, { ...plan, verdict: "FINALIZED_RECONCILED", signature: returned,
    finalizedContextSlot: confirmation.context.slot, state: after }, "wx");
  renameSync(`${journal}.pending`, `${journal}.sent-wire`);
  console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature: returned, state: after, journal }, null, 2));
}

try { await main(); } catch (error) {
  console.error(JSON.stringify({ verdict: "BLOCKED", blocker: error instanceof Error
    ? error.message.replace(process.env.SOLANA_RPC_URL ?? "", "<rpc>") : String(error) }));
  process.exitCode = 1;
}
