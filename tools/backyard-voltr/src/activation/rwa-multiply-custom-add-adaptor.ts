import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { Connection, PublicKey } from "@solana/web3.js";
import { getAdaptorAddReceiptDecoder, getVaultDecoder } from "@voltr/vault-sdk";

import {
  RWA_MULTIPLY_ROUTE,
  rwaMultiplyRouteSpecSha256,
} from "../domain/rwa-multiply-route-spec.js";
import { prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import {
  deriveRwaMultiplyStrategySigningMaterial,
  deriveRwaMultiplyVaultSigningMaterial,
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";
import {
  buildRwaMultiplyVoltrSetup,
  deriveRwaMultiplyVoltrAccounts,
} from "../integrations/rwa-multiply-voltr.js";
import { verifyPendingSignedWire } from "./pending-signed-wire.js";

type Json = Record<string, unknown>;
const PACKET_LIMIT = 1_232;
const MAX_SETUP_COST_LAMPORTS = 10_000_000;
const JOURNAL_SCHEMA = "loyal-rwa-multiply-custom-add-adaptor/v1";
const PHASE = "add_adaptor_receipt";

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function writePrivate(path: string, value: Json, flag: "w" | "wx") {
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
    new PublicKey(route.setupAdmin),
  ], {
    commitment: "finalized",
    ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }),
  });
  const [vaultInfo, configInfo, receiptInfo, previousInfo, adminInfo] = response.value;
  invariant(vaultInfo?.owner.toBase58() === route.programs.voltr,
    "isolated Voltr vault is absent or inexact");
  invariant(configInfo?.owner.toBase58() === route.customAdaptor.program,
    "immutable adaptor config is absent or inexact");
  invariant(previousInfo?.owner.toBase58() === route.programs.voltr,
    "protected Backyard vault is absent or inexact");
  invariant(adminInfo !== null && adminInfo !== undefined, "setup admin account is absent");
  const vault = getVaultDecoder().decode(vaultInfo.data);
  invariant(vault.admin === route.setupAdmin, "vault admin drifted");
  invariant(vault.manager === route.setupAdmin, "vault manager is no longer the setup admin");
  invariant(vault.allowAnyAdaptor === 1,
    "allowAnyAdaptor must remain enabled while the required receipt is created");
  const receipt = receiptInfo ? getAdaptorAddReceiptDecoder().decode(receiptInfo.data) : null;
  if (receiptInfo && receipt) {
    invariant(receiptInfo.owner.toBase58() === route.programs.voltr,
      "existing adaptor receipt is not Voltr-owned");
    invariant(receipt.vault === route.vault.address,
      "existing adaptor receipt is bound to another vault");
    invariant(receipt.adaptorProgram === route.customAdaptor.program,
      "existing adaptor receipt is bound to another adaptor");
  }
  return {
    slot: response.context.slot,
    vault: {
      owner: vaultInfo.owner.toBase58(),
      manager: vault.manager,
      admin: vault.admin,
      allowAnyAdaptor: vault.allowAnyAdaptor,
      dataSha256: sha256(vaultInfo.data),
    },
    config: { owner: configInfo.owner.toBase58(), dataSha256: sha256(configInfo.data) },
    receipt: receiptInfo && receipt ? {
      owner: receiptInfo.owner.toBase58(),
      vault: receipt.vault,
      adaptorProgram: receipt.adaptorProgram,
      version: receipt.version,
      bump: receipt.bump,
      dataSha256: sha256(receiptInfo.data),
    } : null,
    previous: { owner: previousInfo.owner.toBase58(), dataSha256: sha256(previousInfo.data) },
    adminLamports: adminInfo.lamports,
  };
}

