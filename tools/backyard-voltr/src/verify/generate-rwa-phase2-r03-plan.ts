/**
 * Build the current Maple R03 signed-unsent handoff.
 *
 * This is verification tooling only.  It uses the official Kamino SDK for an
 * absent-obligation setup prelude and the reviewed Phase-2 wrappers for the
 * route legs, then hands exact wires to the Go simulate-only command.  There
 * is deliberately no sendTransaction/sendRawTransaction or --execute path.
 */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { Reserve, initObligation, refreshObligation, refreshReserve, userMetadataPda } from "@kamino-finance/klend-sdk";
import { executeTransactionSyncV2 } from "@loyal-labs/loyal-smart-accounts-core/internal";
import { AccountRole, address, createNoopSigner, none, some, type Address, type Instruction } from "@solana/kit";
import bs58 from "bs58";
import { Connection, Keypair, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction, type AccountInfo } from "@solana/web3.js";
import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { toWeb3Instruction } from "../integrations/solana-compat.js";
import { buildRwaMultiplyArmReportInstruction, buildRwaMultiplyManagerInstructions, buildRwaMultiplyWithdrawalStagingInstruction, deriveRwaMultiplyVoltrAccounts, type RwaReportV1 } from "../integrations/rwa-multiply-voltr.js";
import { resolveFreshJupiterEdge } from "../policies/rwa-multiply-jupiter-headers.js";
import { buildPhaseTwoKaminoLaneOperations, hasConfiguredKaminoOracle, resolutionLanes } from "../policies/rwa-multiply-phase2-kamino.js";
import { buildExactJupiterSquadsExecution, signExactJupiterSquadsExecution } from "./rwa-phase2-jupiter-execution.js";
import { buildExactKaminoSquadsExecution } from "./rwa-phase2-kamino-execution.js";

