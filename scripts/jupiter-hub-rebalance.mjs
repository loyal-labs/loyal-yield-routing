#!/usr/bin/env bun
import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  Connection,
  Keypair,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from "@solana/web3.js";
import {
  ASSOCIATED_TOKEN_PROGRAM_ID,
  TOKEN_2022_PROGRAM_ID,
  TOKEN_PROGRAM_ID,
  createAssociatedTokenAccountIdempotentInstruction,
  createTransferCheckedInstruction,
  getAssociatedTokenAddressSync,
  getMint,
} from "@solana/spl-token";
import { readFileSync } from "node:fs";

const CONFIG_SEED = Buffer.from("config");
const HUB_AUTHORITY_SEED = Buffer.from("hub-authority");
const WITHDRAW_INVENTORY_TAG = 2;
const DEFAULT_QUOTE_API = "https://api.jup.ag/swap/v1/quote";
const DEFAULT_SWAP_INSTRUCTIONS_API =
  "https://api.jup.ag/swap/v1/swap-instructions";
const DEFAULT_COMMITMENT = "confirmed";

const args = parseArgs(process.argv.slice(2));

const cluster = requiredArg(args, "cluster");
const rpcUrl = resolveRpcUrl(cluster);
const connection = new Connection(rpcUrl, DEFAULT_COMMITMENT);
const payer = loadKeypair(requiredArg(args, "keypair"));
const programId = new PublicKey(requiredArg(args, "program-id"));
const laneId = Number(requiredArg(args, "lane-id"));
const inputMint = new PublicKey(requiredArg(args, "input-mint"));
const outputMint = new PublicKey(requiredArg(args, "output-mint"));
const hubInputAmount = BigInt(requiredArg(args, "hub-input-amount"));
const hubOutputTopUpAmount = BigInt(requiredArg(args, "hub-output-top-up-amount"));
const slippageBps = Number(args["slippage-bps"] ?? "50");
const quoteApi = args["quote-api"] ?? DEFAULT_QUOTE_API;
const swapInstructionsApi =
  args["swap-instructions-api"] ?? DEFAULT_SWAP_INSTRUCTIONS_API;
const jupiterHeaders = jupiterApiHeaders();
const allowTreasuryOutputBuffer =
  args["allow-treasury-output-buffer"] === "1";
const computeUnitLimit = Number(args["compute-unit-limit"] ?? "600000");

if (!Number.isInteger(laneId) || laneId < 0 || laneId > 255) {
  throw new Error(`lane-id must fit in u8, got ${args["lane-id"]}`);
}
if (hubInputAmount <= 0n || hubOutputTopUpAmount <= 0n) {
  throw new Error("hub input and output top-up amounts must be positive");
}
if (!Number.isInteger(computeUnitLimit) || computeUnitLimit <= 0) {
  throw new Error(`compute-unit-limit must be a positive integer, got ${args["compute-unit-limit"]}`);
}

const inputMintInfo = await fetchMintInfo(inputMint);
const outputMintInfo = await fetchMintInfo(outputMint);
const hubAuthority = deriveHubAuthority(programId, laneId);
const hubInput = associatedTokenAddress(
  inputMint,
  hubAuthority,
  inputMintInfo.tokenProgram,
  true,
);
const hubOutput = associatedTokenAddress(
  outputMint,
  hubAuthority,
  outputMintInfo.tokenProgram,
  true,
);
const treasuryInput = associatedTokenAddress(
  inputMint,
  payer.publicKey,
  inputMintInfo.tokenProgram,
  false,
);
const treasuryOutput = associatedTokenAddress(
  outputMint,
  payer.publicKey,
  outputMintInfo.tokenProgram,
  false,
);

const quote = await fetchJupiterQuote({
  quoteApi,
  inputMint,
  outputMint,
  amount: hubInputAmount,
  slippageBps,
});
const swap = await fetchJupiterSwapInstructions({
  swapInstructionsApi,
  quote,
  userPublicKey: payer.publicKey,
});
const jupiterOutputAmount = quoteAmount(quote, "outAmount");
const jupiterMinimumOutputAmount = quoteAmount(quote, "otherAmountThreshold");
if (!allowTreasuryOutputBuffer && jupiterMinimumOutputAmount < hubOutputTopUpAmount) {
  throw new Error(
    `Jupiter guaranteed output ${jupiterMinimumOutputAmount} is below Hub top-up ${hubOutputTopUpAmount}; refusing to rely on treasury output balance to cover swap slippage.`,
  );
}

const withdrawInstruction = new TransactionInstruction({
  programId,
  keys: [
    { pubkey: deriveConfig(programId), isSigner: false, isWritable: false },
    { pubkey: payer.publicKey, isSigner: true, isWritable: false },
    { pubkey: hubInput, isSigner: false, isWritable: true },
    { pubkey: treasuryInput, isSigner: false, isWritable: true },
    { pubkey: inputMint, isSigner: false, isWritable: false },
    { pubkey: hubAuthority, isSigner: false, isWritable: false },
    { pubkey: inputMintInfo.tokenProgram, isSigner: false, isWritable: false },
  ],
  data: withdrawInventoryData(hubInputAmount, laneId),
});

