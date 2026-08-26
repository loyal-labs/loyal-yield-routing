import { createHash } from "node:crypto";
import bs58 from "bs58";
import { PublicKey } from "@solana/web3.js";

const KLEND_PROGRAM_ID = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const DEPOSIT_NAME = "deposit_reserve_liquidity_and_obligation_collateral_v2";
const DEPOSIT_DISCRIMINATOR = createHash("sha256")
  .update(`global:${DEPOSIT_NAME}`)
  .digest()
  .subarray(0, 8);
const RESERVE_DISCRIMINATOR = createHash("sha256")
  .update("account:Reserve")
  .digest()
  .subarray(0, 8);
const RESERVE_ACCOUNT_DATA_LENGTH = 8 + 8616;

function fail(message: string): never {
  throw new Error(message);
}

function required(name: string): string {
  const index = Bun.argv.indexOf(`--${name}`);
  const value = index >= 0 ? Bun.argv[index + 1] : undefined;
  return value?.trim() || fail(`--${name} is required`);
}

const rpcUrl = Bun.env.SOLANA_RPC_URL?.trim() || fail("SOLANA_RPC_URL is required");
const pullSignature = required("pull-signature");
const pullSlot = BigInt(required("pull-slot"));
const fleetSignature = required("fleet-signature");
const fleetSlot = BigInt(required("fleet-slot"));
const mint = required("mint");
const amount = BigInt(required("amount-raw"));
const walletTokenAccount = required("wallet-token-account");
const vaultTokenAccount = required("vault-token-account");
const targetReserve = required("target-reserve");

async function rpc(method: string, params: unknown[]): Promise<any> {
  const response = await fetch(rpcUrl, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
  });
  if (!response.ok) fail(`${method} returned HTTP ${response.status}`);
  const body = await response.json();
  if (body.error) fail(`${method} failed: ${JSON.stringify(body.error)}`);
  return body.result;
}

const statuses = await rpc("getSignatureStatuses", [
  [pullSignature, fleetSignature],
  { searchTransactionHistory: true },
]);
for (const [index, label] of ["pull", "Fleet deposit"].entries()) {
  const status = statuses?.value?.[index];
  if (
    !status ||
    status.err !== null ||
    !["confirmed", "finalized"].includes(status.confirmationStatus)
  ) {
    fail(`${label} signature has not reached confirmed commitment`);
  }
}

async function transaction(signature: string, slot: bigint, label: string): Promise<any> {
  const value = await rpc("getTransaction", [signature, {
    commitment: "confirmed",
    encoding: "jsonParsed",
    maxSupportedTransactionVersion: 0,
  }]);
  if (!value || value.meta?.err !== null || BigInt(value.slot) !== slot) {
    fail(`${label} transaction is not confirmed at expected slot ${slot}`);
  }
  return value;
}

const pull = await transaction(pullSignature, pullSlot, "pull");
const fleet = await transaction(fleetSignature, fleetSlot, "Fleet deposit");

function accountKeys(transactionValue: any): string[] {
  return transactionValue.transaction.message.accountKeys.map((key: any) =>
    typeof key === "string" ? key : key.pubkey
  );
}

function tokenAmount(transactionValue: any, side: "pre" | "post", account: string): bigint {
  const keys = accountKeys(transactionValue);
  const balances = side === "pre"
    ? transactionValue.meta.preTokenBalances
    : transactionValue.meta.postTokenBalances;
  const matches = (balances ?? []).filter((balance: any) =>
    balance.mint === mint && keys[balance.accountIndex] === account
  );
  if (matches.length !== 1) {
    fail(`${side} token balance for ${account} and mint ${mint} is not unique`);
  }
  return BigInt(matches[0].uiTokenAmount.amount);
}

function verifyDelta(
  transactionValue: any,
  account: string,
  expected: bigint,
  label: string,
): void {
  const actual = tokenAmount(transactionValue, "post", account) -
    tokenAmount(transactionValue, "pre", account);
  if (actual !== expected) fail(`${label} token delta is ${actual}, expected ${expected}`);
}

verifyDelta(pull, walletTokenAccount, -amount, "pull source");
verifyDelta(pull, vaultTokenAccount, amount, "pull destination");

const reserveInfo = await rpc("getAccountInfo", [targetReserve, {
  commitment: "confirmed",
  encoding: "base64",
}]);
if (!reserveInfo?.value || reserveInfo.value.owner !== KLEND_PROGRAM_ID) {
  fail(`target reserve ${targetReserve} is not owned by Kamino`);
}
const reserveData = Buffer.from(reserveInfo.value.data[0], "base64");
if (
  reserveData.length !== RESERVE_ACCOUNT_DATA_LENGTH ||
  !reserveData.subarray(0, 8).equals(RESERVE_DISCRIMINATOR)
) {
  fail(`target reserve ${targetReserve} is not a valid pinned KLend Reserve account`);
}
const reserveMint = new PublicKey(reserveData.subarray(128, 160)).toBase58();
const reserveLiquiditySupply = new PublicKey(reserveData.subarray(160, 192)).toBase58();
if (reserveMint !== mint) fail(`target reserve mint ${reserveMint} does not match ${mint}`);

verifyDelta(fleet, vaultTokenAccount, -amount, "Fleet idle source");
verifyDelta(fleet, reserveLiquiditySupply, amount, "Fleet reserve supply");

const instructions = [
  ...(fleet.transaction.message.instructions ?? []),
  ...(fleet.meta.innerInstructions ?? []).flatMap((group: any) => group.instructions ?? []),
];
const matchingDeposits = instructions.filter((instruction: any) => {
  if (instruction.programId !== KLEND_PROGRAM_ID || !instruction.data) return false;
  const accounts = (instruction.accounts ?? []).map((account: any) =>
    typeof account === "string" ? account : account.pubkey
  );
  if (
    accounts[4] !== targetReserve ||
    accounts[5] !== mint ||
    accounts[6] !== reserveLiquiditySupply ||
    accounts[9] !== vaultTokenAccount
  ) return false;
  const data = Buffer.from(bs58.decode(instruction.data));
  return data.length >= 16 &&
    data.subarray(0, 8).equals(DEPOSIT_DISCRIMINATOR) &&
    data.readBigUInt64LE(8) === amount;
});
if (matchingDeposits.length !== 1) {
  fail(`Fleet transaction has ${matchingDeposits.length} exact Kamino deposit instructions, expected one`);
}

console.log(JSON.stringify({
  status: "verified",
  commitment: "confirmed",
  reserveLiquiditySupply,
  kaminoDepositInstruction: DEPOSIT_NAME,
}));
