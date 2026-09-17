import { verifyPendingSignedWire } from "./pending-signed-wire.js";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { Connection, PublicKey, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import { createHash } from "node:crypto";
import { address, createNoopSigner, getAddressDecoder, getU64Encoder, type TransactionSigner } from "@solana/kit";
import { getUpdateVaultConfigInstructionAsync, getVaultDecoder, getVaultEncoder, VaultConfigField } from "@voltr/vault-sdk";
import { confirmedSnapshots, finalizedSnapshots, toWeb3Instruction, prepareSignedV0Transaction, sendPreparedConfirmedOnce, type AccountSnapshot } from "../integrations/solana-compat.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";

export const PILOT_CAP_RAW = 100_000_000n;
const VAULT = address("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA");
const ADMIN = address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ");
const PROGRAM = address("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8");
const PROGRAM_DATA = "3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az";
const LOADER = "BPFLoaderUpgradeab1e11111111111111111111111";
const MAX_FEE = 50_000;
const sha = (data: Uint8Array) => createHash("sha256").update(data).digest("hex");

function decodeVault(account: AccountSnapshot | null) {
  if (!account || account.address !== VAULT || account.owner !== PROGRAM || account.executable || account.lamports <= 0) throw new Error("Pilot vault identity mismatch");
  const vault = getVaultDecoder().decode(account.data);
  if (vault.admin !== ADMIN || vault.version !== 4 || !Buffer.from(getVaultEncoder().encode(vault)).equals(Buffer.from(account.data))) throw new Error("Pilot vault authority or layout mismatch");
  return vault;
}

/** Exactly one instruction; no fee calibration, withdrawal or custody changes. */
export async function pilotCapInstruction(admin: TransactionSigner) {
  if (admin.address !== ADMIN) throw new Error("Pilot cap signer differs from approved admin");
  return getUpdateVaultConfigInstructionAsync({ admin, vault: VAULT, field: VaultConfigField.MaxCap,
    data: getU64Encoder().encode(PILOT_CAP_RAW) }, { programAddress: PROGRAM });
}

export function verifyPilotCapChange(before: AccountSnapshot | null, after: AccountSnapshot | null): void {
  const oldVault = decodeVault(before);
  const newVault = decodeVault(after);
  if (oldVault.asset.totalValue > PILOT_CAP_RAW) throw new Error("Vault value already exceeds pilot cap");
  if (newVault.lastUpdatedTs < oldVault.lastUpdatedTs) throw new Error("Cap update moved the vault timestamp backwards");
  // The pinned Voltr binary stamps lastUpdatedTs on every configuration write.
  const expected = getVaultEncoder().encode({ ...oldVault, lastUpdatedTs: newVault.lastUpdatedTs, vaultConfiguration: { ...oldVault.vaultConfiguration, maxCap: PILOT_CAP_RAW } });
  if (newVault.vaultConfiguration.maxCap !== PILOT_CAP_RAW || before!.lamports !== after!.lamports ||
      !Buffer.from(expected).equals(Buffer.from(after!.data))) throw new Error(`Cap update changed protected vault state (byte offsets ${Array.from(expected).flatMap((byte, offset) => byte !== after!.data[offset] ? [offset] : []).join(",")}; lamports ${before!.lamports}->${after!.lamports})`);
}

function verifyProgram(accounts: readonly (AccountSnapshot | null)[]) {
  const program = accounts[2]; const data = accounts[3];
  if (!program || !data || program.address !== PROGRAM || program.owner !== LOADER || !program.executable ||
      program.data.length !== 36 || Buffer.from(program.data).readUInt32LE(0) !== 2 ||
      getAddressDecoder().decode(program.data.subarray(4)) !== PROGRAM_DATA || data.address !== PROGRAM_DATA ||
      data.owner !== LOADER || data.executable || data.data.length <= 45 || Buffer.from(data.data).readUInt32LE(0) !== 3 ||
      Buffer.from(data.data).readBigUInt64LE(4) !== 445223838n ||
      sha(data.data.subarray(45)) !== "bf1c1831b3d6350f4340badb942bd2e7bfaca4aa89276cb65e8480aa30d44c56") throw new Error("Pinned Voltr deployment mismatch");
}

export async function updatePilotCap(execute = false, journalPath: string | null = null) {
  if (execute && (!journalPath || !isAbsolute(journalPath))) throw new Error("Execution requires an absolute --journal path for durable attempt recovery");
  if (execute && process.env.CONFIRM_MAINNET !== "1") throw new Error("Pilot cap execution requires CONFIRM_MAINNET=1");
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
  const connection = new Connection(rpcUrl, "finalized");
  if (await connection.getGenesisHash() !== "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d") throw new Error("Mainnet genesis mismatch");
  if (execute && existsSync(journalPath!)) {
    const pending = JSON.parse(readFileSync(journalPath!, "utf8"));
    if (pending.schema !== "pilot-cap-attempt/1" || pending.vault !== VAULT || pending.capRaw !== PILOT_CAP_RAW.toString() ||
        typeof pending.expectedSignature !== "string" || !/^[1-9A-HJ-NP-Za-km-z]{64,88}$/.test(pending.expectedSignature)) throw new Error("Invalid pilot cap recovery journal");
    const verified = verifyPendingSignedWire({ schema: pending.schema, verdict: "SIGNED_SIMULATION_PASS_PENDING_SEND", broadcast: true,
      phase: "cap", transaction: { expectedSignature: pending.expectedSignature, wireSha256: pending.wireSha256 }, signedWireBase64: pending.wireBase64 }, "pilot-cap-attempt/1", ["cap"]);
    const retained = VersionedTransaction.deserialize(verified.wire);
    const expectedMessage = new TransactionMessage({ payerKey: new PublicKey(ADMIN), recentBlockhash: retained.message.recentBlockhash,
      instructions: [toWeb3Instruction(await pilotCapInstruction(createNoopSigner(ADMIN)))] }).compileToV0Message();
    if (!Buffer.from(retained.message.serialize()).equals(Buffer.from(expectedMessage.serialize()))) throw new Error("Pending transaction is not the exact pilot cap operation");
    try {
    const status = (await connection.getSignatureStatuses([pending.expectedSignature], { searchTransactionHistory: true })).value[0];
    if (!status || status.confirmationStatus !== "finalized") return { verdict: "PILOT_CAP_PENDING_RECONCILIATION", broadcast: "unknown", expectedSignature: pending.expectedSignature };
    if (status.err !== null) return { verdict: "PILOT_CAP_ATTEMPT_FAILED", broadcast: true, expectedSignature: pending.expectedSignature };
    const transaction = await connection.getTransaction(pending.expectedSignature, { commitment: "finalized", maxSupportedTransactionVersion: 0 });
    if (!transaction || transaction.meta?.err !== null ||
        !Buffer.from(transaction.transaction.message.serialize()).equals(Buffer.from(retained.message.serialize()))) throw new Error("Finalized transaction differs from retained cap attempt");
    const recovered = await finalizedSnapshots(rpcUrl, [VAULT, ADMIN, PROGRAM, PROGRAM_DATA], status.slot);
    verifyProgram(recovered.accounts);
    const original = { ...pending.beforeVault, data: Buffer.from(pending.beforeVault.dataBase64, "base64") } as AccountSnapshot;
    verifyPilotCapChange(original, recovered.accounts[0] ?? null);
    return { verdict: "PILOT_CAP_FINALIZED_NOT_DEPOSIT_READINESS", broadcast: true, expectedSignature: pending.expectedSignature, slot: recovered.contextSlot };
    } catch {
      return { verdict: "PILOT_CAP_PENDING_RECONCILIATION", broadcast: "unknown", expectedSignature: pending.expectedSignature };
    }
  }
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  const instruction = await pilotCapInstruction(admin.signer);
  const addresses = [VAULT, ADMIN, PROGRAM, PROGRAM_DATA];
  const before = await confirmedSnapshots(rpcUrl, addresses);
  verifyProgram(before.accounts);
  const oldVault = decodeVault(before.accounts[0] ?? null);
  if (oldVault.asset.totalValue > PILOT_CAP_RAW) throw new Error("Vault value already exceeds pilot cap");
  if (oldVault.vaultConfiguration.maxCap === PILOT_CAP_RAW) return { verdict: "PILOT_CAP_ALREADY_INSTALLED_NOT_DEPOSIT_READINESS", broadcast: false, slot: before.contextSlot };
  const prepared = await prepareSignedV0Transaction({ rpcUrl, feePayer: admin, instructions: [instruction],
    prestateAddresses: addresses, inspectedAddresses: [VAULT], minimumContextSlot: before.contextSlot, commitment: "confirmed" });
  if (prepared.simulation.err !== null || prepared.feeLamports > MAX_FEE) throw new Error("Pilot cap simulation or fee gate failed");
  verifyPilotCapChange(before.accounts[0] ?? null, prepared.simulation.postAccounts[0] ?? null);
  const refreshed = await confirmedSnapshots(rpcUrl, addresses, prepared.simulationSlot);
  verifyProgram(refreshed.accounts);
  for (let i = 0; i < addresses.length; i++) {
    const a = before.accounts[i]; const b = refreshed.accounts[i];
    if (!a || !b || a.owner !== b.owner || a.executable !== b.executable || a.lamports !== b.lamports || sha(a.data) !== sha(b.data)) throw new Error("Protected state changed after cap simulation; prepare again");
  }
  if (!execute) return { verdict: "PILOT_CAP_SIMULATION_PASS_NOT_DEPOSIT_READINESS", broadcast: false, slot: prepared.simulationSlot, capRaw: PILOT_CAP_RAW.toString(), feeLamports: prepared.feeLamports };
  // Exclusive creation prevents a second process from sending a new attempt.
  // This file is retained even on success; reruns reconcile this exact signature.
  writeFileSync(journalPath!, JSON.stringify({ schema: "pilot-cap-attempt/1", vault: VAULT,
    capRaw: PILOT_CAP_RAW.toString(), expectedSignature: prepared.expectedSignature,
    wireSha256: sha(prepared.serializedTransaction), wireBase64: Buffer.from(prepared.serializedTransaction).toString("base64"),
    lastValidBlockHeight: prepared.latestBlockhash.lastValidBlockHeight,
    beforeVault: { ...before.accounts[0], data: undefined, dataBase64: Buffer.from(before.accounts[0]!.data).toString("base64") },
  }), { flag: "wx", mode: 0o600, flush: true });
  try {
    const confirmed = await sendPreparedConfirmedOnce(rpcUrl, prepared, refreshed.contextSlot);
    if (confirmed.err !== null) return { verdict: "PILOT_CAP_ATTEMPT_FAILED", broadcast: true, expectedSignature: prepared.expectedSignature };
    const readback = await confirmedSnapshots(rpcUrl, addresses, confirmed.confirmedSlot);
    verifyProgram(readback.accounts);
    verifyPilotCapChange(before.accounts[0] ?? null, readback.accounts[0] ?? null);
    return { verdict: "PILOT_CAP_CONFIRMED_NOT_DEPOSIT_READINESS", broadcast: true, expectedSignature: prepared.expectedSignature, confirmed, slot: readback.contextSlot, capRaw: PILOT_CAP_RAW.toString() };
  } catch {
    return { verdict: "PILOT_CAP_PENDING_RECONCILIATION", broadcast: "unknown", expectedSignature: prepared.expectedSignature };
  }
}

/** Public-RPC simulation: no private key is loaded and no send API is called. */
export async function simulatePilotCap(rpcUrl = process.env.SOLANA_RPC_URL ?? "https://api.mainnet-beta.solana.com") {
  const connection = new Connection(rpcUrl, "confirmed");
  if (await connection.getGenesisHash() !== "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d") throw new Error("Mainnet genesis mismatch");
  const before = await confirmedSnapshots(rpcUrl, [VAULT, ADMIN, PROGRAM, PROGRAM_DATA]);
  verifyProgram(before.accounts);
  decodeVault(before.accounts[0] ?? null);
  const instruction = await pilotCapInstruction(createNoopSigner(ADMIN));
  const lifetime = await connection.getLatestBlockhash({ commitment: "confirmed", minContextSlot: before.contextSlot });
  const message = new TransactionMessage({ payerKey: new PublicKey(ADMIN), recentBlockhash: lifetime.blockhash,
    instructions: [toWeb3Instruction(instruction)] }).compileToV0Message();
  const simulation = await connection.simulateTransaction(new VersionedTransaction(message), {
    commitment: "confirmed", minContextSlot: before.contextSlot, sigVerify: false,
    accounts: { encoding: "base64", addresses: [VAULT] },
  });
  if (simulation.value.err !== null) return { verdict: "PILOT_CAP_UNSIGNED_SIMULATION_FAIL", broadcast: false, slot: simulation.context.slot, error: simulation.value.err };
  const post = simulation.value.accounts?.[0];
  if (!post || typeof post.data[0] !== "string" || post.data[1] !== "base64") throw new Error("Cap simulation omitted vault readback");
  verifyPilotCapChange(before.accounts[0] ?? null, { address: VAULT, owner: post.owner, executable: post.executable,
    lamports: post.lamports, data: Buffer.from(post.data[0], "base64") });
  const fee = await connection.getFeeForMessage(message, "confirmed");
  if (fee.value === null || fee.value > MAX_FEE) throw new Error("Pilot cap simulation fee gate failed");
  return { verdict: "PILOT_CAP_UNSIGNED_SIMULATION_PASS_NOT_DEPOSIT_READINESS", broadcast: false,
    slot: simulation.context.slot, capRaw: PILOT_CAP_RAW.toString(), feeLamports: fee.value,
    messageSha256: sha(message.serialize()), unitsConsumed: simulation.value.unitsConsumed };
}