type Json = Record<string, any>;
type Wire = { role: string; phase: string; wire: Uint8Array; signature: string };
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const PLAN = resolve(ROOT, "docs/evidence/backyard-rwa-go/phase2-runtime/r03-plan-v1.json");
const RESOLUTION = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const COMPILED = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json");
const LIMIT = 1_232;
const LANE = "Maple/syrupUSDC/USDC";
const AMOUNT = 1_000_000n;
const BRIDGE_POLICIES = {
  allocate: "HoDV7mtsb2u1VARZLYuGByW7cCsGWL9NFxHZs7WHjdzz",
  nav: "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP",
  stage: "ALz5Wkt82GhGFH1LfzbnAovkZ6t85ErovbxHUH3yY1wY",
  restore: "DjYYkQWb4zYbySfEndjVdg2NwZ8i77Fb9P1UFVbebc5t",
} as const;
function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function sha(value: Uint8Array | string): string { return createHash("sha256").update(value).digest("hex"); }
function optionalAddress(value: string): ReturnType<typeof none<Address>> | ReturnType<typeof some<Address>> { return hasConfiguredKaminoOracle(value) ? some(address(value)) : none<Address>(); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function array(value: unknown, label: string): any[] { invariant(Array.isArray(value), `${label} is not an array`); return value; }
function createInstruction(value: unknown): TransactionInstruction { const row = object(value, "PolicyCreate"); const data = Buffer.from(String(row.dataBase64), "base64"); return new TransactionInstruction({ programId: new PublicKey(String(row.programId)), data, keys: array(row.accounts, "PolicyCreate accounts").map((entry) => { const account = object(entry, "PolicyCreate account"); return { pubkey: new PublicKey(String(account.address)), isSigner: account.signer === true, isWritable: account.writable === true }; }) }); }
function compileInner(ix: TransactionInstruction) { const accounts: Array<{ pubkey: PublicKey; isWritable: boolean; isSigner: false }> = []; const indexOf = (pubkey: PublicKey, writable: boolean) => { const prior = accounts.findIndex((account) => account.pubkey.equals(pubkey)); if (prior >= 0) { accounts[prior]!.isWritable ||= writable; return prior; } invariant(accounts.length < 255, "initializer inner table exceeds u8"); accounts.push({ pubkey, isWritable: writable, isSigner: false }); return accounts.length - 1; }; invariant(ix.keys.every((key) => !key.isSigner || key.pubkey.toBase58() === RWA_MULTIPLY_ROUTE.squads.vault), "initializer has a non-vault signer"); const indexes = ix.keys.map((key) => indexOf(key.pubkey, key.isWritable)); const length = Buffer.alloc(2); length.writeUInt16LE(ix.data.length); return { accounts, bytes: Buffer.concat([Buffer.from([1, indexOf(ix.programId, false), indexes.length, ...indexes]), length, ix.data]) }; }
function sign(role: string, phase: string, payer: Keypair, instructions: readonly TransactionInstruction[], blockhash: string): Wire { const tx = new VersionedTransaction(new TransactionMessage({ payerKey: payer.publicKey, recentBlockhash: blockhash, instructions: [...instructions] }).compileToV0Message()); tx.sign([payer]); const wire = tx.serialize(); invariant(wire.length <= LIMIT, `${role} packet is ${wire.length} bytes`); return { role, phase, wire, signature: bs58.encode(tx.signatures[0]!) }; }
function policyAddress(seed: bigint): string { const bytes = Buffer.alloc(8); bytes.writeBigUInt64LE(seed); return PublicKey.findProgramAddressSync([Buffer.from("smart_account"), Buffer.from("policy"), new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings).toBuffer(), bytes], new PublicKey(RWA_MULTIPLY_ROUTE.squads.program))[0].toBase58(); }
function policyFor(policies: readonly Json[], operation: string): Json { const row = policies.find((value) => value.logicalName === `lane/${LANE}` && Array.isArray(value.operations) && value.operations[0] === operation); invariant(row, `compiled ${operation} policy is absent`); return row; }
function rebindJupiterAmount(rowValue: Json, inputAmount: bigint, outputThreshold: bigint): Json {
  const row = object(JSON.parse(JSON.stringify(rowValue)), "Jupiter amount clone");
  const header = object(row.header, "Jupiter header"); const indexes = object(header.indexes, "Jupiter indexes");
  const instruction = object(row.instruction, "Jupiter instruction"); const data = Buffer.from(String(instruction.dataBase64), "base64");
  const inputOffset = data.length - 19; const outputOffset = data.length - 11;
  data.writeBigUInt64LE(inputAmount, inputOffset); data.writeBigUInt64LE(outputThreshold, outputOffset);
  data.writeUInt16LE(RWA_MULTIPLY_ROUTE.assets.maxSlippageBps, Number(indexes.slippage)); data[Number(indexes.platformFee)] = 0;
  instruction.dataBase64 = data.toString("base64"); instruction.dataSha256 = sha(data);
  const originalQuote = object(row.quote, "Jupiter quote");
  row.quote = { inAmountRaw: inputAmount.toString(), outAmountRaw: outputThreshold.toString(), otherAmountThresholdRaw: outputThreshold.toString(), routePlanLength: originalQuote.routePlanLength };
  return row;
}
function exitPolicy(): Json {
  const pinnedAccounts: Record<number, string> = {
    0: RWA_MULTIPLY_ROUTE.assets.tokenProgram,
    2: RWA_MULTIPLY_ROUTE.squads.vault,
    3: "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN",
    6: RWA_MULTIPLY_ROUTE.squads.assetAta,
    7: RWA_MULTIPLY_ROUTE.assets.collateralMint,
    8: RWA_MULTIPLY_ROUTE.assets.assetMint,
  };
  return {
    logicalName: "swap/maple/syrupUSDC-USDC",
    seed: "139",
    policy: "FhvEZNhKwF3dPZL36rrcbo5TCvTZTRBadE4YFNWxxwVR",
    constraintCount: 1,
    swapEdges: [{
      from: "syrupUSDC",
      to: "USDC",
      constraintIndex: 0,
      authorityIndex: 2,
      sourceIndex: 3,
      destinationIndex: 6,
      sourceMintIndex: 7,
      destinationMintIndex: 8,
      sourceTokenProgramIndex: 0,
      destinationTokenProgramIndex: 0,
      authority: RWA_MULTIPLY_ROUTE.squads.vault,
      sourceCustody: pinnedAccounts[3],
      destinationCustody: RWA_MULTIPLY_ROUTE.squads.assetAta,
      sourceMint: RWA_MULTIPLY_ROUTE.assets.collateralMint,
      destinationMint: RWA_MULTIPLY_ROUTE.assets.assetMint,
      sourceTokenProgram: RWA_MULTIPLY_ROUTE.assets.tokenProgram,
      destinationTokenProgram: RWA_MULTIPLY_ROUTE.assets.tokenProgram,
    }],
    constraints: [{
      programId: RWA_MULTIPLY_ROUTE.programs.jupiter,
      accountPubkeys: [0, 2, 3, 6, 7, 8].map((index) => ({
        index,
        pubkeys: [pinnedAccounts[index]],
      })),
      data: [
        { kind: "slice-equals", offset: 0, valueHex: "c1209b3341d69c81" },
        { kind: "u64-less-than-or-equal", offset: 18, value: 1_000_000 },
        { kind: "u16-less-than-or-equal", offset: 34, value: 50 },
        { kind: "u8-equals", offset: 36, value: 0 },
      ],
    }],
  };
}
function wrapper(policy: string, instructions: readonly Instruction[], indices: readonly number[]): TransactionInstruction { const inner = instructions.map((ix) => ({ programId: ix.programAddress, accounts: (ix.accounts ?? []).map((account) => ({ address: account.address, signer: account.role === AccountRole.READONLY_SIGNER || account.role === AccountRole.WRITABLE_SIGNER, writable: account.role === AccountRole.WRITABLE || account.role === AccountRole.WRITABLE_SIGNER })), dataBase64: Buffer.from(ix.data ?? []).toString("base64") })); const result = spawnSync("cargo", ["run", "--quiet", "-p", "loyal-actions", "--bin", "compile-voltr-custom-execution"], { cwd: ROOT, input: JSON.stringify({ policy, delegatedSigner: RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, accountIndex: 0, constraintIndices: indices, inner }), encoding: "utf8", maxBuffer: 16 * 1024 * 1024 }); invariant(result.status === 0, `bridge wrapper compiler failed: ${(result.stderr || result.stdout).slice(-500)}`); const out = object(JSON.parse(result.stdout), "bridge wrapper"); const row = object(out.instruction, "bridge wrapper instruction"); return new TransactionInstruction({ programId: new PublicKey(String(row.programId)), data: Buffer.from(String(row.dataBase64), "base64"), keys: array(row.accounts, "bridge wrapper accounts").map((entry) => { const account = object(entry, "bridge wrapper account"); return { pubkey: new PublicKey(String(account.address)), isSigner: account.signer === true, isWritable: account.writable === true }; }) }); }

