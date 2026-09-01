import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { AccountRole, address, createNoopSigner, type Instruction } from "@solana/kit";
import { Connection, PublicKey, type AccountInfo } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { prepareSignedV0Transaction, type AccountSnapshot } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  buildRwaMultiplyArmReportInstruction,
  buildRwaMultiplyManagerInstructions,
  deriveRwaMultiplyVoltrAccounts,
  type RwaReportV1,
} from "../integrations/rwa-multiply-voltr.js";
import {
  DOWNSTREAM_ROLLBACK_MUTATION,
  downstreamRollbackLogProof,
  failedSimulationOverlayAccepted,
} from "./rwa-multiply-rollback-log-proof.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const MANIFEST_PATH = resolve(REPOSITORY_ROOT, "docs/manifests/backyard-rwa-v1.json");
const COMPILER = "compile-voltr-custom-execution";
const NAV_POLICY = "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP";
const ALLOCATION_POLICY = "HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz";
const CONFIG_LEN = 472;
const TICKET_LEN = 96;
const ZERO_HASH = "0".repeat(64);
const BPF_LOADER_UPGRADEABLE_PROGRAM_ID = new PublicKey("BPFLoaderUpgradeab1e11111111111111111111111");

async function confirmedAccountsAtOrAfter(
  connection: Connection,
  addresses: readonly string[],
  minimumContextSlot: number,
) {
  let lastError: unknown = null;
  for (let attempt = 0; attempt < 24; attempt += 1) {
    try {
      return await connection.getMultipleAccountsInfoAndContext(
        addresses.map((value) => new PublicKey(value)),
        { commitment: "confirmed", minContextSlot: minimumContextSlot },
      );
    } catch (error) {
      lastError = error;
      const message = error instanceof Error ? error.message : String(error);
      if (!message.includes("Minimum context slot has not been reached") || attempt === 23) throw error;
      await new Promise((resolve) => setTimeout(resolve, 500));
    }
  }
  throw new Error("confirmed readback bank did not reach the simulation slot", { cause: lastError });
}

type WireInstruction = Readonly<{
  programId: string;
  accounts: readonly Readonly<{ address: string; signer: boolean; writable: boolean }>[];
  dataBase64: string;
}>;

type ConfigState = Readonly<{
  squadsVaultIndex: number;
  voltrProgram: string;
  voltrVault: string;
  strategy: string;
  strategyAuthority: string;
  squadsProgram: string;
  squadsSettings: string;
  squadsSettingsSigner: string;
  squadsVault: string;
  assetMint: string;
  assetTokenProgram: string;
  squadsAssetAta: string;
  maxNavRaw: bigint;
  maxAgeSlots: bigint;
}>;

type TicketState = Readonly<{
  armed: boolean;
  lastConsumedSequence: bigint;
  activeSequence: bigint;
  activeWireSha256: string;
}>;

type MutationBuild = Readonly<{
  inner: readonly Instruction[];
  policy?: string;
  accountIndex?: number;
  constraintIndices?: readonly number[];
  mutateOuter?: (outer: Instruction) => Instruction;
}>;

const REQUIRED_MUTATION_NAMES = [
  "direct_voltr_without_ticket", "consume_before_arm", "arm_only_payload",
  "reversed_instruction_order", "extra_third_instruction", "different_second_instruction",
  "second_consume", "same_sequence_rearm", "lower_sequence_rearm", "arm_while_active",
  "nonsigner_squads", "wrong_squads_vault", "wrong_settings_owner",
  "wrong_settings_or_index", "address_only_lookalike", "wrong_delegated_executor",
  "wrong_policy", "wrong_voltr_authority", "wrong_ticket_pda", "wrong_ticket_owner",
  "wrong_ticket_config", "wrong_ticket_index", "readonly_ticket", "wrong_operation",
  "wrong_amount", "wrong_wire_hash", "zero_sequence", "sequence_below_observed_slot",
  "sequence_above_observed_slot", "stale_slot", "future_slot", "oversized_amount",
  "oversized_nav", "trailing_bytes", "wrong_vault_or_strategy",
  "wrong_mint_or_token_program", "wrong_ata", "duplicate_writable_alias",
  "voltr_failure_rolls_back_ticket_and_capital",
] as const;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function wire(value: Instruction): WireInstruction {
  return {
    programId: value.programAddress,
    accounts: (value.accounts ?? []).map((account) => ({
      address: account.address,
      signer: account.role === AccountRole.READONLY_SIGNER || account.role === AccountRole.WRITABLE_SIGNER,
      writable: account.role === AccountRole.WRITABLE || account.role === AccountRole.WRITABLE_SIGNER,
    })),
    dataBase64: Buffer.from(value.data ?? []).toString("base64"),
  };
}

