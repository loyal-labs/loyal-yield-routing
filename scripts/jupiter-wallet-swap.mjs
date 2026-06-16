#!/usr/bin/env bun
import {
  Connection,
  Keypair,
  PublicKey,
  VersionedTransaction,
} from "@solana/web3.js";
import {
  TOKEN_2022_PROGRAM_ID,
  TOKEN_PROGRAM_ID,
  getMint,
} from "@solana/spl-token";
import { readFileSync } from "node:fs";

const DEFAULT_QUOTE_API = "https://api.jup.ag/swap/v1/quote";
const DEFAULT_SWAP_API = "https://api.jup.ag/swap/v1/swap";
const DEFAULT_COMMITMENT = "confirmed";
const MAINNET_USDC_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const MAINNET_PYUSD_MINT = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo";

const args = parseArgs(process.argv.slice(2));
if (args.help || args.h) {
  showHelp();
  process.exit(0);
}

const cluster = args.cluster ?? "mainnet-beta";
const rpcUrl = resolveRpcUrl(cluster);
const connection = new Connection(rpcUrl, DEFAULT_COMMITMENT);
const keypairPath = requiredArg(args, "keypair");
const wallet = loadKeypair(keypairPath);
const inputMint = new PublicKey(args["input-mint"] ?? MAINNET_USDC_MINT);
const outputMint = new PublicKey(args["output-mint"] ?? MAINNET_PYUSD_MINT);
const slippageBps = Number(args["slippage-bps"] ?? "50");
const quoteApi = args["quote-api"] ?? DEFAULT_QUOTE_API;
const swapApi = args["swap-api"] ?? DEFAULT_SWAP_API;
const headers = jupiterApiHeaders();
const priorityMaxLamports = args["priority-max-lamports"];
const priorityLevel = args["priority-level"] ?? "veryHigh";

if (!Number.isInteger(slippageBps) || slippageBps < 0) {
  throw new Error(`slippage-bps must be a non-negative integer, got ${args["slippage-bps"]}`);
}

const inputMintInfo = await fetchMintInfo(inputMint);
const rawAmount = resolveRawAmount(args, inputMintInfo.decimals);

const quote = await fetchJupiterQuote({
  quoteApi,
  inputMint,
  outputMint,
  amount: rawAmount,
  slippageBps,
});
const outAmount = quoteAmount(quote, "outAmount");
const minOutAmount = quoteAmount(quote, "otherAmountThreshold");

console.log(
  [
    "Quote ok:",
    `input=${rawAmount}`,
    `output=${outAmount}`,
    `minOutput=${minOutAmount}`,
    `slippageBps=${slippageBps}`,
    `priceImpactPct=${quote.priceImpactPct ?? "unknown"}`,
  ].join(" "),
);

const swap = await fetchJupiterSwap({
  swapApi,
  quote,
  userPublicKey: wallet.publicKey,
  priorityMaxLamports,
  priorityLevel,
});
if (swap.simulationError) {
  throw new Error(`Jupiter swap build simulation error: ${JSON.stringify(swap.simulationError)}`);
}
if (typeof swap.swapTransaction !== "string") {
  throw new Error("Jupiter swap response did not include swapTransaction");
}

const transaction = VersionedTransaction.deserialize(
  Buffer.from(swap.swapTransaction, "base64"),
);
transaction.sign([wallet]);

const simulation = await connection.simulateTransaction(transaction, {
  commitment: DEFAULT_COMMITMENT,
  sigVerify: true,
});
if (simulation.value.err) {
  console.error(JSON.stringify({ simulation: simulation.value }, null, 2));
  throw new Error("Jupiter wallet swap simulation failed");
}
console.log(`Simulation ok: units=${simulation.value.unitsConsumed ?? "unknown"}`);

if (args["simulate-only"] === "1") {
  process.exit(0);
}

const signature = await connection.sendRawTransaction(transaction.serialize(), {
  maxRetries: 3,
  skipPreflight: false,
});
const confirmation =
  Number.isSafeInteger(swap.lastValidBlockHeight) && swap.lastValidBlockHeight > 0
    ? await connection.confirmTransaction(
        {
          signature,
          blockhash: transaction.message.recentBlockhash,
          lastValidBlockHeight: swap.lastValidBlockHeight,
        },
        DEFAULT_COMMITMENT,
      )
    : await connection.confirmTransaction(signature, DEFAULT_COMMITMENT);