export async function buildR03Plan(connection: Connection, admin: Keypair, delegated: Keypair): Promise<Json> {
  invariant(admin.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin && delegated.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, "R03 signer identities drifted");
  const resolution = object(JSON.parse(readFileSync(RESOLUTION, "utf8")), "resolution"); const lane = resolutionLanes(resolution).find((value) => value.key === LANE); invariant(lane, "Maple resolution lane absent");
  const policies = array(object(JSON.parse(readFileSync(COMPILED, "utf8")), "compiled artifact").policies, "compiled policies");
  const latest = await connection.getLatestBlockhashAndContext("confirmed"); const obligation = new PublicKey(lane.resolved.obligation); const obligationInfo = await connection.getAccountInfo(obligation, "confirmed"); const absent = obligationInfo === null; if (!absent) invariant(obligationInfo.owner.toBase58() === RWA_MULTIPLY_ROUTE.kamino.program, "Maple obligation owner drifted");
  const wires: Wire[] = [];
  if (absent) { const [metadata] = await userMetadataPda(address(RWA_MULTIPLY_ROUTE.squads.vault), address(RWA_MULTIPLY_ROUTE.kamino.program)); const inner = toWeb3Instruction(initObligation({ args: { tag: 1, id: 0 } }, { obligationOwner: createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault), feePayer: createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault), obligation: address(lane.resolved.obligation), lendingMarket: address(lane.resolved.lendingMarket), seed1Account: address(lane.resolved.collateralReserve.liquidityMint), seed2Account: address(lane.resolved.debtReserve.liquidityMint), ownerUserMetadata: metadata, rent: address("SysvarRent111111111111111111111111111111111"), systemProgram: address(RWA_MULTIPLY_ROUTE.programs.system) }, [], address(RWA_MULTIPLY_ROUTE.kamino.program))); const compiled = compileInner(inner); const execute = executeTransactionSyncV2({ feePayer: admin.publicKey, settingsPda: new PublicKey(RWA_MULTIPLY_ROUTE.squads.settings), accountIndex: 0, numSigners: 1, instructions: compiled.bytes, instruction_accounts: [{ pubkey: admin.publicKey, isSigner: true, isWritable: false }, ...compiled.accounts], programId: new PublicKey(RWA_MULTIPLY_ROUTE.squads.program) }); wires.push(sign("init-obligation", "setup-authority", admin, [execute], latest.value.blockhash)); }
  const voltr = await deriveRwaMultiplyVoltrAccounts();
  const protectedBridgeAddresses = [
    RWA_MULTIPLY_ROUTE.customAdaptor.strategyConfig,
    voltr.reportTicket,
    voltr.strategyInitReceipt,
    voltr.idleAta,
    voltr.strategyAssetAta,
    RWA_MULTIPLY_ROUTE.squads.assetAta,
  ];
  const bridgeState = await connection.getMultipleAccountsInfoAndContext(
    protectedBridgeAddresses.map((value) => new PublicKey(value)),
    { commitment: "confirmed", minContextSlot: latest.context.slot },
  );
  invariant(bridgeState.value.every((value) => value !== null), "bridge state is incomplete");
  const [config, ticket, receipt] = bridgeState.value as AccountInfo<Buffer>[];
  invariant(ticket.data.length === 96 && receipt.data.length >= 112, "bridge report state layout drifted");
  let reportSequence = BigInt(latest.context.slot);
  const manager = createNoopSigner(RWA_MULTIPLY_ROUTE.squads.vault);
  const bridgeReport = (role: string): RwaReportV1 => {
    const sequence = reportSequence++;
    return {
      sequence,
      observedSlot: sequence,
      navAfterRaw: receipt.data.readBigUInt64LE(104),
      snapshotDigest: new Uint8Array(createHash("sha256").update(config.data).update(ticket.data).update(receipt.data).update(`${role}:${sequence}`).digest()),
    };
  };
  const managerPayload = async (role: "allocate" | "restore" | "nav", amount: bigint) => {
    const report = bridgeReport(role);
    const operation = role === "restore" ? "withdraw" : "deposit";
    const capital = await buildRwaMultiplyManagerInstructions(manager, amount, report);
    return wrapper(BRIDGE_POLICIES[role], [
      await buildRwaMultiplyArmReportInstruction(manager, operation, amount, report),
      operation === "withdraw" ? capital.withdraw : capital.deposit,
    ], [0, 1]);
  };
  wires.push(sign("voltr-allocate", "entry", delegated, [await managerPayload("allocate", AMOUNT)], latest.value.blockhash));

  const entryHeader = await resolveFreshJupiterEdge(connection, "USDC->syrupUSDC", "32", "Manifest"); const entryPolicy = policies.find((value) => value.logicalName === "swap/packed/16"); invariant(entryPolicy, "current Maple entry Jupiter policy artifact absent"); const entryExecution = await buildExactJupiterSquadsExecution({ connection, compiledPolicy: entryPolicy, headerRow: entryHeader, delegatedSigner: delegated.publicKey }); wires.push(sign("swap-entry", "entry", delegated, [signExactJupiterSquadsExecution({ execution: entryExecution, payer: delegated, recentBlockhash: latest.value.blockhash }).transaction.message.compiledInstructions ? entryExecution.outerInstruction : entryExecution.outerInstruction], latest.value.blockhash));
  const entryQuote = object(entryHeader.quote, "Maple entry quote");
  const depositAmount = BigInt(String(entryQuote.outAmountRaw));
  invariant(depositAmount > 0n && depositAmount <= AMOUNT, "Maple entry quoted output is outside the authorized cap");
  const reserveAddresses = [lane.resolved.collateralReserve.address, lane.resolved.debtReserve.address];
  const reserveInfos = await connection.getMultipleAccountsInfo(reserveAddresses.map((value) => new PublicKey(value)), "confirmed");
  invariant(reserveInfos.every((value) => value?.owner.toBase58() === RWA_MULTIPLY_ROUTE.kamino.program), "Maple reserve state is unavailable");
  const refreshReserveIxs = reserveInfos.map((value, index) => {
    const reserve = Reserve.decode(value!.data); const token = reserve.config.tokenInfo;
    return toWeb3Instruction(refreshReserve({ reserve: address(reserveAddresses[index]!), lendingMarket: address(lane.resolved.lendingMarket), pythOracle: optionalAddress(String(token.pythConfiguration.price)), switchboardPriceOracle: optionalAddress(String(token.switchboardConfiguration.priceAggregator)), switchboardTwapOracle: optionalAddress(String(token.switchboardConfiguration.twapAggregator)), scopePrices: optionalAddress(String(token.scopeConfiguration.priceFeed)) }, [], address(RWA_MULTIPLY_ROUTE.kamino.program)));
  });
  const refreshObligationIx = (reserves: readonly string[]) => toWeb3Instruction(refreshObligation({ lendingMarket: address(lane.resolved.lendingMarket), obligation: address(lane.resolved.obligation) }, reserves.map((value) => ({ address: address(value), role: AccountRole.WRITABLE })), address(RWA_MULTIPLY_ROUTE.kamino.program)));
  const makeKamino = (operation: string, phase: string, amount: bigint, refreshReserves: readonly string[], obligationReserves: readonly string[]) => { const shape = buildPhaseTwoKaminoLaneOperations(lane, amount).find((value) => value.operation === operation)!; const policy = policyFor(policies, operation); const execution = buildExactKaminoSquadsExecution({ compiledPolicy: policy, operation, innerInstruction: new TransactionInstruction({ programId: new PublicKey(shape.programId), data: Buffer.from(shape.dataBase64, "base64"), keys: shape.accounts.map((account) => ({ pubkey: new PublicKey(account.address), isSigner: account.signer, isWritable: account.writable })) }), delegatedSigner: delegated.publicKey }); const reserveRefreshes = refreshReserves.map((value) => refreshReserveIxs[reserveAddresses.indexOf(value)]!); wires.push(sign(operation, phase, delegated, [...reserveRefreshes, refreshObligationIx(obligationReserves), execution.outerInstruction], latest.value.blockhash)); };
  makeKamino("deposit", "entry", depositAmount, [lane.resolved.collateralReserve.address], []);
  makeKamino("withdraw", "unwind", depositAmount, [lane.resolved.collateralReserve.address], [lane.resolved.collateralReserve.address]);
  const freshExit = await resolveFreshJupiterEdge(connection, "syrupUSDC->USDC", "32", "Manifest");
  const exitQuote = object(freshExit.quote, "Maple exit quote");
  const exitQuoteInput = BigInt(String(exitQuote.inAmountRaw)); const exitQuoteOutput = BigInt(String(exitQuote.outAmountRaw));
  const returnAmount = (exitQuoteOutput * depositAmount) / exitQuoteInput;
  invariant(returnAmount > 0n && returnAmount <= AMOUNT, "Maple return amount is outside the authorized cap");
  const exitHeader = rebindJupiterAmount(freshExit, depositAmount, returnAmount);
  const exitExecution = await buildExactJupiterSquadsExecution({ connection, compiledPolicy: exitPolicy(), headerRow: exitHeader, delegatedSigner: delegated.publicKey }); wires.push(sign("swap-return", "return", delegated, [exitExecution.outerInstruction], latest.value.blockhash));
  const stage = await buildRwaMultiplyWithdrawalStagingInstruction(manager, returnAmount);
  wires.push(sign("stage-withdrawal", "return", delegated, [wrapper(BRIDGE_POLICIES.stage, [stage], [0])], latest.value.blockhash));
  wires.push(sign("voltr-restore", "return", delegated, [await managerPayload("restore", returnAmount)], latest.value.blockhash));
  wires.push(sign("nav-refresh", "nav", delegated, [await managerPayload("nav", 0n)], latest.value.blockhash));
  const hold = { action: "HOLD", reason: "single_loop_position_ready", observationId: sha(`${LANE}:${latest.context.slot}`), slot: latest.context.slot, broadcast: false, signature: null };
  return { schema: "loyal-backyard-rwa-phase2-runtime-signed-unsent/v1", lane: LANE, protectedAddresses: [RWA_MULTIPLY_ROUTE.squads.settings, RWA_MULTIPLY_ROUTE.squads.vault, lane.resolved.obligation, lane.resolved.collateralCustody.address, ...protectedBridgeAddresses], obligationAddress: lane.resolved.obligation, obligationAbsent: absent, hold, transactions: wires.map((row) => ({ role: row.role, phase: row.phase, signature: row.signature, packetBytes: row.wire.length, transactionBase64: Buffer.from(row.wire).toString("base64"), transactionSha256: sha(row.wire) })) };
}

