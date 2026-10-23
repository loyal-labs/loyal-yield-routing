// Prints the base64 v0 message the TS sponsored smart-account create builds
// (web/apps/web/src/features/smart-accounts/server/onchain.ts createSponsoredSmartAccount:
// smartAccounts.prepare.create then client.send's compileToV0Message), so the Go builder
// (earnprep.SponsoredCreate) can be compared byte for byte.
//
//   bun scripts/web/earn-parity-sponsor.ts --input <file.json>
//     input: {wallet, sponsor, treasury, accountIndex, blockhash}; accountIndex is the settings
//     index the create claims (ProgramConfig.smartAccountIndex + 1).
//   op run --env-file=.env.1password -- sh -c 'bun scripts/web/earn-parity-sponsor.ts --simulate "$SOLANA_RPC_URL" --wallet <pubkey> --sponsor <funded pubkey>'
//     reads the live ProgramConfig and a fresh blockhash, prints the input it used (one JSON line)
//     and the message, then runs simulateTransaction (sigVerify off). It never sends.
//
// The Go side is go/workers/internal/earnprep/sponsor_scratch_test.go (git-ignored):
//   EARN_PARITY_INPUT=<file.json> EARN_PARITY_TS=<base64> go test -run Scratch ./internal/earnprep
import { readFileSync } from "node:fs";
import { parseArgs } from "node:util";

import { Connection, PublicKey, VersionedTransaction } from "../../web/node_modules/@solana/web3.js";
import { compilePreparedOperation, ProgramConfig } from "../../web/packages/loyal-smart-accounts-core/src/index";
import { codecs, pda, smartAccounts } from "../../web/packages/loyal-smart-accounts/src/index";

type Input = { wallet: string; sponsor: string; treasury: string; accountIndex: string; blockhash: string };

async function message(input: Input): Promise<VersionedTransaction> {
  const [settings] = pda.getSettingsPda({ accountIndex: BigInt(input.accountIndex) });
  const prepared = await smartAccounts.prepare.create({
    treasury: new PublicKey(input.treasury),
    creator: new PublicKey(input.sponsor),
    settings,
    settingsAuthority: null,
    threshold: 1,
    signers: [{ key: new PublicKey(input.wallet), permissions: codecs.Permissions.all() }],
    timeLock: 0,
    rentCollector: null,
  });
  return compilePreparedOperation({ prepared, blockhash: input.blockhash });
}

const { values } = parseArgs({
  options: {
    input: { type: "string" },
    simulate: { type: "string" },
    wallet: { type: "string" },
    sponsor: { type: "string" },
  },
});

if (values.simulate) {
  if (!values.wallet || !values.sponsor) throw new Error("--simulate needs --wallet and --sponsor");
  const connection = new Connection(values.simulate, "confirmed");
  const [programConfigPda] = pda.getProgramConfigPda({});
  const config = await ProgramConfig.fromAccountAddress(connection, programConfigPda, "confirmed");
  const { blockhash } = await connection.getLatestBlockhash("confirmed");
  const input: Input = {
    wallet: values.wallet,
    sponsor: values.sponsor,
    treasury: config.treasury.toBase58(),
    accountIndex: (BigInt(config.smartAccountIndex.toString()) + 1n).toString(),
    blockhash,
  };
  const tx = await message(input);
  console.log(JSON.stringify(input));
  console.log(Buffer.from(tx.message.serialize()).toString("base64"));
  const result = await connection.simulateTransaction(tx, { sigVerify: false, commitment: "confirmed" });
  console.log(JSON.stringify({ err: result.value.err, unitsConsumed: result.value.unitsConsumed, logs: result.value.logs }));
} else {
  if (!values.input) throw new Error("--input <file.json> or --simulate <rpc>");
  const input = JSON.parse(readFileSync(values.input, "utf8")) as Input;
  console.log(Buffer.from((await message(input)).message.serialize()).toString("base64"));
}
