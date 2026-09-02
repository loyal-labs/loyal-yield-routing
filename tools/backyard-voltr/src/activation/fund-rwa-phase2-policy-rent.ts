import { Connection, Keypair, PublicKey, SystemProgram, TransactionMessage, VersionedTransaction } from "@solana/web3.js";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";

const LAMPORTS = 150_000_000;
async function main() {
  if (process.env.CONFIRM_MAINNET !== "1") throw new Error("CONFIRM_MAINNET=1 is required");
  const rpc = process.env.SOLANA_RPC_URL?.trim(); if (!rpc) throw new Error("SOLANA_RPC_URL is required");
  const connection = new Connection(rpc, "confirmed");
  if (await connection.getGenesisHash() !== RWA_MULTIPLY_ROUTE.genesisHash) throw new Error("RPC is not mainnet-beta");
  const material = await signingMaterialFromEnvironment("POLICY_KEYPAIR");
  if (material.signer.address !== RWA_MULTIPLY_ROUTE.squads.delegatedExecutor) throw new Error("funding signer drifted");
  const signer = Keypair.fromSecretKey(material.secretKey); const destination = new PublicKey(RWA_MULTIPLY_ROUTE.setupAdmin);
  const latest = await connection.getLatestBlockhashAndContext("confirmed");
  const transaction = new VersionedTransaction(new TransactionMessage({ payerKey: signer.publicKey, recentBlockhash: latest.value.blockhash,
    instructions: [SystemProgram.transfer({ fromPubkey: signer.publicKey, toPubkey: destination, lamports: LAMPORTS })] }).compileToV0Message());
  transaction.sign([signer]); const wire = transaction.serialize(); const expected = bs58.encode(transaction.signatures[0]!);
  const returned = await connection.sendRawTransaction(wire, { skipPreflight: false, preflightCommitment: "confirmed", maxRetries: 0, minContextSlot: latest.context.slot });
  if (returned !== expected) throw new Error("RPC signature mismatch");
  const confirmation = await connection.confirmTransaction({ signature: returned, blockhash: latest.value.blockhash,
    lastValidBlockHeight: latest.value.lastValidBlockHeight }, "confirmed");
  if (confirmation.value.err !== null) throw new Error(`funding transfer failed: ${JSON.stringify(confirmation.value.err)}`);
  console.log(JSON.stringify({ verdict: "CONFIRMED", signature: returned, lamports: LAMPORTS,
    from: signer.publicKey.toBase58(), to: destination.toBase58() }));
}
main().catch((error) => { console.error((error instanceof Error ? error.message : String(error)).replace(process.env.SOLANA_RPC_URL ?? "", "<rpc>")); process.exitCode = 1; });
