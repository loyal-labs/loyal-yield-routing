import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { AccountLayout, TOKEN_PROGRAM_ID } from "@solana/spl-token";
import {
  ComputeBudgetProgram,
  Connection,
  PublicKey,
} from "@solana/web3.js";
import {
  getStrategyInitReceiptDecoder,
  getVaultDecoder,
} from "@voltr/vault-sdk";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  fromWeb3Instruction,
  prepareSignedV0Transaction,
} from "../integrations/solana-compat.js";
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
const MAX_SETUP_COST_LAMPORTS = 50_000_000;
const JOURNAL_SCHEMA = "loyal-rwa-multiply-voltr-post-admission/v1";
const PHASES = ["handoff_manager", "initialize_strategy_and_handoff"] as const;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function writePrivate(path: string, value: Json, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, {
    flag,
    mode: 0o600,
  });
  chmodSync(path, 0o600);
}

async function readState(
  connection: Connection,
  minimumContextSlot?: number,
) {
  const route = RWA_MULTIPLY_ROUTE;
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const response = await connection.getMultipleAccountsInfoAndContext([
    new PublicKey(route.vault.address),
    new PublicKey(route.customAdaptor.strategyConfig),
    new PublicKey(accounts.adaptorAddReceipt),
    new PublicKey(accounts.strategyInitReceipt),
    new PublicKey(accounts.strategyAssetAta),
    new PublicKey(route.previousBackyardVault),
    new PublicKey(route.setupAdmin),
  ], {
    commitment: "finalized",
    ...(minimumContextSlot === undefined ? {} : { minContextSlot: minimumContextSlot }),
  });
  const [vaultInfo, configInfo, adaptorReceiptInfo, strategyReceiptInfo,
    strategyAssetInfo, previousInfo, adminInfo] = response.value;
  invariant(vaultInfo !== null && vaultInfo !== undefined, "new vault is absent");
  invariant(vaultInfo.owner.equals(new PublicKey(route.programs.voltr)), "new vault is not Voltr-owned");
  invariant(configInfo !== null && configInfo !== undefined, "immutable strategy config is absent");
  invariant(configInfo.owner.equals(new PublicKey(route.customAdaptor.program)), "immutable strategy config has the wrong owner");
  invariant(previousInfo !== null && previousInfo !== undefined, "previous Backyard vault is absent");
  invariant(previousInfo.owner.equals(new PublicKey(route.programs.voltr)), "previous Backyard vault is no longer Voltr-owned");
  invariant(adminInfo !== null && adminInfo !== undefined, "setup admin account is absent");
  const vault = getVaultDecoder().decode(vaultInfo.data);
  const receipt = strategyReceiptInfo
    ? getStrategyInitReceiptDecoder().decode(strategyReceiptInfo.data)
    : null;
  const strategyAsset = strategyAssetInfo
    ? AccountLayout.decode(strategyAssetInfo.data)
    : null;
  return {
    slot: response.context.slot,
    vault: {
      owner: vaultInfo.owner.toBase58(),
      manager: vault.manager,
      admin: vault.admin,
      allowAnyAdaptor: vault.allowAnyAdaptor,
      dataSha256: sha256(vaultInfo.data),
    },
    config: {
      owner: configInfo.owner.toBase58(),
      dataSha256: sha256(configInfo.data),
    },
    adaptorReceipt: adaptorReceiptInfo
      ? { owner: adaptorReceiptInfo.owner.toBase58(), dataSha256: sha256(adaptorReceiptInfo.data) }
      : null,
    strategyReceipt: strategyReceiptInfo && receipt
      ? {
          owner: strategyReceiptInfo.owner.toBase58(),
          vault: receipt.vault,
          strategy: receipt.strategy,
          adaptorProgram: receipt.adaptorProgram,
          positionValue: receipt.positionValue.toString(),
          dataSha256: sha256(strategyReceiptInfo.data),
        }
      : null,
    strategyAsset: strategyAssetInfo && strategyAsset
      ? {
          owner: strategyAssetInfo.owner.toBase58(),
          mint: new PublicKey(strategyAsset.mint).toBase58(),
          authority: new PublicKey(strategyAsset.owner).toBase58(),
          amountRaw: strategyAsset.amount.toString(),
          dataSha256: sha256(strategyAssetInfo.data),
        }
      : null,
    previous: {
      owner: previousInfo.owner.toBase58(),
      dataSha256: sha256(previousInfo.data),
    },
    adminLamports: adminInfo.lamports,
  };
}

