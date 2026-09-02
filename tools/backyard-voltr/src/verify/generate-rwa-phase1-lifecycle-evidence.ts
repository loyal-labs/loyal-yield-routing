import { createHash } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { neon } from "@neondatabase/serverless";
import { PublicKey, VersionedTransaction } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const OUTPUT = resolve(ROOT, "docs/evidence/backyard-rwa-go/lifecycle-v1.json");
const SIGNATURES = {
  deposit: ["NR3W8mez1poyVhaced1Tmge8Rju2rJ4XWR2rncqh7SYwLtmnUzN1J7wAjyAbC5psJ8tfR4qB3h3qa3TZ4ooBbi4"],
  allocate: ["5DMAhtCM8zLy25mwCd2TQEMm4ZZXu6Gnb8BTRC98NLxdVwGnxh8XUJkW2AXqViuBHcRQxrSF3N4YYZDWf3qkMKd4"],
  withdraw_request: ["yjT6ByNmGJvD4bfwsGtrTuL561d9C2ojh62TFtKKnoBnpxVuxAMi3r7VewpAWqvy41BnFHkJ7yFFvf21sZiFcke"],
  unwind: [
    "4UrZAcNTNDshnci8TgurfEWJM2KPth58BhDFQrhXMnYgWtiqJrwTfNrPypaAMVgUh2pvCX98pnfr4PYzea16E3Me",
    "4ZxkxywBccAVBzwPJ7p6TjqoVTj14mQxt8GeNDeMnmSRMcpBUVPA83DVF6YnCwCTXnhaYdjPf6vq69eLC57DHwJS",
    "2vcMVhDkHsvjyW3AnxeHLG8DcSi7NVFb3k2VEt4aAY71zPdV8SyaFYEQHoewnF1D9tmws2V9x4T94xXHrWy5tf8d",
    "516ezXWKmR1crrBkRURh8aMdbTkbrf6YRgbA1F4dKicXFgPmpxYTSzEjKdk1m5vywRW27WXRqPGMRk6nhXyEVBT8",
    "5W6f4rRUfPxUSAKU8aTrMrPYYrBaX4bu4B2UxsTG6qRrBkkSZaQT1PnRAmGwfTPPaUpA4d1TsUridnQgwYD4gPZa",
  ],
  restore: ["hnQZ9v9iZS5pxhpypciwLv7TSMpw8QVkvJQC5KSKAREmmtH7rwQvJyAFDGaDSfPymAq1JfC6bKUG4K9eubh98j6"],
  claim: ["KGMkirjzZQFaxpis4LH4yEK8b5yv6KVj1qsZAH9vYfCMfaRJMhwRWHF3qSwBRbM44hXS5tvWzFV3vMr8wAHxkWw"],
} as const;

type Json = Record<string, any>;
const hash = (bytes: Uint8Array) => createHash("sha256").update(bytes).digest("hex");
const pda = (program: PublicKey, seeds: Uint8Array[]) => PublicKey.findProgramAddressSync(seeds, program)[0];

