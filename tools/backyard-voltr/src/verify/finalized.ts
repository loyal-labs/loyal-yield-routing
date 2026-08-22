import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { findAssociatedTokenPda, getMintDecoder, getTokenDecoder } from "@solana-program/token";
import { AccountRole, address, createNoopSigner, isSignerRole, isWritableRole, type Instruction, type TransactionSigner } from "@solana/kit";
import { parseTransactionEvents } from "@voltr/vault-sdk";
import { Connection, type VersionedTransactionResponse } from "@solana/web3.js";

import { PARTNER_ROUTE, routeSpecSha256 } from "../domain/route-spec.js";
import { finalizedSnapshots, fromWeb3Instruction, loadDeploymentIdentities, loadMainReserveGraph } from "../integrations/solana-compat.js";
import { createVoltrRouteBuilder, deriveVoltrAccounts } from "../integrations/voltr.js";
import { loadRuntimePolicyArtifact } from "../policies/compiler.js";
import { verifyExistingRuntimePolicies } from "../policies/commands.js";
import { buildManagerWrapperForVerification } from "../runtime/manager.js";
import {
  verifyAdaptorReceipt,
  verifyDeploymentIdentities,
  verifyStrategyBootstrap,
  verifyVaultCurrentState,
  type Gate,
} from "./current.js";

type JsonRecord = Record<string, unknown>;
type Operation =
  | "initializeAndAdaptor"
  | "initializeStrategy"
  | "depositPolicy"
  | "withdrawPolicy"
  | "userDeposit"
  | "managerDeposit"
  | "managerWithdraw"
  | "withdrawRequest"
  | "withdrawClaim";
type TransactionEvidence = Readonly<{ signature: string; messageSha256: string }>;
type LifecycleManifest = Readonly<{
  routeId: string;
  routeSpecSha256: string;
  vault: string;
  settings: string;
  manager: string;
  guardian: string;
  user: string;
  assetMint: string;
  reserve: string;
  lendingMarket: string;
  collateralFarm: string;
  assetAmountRaw: bigint;
  lpAmountRaw: bigint;
  policyArtifactPath: string;
  policyArtifactFileSha256: string;
  transactions: Readonly<Record<Operation, TransactionEvidence>>;
  prematureClaim: Readonly<{ artifactPath: string; artifactFileSha256: string }>;
}>;
type LoadedTransaction = Readonly<{
  operation: Operation;
  evidence: TransactionEvidence;
  response: VersionedTransactionResponse | null;
  keys: readonly string[];
  programs: readonly string[];
  signers: readonly string[];
  messageSha256: string | null;
}>;

const OPERATIONS: readonly Operation[] = [
  "initializeAndAdaptor", "initializeStrategy", "depositPolicy", "withdrawPolicy",
  "userDeposit", "managerDeposit", "managerWithdraw", "withdrawRequest", "withdrawClaim",
];

function sha256(data: ArrayLike<number> | string): string {
  return createHash("sha256").update(typeof data === "string" ? data : Uint8Array.from(data)).digest("hex");
}

