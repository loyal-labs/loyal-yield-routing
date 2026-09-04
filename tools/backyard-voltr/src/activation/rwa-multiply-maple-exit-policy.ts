/**
 * Seed-139 Maple exit activation.  The default path only creates signed,
 * unsent wires and a Helius simulateBundle request.  --execute is an
 * explicitly gated, one-send reconciliation path.
 */
import { createHash } from "node:crypto";
import { chmodSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import bs58 from "bs58";
import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, Keypair, PublicKey, Transaction, VersionedTransaction } from "@solana/web3.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { resolveFreshJupiterEdge } from "../policies/rwa-multiply-jupiter-headers.js";
import { buildExactJupiterSquadsExecution, signExactJupiterSquadsExecution } from "../verify/rwa-phase2-jupiter-execution.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";

export const MAPLE_EXIT = {
  edge: "syrupUSDC->USDC",
  seedBefore: 138n,
  seed: 139n,
  maxAccounts: "32",
  dataLength: 37,
  discriminatorHex: "c1209b3341d69c81",
  amountOffset: 18,
  amountCapRaw: 1_000_000n,
  slippageOffset: 34,
  maxSlippageBps: 50,
  feeOffset: 36,
  program: RWA_MULTIPLY_ROUTE.programs.jupiter,
  accountPins: {
    0: RWA_MULTIPLY_ROUTE.assets.tokenProgram,
    2: RWA_MULTIPLY_ROUTE.squads.vault,
    3: "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN",
    6: "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe",
    7: "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
    8: RWA_MULTIPLY_ROUTE.assets.assetMint,
  },
} as const;

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const PACKET_LIMIT = 1_232;
const INVOKE = process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url));
type Json = Record<string, any>;
function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function sha(value: Uint8Array | string): string { return createHash("sha256").update(value).digest("hex"); }
function writePrivate(path: string, value: unknown, flag: "w" | "wx") {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag, mode: 0o600 }); chmodSync(path, 0o600);
}
function policyAddress(seed: bigint): string {
  const bytes = Buffer.alloc(8); bytes.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync([Buffer.from("smart_account"), Buffer.from("policy"), new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(), bytes], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0].toBase58();
}

/** Fail-closed proof of the Go-compatible legacy 37-byte SharedAccountsRoute. */
export function validateFreshMapleExitHeader(row: any) {
  const ix = row?.instruction;
  const data = Buffer.from(String(ix?.dataBase64 ?? ""), "base64");
  invariant(ix?.programId === MAPLE_EXIT.program, "fresh header program drifted");
  invariant(data.length === MAPLE_EXIT.dataLength, "fresh header is not the legacy 37-byte route");
  invariant(data.subarray(0, 8).toString("hex") === MAPLE_EXIT.discriminatorHex, "fresh header discriminator drifted");
  const observedPrefix = data.subarray(8, 18).toString("hex");
  invariant(data.readBigUInt64LE(18) <= MAPLE_EXIT.amountCapRaw, "fresh header amount exceeds 1m raw cap");
  invariant(data.readUInt16LE(34) <= MAPLE_EXIT.maxSlippageBps && data[36] === 0, "fresh header economics drifted");
  invariant(row?.header?.dialect === "shared-accounts-route" && row?.header?.accountCount === 28, "fresh header dialect/account count drifted");
  for (const [index, expected] of Object.entries(MAPLE_EXIT.accountPins)) {
    const account = ix.accounts[Number(index)];
    invariant(account?.pubkey === expected && account.isSigner === (Number(index) === 2), `fresh header account ${index} drifted`);
  }
  invariant(ix.accounts.filter((account: any) => account.isSigner).length === 1, "fresh header has an unexpected signer");
  return { dataLength: data.length, amountRaw: data.readBigUInt64LE(18).toString(), slippageBps: data.readUInt16LE(34), fee: data[36], observedRoutePlanPrefixHex: observedPrefix } as const;
}