async function main() { invariant(!process.argv.includes("--execute"), "this producer has no broadcast mode"); invariant(!existsSync(PLAN), `${PLAN} already exists`); const rpc = process.env.SOLANA_RPC_URL?.trim(); invariant(rpc, "SOLANA_RPC_URL is required"); const connection = new Connection(rpc, "confirmed"); invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta"); const admin = Keypair.fromSecretKey((await signingMaterialFromEnvironment("SOLANA_TESTING_PK")).secretKey); const delegated = Keypair.fromSecretKey((await signingMaterialFromEnvironment("POLICY_KEYPAIR")).secretKey); const plan = await buildR03Plan(connection, admin, delegated); writeFileSync(PLAN, `${JSON.stringify(plan, null, 2)}\n`, { flag: "wx", mode: 0o600 }); console.log(JSON.stringify({ verdict: "PLAN_READY", broadcast: false, plan: PLAN, next: "go run ./go/backyard-rwa-worker/cmd/r03-signed-unsent-evidence --plan docs/evidence/backyard-rwa-go/phase2-runtime/r03-plan-v1.json --out docs/evidence/backyard-rwa-go/phase2-runtime/signed-unsent-v1.json" }, null, 2)); }
if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) main().catch((error) => { console.error(error instanceof Error ? error.message : String(error)); process.exitCode = 1; });
