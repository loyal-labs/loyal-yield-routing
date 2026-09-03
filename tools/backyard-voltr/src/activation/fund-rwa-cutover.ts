import { createHash } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { findAssociatedTokenPda, getTokenDecoder } from "@solana-program/token";
import { getDepositVaultInstructionAsync } from "@voltr/vault-sdk";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  confirmedSnapshots,
  prepareSignedV0Transaction,
  sendPreparedConfirmedOnce,
  type AccountSnapshot,
} from "../integrations/solana-compat.js";
import { deriveRwaMultiplyVoltrAccounts } from "../integrations/rwa-multiply-voltr.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";

const MAX_AMOUNT_RAW = 1_000_000n;
const MAX_FEE_LAMPORTS = 100_000;
const REPOSITORY_ROOT = resolve(import.meta.dirname, "../../../..");
const INTENT_ROOT = resolve(REPOSITORY_ROOT, "docs/evidence/backyard-rwa-go/phase2-runtime/intents");

function rpcUrl(): string {
  const value = process.env.SOLANA_RPC_URL;
  if (!value) throw new Error("SOLANA_RPC_URL is required");
  return value;
}

function sha256(value: Uint8Array | string): string {
  return createHash("sha256").update(value).digest("hex");
}

function arg(name: string): string | null {
  const index = process.argv.indexOf(name);
  return index < 0 ? null : process.argv[index + 1] ?? null;
}

function tokenAmount(snapshot: AccountSnapshot | null, mint: string, owner: string): bigint {
  if (!snapshot || snapshot.owner !== RWA_MULTIPLY_ROUTE.assets.tokenProgram) throw new Error(`missing token account for ${owner}`);
  const decoded = getTokenDecoder().decode(snapshot.data);
  if (decoded.mint !== mint || decoded.owner !== owner) throw new Error(`token account binding drifted for ${snapshot.address}`);
  return decoded.amount;
}

function stateHash(accounts: readonly (AccountSnapshot | null)[]): string {
  const hash = createHash("sha256");
  for (const account of accounts) {
    hash.update(account?.address ?? "absent");
    hash.update(account?.owner ?? "");
    hash.update(account?.data ?? new Uint8Array());
  }
  return hash.digest("hex");
}