function compilePolicy(input: Json): Json {
  const result = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin", "compile-backyard-rwa-maple-exit-policy"], {
    cwd: ROOT, input: JSON.stringify(input), encoding: "utf8", env: process.env,
  });
  invariant(result.status === 0, `Maple seed-139 compiler failed: ${String(result.stderr).slice(0, 400)}`);
  return JSON.parse(String(result.stdout)) as Json;
}

function syntheticPolicy(compiled: Json): Json {
  const pins = Object.entries(MAPLE_EXIT.accountPins).map(([index, pubkeys]) => ({ index: Number(index), pubkeys: [pubkeys] }));
  return {
    logicalName: "swap/maple/syrupUSDC-USDC", seed: String(compiled.replacementPolicySeed), policy: compiled.replacementPolicy,
    constraintCount: 1, swapEdges: [{ from: "syrupUSDC", to: "USDC", constraintIndex: 0, authorityIndex: 2, sourceIndex: 3, destinationIndex: 6, sourceMintIndex: 7, destinationMintIndex: 8, sourceTokenProgramIndex: 0, destinationTokenProgramIndex: 0, authority: MAPLE_EXIT.accountPins[2], sourceCustody: MAPLE_EXIT.accountPins[3], destinationCustody: MAPLE_EXIT.accountPins[6], sourceMint: MAPLE_EXIT.accountPins[7], destinationMint: MAPLE_EXIT.accountPins[8], sourceTokenProgram: MAPLE_EXIT.accountPins[0], destinationTokenProgram: MAPLE_EXIT.accountPins[0] }],
    constraints: [{ programId: MAPLE_EXIT.program, accountPubkeys: pins, data: [
      { kind: "slice-equals", offset: 0, valueHex: MAPLE_EXIT.discriminatorHex },
      { kind: "u64-less-than-or-equal", offset: 18, value: Number(MAPLE_EXIT.amountCapRaw) }, { kind: "u16-less-than-or-equal", offset: 34, value: MAPLE_EXIT.maxSlippageBps }, { kind: "u8-equals", offset: 36, value: 0 },
    ] }],
  };
}

async function simulateBundle(rpc: string, wires: readonly Uint8Array[], inspect: readonly string[]) {
  const response = await fetch(rpc, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ jsonrpc: "2.0", id: "maple-seed-139", method: "simulateBundle", params: [{ encodedTransactions: wires.map((wire) => Buffer.from(wire).toString("base64")) }, { preExecutionAccountsConfigs: wires.map(() => ({ addresses: inspect, encoding: "base64" })), postExecutionAccountsConfigs: wires.map(() => ({ addresses: inspect, encoding: "base64" })), skipSigVerify: false, simulationBank: { commitment: { commitment: "finalized" } }, transactionEncoding: "base64", replaceRecentBlockhash: false }] }) });
  invariant(response.ok, `simulateBundle HTTP ${response.status}`);
  return await response.json() as Json;
}
function state(value: readonly any[]) { return sha(JSON.stringify(value.map((account) => account ? { owner: account.owner.toBase58(), lamports: account.lamports, data: account.data.toString("base64") } : null))); }

async function readback(connection: Connection, address: string) {
  const info = await connection.getAccountInfoAndContext(new PublicKey(address), "finalized");
  invariant(info.value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, "seed-139 policy owner readback drifted");
  const Policy = (squadsGenerated as any).Policy;
  const decoded = Policy.fromAccountInfo(info.value)[0];
  invariant(decoded.settings.toBase58() === RWA_MULTIPLY_ROUTE.squads.settings && decoded.seed.toString() === "139", "seed-139 policy identity drifted");
  invariant(decoded.signers.length === 1 && decoded.signers[0].key.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, "seed-139 delegated signer drifted");
  invariant(decoded.policyState.__kind === "ProgramInteraction" && decoded.policyState.fields[0].instructionsConstraints.length === 1, "seed-139 ProgramInteraction drifted");
  return { slot: info.context.slot, dataSha256: sha(info.value.data), dataBase64: info.value.data.toString("base64") };
}

