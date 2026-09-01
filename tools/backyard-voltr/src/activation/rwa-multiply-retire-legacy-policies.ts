import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, PublicKey, VersionedTransaction } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { deriveRwaMultiplyVoltrAccounts } from "../integrations/rwa-multiply-voltr.js";
import {
  buildLegacyCustomPolicyRetirementInstruction,
  LEGACY_CUSTOM_POLICY_ADDRESSES,
  LEGACY_CUSTOM_POLICY_DATA_SHA256,
  LEGACY_CUSTOM_POLICY_SEEDS,
  REPLACEMENT_CUSTOM_POLICY_IDENTITIES,
} from "../policies/rwa-multiply-legacy-retirement.js";
import { verifyInstalledCustomPolicies } from "../policies/rwa-multiply-custom.js";

const PACKET_LIMIT = 1_232;

type PolicyState = Readonly<{
  settings: PublicKey;
  seed: { toString(): string };
  threshold: number;
  timeLock: number;
  signers: readonly Readonly<{ key: PublicKey; permissions: Readonly<{ mask: number }> }>[];
  policyState: Readonly<{ __kind: string }>;
}>;

const Policy = (squadsGenerated as unknown as {
  Policy: { fromAccountInfo(account: NonNullable<Awaited<ReturnType<Connection["getAccountInfo"]>>>): readonly [PolicyState, number] };
}).Policy;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

type HashableAccount = Readonly<{
  owner: PublicKey | string;
  lamports: number;
  executable: boolean;
  data: Uint8Array;
}>;

function accountStateSha256(account: HashableAccount | null | undefined): string {
  if (account == null) return sha256(Buffer.from("absent-account/v1"));
  return sha256(Buffer.from(JSON.stringify({
    owner: typeof account.owner === "string" ? account.owner : account.owner.toBase58(),
    lamports: String(account.lamports),
    executable: account.executable,
    dataBase64: Buffer.from(account.data).toString("base64"),
  })));
}

async function protectedAccountManifest() {
  const route = RWA_MULTIPLY_ROUTE;
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const manifest = [
    { label: "squads_asset_ata", address: route.squads.assetAta },
    { label: "squads_collateral_ata", address: route.squads.collateralAta },
    { label: "voltr_idle_ata", address: accounts.idleAta },
    { label: "voltr_strategy_asset_ata", address: accounts.strategyAssetAta },
    { label: "adaptor_strategy_config", address: route.customAdaptor.strategyConfig },
    { label: "adaptor_report_ticket", address: accounts.reportTicket },
    { label: "voltr_strategy_init_receipt", address: accounts.strategyInitReceipt },
    { label: "voltr_adaptor_add_receipt", address: accounts.adaptorAddReceipt },
    { label: "voltr_protocol", address: accounts.protocol },
    { label: "voltr_lp_mint", address: accounts.lpMint },
    { label: "voltr_strategy_authority", address: accounts.strategyAuth },
    { label: "voltr_idle_authority", address: accounts.idleAuth },
    { label: "voltr_lp_mint_authority", address: accounts.lpMintAuth },
  ] as const;
  invariant(new Set(manifest.map(({ address }) => address)).size === manifest.length,
    "protected account manifest contains duplicate addresses");
  return manifest;
}

function protectedRows(
  manifest: readonly Readonly<{ label: string; address: string }>[],
  accounts: readonly (HashableAccount | null | undefined)[],
) {
  invariant(accounts.length === manifest.length, "protected account snapshot length drifted");
  return manifest.map(({ label, address }, index) => {
    const account = accounts[index];
    return {
      label,
      address,
      present: account != null,
      owner: account == null
        ? null
        : typeof account.owner === "string" ? account.owner : account.owner.toBase58(),
      stateSha256: accountStateSha256(account),
    };
  });
}