function instruction(value: WireInstruction): Instruction {
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

function compileWrapper(input: MutationBuild): Instruction {
  const inner = input.inner;
  const source = JSON.stringify({
    policy: input.policy ?? NAV_POLICY,
    delegatedSigner: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
    accountIndex: input.accountIndex ?? 0,
    constraintIndices: input.constraintIndices ?? inner.map((_, index) => index),
    inner: inner.map(wire),
  });
  const result = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin", COMPILER], {
    cwd: REPOSITORY_ROOT,
    input: source,
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
  });
  invariant(result.status === 0, `execution compiler failed: ${(result.stderr || result.stdout).trim()}`);
  const output = JSON.parse(result.stdout) as { schema?: unknown; instruction?: WireInstruction };
  invariant(output.schema === "loyal-voltr-custom-execution/v2" && output.instruction,
    "execution compiler escaped its exact v2 wrapper contract");
  const outer = instruction(output.instruction);
  return input.mutateOuter ? input.mutateOuter(outer) : outer;
}

function decodeConfig(data: Buffer): ConfigState {
  invariant(data.length === CONFIG_LEN
    && data.subarray(0, 8).equals(Buffer.from([46, 154, 12, 115, 203, 165, 199, 235]))
    && data[8] === 2 && data[9] === 0 && data.subarray(10, 16).every((value) => value === 0),
  "deployed adaptor config envelope drifted");
  const keyAt = (index: number) => new PublicKey(
    data.subarray(16 + index * 32, 48 + index * 32),
  ).toBase58();
  invariant(keyAt(11) === PublicKey.default.toBase58(), "deployed adaptor config reserved key is nonzero");
  invariant(data.readBigUInt64LE(416) === 0n
    && data.readBigUInt64LE(424) === 0n
    && data.readBigUInt64LE(432) === 0n
    && data.subarray(440, 472).every((value) => value === 0),
  "deployed adaptor immutable config tail is not zero-reserved");
  return {
    squadsVaultIndex: data[9]!,
    voltrProgram: keyAt(0),
    voltrVault: keyAt(1),
    strategy: keyAt(2),
    strategyAuthority: keyAt(3),
    squadsProgram: keyAt(4),
    squadsSettings: keyAt(5),
    squadsSettingsSigner: keyAt(6),
    squadsVault: keyAt(7),
    assetMint: keyAt(8),
    assetTokenProgram: keyAt(9),
    squadsAssetAta: keyAt(10),
    maxNavRaw: data.readBigUInt64LE(400),
    maxAgeSlots: data.readBigUInt64LE(408),
  };
}

function decodeTicket(data: Buffer, config: string): TicketState {
  invariant(data.length === TICKET_LEN
    && data.subarray(0, 8).equals(Buffer.from("f568b6c53ae774ed", "hex"))
    && data[8] === 1 && data[9] === 254
    && data.subarray(11, 16).every((value) => value === 0)
    && new PublicKey(data.subarray(16, 48)).toBase58() === config,
  "report ticket envelope or config binding drifted");
  return {
    armed: data[10] === 1,
    lastConsumedSequence: data.readBigUInt64LE(48),
    activeSequence: data.readBigUInt64LE(56),
    activeWireSha256: data.subarray(64, 96).toString("hex"),
  };
}

function programIdentity(addressValue: string, info: AccountInfo<Buffer>, dataInfo: AccountInfo<Buffer>) {
  invariant(info.executable && info.owner.equals(BPF_LOADER_UPGRADEABLE_PROGRAM_ID)
    && info.data.length === 36 && info.data.readUInt32LE(0) === 2,
  `${addressValue} is not an upgradeable program`);
  const programDataAddress = new PublicKey(info.data.subarray(4, 36));
  invariant(dataInfo.owner.equals(BPF_LOADER_UPGRADEABLE_PROGRAM_ID)
    && dataInfo.data.length > 45 && dataInfo.data.readUInt32LE(0) === 3,
  `${addressValue} ProgramData is malformed`);
  const headerLength = dataInfo.data[12] === 1 ? 45 : 13;
  return {
    program: addressValue,
    programData: programDataAddress.toBase58(),
    programDataSha256: sha256(dataInfo.data),
    elfSha256: sha256(dataInfo.data.subarray(headerLength)),
    deployedSlot: dataInfo.data.readBigUInt64LE(4).toString(),
    upgradeAuthority: headerLength === 45 ? new PublicKey(dataInfo.data.subarray(13, 45)).toBase58() : null,
  };
}

function normalizedAccount(info: AccountInfo<Buffer> | AccountSnapshot | null) {
  if (info === null) return null;
  return {
    dataBase64: Buffer.from(info.data).toString("base64"),
    executable: info.executable,
    lamports: info.lamports,
    owner: info.owner instanceof PublicKey ? info.owner.toBase58() : info.owner,
  };
}

function accountSetSha256(infos: readonly (AccountInfo<Buffer> | AccountSnapshot | null)[]): string {
  return sha256(JSON.stringify(infos.map(normalizedAccount)));
}

function sameSnapshot(info: AccountInfo<Buffer> | null, post: AccountSnapshot | null): boolean {
  return info === null ? post === null : post !== null
    && info.owner.toBase58() === post.owner
    && info.lamports === post.lamports
    && info.executable === post.executable
    && Buffer.from(info.data).equals(Buffer.from(post.data));
}