async function main() {
  const execute = process.argv.includes("--execute");
  const reconcile = process.argv.includes("--reconcile");
  const journalArg = process.argv.indexOf("--journal");
  const journal = journalArg >= 0 ? resolve(process.argv[journalArg + 1] ?? "") : "";
  invariant(!(execute && reconcile), "--execute and --reconcile are mutually exclusive");
  invariant(!execute || process.env.CONFIRM_MAINNET === "1", "--execute requires CONFIRM_MAINNET=1");
  invariant(!execute || (journal.endsWith(".json") && !existsSync(journal) && !existsSync(`${journal}.pending`)), "--execute requires a fresh --journal PATH");
  invariant(!reconcile || (journal.endsWith(".json") && !existsSync(journal) && existsSync(`${journal}.pending`)), "--reconcile requires one pending journal and no final journal");
  const rpc = process.env.SOLANA_RPC_URL?.trim(); invariant(rpc, "SOLANA_RPC_URL is required");
  const connection = new Connection(rpc, "finalized"); invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  if (reconcile) {
    const pending = JSON.parse(readFileSync(`${journal}.pending`, "utf8")) as Json;
    invariant(pending.policy === policyAddress(MAPLE_EXIT.seed) && pending.seed === "139", "pending journal is not seed-139 Maple exit activation");
    const signature = String(pending.transactions?.[0]?.signature ?? ""); invariant(signature.length > 0, "pending journal lacks create signature");
    const status = (await connection.getSignatureStatuses([signature], { searchTransactionHistory: true })).value[0];
    invariant(status?.err === null && status.confirmationStatus === "finalized", "seed-139 create signature is not finalized successfully");
    const finalReadback = await readback(connection, String(pending.policy));
    const settingsInfo = await connection.getAccountInfo(new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), "finalized"); invariant(settingsInfo, "Settings account is absent");
    const settingsDecoded = (squadsGenerated as any).Settings.fromAccountInfo(settingsInfo)[0]; invariant(settingsDecoded.policySeed.toString() === "139", "finalized Settings seed is not 139");
    const final = { ...pending, verdict: "FINALIZED_RECONCILED", broadcast: true, signature, finalizedSlot: status.slot, finalizedReadback: finalReadback };
    writePrivate(journal, final, "wx"); renameSync(`${journal}.pending`, `${journal}.sent-wire`);
    console.log(JSON.stringify({ verdict: final.verdict, signature, finalizedSlot: status.slot, journal }, null, 2)); return;
  }
  const settings = await connection.getAccountInfo(new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), "finalized"); invariant(settings, "Settings account is absent");
  const Settings = (squadsGenerated as any).Settings; const settingsDecoded = Settings.fromAccountInfo(settings)[0];
  invariant(BigInt(settingsDecoded.policySeed.toString()) === MAPLE_EXIT.seedBefore, "finalized Settings seed is not exactly 138");
  const target = policyAddress(MAPLE_EXIT.seed); invariant(await connection.getAccountInfo(new PublicKey(target), "finalized") === null, "seed-139 policy PDA already exists");
  const row = await resolveFreshJupiterEdge(connection, MAPLE_EXIT.edge, MAPLE_EXIT.maxAccounts, "Manifest"); const header = validateFreshMapleExitHeader(row);
  const latest = await connection.getLatestBlockhashAndContext("finalized");
  const compiled = compilePolicy({ policySeedBefore: Number(MAPLE_EXIT.seedBefore), settingsContextSlot: latest.context.slot, recentBlockhash: latest.value.blockhash, lastValidBlockHeight: latest.value.lastValidBlockHeight });
  invariant(compiled.replacementPolicy === target, "compiler replacement PDA drifted");
  const delegated = Keypair.fromSecretKey((await signingMaterialFromEnvironment("POLICY_KEYPAIR")).secretKey);
  const execution = await buildExactJupiterSquadsExecution({ connection, compiledPolicy: syntheticPolicy(compiled), headerRow: row, delegatedSigner: delegated.publicKey });
  const executionWire = signExactJupiterSquadsExecution({ execution, payer: delegated, recentBlockhash: latest.value.blockhash });
  const createWire = Buffer.from(String(compiled.transactionBase64), "base64"); invariant(createWire.length <= PACKET_LIMIT, "PolicyCreate packet exceeds packet limit");
  const createTx = Transaction.from(createWire); const createSignature = bs58.encode(createTx.signatures[0]!.signature!); invariant(createSignature === compiled.signature, "compiler signature drifted");
  const wires = [createWire, executionWire.wire]; const inspect = [RWA_MULTIPLY_ROUTE.squads.settings, target, RWA_MULTIPLY_ROUTE.squads.vault, MAPLE_EXIT.accountPins[3], MAPLE_EXIT.accountPins[6]];
  const before = await connection.getMultipleAccountsInfo(inspect.map((value) => new PublicKey(value)), "finalized");
  const statusesBefore = await connection.getSignatureStatuses([createSignature, bs58.encode(executionWire.transaction.signatures[0]!)], { searchTransactionHistory: true }); invariant(statusesBefore.value.every((value) => value === null), "activation signatures already exist");
  const simulation = await simulateBundle(rpc, wires, inspect); const results = simulation.result?.value;
  invariant(results?.summary === "succeeded" && results.transactionResults?.every((value: any) => value.err === null), `seed-139 sequential simulation failed: ${JSON.stringify(results)}`);
  const after = await connection.getMultipleAccountsInfo(inspect.map((value) => new PublicKey(value)), "finalized"); invariant(state(before) === state(after), "simulation changed finalized chain state");
  const plan = { schema: "loyal-backyard-rwa-maple-exit-policy-activation/v1", verdict: execute ? "SIGNED_SIMULATION_PASS_PENDING_SEND" : "SIGNED_UNSENT_PASS", broadcast: execute, lane: MAPLE_EXIT.edge, seedBefore: "138", seed: "139", policy: target, header, compiler: compiled, execution: { policy: execution.policy, constraintIndex: execution.constraintIndex, packetBytes: executionWire.packetBytes, signature: bs58.encode(executionWire.transaction.signatures[0]!), wireSha256: sha(executionWire.wire) }, simulation: { method: "simulateBundle", skipSigVerify: false, sigVerify: true, provider: simulation }, signatureAbsentOnChain: true, chainPreStateSha256: state(before), chainPostStateSha256: state(after), transactions: wires.map((wire, index) => ({ role: index === 0 ? "PolicyCreate" : "SquadsExecuteSync", packetBytes: wire.length, signature: index === 0 ? createSignature : bs58.encode(executionWire.transaction.signatures[0]!), transactionBase64: Buffer.from(wire).toString("base64"), transactionSha256: sha(wire) })) };
  if (!execute) { console.log(JSON.stringify(plan, null, 2)); return; }
  writePrivate(`${journal}.pending`, plan, "wx");
  const returned = await connection.sendRawTransaction(createWire, { skipPreflight: false, preflightCommitment: "finalized", maxRetries: 0, minContextSlot: latest.context.slot }); invariant(returned === createSignature, "PolicyCreate RPC signature mismatch");
  const confirmation = await connection.confirmTransaction({ signature: returned, blockhash: latest.value.blockhash, lastValidBlockHeight: latest.value.lastValidBlockHeight }, "finalized"); invariant(confirmation.value.err === null, "seed-139 PolicyCreate failed");
  const finalReadback = await readback(connection, target); const final = { ...plan, verdict: "FINALIZED_RECONCILED", broadcast: true, signature: returned, finalizedReadback: finalReadback };
  writePrivate(journal, final, "wx"); renameSync(`${journal}.pending`, `${journal}.sent-wire`); console.log(JSON.stringify({ verdict: final.verdict, signature: returned, journal }, null, 2));
}
if (INVOKE) main().catch((error) => { console.error(error instanceof Error ? error.message : String(error)); process.exitCode = 1; });
