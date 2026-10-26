import {
  Connection,
  PublicKey,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import { createNoopSigner } from "@solana/kit";
import { getUpdateVaultConfigInstructionAsync, VaultConfigField } from "@voltr/vault-sdk";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import {
  prepareSignedV0Transaction,
  sendPreparedConfirmedOnce,
  toWeb3Instruction,
} from "../integrations/solana-compat.js";

const ADMIN_OFFSET = 400;
const REDEMPTION_FEE_OFFSET = 520;
const ISSUANCE_FEE_OFFSET = 522;
const VAULT_MIN_SIZE = 524;

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function feeTerms(data: Uint8Array) {
  invariant(data.length >= VAULT_MIN_SIZE, "vault account is shorter than the fee fields");
  const view = Buffer.from(data);
  return {
    admin: new PublicKey(view.subarray(ADMIN_OFFSET, ADMIN_OFFSET + 32)).toBase58(),
    redemptionFeeBps: view.readUInt16LE(REDEMPTION_FEE_OFFSET),
    issuanceFeeBps: view.readUInt16LE(ISSUANCE_FEE_OFFSET),
  };
}

function parseBps(argv: readonly string[]): number {
  const index = argv.indexOf("--bps");
  const raw = index >= 0 ? argv[index + 1] : undefined;
  invariant(raw !== undefined && /^\d+$/.test(raw), "--bps <integer> is required");
  const bps = Number(raw);
  invariant(bps <= 10_000, "--bps must be at most 10000");
  return bps;
}

async function main() {
  const argv = process.argv.slice(2);
  const bps = parseBps(argv);
  const send = argv.includes("--send");
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required");

  const route = RWA_MULTIPLY_ROUTE;
  const vault = route.vault.address;
  const connection = new Connection(rpcUrl, "confirmed");
  invariant(await connection.getGenesisHash() === route.genesisHash, "RPC is not Solana mainnet-beta");

  const before = await connection.getAccountInfo(new PublicKey(vault), "confirmed");
  invariant(before?.owner.toBase58() === route.programs.voltr, "vault is absent or not Voltr-owned");
  const current = feeTerms(before.data);
  invariant(current.admin === route.setupAdmin, `vault admin ${current.admin} is not the route setup admin`);
  console.log(JSON.stringify({ vault, admin: current.admin, current, requestedIssuanceFeeBps: bps, mode: send ? "send" : "simulate" }));

  const data = Buffer.alloc(2);
  data.writeUInt16LE(bps);
  const build = (admin: Parameters<typeof getUpdateVaultConfigInstructionAsync>[0]["admin"]) =>
    getUpdateVaultConfigInstructionAsync({
      admin,
      vault,
      field: VaultConfigField.IssuanceFee,
      data,
    }, { programAddress: route.programs.voltr });

  if (!send) {
    const adminKey = new PublicKey(route.setupAdmin);
    const instruction = toWeb3Instruction(await build(createNoopSigner(route.setupAdmin)));
    const message = new TransactionMessage({
      payerKey: adminKey,
      recentBlockhash: (await connection.getLatestBlockhash("confirmed")).blockhash,
      instructions: [instruction],
    }).compileToV0Message();
    const simulation = await connection.simulateTransaction(new VersionedTransaction(message), {
      commitment: "confirmed",
      sigVerify: false,
      replaceRecentBlockhash: true,
      accounts: { encoding: "base64", addresses: [vault] },
    });
    const image = simulation.value.accounts?.[0]?.data[0];
    console.log(JSON.stringify({
      err: simulation.value.err,
      unitsConsumed: simulation.value.unitsConsumed ?? null,
      logs: simulation.value.logs ?? [],
      resulting: image ? feeTerms(Buffer.from(image, "base64")) : null,
    }, null, 2));
    process.exitCode = simulation.value.err ? 1 : 0;
    return;
  }

  invariant(process.env.CONFIRM_MAINNET === "1", "--send requires CONFIRM_MAINNET=1");
  const admin = await signingMaterialFromEnvironment("SOLANA_TESTING_PK");
  invariant(admin.signer.address === route.setupAdmin, "SOLANA_TESTING_PK is not the vault admin");
  const prepared = await prepareSignedV0Transaction({
    rpcUrl,
    feePayer: admin,
    instructions: [await build(admin.signer)],
    inspectedAddresses: [vault],
    commitment: "confirmed",
  });
  invariant(prepared.simulation.err === null, `simulation failed: ${JSON.stringify(prepared.simulation.err)}`);
  const resulting = feeTerms(prepared.simulation.postAccounts[0]!.data);
  invariant(resulting.issuanceFeeBps === bps, `simulation resulted in ${resulting.issuanceFeeBps} bps, not ${bps}`);
  const settled = await sendPreparedConfirmedOnce(rpcUrl, prepared, prepared.prestateSlot);
  console.log(JSON.stringify({ signature: settled.signature, err: settled.err, logs: settled.logs }, null, 2));
  process.exitCode = settled.err ? 1 : 0;
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