async function simulateProtectedAccountBatches(input: Readonly<{
  connection: Connection;
  serializedTransaction: Uint8Array;
  minContextSlot: number;
  manifest: Awaited<ReturnType<typeof protectedAccountManifest>>;
}>) {
  const transaction = VersionedTransaction.deserialize(input.serializedTransaction);
  const rows: ReturnType<typeof protectedRows> = [];
  const batches: Array<{ contextSlot: number; addresses: readonly string[] }> = [];
  for (let offset = 0; offset < input.manifest.length; offset += 8) {
    const manifestBatch = input.manifest.slice(offset, offset + 8);
    const addresses = manifestBatch.map(({ address }) => address);
    const simulation = await input.connection.simulateTransaction(transaction, {
      commitment: "finalized",
      sigVerify: true,
      replaceRecentBlockhash: false,
      minContextSlot: input.minContextSlot,
      accounts: { encoding: "base64", addresses: [...addresses] },
    });
    invariant(simulation.value.err === null,
      `protected-account simulation failed: ${JSON.stringify(simulation.value.err)}`);
    invariant(simulation.context.slot >= input.minContextSlot,
      "protected-account simulation context predates the signed prestate");
    invariant(simulation.value.accounts?.length === manifestBatch.length,
      "protected-account simulation omitted requested post-account images");
    const accounts = simulation.value.accounts.map((account) => {
      if (!account) return null;
      invariant(account.data[1] === "base64", "protected-account simulation returned non-base64 data");
      const encodedData = account.data[0];
      invariant(typeof encodedData === "string", "protected-account simulation omitted account data");
      return {
        owner: account.owner,
        lamports: account.lamports,
        executable: account.executable,
        data: Buffer.from(encodedData, "base64"),
      } satisfies HashableAccount;
    });
    rows.push(...protectedRows(manifestBatch, accounts));
    batches.push({ contextSlot: simulation.context.slot, addresses });
  }
  return { rows, batches } as const;
}

function projectedClosed(account: Readonly<{
  owner: string;
  lamports: number;
  data: Uint8Array;
}> | null | undefined): boolean {
  return account == null
    || (account.lamports === 0
      && account.owner === RWA_MULTIPLY_ROUTE.programs.system
      && account.data.length === 0);
}

function writePrivate(path: string, value: Record<string, unknown>, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag, mode: 0o600 });
  chmodSync(path, 0o600);
}