async function main() {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  const databaseUrl = process.env.NEON_DATABASE_URL?.trim();
  if (!rpcUrl || !databaseUrl) throw new Error("SOLANA_RPC_URL and NEON_DATABASE_URL are required");
  let id = 0;
  const rpc = async (method: string, params: unknown[]) => {
    const response = await fetch(rpcUrl, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ jsonrpc: "2.0", id: ++id, method, params }) });
    const body = await response.json() as Json;
    if (!response.ok || body.error) throw new Error(`RPC ${method} failed`);
    return body.result;
  };
  if (await rpc("getGenesisHash", []) !== RWA_MULTIPLY_ROUTE.genesisHash) throw new Error("RPC is not mainnet-beta");

  const all = Object.values(SIGNATURES).flat();
  const transactions = new Map<string, Json>();
  for (const signature of all) {
    const value = await rpc("getTransaction", [signature, { commitment: "confirmed", encoding: "json", maxSupportedTransactionVersion: 0 }]);
    if (!value?.meta || value.meta.err !== null) throw new Error(`missing or failed transaction ${signature}`);
    const keys = [...value.transaction.message.accountKeys.map((entry: any) => typeof entry === "string" ? entry : entry.pubkey), ...(value.meta.loadedAddresses?.writable ?? []), ...(value.meta.loadedAddresses?.readonly ?? [])];
    const balances = new Map<string, Json>();
    for (const [side, rows] of [["beforeRaw", value.meta.preTokenBalances], ["afterRaw", value.meta.postTokenBalances]] as const) {
      for (const row of rows ?? []) {
        const address = keys[row.accountIndex];
        const prior = balances.get(address) ?? { address, mint: row.mint, owner: row.owner, beforeRaw: "0", afterRaw: "0" };
        if (prior.mint !== row.mint || prior.owner !== row.owner) throw new Error(`token identity changed in ${signature}`);
        prior[side] = row.uiTokenAmount.amount;
        balances.set(address, prior);
      }
    }
    transactions.set(signature, { ...value, keys, tokenBalances: [...balances.values()].sort((a, b) => a.address.localeCompare(b.address)) });
  }

  const deposit = transactions.get(SIGNATURES.deposit[0])!;
  const usdc = RWA_MULTIPLY_ROUTE.assets.assetMint.toString();
  const depositRow = deposit.tokenBalances.find((row: Json) => row.mint === usdc && BigInt(row.afterRaw) < BigInt(row.beforeRaw));
  if (!depositRow) throw new Error("cannot derive depositing user USDC account");
  const userUsdcAta = depositRow.address;
  const userTransferAuthority = depositRow.owner;
  const vault = new PublicKey(String(RWA_MULTIPLY_ROUTE.vault.address));
  const voltrProgram = new PublicKey(String(RWA_MULTIPLY_ROUTE.programs.voltr));
  const withdrawalReceipt = pda(voltrProgram, [Buffer.from("request_withdraw_vault_receipt"), vault.toBuffer(), new PublicKey(userTransferAuthority).toBuffer()]).toString();

  const strategy = new PublicKey(String(RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig));
  const strategyReceipt = pda(voltrProgram, [Buffer.from("strategy_init_receipt"), vault.toBuffer(), strategy.toBuffer()]);
  const idleAuthority = pda(voltrProgram, [Buffer.from("vault_asset_idle_auth"), vault.toBuffer()]);
  const strategyAuthority = pda(voltrProgram, [Buffer.from("vault_strategy_auth"), vault.toBuffer(), strategy.toBuffer()]);
  const tokenProgram = new PublicKey(String(RWA_MULTIPLY_ROUTE.assets.tokenProgram));
  const assetMint = new PublicKey(String(RWA_MULTIPLY_ROUTE.assets.assetMint));
  const associatedTokenProgram = new PublicKey(String(RWA_MULTIPLY_ROUTE.assets.associatedTokenProgram));
  const ata = (owner: PublicKey) => pda(associatedTokenProgram, [owner.toBuffer(), tokenProgram.toBuffer(), assetMint.toBuffer()]);
  const reportTicket = pda(new PublicKey(String(RWA_MULTIPLY_ROUTE.customAdaptor.program)), [Buffer.from("report_ticket"), strategy.toBuffer()]);
  const finalAddresses = [vault, strategy, strategyReceipt, ata(idleAuthority), ata(strategyAuthority), RWA_MULTIPLY_ROUTE.squads.assetAta, RWA_MULTIPLY_ROUTE.squads.collateralAta, RWA_MULTIPLY_ROUTE.kamino.obligation, RWA_MULTIPLY_ROUTE.kamino.collateralReserve, RWA_MULTIPLY_ROUTE.kamino.debtReserve, new PublicKey(withdrawalReceipt), reportTicket].map((address) => new PublicKey(String(address)));
  const finalRead = await rpc("getMultipleAccounts", [finalAddresses.map(String), { commitment: "confirmed", encoding: "base64" }]);
  const mutableCurrent = new Set([vault.toString(), strategyReceipt.toString(), reportTicket.toString(),
    RWA_MULTIPLY_ROUTE.kamino.collateralReserve.toString(), RWA_MULTIPLY_ROUTE.kamino.debtReserve.toString()]);
  const finalAccounts = finalRead.value.map((account: Json | null, index: number) => account === null
    ? { address: finalAddresses[index]!.toString(), owner: null, dataSha256: null }
    : { address: finalAddresses[index]!.toString(), owner: account.owner,
      dataSha256: mutableCurrent.has(finalAddresses[index]!.toString()) ? null : hash(Buffer.from(account.data[0], "base64")) });

  const workerSignatures = [SIGNATURES.allocate[0], ...SIGNATURES.unwind, SIGNATURES.restore[0]];
  const sql = neon(databaseUrl);
  const rows = await sql`SELECT action, transaction_signature, expected_effects, encode(signed_wire, 'base64') AS signed_wire_base64
    FROM loyal_yield.multiply_operations
    WHERE route_key = ${RWA_MULTIPLY_ROUTE.id} AND transaction_signature = ANY(${workerSignatures})
    ORDER BY confirmed_slot` as Json[];
  const reports = rows.filter((row) => ["VOLTR_ALLOCATE_TO_SQUADS", "REPORT_NAV", "VOLTR_RESTORE_IDLE"].includes(row.action)).map((row) => {
    const observationSlot = BigInt(row.expected_effects.decision.observationSlot);
    const prefix = Buffer.alloc(17);
    prefix[0] = 1;
    prefix.writeBigUInt64LE(observationSlot, 1);
    prefix.writeBigUInt64LE(observationSlot, 9);
    const wire = Buffer.from(row.signed_wire_base64, "base64");
    const offset = wire.indexOf(prefix);
    const duplicateOffset = offset < 0 ? -1 : wire.indexOf(prefix, offset + 1);
    if (offset < 0 || duplicateOffset < 0 || offset + 57 > wire.length || duplicateOffset + 57 > wire.length
      || !wire.subarray(offset, offset + 57).equals(wire.subarray(duplicateOffset, duplicateOffset + 57))) {
      const slotBytes = Buffer.alloc(8); slotBytes.writeBigUInt64LE(observationSlot);
      const slotOffset = wire.indexOf(slotBytes);
      const context = slotOffset < 0 ? "absent" : wire.subarray(Math.max(0, slotOffset - 12), Math.min(wire.length, slotOffset + 36)).toString("hex");
      throw new Error(`two identical NAV report envelopes absent from signed wire ${row.transaction_signature}; observationSlot=${observationSlot}; offset=${offset}; duplicateOffset=${duplicateOffset}; slotOffset=${slotOffset}; context=${context}`);
    }
    const nav = wire.readBigUInt64LE(offset + 17);
    return { signature: row.transaction_signature, sequence: String(observationSlot), observedSlot: String(observationSlot), navAfterRaw: String(nav), snapshotDigest: wire.subarray(offset + 25, offset + 57).toString("hex") };
  });

  const step = (name: keyof typeof SIGNATURES) => ({ name, transactions: SIGNATURES[name].map((signature) => ({ signature, tokenBalances: transactions.get(signature)!.tokenBalances })) });
  const restoredRaw = BigInt(transactions.get(SIGNATURES.restore[0])!.tokenBalances.find((row: Json) => row.address === ata(idleAuthority).toString()).afterRaw) - BigInt(transactions.get(SIGNATURES.restore[0])!.tokenBalances.find((row: Json) => row.address === ata(idleAuthority).toString()).beforeRaw);
  const claimedRaw = BigInt(transactions.get(SIGNATURES.claim[0])!.tokenBalances.find((row: Json) => row.address === userUsdcAta).afterRaw) - BigInt(transactions.get(SIGNATURES.claim[0])!.tokenBalances.find((row: Json) => row.address === userUsdcAta).beforeRaw);
  const depositedRaw = BigInt(depositRow.beforeRaw) - BigInt(depositRow.afterRaw);
  const evidence = {
    schema: "loyal-backyard-rwa-live-lifecycle/v3", routeKey: RWA_MULTIPLY_ROUTE.id, genesisHash: RWA_MULTIPLY_ROUTE.genesisHash,
    commitment: "confirmed", broadcast: true, withdrawalWaitSeconds: Number(RWA_MULTIPLY_ROUTE.vault.withdrawalWaitingPeriodSeconds),
    userTransferAuthority, userUsdcAta, withdrawalReceipt, operationalAmountRaw: String(depositedRaw), requestedWithdrawalRaw: String(claimedRaw),
    realizedYieldRaw: "0", explicitProtocolFeesRaw: "0", retainedRaw: String(depositedRaw - claimedRaw), depositedRaw: String(depositedRaw), restoredRaw: String(restoredRaw), claimedRaw: String(claimedRaw),
    steps: [step("deposit"), step("allocate"), step("withdraw_request"), step("unwind"), step("restore"), step("claim")], finalAccounts, navReports: reports,
  };
  mkdirSync(resolve(OUTPUT, ".."), { recursive: true });
  writeFileSync(OUTPUT, `${JSON.stringify(evidence, null, 2)}\n`, { flag: "w" });
  console.log(JSON.stringify({ output: OUTPUT, signatures: all.length, navReports: reports.length, depositedRaw: String(depositedRaw), restoredRaw: String(restoredRaw), claimedRaw: String(claimedRaw) }));
}

await main();
