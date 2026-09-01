import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import {
  ComputeBudgetProgram,
  Connection,
  PublicKey,
  VersionedTransaction,
} from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  buildRwaMultiplyVoltrSetup,
  deriveRwaMultiplyVoltrAccounts,
} from "../integrations/rwa-multiply-voltr.js";
import {
  fromWeb3Instruction,
  prepareSignedV0Transaction,
} from "../integrations/solana-compat.js";
import {
  deriveRwaMultiplyStrategySigningMaterial,
  deriveRwaMultiplyVaultSigningMaterial,
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";
import { verifyPendingSignedWire } from "./pending-signed-wire.js";

type Json = Record<string, unknown>;
const PACKET_LIMIT = 1_232;
const JOURNAL_SCHEMA = "loyal-rwa-multiply-voltr-pre-admission/v2";
const PHASES = ["initialize_vault", "initialize_config", "initialize_ticket"] as const;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function writePrivate(path: string, value: Json, flag: "w" | "wx") {
  const content = `${JSON.stringify(value, null, 2)}\n`;
  writeFileSync(path, content, { flag, mode: 0o600 });
  chmodSync(path, 0o600);
}

async function readState(connection: Connection, minContextSlot?: number) {
  const route = RWA_MULTIPLY_ROUTE;
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const response = await connection.getMultipleAccountsInfoAndContext([
    new PublicKey(route.vault.address),
    new PublicKey(route.customAdaptor.strategyConfig),
    new PublicKey(accounts.reportTicket),
    new PublicKey(route.previousBackyardVault),
  ], {
    commitment: "finalized",
    ...(minContextSlot === undefined ? {} : { minContextSlot }),
  });
  const [vault, config, ticket, previous] = response.value;
  invariant(previous?.owner.toBase58() === route.programs.voltr, "previous Backyard vault is absent or no longer Voltr-owned");
  if (vault) invariant(vault.owner.toBase58() === route.programs.voltr, "new vault owner drifted");
  if (config) invariant(config.owner.toBase58() === route.customAdaptor.program, "strategy config owner drifted");
  if (ticket) invariant(ticket.owner.toBase58() === route.customAdaptor.program
    && ticket.data.length === 96
    && ticket.data.subarray(0, 8).equals(Buffer.from([245, 104, 182, 197, 58, 231, 116, 237]))
    && ticket.data[8] === 1 && ticket.data[9] === 254
    && new PublicKey(ticket.data.subarray(16, 48)).toBase58() === route.customAdaptor.strategyConfig,
  "report ticket identity or immutable config binding drifted");
  invariant(!config || vault, "strategy config exists without its fixed Voltr vault");
  invariant(!ticket || config, "report ticket exists without its fixed strategy config");
  return {
    slot: response.context.slot,
    vault: vault ? { owner: vault.owner.toBase58(), dataSha256: sha256(vault.data) } : null,
    config: config ? { owner: config.owner.toBase58(), dataSha256: sha256(config.data) } : null,
    ticket: ticket ? { owner: ticket.owner.toBase58(), dataSha256: sha256(ticket.data) } : null,
    previous: { owner: previous.owner.toBase58(), dataSha256: sha256(previous.data) },
  };
}

async function finalizedState(connection: Connection, minContextSlot: number) {
  let error: unknown = null;
  for (let attempt = 0; attempt < 24; attempt += 1) {
    try {
      return await readState(connection, minContextSlot);
    } catch (caught) {
      error = caught;
      if (!String(caught).includes("Minimum context slot has not been reached")) throw caught;
      if (attempt < 23) await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
  throw error;
}

function assertProjectedState(
  state: Awaited<ReturnType<typeof readState>>,
  phase: string,
  transaction: Record<string, unknown>,
) {
  invariant(state.vault?.dataSha256 === transaction.projectedVaultDataSha256,
    "finalized vault bytes differ from the signed simulation projection");
  if (phase === "initialize_vault") {
    invariant(transaction.projectedConfigDataSha256 === null && state.config === null,
      "vault-only activation unexpectedly has a strategy config");
    invariant(transaction.projectedTicketDataSha256 === null && state.ticket === null,
      "vault-only activation unexpectedly has a report ticket");
  } else if (phase === "initialize_config") {
    invariant(typeof transaction.projectedConfigDataSha256 === "string"
      && state.config?.dataSha256 === transaction.projectedConfigDataSha256,
    "finalized strategy config bytes differ from the signed simulation projection");
    invariant(transaction.projectedTicketDataSha256 === null && state.ticket === null,
      "config activation unexpectedly has a report ticket");
  } else {
    invariant(typeof transaction.projectedConfigDataSha256 === "string"
      && state.config?.dataSha256 === transaction.projectedConfigDataSha256,
    "ticket activation changed the immutable strategy config");
    invariant(typeof transaction.projectedTicketDataSha256 === "string"
      && state.ticket?.dataSha256 === transaction.projectedTicketDataSha256,
    "finalized report ticket bytes differ from the signed simulation projection");
  }
  invariant(state.previous.dataSha256 === transaction.projectedPreviousVaultDataSha256,
    "protected Backyard vault bytes differ from the signed simulation projection");
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
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
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
      "pending pre-admission signature is not finalized successfully");
    const after = await finalizedState(connection, landed.slot);
    assertProjectedState(after, pending.phase, transaction);
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
  invariant(admin.signer.address === RWA_MULTIPLY_ROUTE.setupAdmin, "setup admin signer drifted");
  const vault = await deriveRwaMultiplyVaultSigningMaterial(admin);
  const strategy = await deriveRwaMultiplyStrategySigningMaterial(admin);
  const before = await readState(connection);
  if (before.vault && before.config && before.ticket) {
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, state: before }, null, 2));
    return;
  }
  const setup = await buildRwaMultiplyVoltrSetup({
    admin: admin.signer,
    vault: vault.signer,
    strategyConfig: strategy.signer,
  });
  const phase = !before.vault ? "initialize_vault"
    : !before.config ? "initialize_config"
      : "initialize_ticket";
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    additionalSigners: phase === "initialize_vault" ? [vault]
      : phase === "initialize_config" ? [strategy] : [],
    commitment: "confirmed",
    instructions: [
      fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitLimit({ units: 150_000 })),
      fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitPrice({ microLamports: 50_000 })),
      phase === "initialize_vault"
        ? setup.instructions.initializeVault
        : phase === "initialize_config"
          ? setup.instructions.initializeConfig
          : setup.instructions.initializeReportTicket,
    ],
    inspectedAddresses: [
      RWA_MULTIPLY_ROUTE.vault.address,
      RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig,
      setup.accounts.reportTicket,
      RWA_MULTIPLY_ROUTE.previousBackyardVault,
    ],
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT, "pre-admission packet exceeds the Solana packet limit");
  invariant(prepared.simulation.err === null, `pre-admission signed simulation failed: ${JSON.stringify(prepared.simulation.err)}`);
  const [projectedVault, projectedConfig, projectedTicket, projectedPrevious] = prepared.simulation.postAccounts;
  invariant(projectedVault?.owner === RWA_MULTIPLY_ROUTE.programs.voltr, "simulation did not retain/create the exact Voltr vault");
  if (phase === "initialize_vault") {
    invariant(projectedConfig == null, "vault-initialization simulation unexpectedly created the strategy config");
    invariant(projectedTicket == null, "vault-initialization simulation unexpectedly created the report ticket");
  } else if (phase === "initialize_config") {
    invariant(projectedConfig?.owner === RWA_MULTIPLY_ROUTE.customAdaptor.program, "simulation did not create the exact immutable strategy config");
    invariant(projectedTicket == null, "config-initialization simulation unexpectedly created the report ticket");
  } else {
    invariant(projectedConfig?.owner === RWA_MULTIPLY_ROUTE.customAdaptor.program,
      "ticket initialization changed or removed the immutable strategy config");
    invariant(projectedTicket?.owner === RWA_MULTIPLY_ROUTE.customAdaptor.program
      && projectedTicket.data.length === 96,
    "simulation did not create the exact report ticket");
  }
  invariant(projectedPrevious != null && sha256(projectedPrevious.data) === before.previous.dataSha256, "simulation changed the previous Backyard vault");
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
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      previousVaultSha256: before.previous.dataSha256,
      projectedVaultDataSha256: sha256(projectedVault.data),
      projectedConfigDataSha256: projectedConfig ? sha256(projectedConfig.data) : null,
      projectedTicketDataSha256: projectedTicket ? sha256(projectedTicket.data) : null,
      projectedPreviousVaultDataSha256: sha256(projectedPrevious.data),
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
    skipPreflight: true,
    maxRetries: 3,
  });
  invariant(returned === prepared.expectedSignature, "RPC returned a signature different from the persisted wire");
  const confirmation = await connection.confirmTransaction({
    signature: returned,
    ...prepared.latestBlockhash,
  }, "finalized");
  invariant(confirmation.value.err === null, `pre-admission transaction finalized with an error: ${JSON.stringify(confirmation.value.err)}`);
  const after = await finalizedState(connection, confirmation.context.slot);
  assertProjectedState(after, phase, plan.transaction);
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
    vault: after.vault,
    config: after.config,
    previousVaultSha256: after.previous.dataSha256,
  }, null, 2));
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
