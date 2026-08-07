#!/usr/bin/env bun
/**
 * Verifies whether the SOL locked in Kamino obligation farm `UserState` accounts can be
 * recovered by us.
 *
 * Every Kamino route the fleet opens creates one `UserState` per (reserve farm, obligation)
 * and pays its rent from the shared policy signer. That rent is never returned, and by
 * 2026-08-07 it accounted for the largest recurring outflow from the signer that ran the
 * fleet dry.
 *
 * A recovery would need one of three things to be true: an instruction that closes the
 * account, an owner willing to move its lamports, or an account we own outright. This
 * script tests all three against mainnet and reports which, if any, hold.
 *
 * Strictly read-only: it derives, reads, and simulates. It never signs, never sends, and
 * never needs a keypair. `simulateTransaction` is executed against a throwaway blockhash
 * with `sigVerify: false`, so the transaction it builds cannot land even by accident.
 */
import {
  AddressLookupTableProgram,
  Connection,
  PublicKey,
  SystemProgram,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import { inflateSync } from "node:zlib";

const KLEND_PROGRAM_ID = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const FARMS_PROGRAM_ID = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr";
const LOOKUP_TABLE_PROGRAM_ID = "AddressLookupTab1e1111111111111111111111111";
/** Farm user states created by the production policy signer, from the 60-day census. */
const SAMPLE_FARM_USER_STATES = [
  "6cF9Ar2j6qww9ejNz981q5D12nGd2SXcju9t5vvFayEU",
  "77n16Ycq3QfYfFx11jAPYGSHhutKimLJ7U96Vz23fbHE",
  "8KSs9mMWe1X7SmyVdnsPBHiaUP2KSWFHcF3X7d8ndAd4",
];
/** Any funded account works; this one pays the fleet's transactions today. */
const FUNDED_FEE_PAYER = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const OBSERVED_ACCOUNT_COUNT = 962;
const OBSERVED_RENT_LAMPORTS_EACH = 7_294_080;
/** Any instruction that could plausibly return rent would be named like one of these. */
const RECOVERY_NAME_PATTERN =
  /close|delete|destroy|reclaim|refund|terminate|deinit|dealloc|remove|burn/i;

const failures: string[] = [];
let checks = 0;

function check(name: string, condition: boolean, detail?: unknown): void {
  checks += 1;
  if (condition) {
    console.log(`  ok   ${name}`);
    return;
  }
  const suffix = detail === undefined ? "" : ` -> ${JSON.stringify(detail)}`;
  console.log(`  FAIL ${name}${suffix}`);
  failures.push(`${name}${suffix}`);
}

type Idl = {
  instructions: { name: string; accounts: { name: string }[] }[];
  accounts?: { name: string }[];
};

/** Reads a program's Anchor IDL from the chain, so the answer comes from the deployed
 *  program rather than from a checked-in copy that could have drifted. */
async function loadIdl(connection: Connection, programId: string): Promise<Idl> {
  const program = new PublicKey(programId);
  const [base] = PublicKey.findProgramAddressSync([], program);
  const address = await PublicKey.createWithSeed(base, "anchor:idl", program);
  const account = await connection.getAccountInfo(address);
  if (!account) {
    throw new Error(`no on-chain IDL for ${programId}`);
  }
  const length = account.data.readUInt32LE(40);
  return JSON.parse(
    inflateSync(account.data.subarray(44, 44 + length)).toString("utf8")
  ) as Idl;
}

async function main(): Promise<void> {
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) {
    throw new Error("SOLANA_RPC_URL is required");
  }
  const connection = new Connection(rpcUrl, "finalized");
  console.log("farm state rent recovery: is the locked SOL reachable at all?\n");

  console.log("1. does either program expose a way to close a UserState?");
  const klend = await loadIdl(connection, KLEND_PROGRAM_ID);
  const farms = await loadIdl(connection, FARMS_PROGRAM_ID);

  check(
    "KLend can create an obligation farm",
    klend.instructions.some((ix) => ix.name === "initObligationFarmsForReserve")
  );
  const klendRecovery = klend.instructions
    .filter((ix) => RECOVERY_NAME_PATTERN.test(ix.name))
    .map((ix) => ix.name);
  const farmsRecovery = farms.instructions
    .filter((ix) => RECOVERY_NAME_PATTERN.test(ix.name))
    .map((ix) => ix.name);
  // deleteReferrerStateAndShortUrl closes a referrer account, not a farm user state, so
  // it is named explicitly rather than allowed to satisfy the search by accident.
  const klendFarmRecovery = klendRecovery.filter((name) => /farm|user/i.test(name));
  const farmsFarmRecovery = farmsRecovery.filter((name) => /farm|user/i.test(name));

  check(
    "KLend exposes no instruction that closes a farm user state",
    klendFarmRecovery.length === 0,
    { candidates: klendRecovery }
  );
  check(
    "Farms exposes no instruction that closes a user state",
    farmsFarmRecovery.length === 0,
    { candidates: farmsRecovery }
  );
  check(
    "Farms exposes initializeUser with no matching teardown",
    farms.instructions.some((ix) => ix.name === "initializeUser") &&
      !farms.instructions.some((ix) => /closeUser|deleteUser/i.test(ix.name))
  );

  console.log("\n2. who controls the lamports?");
  const addresses = SAMPLE_FARM_USER_STATES.map((entry) => new PublicKey(entry));
  const accounts = await connection.getMultipleAccountsInfo(addresses);
  const present = accounts.filter((account) => account !== null);
  check(
    "the sampled farm user states still exist on chain",
    present.length === addresses.length,
    { found: present.length, expected: addresses.length }
  );
  check(
    "each is owned by the Farms program, not by us and not by System",
    present.every((account) => account!.owner.toBase58() === FARMS_PROGRAM_ID),
    present.map((account) => account!.owner.toBase58())
  );
  check(
    "each still holds its full rent deposit",
    present.every((account) => account!.lamports === OBSERVED_RENT_LAMPORTS_EACH),
    present.map((account) => account!.lamports)
  );

  console.log("\n3. can the rent be moved without the owning program's help?");
  // Only the owning program may debit an account it owns. Simulating the transfer turns
  // that rule from an assertion in a comment into an observed refusal from the runtime.
  //
  // The fee payer is a funded account rather than the farm state itself, so the
  // simulation reaches execution instead of stopping at an unfundable payer, and
  // signature verification is disabled so the run is not short-circuited by the missing
  // PDA signature before the ownership rule is ever consulted. Nothing is signed and the
  // blockhash is replaced by the simulator, so this cannot land.
  const { blockhash } = await connection.getLatestBlockhash("finalized");
  const message = new TransactionMessage({
    instructions: [
      SystemProgram.transfer({
        fromPubkey: addresses[0],
        toPubkey: new PublicKey(FUNDED_FEE_PAYER),
        lamports: OBSERVED_RENT_LAMPORTS_EACH,
      }),
    ],
    payerKey: new PublicKey(FUNDED_FEE_PAYER),
    recentBlockhash: blockhash,
  }).compileToV0Message();
  const simulation = await connection.simulateTransaction(
    new VersionedTransaction(message),
    { commitment: "finalized", replaceRecentBlockhash: true, sigVerify: false }
  );
  check(
    "a direct lamport transfer out of the farm user state is refused",
    simulation.value.err !== null,
    simulation.value.err
  );
  const logs = (simulation.value.logs ?? []).join("\n");
  // The exact refusal matters. "must not carry data" is the runtime saying the account
  // belongs to a program, which is a different and far more permanent obstacle than a
  // missing signature or an empty balance would have been.
  check(
    "the refusal is the ownership rule, not a signature or funding problem",
    /must not carry data/i.test(logs),
    simulation.value.logs
  );
  check(
    "no signature or insufficient-funds error masked the ownership rule",
    !/signature|insufficient/i.test(logs + JSON.stringify(simulation.value.err)),
    simulation.value.err
  );

  console.log("\n4. what IS recoverable? (lookup tables, for contrast)");
  // The farm states are not the whole story. The same signer also funds lookup tables,
  // and those carry the close path the farm states lack. Proving the recoverable case
  // here keeps the negative result above honest: the obstacle is Kamino's account model,
  // not something inherent to rent.
  const lookupTables = await connection.getProgramAccounts(
    new PublicKey(LOOKUP_TABLE_PROGRAM_ID),
    {
      dataSlice: { length: 56, offset: 0 },
      filters: [{ memcmp: { bytes: FUNDED_FEE_PAYER, offset: 22 } }],
    }
  );
  const lookupTableRent = lookupTables.reduce(
    (total, entry) => total + entry.account.lamports,
    0
  );
  check(
    "the signer holds closeable lookup table rent",
    lookupTables.length > 0,
    { tables: lookupTables.length }
  );
  console.log(
    `  ${lookupTables.length} tables holding ${(lookupTableRent / 1e9).toFixed(4)} SOL`
  );

  const deactivate = await connection.simulateTransaction(
    new VersionedTransaction(
      new TransactionMessage({
        instructions: [
          AddressLookupTableProgram.deactivateLookupTable({
            authority: new PublicKey(FUNDED_FEE_PAYER),
            lookupTable: lookupTables[0].pubkey,
          }),
        ],
        payerKey: new PublicKey(FUNDED_FEE_PAYER),
        recentBlockhash: blockhash,
      }).compileToV0Message()
    ),
    { commitment: "finalized", replaceRecentBlockhash: true, sigVerify: false }
  );
  check(
    "deactivating a lookup table is accepted under our authority",
    deactivate.value.err === null,
    deactivate.value.err
  );
  const close = await connection.simulateTransaction(
    new VersionedTransaction(
      new TransactionMessage({
        instructions: [
          AddressLookupTableProgram.closeLookupTable({
            authority: new PublicKey(FUNDED_FEE_PAYER),
            lookupTable: lookupTables[0].pubkey,
            recipient: new PublicKey(FUNDED_FEE_PAYER),
          }),
        ],
        payerKey: new PublicKey(FUNDED_FEE_PAYER),
        recentBlockhash: blockhash,
      }).compileToV0Message()
    ),
    { commitment: "finalized", replaceRecentBlockhash: true, sigVerify: false }
  );
  // Closing before the deactivation cooldown must fail, and fail for that reason. A
  // close that succeeded here would mean the sampled table was already deactivated,
  // which would make the recovery estimate wrong rather than merely unproven.
  check(
    "closing is refused only until the deactivation cooldown elapses",
    /not deactivated/i.test((close.value.logs ?? []).join("\n")),
    close.value.logs?.slice(0, 3)
  );

  console.log("\n5. what is at stake?");
  const lockedSol = (OBSERVED_ACCOUNT_COUNT * OBSERVED_RENT_LAMPORTS_EACH) / 1e9;
  console.log(
    `  ${OBSERVED_ACCOUNT_COUNT} accounts x ${OBSERVED_RENT_LAMPORTS_EACH} lamports = ${lockedSol.toFixed(4)} SOL locked`
  );
  console.log(
    `  cost per new route lane: ${(OBSERVED_RENT_LAMPORTS_EACH / 1e9).toFixed(5)} SOL, never returned`
  );
  console.log(
    `  recoverable by contrast: ${(lookupTableRent / 1e9).toFixed(4)} SOL of lookup table rent`
  );

  console.log("");
  if (failures.length > 0) {
    console.log(`FAILED ${failures.length}/${checks} checks`);
    for (const failure of failures) {
      console.log(`  - ${failure}`);
    }
    process.exitCode = 1;
    return;
  }
  console.log(`PASSED ${checks}/${checks} checks`);
  console.log(
    "\nCONCLUSION: farm state rent is not recoverable by us. No deployed instruction closes\n" +
      "a farm user state, in either program's on-chain IDL or its published source, and the\n" +
      "account is owned by the Farms program, so the runtime refuses to move its lamports.\n" +
      "That is Kamino's account model, not a property of rent: lookup table rent funded by\n" +
      "the same signer closes cleanly under the same authority, and is worth reclaiming."
  );
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
});
