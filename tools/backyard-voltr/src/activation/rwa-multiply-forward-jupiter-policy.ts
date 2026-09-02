import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { AccountRole, address, type Instruction } from "@solana/kit";
import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { prepareSignedV0Transaction } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  FORWARD_JUPITER_AMOUNT_OFFSET,
  FORWARD_JUPITER_DATA_LENGTH,
  FORWARD_JUPITER_POLICY_SEED,
  compileCurrentForwardJupiterPolicy,
  forwardJupiterPolicyAddress,
  type ForwardJupiterPolicyArtifact,
} from "../policies/rwa-multiply-forward-jupiter-policy.js";

const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const MANIFEST_PATH = resolve(REPOSITORY_ROOT, "docs/manifests/backyard-rwa-v1.json");
const PACKET_LIMIT = 1_232;
const MAX_POLICY_COST_LAMPORTS = 20_000_000;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function instruction(value: ForwardJupiterPolicyArtifact["policies"][number]["createInstruction"]): Instruction {
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

function protectedPolicySet(target: string): { addresses: string[]; legacyReverseSwapPolicy: string } {
  const manifest = JSON.parse(readFileSync(MANIFEST_PATH, "utf8")) as {
    runtimeBindings?: {
      bridgePolicies?: readonly { account?: unknown }[];
      primeUsdc?: {
        packets?: readonly { policy?: unknown }[];
        swapPolicies?: readonly { action?: unknown; policy?: unknown }[];
      };
    };
  };
  const swapPolicies = manifest.runtimeBindings?.primeUsdc?.swapPolicies ?? [];
  const forward = swapPolicies.filter(({ action }) => action === "SWAP_USDC_TO_PRIME_STEP");
  invariant(forward.length === 1 && forward[0]?.policy === target,
    "checked-in manifest does not bind the exact seed-66 forward policy target");
  const reverse = swapPolicies.filter(({ action }) => action === "SWAP_PRIME_TO_USDC_STEP");
  invariant(reverse.length === 1 && typeof reverse[0]?.policy === "string"
    && reverse[0].policy !== target,
    "checked-in manifest does not bind one distinct legacy reverse-swap policy");
  const legacyReverseSwapPolicy = reverse[0].policy;
  const values = [
    ...(manifest.runtimeBindings?.primeUsdc?.packets ?? []).map(({ policy }) => policy),
    legacyReverseSwapPolicy,
    ...(manifest.runtimeBindings?.bridgePolicies ?? []).map(({ account }) => account),
  ].filter((value): value is string => typeof value === "string");
  const unique = [...new Set(values)];
  invariant(!unique.includes(target), "seed-66 target leaked into the protected seed 57-65 policy set");
  invariant(unique.length === 9, "checked-in manifest is not the exact protected seed 57-65 policy set");
  return { addresses: unique, legacyReverseSwapPolicy };
}

function writePrivate(path: string, value: Record<string, unknown>, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag, mode: 0o600 });
  chmodSync(path, 0o600);
}