function assertFinalState(state: Awaited<ReturnType<typeof readState>>) {
  const route = RWA_MULTIPLY_ROUTE;
  invariant(state.vault.manager === route.squads.vault, "vault manager is not the fixed Loyal Squads Smart Account vault");
  invariant(state.vault.admin === route.setupAdmin, "vault admin drifted during manager handoff");
  invariant(state.vault.allowAnyAdaptor === 1, "allowAnyAdaptor is not enabled on the approved vault");
  invariant(state.adaptorReceipt === null || state.adaptorReceipt.owner === route.programs.voltr,
    "optional custom adaptor receipt has the wrong owner");
  invariant(state.strategyReceipt?.owner === route.programs.voltr, "strategy receipt is absent or has the wrong owner");
  invariant(state.strategyReceipt.vault === route.vault.address, "strategy receipt vault identity drifted");
  invariant(state.strategyReceipt.strategy === route.customAdaptor.strategyConfig, "strategy receipt strategy identity drifted");
  invariant(state.strategyReceipt.adaptorProgram === route.customAdaptor.program, "strategy receipt adaptor identity drifted");
  invariant(state.strategyReceipt.positionValue === "0", "new strategy receipt does not start at zero NAV");
  invariant(state.strategyAsset?.owner === TOKEN_PROGRAM_ID.toBase58(), "strategy USDC ATA is absent or not token-program-owned");
  invariant(state.strategyAsset.mint === route.assets.assetMint, "strategy ATA mint drifted");
  invariant(state.strategyAsset.amountRaw === "0", "strategy ATA is not empty after initialization");
}

