// Signs the production Earn MAX Voltr deposit and claim wires over captured
// mainnet state for the SBF harness and the Go qualifier. `--check` fails when
// the committed fixtures differ from what the current builders produce.
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import {
  compilePreparedOperation,
  generated,
} from "@loyal-labs/loyal-smart-accounts-core";
import {
  type AccountInfo,
  Connection,
  Keypair,
  PublicKey,
} from "@solana/web3.js";
import bs58 from "bs58";
import { Buffer } from "buffer";

import { createSmartAccountVaultsClient } from "../src/client";
import { parseEarnMaxDepositQuote } from "../src/voltr/deposit-quote";
import {
  type VoltrWithdrawalConsent,
  voltrClaimPlan,
  voltrDepositPlan,
} from "../src/voltr/plans";
import {
  prepareEarnMaxVoltr,
  type VoltrPlanBuilder,
} from "../src/voltr/prepare";
import {
  deriveEarnMaxVoltrAuthority,
  VOLTR_DEPOSIT_PRICING_RANGES,
  VOLTR_LP_MINT,
  VOLTR_VAULT,
  voltrUserAccounts,
} from "../src/voltr/program";

const FIXTURES = join(
  import.meta.dir,
  "../../../../crates/squads-test-harness/fixtures",
);
const SQUADS = new PublicKey("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
const SETTINGS = new PublicKey("6YiPBScD6WZKkM4YRp9ui33W79zynP2hNsGQhMY8z9it");
const BLOCKHASH = "CktRuQ2mttgRGkXJtyksdKHjUdc2C4TgDzyB98oEzy8";
const LAST_VALID_BLOCK_HEIGHT = 100000;
const FEE_LAMPORTS = 5000;
const NOW = BigInt(1_793_920_731);
const CERTIFIED_SLOT = BigInt(465_888_518);
const DEPOSIT_RAW = BigInt(250_000_000);
const AUTHORITY_USDC_RAW = BigInt(500);
const owner = Keypair.fromSeed(new Uint8Array(32).fill(7));
const json = (path: string) =>
  JSON.parse(readFileSync(join(FIXTURES, path), "utf8"));

type Captured = {
  addresses: string[];
  result: {
    context: { slot: number };
    value: ({ data: [string]; lamports: number; owner: string } | null)[];
  };
};
const accounts = new Map<string, AccountInfo<Buffer>>();
for (const file of ["voltr/voltr-accounts.json", "voltr/lookup-tables.json"]) {
  const captured = json(file) as Captured;
  captured.addresses.forEach((address, i) => {
    const value = captured.result.value[i];
    if (value)
      accounts.set(address, {
        data: Buffer.from(value.data[0], "base64"),
        executable: false,
        lamports: value.lamports,
        owner: new PublicKey(value.owner),
        rentEpoch: 0,
      });
  });
}
const vaultData = accounts.get(VOLTR_VAULT.toBase58())!.data;
const lpSupply = accounts
  .get(VOLTR_LP_MINT.toBase58())!
  .data.readBigUInt64LE(36);
const authority = deriveEarnMaxVoltrAuthority(SETTINGS, SQUADS);
const user = voltrUserAccounts(authority);

// Owner-signed v1 consent and the Voltr receipt it names, both unchanged from
// the first protected-claim golden.
const consent: VoltrWithdrawalConsent = {
  authority: "213wZCPaVWK6Jz37Mi5um7p6ZxgKkXWX6VZyVBtRA2cq",
  cluster: "mainnet-beta",
  destination: "GmaDrppBC7P5ARKV8g3djiwP89vz1jLK23V2GBjuAEGB",
  expiresAt: "4102444800",
  feeBasisHash:
    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  lpRaw: "100",
  maxUserFeeLamports: "1000000",
  minClaimUsdcRaw: "100",
  nonce: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  owner: "GmaDrppBC7P5ARKV8g3djiwP89vz1jLK23V2GBjuAEGB",
  ownerSignature:
    "+2ax3TGZqNOI2kLeq4bKZOIgkEqek6iFrwoaShveodw8Bwgi3LDg9fttuk0KgEKNZwt/LKE/RcGN7iIDlGgQCA==",
  program: "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8",
  referenceSlot: "1",
  referenceUsdcRaw: "100",
  requestInstruction: 0,
  requestSignature:
    "2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6ijwfYmfZYsKRxboQMPh3R4kUhXRVdtSXFXMheka4Rc4P2",
  settings: "6YiPBScD6WZKkM4YRp9ui33W79zynP2hNsGQhMY8z9it",
  vault: "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA",
  version: "1",
  policySeed: "0",
};
const RECEIPT_BASE64 =
  "y1Hfja9sZXL1pOtWGNCf1yj7k/fgzWLScQTY48rZFcR9W1ZYEX1vVw7fAwqHc4KhNc5heezOUfG31k4qp1lqwJGUJ097QR2KZAAAAAAAAAAAAAAAAABkAAAAAAAAAAAAAQAAAAAAAAD8AAAAAAAAAA==";
const receipt = Buffer.from(RECEIPT_BASE64, "base64");
const tokenAccount = (mint: PublicKey, holder: PublicKey, amount: bigint) => {
  const data = Buffer.alloc(165);
  mint.toBuffer().copy(data, 0);
  holder.toBuffer().copy(data, 32);
  data.writeBigUInt64LE(amount, 64);
  data[108] = 1;
  return {
    data,
    executable: false,
    lamports: 2_039_280,
    owner: new PublicKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    rentEpoch: 0,
  };
};
const claimState = new Map(accounts);
claimState.set(
  user.assetAta.toBase58(),
  tokenAccount(
    new PublicKey("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
    authority,
    AUTHORITY_USDC_RAW,
  ),
);
claimState.set(user.receipt.toBase58(), {
  data: receipt,
  executable: false,
  lamports: 1_670_400,
  owner: new PublicKey("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"),
  rentEpoch: 0,
});

function connectionOver(state: Map<string, AccountInfo<Buffer>>) {
  const connection = new Connection("http://fixture.invalid");
  const context = { slot: Number(CERTIFIED_SLOT) + 1 };
  connection.getMultipleAccountsInfoAndContext = async (keys) => ({
    context,
    value: keys.map((key) => state.get(key.toBase58()) ?? null),
  });
  connection.getAccountInfoAndContext = async (key) => ({
    context,
    value: state.get(key.toBase58()) ?? null,
  });
  connection.getLatestBlockhash = async () => ({
    blockhash: BLOCKHASH,
    lastValidBlockHeight: LAST_VALID_BLOCK_HEIGHT,
  });
  connection.getFeeForMessage = async () => ({ context, value: FEE_LAMPORTS });
  return connection;
}

async function sign(build: VoltrPlanBuilder, connection: Connection) {
  const client = createSmartAccountVaultsClient({
    connection,
    programId: SQUADS,
  });
  client.sdk.smartAccounts.queries.fetchSettings = async () =>
    generated.Settings.fromArgs({
      seed: 16,
      settingsAuthority: PublicKey.default,
      threshold: 1,
      timeLock: 0,
      transactionIndex: 0,
      staleTransactionIndex: 0,
      archivalAuthority: null,
      archivableAfter: 0,
      bump: 0,
      signers: [{ key: owner.publicKey, permissions: { mask: 7 } }],
      accountUtilization: 2,
      policySeed: 0,
      reserved2: 0,
    });
  const prepared = await prepareEarnMaxVoltr({
    build,
    client,
    connection,
    owner: owner.publicKey,
    programId: SQUADS,
    settingsPda: SETTINGS,
  });
  const transaction = compilePreparedOperation({
    prepared,
    blockhash: BLOCKHASH,
  });
  transaction.sign([owner]);
  const wire = Buffer.from(transaction.serialize());
  return {
    signature: Buffer.from(transaction.signatures[0]!),
    wire,
    lookupTables: transaction.message.addressTableLookups.map((lookup) =>
      lookup.accountKey.toBase58(),
    ),
  };
}

function pins() {
  const merged: [number, number][] = [];
  for (const [start, end] of VOLTR_DEPOSIT_PRICING_RANGES) {
    const last = merged[merged.length - 1];
    if (last && last[1] >= start) last[1] = Math.max(last[1], end);
    else merged.push([start, end]);
  }
  return merged.map(([start, end]) => ({
    offset: start,
    hex: vaultData.subarray(start, end).toString("hex"),
  }));
}

const vectors = json("voltr-deposit-lp-vectors.json") as {
  vectors: { amountRaw: string; unixTs: string; lpMinted: string }[];
};
const minted = BigInt(
  vectors.vectors.find(
    (v) => BigInt(v.amountRaw) === DEPOSIT_RAW && BigInt(v.unixTs) === NOW,
  )!.lpMinted,
);

async function deposit(lpMin: bigint, lpMax: bigint) {
  const body = {
    vault: VOLTR_VAULT.toBase58(),
    lpMint: VOLTR_LP_MINT.toBase58(),
    authority: authority.toBase58(),
    lpAta: user.lpAta.toBase58(),
    amountRaw: DEPOSIT_RAW.toString(),
    certifiedSlot: CERTIFIED_SLOT.toString(),
    notAfterSlot: (CERTIFIED_SLOT + BigInt(32)).toString(),
    expiresAt: (NOW + BigInt(60)).toString(),
    vaultPins: pins(),
    lpSupplyRaw: lpSupply.toString(),
    lpBaselineRaw: "0",
    lpMinRaw: lpMin.toString(),
    lpMaxRaw: lpMax.toString(),
    supplyMaxRaw: (lpSupply + lpMax).toString(),
    certificateId: "fixture",
  };
  const quote = parseEarnMaxDepositQuote(body);
  const signed = await sign(
    async (context) => voltrDepositPlan(context, quote, NOW),
    connectionOver(accounts),
  );
  return { quote: body, ...signed };
}

function encode(signed: {
  signature: Buffer;
  wire: Buffer;
  lookupTables: string[];
}) {
  return {
    wireBase64: signed.wire.toString("base64"),
    wireBytes: signed.wire.length,
    signature: bs58.encode(signed.signature),
    lookupTables: signed.lookupTables,
  };
}

const good = await deposit(minted - BigInt(1), minted + BigInt(1));
const overmint = await deposit(minted - BigInt(2), minted - BigInt(1));
const depositFixture = {
  generatedBy:
    "web/packages/smart-account-vaults/scripts/voltr-wire-fixtures.ts",
  owner: owner.publicKey.toBase58(),
  settings: SETTINGS.toBase58(),
  authority: authority.toBase58(),
  blockhash: BLOCKHASH,
  nowUnixTs: NOW.toString(),
  quote: good.quote,
  ...encode(good),
  overmint: {
    lpMinRaw: overmint.quote.lpMinRaw,
    lpMaxRaw: overmint.quote.lpMaxRaw,
    ...encode(overmint),
  },
};

const claim = await sign(
  async (context) =>
    voltrClaimPlan(connectionOver(claimState), context, consent),
  connectionOver(claimState),
);
const claimFixture = {
  generatedBy:
    "web/packages/smart-account-vaults/scripts/voltr-wire-fixtures.ts",
  consent,
  owner: owner.publicKey.toBase58(),
  settings: SETTINGS.toBase58(),
  authority: authority.toBase58(),
  blockhash: BLOCKHASH,
  lastValidBlockHeight: LAST_VALID_BLOCK_HEIGHT,
  feeLamports: String(FEE_LAMPORTS),
  preexistingUsdcRaw: AUTHORITY_USDC_RAW.toString(),
  receiptBase64: RECEIPT_BASE64,
  ...encode(claim),
};

const outputs: [string, unknown][] = [
  ["voltr/deposit-wire.json", depositFixture],
  ["voltr/claim-wire.json", claimFixture],
];
const check = process.argv.includes("--check");
let stale = false;
for (const [path, value] of outputs) {
  const text = `${JSON.stringify(value, null, 2)}\n`;
  if (check) {
    if (readFileSync(join(FIXTURES, path), "utf8") !== text) {
      console.error(`${path} is stale; run bun scripts/voltr-wire-fixtures.ts`);
      stale = true;
    }
  } else writeFileSync(join(FIXTURES, path), text);
  const { wireBytes } = value as { wireBytes: number };
  console.log(path, "wireBytes", wireBytes);
}
console.log("overmint wireBytes", depositFixture.overmint.wireBytes);
if (stale) process.exit(1);
