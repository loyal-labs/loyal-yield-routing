import { pathToFileURL } from "node:url";

import {
  findAssociatedTokenPda,
  getCreateAssociatedTokenIdempotentInstructionAsync,
  getTokenDecoder,
} from "@solana-program/token";
import {
  address,
  createNoopSigner,
  getU16Encoder,
  type Address,
  type Instruction,
  type TransactionSigner,
} from "@solana/kit";
import {
  Connection,
  PublicKey,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import {
  findVaultLpMintPda,
  getUpdateVaultConfigInstructionAsync,
  getVaultDecoder,
  VaultConfigField,
} from "@voltr/vault-sdk";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  prepareSignedV0Transaction,
  sendPreparedConfirmedOnce,
  toWeb3Instruction,
  type AccountSnapshot,
} from "../integrations/solana-compat.js";

// Sets the admin performance fee on one of Loyal's Voltr vaults. Fee LP is
// minted to the admin's LP ATA on harvest; every other fee field, the admin,
// the manager and the vault configuration must stay exactly as they are.
//
// Rehearse the Backyard cutover with the worker's fee collection ready:
//   bun src/activation/voltr-admin-performance-fee.ts --vault looping --bps 0
// Earn Max uses the separate "rwa" vault and keeps its 1500 bps rate.

const route = RWA_MULTIPLY_ROUTE;
const ADMIN = route.setupAdmin;
const MAX_TOTAL_LAMPORTS = 5_000_000;

const VAULTS = {
  rwa: route.vault.address,
  looping: address("GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1"),
} as const satisfies Record<string, Address>;
type VaultName = keyof typeof VAULTS;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function stable(value: unknown): string {
  return JSON.stringify(value, (_key, entry) => {
    if (typeof entry === "bigint") return entry.toString();
    if (entry instanceof Uint8Array) return Buffer.from(entry).toString("hex");
    if (entry?.type === "Buffer" && Array.isArray(entry.data)) return Buffer.from(entry.data).toString("hex");
    return entry;
  });
}

function argValue(argv: readonly string[], flag: string): string | undefined {
  const index = argv.indexOf(flag);
  return index >= 0 ? argv[index + 1] : undefined;
}

function parseArgs(argv: readonly string[]) {
  const vault = argValue(argv, "--vault");
  invariant(vault === "rwa" || vault === "looping", "--vault rwa|looping is required");
  const raw = argValue(argv, "--bps");
  invariant(raw !== undefined && /^\d+$/.test(raw), "--bps <integer> is required");
  const bps = Number(raw);
  invariant(bps <= 5_000, "--bps must be at most 5000");
  return { vault: vault as VaultName, bps, send: argv.includes("--send") };
}

type VaultState = ReturnType<typeof vaultState>;

function vaultState(snapshot: Pick<AccountSnapshot, "owner" | "data"> | null) {
  invariant(snapshot && snapshot.owner === route.programs.voltr, "vault is absent or not Voltr-owned");
  const vault = getVaultDecoder().decode(snapshot.data);
  const { reserved: _reserved, ...fees } = vault.feeConfiguration;
  return {
    admin: vault.admin,
    manager: vault.manager,
    fees,
    vaultConfiguration: stable(vault.vaultConfiguration),
    highWaterMarkBits: vault.highWaterMark.highestAssetPerLpDecimalBits,
    accruedAdminLp: vault.feeState.accumulatedLpAdminFees,
  };
}

function othersUnchanged(before: VaultState, after: VaultState): boolean {
  const { adminPerformanceFee: _a, ...restBefore } = before.fees;
  const { adminPerformanceFee: _b, ...restAfter } = after.fees;
  return stable(restBefore) === stable(restAfter) &&
    before.admin === after.admin && before.manager === after.manager &&
    before.vaultConfiguration === after.vaultConfiguration;
}

function checkResult(before: VaultState, after: VaultState, ata: AccountSnapshot | null, lpMint: Address, bps: number): void {
  invariant(after.fees.adminPerformanceFee === bps, `simulation resulted in ${after.fees.adminPerformanceFee} bps, not ${bps}`);
  invariant(othersUnchanged(before, after), "simulation changed another fee, the admin, the manager or the vault configuration");
  invariant(after.highWaterMarkBits >= before.highWaterMarkBits, "simulation lowered the high-water mark");
  invariant(after.accruedAdminLp >= before.accruedAdminLp, "simulation reduced previously earned admin LP");
  invariant(ata && ata.owner === route.assets.tokenProgram, "admin LP ATA is absent after simulation");
  const token = getTokenDecoder().decode(ata.data);
  invariant(token.mint === lpMint && token.owner === ADMIN, "admin LP ATA has the wrong mint or owner");
}

export async function buildInstructions(admin: TransactionSigner, vault: Address, lpMint: Address, adminLpAta: Address, bps: number): Promise<Instruction[]> {
  return [
    await getCreateAssociatedTokenIdempotentInstructionAsync({
      payer: admin,
      ata: adminLpAta,
      owner: admin.address,
      mint: lpMint,
      systemProgram: route.programs.system,
      tokenProgram: route.assets.tokenProgram,
    }, { programAddress: route.assets.associatedTokenProgram }),
    await getUpdateVaultConfigInstructionAsync({
      admin,
      vault,
      field: VaultConfigField.AdminPerformanceFee,
      data: getU16Encoder().encode(bps),
    }, { programAddress: route.programs.voltr }),
  ];
}

function postSnapshot(account: string, value: { owner: string; lamports: number; executable: boolean; data: [string, string] } | null | undefined): AccountSnapshot | null {
  if (!value) return null;
  return { address: account, owner: value.owner, lamports: value.lamports, executable: value.executable, data: Buffer.from(value.data[0], "base64") };
}