function snapshotDifferences(
  addresses: readonly string[],
  before: readonly (AccountInfo<Buffer> | null)[],
  after: readonly (AccountSnapshot | null)[],
) {
  return addresses.flatMap((account, index) => {
    const left = before[index] ?? null;
    const right = after[index] ?? null;
    if (sameSnapshot(left, right)) return [];
    const leftData = left ? Buffer.from(left.data) : null;
    const rightData = right ? Buffer.from(right.data) : null;
    const changedByteOffsets = leftData && rightData
      ? Array.from({ length: Math.min(leftData.length, rightData.length) }, (_, offset) => offset)
        .filter((offset) => leftData[offset] !== rightData[offset])
      : [];
    return [{
      account,
      before: left === null ? null : {
        owner: left.owner.toBase58(), lamports: left.lamports, executable: left.executable,
        dataLength: leftData!.length, dataSha256: sha256(leftData!),
      },
      after: right === null ? null : {
        owner: right.owner, lamports: right.lamports, executable: right.executable,
        dataLength: rightData!.length, dataSha256: sha256(rightData!),
      },
      changedBytes: changedByteOffsets.slice(0, 64).map((offset) => ({
        offset, before: leftData![offset], after: rightData![offset],
      })),
      changedByteCount: changedByteOffsets.length
        + Math.abs((leftData?.length ?? 0) - (rightData?.length ?? 0)),
    }];
  });
}

function mutateAccount(inner: Instruction, index: number, addressValue: string, role?: AccountRole): Instruction {
  const accounts = [...(inner.accounts ?? [])];
  invariant(accounts[index], `inner account ${index} is absent`);
  accounts[index] = { ...accounts[index]!, address: address(addressValue), ...(role === undefined ? {} : { role }) };
  return { ...inner, accounts };
}

function mutateData(inner: Instruction, mutate: (data: Buffer) => void): Instruction {
  const data = Buffer.from(inner.data ?? []);
  mutate(data);
  return { ...inner, data };
}

function writeU64(data: Buffer, offset: number, value: bigint): void {
  data.writeBigUInt64LE(value, offset);
}