function record(value: unknown, label: string): JsonRecord {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`);
  return value as JsonRecord;
}

function exactKeys(value: JsonRecord, expected: readonly string[], label: string): void {
  if (Object.keys(value).sort().join("\0") !== [...expected].sort().join("\0")) throw new Error(`${label} keys are not exact`);
}

function stringField(value: JsonRecord, key: string): string {
  const result = value[key];
  if (typeof result !== "string" || result.length === 0) throw new Error(`${key} must be a non-empty string`);
  return result;
}

function shaField(value: JsonRecord, key: string): string {
  const result = stringField(value, key);
  if (!/^[0-9a-f]{64}$/.test(result)) throw new Error(`${key} must be lowercase SHA-256`);
  return result;
}

function bigintField(value: JsonRecord, key: string): bigint {
  const result = stringField(value, key);
  if (!/^[0-9]+$/.test(result)) throw new Error(`${key} must be an unsigned integer string`);
  return BigInt(result);
}

function txEvidence(value: unknown, label: string): TransactionEvidence {
  const item = record(value, label);
  exactKeys(item, ["signature", "messageSha256"], label);
  const signature = stringField(item, "signature");
  if (signature.length < 80 || signature.length > 90) throw new Error(`${label}.signature is malformed`);
  return { signature, messageSha256: shaField(item, "messageSha256") };
}

function parseManifest(path: string): LifecycleManifest {
  const root = record(JSON.parse(readFileSync(path, "utf8")), "lifecycle manifest");
  exactKeys(root, [
    "schemaVersion", "routeId", "routeSpecSha256", "vault", "settings", "manager", "guardian", "user",
    "assetMint", "reserve", "lendingMarket", "collateralFarm", "assetAmountRaw", "lpAmountRaw",
    "policyArtifactPath", "policyArtifactFileSha256", "transactions", "prematureClaim",
  ], "lifecycle manifest");
  if (root.schemaVersion !== 1) throw new Error("schemaVersion must be 1");
  const txs = record(root.transactions, "transactions");
  exactKeys(txs, OPERATIONS, "transactions");
  const transactions = Object.fromEntries(OPERATIONS.map((operation) => [operation, txEvidence(txs[operation], `transactions.${operation}`)])) as Record<Operation, TransactionEvidence>;
  const premature = record(root.prematureClaim, "prematureClaim");
  exactKeys(premature, ["artifactPath", "artifactFileSha256"], "prematureClaim");
  const assetAmountRaw = bigintField(root, "assetAmountRaw");
  const lpAmountRaw = bigintField(root, "lpAmountRaw");
  if (assetAmountRaw <= 0n || assetAmountRaw > PARTNER_ROUTE.asset.maxManagerOperationRaw) throw new Error("assetAmountRaw escapes manager policy bound");
  if (lpAmountRaw <= 0n) throw new Error("lpAmountRaw must be positive");
  return {
    routeId: stringField(root, "routeId"), routeSpecSha256: shaField(root, "routeSpecSha256"),
    vault: stringField(root, "vault"), settings: stringField(root, "settings"), manager: stringField(root, "manager"),
    guardian: stringField(root, "guardian"), user: stringField(root, "user"), assetMint: stringField(root, "assetMint"),
    reserve: stringField(root, "reserve"), lendingMarket: stringField(root, "lendingMarket"), collateralFarm: stringField(root, "collateralFarm"),
    assetAmountRaw, lpAmountRaw, policyArtifactPath: stringField(root, "policyArtifactPath"),
    policyArtifactFileSha256: shaField(root, "policyArtifactFileSha256"), transactions,
    prematureClaim: { artifactPath: stringField(premature, "artifactPath"), artifactFileSha256: shaField(premature, "artifactFileSha256") },
  };
}

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown): void {
  gates.push({ name, pass, observed, expected });
}

function childPath(manifestPath: string, child: string): string {
  return resolve(dirname(manifestPath), child);
}

async function loadTransaction(connection: Connection, operation: Operation, evidence: TransactionEvidence): Promise<LoadedTransaction> {
  const response = await connection.getTransaction(evidence.signature, { commitment: "finalized", maxSupportedTransactionVersion: 0 });
  if (!response) return { operation, evidence, response: null, keys: [], programs: [], signers: [], messageSha256: null };
  const keys = [...response.transaction.message.staticAccountKeys, ...(response.meta?.loadedAddresses?.writable ?? []), ...(response.meta?.loadedAddresses?.readonly ?? [])].map((key) => key.toBase58());
  return {
    operation, evidence, response, keys,
    programs: response.transaction.message.compiledInstructions.map((instruction) => keys[instruction.programIdIndex] ?? "<missing>"),
    signers: response.transaction.message.staticAccountKeys.slice(0, response.transaction.message.header.numRequiredSignatures).map((key) => key.toBase58()),
    messageSha256: sha256(response.transaction.message.serialize()),
  };
}

function tokenDelta(transaction: LoadedTransaction, account: string): bigint | null {
  const meta = transaction.response?.meta;
  if (!meta) return null;
  const pre = meta.preTokenBalances?.find((row) => transaction.keys[row.accountIndex] === account);
  const post = meta.postTokenBalances?.find((row) => transaction.keys[row.accountIndex] === account);
  return BigInt(post?.uiTokenAmount.amount ?? "0") - BigInt(pre?.uiTokenAmount.amount ?? "0");
}

function lamportDelta(transaction: LoadedTransaction, account: string): bigint | null {
  const meta = transaction.response?.meta;
  const index = transaction.keys.indexOf(account);
  if (!meta || index < 0 || index >= meta.preBalances.length || index >= meta.postBalances.length) return null;
  return BigInt(meta.postBalances[index]!) - BigInt(meta.preBalances[index]!);
}

function tokenRowsInside(transaction: LoadedTransaction, approved: ReadonlySet<string>): boolean {
  const meta = transaction.response?.meta;
  return !!meta && [...(meta.preTokenBalances ?? []), ...(meta.postTokenBalances ?? [])]
    .every((row) => approved.has(transaction.keys[row.accountIndex] ?? "<missing>"));
}

function compiledInstructionMatch(
  transaction: LoadedTransaction,
  actual: { programIdIndex: number; accountKeyIndexes: readonly number[]; data: Uint8Array } | null,
  expected: Instruction,
): Readonly<{ pass: boolean; observed: unknown; expected: unknown }> {
  const response = transaction.response;
  if (!response) return { pass: false, observed: null, expected: expected.programAddress };
  const message = response.transaction.message;
  const expectedAccounts = (expected.accounts ?? []).map((meta) => ({
    address: meta.address,
    signer: isSignerRole(meta.role),
    writable: isWritableRole(meta.role),
  }));
  const actualAccounts = actual?.accountKeyIndexes.map((index) => {
    const signer = index < message.header.numRequiredSignatures;
    const writable = signer
      ? index < message.header.numRequiredSignatures - message.header.numReadonlySignedAccounts
      : index < message.staticAccountKeys.length
        ? index < message.staticAccountKeys.length - message.header.numReadonlyUnsignedAccounts
        : index < message.staticAccountKeys.length + (response.meta?.loadedAddresses?.writable.length ?? 0);
    return { address: transaction.keys[index] ?? "<missing>", signer, writable };
  }) ?? null;
  const pass = actual !== null
    && actual.data.length === (expected.data?.length ?? 0)
    && Buffer.from(actual.data).equals(Buffer.from(expected.data ?? []))
    && JSON.stringify(actualAccounts) === JSON.stringify(expectedAccounts);
  return {
    pass,
    observed: actual ? { programId: transaction.keys[actual.programIdIndex], accounts: actualAccounts, dataSha256: sha256(actual.data) } : null,
    expected: { programId: expected.programAddress, accounts: expectedAccounts, dataSha256: sha256(expected.data ?? []) },
  };
}

function compiledInstructionExact(
  transaction: LoadedTransaction,
  expected: Instruction,
): Readonly<{ pass: boolean; observed: unknown; expected: unknown }> {
  const response = transaction.response;
  if (!response) return { pass: false, observed: null, expected: expected.programAddress };
  const matches = response.transaction.message.compiledInstructions.filter((instruction) => transaction.keys[instruction.programIdIndex] === expected.programAddress);
  return compiledInstructionMatch(transaction, matches.length === 1 ? matches[0]! : null, expected);
}

function compiledInstructionSequenceExact(
  transaction: LoadedTransaction,
  expected: readonly Instruction[],
): Readonly<{ pass: boolean; observed: unknown; expected: unknown }> {
  const actual = transaction.response?.transaction.message.compiledInstructions ?? [];
  // Solana compiles one global role per address. A key that is readonly in one
  // instruction is legitimately elevated when another instruction in the same
  // transaction needs it writable or signed. Reproduce that deterministic
  // union before comparing the compiled message; do not compare per-ix roles
  // directly to global message roles.
  const effectiveRoles = new Map<string, { signer: boolean; writable: boolean }>();
  for (const instruction of expected) {
    for (const meta of instruction.accounts ?? []) {
      const previous = effectiveRoles.get(meta.address) ?? { signer: false, writable: false };
      effectiveRoles.set(meta.address, {
        signer: previous.signer || isSignerRole(meta.role),
        writable: previous.writable || isWritableRole(meta.role),
      });
    }
  }
  const effectiveExpected = expected.map((instruction) => ({
    ...instruction,
    accounts: (instruction.accounts ?? []).map((meta) => {
      const role = effectiveRoles.get(meta.address)!;
      return {
        ...meta,
        role: role.signer
          ? role.writable ? AccountRole.WRITABLE_SIGNER : AccountRole.READONLY_SIGNER
          : role.writable ? AccountRole.WRITABLE : AccountRole.READONLY,
      };
    }),
  }));
  const results = effectiveExpected.map((instruction, index) => compiledInstructionMatch(transaction, actual[index] ?? null, instruction));
  return {
    pass: actual.length === expected.length && results.every((result) => result.pass),
    observed: results.map((result) => result.observed),
    expected: results.map((result) => result.expected),
  };
}

function effectiveManagerInstruction(instruction: Instruction): Instruction {
  return {
    ...instruction,
    accounts: (instruction.accounts ?? []).map((meta) => meta.address === PARTNER_ROUTE.squads.guardian
      ? { ...meta, role: AccountRole.WRITABLE_SIGNER }
      : meta),
  };
}

function effectiveFeePayerInstruction(instruction: Instruction, feePayer: string): Instruction {
  return {
    ...instruction,
    accounts: (instruction.accounts ?? []).map((meta) => meta.address === feePayer
      ? { ...meta, role: AccountRole.WRITABLE_SIGNER }
      : meta),
  };
}

function innerProgramIds(transaction: LoadedTransaction): readonly string[] {
  const inner = transaction.response?.meta?.innerInstructions ?? [];
  return inner.flatMap(({ instructions }) => instructions.flatMap((instruction) => {
    const programId = (instruction as { programId?: unknown }).programId;
    if (typeof programId === "string") return [programId];
    if (programId && typeof programId === "object" && "toBase58" in programId && typeof programId.toBase58 === "function") return [programId.toBase58()];
    if ("programIdIndex" in instruction) return [transaction.keys[instruction.programIdIndex] ?? "<missing>"];
    return [];
  }));
}

function tokenRowsValid(transaction: LoadedTransaction, approved: ReadonlySet<string>, allowedMints: ReadonlySet<string>): boolean {
  const meta = transaction.response?.meta;
  if (!meta) return false;
  const seen = new Set<string>();
  return [
    ...(meta.preTokenBalances ?? []).map((row) => ["pre", row] as const),
    ...(meta.postTokenBalances ?? []).map((row) => ["post", row] as const),
  ].every(([phase, row]) => {
    const account = transaction.keys[row.accountIndex] ?? "<missing>";
    const key = `${phase}:${row.accountIndex}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return approved.has(account) && allowedMints.has(row.mint);
  });
}

