// Prints the base64 v0 message the TS builder makes for one Earn input, so a Go builder can be
// compared byte for byte (the blockhash is pinned by the input, so nothing else varies).
//
//   bun scripts/web/earn-tx-parity.ts --route <name> --input <file.json>
//   op run --env-file=.env.1password -- sh -c 'bun scripts/web/earn-tx-parity.ts --route <name> --input in.json --simulate "$WEB_SOLANA_RPC_URL"'
//
// --simulate also runs simulateTransaction (sigVerify off, replaceRecentBlockhash off) and prints
// its err and logs; it never sends. Use the test wallet behind POLICY_KEYPAIR (scripts/web/mobile-go-check.ts)
// as the input's wallet, and its smart account as settings.
//
// The Go side is a git-ignored go/workers/internal/<pkg>/<family>_scratch_test.go that reads the
// same input (EARN_PARITY_INPUT=<file>) and this script's output (EARN_PARITY_TS=<base64>), builds
// the message with the Go builder and fails on the first differing byte.
//
// Routes (add one per ported builder; each calls the function the Next route calls):
//   member-sync  {wallet, settings, vaultIndex, blockhash, computeUnits, mint, destination, amountRaw, decimals,
//                lookupTable?: {key, addresses}}
//                CL:7969-8026 shape: compute limit, idempotent ATA, execute_transaction_sync_v2
//                (SyncPayload::Transaction, the wallet as member) of a vault TransferChecked.
import { readFileSync } from "node:fs";
import { parseArgs } from "node:util";

import {
  createAssociatedTokenAccountIdempotentInstruction,
  createTransferCheckedInstruction,
  getAssociatedTokenAddressSync,
  TOKEN_PROGRAM_ID,
} from "../../web/node_modules/@solana/spl-token";
import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  Connection,
  PublicKey,
  TransactionMessage,
  VersionedTransaction,
  type TransactionInstruction,
} from "../../web/node_modules/@solana/web3.js";
import { instructionsToSynchronousTransactionDetailsV2, pda } from "../../web/packages/loyal-smart-accounts-core/src/index";
import { executeTransactionSyncV2 } from "../../web/packages/loyal-smart-accounts-core/src/internal/instructions/index";

type Built = { payer: PublicKey; instructions: TransactionInstruction[]; lookupTables?: AddressLookupTableAccount[] };

type Input = Record<string, string | number> & { lookupTable?: { key: string; addresses: string[] } };

function lookupTables(input: Input): AddressLookupTableAccount[] {
  if (!input.lookupTable) return [];
  return [
    new AddressLookupTableAccount({
      key: new PublicKey(input.lookupTable.key),
      state: { addresses: input.lookupTable.addresses.map((a) => new PublicKey(a)), deactivationSlot: BigInt("18446744073709551615"), lastExtendedSlot: 0, lastExtendedSlotStartIndex: 0 },
    }),
  ];
}

const routes: Record<string, (input: Input) => Built> = {
  "member-sync": (input) => {
    const wallet = new PublicKey(input.wallet);
    const settingsPda = new PublicKey(input.settings);
    const accountIndex = Number(input.vaultIndex);
    const [vaultPda] = pda.getSmartAccountPda({ settingsPda, accountIndex });
    const mint = new PublicKey(input.mint);
    const destination = new PublicKey(input.destination);
    const vaultAta = getAssociatedTokenAddressSync(mint, vaultPda, true, TOKEN_PROGRAM_ID);
    const destinationAta = getAssociatedTokenAddressSync(mint, destination, true, TOKEN_PROGRAM_ID);
    const compiled = instructionsToSynchronousTransactionDetailsV2({
      vaultPda,
      members: [wallet],
      transaction_instructions: [
        createTransferCheckedInstruction(vaultAta, mint, destinationAta, vaultPda, BigInt(input.amountRaw), Number(input.decimals)),
      ],
    });
    return {
      payer: wallet,
      lookupTables: lookupTables(input),
      instructions: [
        ComputeBudgetProgram.setComputeUnitLimit({ units: Number(input.computeUnits) }),
        createAssociatedTokenAccountIdempotentInstruction(wallet, destinationAta, destination, mint, TOKEN_PROGRAM_ID),
        executeTransactionSyncV2({
          feePayer: wallet,
          settingsPda,
          accountIndex,
          numSigners: 1,
          instructions: compiled.instructions,
          instruction_accounts: compiled.accounts,
        }),
      ],
    };
  },
};

const { values } = parseArgs({ options: { route: { type: "string" }, input: { type: "string" }, simulate: { type: "string" } } });
const build = values.route ? routes[values.route] : undefined;
if (!build || !values.input) {
  console.error(`usage: bun scripts/web/earn-tx-parity.ts --route <${Object.keys(routes).join("|")}> --input <file.json> [--simulate <rpc url>]`);
  process.exit(2);
}
const input = JSON.parse(readFileSync(values.input, "utf8")) as Input;
const built = build(input);
const message = new TransactionMessage({
  payerKey: built.payer,
  recentBlockhash: String(input.blockhash),
  instructions: built.instructions,
}).compileToV0Message(built.lookupTables ?? []);
console.log(Buffer.from(message.serialize()).toString("base64"));

if (values.simulate) {
  const result = await new Connection(values.simulate, "confirmed").simulateTransaction(new VersionedTransaction(message), {
    sigVerify: false,
    replaceRecentBlockhash: false,
  });
  console.error(JSON.stringify({ err: result.value.err, logs: result.value.logs }, null, 2));
}