async function main() {
  invariant(!process.argv.includes("--execute"), "this generator has no execute mode");
  const outIndex = process.argv.indexOf("--out");
  const outputPath = outIndex >= 0 ? resolve(process.argv[outIndex + 1] ?? "") : "";
  invariant(outputPath.endsWith(".json") && existsSync(dirname(outputPath)),
    "--out must name a .json file under an existing directory");
  invariant(!existsSync(outputPath), "--out is exclusive and already exists");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const signerName = process.env.POLICY_KEYPAIR ? "POLICY_KEYPAIR" : "SOLANA_TESTING_PK";
  const delegated = await signingMaterialFromEnvironment(signerName);
  invariant(delegated.signer.address === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
    `${signerName} is not the fixed delegated executor`);
  const connection = new Connection(rpcUrl, "confirmed");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash,
    "RPC is not Solana mainnet-beta");

  const accounts = await deriveRwaMultiplyVoltrAccounts(RWA_MULTIPLY_ROUTE);
  const configAddress = RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig;
  const initialAddresses = [configAddress, accounts.reportTicket,
    RWA_MULTIPLY_ROUTE.customAdaptor.program, RWA_MULTIPLY_ROUTE.programs.voltr,
    NAV_POLICY, accounts.strategyInitReceipt];
  const initial = await connection.getMultipleAccountsInfoAndContext(
    initialAddresses.map((value) => new PublicKey(value)), { commitment: "confirmed" });
  const [configInfo, ticketInfo, adaptorProgramInfo, voltrProgramInfo, navPolicyInfo, receiptInfo] = initial.value;
  invariant(configInfo?.owner.toBase58() === RWA_MULTIPLY_ROUTE.customAdaptor.program
    && ticketInfo?.owner.toBase58() === RWA_MULTIPLY_ROUTE.customAdaptor.program,
    "adaptor config or report ticket is absent or has the wrong owner");
  invariant(navPolicyInfo?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program,
    "NAV policy is absent or has the wrong owner");
  invariant(adaptorProgramInfo && voltrProgramInfo && receiptInfo, "deployed program or strategy receipt is absent");
  const adaptorDataAddress = new PublicKey(adaptorProgramInfo.data.subarray(4, 36));
  const voltrDataAddress = new PublicKey(voltrProgramInfo.data.subarray(4, 36));
  const [adaptorDataInfo, voltrDataInfo] = await connection.getMultipleAccountsInfo([
    adaptorDataAddress, voltrDataAddress,
  ], "confirmed");
  invariant(adaptorDataInfo && voltrDataInfo, "ProgramData account is absent");
  const adaptorIdentity = programIdentity(RWA_MULTIPLY_ROUTE.customAdaptor.program,
    adaptorProgramInfo, adaptorDataInfo);
  const voltrIdentity = programIdentity(RWA_MULTIPLY_ROUTE.programs.voltr,
    voltrProgramInfo, voltrDataInfo);
  const config = decodeConfig(configInfo.data);
  const ticketBefore = decodeTicket(ticketInfo.data, configAddress);
  invariant(!ticketBefore.armed && ticketBefore.activeSequence === 0n
    && ticketBefore.activeWireSha256 === ZERO_HASH,
  "report ticket must be inactive before signed-unsent proof");
  invariant(config.squadsVaultIndex === RWA_MULTIPLY_ROUTE.squads.vaultIndex
    && config.voltrProgram === RWA_MULTIPLY_ROUTE.programs.voltr
    && config.voltrVault === RWA_MULTIPLY_ROUTE.vault.address
    && config.strategy === configAddress
    && config.strategyAuthority === accounts.strategyAuth
    && config.squadsProgram === RWA_MULTIPLY_ROUTE.squads.program
    && config.squadsSettings === RWA_MULTIPLY_ROUTE.squads.settings
    && config.squadsSettingsSigner === RWA_MULTIPLY_ROUTE.customAdaptor.settingsSigner
    && config.squadsVault === RWA_MULTIPLY_ROUTE.squads.vault
    && config.assetMint === RWA_MULTIPLY_ROUTE.assets.assetMint
    && config.assetTokenProgram === RWA_MULTIPLY_ROUTE.assets.tokenProgram
    && config.squadsAssetAta === RWA_MULTIPLY_ROUTE.squads.assetAta,
  "deployed adaptor config authority graph drifted from the frozen route");
  const currentSlot = await connection.getSlot("confirmed");
  invariant(BigInt(currentSlot) > ticketBefore.lastConsumedSequence,
    "confirmed slot must exceed the ticket's consumed sequence");
  invariant(receiptInfo.data.length >= 112, "strategy receipt is too short for current NAV");

  const snapshotDigest = createHash("sha256")
    .update(configInfo.data).update(ticketInfo.data).update(receiptInfo.data)
    .update(navPolicyInfo.data).update(String(currentSlot)).digest();
  const canonicalReport: RwaReportV1 = {
    sequence: BigInt(currentSlot),
    observedSlot: BigInt(currentSlot),
    navAfterRaw: receiptInfo.data.readBigUInt64LE(104),
    snapshotDigest,
  };
  const manager = createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault);
  const makePair = async (report: RwaReportV1, amount = 0n,
    operation: "deposit" | "withdraw" = "deposit") => {
    const capital = await buildRwaMultiplyManagerInstructions(manager, amount, report, RWA_MULTIPLY_ROUTE);
    const arm = await buildRwaMultiplyArmReportInstruction(manager, operation, amount, report, RWA_MULTIPLY_ROUTE);
    return [arm, operation === "deposit" ? capital.deposit : capital.withdraw] as const;
  };
  const canonicalPair = await makePair(canonicalReport);
  const [canonicalArm, canonicalCapital] = canonicalPair;
  const canonicalArmWire = wire(canonicalArm);
  const canonicalCapitalWire = wire(canonicalCapital);
  const armBytes = Buffer.from(canonicalArm.data ?? []);
  const capitalBytes = Buffer.from(canonicalCapital.data ?? []);
  invariant(canonicalArmWire.programId === RWA_MULTIPLY_ROUTE.customAdaptor.program
    && canonicalArmWire.accounts.length === 5
    && canonicalArmWire.accounts[0]?.address === configAddress
    && canonicalArmWire.accounts[0]?.writable === false
    && canonicalArmWire.accounts[1]?.address === accounts.reportTicket
    && canonicalArmWire.accounts[1]?.writable === true
    && canonicalArmWire.accounts[3]?.address === RWA_MULTIPLY_ROUTE.squads.vault
    && canonicalArmWire.accounts[3]?.signer === true
    && armBytes.length === 79
    && canonicalCapitalWire.programId === RWA_MULTIPLY_ROUTE.programs.voltr
    && canonicalCapitalWire.accounts.length === 18
    && canonicalCapitalWire.accounts[17]?.address === accounts.reportTicket
    && canonicalCapitalWire.accounts[17]?.writable === true
    && capitalBytes.length === 91
    && armBytes.subarray(9).equals(Buffer.concat([capitalBytes.subarray(8, 16), capitalBytes.subarray(29)])),
  "canonical ArmReport/Voltr wire or ticket indexes drifted");
  const canonicalOuter = compileWrapper({ inner: canonicalPair, constraintIndices: [0, 1] });
  const inspectedAddresses = [configAddress, accounts.reportTicket, accounts.strategyInitReceipt,
    accounts.idleAta, accounts.strategyAssetAta, RWA_MULTIPLY_ROUTE.squads.assetAta];
  const preInfosResponse = await connection.getMultipleAccountsInfoAndContext(
    inspectedAddresses.map((value) => new PublicKey(value)), { commitment: "confirmed" });
  invariant(preInfosResponse.value.every((value) => value !== null), "protected simulation account is absent");
  const preStateSha256 = accountSetSha256(preInfosResponse.value);

  const prepare = async (outer: Instruction) => prepareSignedV0Transaction({
    rpcUrl, feePayer: delegated, commitment: "confirmed",
    minimumContextSlot: preInfosResponse.context.slot,
    instructions: [outer], prestateAddresses: inspectedAddresses, inspectedAddresses,
  });
  const canonical = await prepare(canonicalOuter);
  invariant(canonical.simulation.err === null,
    `canonical ticket simulation failed: ${JSON.stringify(canonical.simulation.err)}; logs=${JSON.stringify(canonical.simulation.logs)}`);
  const configAfter = canonical.simulation.postAccounts[inspectedAddresses.indexOf(configAddress)];
  const ticketAfterSnapshot = canonical.simulation.postAccounts[inspectedAddresses.indexOf(accounts.reportTicket)];
  invariant(configAfter && ticketAfterSnapshot, "canonical simulation omitted config/ticket poststate");
  const ticketAfter = decodeTicket(Buffer.from(ticketAfterSnapshot.data), configAddress);
  invariant(!ticketAfter.armed && ticketAfter.lastConsumedSequence === canonicalReport.sequence
    && ticketAfter.activeSequence === 0n && ticketAfter.activeWireSha256 === ZERO_HASH,
  "canonical simulation did not consume and clear the one-use ticket");
  invariant(sha256(configInfo.data) === sha256(configAfter.data), "canonical simulation mutated immutable config");

  const wrong = RWA_MULTIPLY_ROUTE.setupAdmin;
  const system = RWA_MULTIPLY_ROUTE.programs.system;
  const mutationSpecs: readonly Readonly<{ name: string; build(): Promise<MutationBuild> }>[] = [
    { name: "direct_voltr_without_ticket", build: async () => ({ inner: [canonicalCapital], constraintIndices: [1] }) },
    { name: "consume_before_arm", build: async () => ({ inner: [canonicalCapital, canonicalArm], constraintIndices: [1, 0] }) },
    { name: "arm_only_payload", build: async () => ({ inner: [canonicalArm], constraintIndices: [0] }) },
    { name: "reversed_instruction_order", build: async () => ({ inner: [canonicalCapital, canonicalArm], constraintIndices: [0, 1] }) },
    { name: "extra_third_instruction", build: async () => ({ inner: [canonicalArm, canonicalCapital,
      { programAddress: system, accounts: [], data: new Uint8Array() }], constraintIndices: [0, 1, 0] }) },
    { name: "different_second_instruction", build: async () => ({ inner: [canonicalArm, canonicalArm], constraintIndices: [0, 1] }) },
    { name: "second_consume", build: async () => ({ inner: [canonicalArm, canonicalCapital, canonicalCapital], constraintIndices: [0, 1, 1] }) },
    { name: "same_sequence_rearm", build: async () => ({ inner: await makePair({ ...canonicalReport,
      sequence: ticketBefore.lastConsumedSequence, observedSlot: ticketBefore.lastConsumedSequence }) }) },
    { name: "lower_sequence_rearm", build: async () => { const sequence = ticketBefore.lastConsumedSequence > 0n ? ticketBefore.lastConsumedSequence - 1n : 0n;
      return { inner: await makePair({ ...canonicalReport, sequence, observedSlot: sequence }), constraintIndices: [1, 0] }; } },
    { name: "arm_while_active", build: async () => ({ inner: [canonicalArm, canonicalArm], constraintIndices: [0, 0] }) },
    { name: "nonsigner_squads", build: async () => ({ inner: [mutateAccount(canonicalArm, 3,
      RWA_MULTIPLY_ROUTE.squads.vault, AccountRole.READONLY), canonicalCapital] }) },
    { name: "wrong_squads_vault", build: async () => ({ inner: [mutateAccount(canonicalArm, 3, wrong,
      AccountRole.READONLY_SIGNER), canonicalCapital] }) },
    { name: "wrong_settings_owner", build: async () => ({ inner: [mutateAccount(canonicalArm, 2, system), canonicalCapital] }) },
    { name: "wrong_settings_or_index", build: async () => ({ inner: canonicalPair, accountIndex: 1 }) },
    { name: "address_only_lookalike", build: async () => ({ inner: [mutateAccount(canonicalArm, 3,
      RWA_MULTIPLY_ROUTE.squads.vault, AccountRole.READONLY), canonicalCapital], constraintIndices: [0, 0] }) },
    { name: "wrong_delegated_executor", build: async () => ({ inner: canonicalPair,
      mutateOuter: (outer) => mutateAccount(outer, 2, wrong, AccountRole.READONLY) }) },
    { name: "wrong_policy", build: async () => ({ inner: canonicalPair, policy: configAddress }) },
    { name: "wrong_voltr_authority", build: async () => ({ inner: [canonicalArm, mutateAccount(canonicalCapital, 7, configAddress)] }) },
    { name: "wrong_ticket_pda", build: async () => ({ inner: [mutateAccount(canonicalArm, 1, wrong), mutateAccount(canonicalCapital, 17, wrong)] }) },
    { name: "wrong_ticket_owner", build: async () => ({ inner: [mutateAccount(canonicalArm, 1, system), mutateAccount(canonicalCapital, 17, system)] }) },
    { name: "wrong_ticket_config", build: async () => ({ inner: [mutateAccount(canonicalArm, 1, configAddress), mutateAccount(canonicalCapital, 17, configAddress)] }) },
    { name: "wrong_ticket_index", build: async () => ({ inner: [canonicalArm, mutateAccount(canonicalCapital, 17, accounts.strategyAssetAta)] }) },
    { name: "readonly_ticket", build: async () => ({ inner: [mutateAccount(canonicalArm, 1, accounts.reportTicket, AccountRole.READONLY),
      mutateAccount(canonicalCapital, 17, accounts.reportTicket, AccountRole.READONLY)] }) },
    { name: "wrong_operation", build: async () => ({ inner: [mutateData(canonicalArm, (data) => { data[8] = 1; }), canonicalCapital] }) },
    { name: "wrong_amount", build: async () => ({ inner: [mutateData(canonicalArm, (data) => writeU64(data, 9, 1n)), canonicalCapital] }) },
    { name: "wrong_wire_hash", build: async () => ({ inner: [mutateData(canonicalArm, (data) => {
      data[data.length - 1] = data[data.length - 1]! ^ 1;
    }), canonicalCapital] }) },
    { name: "zero_sequence", build: async () => ({ inner: await makePair({ ...canonicalReport, sequence: 0n, observedSlot: 0n }) }) },
    { name: "sequence_below_observed_slot", build: async () => ({ inner: await makePair({ ...canonicalReport, sequence: canonicalReport.observedSlot - 1n }) }) },
    { name: "sequence_above_observed_slot", build: async () => ({ inner: await makePair({ ...canonicalReport, sequence: canonicalReport.observedSlot + 1n }) }) },
    { name: "stale_slot", build: async () => { const slot = BigInt(currentSlot) - config.maxAgeSlots - 1n;
      return { inner: await makePair({ ...canonicalReport, sequence: slot, observedSlot: slot }) }; } },
    { name: "future_slot", build: async () => { const slot = BigInt(currentSlot) + 100n;
      return { inner: await makePair({ ...canonicalReport, sequence: slot, observedSlot: slot }) }; } },
    { name: "oversized_amount", build: async () => ({ inner: [
      mutateData(canonicalArm, (data) => writeU64(data, 9, RWA_MULTIPLY_ROUTE.vault.capRaw + 1n)),
      mutateData(canonicalCapital, (data) => writeU64(data, 8, RWA_MULTIPLY_ROUTE.vault.capRaw + 1n))] }) },
    { name: "oversized_nav", build: async () => ({ inner: await makePair({ ...canonicalReport, navAfterRaw: config.maxNavRaw + 1n }) }) },
    { name: "trailing_bytes", build: async () => ({ inner: [canonicalArm, { ...canonicalCapital,
      data: Uint8Array.from([...(canonicalCapital.data ?? []), 0]) }] }) },
    { name: "wrong_vault_or_strategy", build: async () => ({ inner: [canonicalArm, mutateAccount(canonicalCapital, 3, wrong)] }) },
    { name: "wrong_mint_or_token_program", build: async () => ({ inner: [canonicalArm, mutateAccount(canonicalCapital, 8, wrong)] }) },
    { name: "wrong_ata", build: async () => ({ inner: [canonicalArm, mutateAccount(canonicalCapital, 16, accounts.strategyAssetAta)] }) },
    { name: "duplicate_writable_alias", build: async () => ({ inner: [canonicalArm, mutateAccount(canonicalCapital, 16, accounts.reportTicket)] }) },
    { name: "voltr_failure_rolls_back_ticket_and_capital", build: async () => { const pair = await makePair(canonicalReport, 1n);
      return { inner: [pair[0], mutateAccount(pair[1], 11, accounts.strategyAssetAta, AccountRole.READONLY)],
        policy: ALLOCATION_POLICY, constraintIndices: [0, 1] }; } },
  ];

  const capitalWireSha256 = sha256(Buffer.concat([
    Buffer.from([242, 35, 198, 137, 82, 225, 242, 182]), armBytes.subarray(9),
  ]));
  const mutations = [];
  for (const spec of mutationSpecs) {
    const built = await spec.build();
    const prepared = await prepare(compileWrapper(built));
    const armOnlyExpectedSuccess = spec.name === "arm_only_payload";
    invariant(armOnlyExpectedSuccess
      ? prepared.simulation.err === null
      : prepared.simulation.err !== null,
    `${spec.name} simulation outcome escaped the v10 contract`);
    const simulationNullAddresses = inspectedAddresses.filter(
      (_, index) => prepared.simulation.postAccounts[index] === null,
    );
    const simulationPostAccountsAvailable = simulationNullAddresses.length === 0;
    invariant(simulationPostAccountsAvailable || simulationNullAddresses.length === inspectedAddresses.length,
      `${spec.name} simulation returned only a partial protected account image: ${JSON.stringify(simulationNullAddresses)}`);
    const simulationAccountDifferences = snapshotDifferences(
      inspectedAddresses, preInfosResponse.value, prepared.simulation.postAccounts,
    );
    // A null RPC overlay is unavailable evidence, not evidence that every
    // protected account changed. Only enumerate diffs from concrete images.
    const simulationChangedAddresses = simulationPostAccountsAvailable
      ? simulationAccountDifferences.map(({ account }) => account)
      : [];
    let armOnlyTicketTransition = null;
    let downstreamRollbackProof = null;
    if (armOnlyExpectedSuccess) {
      invariant(simulationPostAccountsAvailable
        && simulationChangedAddresses.length === 1
        && simulationChangedAddresses[0] === accounts.reportTicket,
      `arm_only_payload did not change exactly the report ticket: ${JSON.stringify(simulationAccountDifferences)}`);
      const ticketSnapshot = prepared.simulation.postAccounts[inspectedAddresses.indexOf(accounts.reportTicket)];
      invariant(ticketSnapshot !== null && ticketSnapshot !== undefined,
        "arm_only_payload omitted the report ticket poststate");
      const armedTicket = decodeTicket(Buffer.from(ticketSnapshot.data), configAddress);
      invariant(armedTicket.armed
        && armedTicket.lastConsumedSequence === ticketBefore.lastConsumedSequence
        && armedTicket.activeSequence === canonicalReport.sequence
        && armedTicket.activeWireSha256 === capitalWireSha256,
      "arm_only_payload ticket overlay is not the exact canonical armed state");
      armOnlyTicketTransition = {
        armed: true,
        lastConsumedSequence: armedTicket.lastConsumedSequence.toString(),
        activeSequence: armedTicket.activeSequence.toString(),
        activeWireSha256: armedTicket.activeWireSha256,
      };
    }
    if (spec.name === DOWNSTREAM_ROLLBACK_MUTATION) {
      invariant(built.inner.length === 2
        && JSON.stringify(wire(built.inner[0]!)) === JSON.stringify(canonicalArmWire),
      "dedicated downstream failure did not preserve the exact canonical ArmReport wire");
      const logProof = downstreamRollbackLogProof(
        prepared.simulation.logs,
        RWA_MULTIPLY_ROUTE.customAdaptor.program,
        RWA_MULTIPLY_ROUTE.programs.voltr,
      );
      invariant(logProof !== null,
        "dedicated downstream failure did not prove ArmReport success before Voltr failure");
      downstreamRollbackProof = {
        mode: simulationPostAccountsAvailable
          ? "simulation-poststate"
          : "all-null-overlay-confirmed-readback",
        canonicalArmWireExact: true,
        atomicTransactionFailed: true,
        ...logProof,
      };
    }
    if (!armOnlyExpectedSuccess) {
      invariant(failedSimulationOverlayAccepted({
        mutationName: spec.name,
        inspectedAddresses,
        postAccountsAvailable: simulationPostAccountsAvailable,
        nullAddresses: simulationNullAddresses,
        changedAddresses: simulationChangedAddresses,
        downstreamRollbackProven: downstreamRollbackProof !== null,
      }), `${spec.name} lacks the required concrete unchanged simulation poststate`);
    }
    const chainReadback = await confirmedAccountsAtOrAfter(
      connection, inspectedAddresses, prepared.simulationSlot,
    );
    invariant(chainReadback.value.every((value, index) => sameSnapshot(
      preInfosResponse.value[index] ?? null,
      value === null ? null : {
        address: inspectedAddresses[index]!,
        owner: value.owner.toBase58(),
        executable: value.executable,
        lamports: value.lamports,
        data: value.data,
      },
    )), `${spec.name} independent confirmed readback detected protected state drift`);
    const signatureStatuses = await connection.getSignatureStatuses(
      [prepared.expectedSignature], { searchTransactionHistory: true },
    );
    invariant(signatureStatuses.value.length === 1 && signatureStatuses.value[0] === null,
      `${spec.name} signed-unsent wire unexpectedly has an on-chain signature status`);
    const logsSha256 = sha256(prepared.simulation.logs.join("\n"));
    const chainReadbackStateSha256 = accountSetSha256(chainReadback.value);
    const postStateSha256 = chainReadbackStateSha256;
    invariant(postStateSha256 === preStateSha256,
      `${spec.name} independent chain readback detected signed-unsent state drift`);
    mutations.push({
      name: spec.name,
      expectation: armOnlyExpectedSuccess ? "arm-only-success" : "rejection",
      transactionBase64: Buffer.from(prepared.serializedTransaction).toString("base64"),
      transactionSha256: sha256(prepared.serializedTransaction),
      messageSha256: sha256(prepared.serializedMessage),
      inspectedAddresses,
      logsSha256,
      preStateSha256,
      postStateSha256,
      simulationPostAccountsAvailable,
      simulationNullAddresses,
      simulationChangedAddresses,
      simulationStateSha256: accountSetSha256(prepared.simulation.postAccounts),
      armOnlyTicketTransition,
      downstreamRollbackProof,
      chainReadbackContextSlot: chainReadback.context.slot,
      chainReadbackStateSha256,
      signatureStatus: null,
      error: prepared.simulation.err === null ? null : JSON.stringify(prepared.simulation.err),
      rejectedBeforeMutation: !armOnlyExpectedSuccess && spec.name !== DOWNSTREAM_ROLLBACK_MUTATION,
      simulation: { sigVerify: true, replaceRecentBlockhash: false,
        commitment: "confirmed", contextSlot: prepared.simulationSlot },
    });
  }
  invariant(new Set(mutations.map(({ name }) => name)).size === REQUIRED_MUTATION_NAMES.length
    && [...mutations.map(({ name }) => name)].sort().join("|")
      === [...REQUIRED_MUTATION_NAMES].sort().join("|"),
  "exact v10 matrix names drifted");
  invariant(mutations.filter(({ expectation }) => expectation === "rejection").length === 38
    && mutations.filter(({ expectation }) => expectation === "arm-only-success").length === 1,
  "exact v10 rejection/expected-success cardinality drifted");
  const artifact = {
    schema: "loyal-backyard-rwa-adaptor-simulation/v2",
    broadcast: false,
    signedUnsent: true,
    path: "Squads->[ArmReport,Voltr->adaptor]",
    success: true,
    cluster: "mainnet-beta",
    genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    commitment: "confirmed",
    manifestSha256: sha256(readFileSync(MANIFEST_PATH)),
    programElfSha256: adaptorIdentity.elfSha256,
    programDataSha256: adaptorIdentity.programDataSha256,
    configDataSha256: sha256(configInfo.data),
    transactionBase64: Buffer.from(canonical.serializedTransaction).toString("base64"),
    transactionSha256: sha256(canonical.serializedTransaction),
    messageSha256: sha256(canonical.serializedMessage),
    inspectedAddresses,
    simulation: { sigVerify: true, replaceRecentBlockhash: false, err: null,
      commitment: "confirmed", contextSlot: canonical.simulationSlot,
      logsSha256: sha256(canonical.simulation.logs.join("\n")),
      returnData: canonical.simulation.returnData,
      configPreStateSha256: sha256(configInfo.data),
      configPostStateSha256: sha256(configAfter.data) },
    report: {
      sequence: canonicalReport.sequence.toString(),
      observedSlot: canonicalReport.observedSlot.toString(),
      navAfterRaw: canonicalReport.navAfterRaw.toString(),
      snapshotDigest: Buffer.from(canonicalReport.snapshotDigest).toString("hex"),
    },
    topology: {
      squadsInnerInstructionCount: 2,
      orderedInstructions: ["ArmReport", "VoltrCapital"],
      voltrRemainingTicketIndex: 17,
      adaptorTicketIndex: 8,
      ticketWritable: true,
      threeInstructionFallback: false,
      capitalWireSha256,
    },
    ticket: {
      address: accounts.reportTicket,
      bump: 254,
      config: configAddress,
      before: {
        armed: ticketBefore.armed,
        lastConsumedSequence: ticketBefore.lastConsumedSequence.toString(),
        activeSequence: ticketBefore.activeSequence.toString(),
        activeWireSha256: ticketBefore.activeWireSha256,
      },
      after: {
        armed: ticketAfter.armed,
        lastConsumedSequence: ticketAfter.lastConsumedSequence.toString(),
        activeSequence: ticketAfter.activeSequence.toString(),
        activeWireSha256: ticketAfter.activeWireSha256,
      },
    },
    bindings: {
      voltrProgram: RWA_MULTIPLY_ROUTE.programs.voltr,
      voltrVault: RWA_MULTIPLY_ROUTE.vault.address,
      strategyConfig: configAddress,
      strategyAuthority: accounts.strategyAuth,
      adaptorProgram: RWA_MULTIPLY_ROUTE.customAdaptor.program,
      squadsProgram: RWA_MULTIPLY_ROUTE.squads.program,
      squadsSettings: RWA_MULTIPLY_ROUTE.squads.settings,
      squadsVaultIndex: RWA_MULTIPLY_ROUTE.squads.vaultIndex,
      squadsVault: RWA_MULTIPLY_ROUTE.squads.vault,
      delegatedExecutor: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor,
      squadsAssetAta: RWA_MULTIPLY_ROUTE.squads.assetAta,
      reportTicket: accounts.reportTicket,
    },
    armSignerMetas: [
      { address: RWA_MULTIPLY_ROUTE.squads.vault, isSigner: true,
        source: "Squads invoke_signed vault at direct ArmReport" },
    ],
    consumeSignerMetas: [
      { address: accounts.strategyAuth, isSigner: true,
        source: "Voltr invoke_signed strategy authority at adaptor consume" },
    ],
    deployedPrograms: { adaptor: adaptorIdentity, voltr: voltrIdentity },
    mutations,
  };
  writeFileSync(outputPath, `${JSON.stringify(artifact, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  chmodSync(outputPath, 0o600);
  console.log(JSON.stringify({ verdict: "SIGNED_UNSENT_PASS", broadcast: false, outputPath,
    canonicalContextSlot: canonical.simulationSlot, mutationCount: mutations.length,
    transactionSha256: artifact.transactionSha256, ticket: accounts.reportTicket,
    configDataSha256: artifact.configDataSha256,
    programElfSha256: artifact.programElfSha256 }, null, 2));
}

main().catch((error) => {
  const message = error instanceof Error ? error.message : String(error);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  console.error(rpcUrl ? message.replaceAll(rpcUrl, "<rpc>") : message);
  process.exitCode = 1;
});
