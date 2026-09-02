/**
 * One exact nonzero Phase-2 market deposit proof per invocation.  The same
 * worker is used for the five required market representatives; each bundle
 * contains only prerequisite PolicyCreates, one constrained USDC->collateral
 * swap, permissionless K-Lend refreshes, and one constrained deposit.
 */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Obligation, Reserve, refreshObligation, refreshReserve } from "@kamino-finance/klend-sdk";
import bs58 from "bs58";
import { AccountRole, address, none, some, type Address } from "@solana/kit";
import { Connection, Keypair, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction, type AccountInfo } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { signingMaterialFromEnvironment } from "../integrations/signer.js";
import { toWeb3Instruction } from "../integrations/solana-compat.js";
import { resolveFreshJupiterEdge } from "../policies/rwa-multiply-jupiter-headers.js";
import { buildPhaseTwoKaminoLaneOperations, hasConfiguredKaminoOracle, resolutionLanes } from "../policies/rwa-multiply-phase2-kamino.js";
import { buildExactJupiterSquadsExecution, signExactJupiterSquadsExecution } from "./rwa-phase2-jupiter-execution.js";
import { buildExactKaminoSquadsExecution } from "./rwa-phase2-kamino-execution.js";

type Json = Record<string, unknown>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const COMPILED_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json");
const RESOLUTION_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const JUPITER_HEADERS_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-jupiter-headers-v1.json");
const PACKET_LIMIT = 1_232;
const ALLOWED = new Set(["OnRe/ONyc/USDC", "Prime/PRIME/USDC", "Maple/syrupUSDC/USDC", "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD"]);
function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function array(value: unknown, label: string): unknown[] { invariant(Array.isArray(value), `${label} is not an array`); return value; }
function sha256(value: Uint8Array | string): string { return createHash("sha256").update(value).digest("hex"); }
function stateSha256(infos: readonly (AccountInfo<Buffer> | null)[]): string { return sha256(JSON.stringify(infos.map((info) => info === null ? null : ({ owner: info.owner.toBase58(), executable: info.executable, lamports: info.lamports, data: info.data.toString("base64") })))); }
function createInstruction(value: unknown): TransactionInstruction { const raw = object(value, "compiled PolicyCreate"); const encoded = String(raw.dataBase64 ?? ""); const data = Buffer.from(encoded, "base64"); invariant(data.toString("base64") === encoded && sha256(data) === raw.dataSha256 && Array.isArray(raw.accounts), "compiled PolicyCreate wire drifted"); return new TransactionInstruction({ programId: new PublicKey(String(raw.programId)), data, keys: raw.accounts.map((entry, index) => { const account = object(entry, `PolicyCreate account ${index}`); return { pubkey: new PublicKey(String(account.address)), isSigner: account.signer === true, isWritable: account.writable === true }; }) }); }
function optionalAddress(value: string): ReturnType<typeof none<Address>> | ReturnType<typeof some<Address>> {
  return hasConfiguredKaminoOracle(value) ? some(address(value)) : none<Address>();
}
function frozenJupiterHeader(edgeKey: string, inputAmountRaw: bigint, outputThresholdRaw: bigint) {
  invariant(inputAmountRaw > 0n && inputAmountRaw <= 1_000_000_000_000n, `${edgeKey} frozen input is outside the exact policy cap`);
  invariant(outputThresholdRaw > 0n, `${edgeKey} frozen output threshold must be positive`);
  const headers = object(JSON.parse(readFileSync(JUPITER_HEADERS_PATH, "utf8")), "frozen Jupiter headers");
  const original = array(headers.rows, "frozen Jupiter rows").map((entry) => object(entry, "frozen Jupiter row")).find((entry) => entry.key === edgeKey);
  invariant(original, `${edgeKey} frozen Jupiter header is absent`);
  const row = object(JSON.parse(JSON.stringify(original)), `${edgeKey} frozen template clone`);
  const header = object(row.header, `${edgeKey} frozen header`); const indexes = object(header.indexes, `${edgeKey} frozen indexes`);
  const instruction = object(row.instruction, `${edgeKey} frozen instruction`); const originalData = Buffer.from(String(instruction.dataBase64), "base64");
  invariant(originalData.toString("base64") === instruction.dataBase64 && sha256(originalData) === instruction.dataSha256, `${edgeKey} frozen template hash drifted`);
  const inputOffset = originalData.length - 19; const outputOffset = originalData.length - 11;
  const slippageOffset = Number(indexes.slippage); const feeOffset = Number(indexes.platformFee);
  invariant(inputOffset >= 8 && outputOffset >= 0 && slippageOffset + 2 <= originalData.length && feeOffset < originalData.length, `${edgeKey} frozen dynamic tail is malformed`);
  const data = Buffer.from(originalData);
  data.writeBigUInt64LE(inputAmountRaw, inputOffset); data.writeBigUInt64LE(outputThresholdRaw, outputOffset);
  data.writeUInt16LE(RWA_MULTIPLY_ROUTE.assets.maxSlippageBps, slippageOffset); data[feeOffset] = 0;
  instruction.dataBase64 = data.toString("base64"); instruction.dataSha256 = sha256(data);
  const storedQuote = object(row.quote, `${edgeKey} frozen quote`);
  const originalMinimumOutputRaw = String(storedQuote.otherAmountThresholdRaw);
  invariant(/^\d+$/.test(originalMinimumOutputRaw) && BigInt(originalMinimumOutputRaw) > 0n, `${edgeKey} frozen template lacks a positive stored minimum output`);
  row.quote = { inAmountRaw: inputAmountRaw.toString(), outAmountRaw: outputThresholdRaw.toString(), otherAmountThresholdRaw: outputThresholdRaw.toString(), routePlanLength: storedQuote.routePlanLength };
  row.frozenTemplate = { source: JUPITER_HEADERS_PATH.replace(`${ROOT}/`, ""), inputOffset, outputOffset, slippageOffset, feeOffset, originalMinimumOutputRaw, productionRequirement: "production must obtain a current quote and bind a quote-derived minimum output before signing; this proof uses threshold=1 only to establish frozen-template capability" };
  return row;
}
async function freshness(connection: Connection, lane: ReturnType<typeof resolutionLanes>[number]) {
  const obligationInfo = await connection.getAccountInfo(new PublicKey(lane.resolved.obligation), "confirmed"); invariant(obligationInfo?.owner.toBase58() === RWA_MULTIPLY_ROUTE.kamino.program, `${lane.key} obligation absent`); const obligation = Obligation.decode(obligationInfo.data);
  const reserveAddresses = [...new Set([lane.resolved.collateralReserve.address, ...obligation.deposits.map((entry) => String(entry.depositReserve)), ...obligation.borrows.map((entry) => String(entry.borrowReserve))].filter((entry) => entry !== "11111111111111111111111111111111"))];
  const infos = await connection.getMultipleAccountsInfo(reserveAddresses.map((entry) => new PublicKey(entry)), "confirmed"); invariant(infos.every((info) => info?.owner.toBase58() === RWA_MULTIPLY_ROUTE.kamino.program), `${lane.key} refresh reserve missing`);
  const reserves = infos.map((info, index) => { invariant(info, "reserve disappeared"); const reserve = Reserve.decode(info.data); const tokenInfo = reserve.config.tokenInfo; return toWeb3Instruction(refreshReserve({ reserve: address(reserveAddresses[index]!), lendingMarket: address(lane.resolved.lendingMarket), pythOracle: optionalAddress(String(tokenInfo.pythConfiguration.price)), switchboardPriceOracle: optionalAddress(String(tokenInfo.switchboardConfiguration.priceAggregator)), switchboardTwapOracle: optionalAddress(String(tokenInfo.switchboardConfiguration.twapAggregator)), scopePrices: optionalAddress(String(tokenInfo.scopeConfiguration.priceFeed)) }, [], address(RWA_MULTIPLY_ROUTE.kamino.program))); });
  const obligationReserveAddresses = [...obligation.deposits.map((entry) => String(entry.depositReserve)), ...obligation.borrows.map((entry) => String(entry.borrowReserve))].filter((entry) => entry !== "11111111111111111111111111111111");
  return { instructions: [...reserves, toWeb3Instruction(refreshObligation({ lendingMarket: address(lane.resolved.lendingMarket), obligation: address(lane.resolved.obligation) }, obligationReserveAddresses.map((entry) => ({ address: address(entry), role: AccountRole.WRITABLE })), address(RWA_MULTIPLY_ROUTE.kamino.program)))], reserveAddresses };
}
function providerSummary(value: unknown, tokenAccountIndex: number | null): Json {
  const root = object(value, "Helius response");
  if (root.error) return { kind: "rpc-error", error: root.error };
  const result = object(root.result, "Helius result");
  const body = object(result.value, "Helius result value");
  const canonicalReturnData = (value: unknown) => {
    if (value === null || value === undefined) return null;
    const row = object(value, "Helius transaction return data");
    const data = row.data;
    invariant(typeof row.programId === "string" && Array.isArray(data) && data.length === 2
      && typeof data[0] === "string" && data[1] === "base64"
      && Buffer.from(data[0], "base64").toString("base64") === data[0], "Helius return data is malformed");
    return { programId: row.programId, dataBase64: data[0] };
  };
  const canonicalInstructionError = (value: unknown) => {
    if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
    const row = value as Json;
    const raw = row.InstructionError;
    if (!Array.isArray(raw) || raw.length !== 2 || !Number.isSafeInteger(raw[0]) || (raw[0] as number) < 0) return null;
    return { instructionIndex: raw[0] as number, error: raw[1] ?? null };
  };
  const capturedTokenAmount = (value: unknown) => {
    if (tokenAccountIndex === null || !Array.isArray(value)) return null;
    const account = value[tokenAccountIndex];
    if (account === null || account === undefined || typeof account !== "object" || Array.isArray(account)) return null;
    const data = (account as Json).data;
    const base64 = Array.isArray(data) && typeof data[0] === "string" && data[1] === "base64" ? data[0] : null;
    if (base64 === null) return null;
    const bytes = Buffer.from(base64, "base64");
    return bytes.length >= 72 ? bytes.readBigUInt64LE(64).toString() : null;
  };
  return {
    kind: "result",
    contextSlot: object(result.context ?? {}, "Helius context").slot ?? null,
    summary: body.summary ?? null,
    transactionResults: Array.isArray(body.transactionResults) ? body.transactionResults.map((entry) => {
      const row = object(entry, "Helius transaction result");
      const err = row.err ?? null;
      return {
        err,
        topLevelInstructionError: canonicalInstructionError(err),
        returnData: canonicalReturnData(row.returnData),
        capturedPre: Array.isArray(row.preExecutionAccounts),
        capturedPost: Array.isArray(row.postExecutionAccounts),
        capturedTokenAmountPre: capturedTokenAmount(row.preExecutionAccounts),
        capturedTokenAmountPost: capturedTokenAmount(row.postExecutionAccounts),
        logsSha256: sha256(JSON.stringify(row.logs ?? [])),
        ...(process.env.RWA_PHASE2_DIAGNOSTIC_LOGS === "1" ? { logs: row.logs ?? [] } : {}),
      };
    }) : [],
  };
}

