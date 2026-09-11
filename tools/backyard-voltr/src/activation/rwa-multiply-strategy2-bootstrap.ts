import { createHash } from "node:crypto";
import { chmodSync, existsSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import {
  AccountRole,
  address,
  createNoopSigner,
  getAddressEncoder,
  type Address,
  type Instruction,
} from "@solana/kit";
import { getCreateAssociatedTokenIdempotentInstructionAsync } from "@solana-program/token";
import { Connection, Keypair, PublicKey, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import {
  getInitializeStrategyInstructionAsync,
  getUpdateVaultConfigInstructionAsync,
  VaultConfigField,
} from "@voltr/vault-sdk";

import {
  deriveStrategyTwoVoltrAccounts,
  deriveStrategyTwoPolicySeeds,
  rwaMultiplyStrategyTwoTarget,
  STRATEGY_TWO_DERIVATION_DOMAIN,
  STRATEGY_TWO_REPORT_NAV_CAP_RAW,
  type StrategyTwoIdentity,
} from "../domain/rwa-multiply-strategy2-route-spec.js";
import type { RwaMultiplyRouteSpec } from "../domain/rwa-multiply-route-spec.js";
import {
  deriveRwaMultiplyVoltrAccounts,
  initializeRwaAdaptorConfigInstruction,
  initializeRwaAdaptorReportTicketInstruction,
  RWA_ADAPTOR_DISCRIMINATORS,
} from "../integrations/rwa-multiply-voltr.js";
import { prepareSignedV0Transaction, sendPreparedOnce } from "../integrations/solana-compat.js";
import {
  deriveRwaMultiplyStrategySigningMaterial,
  signingMaterialFromEnvironment,
} from "../integrations/signer.js";
import { compileCustomPolicyArtifact, readFinalizedCustomPolicySeed } from "../policies/rwa-multiply-custom.js";
import { customPolicyAddress } from "../policies/rwa-multiply-legacy-retirement.js";
import { STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE } from "../policies/rwa-multiply-strategy2-seed-journal.js";
import {
  assertStrategyTwoBootstrapPoststate,
  reconcileStrategyTwoBootstrap,
} from "./rwa-multiply-strategy2-bootstrap-reconcile.js";

const PACKET_LIMIT = 1_232;
/**
 * The rehearsal compiles the same seed set the installer journals: the base is
 * the pinned seed-journal constant, never an independent literal.
 */
const SIMULATED_POLICY_SEED_BEFORE = STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE;
/** Read-only fallback endpoint for unsigned preflight simulations; never logged. */
const PUBLIC_MAINNET_RPC = "https://api.mainnet-beta.solana.com";

type HashableAccount = Readonly<{
  owner: string;
  lamports: number;
  executable: boolean;
  data: Uint8Array;
}>;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function cliValue(flag: string): string {
  const index = process.argv.indexOf(flag);
  if (index < 0) return "";
  const value = process.argv[index + 1] ?? "";
  invariant(value.length > 0 && !value.startsWith("--"), `${flag} requires a value`);
  return value;
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function accountRow(label: string, expected: "created" | "absent-authority", account: HashableAccount | null | undefined) {
  return {
    label,
    expected,
    present: account != null,
    owner: account?.owner ?? null,
    lamports: account?.lamports ?? null,
    dataBytes: account?.data.length ?? 0,
    stateSha256: account == null ? null : sha256(account.data),
  };
}

/**
 * The three ordered bootstrap wires. Account privileges are message-wide, so
 * initializeConfig (config WRITABLE_SIGNER) can never share a transaction with
 * initializeReportTicket (config READONLY), and wire C is the custody ATA plus
 * the manager round-trip (BAqg -> initializeStrategy -> ST999). Every signer is
 * a no-op signer: address/role shape is what matters here, never key material.
 */
async function buildBootstrapWires(route: RwaMultiplyRouteSpec) {
  const admin = createNoopSigner(route.setupAdmin);
  const configKeypair = createNoopSigner(route.customAdaptor.strategyConfig);
  const settingsSigner = createNoopSigner(route.customAdaptor.settingsSigner);
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const strategyAccounts = await deriveStrategyTwoVoltrAccounts(route.customAdaptor.strategyConfig);
  invariant(strategyAccounts.reportTicket === accounts.reportTicket
    && strategyAccounts.strategyAuth === accounts.strategyAuth
    && strategyAccounts.strategyInitReceipt === accounts.strategyInitReceipt,
  "strategy-two derivation drifted from the route-derived Voltr accounts");

  const addressEncoder = getAddressEncoder();
  const initializeStrategy = appendAccounts(await getInitializeStrategyInstructionAsync({
    payer: admin,
    manager: settingsSigner,
    vault: route.vault.address,
    strategy: route.customAdaptor.strategyConfig,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    strategyInitReceipt: accounts.strategyInitReceipt,
    vaultStrategyAuth: accounts.strategyAuth,
    adaptorProgram: route.customAdaptor.program,
    instructionDiscriminator: RWA_ADAPTOR_DISCRIMINATORS.initialize,
    additionalArgs: null,
  }, { programAddress: route.programs.voltr }), [
    { address: route.squads.settings, role: AccountRole.READONLY },
    { address: route.squads.vault, role: AccountRole.READONLY_SIGNER },
    { address: route.assets.assetMint, role: AccountRole.READONLY },
    { address: route.assets.tokenProgram, role: AccountRole.READONLY },
    { address: route.squads.assetAta, role: AccountRole.READONLY },
    { address: route.squads.program, role: AccountRole.READONLY },
  ]);
  const custodyAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: admin,
    ata: accounts.strategyAssetAta,
    owner: accounts.strategyAuth,
    mint: route.assets.assetMint,
    systemProgram: route.programs.system,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const handoffToSettingsSigner = await getUpdateVaultConfigInstructionAsync({
    admin,
    vault: route.vault.address,
    field: VaultConfigField.Manager,
    data: addressEncoder.encode(route.customAdaptor.settingsSigner),
  }, { programAddress: route.programs.voltr });
  const restoreManager = await getUpdateVaultConfigInstructionAsync({
    admin,
    vault: route.vault.address,
    field: VaultConfigField.Manager,
    data: addressEncoder.encode(route.squads.vault),
  }, { programAddress: route.programs.voltr });
  return {
    accounts,
    wireA: [initializeRwaAdaptorConfigInstruction(admin, configKeypair, accounts, route)],
    wireB: [await initializeRwaAdaptorReportTicketInstruction(admin, route)],
    wireC: [custodyAta, handoffToSettingsSigner, initializeStrategy, restoreManager],
  } as const;
}

function writePrivate(path: string, value: Record<string, unknown>, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, (_key, entry) => typeof entry === "bigint" ? entry.toString() : entry, 2)}\n`, {
    flag,
    mode: 0o600,
  });
  chmodSync(path, 0o600);
}

/** The finalized chain-state gate each wire must satisfy before it may be sent. */
async function assertWirePreconditions(
  connection: Connection,
  route: RwaMultiplyRouteSpec,
  accounts: Awaited<ReturnType<typeof deriveRwaMultiplyVoltrAccounts>>,
  phase: "A" | "B" | "C",
) {
  const [rawConfig, rawTicket, rawCustody, rawVault] = await connection.getMultipleAccountsInfo([
    new PublicKey(route.customAdaptor.strategyConfig),
    new PublicKey(accounts.reportTicket),
    new PublicKey(accounts.strategyAssetAta),
    new PublicKey(route.vault.address),
  ], "finalized");
  const config = rawConfig ?? null;
  const ticket = rawTicket ?? null;
  const custody = rawCustody ?? null;
  const vault = rawVault ?? null;
  if (phase === "A") {
    invariant(config === null, "adaptor config already exists; wire A already ran");
  } else if (phase === "B") {
    invariant(config !== null && config.owner.toBase58() === route.customAdaptor.program
      && config.data.length === 472,
    "wire B requires the 472-byte adaptor config created by wire A");
    invariant(ticket === null, "report ticket already exists; wire B already ran");
  } else {
    invariant(ticket !== null, "wire C requires the report ticket created by wire B");
    invariant(custody === null, "strategy custody ATA already exists; wire C already ran");
  }
  invariant(vault !== null && vault.owner.toBase58() === route.programs.voltr,
    "active Voltr vault is absent or inexact");
  return {
    configSha256: config === null ? null : sha256(config.data),
    vaultSha256: sha256(vault.data),
    /** Re-derived over finalized state after the wire lands (send and reconcile). */
    assertFinalized: (
      finalized: readonly ({ owner: { toBase58(): string }; data: Uint8Array } | null)[],
    ) => {
      assertStrategyTwoBootstrapPoststate({
        phase,
        route,
        finalized,
        expectedConfigSha256: phase === "B" && config !== null ? sha256(config.data) : null,
        expectedVaultSha256: phase === "C" && vault !== null ? sha256(vault.data) : null,
      });
    },
  };
}

/**
 * The journaled operator send path for exactly one bootstrap wire. This is the
 * only code in this tool that signs and broadcasts; it runs solely under
 * `--phase A|B|C --execute|--reconcile --journal`, refuses to send without
 * CONFIRM_MAINNET=1, simulates the signed wire before sending, and derives the
 * config keypair from the setup admin over STRATEGY_TWO_DERIVATION_DOMAIN so it
 * refuses `--config` values the operator cannot reproduce.
 */
async function runOperatorWire() {
  const phaseValue = cliValue("--phase");
  invariant(phaseValue === "A" || phaseValue === "B" || phaseValue === "C",
    "--phase must be one of A, B, C");
  const phase: "A" | "B" | "C" = phaseValue;
  const execute = process.argv.includes("--execute");
  const reconcile = process.argv.includes("--reconcile");
  const journalIndex = process.argv.indexOf("--journal");
  const journal = journalIndex >= 0 ? resolve(process.argv[journalIndex + 1] ?? "") : "";
  invariant(!(execute && reconcile), "--execute and --reconcile are mutually exclusive");
  invariant(execute || reconcile, "the operator path requires --execute or --reconcile");
  invariant(!execute || process.env.CONFIRM_MAINNET === "1", "--execute requires CONFIRM_MAINNET=1");
  invariant(journal.endsWith(".json") && existsSync(dirname(journal)),
    "--journal PATH.json under an existing directory is required");
  invariant(!execute || (!existsSync(journal) && !existsSync(`${journal}.pending`)),
    "journal replay barrier already exists");
  invariant(!reconcile || (!existsSync(journal) && existsSync(`${journal}.pending`)),
    "--reconcile requires one pending journal and no finalized journal");
  const configArg = cliValue("--config");
  const delegatedArg = cliValue("--delegated-signer");
  invariant(configArg.length > 0 && delegatedArg.length > 0,
    "the operator path requires --config and --delegated-signer");

  const route = (await rwaMultiplyStrategyTwoTarget({
    config: address(configArg),
    delegatedSigner: address(delegatedArg),
  }, SIMULATED_POLICY_SEED_BEFORE)).route;
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required for the operator path");
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not mainnet-beta");
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);

  if (reconcile) {
    // Reconciliation is intentionally ahead of all fresh-state absence
    // checks. A send may have finalized immediately before a process crash;
    // the pending journal is the authority for recovering that outcome.
    const settled = await reconcileStrategyTwoBootstrap({
      connection,
      route,
      accounts,
      phase,
      journal,
    });
    console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", phase,
      ...settled, journal }, null, 2));
    return;
  }

  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "setup admin signer drifted");
  const configMaterial = await deriveRwaMultiplyStrategySigningMaterial(admin, route);
  invariant(configMaterial.signer.address === route.customAdaptor.strategyConfig,
    "derived strategy-two config keypair does not match --config; refusing to send");

  const { wireA, wireB, wireC } = await buildBootstrapWires(route);
  const wire = phase === "A" ? wireA : phase === "B" ? wireB : wireC;
  const chainGates = await assertWirePreconditions(connection, route, accounts, phase);

  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    additionalSigners: phase === "A" ? [configMaterial] : [],
    instructions: wire,
    inspectedAddresses: [
      route.customAdaptor.strategyConfig,
      accounts.reportTicket,
      accounts.strategyAssetAta,
      route.vault.address,
    ],
    prestateAddresses: [
      route.customAdaptor.strategyConfig,
      accounts.reportTicket,
      accounts.strategyAssetAta,
      route.vault.address,
      route.setupAdmin,
    ],
    commitment: "finalized",
  });
  invariant(prepared.packetBytes <= PACKET_LIMIT, `bootstrap wire ${phase} exceeds ${PACKET_LIMIT} bytes`);
  invariant(prepared.simulation.err === null,
    `signed bootstrap wire ${phase} simulation failed: ${JSON.stringify({
      err: prepared.simulation.err, logs: prepared.simulation.logs,
    })}`);
  const [rawConfigPost, rawTicketPost, rawCustodyPost, rawVaultPost] = prepared.simulation.postAccounts;
  const configPost = rawConfigPost ?? null;
  const ticketPost = rawTicketPost ?? null;
  const custodyPost = rawCustodyPost ?? null;
  const vaultPost = rawVaultPost ?? null;
  if (phase === "A") {
    invariant(configPost !== null && configPost.data.length === 472
      && configPost.owner === route.customAdaptor.program,
    "signed wire A simulation did not project the 472-byte adaptor config");
  } else if (phase === "B") {
    invariant(ticketPost !== null && ticketPost.owner === route.customAdaptor.program,
      "signed wire B simulation did not project the report ticket");
    invariant(configPost !== null && chainGates.configSha256 !== null
      && sha256(configPost.data) === chainGates.configSha256,
    "signed wire B simulation changed the adaptor config");
  } else {
    invariant(custodyPost !== null, "signed wire C simulation did not project the custody ATA");
    invariant(vaultPost !== null && sha256(vaultPost.data) === chainGates.vaultSha256,
      "signed wire C simulation changed the Voltr vault: wire C must be state-neutral");
  }
  const plan = {
    schema: "loyal-rwa-multiply-strategy-two-bootstrap-wire/v1",
    verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND",
    broadcast: true,
    phase,
    config: route.customAdaptor.strategyConfig,
    delegatedSigner: route.squads.delegatedExecutor,
    derivationDomain: STRATEGY_TWO_DERIVATION_DOMAIN,
    wireLabel: phase === "A" ? "initialize_config"
      : phase === "B" ? "initialize_report_ticket"
      : "custody_ata+manager_round_trip",
    transaction: {
      packetBytes: prepared.packetBytes,
      instructionCount: wire.length,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      configSha256Before: chainGates.configSha256,
      vaultSha256Before: chainGates.vaultSha256,
      latestBlockhash: prepared.latestBlockhash,
    },
  };
  writePrivate(`${journal}.pending`, {
    ...plan,
    signedWireBase64: Buffer.from(prepared.serializedTransaction).toString("base64"),
  }, "wx");
  const settled = await sendPreparedOnce(rpcUrl, prepared, prepared.simulationSlot);
  invariant(settled.err === null, `bootstrap wire ${phase} finalized with ${JSON.stringify(settled.err)}`);
  const finalizedAccounts = await connection.getMultipleAccountsInfo([
    new PublicKey(route.customAdaptor.strategyConfig),
    new PublicKey(accounts.reportTicket),
    new PublicKey(accounts.strategyAssetAta),
    new PublicKey(route.vault.address),
  ], "finalized");
  chainGates.assertFinalized(finalizedAccounts);
  writePrivate(journal, { ...plan, verdict: "FINALIZED_RECONCILED", signature: settled.signature,
    finalizedSlot: settled.finalizedSlot, finalizedContextSlot: settled.confirmationSlot }, "wx");
  renameSync(`${journal}.pending`, `${journal}.sent-wire`);
  console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", phase, signature: settled.signature,
    finalizedSlot: settled.finalizedSlot, finalizedContextSlot: settled.confirmationSlot, journal }, null, 2));
}

/**
 * Keyless strategy-two rehearsal (no `--phase`): nothing here loads key
 * material and there is no send path. The config keypair is a generated
 * throwaway (or an address the operator supplies for rehearsal), every signer
 * is a no-op signer, and the atomic bootstrap wire is simulated unsigned
 * (`sigVerify: false`) against live mainnet. Every policy data hash in this
 * evidence is therefore a rehearsal value, not the install value.
 */
async function main() {
  if (process.argv.includes("--phase")) {
    await runOperatorWire();
    return;
  }
  const evidence = cliValue("--evidence");
  invariant(evidence.endsWith(".json") && existsSync(dirname(evidence)),
    "--evidence PATH.json under an existing directory is required");
  invariant(!process.argv.includes("--send"), "--send is not a rehearsal flag; the send path is --phase A|B|C --execute");
  invariant(!process.argv.includes("--execute") && !process.argv.includes("--reconcile"),
    "--execute/--reconcile require --phase A|B|C");

  const route = (await rwaMultiplyStrategyTwoTarget({
    config: address(cliValue("--config") || Keypair.generate().publicKey.toBase58()),
    delegatedSigner: address(cliValue("--delegated-signer") || Keypair.generate().publicKey.toBase58()),
  }, SIMULATED_POLICY_SEED_BEFORE)).route;
  const config = route.customAdaptor.strategyConfig;

  const rpcUrl = process.env.SOLANA_RPC_URL?.trim() || PUBLIC_MAINNET_RPC;
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not mainnet-beta");
  await sleep(400);
  // The rehearsal never sends, but its readback must still state whether the
  // live Settings counter still sits on the pinned seed base.
  const liveSeed = await readFinalizedCustomPolicySeed(connection);

  const { accounts, wireA, wireB, wireC } = await buildBootstrapWires(route);
  const instructions: readonly Instruction[] = [...wireA, ...wireB, ...wireC];
  const latestBlockhash = await connection.getLatestBlockhash("finalized");
  await sleep(400);
  const compile = (list: readonly Instruction[]) => {
    const wire = new TransactionMessage({
      payerKey: new PublicKey(route.setupAdmin),
      recentBlockhash: latestBlockhash.blockhash,
      instructions: list.map((instruction) => ({
        programId: new PublicKey(instruction.programAddress),
        keys: (instruction.accounts ?? []).map((meta) => ({
          pubkey: new PublicKey(meta.address),
          isSigner: meta.role === AccountRole.READONLY_SIGNER || meta.role === AccountRole.WRITABLE_SIGNER,
          isWritable: meta.role === AccountRole.WRITABLE || meta.role === AccountRole.WRITABLE_SIGNER,
        })),
        data: Buffer.from(instruction.data ?? []),
      })),
    }).compileToV0Message();
    return { wire, serialized: new VersionedTransaction(wire).serialize() };
  };
  const wires = { A: compile(wireA), B: compile(wireB), C: compile(wireC) };
  for (const [name, { serialized: bytes }] of Object.entries(wires)) {
    invariant(bytes.length <= PACKET_LIMIT, `bootstrap wire ${name} exceeds ${PACKET_LIMIT} bytes`);
  }
  const { wire, serialized } = wires.A;

  let simulation: Awaited<ReturnType<Connection["simulateTransaction"]>>;
  try {
    simulation = await connection.simulateTransaction(new VersionedTransaction(wire), {
      commitment: "finalized",
      sigVerify: false,
      replaceRecentBlockhash: false,
      accounts: {
        encoding: "base64",
        addresses: [config, route.vault.address],
      },
    });
  } catch (error) {
    writeFileSync(resolve(evidence), `${JSON.stringify({
      schema: "loyal-rwa-multiply-strategy-two-bootstrap-preflight/v1",
      verdict: "RPC_BLOCKED", sent: false, signed: false,
      generatedAtUtc: new Date().toISOString(),
      blocker: error instanceof Error ? error.message : String(error),
    }, null, 2)}\n`, { flag: "wx" });
    throw error;
  }
  if (simulation.value.err !== null) {
    // Record the blocked wire verbatim: a rehearsal finding is evidence too.
    writeFileSync(resolve(evidence), `${JSON.stringify({
      schema: "loyal-rwa-multiply-strategy-two-bootstrap-preflight/v1",
      verdict: "UNSIGNED_SIMULATION_BLOCKED", sent: false, signed: false,
      generatedAtUtc: new Date().toISOString(),
      simulationError: simulation.value.err,
      failedInstructionIndex: (simulation.value.err as { InstructionError?: readonly unknown[] } | null)
        ?.InstructionError?.[1] ?? null,
      logs: simulation.value.logs ?? [],
      packetBytes: serialized.length,
      strategyTwo: { config, delegatedSigner: route.squads.delegatedExecutor },
    }, null, 2)}\n`, { flag: "wx" });
    invariant(false, `unsigned strategy-two bootstrap simulation failed: ${
      JSON.stringify({ err: simulation.value.err, logs: simulation.value.logs })}`);
  }
  invariant(simulation.value.err === null,
    `unsigned strategy-two bootstrap simulation failed: ${JSON.stringify({
      err: simulation.value.err, logs: simulation.value.logs,
    })}`);
  await sleep(400);
  const postAccounts = (simulation.value.accounts ?? []).map((raw): HashableAccount | null => {
    if (!raw) return null;
    const data = raw.data as readonly string[];
    invariant(data[1] === "base64" && typeof data[0] === "string",
      "simulation returned non-base64 account data");
    return {
      owner: raw.owner,
      lamports: raw.lamports,
      executable: raw.executable,
      data: Buffer.from(data[0], "base64"),
    };
  });
  const [configImage, vaultImage] = postAccounts;
  const configBytes = configImage?.data.length ?? 0;
  invariant(configImage != null && configBytes === 472,
    `simulated adaptor config is ${configBytes} bytes; expected the 472-byte layout`);
  // vault_strategy_auth is a PDA signing authority: it exists only while it
  // holds lamports, so absence is the expected shape and is not a gate.
  invariant(vaultImage != null && sha256(vaultImage.data).length === 64,
    "simulation lost the active Voltr vault image");

  const initializeConfigData = instructions[0]!.data!;
  invariant(initializeConfigData.length === 25,
    `initialize_config wire is ${initializeConfigData.length} bytes; expected 8 discriminator + 17 arg bytes`);
  invariant(initializeConfigData[8] === route.squads.vaultIndex
    && initializeConfigData.subarray(9, 17).every((byte, index) =>
      byte === Number((STRATEGY_TWO_REPORT_NAV_CAP_RAW >> BigInt(8 * index)) & 0xffn)),
  "initialize_config args do not encode vault index 0 and the 1e12 reported-NAV ceiling");

  const compiled = await compileCustomPolicyArtifact(
    SIMULATED_POLICY_SEED_BEFORE,
    await rwaMultiplyStrategyTwoTarget({ config, delegatedSigner: route.squads.delegatedExecutor }, SIMULATED_POLICY_SEED_BEFORE),
  );
  const expectedSeeds = deriveStrategyTwoPolicySeeds(SIMULATED_POLICY_SEED_BEFORE);
  invariant(JSON.stringify(compiled.policies.map(({ seed }) => seed))
    === JSON.stringify(Object.values(expectedSeeds).map(String)),
  "compiled strategy-two policy seeds drifted from the derived Settings counter");
  invariant(JSON.stringify(compiled.policies.map(({ policy }) => policy))
    === JSON.stringify(Object.values(expectedSeeds).map((seed) => customPolicyAddress(seed))),
  "compiled strategy-two policy addresses drifted from the Settings PDA derivation");

  const evidenceDocument = {
    schema: "loyal-rwa-multiply-strategy-two-bootstrap-preflight/v1",
    verdict: "UNSIGNED_SIMULATION_PASS",
    sent: false,
    signed: false,
    identityClass: cliValue("--config") ? "operator-supplied-throwaway" : "generated-throwaway",
    authoritativeHashCaveat: "Policy data hashes below come from the throwaway config identity. The install-time hashes are produced only after the operator derives the real config keypair from the setup admin over STRATEGY_TWO_DERIVATION_DOMAIN and the strategy-two target is recompiled.",
    generatedAtUtc: new Date().toISOString(),
    rpcEndpoint: process.env.SOLANA_RPC_URL ? "custom" : "api.mainnet-beta.solana.com",
    derivationDomain: STRATEGY_TWO_DERIVATION_DOMAIN,
    strategyTwo: {
      config,
      delegatedSigner: route.squads.delegatedExecutor,
      strategyInitReceipt: accounts.strategyInitReceipt,
      strategyAuth: accounts.strategyAuth,
      strategyAssetAta: accounts.strategyAssetAta,
      reportTicket: accounts.reportTicket,
      sharedWithV2: {
        vault: route.vault.address,
        adaptorProgram: route.customAdaptor.program,
        adaptorAddReceipt: accounts.adaptorAddReceipt,
      },
    },
    replacementPolicies: {
      policySeedBefore: SIMULATED_POLICY_SEED_BEFORE.toString(),
      observedSettingsPolicySeedBefore: liveSeed.policySeedBefore.toString(),
      seedBaseMatchesLiveCounter: liveSeed.policySeedBefore === SIMULATED_POLICY_SEED_BEFORE,
      seeds: Object.values(expectedSeeds).map(String),
      policies: Object.values(expectedSeeds).map((seed) => customPolicyAddress(seed)),
      caps: {
        amountRaw: "100000000",
        reportNavRaw: STRATEGY_TWO_REPORT_NAV_CAP_RAW.toString(),
      },
      compiled: {
        sourceSha256: compiled.sourceSha256,
        physicalPolicyCount: compiled.physicalPolicyCount,
        deploymentReady: compiled.deploymentReady,
        rows: compiled.policies.map(({ operation, seed, policy, createInstruction }) => ({
          operation,
          seed,
          policy,
          createDataSha256: sha256(Buffer.from(createInstruction.dataBase64, "base64")),
          createDataBytes: Buffer.from(createInstruction.dataBase64, "base64").length,
          authoritative: false,
        })),
      },
    },
    simulation: {
      sequenceNote: "Account privileges are transaction-wide, so the bootstrap is three wires: A initialize_config (config keypair signs), B initialize_report_ticket, C custody ATA + manager round-trip (BAqg -> initializeStrategy -> ST999). Wires B and C read state that A and B create, so only wire A is simulable against live mainnet; the full ordered sequence is rehearsed by the LiteSVM proof crates/squads-test-harness/tests/voltr_reset_sequence.rs (R5a).",
      wires: {
        A: { label: "initialize_config", instructionCount: wireA.length, packetBytes: wires.A.serialized.length, wireSha256: sha256(wires.A.serialized), simulated: true },
        B: { label: "initialize_report_ticket", instructionCount: wireB.length, packetBytes: wires.B.serialized.length, wireSha256: sha256(wires.B.serialized), simulated: false },
        C: { label: "custody_ata+manager_round_trip", instructionCount: wireC.length, packetBytes: wires.C.serialized.length, wireSha256: sha256(wires.C.serialized), simulated: false },
      },
      simulatedInstructionCount: wireA.length,
      simulatedPacketBytes: serialized.length,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
      messageSha256: sha256(wire.serialize()),
      wireSha256: sha256(serialized),
      latestBlockhash,
      simulationSlot: simulation.context.slot,
      initializeConfigArgsHex: Buffer.from(initializeConfigData).toString("hex"),
      postAccounts: [
        accountRow("adaptor_strategy_config", "created", configImage),
        accountRow("voltr_vault", "created", vaultImage),
      ],
      authorityNote: "voltr_strategy_authority is a PDA signing authority with no on-chain account data; absence is expected and is not a gate.",
      logsTail: (simulation.value.logs ?? []).slice(-10),
    },
  };
  writeFileSync(resolve(evidence), `${JSON.stringify(evidenceDocument, null, 2)}\n`, { flag: "wx" });
  console.log(JSON.stringify({ verdict: "UNSIGNED_SIMULATION_PASS", sent: false, signed: false,
    config, strategyInitReceipt: accounts.strategyInitReceipt, strategyAuth: accounts.strategyAuth,
    strategyAssetAta: accounts.strategyAssetAta, reportTicket: accounts.reportTicket,
    policySeedBefore: SIMULATED_POLICY_SEED_BEFORE.toString(),
    observedSettingsPolicySeedBefore: liveSeed.policySeedBefore.toString(),
    seedBaseMatchesLiveCounter: liveSeed.policySeedBefore === SIMULATED_POLICY_SEED_BEFORE,
    policySeeds: Object.values(expectedSeeds).map(String),
    evidence }, null, 2));
}

function appendAccounts(
  instruction: Instruction,
  accounts: readonly Readonly<{ address: Address; role: AccountRole }>[],
): Instruction {
  return { ...instruction, accounts: [...(instruction.accounts ?? []), ...accounts] };
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