function assertProjectedState(
  state: Awaited<ReturnType<typeof readState>>,
  transaction: Record<string, unknown>,
) {
  const route = RWA_MULTIPLY_ROUTE;
  invariant(state.vault.admin === route.setupAdmin && state.vault.manager === route.setupAdmin,
    "finalized vault authority bindings drifted");
  invariant(state.vault.allowAnyAdaptor === 1,
    "finalized receipt creation changed allowAnyAdaptor");
  invariant(state.receipt?.owner === route.programs.voltr,
    "finalized adaptor receipt is absent or not Voltr-owned");
  invariant(state.receipt.vault === route.vault.address,
    "finalized adaptor receipt vault binding drifted");
  invariant(state.receipt.adaptorProgram === route.customAdaptor.program,
    "finalized adaptor receipt program binding drifted");
  invariant(state.vault.dataSha256 === transaction.projectedVaultDataSha256,
    "finalized vault bytes differ from the signed simulation projection");
  invariant(state.config.dataSha256 === transaction.projectedConfigDataSha256,
    "finalized adaptor config bytes differ from the signed simulation projection");
  invariant(state.receipt.dataSha256 === transaction.projectedReceiptDataSha256,
    "finalized adaptor receipt bytes differ from the signed simulation projection");
  invariant(state.previous.dataSha256 === transaction.projectedPreviousVaultDataSha256,
    "protected Backyard vault bytes differ from the signed simulation projection");
}