async function main() {
  const laneKey = process.argv[2]; invariant(typeof laneKey === "string" && ALLOWED.has(laneKey), `usage: ${[...ALLOWED].join(" | ")}`);
  const evidenceVersion = process.env.RWA_PHASE2_EVIDENCE_VERSION?.trim() || "v5";
  invariant(/^v[1-9][0-9]*$/.test(evidenceVersion), "RWA_PHASE2_EVIDENCE_VERSION must be a vN suffix");
  const outputPath = resolve(ROOT, `docs/evidence/backyard-rwa-go/policy-helius-market-${laneKey.replaceAll("/", "-")}-${evidenceVersion}.json`); invariant(!existsSync(outputPath), `${outputPath} already exists`);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim(); invariant(rpcUrl && new URL(rpcUrl).hostname.includes("helius"), "Helius SOLANA_RPC_URL is required");
  const compiledBytes = readFileSync(COMPILED_PATH); const artifact = object(JSON.parse(compiledBytes.toString("utf8")), "compiled artifact"); invariant(artifact.phase === "phase2" && Array.isArray(artifact.policies), "Phase-2 artifact absent"); const policies = artifact.policies.map((entry) => object(entry, "compiled policy"));
  const [market, collateral] = laneKey.split("/"); invariant(market && collateral, "lane key malformed"); const deposit = policies.find((policy) => policy.logicalName === `lane/${laneKey}` && Array.isArray(policy.operations) && policy.operations.length === 1 && policy.operations[0] === "deposit"); invariant(deposit, `${laneKey} deposit policy absent`);
  const edgeKey = `USDC->${collateral}`;
  const swapPolicy = policies.find((policy) => Array.isArray(policy.swapEdges) && policy.swapEdges.some((entry) => { const edge = object(entry, "packed swap edge"); return edge.from === "USDC" && edge.to === collateral; })); invariant(swapPolicy, `${edgeKey} packed policy absent`);
  const resolution = object(JSON.parse(readFileSync(RESOLUTION_PATH, "utf8")), "resolution"); const lane = resolutionLanes(resolution).find((entry) => entry.key === laneKey); invariant(lane, `${laneKey} resolution absent`);
  const admin = Keypair.fromSecretKey((await signingMaterialFromEnvironment("SOLANA_TESTING_PK")).secretKey); const delegated = Keypair.fromSecretKey((await signingMaterialFromEnvironment("POLICY_KEYPAIR")).secretKey); invariant(admin.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.setupAdmin && delegated.publicKey.toBase58() === RWA_MULTIPLY_ROUTE.squads.delegatedExecutor, "signer identity drifted");
  const connection = new Connection(rpcUrl, "confirmed"); invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "not mainnet-beta");
  const frozenTemplate = process.env.RWA_PHASE2_FROZEN_JUPITER_TEMPLATE === "1";
  const freshHeader = frozenTemplate ? frozenJupiterHeader(edgeKey, 1_000_000n, 1n) : await resolveFreshJupiterEdge(connection, edgeKey);
  invariant(freshHeader.pass === true, `${edgeKey} Jupiter instruction did not resolve`);
  const quote = object(freshHeader.quote, `${edgeKey} Jupiter quote`);
  const freshInstruction = object(freshHeader.instruction, `${edgeKey} Jupiter instruction`);
  const frozenMetadata = frozenTemplate ? object(object(freshHeader, `${edgeKey} frozen header`).frozenTemplate, `${edgeKey} frozen template metadata`) : null;
  // Frozen route templates prove policy capability, not quote exactness. Keep
  // a 10% buffer under the captured minimum so harmless AMM drift cannot
  // starve the deposit while still clearing Kamino's non-dust minimums.
  const depositAmountRaw = frozenTemplate
    ? (BigInt(String(frozenMetadata!.originalMinimumOutputRaw)) * 9n) / 10n
    : BigInt(String(quote.otherAmountThresholdRaw ?? ""));
  invariant(depositAmountRaw > 0n, `${edgeKey} Jupiter output threshold is absent`);
  const source = object(freshHeader.source, `${edgeKey} source`); const sourceBalance = BigInt((await connection.getTokenAccountBalance(new PublicKey(String(source.ata)), "confirmed")).value.amount);
  invariant(sourceBalance >= BigInt(String(quote.inAmountRaw)), `${edgeKey} frozen input exceeds confirmed custody balance`);
  const latest = await connection.getLatestBlockhashAndContext("confirmed");
  const canonical = buildPhaseTwoKaminoLaneOperations(lane, depositAmountRaw).find((entry) => entry.operation === "deposit"); invariant(canonical, `${laneKey} canonical deposit absent`); const kaminoInner = new TransactionInstruction({ programId: new PublicKey(canonical.programId), data: Buffer.from(canonical.dataBase64, "base64"), keys: canonical.accounts.map((account) => ({ pubkey: new PublicKey(account.address), isSigner: account.signer, isWritable: account.writable })) });
  const swap = await buildExactJupiterSquadsExecution({ connection, compiledPolicy: swapPolicy, headerRow: freshHeader, delegatedSigner: delegated.publicKey }); const depositExecution = buildExactKaminoSquadsExecution({ compiledPolicy: deposit, operation: "deposit", innerInstruction: kaminoInner, delegatedSigner: delegated.publicKey }); const refresh = await freshness(connection, lane);
  const prefixCreates = policies.slice(0, policies.indexOf(deposit) + 1).map((policy) => createInstruction(policy.createInstruction));
  const normalWire = (payer: Keypair, instruction: TransactionInstruction, role: string) => { const tx = new VersionedTransaction(new TransactionMessage({ payerKey: payer.publicKey, recentBlockhash: latest.value.blockhash, instructions: [instruction] }).compileToV0Message()); tx.sign([payer]); const wire = tx.serialize(); invariant(wire.length <= PACKET_LIMIT, `${role} packet ${wire.length} exceeds ${PACKET_LIMIT}`); return { role, wire, signature: bs58.encode(tx.signatures[0]!) }; };
  const swapWire = signExactJupiterSquadsExecution({ execution: swap, payer: delegated, recentBlockhash: latest.value.blockhash });
  const wires = [
    ...prefixCreates.map((instruction, index) => normalWire(admin, instruction, `create-prefix-${index + 1}`)),
    { role: `swap-${edgeKey}`, wire: swapWire.wire, signature: bs58.encode(swapWire.transaction.signatures[0]!) },
    (() => { const tx = new VersionedTransaction(new TransactionMessage({ payerKey: delegated.publicKey, recentBlockhash: latest.value.blockhash, instructions: refresh.instructions }).compileToV0Message()); tx.sign([delegated]); const wire = tx.serialize(); invariant(wire.length <= PACKET_LIMIT, `refresh packet ${wire.length} exceeds ${PACKET_LIMIT}`); return { role: "permissionless-refresh", wire, signature: bs58.encode(tx.signatures[0]!) }; })(),
    normalWire(delegated, depositExecution.outerInstruction, "deposit"),
  ];
  invariant(wires.length <= 20, `${laneKey} bundle exceeds Helius 20 transaction cap`); const inspected = [RWA_MULTIPLY_ROUTE.squads.settings, RWA_MULTIPLY_ROUTE.squads.vault, String(deposit.policy), String(swapPolicy.policy), lane.resolved.obligation, lane.resolved.collateralCustody.address, RWA_MULTIPLY_ROUTE.squads.assetAta];
  const before = await connection.getMultipleAccountsInfoAndContext(inspected.map((entry) => new PublicKey(entry)), { commitment: "confirmed", minContextSlot: latest.context.slot }); const statusesBefore = await connection.getSignatureStatuses(wires.map((entry) => entry.signature), { searchTransactionHistory: true }); invariant(statusesBefore.value.every((entry) => entry === null), "signed-unsent bundle wire already landed");
  const http = await fetch(rpcUrl, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ jsonrpc: "2.0", id: `rwa-phase2-market-${market}`, method: "simulateBundle", params: [{ encodedTransactions: wires.map((entry) => Buffer.from(entry.wire).toString("base64")) }, { preExecutionAccountsConfigs: wires.map(() => ({ addresses: inspected, encoding: "base64" })), postExecutionAccountsConfigs: wires.map(() => ({ addresses: inspected, encoding: "base64" })), skipSigVerify: false, simulationBank: { commitment: { commitment: "confirmed" } }, transactionEncoding: "base64", replaceRecentBlockhash: false }] }) }); const result = await http.json() as unknown;
  const statusesAfter = await connection.getSignatureStatuses(wires.map((entry) => entry.signature), { searchTransactionHistory: true }); invariant(statusesAfter.value.every((entry) => entry === null), "simulation landed a signed wire"); const after = await connection.getMultipleAccountsInfoAndContext(inspected.map((entry) => new PublicKey(entry)), { commitment: "confirmed", minContextSlot: before.context.slot }); invariant(stateSha256(before.value) === stateSha256(after.value), "confirmed state changed across signed-unsent bundle");
  const custodyIndex = inspected.indexOf(lane.resolved.collateralCustody.address);
  invariant(custodyIndex >= 0, "collateral custody is not captured by the signed-unsent bundle");
  const provider = providerSummary(result, custodyIndex); const rows = Array.isArray(provider.transactionResults) ? provider.transactionResults as Json[] : [];
  const swapRow = rows[prefixCreates.length]; const swapPre = swapRow?.capturedTokenAmountPre; const swapPost = swapRow?.capturedTokenAmountPost;
  const simulatedSwapDelta = typeof swapPre === "string" && typeof swapPost === "string" ? BigInt(swapPost) - BigInt(swapPre) : null;
  if (frozenTemplate) invariant(
    simulatedSwapDelta !== null && simulatedSwapDelta >= depositAmountRaw,
    `${edgeKey} simulated custody delta ${simulatedSwapDelta?.toString() ?? "missing"} does not fund deposit ${depositAmountRaw.toString()}`,
  );
  const pass = http.ok && provider.summary === "succeeded" && rows.length === wires.length && rows.every((entry) => entry.err === null && entry.capturedPre === true && entry.capturedPost === true);
  writeFileSync(outputPath, `${JSON.stringify({ schema: "loyal-backyard-rwa-phase2-market-positive/v1", verdict: pass ? "PASS" : "REJECTED", broadcast: false, signedUnsent: true, cluster: "mainnet-beta", commitment: "confirmed", compiledArtifactSha256: sha256(compiledBytes), lane: laneKey, edge: edgeKey, frozenTemplate, freshHeader: { dataSha256: freshInstruction.dataSha256, lookupTables: freshHeader.lookupTables, quote: freshHeader.quote, ...(frozenTemplate ? { productionRequirement: "quote-bound output threshold is mandatory for production signing; this signed-unsent capability proof uses a threshold of one raw unit" } : {}) }, confirmedSourceBalanceRaw: sourceBalance.toString(), simulatedSwapCollateralDeltaRaw: simulatedSwapDelta?.toString() ?? null, depositAmountRaw: depositAmountRaw.toString(), policies: { swap: { seed: swap.policySeed, address: swap.policy, constraintIndex: swap.constraintIndex }, deposit: { seed: depositExecution.policySeed, address: depositExecution.policy } }, simulation: { method: "simulateBundle", skipSigVerify: false, contextSlot: provider.contextSlot ?? null, err: pass ? null : provider }, transactions: wires.map((entry) => ({ role: entry.role, signature: entry.signature, packetBytes: entry.wire.length, transactionBase64: Buffer.from(entry.wire).toString("base64"), transactionSha256: sha256(entry.wire) })), signatureAbsentOnChain: statusesBefore.value.every((entry) => entry === null) && statusesAfter.value.every((entry) => entry === null), chainPreStateSha256: stateSha256(before.value), chainPostStateSha256: stateSha256(after.value), confirmedReadbackSlot: after.context.slot, freshnessPrelude: { reserveAddresses: refresh.reserveAddresses }, provider }, null, 2)}\n`, { flag: "wx", mode: 0o600 }); console.log(JSON.stringify({ verdict: pass ? "PASS" : "REJECTED", lane: laneKey, output: outputPath, packets: wires.map((entry) => entry.wire.length) }));
}
main().catch((error) => { const rpcUrl = process.env.SOLANA_RPC_URL?.trim(); const message = error instanceof Error ? error.message : String(error); console.error(rpcUrl ? message.replaceAll(rpcUrl, "<rpc>") : message); process.exitCode = 1; });