function assertProjectedFinalState(
  state: Awaited<ReturnType<typeof readState>>,
  transaction: Record<string, unknown>,
) {
  assertFinalState(state);
  // Voltr writes slot-derived bookkeeping into the vault and strategy receipt,
  // so their full bytes legitimately differ between simulation and execution.
  // `assertFinalState` independently decodes and pins every security-relevant
  // field; keep byte equality only for accounts whose data is deterministic.
  invariant(state.config.dataSha256 === transaction.projectedConfigDataSha256,
    "immutable strategy config changed across manager handoff");
  invariant((state.adaptorReceipt?.dataSha256 ?? null) === transaction.projectedAdaptorReceiptDataSha256,
    "optional custom adaptor receipt changed across manager handoff");
  invariant(state.strategyAsset?.dataSha256 === transaction.projectedStrategyAssetDataSha256,
    "finalized strategy ATA bytes differ from the signed simulation projection");
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
  invariant(!execute || (!existsSync(journal) && !existsSync(`${journal}.pending`)), "journal replay barrier exists");
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
      PHASES,
    );
    const transaction = pending.record.transaction as Record<string, unknown>;
    const status = await connection.getSignatureStatuses([pending.expectedSignature], {
      searchTransactionHistory: true,
    });
    const landed = status.value[0];
    invariant(landed?.err === null && landed.confirmationStatus === "finalized" && landed.slot !== null,
      "pending post-admission signature is not finalized successfully");
    const after = await finalizedState(connection, landed.slot);
    assertProjectedFinalState(after, transaction);
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
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "setup admin signer drifted");
  const before = await readState(connection);
  invariant(before.vault.allowAnyAdaptor === 1, "approved vault no longer permits the exact custom adaptor");
  invariant(before.adaptorReceipt === null || before.adaptorReceipt.owner === route.programs.voltr,
    "optional adaptor receipt owner drifted");
  if (before.strategyReceipt && before.vault.manager === route.squads.vault) {
    assertFinalState(before);
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, state: before }, null, 2));
    return;
  }
  invariant(before.vault.manager === route.setupAdmin, "vault manager is neither the setup admin nor the fixed Loyal Squads Smart Account vault");
  const vaultSigner = await deriveRwaMultiplyVaultSigningMaterial(admin);
  const strategySigner = await deriveRwaMultiplyStrategySigningMaterial(admin);
  const setup = await buildRwaMultiplyVoltrSetup({
    admin: admin.signer,
    vault: vaultSigner.signer,
    strategyConfig: strategySigner.signer,
  });
  const phase = before.strategyReceipt ? "handoff_manager" : "initialize_strategy_and_handoff";
  const instructions = [
    fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitLimit({ units: 300_000 })),
    fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitPrice({ microLamports: 50_000 })),
    setup.instructions.createStrategyAssetAta,
    ...(before.strategyReceipt ? [] : [setup.instructions.initializeStrategy]),
    setup.instructions.handoffManager,
  ];
  const accountList = await deriveRwaMultiplyVoltrAccounts(route);
  const inspectedAddresses = [
    route.vault.address,
    accountList.strategyInitReceipt,
    accountList.strategyAssetAta,
    route.previousBackyardVault,
    route.setupAdmin,
  ];
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    commitment: "finalized",
    minimumContextSlot: before.slot,
    instructions,
    inspectedAddresses,
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT, "post-admission packet exceeds the Solana packet limit");
  if (prepared.simulation.err !== null) {
    const diagnosticResponse = await fetch(rpcUrl, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "simulateTransaction",
        params: [Buffer.from(prepared.serializedTransaction).toString("base64"), {
          commitment: "finalized",
          encoding: "base64",
          innerInstructions: true,
          minContextSlot: prepared.simulationSlot,
          replaceRecentBlockhash: false,
          sigVerify: true,
        }],
      }),
    });
    const diagnostic = await diagnosticResponse.json() as { result?: { value?: { innerInstructions?: unknown } } };
    throw new Error(`post-admission signed simulation failed: ${JSON.stringify({
      err: prepared.simulation.err,
      logs: prepared.simulation.logs,
      innerInstructions: diagnostic.result?.value?.innerInstructions ?? null,
    })}`);
  }
  const [postVaultInfo, postReceiptInfo, postStrategyAssetInfo, postPreviousInfo, postAdminInfo] = prepared.simulation.postAccounts;
  invariant(postVaultInfo?.owner === route.programs.voltr, "simulation lost the exact Voltr vault");
  const postVault = getVaultDecoder().decode(postVaultInfo.data);
  invariant(postVault.manager === route.squads.vault, "simulation did not hand manager control to the Loyal Squads Smart Account vault");
  invariant(postVault.admin === route.setupAdmin && postVault.allowAnyAdaptor === 1, "simulation changed vault admin or adaptor policy");
  invariant(postReceiptInfo?.owner === route.programs.voltr, "simulation did not retain/create the exact strategy receipt");
  const postReceipt = getStrategyInitReceiptDecoder().decode(postReceiptInfo.data);
  invariant(postReceipt.vault === route.vault.address
    && postReceipt.strategy === route.customAdaptor.strategyConfig
    && postReceipt.adaptorProgram === route.customAdaptor.program
    && postReceipt.positionValue === 0n,
  "simulation created an inexact strategy receipt");
  invariant(postStrategyAssetInfo?.owner === route.assets.tokenProgram, "simulation did not retain/create the strategy USDC ATA");
  const postStrategyAsset = AccountLayout.decode(postStrategyAssetInfo.data);
  invariant(new PublicKey(postStrategyAsset.mint).toBase58() === route.assets.assetMint
    && postStrategyAsset.amount === 0n,
  "simulation created an inexact or nonempty strategy USDC ATA");
  invariant(postPreviousInfo !== null && postPreviousInfo !== undefined, "simulation omitted the previous Backyard vault");
  invariant(sha256(postPreviousInfo.data) === before.previous.dataSha256, "simulation changed the previous Backyard vault");
  invariant(postAdminInfo !== null && postAdminInfo !== undefined, "simulation omitted the setup admin post-account");
  const projectedSetupCostLamports = before.adminLamports - postAdminInfo.lamports;
  invariant(projectedSetupCostLamports >= 0 && projectedSetupCostLamports <= MAX_SETUP_COST_LAMPORTS,
    `projected setup cost ${projectedSetupCostLamports} exceeds the ${MAX_SETUP_COST_LAMPORTS} lamport bound`);
  const plan = {
    schema: JOURNAL_SCHEMA,
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    phase,
    before,
    transaction: {
      packetBytes: prepared.packetBytes,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      projectedSetupCostLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      previousVaultSha256: before.previous.dataSha256,
      projectedVaultDataSha256: sha256(postVaultInfo.data),
      projectedConfigDataSha256: before.config.dataSha256,
      projectedAdaptorReceiptDataSha256: before.adaptorReceipt?.dataSha256 ?? null,
      projectedStrategyReceiptDataSha256: sha256(postReceiptInfo.data),
      projectedStrategyAssetDataSha256: sha256(postStrategyAssetInfo.data),
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
  invariant(returned === prepared.expectedSignature, "RPC returned a signature different from the persisted wire");
  const confirmation = await connection.confirmTransaction({
    signature: returned,
    ...prepared.latestBlockhash,
  }, "finalized");
  invariant(confirmation.value.err === null, `post-admission transaction finalized with an error: ${JSON.stringify(confirmation.value.err)}`);
  const after = await finalizedState(connection, confirmation.context.slot);
  assertProjectedFinalState(after, plan.transaction);
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