const topUpInstruction = createTransferCheckedInstruction(
  treasuryOutput,
  outputMint,
  hubOutput,
  payer.publicKey,
  hubOutputTopUpAmount,
  outputMintInfo.decimals,
  [],
  outputMintInfo.tokenProgram,
);

const setupAtaInstructions = [
  createAssociatedTokenAccountIdempotentInstruction(
    payer.publicKey,
    treasuryInput,
    payer.publicKey,
    inputMint,
    inputMintInfo.tokenProgram,
    ASSOCIATED_TOKEN_PROGRAM_ID,
  ),
  createAssociatedTokenAccountIdempotentInstruction(
    payer.publicKey,
    treasuryOutput,
    payer.publicKey,
    outputMint,
    outputMintInfo.tokenProgram,
    ASSOCIATED_TOKEN_PROGRAM_ID,
  ),
];

const instructions = [
  ComputeBudgetProgram.setComputeUnitLimit({ units: computeUnitLimit }),
  ...jupiterComputeBudgetInstructions(swap.computeBudgetInstructions),
  ...setupAtaInstructions,
  withdrawInstruction,
  ...jupiterInstructions(swap.setupInstructions),
  jupiterInstruction(swap.swapInstruction),
  ...jupiterInstructions(swap.cleanupInstruction ? [swap.cleanupInstruction] : []),
  topUpInstruction,
];
const lookupTables = await fetchLookupTables(swap.addressLookupTableAddresses ?? []);
const latestBlockhash = await connection.getLatestBlockhash(DEFAULT_COMMITMENT);
const message = new TransactionMessage({
  payerKey: payer.publicKey,
  recentBlockhash: latestBlockhash.blockhash,
  instructions,
}).compileToV0Message(lookupTables);
const transaction = new VersionedTransaction(message);
transaction.sign([payer]);

const simulation = await connection.simulateTransaction(transaction, {
  commitment: DEFAULT_COMMITMENT,
  sigVerify: true,
});
if (simulation.value.err) {
  console.error(JSON.stringify({ simulation: simulation.value }, null, 2));
  throw new Error(`Jupiter Hub rebalance simulation failed`);
}
console.log(
  `Simulation ok: units=${simulation.value.unitsConsumed ?? "unknown"} input=${hubInputAmount} quoteOut=${jupiterOutputAmount} quoteMinOut=${jupiterMinimumOutputAmount} topUp=${hubOutputTopUpAmount}`,
);

if (args["simulate-only"] === "1") {
  process.exit(0);
}

const signature = await connection.sendRawTransaction(transaction.serialize(), {
  maxRetries: 3,
  skipPreflight: false,
});
const confirmation = await connection.confirmTransaction(
  {
    signature,
    blockhash: latestBlockhash.blockhash,
    lastValidBlockHeight: latestBlockhash.lastValidBlockHeight,
  },
  DEFAULT_COMMITMENT,
);
if (confirmation.value.err) {
  throw new Error(
    `Jupiter Hub rebalance ${signature} failed: ${JSON.stringify(
      confirmation.value.err,
    )}`,
  );
}
console.log(`Jupiter Hub rebalance signature: ${signature}`);

function parseArgs(values) {
  const parsed = {};
  for (let index = 0; index < values.length; index += 1) {
    const key = values[index];
    if (!key.startsWith("--")) {
      throw new Error(`unexpected argument: ${key}`);
    }
    const name = key.slice(2);
    const value = values[index + 1];
    if (value === undefined || value.startsWith("--")) {
      parsed[name] = "1";
    } else {
      parsed[name] = value;
      index += 1;
    }
  }
  return parsed;
}

function requiredArg(parsed, name) {
  const value = parsed[name];
  if (!value) {
    throw new Error(`missing --${name}`);
  }
  return value;
}

function quoteAmount(quote, name) {
  const value = quote[name];
  if (typeof value !== "string" || !/^\d+$/.test(value)) {
    throw new Error(`Jupiter quote missing valid ${name}`);
  }
  return BigInt(value);
}

function resolveRpcUrl(value) {
  switch (value) {
    case "m":
    case "mainnet":
    case "mainnet-beta":
      return "https://api.mainnet-beta.solana.com";
    case "d":
    case "devnet":
      return "https://api.devnet.solana.com";
    case "t":
    case "testnet":
      return "https://api.testnet.solana.com";
    case "l":
    case "local":
    case "localhost":
      return "http://localhost:8899";
    default:
      return value;
  }
}

function loadKeypair(path) {
  const bytes = JSON.parse(readFileSync(path, "utf8"));
  return Keypair.fromSecretKey(Uint8Array.from(bytes));
}