async function inspectProtectedState(
  connection: Connection,
  minContextSlot: number,
  protectedManifest: Awaited<ReturnType<typeof protectedAccountManifest>>,
) {
  const route = RWA_MULTIPLY_ROUTE;
  const addresses = [
    route.squads.settings,
    ...LEGACY_CUSTOM_POLICY_ADDRESSES,
    route.previousBackyardVault,
    route.vault.address,
    ...protectedManifest.map(({ address }) => address),
  ];
  const response = await connection.getMultipleAccountsInfoAndContext(
    addresses.map((value) => new PublicKey(value)),
    { commitment: "finalized", minContextSlot },
  );
  const [settings, ...tail] = response.value;
  const legacy = tail.slice(0, LEGACY_CUSTOM_POLICY_ADDRESSES.length);
  const previousVault = tail[LEGACY_CUSTOM_POLICY_ADDRESSES.length];
  const activeVault = tail[LEGACY_CUSTOM_POLICY_ADDRESSES.length + 1];
  const protectedAccounts = tail.slice(LEGACY_CUSTOM_POLICY_ADDRESSES.length + 2);
  invariant(settings?.owner.toBase58() === route.squads.program, "Squads Settings is absent or has the wrong owner");
  invariant(previousVault?.owner.toBase58() === route.programs.voltr,
    "protected Backyard vault is absent or has the wrong owner");
  invariant(activeVault?.owner.toBase58() === route.programs.voltr,
    "active Voltr vault is absent or has the wrong owner");

  const rows = legacy.map((info, index) => {
    const seed = LEGACY_CUSTOM_POLICY_SEEDS[index]!;
    const address = LEGACY_CUSTOM_POLICY_ADDRESSES[index]!;
    if (!info) return { seed: seed.toString(), policy: address, present: false } as const;
    invariant(info.owner.toBase58() === route.squads.program,
      `legacy policy ${seed} has the wrong owner`);
    let policy: PolicyState;
    try {
      [policy] = Policy.fromAccountInfo(info);
    } catch {
      throw new Error(`legacy policy ${seed} is not a decodable Squads policy`);
    }
    invariant(policy.settings.toBase58() === route.squads.settings
      && policy.seed.toString() === seed.toString()
      && policy.threshold === 1
      && policy.timeLock === 0
      && policy.signers.length === 1
      && policy.signers[0]?.key.toBase58() === route.squads.delegatedExecutor
      && policy.signers[0]?.permissions.mask === 7
      && policy.policyState.__kind === "ProgramInteraction",
    `legacy policy ${seed} escaped the expected Settings/delegate boundary`);
    const dataSha256 = sha256(info.data);
    invariant(dataSha256 === LEGACY_CUSTOM_POLICY_DATA_SHA256[index],
      `legacy policy ${seed} data hash drifted; refusing unrecognized policy removal`);
    return {
      seed: seed.toString(),
      policy: address,
      present: true,
      dataSha256,
      lamports: info.lamports,
    } as const;
  });
  return {
    contextSlot: response.context.slot,
    rows,
    allPresent: rows.every(({ present }) => present),
    allAbsent: rows.every(({ present }) => !present),
    settingsSha256: sha256(settings.data),
    previousVaultSha256: sha256(previousVault.data),
    activeVaultSha256: sha256(activeVault.data),
    protectedAccounts: protectedRows(protectedManifest, protectedAccounts),
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
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not mainnet-beta");
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "setup admin signer drifted");

  const installed = await verifyInstalledCustomPolicies(connection);
  const replacementIdentities = installed.rows.map(({ seed, policy, dataSha256 }) => ({
    seed, policy, dataSha256,
  }));
  invariant(installed.pass
    && JSON.stringify(replacementIdentities) === JSON.stringify(REPLACEMENT_CUSTOM_POLICY_IDENTITIES),
  "exact ordered replacement policy PDA/seed/data hashes 62-65 are not finalized; refusing legacy retirement");
  const protectedManifest = await protectedAccountManifest();
  const before = await inspectProtectedState(connection, installed.contextSlot, protectedManifest);

  if (reconcile) {
    const pending = JSON.parse(readFileSync(`${journal}.pending`, "utf8")) as {
      schema?: unknown;
      legacyPolicies?: unknown;
      legacyPolicyDataSha256?: unknown;
      protectedAccounts?: unknown;
      transaction?: {
        expectedSignature?: unknown;
        projectedSettingsSha256?: unknown;
        protectedPreviousVaultSha256?: unknown;
        protectedActiveVaultSha256?: unknown;
      };
    };
    invariant(pending.schema === "loyal-rwa-multiply-legacy-policy-retirement/v1"
      && JSON.stringify(pending.legacyPolicies) === JSON.stringify(LEGACY_CUSTOM_POLICY_ADDRESSES)
      && JSON.stringify(pending.legacyPolicyDataSha256) === JSON.stringify(LEGACY_CUSTOM_POLICY_DATA_SHA256),
    "pending journal does not describe the exact legacy policy set");
    const signature = String(pending.transaction?.expectedSignature ?? "");
    invariant(signature.length > 0, "pending journal lacks the expected signature");
    const statuses = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    const status = statuses.value[0];
    invariant(status?.err === null && status.confirmationStatus === "finalized",
      "pending signature is not finalized successfully; no replacement transaction was sent");
    invariant(before.allAbsent, "finalized retirement did not remove exactly all legacy policies");
    invariant(before.settingsSha256 === pending.transaction?.projectedSettingsSha256,
      "Squads Settings changed across legacy retirement");
    invariant(before.previousVaultSha256 === pending.transaction?.protectedPreviousVaultSha256,
      "protected Backyard vault changed across legacy retirement");
    invariant(before.activeVaultSha256 === pending.transaction?.protectedActiveVaultSha256,
      "active Voltr vault changed across legacy retirement");
    invariant(JSON.stringify(before.protectedAccounts) === JSON.stringify(pending.protectedAccounts),
      "Squads/Voltr custody or adaptor protected state changed across legacy retirement");
    const final = { ...pending, verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: status.slot, finalizedContextSlot: statuses.context.slot };
    writePrivate(journal, final, "wx");
    renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: status.slot, journal }, null, 2));
    return;
  }

  if (before.allAbsent) {
    console.log(JSON.stringify({ verdict: "PASS_ALREADY_RETIRED", broadcast: false,
      replacementPolicies: installed.rows, legacyPolicies: before.rows }, null, 2));
    return;
  }
  invariant(before.allPresent,
    "legacy policy set is partially present; refusing a transaction that is not the exact 53-56 retirement");

  const retirement = buildLegacyCustomPolicyRetirementInstruction();
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    commitment: "finalized",
    minimumContextSlot: before.contextSlot,
    instructions: [retirement],
    inspectedAddresses: [
      ...LEGACY_CUSTOM_POLICY_ADDRESSES,
      route.squads.settings,
      route.previousBackyardVault,
      route.vault.address,
    ],
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT,
    `legacy policy retirement packet exceeds ${PACKET_LIMIT} bytes`);
  invariant(prepared.simulation.err === null,
    `legacy policy retirement simulation failed: ${JSON.stringify({
      err: prepared.simulation.err,
      logs: prepared.simulation.logs,
    })}`);
  const [old53, old54, old55, old56, postSettings, postPreviousVault, postActiveVault] =
    prepared.simulation.postAccounts;
  invariant([old53, old54, old55, old56].every(projectedClosed),
    "simulation did not close exactly all legacy policy accounts");
  invariant(postSettings?.owner === route.squads.program
    && sha256(postSettings.data) === before.settingsSha256,
  "simulation changed Squads Settings owner or bytes");
  invariant(postPreviousVault != null && sha256(postPreviousVault.data) === before.previousVaultSha256,
    "simulation changed the protected Backyard vault");
  invariant(postActiveVault != null && sha256(postActiveVault.data) === before.activeVaultSha256,
    "simulation changed the active Voltr vault");
  const protectedSimulation = await simulateProtectedAccountBatches({
    connection,
    serializedTransaction: prepared.serializedTransaction,
    minContextSlot: prepared.simulationSlot,
    manifest: protectedManifest,
  });
  invariant(JSON.stringify(protectedSimulation.rows)
    === JSON.stringify(before.protectedAccounts),
  "simulation changed Squads/Voltr custody or adaptor protected state");

  const plan = {
    schema: "loyal-rwa-multiply-legacy-policy-retirement/v1",
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    legacySeeds: LEGACY_CUSTOM_POLICY_SEEDS.map(String),
    legacyPolicies: LEGACY_CUSTOM_POLICY_ADDRESSES,
    legacyPolicyDataSha256: LEGACY_CUSTOM_POLICY_DATA_SHA256,
    protectedAccounts: before.protectedAccounts,
    replacementPolicies: installed.rows.map(({ operation, seed, policy, dataSha256 }) => ({
      operation, seed, policy, dataSha256,
    })),
    transaction: {
      instructionCount: 1,
      actionCount: 4,
      packetBytes: prepared.packetBytes,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      latestBlockhash: prepared.latestBlockhash,
      prestateSlot: prepared.prestateSlot,
      simulationSlot: prepared.simulationSlot,
      protectedSimulationBatches: protectedSimulation.batches,
      previousLegacyPolicyDataSha256: before.rows.map((row) => row.present ? row.dataSha256 : null),
      projectedSettingsSha256: sha256(postSettings.data),
      protectedPreviousVaultSha256: before.previousVaultSha256,
      protectedActiveVaultSha256: before.activeVaultSha256,
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
  invariant(returned === prepared.expectedSignature,
    "RPC returned a signature different from the persisted wire");
  const confirmation = await connection.confirmTransaction(
    { signature: returned, ...prepared.latestBlockhash },
    "finalized",
  );
  invariant(confirmation.value.err === null,
    `legacy policy retirement finalized with ${JSON.stringify(confirmation.value.err)}`);
  const replacementAfter = await verifyInstalledCustomPolicies(connection);
  invariant(replacementAfter.pass, "replacement policies drifted after legacy retirement");
  const after = await inspectProtectedState(connection, confirmation.context.slot, protectedManifest);
  invariant(after.allAbsent, "finalized retirement did not remove exactly all legacy policies");
  invariant(after.settingsSha256 === plan.transaction.projectedSettingsSha256,
    "Squads Settings changed across finalized legacy retirement");
  invariant(after.previousVaultSha256 === plan.transaction.protectedPreviousVaultSha256,
    "protected Backyard vault changed across finalized legacy retirement");
  invariant(after.activeVaultSha256 === plan.transaction.protectedActiveVaultSha256,
    "active Voltr vault changed across finalized legacy retirement");
  invariant(JSON.stringify(after.protectedAccounts) === JSON.stringify(plan.protectedAccounts),
    "Squads/Voltr custody or adaptor protected state changed across finalized legacy retirement");
  writePrivate(journal, { ...plan, verdict: "FINALIZED_RECONCILED", signature: returned,
    finalizedContextSlot: confirmation.context.slot }, "wx");
  renameSync(`${journal}.pending`, `${journal}.sent-wire`);
  console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature: returned,
    finalizedContextSlot: confirmation.context.slot, journal }, null, 2));
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