async function main() {
  const { vault: name, bps, send } = parseArgs(process.argv.slice(2));
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");
  const connection = new Connection(rpcUrl, "confirmed");
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not Solana mainnet-beta");

  const vault = VAULTS[name];
  const [lpMint] = await findVaultLpMintPda({ vault }, { programAddress: route.programs.voltr });
  const [adminLpAta] = await findAssociatedTokenPda(
    { owner: ADMIN, mint: lpMint, tokenProgram: route.assets.tokenProgram },
    { programAddress: route.assets.associatedTokenProgram },
  );
  const account = await connection.getAccountInfo(new PublicKey(vault), "confirmed");
  invariant(account, "vault is absent");
  const before = vaultState({ owner: account.owner.toBase58(), data: account.data });
  invariant(before.admin === ADMIN, `vault admin ${before.admin} is not ${ADMIN}`);
  const header = { vault: name, address: vault, lpMint, adminLpAta, before: before.fees, requestedAdminPerformanceFeeBps: bps, mode: send ? "send" : "simulate" };
  console.log(stable(header));
  if (before.fees.adminPerformanceFee === bps) {
    console.log(stable({ verdict: "ALREADY_SET", adminPerformanceFee: bps }));
    return;
  }

  if (!send) {
    const instructions = await buildInstructions(createNoopSigner(ADMIN), vault, lpMint, adminLpAta, bps);
    const message = new TransactionMessage({
      payerKey: new PublicKey(ADMIN),
      recentBlockhash: (await connection.getLatestBlockhash("confirmed")).blockhash,
      instructions: instructions.map(toWeb3Instruction),
    }).compileToV0Message();
    const transaction = new VersionedTransaction(message);
    const fee = await connection.getFeeForMessage(message, "confirmed");
    const simulation = await connection.simulateTransaction(transaction, {
      commitment: "confirmed",
      sigVerify: false,
      replaceRecentBlockhash: true,
      accounts: { encoding: "base64", addresses: [vault, adminLpAta] },
    });
    invariant(simulation.value.err === null, `simulation failed: ${stable(simulation.value.err)}\n${(simulation.value.logs ?? []).join("\n")}`);
    const post = simulation.value.accounts ?? [];
    const after = vaultState(postSnapshot(vault, post[0] as Parameters<typeof postSnapshot>[1]));
    checkResult(before, after, postSnapshot(adminLpAta, post[1] as Parameters<typeof postSnapshot>[1]), lpMint, bps);
    invariant(fee.value !== null && fee.value <= MAX_TOTAL_LAMPORTS, `quoted fee ${fee.value} exceeds ${MAX_TOTAL_LAMPORTS}`);
    console.log(stable({
      verdict: "SIMULATION_PASS",
      after: after.fees,
      accruedAdminLp: { before: before.accruedAdminLp, after: after.accruedAdminLp },
      highWaterMarkBits: { before: before.highWaterMarkBits, after: after.highWaterMarkBits },
      adminLpAtaExisted: (await connection.getAccountInfo(new PublicKey(adminLpAta), "confirmed")) !== null,
      packetBytes: transaction.serialize().length,
      feeLamports: fee.value,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
      sendWith: `CONFIRM_MAINNET=1 bun src/activation/voltr-admin-performance-fee.ts --vault ${name} --bps ${bps} --send`,
    }));
    return;
  }

  invariant(process.env.CONFIRM_MAINNET === "1", "--send requires CONFIRM_MAINNET=1");
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === ADMIN, "SOLANA_TESTING_PK is not the vault admin");
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    instructions: await buildInstructions(admin.signer, vault, lpMint, adminLpAta, bps),
    inspectedAddresses: [vault, adminLpAta],
    commitment: "confirmed",
  });
  invariant(prepared.simulation.err === null, `signed simulation failed: ${stable(prepared.simulation.err)}`);
  checkResult(before, vaultState(prepared.simulation.postAccounts[0] ?? null), prepared.simulation.postAccounts[1] ?? null, lpMint, bps);
  invariant(prepared.feeLamports <= MAX_TOTAL_LAMPORTS, `quoted fee ${prepared.feeLamports} exceeds ${MAX_TOTAL_LAMPORTS}`);

  // NAV reports keep rewriting the vault account, so recheck the fields this
  // transaction depends on rather than the raw account hash.
  const fresh = await connection.getAccountInfo(new PublicKey(vault), { commitment: "confirmed", minContextSlot: prepared.simulationSlot });
  invariant(fresh, "vault vanished before send");
  const current = vaultState({ owner: fresh.owner.toBase58(), data: fresh.data });
  invariant(stable(current.fees) === stable(before.fees) && othersUnchanged(before, current), "vault fees, admin, manager or configuration changed after simulation; refusing send");

  const settled = await sendPreparedConfirmedOnce(rpcUrl, prepared, prepared.simulationSlot);
  const readback = await connection.getAccountInfo(new PublicKey(vault), { commitment: "confirmed", minContextSlot: settled.confirmedSlot });
  const finalState = readback ? vaultState({ owner: readback.owner.toBase58(), data: readback.data }) : null;
  console.log(stable({
    verdict: settled.err === null && finalState?.fees.adminPerformanceFee === bps ? "CONFIRMED_AND_VERIFIED" : "CONFIRMED_READBACK_FAIL",
    signature: settled.signature,
    confirmedSlot: settled.confirmedSlot,
    err: settled.err,
    after: finalState?.fees ?? null,
  }));
  process.exitCode = settled.err === null && finalState?.fees.adminPerformanceFee === bps ? 0 : 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exit(1);
  });
}