async function readProtected(connection: Connection, addresses: readonly string[], minContextSlot?: number) {
  const response = await connection.getMultipleAccountsInfoAndContext(
    addresses.map((value) => new PublicKey(value)), {
      commitment: "finalized", ...(minContextSlot === undefined ? {} : { minContextSlot }),
    });
  invariant(response.value.every((value) => value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program),
    "one or more protected seed 57-65 policies are absent or have the wrong owner");
  return { contextSlot: response.context.slot, hashes: response.value.map((value) => sha256(value!.data)) };
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
  const connection = new Connection(rpcUrl, "finalized");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash,
    "RPC is not mainnet-beta");
  const target = forwardJupiterPolicyAddress();
  const { addresses: protectedAddresses, legacyReverseSwapPolicy } = protectedPolicySet(target);

  if (reconcile) {
    const pending = JSON.parse(readFileSync(`${journal}.pending`, "utf8")) as {
      policy?: unknown;
      seed?: unknown;
      transaction?: {
        expectedSignature?: unknown;
        projectedPolicyDataSha256?: unknown;
        projectedSettingsDataSha256?: unknown;
        protectedPolicyDataSha256?: unknown;
      };
    };
    invariant(pending.policy === target && pending.seed === FORWARD_JUPITER_POLICY_SEED.toString(),
      "pending journal is not the exact seed-66 forward policy");
    const signature = String(pending.transaction?.expectedSignature ?? "");
    invariant(signature.length > 0, "pending journal lacks the signed-wire signature");
    const status = await connection.getSignatureStatuses([signature], { searchTransactionHistory: true });
    const landed = status.value[0];
    invariant(landed?.err === null && landed.confirmationStatus === "finalized",
      "pending signature is not finalized successfully");
    const [policy, settings] = await connection.getMultipleAccountsInfo(
      [new PublicKey(target), new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings)], "finalized");
    invariant(policy?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program
      && sha256(policy.data) === pending.transaction?.projectedPolicyDataSha256,
    "finalized seed-66 policy differs from the signed simulation projection");
    invariant(settings?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program
      && sha256(settings.data) === pending.transaction?.projectedSettingsDataSha256,
    "finalized Settings differs from the signed simulation projection");
    const protectedRead = await readProtected(connection, protectedAddresses, status.context.slot);
    invariant(JSON.stringify(protectedRead.hashes) === JSON.stringify(pending.transaction?.protectedPolicyDataSha256),
      "a protected seed 57-65 policy changed during forward policy activation");
    const binding = {
    action: "SWAP_USDC_TO_PRIME_STEP",
    policy: target,
    policyAccountDataSha256: sha256(policy.data),
      constraintBindings: [
        { routePlanPrefixHex: "01010000007400640001", policyConstraintIndex: 0 },
        { routePlanPrefixHex: "02010000007400640001", policyConstraintIndex: 1 },
      ],
      instructionDataLength: FORWARD_JUPITER_DATA_LENGTH,
      amountOffset: FORWARD_JUPITER_AMOUNT_OFFSET,
    } as const;
    writePrivate(journal, { ...pending, verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: landed.slot, finalizedContextSlot: status.context.slot, manifestBinding: binding }, "wx");
    renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({ verdict: "FINALIZED_RECONCILED", signature,
      finalizedSlot: landed.slot, manifestBinding: binding, journal }, null, 2));
    return;
  }

  const compiled = await compileCurrentForwardJupiterPolicy(connection);
  const policy = compiled.artifact.policies[0];
  invariant(policy.policy === target && policy.seed === FORWARD_JUPITER_POLICY_SEED.toString(),
    "compiler did not select the exact next seed");
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === RWA_MULTIPLY_ROUTE.setupAdmin, "setup admin signer drifted");
  const [settingsBefore, targetBefore] = await connection.getMultipleAccountsInfo(
    [new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), new PublicKey(target)], "finalized");
  invariant(settingsBefore?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program && targetBefore === null,
    "seed-66 create prestate drifted after compilation");
  const protectedBefore = await readProtected(connection, protectedAddresses, compiled.settingsSlot);
  const legacyReverseIndex = protectedAddresses.indexOf(legacyReverseSwapPolicy);
  invariant(legacyReverseIndex >= 0, "legacy reverse-swap policy is absent from the protected set");
  const inspectedAddresses = [target, RWA_MULTIPLY_ROUTE.squads.settings, legacyReverseSwapPolicy];
  const prepared = await prepareSignedV0Transaction({
    rpcUrl, feePayer: admin, commitment: "finalized", minimumContextSlot: protectedBefore.contextSlot,
    instructions: [instruction(policy.createInstruction)], inspectedAddresses,
  });
  invariant(policy.createPacketBytes <= PACKET_LIMIT && prepared.packetBytes <= PACKET_LIMIT,
    `seed-66 create packet exceeds the packet limit: compiler=${policy.createPacketBytes} signedV0=${prepared.packetBytes}`);
  invariant(prepared.simulation.err === null,
    `seed-66 create simulation failed: ${JSON.stringify(prepared.simulation.err)}`);
  invariant(prepared.simulation.postAccounts.every((value) => value !== null),
    "seed-66 create simulation omitted protected account images");
  const [postPolicy, postSettings, postLegacySwap] = prepared.simulation.postAccounts;
  invariant(postPolicy?.owner === RWA_MULTIPLY_ROUTE.squads.program
    && postSettings?.owner === RWA_MULTIPLY_ROUTE.squads.program,
  "seed-66 simulation projected an invalid owner");
  invariant(postLegacySwap !== null && postLegacySwap !== undefined
    && sha256(postLegacySwap.data) === protectedBefore.hashes[legacyReverseIndex],
  "seed-66 simulation changed the legacy reverse-swap policy");
  const protectedAfter = await readProtected(connection, protectedAddresses, prepared.simulationSlot);
  invariant(JSON.stringify(protectedAfter.hashes) === JSON.stringify(protectedBefore.hashes),
    "seed-66 simulation readback detected seed 57-65 policy drift");
  const projectedCostLamports = postPolicy.lamports + prepared.feeLamports;
  invariant(projectedCostLamports <= MAX_POLICY_COST_LAMPORTS,
    "seed-66 projected activation cost exceeds the bounded limit");
  const binding = {
    action: "SWAP_USDC_TO_PRIME_STEP",
    policy: target,
    policyAccountDataSha256: sha256(postPolicy.data),
    constraintBindings: [
      { routePlanPrefixHex: "01010000007400640001", policyConstraintIndex: 0 },
      { routePlanPrefixHex: "02010000007400640001", policyConstraintIndex: 1 },
    ],
    instructionDataLength: FORWARD_JUPITER_DATA_LENGTH,
    amountOffset: FORWARD_JUPITER_AMOUNT_OFFSET,
  } as const;
  const plan = {
    schema: "loyal-backyard-rwa-forward-jupiter-policy-activation/v1",
    verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS",
    broadcast: execute,
    seed: policy.seed,
    policy: target,
    header: compiled.header,
    compilerInput: compiled.compilerInput,
    artifact: compiled.artifact,
    manifestBinding: binding,
    transaction: {
      packetBytes: prepared.packetBytes,
      compilerLegacyPacketBytes: policy.createPacketBytes,
      unitsConsumed: prepared.simulation.unitsConsumed,
      feeLamports: prepared.feeLamports,
      projectedCostLamports,
      expectedSignature: prepared.expectedSignature,
      wireSha256: sha256(prepared.serializedTransaction),
      projectedPolicyDataSha256: sha256(postPolicy.data),
      projectedSettingsDataSha256: sha256(postSettings.data),
      protectedPolicyDataSha256: protectedBefore.hashes,
    },
  };
  if (!execute) {
    console.log(JSON.stringify(plan, null, 2));
    return;
  }
  writePrivate(`${journal}.pending`, { ...plan,
    signedWireBase64: Buffer.from(prepared.serializedTransaction).toString("base64") }, "wx");
  const returned = await connection.sendRawTransaction(prepared.serializedTransaction, {
    skipPreflight: false, preflightCommitment: "finalized", maxRetries: 0,
    minContextSlot: prepared.simulationSlot,
  });
  invariant(returned === prepared.expectedSignature,
    "RPC returned a signature different from the persisted wire");
  console.log(JSON.stringify({ verdict: "SENT_RECONCILIATION_REQUIRED", signature: returned,
    journalPending: `${journal}.pending` }, null, 2));
}

main().catch((error) => {
  console.error(JSON.stringify({ verdict: "BLOCKED",
    blocker: error instanceof Error
      ? error.message.replace(process.env.SOLANA_RPC_URL ?? "", "<rpc>") : String(error) }));
  process.exitCode = 1;
});