if (confirmation.value.err) {
  throw new Error(
    `Jupiter wallet swap ${signature} failed: ${JSON.stringify(
      confirmation.value.err,
    )}`,
  );
}
console.log(`Jupiter wallet swap signature: ${signature}`);

function showHelp() {
  console.log(`Swap wallet tokens through Jupiter.

Defaults swap mainnet USDC to mainnet PYUSD.

Required:
  --keypair <path>             Signing wallet keypair.
  --amount-ui <amount>         UI amount, for example 2.5.
or
  --amount-raw <amount>        Raw integer amount.

Common options:
  --cluster mainnet-beta       Cluster or RPC URL. Default: mainnet-beta.
  --input-mint <mint>          Default: mainnet USDC.
  --output-mint <mint>         Default: mainnet PYUSD.
  --slippage-bps <bps>         Default: 50.
  --simulate-only              Quote, build, and simulate without sending.
  --priority-max-lamports <n>  Optional Jupiter priority fee max.
  --priority-level <level>     Default: veryHigh.

Environment:
  JUPITER_API_KEY              Optional Jupiter API key.

Examples:
  bun scripts/jupiter-wallet-swap.mjs --keypair ~/.config/solana/id.json --amount-ui 2.5 --simulate-only
  bun scripts/jupiter-wallet-swap.mjs --keypair ~/.config/solana/id.json --amount-ui 2.5
`);
}

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

function resolveRawAmount(parsed, decimals) {
  const raw = parsed["amount-raw"];
  const ui = parsed["amount-ui"];
  if ((raw && ui) || (!raw && !ui)) {
    throw new Error("provide exactly one of --amount-raw or --amount-ui");
  }
  if (raw) {
    if (!/^\d+$/.test(raw) || BigInt(raw) <= 0n) {
      throw new Error(`amount-raw must be a positive integer, got ${raw}`);
    }
    return BigInt(raw);
  }
  return parseUiAmount(ui, decimals);
}

function parseUiAmount(value, decimals) {
  if (!/^\d+(\.\d+)?$/.test(value)) {
    throw new Error(`amount-ui must be a positive decimal, got ${value}`);
  }
  const [whole, fraction = ""] = value.split(".");
  if (fraction.length > decimals) {
    throw new Error(`amount-ui has more than ${decimals} decimal places`);
  }
  const paddedFraction = fraction.padEnd(decimals, "0");
  const raw = BigInt(`${whole}${paddedFraction}`.replace(/^0+(?=\d)/, ""));
  if (raw <= 0n) {
    throw new Error("amount must be positive");
  }
  return raw;
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
  return fetchJson(url.toString(), { method: "GET", headers });
}

async function fetchJupiterSwap({
  swapApi,
  quote,
  userPublicKey,
  priorityMaxLamports,
  priorityLevel,
}) {
  const body = {
    quoteResponse: quote,
    userPublicKey: userPublicKey.toBase58(),
    wrapAndUnwrapSol: false,
    dynamicComputeUnitLimit: true,
  };
  if (priorityMaxLamports) {
    if (!/^\d+$/.test(priorityMaxLamports)) {
      throw new Error(`priority-max-lamports must be an integer, got ${priorityMaxLamports}`);
    }
    body.prioritizationFeeLamports = {
      priorityLevelWithMaxLamports: {
        maxLamports: Number(priorityMaxLamports),
        priorityLevel,
        global: false,
      },
    };
  }
  return fetchJson(swapApi, {
    method: "POST",
    headers: { "content-type": "application/json", ...headers },
    body: JSON.stringify(body),
  });
}

async function fetchJson(url, init) {
  const response = await fetch(url, init);
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`${url} failed with ${response.status}: ${text}`);
  }
  return JSON.parse(text);
}

function jupiterApiHeaders() {
  if (!process.env.JUPITER_API_KEY) {
    return {};
  }
  return { "x-api-key": process.env.JUPITER_API_KEY };
}