function compiledMessageWellFormed(transaction: LoadedTransaction): boolean {
  const message = transaction.response?.transaction.message;
  if (!message) return false;
  return message.compiledInstructions.every((instruction) =>
    instruction.programIdIndex >= 0
    && instruction.programIdIndex < transaction.keys.length
    && instruction.accountKeyIndexes.every((index) => index >= 0 && index < transaction.keys.length));
}

function expectedSigner(operation: Operation, manifest: LifecycleManifest): readonly string[] {
  // Voltr initializeVault requires both the setup payer/admin and the fresh
  // vault keypair. The vault later becomes a program-owned account, but it is
  // a real transaction signer for this one initialization.
  if (operation === "initializeAndAdaptor") return [PARTNER_ROUTE.setupAdmin, PARTNER_ROUTE.vault];
  if (operation === "managerDeposit" || operation === "managerWithdraw") return [manifest.guardian];
  if (operation === "userDeposit" || operation === "withdrawRequest" || operation === "withdrawClaim") return [manifest.user];
  return [PARTNER_ROUTE.setupAdmin];
}

function expectedPrograms(operation: Operation): readonly string[] {
  if (operation === "initializeAndAdaptor") return [PARTNER_ROUTE.programs.voltrVault, PARTNER_ROUTE.programs.voltrVault];
  if (operation === "initializeStrategy") return [PARTNER_ROUTE.programs.voltrVault, PARTNER_ROUTE.programs.voltrVault, PARTNER_ROUTE.programs.voltrVault];
  if (["depositPolicy", "withdrawPolicy"].includes(operation)) return [PARTNER_ROUTE.squads.program];
  if (["managerDeposit", "managerWithdraw"].includes(operation)) return ["ComputeBudget111111111111111111111111111111", "ComputeBudget111111111111111111111111111111", PARTNER_ROUTE.squads.program];
  if (operation === "userDeposit" || operation === "withdrawRequest") return [PARTNER_ROUTE.programs.associatedToken, PARTNER_ROUTE.programs.voltrVault];
  return [PARTNER_ROUTE.programs.voltrVault];
}

function routeAccountsPresent(transaction: LoadedTransaction, manifest: LifecycleManifest): boolean {
  const required = new Set<string>();
  if (!["depositPolicy", "withdrawPolicy"].includes(transaction.operation)) required.add(manifest.vault);
  // Initialize-strategy and request-withdraw do not take the asset mint. Their
  // full SDK-built account lists are compared below; keep this coarse route
  // membership gate aligned with the actual operation surface.
  if (["initializeAndAdaptor", "userDeposit", "managerDeposit", "managerWithdraw", "withdrawClaim"].includes(transaction.operation)) required.add(manifest.assetMint);
  if (["initializeAndAdaptor", "initializeStrategy"].includes(transaction.operation)) required.add(PARTNER_ROUTE.setupAdmin);
  if (["depositPolicy", "withdrawPolicy"].includes(transaction.operation)) required.add(manifest.settings);
  if (transaction.operation === "initializeStrategy") {
    // The atomic bootstrap temporarily installs setupAdmin as manager; the
    // permanent Squads manager is restored by the third canonical instruction.
    [manifest.reserve, manifest.lendingMarket, manifest.collateralFarm].forEach((value) => required.add(value));
  }
  if (["managerDeposit", "managerWithdraw"].includes(transaction.operation)) {
    [manifest.manager, manifest.guardian, manifest.reserve, manifest.lendingMarket, manifest.collateralFarm].forEach((value) => required.add(value));
  }
  if (["userDeposit", "withdrawRequest", "withdrawClaim"].includes(transaction.operation)) required.add(manifest.user);
  return [...required].every((value) => transaction.keys.includes(value));
}

function eventPayloads(transaction: LoadedTransaction, name: string): JsonRecord[] {
  return parseTransactionEvents({ logMessages: transaction.response?.meta?.logMessages ?? [] })
    .filter((event) => event.name === name)
    .map((event) => event.payload as unknown as JsonRecord);
}

function verifyPrematureArtifact(manifestPath: string, manifest: LifecycleManifest, request: LoadedTransaction, gates: Gate[]): void {
  const path = childPath(manifestPath, manifest.prematureClaim.artifactPath);
  let source: string;
  let root: JsonRecord;
  try {
    source = readFileSync(path, "utf8");
    root = record(JSON.parse(source), "premature claim artifact");
  } catch (error) {
    add(gates, "premature claim artifact readable", false, error instanceof Error ? error.message : String(error), path);
    return;
  }
  add(gates, "premature claim artifact hash", sha256(source) === manifest.prematureClaim.artifactFileSha256, sha256(source), manifest.prematureClaim.artifactFileSha256);
  add(gates, "premature claim is simulation-only PASS", root.routeSpecSha256 === routeSpecSha256() && root.verdict === "PARTNER_WITHDRAW_CLAIM_PREMATURE_REJECTION_PASS" && root.broadcast === false && root.readyForBroadcast === false, { routeSpecSha256: root.routeSpecSha256, verdict: root.verdict, broadcast: root.broadcast, readyForBroadcast: root.readyForBroadcast }, { routeSpecSha256: routeSpecSha256(), verdict: "PARTNER_WITHDRAW_CLAIM_PREMATURE_REJECTION_PASS", broadcast: false, readyForBroadcast: false });
  const tx = record(root.transaction, "premature transaction");
  add(gates, "premature claim links finalized request", tx.requestSignature === request.evidence.signature, tx.requestSignature, request.evidence.signature);
  const artifactGates = Array.isArray(root.gates) ? root.gates.map((gate) => record(gate, "premature gate")) : [];
  const namedPass = (fragment: string) => artifactGates.some((gate) => typeof gate.name === "string" && gate.name.includes(fragment) && gate.pass === true);
  add(gates, "premature claim exact 6012", namedPass("Custom 6012"), artifactGates.map(({ name, pass }) => ({ name, pass })), "passing Custom 6012 gate");
  add(gates, "premature claim no mutation", namedPass("no finalized protected-account mutation"), artifactGates.map(({ name, pass }) => ({ name, pass })), "passing no-mutation gate");
  add(gates, "premature claim exact 600-second origin", namedPass("request origin is exact 600-second"), artifactGates.map(({ name, pass }) => ({ name, pass })), "passing request-origin gate");
}