async function fetchMintInfo(mint) {
  const account = await connection.getAccountInfo(mint, DEFAULT_COMMITMENT);
  if (!account) {
    throw new Error(`mint account does not exist: ${mint.toBase58()}`);
  }
  const tokenProgram = supportedTokenProgram(account.owner);
  const mintInfo = await getMint(
    connection,
    mint,
    DEFAULT_COMMITMENT,
    tokenProgram,
  );
  return { tokenProgram, decimals: mintInfo.decimals };
}

function supportedTokenProgram(owner) {
  if (owner.equals(TOKEN_PROGRAM_ID)) {
    return TOKEN_PROGRAM_ID;
  }
  if (owner.equals(TOKEN_2022_PROGRAM_ID)) {
    return TOKEN_2022_PROGRAM_ID;
  }
  throw new Error(`unsupported mint owner ${owner.toBase58()}`);
}

function associatedTokenAddress(mint, owner, tokenProgram, allowOwnerOffCurve) {
  return getAssociatedTokenAddressSync(
    mint,
    owner,
    allowOwnerOffCurve,
    tokenProgram,
    ASSOCIATED_TOKEN_PROGRAM_ID,
  );
}

function deriveConfig(programId) {
  return PublicKey.findProgramAddressSync([CONFIG_SEED], programId)[0];
}

function deriveHubAuthority(programId, laneId) {
  return PublicKey.findProgramAddressSync(
    [HUB_AUTHORITY_SEED, Buffer.from([laneId])],
    programId,
  )[0];
}

function withdrawInventoryData(amount, laneId) {
  const data = Buffer.alloc(10);
  data[0] = WITHDRAW_INVENTORY_TAG;
  data.writeBigUInt64LE(amount, 1);
  data[9] = laneId;
  return data;
}

async function fetchJupiterQuote({
  quoteApi,
  inputMint,
  outputMint,
  amount,
  slippageBps,
}) {
  const url = new URL(quoteApi);
  url.searchParams.set("inputMint", inputMint.toBase58());
  url.searchParams.set("outputMint", outputMint.toBase58());
  url.searchParams.set("amount", amount.toString());
  url.searchParams.set("swapMode", "ExactIn");
  url.searchParams.set("slippageBps", String(slippageBps));
  url.searchParams.set("restrictIntermediateTokens", "true");
  url.searchParams.set("instructionVersion", "V2");
  return fetchJson(url.toString(), { method: "GET", headers: jupiterHeaders });
}

async function fetchJupiterSwapInstructions({
  swapInstructionsApi,
  quote,
  userPublicKey,
}) {
  return fetchJson(swapInstructionsApi, {
    method: "POST",
    headers: { "content-type": "application/json", ...jupiterHeaders },
    body: JSON.stringify({
      quoteResponse: quote,
      userPublicKey: userPublicKey.toBase58(),
      wrapAndUnwrapSol: false,
      dynamicComputeUnitLimit: false,
    }),
  });
}

async function fetchJson(url, init) {
  const maxAttempts = 5;
  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    const response = await fetch(url, init);
    const text = await response.text();
    if (response.ok) {
      return JSON.parse(text);
    }
    if (
      attempt + 1 < maxAttempts &&
      (response.status === 429 || response.status >= 500)
    ) {
      const delayMs = 500 * 2 ** attempt;
      console.warn(
        `${url} failed with ${response.status}; retrying after ${delayMs}ms`,
      );
      await sleep(delayMs);
      continue;
    }
    throw new Error(`${url} failed with ${response.status}: ${text}`);
  }
  throw new Error(`${url} failed after ${maxAttempts} attempts`);
}

function sleep(delayMs) {
  return new Promise((resolve) => setTimeout(resolve, delayMs));
}

function jupiterApiHeaders() {
  if (!process.env.JUPITER_API_KEY) {
    return {};
  }
  return { "x-api-key": process.env.JUPITER_API_KEY };
}

function jupiterInstructions(values = []) {
  return values.map(jupiterInstruction);
}

function jupiterComputeBudgetInstructions(values = []) {
  return jupiterInstructions(values).filter(
    (instruction) =>
      !instruction.programId.equals(ComputeBudgetProgram.programId) ||
      instruction.data[0] !== 2,
  );
}

function jupiterInstruction(value) {
  return new TransactionInstruction({
    programId: new PublicKey(value.programId),
    keys: value.accounts.map((account) => ({
      pubkey: new PublicKey(account.pubkey),
      isSigner: account.isSigner,
      isWritable: account.isWritable,
    })),
    data: Buffer.from(value.data, "base64"),
  });
}

async function fetchLookupTables(addresses) {
  const keys = addresses.map((address) => new PublicKey(address));
  if (keys.length === 0) {
    return [];
  }
  const accounts = await connection.getMultipleAccountsInfo(keys, DEFAULT_COMMITMENT);
  return accounts.map((account, index) => {
    if (!account) {
      throw new Error(`missing address lookup table ${keys[index].toBase58()}`);
    }
    return new AddressLookupTableAccount({
      key: keys[index],
      state: AddressLookupTableAccount.deserialize(account.data),
    });
  });
}