async function main(): Promise<void> {
  const amountRaw = BigInt(arg("--amount-raw") ?? "0");
  if (amountRaw <= 0n || amountRaw > MAX_AMOUNT_RAW) throw new Error(`amount must be in 1..${MAX_AMOUNT_RAW}`);
  const user = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  if (user.signer.address !== RWA_MULTIPLY_ROUTE.setupAdmin) throw new Error("SOLANA_TESTING_PK is not the frozen RWA user");
  const accounts = await deriveRwaMultiplyVoltrAccounts();
  const [userAssetAta] = await findAssociatedTokenPda({
    owner: user.signer.address,
    mint: RWA_MULTIPLY_ROUTE.assets.assetMint,
    tokenProgram: RWA_MULTIPLY_ROUTE.assets.tokenProgram,
  }, { programAddress: RWA_MULTIPLY_ROUTE.assets.associatedTokenProgram });
  const [userLpAta] = await findAssociatedTokenPda({
    owner: user.signer.address,
    mint: accounts.lpMint,
    tokenProgram: RWA_MULTIPLY_ROUTE.assets.tokenProgram,
  }, { programAddress: RWA_MULTIPLY_ROUTE.assets.associatedTokenProgram });
  const instruction = await getDepositVaultInstructionAsync({
    userTransferAuthority: user.signer,
    vault: RWA_MULTIPLY_ROUTE.vault.address,
    vaultAssetMint: RWA_MULTIPLY_ROUTE.assets.assetMint,
    vaultLpMint: accounts.lpMint,
    userAssetAta,
    vaultAssetIdleAta: accounts.idleAta,
    vaultAssetIdleAuth: accounts.idleAuth,
    userLpAta,
    vaultLpMintAuth: accounts.lpMintAuth,
    assetTokenProgram: RWA_MULTIPLY_ROUTE.assets.tokenProgram,
    amount: amountRaw,
  }, { programAddress: RWA_MULTIPLY_ROUTE.programs.voltr });
  const inspected = [userAssetAta, accounts.idleAta, userLpAta, accounts.lpMint].map(String);
  const before = await confirmedSnapshots(rpcUrl(), inspected);
  const sourceBefore = tokenAmount(before.accounts[0] ?? null, RWA_MULTIPLY_ROUTE.assets.assetMint, user.signer.address);
  const idleBefore = tokenAmount(before.accounts[1] ?? null, RWA_MULTIPLY_ROUTE.assets.assetMint, accounts.idleAuth);
  if (sourceBefore < amountRaw) throw new Error("frozen RWA user has insufficient confirmed USDC");
  const prepared = await prepareSignedV0Transaction({
    rpcUrl: rpcUrl(),
    feePayer: user,
    instructions: [instruction],
    inspectedAddresses: inspected,
    minimumContextSlot: before.contextSlot,
    commitment: "confirmed",
  });
  const simulatedSource = tokenAmount(prepared.simulation.postAccounts[0] ?? null, RWA_MULTIPLY_ROUTE.assets.assetMint, user.signer.address);
  const simulatedIdle = tokenAmount(prepared.simulation.postAccounts[1] ?? null, RWA_MULTIPLY_ROUTE.assets.assetMint, accounts.idleAuth);
  const ready = prepared.simulation.err === null && sourceBefore-simulatedSource === amountRaw && simulatedIdle-idleBefore === amountRaw && prepared.feeLamports <= MAX_FEE_LAMPORTS;
  const envelope = {
    schema: "loyal-backyard-rwa-cutover-funding/v1",
    broadcast: false,
    readyForBroadcast: ready,
    routeKey: RWA_MULTIPLY_ROUTE.id,
    vault: RWA_MULTIPLY_ROUTE.vault.address,
    user: user.signer.address,
    amountRaw: amountRaw.toString(),
    confirmedPrestateSlot: before.contextSlot,
    confirmedPrestateSha256: stateHash(before.accounts),
    simulationSlot: prepared.simulationSlot,
    simulationError: prepared.simulation.err,
    sourceBeforeRaw: sourceBefore.toString(),
    sourceAfterRaw: simulatedSource.toString(),
    idleBeforeRaw: idleBefore.toString(),
    idleAfterRaw: simulatedIdle.toString(),
    packetBytes: prepared.packetBytes,
    feeLamports: prepared.feeLamports,
    expectedSignature: prepared.expectedSignature,
    signedWireSha256: sha256(prepared.serializedTransaction),
  } as const;
  if (!process.argv.includes("--execute")) {
    console.log(JSON.stringify(envelope, null, 2));
    return;
  }
  if (process.env.CONFIRM_MAINNET !== "1" || !ready) throw new Error("execution requires CONFIRM_MAINNET=1 and a passing simulation");
  if (arg("--confirm-vault") !== RWA_MULTIPLY_ROUTE.vault.address || arg("--confirm-user") !== user.signer.address || arg("--confirm-amount-raw") !== amountRaw.toString()) {
    throw new Error("execution confirmations do not match the frozen RWA deposit");
  }
  const intentInput = arg("--intent-path");
  if (!intentInput) throw new Error("--intent-path is required");
  const intentPath = resolve(intentInput);
  if (!intentPath.startsWith(`${INTENT_ROOT}/`)) throw new Error(`intent must be inside ${INTENT_ROOT}`);
  mkdirSync(dirname(intentPath), { recursive: true });
  writeFileSync(intentPath, `${JSON.stringify(envelope, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  const refreshed = await confirmedSnapshots(rpcUrl(), inspected, prepared.simulationSlot);
  if (stateHash(refreshed.accounts) !== envelope.confirmedPrestateSha256) throw new Error("RWA deposit prestate changed after simulation");
  const settled = await sendPreparedConfirmedOnce(rpcUrl(), prepared, refreshed.contextSlot);
  const sourceDelta = settled.tokenDeltas.find(({ address }) => address === String(userAssetAta));
  const idleDelta = settled.tokenDeltas.find(({ address }) => address === String(accounts.idleAta));
  if (settled.err !== null || settled.signature !== prepared.expectedSignature || sourceDelta?.deltaRaw !== `-${amountRaw}` || idleDelta?.deltaRaw !== amountRaw.toString()) {
    throw new Error("confirmed RWA cutover funding did not reconcile exact source and idle deltas");
  }
  console.log(JSON.stringify({ ...envelope, broadcast: true, signature: settled.signature, confirmedSlot: settled.confirmedSlot, settlementError: settled.err }, null, 2));
}

await main();