async function addCurrentStateGates(rpcUrl: string, gates: Gate[]): Promise<number> {
  const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
  const reserve = await loadMainReserveGraph(rpcUrl, PARTNER_ROUTE, accounts.strategyAuth);
  const state = await finalizedSnapshots(rpcUrl, [PARTNER_ROUTE.vault, accounts.lpMint, accounts.idleAta, PARTNER_ROUTE.asset.mint, accounts.adaptorAddReceipt, accounts.strategyInitReceipt, reserve.graph.userMetadata, reserve.graph.obligation, reserve.graph.obligationFarm], reserve.contextSlot);
  const current = verifyVaultCurrentState({ route: PARTNER_ROUTE, accounts, vault: state.accounts[0] ?? null, lpMint: state.accounts[1] ?? null, idleAta: state.accounts[2] ?? null, assetMint: state.accounts[3] ?? null, requireIdleOnly: true });
  gates.push(...current.gates.map((gate) => ({ ...gate, name: `current vault: ${gate.name}` })));
  gates.push(...verifyAdaptorReceipt(PARTNER_ROUTE, accounts.adaptorAddReceipt, state.accounts[4] ?? null).map((gate) => ({ ...gate, name: `current adaptor: ${gate.name}` })));
  gates.push(...verifyStrategyBootstrap({ route: PARTNER_ROUTE, accounts, graph: reserve.graph, strategyReceipt: state.accounts[5] ?? null, userMetadata: state.accounts[6] ?? null, obligation: state.accounts[7] ?? null, obligationFarm: state.accounts[8] ?? null }).map((gate) => ({ ...gate, name: `current strategy: ${gate.name}` })));
  const deployments = await loadDeploymentIdentities(rpcUrl, PARTNER_ROUTE, state.contextSlot);
  gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities).map((gate) => ({ ...gate, name: `current deployment: ${gate.name}` })));
  return Math.max(state.contextSlot, deployments.contextSlot);
}

