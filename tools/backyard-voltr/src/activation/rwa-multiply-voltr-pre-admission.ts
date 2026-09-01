import { createHash } from "node:crypto";
import { chmodSync, existsSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import {
  ComputeBudgetProgram,
  Connection,
  PublicKey,
  VersionedTransaction,
} from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { buildRwaMultiplyVoltrSetup } from "../integrations/rwa-multiply-voltr.js";
import {
  fromWeb3Instruction,
  prepareSignedV0Transaction,
} from "../integrations/solana-compat.js";
import {
  deriveRwaMultiplyStrategySigningMaterial,
  deriveRwaMultiplyVaultSigningMaterial,
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";

type Json = Record<string, unknown>;
const PACKET_LIMIT = 1_232;

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
  const response = await connection.getMultipleAccountsInfoAndContext([
    new PublicKey(route.vault.address),
    new PublicKey(route.customAdaptor.strategyConfig),
    new PublicKey(route.previousBackyardVault),
  ], {
    commitment: "finalized",
    ...(minContextSlot === undefined ? {} : { minContextSlot }),
  });
  const [vault, config, previous] = response.value;
  invariant(previous?.owner.toBase58() === route.programs.voltr, "previous Backyard vault is absent or no longer Voltr-owned");
  if (vault) invariant(vault.owner.toBase58() === route.programs.voltr, "new vault owner drifted");
  if (config) invariant(config.owner.toBase58() === route.customAdaptor.program, "strategy config owner drifted");
  invariant(!config || vault, "strategy config exists without its fixed Voltr vault");
  return {
    slot: response.context.slot,
    vault: vault ? { owner: vault.owner.toBase58(), dataSha256: sha256(vault.data) } : null,
    config: config ? { owner: config.owner.toBase58(), dataSha256: sha256(config.data) } : null,
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

async function main() {
  const execute = process.argv.includes("--execute");
  const journalIndex = process.argv.indexOf("--journal");
  const journal = journalIndex >= 0 ? resolve(process.argv[journalIndex + 1] ?? "") : "";
  invariant(!execute || process.env.CONFIRM_MAINNET === "1", "--execute requires CONFIRM_MAINNET=1");
  invariant(!execute || (journal.endsWith(".json") && existsSync(dirname(journal))), "--execute requires --journal PATH");
  invariant(!execute || (!existsSync(journal) && !existsSync(`${journal}.pending`)), "journal replay barrier exists");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === RWA_MULTIPLY_ROUTE.setupAdmin, "setup admin signer drifted");
  const vault = await deriveRwaMultiplyVaultSigningMaterial(admin);
  const strategy = await deriveRwaMultiplyStrategySigningMaterial(admin);
  const before = await readState(connection);
  if (before.vault && before.config) {
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_FINALIZED", broadcast: false, state: before }, null, 2));
    return;
  }
  const setup = await buildRwaMultiplyVoltrSetup({
    admin: admin.signer,
    vault: vault.signer,
    strategyConfig: strategy.signer,
  });
  const phase = before.vault ? "initialize_config" : "initialize_vault";
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    additionalSigners: phase === "initialize_vault" ? [vault] : [strategy],
    commitment: "confirmed",
    instructions: [
      fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitLimit({ units: 150_000 })),
      fromWeb3Instruction(ComputeBudgetProgram.setComputeUnitPrice({ microLamports: 50_000 })),
      phase === "initialize_vault"
        ? setup.instructions.initializeVault
        : setup.instructions.initializeConfig,
    ],
    inspectedAddresses: [
      RWA_MULTIPLY_ROUTE.vault.address,
      RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig,
      RWA_MULTIPLY_ROUTE.previousBackyardVault,
    ],
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT, "pre-admission packet exceeds the Solana packet limit");
  invariant(prepared.simulation.err === null, `pre-admission signed simulation failed: ${JSON.stringify(prepared.simulation.err)}`);
  const [projectedVault, projectedConfig, projectedPrevious] = prepared.simulation.postAccounts;
  invariant(projectedVault?.owner === RWA_MULTIPLY_ROUTE.programs.voltr, "simulation did not retain/create the exact Voltr vault");
  if (phase === "initialize_vault") {
    invariant(projectedConfig == null, "vault-initialization simulation unexpectedly created the strategy config");
  } else {
    invariant(projectedConfig?.owner === RWA_MULTIPLY_ROUTE.customAdaptor.program, "simulation did not create the exact immutable strategy config");
  }
  invariant(projectedPrevious != null && sha256(projectedPrevious.data) === before.previous.dataSha256, "simulation changed the previous Backyard vault");
  const plan = {
    schema: "loyal-rwa-multiply-voltr-pre-admission/v1",
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
  invariant(after.vault !== null, "new Voltr vault is absent after finalization");
  if (phase === "initialize_config") invariant(after.config !== null, "strategy config is absent after finalization");
  invariant(after.previous.dataSha256 === before.previous.dataSha256, "previous Backyard vault changed during pre-admission setup");
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