async function finalizedState(connection: Connection, minimumContextSlot: number) {
  let lastError: unknown = null;
  for (let attempt = 0; attempt < 24; attempt += 1) {
    try {
      return await readState(connection, minimumContextSlot);
    } catch (error) {
      lastError = error;
      if (!String(error).includes("Minimum context slot has not been reached")) throw error;
      if (attempt < 23) await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
  throw lastError;
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
    "journal replay barrier exists");
  invariant(!reconcile || (!existsSync(journal) && existsSync(`${journal}.pending`)),
    "--reconcile requires one pending journal and no finalized journal");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const route = RWA_MULTIPLY_ROUTE;
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not mainnet-beta");

  if (reconcile) {
    const pending = verifyPendingSignedWire(
      JSON.parse(readFileSync(`${journal}.pending`, "utf8")) as unknown,
      JOURNAL_SCHEMA,
      [PHASE],
    );
    invariant(pending.record.routeSpecSha256 === rwaMultiplyRouteSpecSha256(route),
      "pending journal RouteSpec hash drifted");
    invariant(pending.record.programId === route.customAdaptor.program,
      "pending journal adaptor program drifted");
    const accounts = await deriveRwaMultiplyVoltrAccounts(route);
    invariant(pending.record.adaptorAddReceipt === accounts.adaptorAddReceipt,
      "pending journal adaptor receipt address drifted");
    const transaction = pending.record.transaction as Record<string, unknown>;
    const status = await connection.getSignatureStatuses([pending.expectedSignature], {
      searchTransactionHistory: true,
    });
    const landed = status.value[0];
    invariant(landed?.err === null && landed.confirmationStatus === "finalized" && landed.slot !== null,
      "pending add-adaptor signature is not finalized successfully");
    const after = await finalizedState(connection, landed.slot);
    assertProjectedState(after, transaction);
    writePrivate(journal, {
      ...pending.record,
      verdict: "FINALIZED_RECONCILED",
      signature: pending.expectedSignature,
      finalizedSlot: landed.slot,
      finalizedContextSlot: status.context.slot,
      after,
    }, "wx");
    renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({
      verdict: "FINALIZED_RECONCILED",
      signature: pending.expectedSignature,
      finalizedSlot: landed.slot,
      finalizedContextSlot: status.context.slot,
      journal,
      after,
    }, null, 2));
    return;
  }

  const before = await readState(connection);
  invariant(before.previous.dataSha256 === route.previousBackyardVaultDataSha256,
    "protected Backyard vault changed before add-adaptor");
  if (before.receipt) {
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, state: before }, null, 2));
    return;
  }

  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "setup admin signer drifted");
  const vaultSigner = await deriveRwaMultiplyVaultSigningMaterial(admin);
  const strategySigner = await deriveRwaMultiplyStrategySigningMaterial(admin);
  const setup = await buildRwaMultiplyVoltrSetup({
    admin: admin.signer,
    vault: vaultSigner.signer,
    strategyConfig: strategySigner.signer,
  });
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    commitment: "finalized",
    minimumContextSlot: before.slot,
    instructions: [setup.instructions.addAdaptor],
    inspectedAddresses: [
      accounts.adaptorAddReceipt,
      route.vault.address,
      route.customAdaptor.strategyConfig,
      route.previousBackyardVault,
      route.setupAdmin,
    ],
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT,
    "add-adaptor packet exceeds the Solana packet limit");
  invariant(prepared.simulation.err === null,
    `add-adaptor signed simulation failed: ${JSON.stringify({ err: prepared.simulation.err, logs: prepared.simulation.logs })}`);
  const [postReceiptInfo, postVaultInfo, postConfigInfo, postPreviousInfo, postAdminInfo] =
    prepared.simulation.postAccounts;
  invariant(postReceiptInfo?.owner === route.programs.voltr,
    "simulation did not create the exact Voltr adaptor receipt");
  const postReceipt = getAdaptorAddReceiptDecoder().decode(postReceiptInfo.data);
  invariant(postReceipt.vault === route.vault.address,
    "simulation created an adaptor receipt for another vault");
  invariant(postReceipt.adaptorProgram === route.customAdaptor.program,
    "simulation created an adaptor receipt for another adaptor");
  invariant(postVaultInfo?.owner === route.programs.voltr, "simulation lost the exact Voltr vault");
  invariant(sha256(postVaultInfo.data) === before.vault.dataSha256,
    "simulation changed vault bytes, including authority or allowAnyAdaptor state");
  invariant(postConfigInfo?.owner === route.customAdaptor.program,
    "simulation lost the immutable adaptor config");
  invariant(sha256(postConfigInfo.data) === before.config.dataSha256,
    "simulation changed immutable adaptor config bytes");
  invariant(postPreviousInfo?.owner === route.programs.voltr,
    "simulation lost the protected Backyard vault");
  invariant(sha256(postPreviousInfo.data) === before.previous.dataSha256,
    "simulation changed the protected Backyard vault");
  invariant(postAdminInfo !== null && postAdminInfo !== undefined,
    "simulation omitted the setup admin post-account");
  const projectedSetupCostLamports = before.adminLamports - postAdminInfo.lamports + prepared.feeLamports;
  invariant(projectedSetupCostLamports >= 0 && projectedSetupCostLamports <= MAX_SETUP_COST_LAMPORTS,
    `projected setup cost ${projectedSetupCostLamports} exceeds the ${MAX_SETUP_COST_LAMPORTS} lamport bound`);
  const plan = {
    schema: JOURNAL_SCHEMA,
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    phase: PHASE,
    routeSpecSha256: rwaMultiplyRouteSpecSha256(route),
    programId: route.customAdaptor.program,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    before,
    transaction: {
      packetBytes: prepared.packetBytes,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      projectedSetupCostLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      projectedReceiptDataSha256: sha256(postReceiptInfo.data),
      projectedVaultDataSha256: sha256(postVaultInfo.data),
      projectedConfigDataSha256: sha256(postConfigInfo.data),
      projectedPreviousVaultDataSha256: sha256(postPreviousInfo.data),
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
  let returned: string;
  try {
    returned = await connection.sendRawTransaction(prepared.serializedTransaction, {
      skipPreflight: false,
      preflightCommitment: "finalized",
      maxRetries: 0,
      minContextSlot: prepared.simulationSlot,
    });
  } catch (error) {
    const status = await connection.getSignatureStatus(prepared.expectedSignature, {
      searchTransactionHistory: true,
    });
    throw new Error(
      `single submission returned ambiguously; no retry was attempted; expected signature ${prepared.expectedSignature}; observed status ${JSON.stringify(status.value)}; original error ${String(error)}`,
    );
  }
  invariant(returned === prepared.expectedSignature,
    "RPC returned a signature different from the persisted wire");
  const confirmation = await connection.confirmTransaction({
    signature: returned,
    ...prepared.latestBlockhash,
  }, "finalized");
  invariant(confirmation.value.err === null,
    `add-adaptor transaction finalized with an error: ${JSON.stringify(confirmation.value.err)}`);
  const after = await finalizedState(connection, confirmation.context.slot);
  assertProjectedState(after, plan.transaction);
  writePrivate(journal, {
    ...plan,
    verdict: "FINALIZED_RECONCILED",
    signature: returned,
    finalizedContextSlot: confirmation.context.slot,
    after,
  }, "wx");
  renameSync(`${journal}.pending`, `${journal}.sent-wire`);
  console.log(JSON.stringify({
    verdict: "FINALIZED_RECONCILED",
    signature: returned,
    finalizedContextSlot: confirmation.context.slot,
    journal,
    after,
  }, null, 2));
}

try {
  await main();
} catch (error) {
  console.error(JSON.stringify({
    verdict: "BLOCKED",
    blocker: error instanceof Error
      ? error.message.replace(process.env.SOLANA_RPC_URL ?? "", "<rpc>")
      : String(error),
  }));
  process.exitCode = 1;
}