export async function verifyFinalizedLifecycle(evidencePath: string) {
  const path = resolve(evidencePath);
  const gates: Gate[] = [];
  let manifest: LifecycleManifest;
  try {
    manifest = parseManifest(path);
  } catch (error) {
    add(gates, "lifecycle manifest parses exactly", false, error instanceof Error ? error.message : String(error), "schemaVersion 1 exact manifest");
    return { verdict: "PARTNER_LIFECYCLE_FAIL", broadcast: false, evidencePath: path, failedGateCount: 1, gates } as const;
  }
  const expectedRoute = { routeId: PARTNER_ROUTE.id, routeSpecSha256: routeSpecSha256(), vault: PARTNER_ROUTE.vault, settings: PARTNER_ROUTE.squads.settings, manager: PARTNER_ROUTE.squads.manager, guardian: PARTNER_ROUTE.squads.guardian, assetMint: PARTNER_ROUTE.asset.mint, reserve: PARTNER_ROUTE.strategy.reserve, lendingMarket: PARTNER_ROUTE.strategy.lendingMarket, collateralFarm: PARTNER_ROUTE.strategy.collateralFarm };
  const observedRoute = { routeId: manifest.routeId, routeSpecSha256: manifest.routeSpecSha256, vault: manifest.vault, settings: manifest.settings, manager: manifest.manager, guardian: manifest.guardian, assetMint: manifest.assetMint, reserve: manifest.reserve, lendingMarket: manifest.lendingMarket, collateralFarm: manifest.collateralFarm };
  add(gates, "manifest exact 600-second RouteSpec binding", JSON.stringify(observedRoute) === JSON.stringify(expectedRoute) && PARTNER_ROUTE.vaultConfiguration.withdrawalWaitingPeriodSeconds === 600n, observedRoute, expectedRoute);
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) {
    add(gates, "finalized RPC configured", false, null, "SOLANA_RPC_URL");
    return { verdict: "PARTNER_LIFECYCLE_FAIL", broadcast: false, evidencePath: path, routeSpecSha256: routeSpecSha256(), failedGateCount: gates.filter(({ pass }) => !pass).length, gates } as const;
  }
  const connection = new Connection(rpcUrl, "finalized");
  const genesis = await connection.getGenesisHash();
  add(gates, "RPC is mainnet-beta", genesis === PARTNER_ROUTE.genesisHash, genesis, PARTNER_ROUTE.genesisHash);
  const loaded = await Promise.all(OPERATIONS.map((operation) => loadTransaction(connection, operation, manifest.transactions[operation])));
  const byOperation = new Map(loaded.map((tx) => [tx.operation, tx]));
  for (const tx of loaded) {
    add(gates, `${tx.operation} finalized success`, tx.response !== null && tx.response.meta?.err === null, tx.response ? { slot: tx.response.slot, err: tx.response.meta?.err ?? null } : null, { finalized: true, err: null });
    add(gates, `${tx.operation} message hash exact`, tx.messageSha256 === tx.evidence.messageSha256, tx.messageSha256, tx.evidence.messageSha256);
    add(gates, `${tx.operation} compiled account indexes resolve exactly`, compiledMessageWellFormed(tx), tx.keys.length, "all program/account indexes resolve");
    add(gates, `${tx.operation} signer set exact`, tx.signers.join("\0") === expectedSigner(tx.operation, manifest).join("\0"), tx.signers, expectedSigner(tx.operation, manifest));
    add(gates, `${tx.operation} top-level programs exact`, tx.programs.join("\0") === expectedPrograms(tx.operation).join("\0"), tx.programs, expectedPrograms(tx.operation));
    add(gates, `${tx.operation} route accounts present`, routeAccountsPresent(tx, manifest), tx.keys, "required route accounts");
    add(gates, `${tx.operation} fee is positive and bounded`, (tx.response?.meta?.fee ?? 0) > 0 && (tx.response?.meta?.fee ?? 0) <= 100_000, tx.response?.meta?.fee ?? null, "1..100000 lamports");
  }
  const slots = loaded.map((tx) => tx.response?.slot ?? 0);
  add(gates, "lifecycle signatures unique and ordered", new Set(loaded.map(({ evidence }) => evidence.signature)).size === loaded.length && slots.every((slot, index) => index === 0 || slot >= slots[index - 1]!), slots, "unique nondecreasing slots");
  const latestPinnedDeploymentSlot = PARTNER_ROUTE.deployments.reduce((latest, deployment) => deployment.deployedSlot > latest ? deployment.deployedSlot : latest, 0n);
  add(gates, "every lifecycle transaction post-dates pinned deployments", loaded.every((tx) => tx.response !== null && BigInt(tx.response.slot) > latestPinnedDeploymentSlot), { minimumTransactionSlot: slots.length > 0 ? Math.min(...slots) : 0, latestPinnedDeploymentSlot }, `every tx slot > ${latestPinnedDeploymentSlot}`);

  const accounts = await deriveVoltrAccounts(PARTNER_ROUTE);
  const reserve = await loadMainReserveGraph(rpcUrl, PARTNER_ROUTE, accounts.strategyAuth);
  const builder = await createVoltrRouteBuilder(PARTNER_ROUTE, reserve.graph);
  const policyPath = childPath(path, manifest.policyArtifactPath);
  const userAccounts = await builder.userAccounts(address(manifest.user));
  const historicalUserSigner = { address: address(manifest.user) } as unknown as TransactionSigner;
  const canonicalUserDeposit = await builder.user.deposit({ user: historicalUserSigner }, manifest.assetAmountRaw);
  const canonicalWithdrawRequest = await builder.user.requestWithdraw({ user: historicalUserSigner, payer: historicalUserSigner }, manifest.lpAmountRaw, true);
  const canonicalWithdrawClaim = await builder.user.claimWithdraw(historicalUserSigner);
  const historicalAdmin = createNoopSigner(address(PARTNER_ROUTE.setupAdmin));
  const historicalVault = createNoopSigner(address(PARTNER_ROUTE.vault));
  const setupSigners = { payer: historicalAdmin, admin: historicalAdmin, vault: historicalVault };
  const canonicalInitializeVault = await builder.setup.initializeVault(setupSigners);
  const canonicalAddAdaptor = await builder.setup.addAdaptor(setupSigners);
  const canonicalSetManager = await builder.setup.setManagerToAdmin(setupSigners);
  const canonicalInitializeStrategy = await builder.setup.initializeStrategyAsAdmin(setupSigners);
  const canonicalRestoreManager = await builder.setup.restoreManager(setupSigners);
  const historicalManager = createNoopSigner(address(manifest.manager));
  const canonicalManagerDeposit = await builder.strategy.deposit(historicalManager, manifest.assetAmountRaw);
  const canonicalManagerWithdraw = await builder.strategy.withdraw(historicalManager, manifest.assetAmountRaw);
  let policyArtifact: Awaited<ReturnType<typeof loadRuntimePolicyArtifact>> | null = null;
  try {
    policyArtifact = loadRuntimePolicyArtifact(policyPath);
  } catch (error) {
    add(gates, "runtime policy artifact loads for canonical manager verification", false, error instanceof Error ? error.message : String(error), policyPath);
  }
  const [strategyAssetAta] = await findAssociatedTokenPda({
    owner: accounts.strategyAuth,
    mint: PARTNER_ROUTE.asset.mint,
    tokenProgram: PARTNER_ROUTE.programs.token,
  }, { programAddress: PARTNER_ROUTE.programs.associatedToken });
  const approvedTokenAccounts = new Set([
    userAccounts.userAssetAta,
    userAccounts.userLpAta,
    userAccounts.requestWithdrawLpAta,
    accounts.idleAta,
    strategyAssetAta,
    reserve.graph.reserveLiquiditySupply,
    reserve.graph.reserveCollateralSupplyVault,
  ]);
  const userOperationMints = new Set<string>([manifest.assetMint, accounts.lpMint]);
  const managerOperationMints = new Set<string>([manifest.assetMint, accounts.lpMint, reserve.graph.reserveCollateralMint]);
  const deposit = byOperation.get("userDeposit")!;
  const managerDeposit = byOperation.get("managerDeposit")!;
  const managerWithdraw = byOperation.get("managerWithdraw")!;
  const request = byOperation.get("withdrawRequest")!;
  const claim = byOperation.get("withdrawClaim")!;
  const initializeAndAdaptor = byOperation.get("initializeAndAdaptor")!;
  const initializeStrategy = byOperation.get("initializeStrategy")!;
  const initializeSequence = compiledInstructionSequenceExact(initializeAndAdaptor, [canonicalInitializeVault.raw, canonicalAddAdaptor.raw]);
  const strategySequence = compiledInstructionSequenceExact(initializeStrategy, [canonicalSetManager.raw, canonicalInitializeStrategy.raw, canonicalRestoreManager.raw]);
  add(gates, "initialize-and-adaptor canonical SDK instruction sequence exact", initializeSequence.pass, initializeSequence.observed, initializeSequence.expected);
  add(gates, "initialize-strategy canonical SDK instruction sequence exact", strategySequence.pass, strategySequence.observed, strategySequence.expected);
  add(gates, "user deposit USDC exact", tokenDelta(deposit, userAccounts.userAssetAta) === -manifest.assetAmountRaw && tokenDelta(deposit, accounts.idleAta) === manifest.assetAmountRaw, { source: tokenDelta(deposit, userAccounts.userAssetAta), idle: tokenDelta(deposit, accounts.idleAta) }, { source: -manifest.assetAmountRaw, idle: manifest.assetAmountRaw });
  const lpDeposit = tokenDelta(deposit, userAccounts.userLpAta);
  add(gates, "user deposit LP equals lifecycle amount", lpDeposit === manifest.lpAmountRaw, lpDeposit, manifest.lpAmountRaw);
  const depositInstruction = compiledInstructionExact(deposit, effectiveFeePayerInstruction(canonicalUserDeposit.raw, manifest.user));
  add(gates, "user deposit canonical instruction accounts and data exact", depositInstruction.pass, depositInstruction.observed, depositInstruction.expected);
  add(gates, "user deposit token rows bounded and mint-valid", tokenRowsInside(deposit, approvedTokenAccounts) && tokenRowsValid(deposit, approvedTokenAccounts, userOperationMints), deposit.response?.meta?.preTokenBalances ?? [], [...approvedTokenAccounts]);
  const depositEvents = eventPayloads(deposit, "DepositVaultEvent");
  const depositEvent = depositEvents.length === 1 ? depositEvents[0]! : null;
  const depositValueDelta = depositEvent
    && typeof depositEvent.vaultAssetTotalValueBefore === "bigint"
    && typeof depositEvent.vaultAssetTotalValueAfter === "bigint"
    ? depositEvent.vaultAssetTotalValueAfter - depositEvent.vaultAssetTotalValueBefore
    : null;
  const depositSupplyDelta = depositEvent
    && typeof depositEvent.vaultLpSupplyInclFeesBefore === "bigint"
    && typeof depositEvent.vaultLpSupplyInclFeesAfter === "bigint"
    ? depositEvent.vaultLpSupplyInclFeesAfter - depositEvent.vaultLpSupplyInclFeesBefore
    : null;
  const depositDeadWeightDelta = depositEvent
    && typeof depositEvent.vaultLpDeadWeightBefore === "bigint"
    && typeof depositEvent.vaultLpDeadWeightAfter === "bigint"
    ? depositEvent.vaultLpDeadWeightAfter - depositEvent.vaultLpDeadWeightBefore
    : null;
  add(gates, "user deposit event route and amounts exact", depositEvent !== null
    && depositEvent.user === manifest.user
    && depositEvent.vault === manifest.vault
    && depositEvent.vaultAssetMint === manifest.assetMint
    && depositEvent.userAmountAssetDeposited === manifest.assetAmountRaw
    && depositEvent.userAmountLpMinted === manifest.lpAmountRaw,
  depositEvent, { user: manifest.user, vault: manifest.vault, vaultAssetMint: manifest.assetMint, userAmountAssetDeposited: manifest.assetAmountRaw, userAmountLpMinted: manifest.lpAmountRaw });
  add(gates, "user deposit event vault value delta exact", depositValueDelta === manifest.assetAmountRaw, depositValueDelta, manifest.assetAmountRaw);
  add(gates, "user deposit event LP economics exact", depositSupplyDelta !== null
    && depositDeadWeightDelta !== null
    && depositSupplyDelta === manifest.lpAmountRaw + depositDeadWeightDelta
    && depositEvent?.vaultLpTotalAccumulatedFeesAfter === depositEvent?.vaultLpTotalAccumulatedFeesBefore,
  { depositSupplyDelta, depositDeadWeightDelta, accumulatedFeesBefore: depositEvent?.vaultLpTotalAccumulatedFeesBefore ?? null, accumulatedFeesAfter: depositEvent?.vaultLpTotalAccumulatedFeesAfter ?? null },
  { depositSupplyDelta: "userAmountLpMinted + deadWeightDelta", accumulatedFees: "unchanged (zero-fee RouteSpec)" });
  add(gates, "manager deposit idle exact", tokenDelta(managerDeposit, accounts.idleAta) === -manifest.assetAmountRaw, tokenDelta(managerDeposit, accounts.idleAta), -manifest.assetAmountRaw);
  const managerReturned = tokenDelta(managerWithdraw, accounts.idleAta);
  add(gates, "manager withdraw idle within one-raw-unit floor", managerReturned !== null && managerReturned >= manifest.assetAmountRaw - 1n && managerReturned <= manifest.assetAmountRaw, managerReturned, `${manifest.assetAmountRaw - 1n}..${manifest.assetAmountRaw}`);
  const managerDepositReserveLiquidity = tokenDelta(managerDeposit, reserve.graph.reserveLiquiditySupply);
  const managerWithdrawReserveLiquidity = tokenDelta(managerWithdraw, reserve.graph.reserveLiquiditySupply);
  const managerDepositCollateral = tokenDelta(managerDeposit, reserve.graph.reserveCollateralSupplyVault);
  const managerWithdrawCollateral = tokenDelta(managerWithdraw, reserve.graph.reserveCollateralSupplyVault);
  add(gates, "manager deposit reaches exact Main reserve liquidity supply", managerDepositReserveLiquidity === manifest.assetAmountRaw, managerDepositReserveLiquidity, manifest.assetAmountRaw);
  add(gates, "manager withdraw returns same bounded Main reserve liquidity", managerReturned !== null && managerWithdrawReserveLiquidity === -managerReturned, managerWithdrawReserveLiquidity, managerReturned === null ? null : -managerReturned);
  add(gates, "manager Main collateral position opens and unwinds", managerDepositCollateral !== null
    && managerDepositCollateral > 0n
    && managerWithdrawCollateral !== null
    && managerWithdrawCollateral < 0n,
  { depositCollateralDelta: managerDepositCollateral, withdrawCollateralDelta: managerWithdrawCollateral },
  { depositCollateralDelta: ">0", withdrawCollateralDelta: "<0" });
  add(gates, "manager transient strategy USDC ATA nets zero", tokenDelta(managerDeposit, strategyAssetAta) === 0n && tokenDelta(managerWithdraw, strategyAssetAta) === 0n, { deposit: tokenDelta(managerDeposit, strategyAssetAta), withdraw: tokenDelta(managerWithdraw, strategyAssetAta) }, { deposit: 0n, withdraw: 0n });
  if (policyArtifact) {
    try {
      const depositEntry = policyArtifact.artifact.policies.find((entry) => entry.operation === "deposit");
      const withdrawEntry = policyArtifact.artifact.policies.find((entry) => entry.operation === "withdraw");
      if (!depositEntry || !withdrawEntry) throw new Error("runtime policy artifact is missing deposit or withdraw entry");
      const depositWrapper = buildManagerWrapperForVerification("deposit", depositEntry, canonicalManagerDeposit.canonical, manifest.assetAmountRaw);
      const withdrawWrapper = buildManagerWrapperForVerification("withdraw", withdrawEntry, canonicalManagerWithdraw.canonical, manifest.assetAmountRaw);
      const depositInstruction = compiledInstructionExact(managerDeposit, effectiveManagerInstruction(fromWeb3Instruction(depositWrapper.instruction)));
      const withdrawInstruction = compiledInstructionExact(managerWithdraw, effectiveManagerInstruction(fromWeb3Instruction(withdrawWrapper.instruction)));
      add(gates, "manager deposit canonical wrapper accounts and data exact", depositInstruction.pass, depositInstruction.observed, depositInstruction.expected);
      add(gates, "manager withdraw canonical wrapper accounts and data exact", withdrawInstruction.pass, withdrawInstruction.observed, withdrawInstruction.expected);
    } catch (error) {
      add(gates, "manager canonical wrapper reconstruction succeeds", false, error instanceof Error ? error.message : String(error), "exact SDK wrapper");
    }
  }
  const requiredInnerPrograms: readonly string[] = [PARTNER_ROUTE.programs.voltrVault, PARTNER_ROUTE.programs.kaminoAdaptor, PARTNER_ROUTE.programs.klend, PARTNER_ROUTE.programs.farms, PARTNER_ROUTE.programs.token];
  const allowedInnerPrograms = new Set<string>([...requiredInnerPrograms, PARTNER_ROUTE.programs.system, PARTNER_ROUTE.programs.associatedToken, PARTNER_ROUTE.squads.program]);
  const managerDepositInnerPrograms = innerProgramIds(managerDeposit);
  const managerWithdrawInnerPrograms = innerProgramIds(managerWithdraw);
  add(gates, "manager deposit inner Main route programs exact", requiredInnerPrograms.every((program) => managerDepositInnerPrograms.includes(program)) && managerDepositInnerPrograms.every((program) => allowedInnerPrograms.has(program)), managerDepositInnerPrograms, requiredInnerPrograms);
  add(gates, "manager withdraw inner Main route programs exact", requiredInnerPrograms.every((program) => managerWithdrawInnerPrograms.includes(program)) && managerWithdrawInnerPrograms.every((program) => allowedInnerPrograms.has(program)), managerWithdrawInnerPrograms, requiredInnerPrograms);
  add(gates, "manager token rows bounded and mint-valid", tokenRowsInside(managerDeposit, approvedTokenAccounts) && tokenRowsInside(managerWithdraw, approvedTokenAccounts) && tokenRowsValid(managerDeposit, approvedTokenAccounts, managerOperationMints) && tokenRowsValid(managerWithdraw, approvedTokenAccounts, managerOperationMints), { deposit: managerDeposit.response?.meta?.preTokenBalances ?? [], withdraw: managerWithdraw.response?.meta?.preTokenBalances ?? [] }, [...approvedTokenAccounts]);
  add(gates, "withdraw request LP escrow exact", tokenDelta(request, userAccounts.userLpAta) === -manifest.lpAmountRaw && tokenDelta(request, userAccounts.requestWithdrawLpAta) === manifest.lpAmountRaw, { userLp: tokenDelta(request, userAccounts.userLpAta), escrow: tokenDelta(request, userAccounts.requestWithdrawLpAta) }, { userLp: -manifest.lpAmountRaw, escrow: manifest.lpAmountRaw });
  const requestInstruction = compiledInstructionExact(request, effectiveFeePayerInstruction(canonicalWithdrawRequest.raw, manifest.user));
  add(gates, "withdraw request canonical instruction accounts and data exact", requestInstruction.pass, requestInstruction.observed, requestInstruction.expected);
  const depositPayerDelta = lamportDelta(deposit, manifest.user);
  const depositLpRent = lamportDelta(deposit, userAccounts.userLpAta) ?? 0n;
  add(gates, "user deposit SOL is fee plus LP ATA rent", depositPayerDelta !== null && depositPayerDelta === -(BigInt(deposit.response?.meta?.fee ?? 0) + (depositLpRent > 0n ? depositLpRent : 0n)), depositPayerDelta, -(BigInt(deposit.response?.meta?.fee ?? 0) + (depositLpRent > 0n ? depositLpRent : 0n)));
  const requestPayerDelta = lamportDelta(request, manifest.user);
  const requestReceiptDelta = lamportDelta(request, userAccounts.requestWithdrawVaultReceipt) ?? 0n;
  const requestEscrowDelta = lamportDelta(request, userAccounts.requestWithdrawLpAta) ?? 0n;
  add(gates, "withdraw request SOL is fee plus receipt and escrow rent", requestPayerDelta !== null && requestReceiptDelta > 0n && requestEscrowDelta > 0n && requestPayerDelta === -(BigInt(request.response?.meta?.fee ?? 0) + requestReceiptDelta + requestEscrowDelta), { payer: requestPayerDelta, receipt: requestReceiptDelta, escrow: requestEscrowDelta }, { payer: -(BigInt(request.response?.meta?.fee ?? 0) + requestReceiptDelta + requestEscrowDelta), receipt: ">0", escrow: ">0" });
  const requestEvents = eventPayloads(request, "RequestWithdrawVaultEvent");
  const requestEvent = requestEvents.length === 1 ? requestEvents[0]! : null;
  const requestedTs = requestEvent && typeof requestEvent.requestedTs === "bigint" ? requestEvent.requestedTs : null;
  const deadline = requestEvent && typeof requestEvent.withdrawableFromTs === "bigint" ? requestEvent.withdrawableFromTs : null;
  const quotedAssetRaw = requestEvent && typeof requestEvent.amountAssetToWithdrawDecimalBits === "bigint"
    ? requestEvent.amountAssetToWithdrawDecimalBits >> 48n
    : null;
  const requestUnlockedValue = requestEvent && typeof requestEvent.vaultAssetTotalValueUnlocked === "bigint"
    ? requestEvent.vaultAssetTotalValueUnlocked
    : null;
  add(gates, "withdraw request event exact route, receipt, and flags", requestEvent !== null
    && requestEvent.vault === manifest.vault
    && requestEvent.user === manifest.user
    && requestEvent.vaultAssetMint === manifest.assetMint
    && requestEvent.requestWithdrawVaultReceipt === userAccounts.requestWithdrawVaultReceipt
    && requestEvent.requestedAmount === manifest.lpAmountRaw
    && requestEvent.isAmountInLp === true
    && requestEvent.isWithdrawAll === true
    && requestEvent.amountLpEscrowed === manifest.lpAmountRaw,
  requestEvent, { vault: manifest.vault, user: manifest.user, vaultAssetMint: manifest.assetMint, requestWithdrawVaultReceipt: userAccounts.requestWithdrawVaultReceipt, requestedAmount: manifest.lpAmountRaw, isAmountInLp: true, isWithdrawAll: true, amountLpEscrowed: manifest.lpAmountRaw });
  add(gates, "withdraw request event exact 600 seconds", requestedTs !== null && deadline === requestedTs + 600n, { requestedTs, deadline }, { waitingPeriodSeconds: 600n });
  add(gates, "withdraw request quote is positive and bounded", quotedAssetRaw !== null && requestUnlockedValue !== null && quotedAssetRaw > 0n && quotedAssetRaw <= requestUnlockedValue, { quotedAssetRaw, vaultAssetTotalValueUnlocked: requestUnlockedValue }, "0 < quote <= unlocked value");
  const claimEvents = eventPayloads(claim, "WithdrawVaultEvent");
  const claimEvent = claimEvents.length === 1 ? claimEvents[0]! : null;
  const claimTime = claim.response ? await connection.getBlockTime(claim.response.slot) : null;
  add(gates, "claim at or after deadline", deadline !== null && claimTime !== null && BigInt(claimTime) >= deadline, { claimTime, deadline }, "claimTime >= deadline");
  add(gates, "claim event route and LP exact", claimEvent !== null
    && claimEvent.vault === manifest.vault
    && claimEvent.user === manifest.user
    && claimEvent.vaultAssetMint === manifest.assetMint
    && claimEvent.userAmountLpBurned === manifest.lpAmountRaw,
  claimEvent, { vault: manifest.vault, user: manifest.user, vaultAssetMint: manifest.assetMint, userAmountLpBurned: manifest.lpAmountRaw });
  const claimInstruction = compiledInstructionExact(claim, effectiveFeePayerInstruction(canonicalWithdrawClaim.raw, manifest.user));
  add(gates, "withdraw claim canonical instruction accounts and data exact", claimInstruction.pass, claimInstruction.observed, claimInstruction.expected);
  const userPayout = tokenDelta(claim, userAccounts.userAssetAta);
  const idlePayout = tokenDelta(claim, accounts.idleAta);
  add(gates, "claim USDC payout exact", quotedAssetRaw !== null && userPayout === quotedAssetRaw && idlePayout === -quotedAssetRaw && claimEvent?.userAmountAssetWithdrawn === quotedAssetRaw, { quote: quotedAssetRaw, user: userPayout, idle: idlePayout, event: claimEvent?.userAmountAssetWithdrawn ?? null }, { user: quotedAssetRaw, idle: quotedAssetRaw === null ? null : -quotedAssetRaw, event: quotedAssetRaw });
  const claimValueDelta = claimEvent
    && typeof claimEvent.vaultAssetTotalValueBefore === "bigint"
    && typeof claimEvent.vaultAssetTotalValueAfter === "bigint"
    ? claimEvent.vaultAssetTotalValueBefore - claimEvent.vaultAssetTotalValueAfter
    : null;
  const claimSupplyDelta = claimEvent
    && typeof claimEvent.vaultLpSupplyInclFeesBefore === "bigint"
    && typeof claimEvent.vaultLpSupplyInclFeesAfter === "bigint"
    ? claimEvent.vaultLpSupplyInclFeesBefore - claimEvent.vaultLpSupplyInclFeesAfter
    : null;
  add(gates, "claim event vault accounting exact", quotedAssetRaw !== null
    && claimValueDelta === quotedAssetRaw
    && claimSupplyDelta === manifest.lpAmountRaw
    && claimEvent?.vaultLpTotalAccumulatedFeesAfter === claimEvent?.vaultLpTotalAccumulatedFeesBefore
    && claimEvent?.vaultLpDeadWeightAfter === claimEvent?.vaultLpDeadWeightBefore,
  { claimValueDelta, claimSupplyDelta, accumulatedFeesBefore: claimEvent?.vaultLpTotalAccumulatedFeesBefore ?? null, accumulatedFeesAfter: claimEvent?.vaultLpTotalAccumulatedFeesAfter ?? null, deadWeightBefore: claimEvent?.vaultLpDeadWeightBefore ?? null, deadWeightAfter: claimEvent?.vaultLpDeadWeightAfter ?? null },
  { claimValueDelta: quotedAssetRaw, claimSupplyDelta: manifest.lpAmountRaw, accumulatedFees: "unchanged", deadWeight: "unchanged" });
  add(gates, "claim event timestamp is after request deadline", deadline !== null && typeof claimEvent?.withdrawnTs === "bigint" && claimEvent.withdrawnTs >= deadline, { withdrawnTs: claimEvent?.withdrawnTs ?? null, deadline }, "withdrawnTs >= deadline");
  add(gates, "claim user LP account unchanged", tokenDelta(claim, userAccounts.userLpAta) === 0n, tokenDelta(claim, userAccounts.userLpAta), 0n);
  add(gates, "claim escrow burns exact LP", tokenDelta(claim, userAccounts.requestWithdrawLpAta) === -manifest.lpAmountRaw, tokenDelta(claim, userAccounts.requestWithdrawLpAta), -manifest.lpAmountRaw);
  add(gates, "claim token rows bounded and mint-valid", tokenRowsInside(claim, approvedTokenAccounts) && tokenRowsValid(claim, approvedTokenAccounts, userOperationMints), claim.response?.meta?.preTokenBalances ?? [], [...approvedTokenAccounts]);
  const claimPayerDelta = lamportDelta(claim, manifest.user);
  const claimReceiptDelta = lamportDelta(claim, userAccounts.requestWithdrawVaultReceipt) ?? 0n;
  const claimEscrowDelta = lamportDelta(claim, userAccounts.requestWithdrawLpAta) ?? 0n;
  add(gates, "claim SOL is fee net receipt refund with escrow rent retained", claimPayerDelta !== null && claimReceiptDelta < 0n && claimEscrowDelta === 0n && claimPayerDelta === -(BigInt(claim.response?.meta?.fee ?? 0) + claimReceiptDelta), { payer: claimPayerDelta, receipt: claimReceiptDelta, escrow: claimEscrowDelta }, { payer: -(BigInt(claim.response?.meta?.fee ?? 0) + claimReceiptDelta), receipt: "<0", escrow: "0" });
  verifyPrematureArtifact(path, manifest, request, gates);

  let policyHash: string | null = null;
  try { policyHash = sha256(readFileSync(policyPath)); } catch { /* hash gate fails */ }
  add(gates, "runtime policy artifact hash", policyHash === manifest.policyArtifactFileSha256, policyHash, manifest.policyArtifactFileSha256);
  let verifiedPolicies: Awaited<ReturnType<typeof verifyExistingRuntimePolicies>> | null = null;
  try {
    verifiedPolicies = await verifyExistingRuntimePolicies(policyPath);
    add(gates, "exact two runtime policies finalized", verifiedPolicies.verdict === "PARTNER_RUNTIME_POLICIES_FINALIZED_PASS" && verifiedPolicies.failedGateCount === 0, { verdict: verifiedPolicies.verdict, failedGateCount: verifiedPolicies.failedGateCount }, { verdict: "PARTNER_RUNTIME_POLICIES_FINALIZED_PASS", failedGateCount: 0 });
    const policyOrigins = new Map(verifiedPolicies.policies.map((policy) => [policy.operation, policy.origin]));
    for (const [operation, policyOperation] of [["depositPolicy", "deposit"], ["withdrawPolicy", "withdraw"]] as const) {
      const origin = policyOrigins.get(policyOperation);
      add(gates, `${operation} manifest signature binds exact policy-create origin`, origin?.signature === manifest.transactions[operation].signature, { manifest: manifest.transactions[operation].signature, origin: origin?.signature ?? null }, "same finalized signature");
    }
  } catch (error) {
    add(gates, "exact two runtime policies finalized", false, error instanceof Error ? error.message : String(error), "two exact finalized policies");
  }
  let currentContextSlot = 0;
  try { currentContextSlot = await addCurrentStateGates(rpcUrl, gates); }
  catch (error) { add(gates, "current end state readable", false, error instanceof Error ? error.message : String(error), "readable exact route"); }
  const finalSlot = claim.response?.slot ?? 0;
  add(gates, "current reconciliation after claim", currentContextSlot >= finalSlot && finalSlot > 0, { currentContextSlot, finalSlot }, "current >= claim > 0");
  const final = await finalizedSnapshots(rpcUrl, [userAccounts.requestWithdrawVaultReceipt, userAccounts.requestWithdrawLpAta, accounts.lpMint], finalSlot || undefined);
  add(gates, "final account readback is anchored at or after claim", final.contextSlot >= finalSlot && finalSlot > 0, final.contextSlot, `>= ${finalSlot}`);
  let lpSupply: bigint | null = null;
  try { lpSupply = final.accounts[2] ? getMintDecoder().decode(final.accounts[2]!.data).supply : null; } catch { lpSupply = null; }
  let finalEscrow: ReturnType<ReturnType<typeof getTokenDecoder>["decode"]> | null = null;
  try { finalEscrow = final.accounts[1] ? getTokenDecoder().decode(final.accounts[1]!.data) : null; } catch { finalEscrow = null; }
  const escrowRent = await connection.getMinimumBalanceForRentExemption(165, "finalized");
  add(gates, "final receipt closed and canonical escrow retained empty", final.accounts[0] === null
    && final.accounts[1]?.owner === PARTNER_ROUTE.programs.token
    && final.accounts[1]?.lamports === escrowRent
    && finalEscrow?.mint.toString() === accounts.lpMint
    && finalEscrow?.owner.toString() === userAccounts.requestWithdrawVaultReceipt
    && finalEscrow?.amount === 0n,
  { receipt: final.accounts[0]?.address ?? null, escrow: final.accounts[1] ? { address: final.accounts[1]!.address, ownerProgram: final.accounts[1]!.owner, lamports: final.accounts[1]!.lamports, mint: finalEscrow?.mint.toString() ?? null, authority: finalEscrow?.owner.toString() ?? null, amount: finalEscrow?.amount ?? null } : null },
  { receipt: null, escrow: { address: userAccounts.requestWithdrawLpAta, ownerProgram: PARTNER_ROUTE.programs.token, lamports: escrowRent, mint: accounts.lpMint, authority: userAccounts.requestWithdrawVaultReceipt, amount: 0n } });
  add(gates, "final LP mint canonical", final.accounts[2]?.owner === PARTNER_ROUTE.asset.tokenProgram && lpSupply !== null, final.accounts[2] ? { owner: final.accounts[2]!.owner, supply: lpSupply } : null, { owner: PARTNER_ROUTE.asset.tokenProgram, supply: "decoded" });
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return { verdict: failedGateCount === 0 ? "PARTNER_LIFECYCLE_PASS" : "PARTNER_LIFECYCLE_FAIL", broadcast: false, evidencePath: path, routeSpecSha256: routeSpecSha256(), currentContextSlot, transactions: loaded.map(({ operation, evidence, response, messageSha256 }) => ({ operation, signature: evidence.signature, slot: response?.slot ?? null, messageSha256 })), failedGateCount, gates } as const;
}
