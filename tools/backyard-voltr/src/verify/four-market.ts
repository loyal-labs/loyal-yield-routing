import { createHash } from "node:crypto";
import { lstatSync, readFileSync, realpathSync } from "node:fs";
import { dirname, isAbsolute, resolve, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { findRequestWithdrawVaultReceiptPda, getRequestWithdrawVaultReceiptDiscriminatorBytes, getStrategyInitReceiptDecoder, getStrategyInitReceiptDiscriminatorBytes, parseTransactionEvents } from "@voltr/vault-sdk";
import { Obligation } from "@kamino-finance/klend-sdk";
import { address, createNoopSigner, isSignerRole, isWritableRole } from "@solana/kit";
import { getCreateAssociatedTokenIdempotentInstructionAsync, getMintDecoder, getTokenDecoder } from "@solana-program/token";
import bs58 from "bs58";
import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  Connection,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
  type Commitment,
  type VersionedTransactionResponse,
} from "@solana/web3.js";

import {
  PARTNER_FOUR_MARKET_ROUTE,
  PARTNER_ROUTE,
  fourMarketRouteSpecSha256,
  partnerBuilderRoute,
  partnerStrategyIdentity,
  type PartnerStrategyId,
} from "../domain/route-spec.js";
import { confirmedSnapshots, loadDeploymentIdentities, loadMainReserveGraph } from "../integrations/solana-compat.js";
import { createVoltrRouteBuilder, deriveVoltrAccountsForStrategy, type CanonicalInstruction, type ReserveGraph } from "../integrations/voltr.js";
import { intentSha256 as executionIntentSha256, type ExecutionIntent } from "../domain/execution-intent.js";
import { effectiveRouteAuthorizationDigest, loadPolicyCatalogAuthorization } from "../policies/authorization.js";
import { loadRuntimePolicyArtifact } from "../policies/compiler.js";
import { verifyExistingRuntimePolicies } from "../policies/commands.js";
import { buildManagerWrapperForVerification } from "../runtime/manager.js";
import {
  assertProtectedPreSendAttestation,
  assertProtectedSettlementAttestation,
  assertProtectedSnapshotEvidence,
  fourMarketProtectedAddressSetSha256,
  verifyProtectedAttestationSignature,
  type ProtectedPreSendAttestation,
  type ProtectedSettlementAttestation,
  type ProtectedSnapshotEvidence,
} from "../runtime/protected-state.js";
import { decodeReceipt } from "../runtime/receipt.js";
import { planWithdrawalRestoration, type WithdrawalRestorationScan, type WithdrawalRestorationSource } from "../runtime/withdrawal-restoration.js";
import { validateEarnSharedReplay } from "../runtime/earn-adapter.js";
import { scanWithdrawalDemand } from "../runtime/withdrawal-scanner.js";
import {
  verifyAdaptorReceipt,
  verifyDeploymentIdentities,
  verifyStrategyBootstrap,
  verifyVaultCurrentState,
  type Gate,
} from "./current.js";
import { verifyLegacyVoltrPolicyCatalog, verifyNonCatalogSquadsPoliciesIsolated } from "./squads.js";
import { verifyNegativeMutationArtifact } from "./negative-mutations.js";

const REQUIRED_STRATEGIES = ["main", "onre", "prime", "maple"] as const;
const REQUIRED_TXS = [
  "userDeposit",
  "managerMainDeposit", "managerMainWithdraw",
  "managerOnreDeposit", "managerOnreWithdraw",
  "managerPrimeDeposit", "managerPrimeWithdraw",
  "managerMapleDeposit", "managerMapleWithdraw",
  "managerMainFallbackDeposit", "withdrawRequest",
  "managerMainRestorationWithdraw", "withdrawClaim",
] as const;
const REQUIRED_ARTIFACTS = [
  "instantWithdrawRejection", "prematureClaim", "withdrawalScanner", "restoration", "earnAdapter",
  "negativeMutations", "finalReconciliation",
] as const;
const MANAGER_COMPUTE_UNIT_LIMIT = 500_000;
const MANAGER_HEAP_FRAME_BYTES = 256 * 1_024;
const FRACTION_SCALE = 1n << 48n;
const DEPOSIT_CONSTRAINED_INDEXES = [0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15, 17, 21, 29, 30] as const;
const WITHDRAW_CONSTRAINED_INDEXES = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12, 13, 14, 15, 17, 21, 26, 27] as const;
const REQUIRED_NEGATIVE_MUTATIONS = [
  "wrong-guardian", "wrong-manager", "wrong-vault", "wrong-strategy", "wrong-reserve",
  "wrong-market", "wrong-farm", "wrong-receipt", "wrong-obligation", "wrong-mint",
  "wrong-program", "account-order", "account-role", "wrong-discriminator", "adaptor-tail",
  "zero-amount", "over-limit-amount", "mixed-graph", "extra-instruction", "reordered-instruction",
] as const;
const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const EARN_ADAPTER_SOURCE_PATHS = [
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/observation.rs",
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/planner.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/queue.rs",
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-earn-replay.rs",
  "crates/loyal-yield-orchestrator/src/fleet_orchestration/mod.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/domain.rs",
  "tools/backyard-voltr/src/domain/route-spec.ts",
  "tools/backyard-voltr/src/runtime/earn-adapter.ts",
] as const;
const EXECUTION_SOURCE_CONTRACT_PATHS = [
  "tools/backyard-voltr/src/runtime/manager.ts",
  "tools/backyard-voltr/src/runtime/commands.ts",
  "tools/backyard-voltr/src/integrations/solana-compat.ts",
  "tools/backyard-voltr/src/runtime/restoration-bridge.ts",
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-restoration-bridge.rs",
  "crates/loyal-yield-store/src/fleet_orchestration/voltr_restoration.rs",
  "tools/backyard-voltr/src/runtime/restoration-evidence.ts",
  "tools/backyard-voltr/src/runtime/withdrawal-restoration.ts",
  "tools/backyard-voltr/src/runtime/withdrawal-scanner.ts",
  "tools/backyard-voltr/src/runtime/receipt.ts",
  "crates/loyal-yield-orchestrator/src/bin/backyard-voltr-restoration-readback.rs",
  "crates/loyal-yield-orchestrator/src/bin/fleet-opportunity-planner.rs",
  "tools/backyard-voltr/src/verify/four-market.ts",
  "tools/backyard-voltr/src/runtime/protected-state.ts",
] as const;

type JsonRecord = Record<string, unknown>;
type StrategyId = (typeof REQUIRED_STRATEGIES)[number];
type TxName = (typeof REQUIRED_TXS)[number];
type ArtifactName = (typeof REQUIRED_ARTIFACTS)[number];
type ArtifactRef = Readonly<{ path: string; fileSha256: string }>;
type TxEvidence = ArtifactRef & Readonly<{
  signature: string;
  intentSha256: string;
  messageSha256: string;
  slot: number;
  protectedAddressSetSha256: string;
  protectedPrestateSha256: string;
  protectedPoststateSha256: string;
  protectedBeforeContextSlot: number;
  protectedAfterContextSlot: number;
  protectedPreAttestationSha256: string;
  protectedSettlementAttestationSha256: string;
}>;
type RequestOrigin = Readonly<{
  signature: string;
  eventIndex: number;
  receipt: string;
  rawAccountSha256: string;
  generationFingerprint: string;
}>;
type VerifiedWithdrawalScan = WithdrawalRestorationScan & Readonly<{
  requestOrigin: RequestOrigin;
  rawQuerySha256: string;
  queryConfigSha256: string;
}>;

function executionSourceContract(): Readonly<{ pass: boolean; observed: JsonRecord; expected: JsonRecord }> {
  const files = Object.fromEntries(EXECUTION_SOURCE_CONTRACT_PATHS.map((path) => {
    const source = readFileSync(resolve(REPOSITORY_ROOT, path), "utf8");
    return [path, { sha256: sha256(source), source }];
  })) as Record<string, { sha256: string; source: string }>;
  const manager = files[EXECUTION_SOURCE_CONTRACT_PATHS[0]]!.source;
  const commands = files[EXECUTION_SOURCE_CONTRACT_PATHS[1]]!.source;
  const transport = files[EXECUTION_SOURCE_CONTRACT_PATHS[2]]!.source;
  const restorationAdapter = files[EXECUTION_SOURCE_CONTRACT_PATHS[3]]!.source;
  const restorationBridge = files[EXECUTION_SOURCE_CONTRACT_PATHS[4]]!.source;
  const restorationStore = files[EXECUTION_SOURCE_CONTRACT_PATHS[5]]!.source;
  const persistedManagerCall = manager.indexOf("const persistedIntent = persistManagerIntent");
  const restorationPhaseACall = manager.indexOf("restorationPhaseA = prepareRestorationBridge", persistedManagerCall);
  const managerSendCall = manager.indexOf("sendPreparedConfirmedOnce(", restorationPhaseACall);
  const restorationPhaseBCall = manager.indexOf("restorationPhaseB = confirmRestorationBridge", managerSendCall);
  const restorationReadbackGuard = manager.indexOf("if (managerReadbackExact) {", managerSendCall);
  const restorationReadbackElse = manager.indexOf("} else {", restorationReadbackGuard);
  const bridgeLease = restorationBridge.indexOf("lease_exact_voltr_restoration_handoff");
  const bridgeConflictFence = restorationBridge.indexOf("acquire_voltr_restoration_logical_conflict_lease", bridgeLease);
  const bridgeSignedWire = restorationBridge.indexOf("persist_voltr_manager_signed_intent", bridgeConflictFence);
  const bridgeBroadcastIntent = restorationBridge.indexOf("mark_voltr_manager_broadcast_intent", bridgeSignedWire);
  const checks = {
    managerPersistsBeforeSend: persistedManagerCall >= 0 && managerSendCall > persistedManagerCall,
    restorationPhaseABeforeSend: restorationPhaseACall > persistedManagerCall && managerSendCall > restorationPhaseACall,
    restorationPhaseBAfterConfirmedReadback: restorationReadbackGuard > managerSendCall && restorationPhaseBCall > restorationReadbackGuard && restorationReadbackElse > restorationPhaseBCall,
    restorationAdapterUsesPrebuiltNoShellBinary: restorationAdapter.includes("execFileSync(binary, [\"--input\", inputPath]") && restorationAdapter.includes("inputFileSha256") && !restorationAdapter.includes("shell: true"),
    restorationDurableOrdering: bridgeLease >= 0 && bridgeConflictFence > bridgeLease && bridgeSignedWire > bridgeConflictFence && bridgeBroadcastIntent > bridgeSignedWire,
    restorationStoreFencesExactBroadcastIntent: restorationStore.includes("broadcast_intent_persisted") && restorationStore.includes("'{execution,broadcastCount}', '1'::jsonb") && restorationStore.includes("input.remaining_shortfall_raw != 0"),
    managerExpectedSignatureRecovery: manager.includes("expectedSignature") && manager.includes("Do not resend"),
    userPersistsIntent: commands.includes("persistRuntimeIntent") && commands.includes("intentPath"),
    oneSendAndContextFence: transport.includes("sendRawTransaction") && transport.includes("maxRetries: 0") && transport.includes("minContextSlot") && transport.includes("MAX_IDENTICAL_SUBMISSION_ATTEMPTS") && transport.includes("getSignatureStatuses"),
  };
  return {
    pass: Object.values(checks).every(Boolean),
    observed: { files: Object.fromEntries(Object.entries(files).map(([path, value]) => [path, value.sha256])), checks, residual: "source inspection proves the maintained contract shape, not historical process execution or provider truth" },
    expected: { checks: { managerPersistsBeforeSend: true, restorationPhaseABeforeSend: true, restorationPhaseBAfterConfirmedReadback: true, restorationAdapterUsesPrebuiltNoShellBinary: true, restorationDurableOrdering: true, restorationStoreFencesExactBroadcastIntent: true, managerExpectedSignatureRecovery: true, userPersistsIntent: true, oneSendAndContextFence: true } },
  };
}
function managerTransactionDescriptor(name: TxName): Readonly<{ strategyId: StrategyId; operation: "deposit" | "withdraw" }> | null {
  if (name === "managerMainFallbackDeposit") return { strategyId: "main", operation: "deposit" };
  if (name === "managerMainRestorationWithdraw") return { strategyId: "main", operation: "withdraw" };
  const match = name.match(/^manager(Main|Onre|Prime|Maple)(Deposit|Withdraw)$/);
  return match ? { strategyId: match[1]!.toLowerCase() as StrategyId, operation: match[2]!.toLowerCase() as "deposit" | "withdraw" } : null;
}

function derivePolicyAddress(seed: bigint): string {
  const encodedSeed = Buffer.alloc(8);
  encodedSeed.writeBigUInt64LE(seed);
  return PublicKey.findProgramAddressSync(
    [Buffer.from("smart_account"), Buffer.from("policy"), new PublicKey(PARTNER_ROUTE.squads.settings).toBuffer(), encodedSeed],
    new PublicKey(PARTNER_ROUTE.squads.program),
  )[0].toBase58();
}
export type FourMarketManifest = Readonly<{
  schemaVersion: 1;
  evidenceType: "backyard-voltr-four-market-confirmed-lifecycle";
  commitment: "confirmed";
  routeId: string;
  routeSpecSha256: string;
  lifecycleId: string;
  routeAuthorizationSha256: string;
  requestOrigin: RequestOrigin;
  amounts: Readonly<{
    userDepositAssetRaw: bigint;
    managerAssetRaw: bigint;
    requestWithdrawLpRaw: bigint;
    restorationAssetRaw: bigint;
  }>;
  identities: Readonly<{
    vault: string; lpMint: string; settings: string; manager: string; guardian: string;
    user: string; assetMint: string;
  }>;
  strategies: readonly Readonly<{ id: StrategyId; reserve: string; strategyReceipt: string; strategyAssetAta: string }>[];
  policyCatalog: ArtifactRef & Readonly<{ artifactSha256: string }>;
  policyAuthorization: ArtifactRef & Readonly<{ authorizationSha256: string }>;
  transactions: Readonly<Record<TxName, TxEvidence>>;
  artifacts: Readonly<Record<ArtifactName, ArtifactRef>>;
}>;

function assertRestorationBridgeOutput(
  manifestPath: string,
  output: JsonRecord,
  intent: JsonRecord,
  wire: Buffer,
  signature: string,
  messageSha256: string,
  slot: number,
  protectedState: JsonRecord,
): void {
  const bridge = record(output.restorationBridge, "managerMainRestorationWithdraw.restorationBridge");
  exactKeys(bridge, ["phaseA", "phaseB", "requiredIdleRaw", "remainingShortfallRaw"], "managerMainRestorationWithdraw.restorationBridge");
  const phaseA = record(bridge.phaseA, "restorationBridge.phaseA");
  const phaseB = record(bridge.phaseB, "restorationBridge.phaseB");
  exactKeys(phaseA, ["verdict", "broadcast", "signerLoaded", "phase", "token", "tokenSha256", "managerHandoff", "nextStep", "inputPath", "inputFileSha256"], "restorationBridge.phaseA");
  exactKeys(phaseB, ["verdict", "broadcast", "signerLoaded", "phase", "completion", "tokenSha256", "inputPath", "inputFileSha256"], "restorationBridge.phaseB");
  if (phaseA.verdict !== "BACKYARD_VOLTR_RESTORATION_BRIDGE_PHASE_A_PASS" || phaseA.broadcast !== false || phaseA.signerLoaded !== false || phaseA.phase !== "prepare") throw new Error("restoration bridge Phase A is not the exact no-broadcast durable preparation");
  if (phaseB.verdict !== "BACKYARD_VOLTR_RESTORATION_BRIDGE_PHASE_B_PASS" || phaseB.broadcast !== false || phaseB.signerLoaded !== false || phaseB.phase !== "confirm") throw new Error("restoration bridge Phase B is not the exact no-broadcast confirmed acknowledgement");
  const token = record(phaseA.token, "restorationBridge.phaseA.token");
  const tokenKeys = ["schemaVersion", "eventId", "cluster", "owner", "fencingToken", "originId", "generation", "legId", "managerIntentId", "expectedSignature", "signedTransactionSha256", "messageSha256", "strategyId", "reserve", "amountRaw", "lifecycleId", "routeAuthorizationSha256", "protectedPrestateSha256", "protectedAddressSetSha256", "protectedContextSlot"] as const;
  exactKeys(token, tokenKeys, "restorationBridge.phaseA.token");
  const amountRaw = integerString(intent.amountRaw, "managerMainRestorationWithdraw.intent.amountRaw");
  const generation = token.generation;
  if (token.schemaVersion !== 1 || token.cluster !== "mainnet-beta" || typeof token.eventId !== "number" || !Number.isSafeInteger(token.eventId) || token.eventId <= 0 || typeof token.fencingToken !== "number" || !Number.isSafeInteger(token.fencingToken) || token.fencingToken <= 0 || typeof generation !== "number" || !Number.isSafeInteger(generation) || generation <= 0) throw new Error("restoration bridge token fence/generation is malformed");
  const originId = shaField(token, "originId", "restorationBridge.phaseA.token");
  const legId = shaField(token, "legId", "restorationBridge.phaseA.token");
  const managerIntentId = shaField(token, "managerIntentId", "restorationBridge.phaseA.token");
  const expectedManagerIntentId = sha256(`backyard-voltr-manager-intent-v1:${originId}:${generation}:${legId}`);
  const tokenExact = managerIntentId === expectedManagerIntentId
    && token.expectedSignature === signature
    && token.signedTransactionSha256 === sha256(wire)
    && token.messageSha256 === messageSha256
    && token.strategyId === "main"
    && token.reserve === partnerStrategyIdentity("main").reserve
    && safeIntegerNumber(token.amountRaw, "restorationBridge.phaseA.token.amountRaw") === amountRaw
    && token.lifecycleId === intent.lifecycleId
    && token.routeAuthorizationSha256 === intent.routeAuthorizationSha256
    && token.protectedPrestateSha256 === protectedState.beforeSha256
    && token.protectedAddressSetSha256 === protectedState.addressSetSha256
    && typeof token.protectedContextSlot === "number"
    && Number.isSafeInteger(token.protectedContextSlot)
    && token.protectedContextSlot > 0
    && typeof protectedState.beforeContextSlot === "number"
    && token.protectedContextSlot <= protectedState.beforeContextSlot;
  if (!tokenExact) throw new Error("restoration bridge token is not bound to the exact signed Main withdrawal and protected checkpoint");
  const orderedToken = Object.fromEntries(tokenKeys.map((key) => [key, token[key]]));
  const tokenSha256 = sha256(JSON.stringify(orderedToken));
  if (phaseA.tokenSha256 !== tokenSha256 || phaseB.tokenSha256 !== tokenSha256) throw new Error("restoration bridge Phase A/B token hash differs");
  const handoff = record(phaseA.managerHandoff, "restorationBridge.phaseA.managerHandoff");
  exactKeys(handoff, ["operation", "strategyId", "reserve", "amountRaw", "eventId", "fencingToken", "leaseExpiresAt", "expectedSignature"], "restorationBridge.phaseA.managerHandoff");
  if (handoff.operation !== "manager-withdraw" || handoff.strategyId !== "main" || handoff.reserve !== token.reserve || safeIntegerNumber(handoff.amountRaw, "restorationBridge.phaseA.managerHandoff.amountRaw") !== amountRaw || handoff.eventId !== token.eventId || handoff.fencingToken !== token.fencingToken || handoff.expectedSignature !== signature || typeof handoff.leaseExpiresAt !== "string" || handoff.leaseExpiresAt.length === 0 || typeof phaseA.nextStep !== "string" || phaseA.nextStep.length === 0) throw new Error("restoration bridge manager handoff differs from its exact token");

  const readInput = (rawPath: unknown, rawSha256: unknown, label: string): JsonRecord => {
    if (typeof rawPath !== "string" || rawPath.length === 0) throw new Error(`${label} path is missing`);
    if (typeof rawSha256 !== "string" || !/^[0-9a-f]{64}$/.test(rawSha256)) throw new Error(`${label} SHA-256 is malformed`);
    const evidenceRoot = realpathSync(resolve(dirname(manifestPath)));
    const absolute = resolve(rawPath);
    const stat = lstatSync(absolute);
    if (!stat.isFile() || stat.isSymbolicLink()) throw new Error(`${label} must be a regular non-symlink file`);
    const real = realpathSync(absolute);
    if (real !== absolute) throw new Error(`${label} path contains a symlink`);
    const relativePath = relative(evidenceRoot, real);
    if (relativePath.length === 0 || relativePath === ".." || relativePath.startsWith("../") || relativePath.startsWith("/")) throw new Error(`${label} path escapes the lifecycle evidence directory`);
    const bytes = readFileSync(real);
    if (sha256(bytes) !== rawSha256) throw new Error(`${label} file hash changed after the bridge consumed it`);
    return record(JSON.parse(bytes.toString("utf8")), label);
  };
  const phaseAInput = readInput(phaseA.inputPath, phaseA.inputFileSha256, "restorationBridge.phaseA.input");
  exactKeys(phaseAInput, ["schemaVersion", "phase", "cluster", "routeId", "routeSpecSha256", "vault", "owner", "leaseSeconds", "originId", "generation", "legId", "signedIntent"], "restorationBridge.phaseA.input");
  const signedIntent = record(phaseAInput.signedIntent, "restorationBridge.phaseA.input.signedIntent");
  exactKeys(signedIntent, ["managerIntentId", "lifecycleId", "strategyId", "reserve", "amountRaw", "routeAuthorizationSha256", "protectedPrestateSha256", "protectedAddressSetSha256", "protectedContextSlot", "signedTransactionHex", "signedTransactionSha256", "messageSha256", "expectedSignature", "recentBlockhash", "lastValidBlockHeight", "feePayer", "compiledFeeLamports", "writableAccountKeys", "logicalConflictKeys"], "restorationBridge.phaseA.input.signedIntent");
  const conflictKeys = Array.isArray(signedIntent.logicalConflictKeys) ? [...signedIntent.logicalConflictKeys].sort() : [];
  const expectedConflictKeys = [`kamino:reserve:${partnerStrategyIdentity("main").reserve}`, `voltr:vault:${PARTNER_ROUTE.vault}`].sort();
  if (phaseAInput.schemaVersion !== 1 || phaseAInput.phase !== "prepare" || phaseAInput.cluster !== "mainnet-beta" || phaseAInput.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || phaseAInput.routeSpecSha256 !== fourMarketRouteSpecSha256() || phaseAInput.vault !== PARTNER_ROUTE.vault || phaseAInput.originId !== originId || phaseAInput.generation !== generation || phaseAInput.legId !== legId || typeof phaseAInput.owner !== "string" || phaseAInput.owner.length === 0 || typeof phaseAInput.leaseSeconds !== "number" || !Number.isSafeInteger(phaseAInput.leaseSeconds) || phaseAInput.leaseSeconds < 60 || phaseAInput.leaseSeconds > 900 || signedIntent.managerIntentId !== managerIntentId || signedIntent.lifecycleId !== intent.lifecycleId || signedIntent.strategyId !== "main" || signedIntent.reserve !== token.reserve || safeIntegerNumber(signedIntent.amountRaw, "restorationBridge.phaseA.input.signedIntent.amountRaw") !== amountRaw || signedIntent.routeAuthorizationSha256 !== intent.routeAuthorizationSha256 || signedIntent.protectedPrestateSha256 !== token.protectedPrestateSha256 || signedIntent.protectedAddressSetSha256 !== token.protectedAddressSetSha256 || signedIntent.protectedContextSlot !== token.protectedContextSlot || signedIntent.signedTransactionHex !== wire.toString("hex") || signedIntent.signedTransactionSha256 !== sha256(wire) || signedIntent.messageSha256 !== messageSha256 || signedIntent.expectedSignature !== signature || signedIntent.feePayer !== PARTNER_ROUTE.squads.guardian || JSON.stringify(conflictKeys) !== JSON.stringify(expectedConflictKeys)) throw new Error("restoration bridge Phase A input is not the exact signed manager wire/fence");

  const phaseBInput = readInput(phaseB.inputPath, phaseB.inputFileSha256, "restorationBridge.phaseB.input");
  exactKeys(phaseBInput, ["schemaVersion", "phase", "token", "confirmation"], "restorationBridge.phaseB.input");
  const confirmation = record(phaseBInput.confirmation, "restorationBridge.phaseB.input.confirmation");
  exactKeys(confirmation, ["managerIntentId", "lifecycleId", "strategyId", "reserve", "amountRaw", "routeAuthorizationSha256", "signedTransactionSha256", "messageSha256", "expectedSignature", "confirmedSlot", "readbackContextSlot", "commitment", "managerTransactionSignature", "idleRawAfter", "remainingShortfallRaw", "readbackFingerprint"], "restorationBridge.phaseB.input.confirmation");
  if (phaseBInput.schemaVersion !== 1 || phaseBInput.phase !== "confirm" || canonicalJson(phaseBInput.token) !== canonicalJson(token)) throw new Error("restoration bridge Phase B does not consume the exact Phase A token");
  const outputReadback = record(output.readback, "managerMainRestorationWithdraw.readback");
  const idleBefore = integerString(outputReadback.idleBefore, "managerMainRestorationWithdraw.readback.idleBefore");
  const idleAfter = integerString(outputReadback.idleAfter, "managerMainRestorationWithdraw.readback.idleAfter");
  const requiredIdleRaw = integerString(bridge.requiredIdleRaw, "managerMainRestorationWithdraw.restorationBridge.requiredIdleRaw");
  const remainingShortfallRaw = integerString(bridge.remainingShortfallRaw, "managerMainRestorationWithdraw.restorationBridge.remainingShortfallRaw");
  const readbackContextSlot = output.readbackContextSlot;
  if (requiredIdleRaw !== idleBefore + amountRaw || remainingShortfallRaw !== 0n || typeof readbackContextSlot !== "number" || !Number.isSafeInteger(readbackContextSlot) || readbackContextSlot < slot) throw new Error("restoration bridge output does not restore the exact required idle amount at a confirmed context");
  const expectedReadbackFingerprint = sha256(Buffer.from(JSON.stringify({ signature, confirmedSlot: slot, readbackContextSlot, idleRawAfter: idleAfter.toString(), remainingShortfallRaw: "0", protectedPoststateSha256: protectedState.afterSha256 }), "utf8"));
  if (confirmation.managerIntentId !== managerIntentId || confirmation.lifecycleId !== intent.lifecycleId || confirmation.strategyId !== "main" || confirmation.reserve !== token.reserve || safeIntegerNumber(confirmation.amountRaw, "restorationBridge.phaseB.input.confirmation.amountRaw") !== amountRaw || confirmation.routeAuthorizationSha256 !== intent.routeAuthorizationSha256 || confirmation.signedTransactionSha256 !== sha256(wire) || confirmation.messageSha256 !== messageSha256 || confirmation.expectedSignature !== signature || confirmation.confirmedSlot !== slot || confirmation.readbackContextSlot !== readbackContextSlot || confirmation.commitment !== "confirmed" || confirmation.managerTransactionSignature !== signature || safeIntegerNumber(confirmation.idleRawAfter, "restorationBridge.phaseB.input.confirmation.idleRawAfter") !== idleAfter || safeIntegerNumber(confirmation.remainingShortfallRaw, "restorationBridge.phaseB.input.confirmation.remainingShortfallRaw") !== 0n || confirmation.readbackFingerprint !== expectedReadbackFingerprint) throw new Error("restoration bridge Phase B confirmation is not the exact confirmed manager readback");
  const completion = record(phaseB.completion, "restorationBridge.phaseB.completion");
  exactKeys(completion, ["eventId", "originId", "generation", "legId", "state", "acknowledged", "canceledSiblingCount"], "restorationBridge.phaseB.completion");
  if (completion.eventId !== token.eventId || completion.originId !== originId || completion.generation !== generation || completion.legId !== legId || completion.state !== "acknowledged" || completion.acknowledged !== true || typeof completion.canceledSiblingCount !== "number" || !Number.isSafeInteger(completion.canceledSiblingCount) || completion.canceledSiblingCount < 0) throw new Error("restoration bridge completion is not the exact acknowledged durable fence");
}

/**
 * Build the verifier envelope from maintained command outputs. The builder
 * derives signature, confirmed slot, canonical message hash, and file hashes
 * from each output; it deliberately has no fields for caller-supplied signer,
 * program, account, or delta expectations.
 */
export function buildFourMarketManifestFromArtifacts(input: Readonly<{
  manifestPath: string;
  policyCatalogPath: string;
  policyAuthorizationPath: string;
  transactionPaths: Readonly<Record<TxName, string>>;
  artifactPaths: Readonly<Record<ArtifactName, string>>;
}>): FourMarketManifest {
  const manifestPath = resolve(input.manifestPath);
  const childRef = (path: string): ArtifactRef => {
    const absolute = resolveChild(manifestPath, path);
    const bytes = readFileSync(absolute);
    return { path, fileSha256: sha256(bytes) };
  };
  const policyCatalogAbsolute = resolveChild(manifestPath, input.policyCatalogPath);
  const policyAuthorizationAbsolute = resolveChild(manifestPath, input.policyAuthorizationPath);
  const policyCatalogLoaded = loadRuntimePolicyArtifact(policyCatalogAbsolute);
  const policyAuthorizationRef = childRef(input.policyAuthorizationPath);
  const policyAuthorizationLoaded = loadPolicyCatalogAuthorization(policyAuthorizationAbsolute, policyCatalogAbsolute, policyAuthorizationRef.fileSha256);
  const routeAuthorizationSha256 = effectiveRouteAuthorizationDigest(
    policyCatalogLoaded,
    policyAuthorizationLoaded,
  ).sha256;
  const policyCatalogValue = record(JSON.parse(readFileSync(policyCatalogAbsolute, "utf8")), "policy catalog");
  const catalogPolicies = policyCatalogValue.policies;
  if (!Array.isArray(catalogPolicies) || catalogPolicies.length !== 8) throw new Error("policy catalog must contain exactly eight policies before building a lifecycle manifest");
  const intents = new Map<TxName, JsonRecord>();
  const lifecycleIds = new Map<TxName, string>();
  const protectedEvidenceByName = new Map<TxName, ValidatedProtectedTransactionEvidence>();
  const transactionEntries = Object.fromEntries(REQUIRED_TXS.map((name) => {
    const path = input.transactionPaths[name];
    const absolute = resolveChild(manifestPath, path);
    const value = record(JSON.parse(readFileSync(absolute, "utf8")), `transaction artifact ${name}`);
    const settled = confirmedSettlement(value, name);
    const intent = record(value.intent, `${name}.intent`);
    const derivedIntentSha256 = executionIntentSha256(intent as unknown as ExecutionIntent);
    if (value.intentSha256 !== derivedIntentSha256) throw new Error(`${name} command output intent hash does not match its exact intent`);
    const lifecycleId = shaField(value, "lifecycleId", `${name}.output`);
    if (intent.lifecycleId !== lifecycleId) throw new Error(`${name} signed intent does not bind its lifecycle id`);
    lifecycleIds.set(name, lifecycleId);
    intents.set(name, intent);
    const managerDescriptor = managerTransactionDescriptor(name);
    const expectedIntentOperation = managerDescriptor ? (managerDescriptor.operation === "deposit" ? "manager-deposit" : "manager-withdraw") : ({ userDeposit: "user-deposit", withdrawRequest: "withdraw-request", withdrawClaim: "withdraw-claim" } as const)[name as "userDeposit" | "withdrawRequest" | "withdrawClaim"];
    const expectedPolicy = managerDescriptor ? catalogPolicies.find((candidate) => {
      const entry = record(candidate, "policy catalog entry");
      return entry.strategyId === managerDescriptor.strategyId && entry.operation === managerDescriptor.operation;
    }) : null;
    const routeAuthorizationExact = !managerDescriptor
      || intent.routeAuthorizationSha256 === routeAuthorizationSha256;
    if (intent.routeId !== PARTNER_FOUR_MARKET_ROUTE.id || intent.routeSpecSha256 !== fourMarketRouteSpecSha256() || intent.operation !== expectedIntentOperation || typeof value.intentSha256 !== "string" || (expectedPolicy !== null && intent.policy !== record(expectedPolicy, "expected policy").policy) || !routeAuthorizationExact) throw new Error(`${name} intent is not bound to the four-market route, policy, effective route authorization, and expected operation`);
    const signature = stringField(settled, "signature", `${name}.settled`);
    const intentPath = resolve(stringField(value, "intentPath", `${name}.output`));
    const intentRoot = resolve(dirname(manifestPath), "intents");
    const intentStat = lstatSync(intentPath);
    if (!intentStat.isFile() || intentStat.isSymbolicLink()) throw new Error(`${name} persisted intent must be a regular non-symlink file`);
    const canonicalIntentRoot = realpathSync(intentRoot);
    const canonicalIntentPath = realpathSync(intentPath);
    if (canonicalIntentPath !== intentPath) throw new Error(`${name} persisted intent path contains a symlink`);
    const intentRelative = relative(canonicalIntentRoot, canonicalIntentPath);
    if (intentRelative.length === 0 || intentRelative === ".." || intentRelative.startsWith("../") || intentRelative.startsWith("/")) throw new Error(`${name} persisted intent path escapes the manifest intents directory`);
    const intentBytes = readFileSync(intentPath);
    if (sha256(intentBytes) !== shaField(value, "intentFileSha256", `${name}.output`)) throw new Error(`${name} persisted intent file hash changed`);
    const persisted = record(JSON.parse(intentBytes.toString("utf8")), `${name}.persistedIntent`);
    const wireBase64 = stringField(persisted, "serializedTransactionBase64", `${name}.persistedIntent`);
    const wire = Buffer.from(wireBase64, "base64");
    if (wire.toString("base64") !== wireBase64 || sha256(wire) !== shaField(persisted, "serializedTransactionSha256", `${name}.persistedIntent`)) throw new Error(`${name} persisted signed wire encoding/hash is not canonical`);
    const signed = VersionedTransaction.deserialize(wire);
    const wireSignature = signed.signatures.length === 1 ? bs58.encode(signed.signatures[0]!) : null;
    const messageSha256 = sha256(signed.message.serialize());
    const persistedMessageSha256 = typeof persisted.serializedMessageSha256 === "string"
      ? shaField(persisted, "serializedMessageSha256", `${name}.persistedIntent`)
      : shaField(persisted, "canonicalMessageSha256", `${name}.persistedIntent`);
    if (wireSignature !== signature || persisted.expectedSignature !== signature || persistedMessageSha256 !== messageSha256 || shaField(intent, "canonicalMessageSha256", `${name}.intent`) !== messageSha256 || canonicalJson(record(persisted.intent, `${name}.persistedIntent.intent`)) !== canonicalJson(intent)) throw new Error(`${name} persisted signed wire is not bound to the confirmed signature/message/intent`);
    const slotRaw = settled.confirmedSlot;
    const slot = typeof slotRaw === "number" ? slotRaw : Number(stringField(settled, "confirmedSlot", `${name}.settled`));
    if (!Number.isSafeInteger(slot) || slot <= 0 || value.broadcast !== true || settled.err !== null || settled.settlementCommitment !== "confirmed") throw new Error(`${name} artifact is not a successful confirmed command output`);
    const senderProof = record(value.senderProof, `${name}.senderProof`);
    const legacySenderKeys = ["schemaVersion", "signerRole", "signer", "senderSourceSha256", "persistedBeforeSend", "sendAttemptCount", "maxRetries", "recoveryByExpectedSignatureOnly", "expectedSignature", "serializedTransactionSha256", "serializedMessageSha256", "oneSendOnly", "confirmedSlot"] as const;
    const recoveredSenderKeys = [...legacySenderKeys, "submissionAttemptCount", "submissionWireSha256", "submissionAttempts"] as const;
    const senderHasRecoveryProof = Object.keys(senderProof).sort().join("\0") === [...recoveredSenderKeys].sort().join("\0");
    const senderHasLegacyProof = Object.keys(senderProof).sort().join("\0") === [...legacySenderKeys].sort().join("\0");
    if (!senderHasRecoveryProof && !senderHasLegacyProof) throw new Error(`${name}.senderProof keys are not exact`);
    const submissionAttempts = senderHasRecoveryProof && Array.isArray(senderProof.submissionAttempts)
      ? senderProof.submissionAttempts.map((attempt, index) => {
        const row = record(attempt, `${name}.senderProof.submissionAttempts[${index}]`);
        exactKeys(row, ["attempt", "wireSha256", "expectedSignature", "returnedSignature", "error"], `${name}.senderProof.submissionAttempts[${index}]`);
        return row;
      })
      : [];
    const persistence = record(value.persistenceContract, `${name}.persistenceContract`);
    const persistedContract = record(persisted.persistenceContract, `${name}.persistedIntent.persistenceContract`);
    const legacyPersistenceKeys = ["schemaVersion", "kind", "persistedBeforeSend", "oneSendOnly", "maxSendAttempts", "maxRetries", "recoveryByExpectedSignature", "recoveryByExpectedSignatureOnly", "expectedSignature", "serializedTransactionSha256", "serializedMessageSha256", "intentSha256", "lifecycleId", ...(managerDescriptor ? ["routeAuthorizationSha256"] : []), "protectedPrestateSha256"];
    const recoveredPersistenceKeys = [...legacyPersistenceKeys, "maxSubmissionAttempts", "submissionWireSha256"];
    const attestedManagerPersistenceKeys = [...recoveredPersistenceKeys, "preSendAttestationSha256", "preSendAttestationSignatureSha256"];
    const exactContractKeys = (candidate: JsonRecord, label: string): "legacy" | "recovered" | "attested-manager" => {
      const keys = Object.keys(candidate).sort().join("\0");
      if (managerDescriptor && keys === [...attestedManagerPersistenceKeys].sort().join("\0")) return "attested-manager";
      if (keys === [...recoveredPersistenceKeys].sort().join("\0")) return "recovered";
      if (keys === [...legacyPersistenceKeys].sort().join("\0")) return "legacy";
      throw new Error(`${label} keys are not exact`);
    };
    const persistenceVersion = exactContractKeys(persistence, `${name}.persistenceContract`);
    if (exactContractKeys(persistedContract, `${name}.persistedIntent.persistenceContract`) !== persistenceVersion) throw new Error(`${name} persistence contract schema differs between output and persisted intent`);
    const senderSourcePath = managerDescriptor
      ? "tools/backyard-voltr/src/runtime/manager.ts"
      : "tools/backyard-voltr/src/runtime/commands.ts";
    const expectedSenderSourceSha256 = sha256(readFileSync(resolve(REPOSITORY_ROOT, senderSourcePath)));
    const expectedSignerRole = managerDescriptor ? "guardian" : "user";
    const expectedSigner = managerDescriptor ? PARTNER_ROUTE.squads.guardian : stringField(intent, "user", `${name}.intent`);
    const wireSha256 = sha256(wire);
    const submissionProofExact = senderHasRecoveryProof
      && typeof senderProof.submissionAttemptCount === "number"
      && Number.isSafeInteger(senderProof.submissionAttemptCount)
      && senderProof.submissionAttemptCount === 1
      && senderProof.submissionAttemptCount === submissionAttempts.length
      && senderProof.submissionWireSha256 === wireSha256
      && submissionAttempts.every((attempt, index) => attempt.attempt === index + 1 && attempt.wireSha256 === wireSha256 && attempt.expectedSignature === signature && (attempt.returnedSignature === null || attempt.returnedSignature === signature));
    const senderExact = senderProof.schemaVersion === (senderHasRecoveryProof ? 2 : 1)
      && senderProof.signerRole === expectedSignerRole
      && senderProof.signer === expectedSigner
      && senderProof.senderSourceSha256 === expectedSenderSourceSha256
      && senderProof.persistedBeforeSend === true
      && senderProof.sendAttemptCount === 1
      && senderProof.maxRetries === 0
      && senderProof.recoveryByExpectedSignatureOnly === true
      && senderProof.expectedSignature === signature
      && senderProof.serializedTransactionSha256 === wireSha256
      && senderProof.serializedMessageSha256 === messageSha256
      && senderProof.oneSendOnly === true
      && senderProof.confirmedSlot === slot
      && (senderHasLegacyProof || submissionProofExact);
    const persistenceExact = persistence.schemaVersion === (persistenceVersion === "legacy" ? 1 : 2)
      && persistence.kind === "pre-send-signed-wire"
      && persistence.persistedBeforeSend === true
      && persistence.oneSendOnly === true
      && persistence.maxSendAttempts === 1
      && persistence.maxRetries === 0
      && persistence.recoveryByExpectedSignature === true
      && persistence.recoveryByExpectedSignatureOnly === true
      && persistence.expectedSignature === signature
      && persistence.serializedTransactionSha256 === wireSha256
      && persistence.serializedMessageSha256 === messageSha256
      && persistence.intentSha256 === derivedIntentSha256
      && persistence.lifecycleId === lifecycleId
      && persistence.protectedPrestateSha256 === intent.protectedPrestateSha256
      && (persistenceVersion === "legacy" || (persistence.maxSubmissionAttempts === 1 && persistence.submissionWireSha256 === wireSha256))
      && (!managerDescriptor || persistence.routeAuthorizationSha256 === routeAuthorizationSha256)
      && canonicalJson(persistence) === canonicalJson(persistedContract);
    if (!senderExact || !persistenceExact) throw new Error(`${name} sender/persistence contract is not exact or was not retained in the pre-send file`);
    const protectedState = record(value.protectedState, `${name}.protectedState`);
    exactKeys(protectedState, ["schemaVersion", "addressSetSha256", "beforeContextSlot", "beforeSha256", "afterContextSlot", "afterSha256"], `${name}.protectedState`);
    const beforeContextSlot = protectedState.beforeContextSlot;
    const afterContextSlot = protectedState.afterContextSlot;
    if (protectedState.schemaVersion !== 1 || protectedState.addressSetSha256 !== fourMarketProtectedAddressSetSha256() || typeof beforeContextSlot !== "number" || !Number.isSafeInteger(beforeContextSlot) || beforeContextSlot <= 0 || typeof afterContextSlot !== "number" || !Number.isSafeInteger(afterContextSlot) || afterContextSlot < slot || afterContextSlot < beforeContextSlot || intent.protectedPrestateSha256 !== protectedState.beforeSha256 || !/^[0-9a-f]{64}$/.test(protectedState.afterSha256 as string)) throw new Error(`${name} protected state envelope/context is not exact`);
    const protectedEvidence = validatedProtectedTransactionEvidence(value, persisted, {
      label: name,
      lifecycleId,
      operation: expectedIntentOperation,
      expectedSigner,
      expectedSignature: signature,
      messageSha256,
      serializedTransactionSha256: wireSha256,
      intentSha256: derivedIntentSha256,
      intentProtectedPrestateSha256: shaField(intent, "protectedPrestateSha256", `${name}.intent`),
      confirmedSlot: slot,
    });
    const intentPrestateSlotRaw = stringField(intent, "prestateSlot", `${name}.intent`);
    if (!/^[1-9]\d*$/.test(intentPrestateSlotRaw) || BigInt(intentPrestateSlotRaw) > BigInt(protectedEvidence.before.contextSlot)) {
      throw new Error(`${name} intent prestate slot is not at or before the exact attested pre-send snapshot context`);
    }
    if (managerDescriptor && (persistence.preSendAttestationSha256 !== protectedEvidence.preSendAttestation.attestationSha256
      || persistence.preSendAttestationSignatureSha256 !== protectedEvidence.preSendAttestation.signatureSha256)) {
      throw new Error(`${name} persistence contract does not bind the exact protected pre-send attestation`);
    }
    protectedEvidenceByName.set(name, protectedEvidence);
    if (name === "managerMainRestorationWithdraw") {
      assertRestorationBridgeOutput(manifestPath, value, intent, wire, signature, messageSha256, slot, protectedState);
    } else if (managerDescriptor && value.restorationBridge !== null) {
      throw new Error(`${name} unexpectedly used the withdrawal-restoration durable bridge`);
    }
    return [name, { ...childRef(path), signature, intentSha256: derivedIntentSha256, messageSha256, slot, protectedAddressSetSha256: shaField(protectedState, "addressSetSha256", `${name}.protectedState`), protectedPrestateSha256: protectedEvidence.before.stateSha256, protectedPoststateSha256: protectedEvidence.after.stateSha256, protectedBeforeContextSlot: protectedEvidence.before.contextSlot, protectedAfterContextSlot: protectedEvidence.after.contextSlot, protectedPreAttestationSha256: protectedEvidence.preSendAttestation.attestationSha256, protectedSettlementAttestationSha256: protectedEvidence.settlementAttestation.attestationSha256 }];
  })) as Record<TxName, TxEvidence>;
  const amount = (name: TxName): bigint => BigInt(stringField(intents.get(name)!, "amountRaw", `${name}.intent`));
  const user = stringField(intents.get("userDeposit")!, "user", "userDeposit.intent");
  const userNames = ["userDeposit", "withdrawRequest", "withdrawClaim"] as const;
  if (userNames.some((name) => intents.get(name)!.user !== user || intents.get(name)!.signerRole !== "user")) throw new Error("all user lifecycle intents must use the same exact user identity");
  const managerAmounts = REQUIRED_TXS
    .filter((name) => name.startsWith("manager") && name !== "managerMainRestorationWithdraw")
    .map(amount);
  if (managerAmounts.some((value) => value !== managerAmounts[0])) throw new Error("all manager proof and fallback-allocation legs must use the same bounded manager amount");
  const restorationAssetRaw = amount("managerMainRestorationWithdraw");
  if (restorationAssetRaw <= 0n || restorationAssetRaw > PARTNER_ROUTE.asset.maxManagerOperationRaw) throw new Error("restoration manager leg must use a positive amount inside the manager cap");
  if (amount("withdrawClaim") !== amount("withdrawRequest")) throw new Error("claim intent amount must equal the request LP amount");
  const authorizationValue = record(JSON.parse(readFileSync(policyAuthorizationAbsolute, "utf8")), "policy authorization");
  const scannerArtifactValue = record(JSON.parse(readFileSync(resolveChild(manifestPath, input.artifactPaths.withdrawalScanner), "utf8")), "withdrawal scanner artifact");
  const claimOutputValue = record(JSON.parse(readFileSync(resolveChild(manifestPath, input.transactionPaths.withdrawClaim), "utf8")), "withdrawal claim artifact");
  const scannerOrigin = requestOrigin(scannerArtifactValue.requestOrigin, "withdrawalScanner.requestOrigin");
  const claimOrigin = requestOrigin(claimOutputValue.requestOrigin, "withdrawClaim.requestOrigin");
  if (!sameRequestOrigin(scannerOrigin, claimOrigin)) throw new Error("withdrawal scanner and claim output requestOrigin tuples differ");
  if (scannerOrigin.signature !== transactionEntries.withdrawRequest!.signature) throw new Error("requestOrigin signature does not identify the manifest withdrawal request");
  const lifecycleId = lifecycleIds.get(REQUIRED_TXS[0]!)!;
  if ([...lifecycleIds.values()].some((value) => value !== lifecycleId)) throw new Error("all maintained command artifacts must bind the same lifecycleId");
  for (let index = 1; index < REQUIRED_TXS.length; index += 1) {
    const previous = transactionEntries[REQUIRED_TXS[index - 1]!]!;
    const current = transactionEntries[REQUIRED_TXS[index]!]!;
    const previousEvidence = protectedEvidenceByName.get(REQUIRED_TXS[index - 1]!)!;
    const currentEvidence = protectedEvidenceByName.get(REQUIRED_TXS[index]!)!;
    if (previous.protectedPoststateSha256 !== current.protectedPrestateSha256
      || previous.protectedAddressSetSha256 !== current.protectedAddressSetSha256
      || previous.protectedAfterContextSlot > current.protectedBeforeContextSlot
      || canonicalJson(previousEvidence.after.rows) !== canonicalJson(currentEvidence.before.rows)) {
      throw new Error(`protected state bytes/context chain breaks before ${REQUIRED_TXS[index]}`);
    }
  }
  return {
    schemaVersion: 1,
    evidenceType: "backyard-voltr-four-market-confirmed-lifecycle",
    commitment: "confirmed",
    routeId: PARTNER_FOUR_MARKET_ROUTE.id,
    routeSpecSha256: fourMarketRouteSpecSha256(),
    lifecycleId,
    routeAuthorizationSha256,
    requestOrigin: scannerOrigin,
    amounts: { userDepositAssetRaw: amount("userDeposit"), managerAssetRaw: managerAmounts[0]!, requestWithdrawLpRaw: amount("withdrawRequest"), restorationAssetRaw },
    identities: { vault: PARTNER_ROUTE.vault, lpMint: PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint, settings: PARTNER_ROUTE.squads.settings, manager: PARTNER_ROUTE.squads.manager, guardian: PARTNER_ROUTE.squads.guardian, user, assetMint: PARTNER_ROUTE.asset.mint },
    strategies: REQUIRED_STRATEGIES.map((id) => ({ id, reserve: partnerStrategyIdentity(id).reserve, strategyReceipt: partnerStrategyIdentity(id).voltr.strategyInitReceipt, strategyAssetAta: partnerStrategyIdentity(id).voltr.strategyAssetAta })),
    policyCatalog: { ...childRef(input.policyCatalogPath), artifactSha256: shaField(policyCatalogValue, "artifactSha256", "policy catalog") },
    policyAuthorization: { ...policyAuthorizationRef, authorizationSha256: shaField(authorizationValue, "authorizationSha256", "policy authorization") },
    transactions: transactionEntries,
    artifacts: Object.fromEntries(REQUIRED_ARTIFACTS.map((name) => [name, childRef(input.artifactPaths[name])])) as Record<ArtifactName, ArtifactRef>,
  };
}

/**
 * Strict path-only CLI input. Expectations are intentionally absent: the
 * maintained command artifacts, RouteSpec, catalog, and authorization derive
 * every identity, amount, signature, slot, intent hash, and message hash.
 */
export function buildFourMarketManifestFromInputsFile(inputsPath: string, manifestPath: string): FourMarketManifest {
  const inputs = record(JSON.parse(readFileSync(resolve(inputsPath), "utf8")), "four-market manifest inputs");
  exactKeys(inputs, ["schemaVersion", "evidenceType", "policyCatalogPath", "policyAuthorizationPath", "transactionPaths", "artifactPaths"], "four-market manifest inputs");
  if (inputs.schemaVersion !== 1 || inputs.evidenceType !== "backyard-voltr-four-market-manifest-inputs") throw new Error("four-market manifest inputs must use exact schema v1");
  const transactionPaths = record(inputs.transactionPaths, "four-market manifest inputs.transactionPaths");
  exactKeys(transactionPaths, REQUIRED_TXS, "four-market manifest inputs.transactionPaths");
  const artifactPaths = record(inputs.artifactPaths, "four-market manifest inputs.artifactPaths");
  exactKeys(artifactPaths, REQUIRED_ARTIFACTS, "four-market manifest inputs.artifactPaths");
  return buildFourMarketManifestFromArtifacts({
    manifestPath: resolve(manifestPath),
    policyCatalogPath: stringField(inputs, "policyCatalogPath", "four-market manifest inputs"),
    policyAuthorizationPath: stringField(inputs, "policyAuthorizationPath", "four-market manifest inputs"),
    transactionPaths: Object.fromEntries(REQUIRED_TXS.map((name) => [name, stringField(transactionPaths, name, "four-market manifest inputs.transactionPaths")])) as Record<TxName, string>,
    artifactPaths: Object.fromEntries(REQUIRED_ARTIFACTS.map((name) => [name, stringField(artifactPaths, name, "four-market manifest inputs.artifactPaths")])) as Record<ArtifactName, string>,
  });
}

function sha256(value: string | ArrayLike<number>): string {
  return createHash("sha256").update(typeof value === "string" ? value : Uint8Array.from(value)).digest("hex");
}

function canonicalJson(value: unknown): string {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") return `{${Object.entries(value as JsonRecord).sort(([left], [right]) => left.localeCompare(right)).map(([key, entry]) => `${JSON.stringify(key)}:${canonicalJson(entry)}`).join(",")}}`;
  return JSON.stringify(value);
}

function record(value: unknown, label: string): JsonRecord {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`);
  return value as JsonRecord;
}

function exactKeys(value: JsonRecord, expected: readonly string[], label: string): void {
  if (Object.keys(value).sort().join("\0") !== [...expected].sort().join("\0")) throw new Error(`${label} keys are not exact`);
}

type ValidatedProtectedSnapshotEnvelope = Readonly<{
  before: ProtectedSnapshotEvidence;
  after: ProtectedSnapshotEvidence;
}>;

type ValidatedProtectedTransactionEvidence = ValidatedProtectedSnapshotEnvelope & Readonly<{
  preSendAttestation: ProtectedPreSendAttestation;
  settlementAttestation: ProtectedSettlementAttestation;
}>;

function validatedProtectedSnapshotEnvelope(
  value: unknown,
  protectedStateValue: unknown,
  label: string,
): ValidatedProtectedSnapshotEnvelope {
  const envelope = record(value, `${label}.protectedSnapshotEvidence`);
  exactKeys(envelope, ["schemaVersion", "addressSetSha256", "before", "after"], `${label}.protectedSnapshotEvidence`);
  const beforeValue = envelope.before;
  const afterValue = envelope.after;
  assertProtectedSnapshotEvidence(beforeValue);
  assertProtectedSnapshotEvidence(afterValue);
  const expectedAddressSetSha256 = fourMarketProtectedAddressSetSha256();
  if (envelope.schemaVersion !== 1
    || envelope.addressSetSha256 !== expectedAddressSetSha256
    || beforeValue.addressSetSha256 !== expectedAddressSetSha256
    || afterValue.addressSetSha256 !== expectedAddressSetSha256
    || afterValue.contextSlot < beforeValue.contextSlot) {
    throw new Error(`${label} protected snapshot envelope is not the exact ordered four-market address set/context`);
  }
  const protectedState = record(protectedStateValue, `${label}.protectedState`);
  exactKeys(protectedState, ["schemaVersion", "addressSetSha256", "beforeContextSlot", "beforeSha256", "afterContextSlot", "afterSha256"], `${label}.protectedState`);
  if (protectedState.schemaVersion !== 1
    || protectedState.addressSetSha256 !== expectedAddressSetSha256
    || protectedState.beforeContextSlot !== beforeValue.contextSlot
    || protectedState.beforeSha256 !== beforeValue.stateSha256
    || protectedState.afterContextSlot !== afterValue.contextSlot
    || protectedState.afterSha256 !== afterValue.stateSha256) {
    throw new Error(`${label} legacy protected-state envelope does not match independently recomputed snapshot evidence`);
  }
  return { before: beforeValue, after: afterValue };
}

function validatedProtectedTransactionEvidence(
  output: JsonRecord,
  persisted: JsonRecord,
  input: Readonly<{
    label: string;
    lifecycleId: string;
    operation: string;
    expectedSigner: string;
    expectedSignature: string;
    messageSha256: string;
    serializedTransactionSha256: string;
    intentSha256: string;
    intentProtectedPrestateSha256: string;
    confirmedSlot: number;
  }>,
): ValidatedProtectedTransactionEvidence {
  const snapshots = validatedProtectedSnapshotEnvelope(output.protectedSnapshotEvidence, output.protectedState, input.label);
  if (snapshots.before.contextSlot > input.confirmedSlot || snapshots.after.contextSlot < input.confirmedSlot) {
    throw new Error(`${input.label} protected snapshot contexts do not bracket the confirmed transaction slot`);
  }
  if (snapshots.before.stateSha256 !== input.intentProtectedPrestateSha256) {
    throw new Error(`${input.label} protected prestate bytes do not match the signed execution intent`);
  }

  const persistedEnvelope = record(persisted.protectedSnapshotEvidence, `${input.label}.persistedIntent.protectedSnapshotEvidence`);
  exactKeys(persistedEnvelope, ["before"], `${input.label}.persistedIntent.protectedSnapshotEvidence`);
  const persistedBefore = persistedEnvelope.before;
  const persistedPrestateEvidence = persisted.protectedPrestateEvidence;
  assertProtectedSnapshotEvidence(persistedBefore);
  assertProtectedSnapshotEvidence(persistedPrestateEvidence);
  if (canonicalJson(persistedBefore) !== canonicalJson(snapshots.before)
    || canonicalJson(persistedPrestateEvidence) !== canonicalJson(snapshots.before)) {
    throw new Error(`${input.label} persisted pre-send account bytes differ from the confirmed output prestate evidence`);
  }

  const preSendAttestationValue = output.preSendAttestation;
  const settlementAttestationValue = output.settlementAttestation;
  const persistedPreSendAttestationValue = persisted.preSendAttestation;
  assertProtectedPreSendAttestation(preSendAttestationValue);
  assertProtectedSettlementAttestation(settlementAttestationValue);
  assertProtectedPreSendAttestation(persistedPreSendAttestationValue);
  if (canonicalJson(preSendAttestationValue) !== canonicalJson(persistedPreSendAttestationValue)) {
    throw new Error(`${input.label} pre-send attestation differs from the persist-before-send file`);
  }
  if (preSendAttestationValue.signer !== input.expectedSigner || settlementAttestationValue.signer !== input.expectedSigner) {
    throw new Error(`${input.label} protected attestations are not declared by the fixed transaction signer`);
  }
  const pre = preSendAttestationValue.payload;
  const settlement = settlementAttestationValue.payload;
  const preExact = pre.lifecycleId === input.lifecycleId
    && pre.operation === input.operation
    && pre.expectedSignature === input.expectedSignature
    && pre.messageSha256 === input.messageSha256
    && pre.intentSha256 === input.intentSha256
    && pre.addressSetSha256 === snapshots.before.addressSetSha256
    && pre.preContextSlot === snapshots.before.contextSlot
    && pre.preStateSha256 === snapshots.before.stateSha256;
  const settlementExact = settlement.lifecycleId === input.lifecycleId
    && settlement.operation === input.operation
    && settlement.expectedSignature === input.expectedSignature
    && settlement.confirmedSignature === input.expectedSignature
    && settlement.messageSha256 === input.messageSha256
    && settlement.serializedTransactionSha256 === input.serializedTransactionSha256
    && settlement.intentSha256 === input.intentSha256
    && settlement.addressSetSha256 === snapshots.after.addressSetSha256
    && settlement.preAttestationSha256 === preSendAttestationValue.attestationSha256
    && settlement.preSignatureSha256 === preSendAttestationValue.signatureSha256
    && settlement.confirmedSlot === input.confirmedSlot
    && settlement.postContextSlot === snapshots.after.contextSlot
    && settlement.postStateSha256 === snapshots.after.stateSha256;
  if (!preExact || !settlementExact) {
    throw new Error(`${input.label} protected attestations do not bind the exact lifecycle/wire/snapshots/confirmed slot`);
  }
  return {
    ...snapshots,
    preSendAttestation: preSendAttestationValue,
    settlementAttestation: settlementAttestationValue,
  };
}

function confirmedSettlement(value: JsonRecord, label: string): JsonRecord {
  const hasConfirmed = value.confirmed !== undefined;
  const hasFinalized = value.finalized !== undefined;
  if (hasConfirmed === hasFinalized) throw new Error(`${label} must contain exactly one confirmed settlement envelope (.confirmed or legacy .finalized)`);
  const settlement = record(hasConfirmed ? value.confirmed : value.finalized, `${label}.${hasConfirmed ? "confirmed" : "finalized"}`);
  if (settlement.settlementCommitment !== "confirmed") throw new Error(`${label} settlement commitment must be confirmed`);
  return settlement;
}

function stringField(value: JsonRecord, key: string, label: string): string {
  const result = value[key];
  if (typeof result !== "string" || result.length === 0) throw new Error(`${label}.${key} must be a non-empty string`);
  return result;
}

function shaField(value: JsonRecord, key: string, label: string): string {
  const result = stringField(value, key, label);
  if (!/^[0-9a-f]{64}$/.test(result)) throw new Error(`${label}.${key} must be a lowercase SHA-256 digest`);
  return result;
}

function requestOrigin(value: unknown, label: string): RequestOrigin {
  const root = record(value, label);
  exactKeys(root, ["signature", "eventIndex", "receipt", "rawAccountSha256", "generationFingerprint"], label);
  const eventIndex = root.eventIndex;
  if (typeof eventIndex !== "number" || !Number.isSafeInteger(eventIndex) || eventIndex < 0) {
    throw new Error(`${label}.eventIndex must be a non-negative safe integer`);
  }
  return {
    signature: stringField(root, "signature", label),
    eventIndex,
    receipt: stringField(root, "receipt", label),
    rawAccountSha256: shaField(root, "rawAccountSha256", label),
    generationFingerprint: shaField(root, "generationFingerprint", label),
  };
}

function userRuntimeIntent(value: unknown, label: string): ExecutionIntent {
  const root = record(value, label);
  exactKeys(root, ["schemaVersion", "kind", "operation", "signerRole", "user", "amountRaw", "lifecycleId", "protectedPrestateSha256", "routeId", "routeSpecSha256", "nonce", "prestateSlot", "expiresAtUnix", "canonicalMessageSha256"], label);
  const amountRaw = integerString(root.amountRaw, `${label}.amountRaw`);
  const prestateSlot = integerString(root.prestateSlot, `${label}.prestateSlot`);
  const expiresAtUnix = integerString(root.expiresAtUnix, `${label}.expiresAtUnix`);
  if (root.schemaVersion !== 1 || root.kind !== "runtime" || root.signerRole !== "user" || typeof root.operation !== "string" || root.operation.length === 0) throw new Error(`${label} is not a runtime user intent`);
  return {
    schemaVersion: 1,
    kind: "runtime",
    operation: root.operation as "user-deposit" | "instant-withdraw" | "withdraw-request" | "withdraw-claim",
    signerRole: "user",
    user: address(stringField(root, "user", label)),
    amountRaw,
    lifecycleId: shaField(root, "lifecycleId", label),
    protectedPrestateSha256: shaField(root, "protectedPrestateSha256", label),
    routeId: stringField(root, "routeId", label),
    routeSpecSha256: shaField(root, "routeSpecSha256", label),
    nonce: stringField(root, "nonce", label),
    prestateSlot,
    expiresAtUnix,
    canonicalMessageSha256: shaField(root, "canonicalMessageSha256", label),
  } as unknown as ExecutionIntent;
}

function sameRequestOrigin(left: RequestOrigin, right: RequestOrigin): boolean {
  return left.signature === right.signature
    && left.eventIndex === right.eventIndex
    && left.receipt === right.receipt
    && left.rawAccountSha256 === right.rawAccountSha256
    && left.generationFingerprint === right.generationFingerprint;
}

function scannerQueryProof(value: JsonRecord, label: string): Readonly<{ rawQuerySha256: string; queryConfigSha256: string }> {
  const rawQuery = record(value.rawQuery, `${label}.rawQuery`);
  exactKeys(rawQuery, ["method", "params"], `${label}.rawQuery`);
  if (rawQuery.method !== "getProgramAccounts" || !Array.isArray(rawQuery.params) || rawQuery.params.length !== 2 || rawQuery.params[0] !== PARTNER_ROUTE.programs.voltrVault) {
    throw new Error(`${label}.rawQuery must be the exact confirmed Voltr getProgramAccounts call`);
  }
  const queryConfig = record(rawQuery.params[1], `${label}.rawQuery.params[1]`);
  const configKeys = Object.keys(queryConfig);
  const allowed = ["commitment", "encoding", "withContext", "filters", "minContextSlot"];
  if (configKeys.some((key) => !allowed.includes(key)) || !configKeys.includes("commitment") || !configKeys.includes("encoding") || !configKeys.includes("withContext") || !configKeys.includes("filters")) {
    throw new Error(`${label}.rawQuery.params[1] has an unexpected confirmed query-config shape`);
  }
  if (queryConfig.commitment !== "confirmed" || queryConfig.encoding !== "base64" || queryConfig.withContext !== true) {
    throw new Error(`${label}.rawQuery query-config must use confirmed/base64/withContext`);
  }
  if ("minContextSlot" in queryConfig && (typeof queryConfig.minContextSlot !== "number" || !Number.isSafeInteger(queryConfig.minContextSlot) || queryConfig.minContextSlot <= 0)) {
    throw new Error(`${label}.rawQuery minContextSlot is malformed`);
  }
  const filters = queryConfig.filters;
  const discriminator = bs58.encode(Buffer.from(getRequestWithdrawVaultReceiptDiscriminatorBytes()));
  // The receipt discriminator is checked below against the request scanner's
  // actual artifact; keeping the filter shape exact here prevents a broad
  // program-account query from being presented as the authoritative scan.
  if (!Array.isArray(filters) || filters.length !== 2 || canonicalJson(filters[0]) !== canonicalJson({ memcmp: { offset: 0, bytes: discriminator } }) || canonicalJson(filters[1]) !== canonicalJson({ memcmp: { offset: 8, bytes: PARTNER_ROUTE.vault } })) {
    throw new Error(`${label}.rawQuery filters are not the exact Voltr receipt discriminator/vault filters`);
  }
  const queryConfigSha256 = shaField(value, "queryConfigSha256", label);
  const rawQuerySha256 = shaField(value, "rawQuerySha256", label);
  if (queryConfigSha256 !== sha256(canonicalJson(queryConfig)) || rawQuerySha256 !== sha256(canonicalJson(rawQuery))) {
    throw new Error(`${label} raw query/query-config SHA-256 does not match its canonical bytes`);
  }
  return { rawQuerySha256, queryConfigSha256 };
}

function stringMap(value: unknown, label: string): Readonly<Record<string, string>> {
  const root = record(value, label);
  return Object.fromEntries(Object.entries(root).map(([key, raw]) => {
    if (typeof raw !== "string" || !/^-?[0-9]+$/.test(raw)) throw new Error(`${label}.${key} must be an integer string`);
    return [key, raw];
  }));
}

function stringArray(value: unknown, label: string): readonly string[] {
  if (!Array.isArray(value) || value.some((item) => typeof item !== "string" || item.length === 0)) throw new Error(`${label} must be an array of strings`);
  return value;
}

function ref(value: unknown, label: string, extra: readonly string[] = []): ArtifactRef & JsonRecord {
  const root = record(value, label);
  exactKeys(root, ["path", "fileSha256", ...extra], label);
  return { ...root, path: stringField(root, "path", label), fileSha256: shaField(root, "fileSha256", label) };
}

function tx(value: unknown, label: string): TxEvidence {
  const root = ref(value, label, ["signature", "intentSha256", "messageSha256", "slot", "protectedAddressSetSha256", "protectedPrestateSha256", "protectedPoststateSha256", "protectedBeforeContextSlot", "protectedAfterContextSlot", "protectedPreAttestationSha256", "protectedSettlementAttestationSha256"]);
  const slot = root.slot;
  if (typeof slot !== "number" || !Number.isSafeInteger(slot) || slot <= 0) throw new Error(`${label}.slot must be a positive slot`);
  const signature = stringField(root, "signature", label);
  if (signature.length < 80 || signature.length > 90) throw new Error(`${label}.signature is malformed`);
  const protectedBeforeContextSlot = root.protectedBeforeContextSlot;
  const protectedAfterContextSlot = root.protectedAfterContextSlot;
  if (typeof protectedBeforeContextSlot !== "number" || !Number.isSafeInteger(protectedBeforeContextSlot) || protectedBeforeContextSlot <= 0 || typeof protectedAfterContextSlot !== "number" || !Number.isSafeInteger(protectedAfterContextSlot) || protectedAfterContextSlot < slot || protectedAfterContextSlot < protectedBeforeContextSlot) throw new Error(`${label} protected context slots are malformed`);
  return {
    path: root.path,
    fileSha256: root.fileSha256,
    signature,
    intentSha256: shaField(root, "intentSha256", label),
    messageSha256: shaField(root, "messageSha256", label),
    slot,
    protectedAddressSetSha256: shaField(root, "protectedAddressSetSha256", label),
    protectedPrestateSha256: shaField(root, "protectedPrestateSha256", label),
    protectedPoststateSha256: shaField(root, "protectedPoststateSha256", label),
    protectedBeforeContextSlot,
    protectedAfterContextSlot,
    protectedPreAttestationSha256: shaField(root, "protectedPreAttestationSha256", label),
    protectedSettlementAttestationSha256: shaField(root, "protectedSettlementAttestationSha256", label),
  };
}

function parseManifest(path: string): FourMarketManifest {
  const root = record(JSON.parse(readFileSync(path, "utf8")), "four-market lifecycle manifest");
  exactKeys(root, ["schemaVersion", "evidenceType", "commitment", "routeId", "routeSpecSha256", "lifecycleId", "routeAuthorizationSha256", "requestOrigin", "amounts", "identities", "strategies", "policyCatalog", "policyAuthorization", "transactions", "artifacts"], "four-market lifecycle manifest");
  if (root.schemaVersion !== 1 || root.evidenceType !== "backyard-voltr-four-market-confirmed-lifecycle" || root.commitment !== "confirmed") throw new Error("manifest must be schema v1 confirmed four-market lifecycle evidence");
  const identities = record(root.identities, "identities");
  exactKeys(identities, ["vault", "lpMint", "settings", "manager", "guardian", "user", "assetMint"], "identities");
  const parsedIdentities = Object.fromEntries(["vault", "lpMint", "settings", "manager", "guardian", "user", "assetMint"].map((key) => [key, stringField(identities, key, "identities")])) as FourMarketManifest["identities"];
  for (const [key, value] of Object.entries(parsedIdentities)) {
    try { new PublicKey(value); }
    catch { throw new Error(`identities.${key} is not a valid Solana public key`); }
  }
  if (stringField(root, "routeId", "manifest") !== PARTNER_FOUR_MARKET_ROUTE.id || shaField(root, "routeSpecSha256", "manifest") !== fourMarketRouteSpecSha256()) throw new Error("manifest route binding does not match the immutable four-market RouteSpec");
  const routeAuthorizationSha256 = shaField(root, "routeAuthorizationSha256", "manifest");
  const parsedRequestOrigin = requestOrigin(root.requestOrigin, "manifest.requestOrigin");
  const amountsRoot = record(root.amounts, "amounts");
  exactKeys(amountsRoot, ["userDepositAssetRaw", "managerAssetRaw", "requestWithdrawLpRaw", "restorationAssetRaw"], "amounts");
  const userDepositAssetRaw = BigInt(stringField(amountsRoot, "userDepositAssetRaw", "amounts"));
  const managerAssetRaw = BigInt(stringField(amountsRoot, "managerAssetRaw", "amounts"));
  const requestWithdrawLpRaw = BigInt(stringField(amountsRoot, "requestWithdrawLpRaw", "amounts"));
  const restorationAssetRaw = BigInt(stringField(amountsRoot, "restorationAssetRaw", "amounts"));
  if (userDepositAssetRaw <= 0n || userDepositAssetRaw > PARTNER_ROUTE.asset.vaultCapRaw || managerAssetRaw <= 0n || managerAssetRaw > PARTNER_ROUTE.asset.maxManagerOperationRaw || requestWithdrawLpRaw <= 0n || new Set([userDepositAssetRaw, managerAssetRaw, requestWithdrawLpRaw].map(String)).size !== 3) throw new Error("manifest requires three distinct positive proof amounts inside the exact vault/manager bounds");
  if (restorationAssetRaw <= 0n || restorationAssetRaw > PARTNER_ROUTE.asset.maxManagerOperationRaw) throw new Error("manifest restoration amount must be positive and inside the manager cap");
  if (!Array.isArray(root.strategies) || root.strategies.length !== 4) throw new Error("strategies must contain exactly four entries");
  const strategies = root.strategies.map((value, index) => {
    const item = record(value, `strategies[${index}]`);
    exactKeys(item, ["id", "reserve", "strategyReceipt", "strategyAssetAta"], `strategies[${index}]`);
    const id = stringField(item, "id", `strategies[${index}]`) as StrategyId;
    if (!REQUIRED_STRATEGIES.includes(id)) throw new Error(`unsupported strategy id ${id}`);
    return { id, reserve: stringField(item, "reserve", `strategies[${index}]`), strategyReceipt: stringField(item, "strategyReceipt", `strategies[${index}]`), strategyAssetAta: stringField(item, "strategyAssetAta", `strategies[${index}]`) };
  });
  if (new Set(strategies.map(({ id }) => id)).size !== 4 || strategies.some(({ id }, index) => id !== REQUIRED_STRATEGIES[index])) throw new Error("strategies must be ordered main,onre,prime,maple with no duplicates");
  const policyCatalog = ref(root.policyCatalog, "policyCatalog", ["artifactSha256"]) as ArtifactRef & { artifactSha256: string };
  const policyAuthorization = ref(root.policyAuthorization, "policyAuthorization", ["authorizationSha256"]) as ArtifactRef & { authorizationSha256: string };
  const transactionsRoot = record(root.transactions, "transactions");
  exactKeys(transactionsRoot, REQUIRED_TXS, "transactions");
  const transactions = Object.fromEntries(REQUIRED_TXS.map((name) => [name, tx(transactionsRoot[name], `transactions.${name}`)])) as Record<TxName, TxEvidence>;
  const artifactsRoot = record(root.artifacts, "artifacts");
  exactKeys(artifactsRoot, REQUIRED_ARTIFACTS, "artifacts");
  const artifacts = Object.fromEntries(REQUIRED_ARTIFACTS.map((name) => [name, ref(artifactsRoot[name], `artifacts.${name}`)])) as Record<ArtifactName, ArtifactRef>;
  const lifecycleId = shaField(root, "lifecycleId", "manifest");
  const addressSetHashes = REQUIRED_TXS.map((name) => transactions[name].protectedAddressSetSha256);
  if (new Set(addressSetHashes).size !== 1 || addressSetHashes[0] !== fourMarketProtectedAddressSetSha256()) throw new Error("all lifecycle transactions must use the exact maintained protected address set");
  for (let index = 1; index < REQUIRED_TXS.length; index += 1) {
    const previous = transactions[REQUIRED_TXS[index - 1]!]!;
    const current = transactions[REQUIRED_TXS[index]!]!;
    if (previous.protectedPoststateSha256 !== current.protectedPrestateSha256 || previous.protectedAfterContextSlot > current.protectedBeforeContextSlot) throw new Error(`protected state chain breaks before ${REQUIRED_TXS[index]}`);
  }
  return { schemaVersion: 1, evidenceType: "backyard-voltr-four-market-confirmed-lifecycle", commitment: "confirmed", routeId: stringField(root, "routeId", "manifest"), routeSpecSha256: shaField(root, "routeSpecSha256", "manifest"), lifecycleId, routeAuthorizationSha256, requestOrigin: parsedRequestOrigin, amounts: { userDepositAssetRaw, managerAssetRaw, requestWithdrawLpRaw, restorationAssetRaw }, identities: parsedIdentities, strategies, policyCatalog, policyAuthorization, transactions, artifacts };
}

function add(gates: Gate[], name: string, pass: boolean, observed: unknown, expected: unknown, nextExperiment: string): void {
  gates.push({ name: `${name}; next=${nextExperiment}`, pass, observed, expected });
}

function resolveChild(manifestPath: string, child: string): string {
  if (isAbsolute(child)) throw new Error(`evidence path must be relative: ${child}`);
  const root = dirname(manifestPath);
  const absolute = resolve(root, child);
  const lexicalRelative = relative(root, absolute);
  if (lexicalRelative.length === 0 || lexicalRelative === ".." || lexicalRelative.startsWith("../") || lexicalRelative.startsWith("/")) throw new Error(`evidence path escapes manifest directory: ${child}`);
  if (lstatSync(absolute).isSymbolicLink()) throw new Error(`symlinked evidence path is not accepted: ${child}`);
  const canonicalRoot = realpathSync(root);
  const canonicalPath = realpathSync(absolute);
  const canonicalRelative = relative(canonicalRoot, canonicalPath);
  if (canonicalRelative.length === 0 || canonicalRelative === ".." || canonicalRelative.startsWith("../") || canonicalRelative.startsWith("/")) throw new Error(`evidence realpath escapes manifest directory: ${child}`);
  return absolute;
}

function verifyRef(manifestPath: string, item: ArtifactRef, label: string, gates: Gate[]): string | null {
  try {
    const path = resolveChild(manifestPath, item.path);
    if (lstatSync(path).isSymbolicLink()) throw new Error("symlinked evidence file is not accepted");
    const canonicalRoot = realpathSync(dirname(manifestPath));
    const canonicalPath = realpathSync(path);
    const canonicalRelative = relative(canonicalRoot, canonicalPath);
    if (canonicalRelative.length === 0 || canonicalRelative === ".." || canonicalRelative.startsWith("../") || canonicalRelative.startsWith("/")) throw new Error("evidence file realpath escapes manifest directory");
    const bytes = readFileSync(path);
    const actual = sha256(bytes);
    add(gates, `${label} artifact hash`, actual === item.fileSha256, actual, item.fileSha256, `regenerate ${label} with the exact confirmed route hash`);
    return bytes.toString("utf8");
  } catch (error) {
    add(gates, `${label} artifact readable`, false, error instanceof Error ? error.message : String(error), item.path, `produce ${label} evidence before rerunning this verifier`);
    return null;
  }
}

function sameNumbers(left: readonly number[], right: readonly number[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function policyOmittedIndexInventory(loaded: ReturnType<typeof loadRuntimePolicyArtifact>): readonly Readonly<{
  strategyId: string;
  operation: string;
  accountCount: number;
  constrained: readonly number[];
  omitted: readonly number[];
  omittedAccounts: readonly Readonly<{ index: number; address: string; signer: boolean; writable: boolean }>[];
}>[] {
  return loaded.artifact.policies.map((entry) => {
    const manifest = loaded.artifact.sourceManifests?.find((candidate) => candidate.strategyId === entry.strategyId);
    const innerAccounts = manifest?.instructions[entry.operation].accounts;
    const accountCount = innerAccounts?.length ?? 0;
    if (!innerAccounts || accountCount <= 0) throw new Error(`${entry.strategyId} ${entry.operation} source manifest has no inner account vector`);
    const expectedConstrained = entry.operation === "deposit" ? DEPOSIT_CONSTRAINED_INDEXES : WITHDRAW_CONSTRAINED_INDEXES;
    if (!sameNumbers(entry.constrainedAccountIndexes, expectedConstrained)) throw new Error(`${entry.strategyId} ${entry.operation} constrained indexes differ from the maintained policy contract`);
    if (expectedConstrained.some((index) => index < 0 || index >= accountCount)) throw new Error(`${entry.strategyId} ${entry.operation} constrained index escapes its inner account vector`);
    const constrained = new Set<number>(expectedConstrained);
    const omitted = Array.from({ length: accountCount }, (_, index) => index).filter((index) => !constrained.has(index));
    return {
      strategyId: entry.strategyId ?? "<missing>",
      operation: entry.operation,
      accountCount,
      constrained: [...expectedConstrained],
      omitted,
      omittedAccounts: omitted.map((index) => ({ index, address: innerAccounts[index]!.address, signer: innerAccounts[index]!.signer, writable: innerAccounts[index]!.writable })),
    };
  });
}

function verifyPersistedSignedIntent(
  manifestPath: string,
  output: JsonRecord,
  name: TxName,
  expectedSignature: string,
  gates: Gate[],
): VersionedTransaction | null {
  const intentPathValue = output.intentPath;
  const intentFileShaValue = output.intentFileSha256;
  if (typeof intentPathValue !== "string" || typeof intentFileShaValue !== "string") {
    add(gates, `${name} persisted signed intent present`, false, { intentPath: intentPathValue ?? null, intentFileSha256: intentFileShaValue ?? null }, "intentPath + intentFileSha256", "persist the exact signed wire before sending and include its path/hash in command output");
    return null;
  }
  const intentPath = resolve(intentPathValue);
  const intentRoot = resolve(dirname(manifestPath), "intents");
  const intentRelative = relative(intentRoot, intentPath);
  const pathInsideRoot = intentRelative.length > 0 && intentRelative !== ".." && !intentRelative.startsWith("../") && !intentRelative.startsWith("/" );
  add(gates, `${name} persisted intent path is inside evidence intents`, pathInsideRoot, intentPath, intentRoot, "keep signed intent files under the maintained four-market intents directory");
  if (!pathInsideRoot) return null;
  try {
    if (lstatSync(intentPath).isSymbolicLink() || lstatSync(intentRoot).isSymbolicLink()) throw new Error("symlinked persisted intent evidence is not accepted");
    const canonicalRoot = realpathSync(intentRoot);
    const canonicalPath = realpathSync(intentPath);
    const canonicalRelative = relative(canonicalRoot, canonicalPath);
    if (canonicalRelative.length === 0 || canonicalRelative === ".." || canonicalRelative.startsWith("../") || canonicalRelative.startsWith("/")) throw new Error("persisted intent realpath escapes evidence intents");
  } catch (error) {
    add(gates, `${name} persisted intent realpath is inside evidence intents`, false, error instanceof Error ? error.message : String(error), intentRoot, "retain non-symlink signed intent evidence inside the maintained intents directory");
    return null;
  }
  let bytes: Buffer;
  try {
    bytes = readFileSync(intentPath);
  } catch (error) {
    add(gates, `${name} persisted signed intent readable`, false, error instanceof Error ? error.message : String(error), intentPath, "retain the pre-send signed intent artifact");
    return null;
  }
  add(gates, `${name} persisted intent file hash`, sha256(bytes) === intentFileShaValue, sha256(bytes), intentFileShaValue, "do not alter the persisted signed intent after send");
  let intent: JsonRecord;
  try {
    intent = record(JSON.parse(bytes.toString("utf8")), `${name}.persistedIntent`);
  } catch (error) {
    add(gates, `${name} persisted signed intent JSON`, false, error instanceof Error ? error.message : String(error), "valid JSON", "write a schema-valid persisted signed intent");
    return null;
  }
  const wireBase64 = stringField(intent, "serializedTransactionBase64", `${name}.persistedIntent`);
  const wire = Buffer.from(wireBase64, "base64");
  const canonicalBase64 = wire.toString("base64");
  const wireSha256 = sha256(wire);
  const expectedWireSha256 = shaField(intent, "serializedTransactionSha256", `${name}.persistedIntent`);
  add(gates, `${name} persisted wire base64 canonical`, canonicalBase64 === wireBase64, canonicalBase64, wireBase64, "persist the canonical base64 encoding of the signed wire");
  add(gates, `${name} persisted wire hash`, wireSha256 === expectedWireSha256, wireSha256, expectedWireSha256, "bind the signed wire hash to its persisted bytes");
  let transaction: VersionedTransaction;
  try {
    transaction = VersionedTransaction.deserialize(wire);
  } catch (error) {
    add(gates, `${name} persisted signed wire decodes`, false, error instanceof Error ? error.message : String(error), "VersionedTransaction", "persist the exact Solana versioned transaction bytes");
    return null;
  }
  const packetBytes = intent.packetBytes;
  add(gates, `${name} persisted packet length exact`, typeof packetBytes === "number" && packetBytes === wire.length, wire.length, packetBytes ?? null, "bind packetBytes to the exact persisted wire length");
  const actualSignature = transaction.signatures.length === 1 ? bs58.encode(transaction.signatures[0]!) : null;
  add(gates, `${name} persisted wire signature exact`, actualSignature === intent.expectedSignature && actualSignature === expectedSignature, { persisted: actualSignature, intent: intent.expectedSignature, output: expectedSignature }, "the confirmed transaction signature", "do not substitute a different signed packet");
  const messageSha256 = sha256(transaction.message.serialize());
  const expectedMessageSha256 = typeof intent.serializedMessageSha256 === "string"
    ? intent.serializedMessageSha256
    : typeof intent.canonicalMessageSha256 === "string" ? intent.canonicalMessageSha256 : null;
  add(gates, `${name} persisted message hash exact`, expectedMessageSha256 !== null && messageSha256 === expectedMessageSha256, messageSha256, expectedMessageSha256, "bind the signed wire to the canonical message");
  return transaction;
}

function tokenDelta(response: VersionedTransactionResponse, address: string): bigint | null {
  if (!response.meta) return null;
  const keys = [...response.transaction.message.staticAccountKeys, ...(response.meta.loadedAddresses?.writable ?? []), ...(response.meta.loadedAddresses?.readonly ?? [])].map((key) => key.toBase58());
  const pre = response.meta.preTokenBalances?.find((row) => keys[row.accountIndex] === address);
  const post = response.meta.postTokenBalances?.find((row) => keys[row.accountIndex] === address);
  return BigInt(post?.uiTokenAmount.amount ?? "0") - BigInt(pre?.uiTokenAmount.amount ?? "0");
}

function tokenPostAmount(response: VersionedTransactionResponse | null, account: string): bigint | null {
  if (!response?.meta) return null;
  const keys = transactionKeys(response);
  const row = response.meta.postTokenBalances?.find((candidate) => keys[candidate.accountIndex] === account);
  return row ? BigInt(row.uiTokenAmount.amount) : null;
}

function tokenPreAmount(response: VersionedTransactionResponse | null, account: string): bigint | null {
  if (!response?.meta) return null;
  const keys = transactionKeys(response);
  const row = response.meta.preTokenBalances?.find((candidate) => keys[candidate.accountIndex] === account);
  return row ? BigInt(row.uiTokenAmount.amount) : null;
}

function lamportDelta(response: VersionedTransactionResponse, address: string): bigint | null {
  if (!response.meta) return null;
  const keys = [...response.transaction.message.staticAccountKeys, ...(response.meta.loadedAddresses?.writable ?? []), ...(response.meta.loadedAddresses?.readonly ?? [])].map((key) => key.toBase58());
  const index = keys.indexOf(address);
  return index < 0 ? null : BigInt(response.meta.postBalances[index]!) - BigInt(response.meta.preBalances[index]!);
}

type ExpectedMeta = Readonly<{ index: number; label: string | null; address: string; signer: boolean; writable: boolean }>;
type ExpectedInstruction = Readonly<{ programId: string; data: Uint8Array; accounts: readonly ExpectedMeta[] }>;
type ExpectedTx = Readonly<{
  operation: "user-deposit" | "manager-deposit" | "manager-withdraw" | "instant-withdraw" | "withdraw-request" | "withdraw-claim";
  amountRaw: bigint;
  policy: string | null;
  strategyId: StrategyId | null;
  signer: string;
  requiredProgram: string;
  allowedPrograms: readonly string[];
  instruction: ExpectedInstruction;
  auxiliaryInstructions: readonly ExpectedInstruction[];
  orderedInstructions: readonly ExpectedInstruction[];
  lookupTable: AddressLookupTableAccount | null;
  allowedTokenAccounts: readonly string[];
  allowedMints: readonly string[];
  reserveLiquidity: string;
  reserveCollateral: string;
  strategyAssetAta: string;
  strategyAuth: string;
  obligation: string;
  allowedLamportAccounts: readonly string[];
  instructionCount: number;
}>;

function transactionKeys(response: VersionedTransactionResponse): string[] {
  return [...response.transaction.message.staticAccountKeys, ...(response.meta?.loadedAddresses?.writable ?? []), ...(response.meta?.loadedAddresses?.readonly ?? [])].map((key) => key.toBase58());
}

function tokenRows(response: VersionedTransactionResponse): readonly Readonly<{ address: string; mint: string; delta: bigint }>[] {
  if (!response.meta) return [];
  const keys = transactionKeys(response);
  const rows = new Map<string, { mint: string; pre: bigint; post: bigint }>();
  for (const row of response.meta.preTokenBalances ?? []) rows.set(`${row.accountIndex}:${row.mint}`, { mint: row.mint, pre: BigInt(row.uiTokenAmount.amount), post: 0n });
  for (const row of response.meta.postTokenBalances ?? []) {
    const key = `${row.accountIndex}:${row.mint}`;
    const old = rows.get(key) ?? { mint: row.mint, pre: 0n, post: 0n };
    old.post = BigInt(row.uiTokenAmount.amount);
    rows.set(key, old);
  }
  return [...rows.entries()].map(([key, value]) => ({ address: keys[Number(key.split(":", 1)[0])] ?? "<missing>", mint: value.mint, delta: value.post - value.pre }));
}

function expectedInstructionFromCanonical(instruction: CanonicalInstruction): ExpectedInstruction {
  return { programId: instruction.programId, data: instruction.data, accounts: instruction.accounts.map(({ index, label, address: account, signer, writable }) => ({ index, label, address: account, signer, writable })) };
}

function expectedInstructionFromKit(instruction: { programAddress: string; data?: ArrayLike<number>; accounts?: readonly { address: string; role: Parameters<typeof isSignerRole>[0] }[] }): ExpectedInstruction {
  return { programId: instruction.programAddress, data: new Uint8Array(instruction.data ?? []), accounts: (instruction.accounts ?? []).map(({ address: account, role }, index) => ({ index, label: null, address: account, signer: isSignerRole(role), writable: isWritableRole(role) })) };
}

function expectedInstructionFromWeb3(instruction: { programId: PublicKey; data: Uint8Array; keys: readonly { pubkey: PublicKey; isSigner: boolean; isWritable: boolean }[] }): ExpectedInstruction {
  return { programId: instruction.programId.toBase58(), data: instruction.data, accounts: instruction.keys.map(({ pubkey, isSigner, isWritable }, index) => ({ index, label: null, address: pubkey.toBase58(), signer: isSigner, writable: isWritable })) };
}

function reportedCanonicalInstruction(raw: unknown, label: string): ExpectedInstruction {
  const instruction = record(raw, label);
  exactKeys(instruction, ["programId", "data", "dataBase64", "dataSha256", "dataLength", "accounts"], label);
  const dataBase64 = stringField(instruction, "dataBase64", label);
  if (instruction.data !== dataBase64) throw new Error(`${label}.data must equal dataBase64`);
  const data = Buffer.from(dataBase64, "base64");
  if (data.toString("base64") !== dataBase64 || instruction.dataLength !== data.length || instruction.dataSha256 !== sha256(data)) throw new Error(`${label} data envelope is not canonical`);
  if (!Array.isArray(instruction.accounts)) throw new Error(`${label}.accounts must be an array`);
  const accounts = instruction.accounts.map((rawAccount, index) => {
    const account = record(rawAccount, `${label}.accounts[${index}]`);
    exactKeys(account, ["index", "label", "address", "signer", "writable"], `${label}.accounts[${index}]`);
    if (account.index !== index || typeof account.signer !== "boolean" || typeof account.writable !== "boolean") throw new Error(`${label}.accounts[${index}] role/index is malformed`);
    return { index, label: stringField(account, "label", `${label}.accounts[${index}]`), address: stringField(account, "address", `${label}.accounts[${index}]`), signer: account.signer, writable: account.writable };
  });
  return { programId: stringField(instruction, "programId", label), data, accounts };
}

function canonicalInstructionEqual(left: ExpectedInstruction, right: ExpectedInstruction): boolean {
  return left.programId === right.programId
    && Buffer.from(left.data).equals(Buffer.from(right.data))
    && canonicalJson(left.accounts) === canonicalJson(right.accounts);
}

function compareExpectedInstruction(response: VersionedTransactionResponse, expected: ExpectedInstruction): { pass: boolean; observed: unknown; expected: unknown } {
  const keys = transactionKeys(response);
  const matches = response.transaction.message.compiledInstructions.filter((ix) => keys[ix.programIdIndex] === expected.programId);
  const actual = matches.length === 1 ? matches[0]! : null;
  const actualAccounts = actual?.accountKeyIndexes.map((index) => keys[index] ?? "<missing>") ?? null;
  const actualData = actual ? Uint8Array.from(actual.data) : null;
  return {
    pass: actual !== null && JSON.stringify(actualAccounts) === JSON.stringify(expected.accounts.map(({ address: account }) => account)) && actualData !== null && Buffer.from(actualData).equals(Buffer.from(expected.data)),
    observed: actual ? { accounts: actualAccounts, dataSha256: sha256(actualData!) } : null,
    expected: { programId: expected.programId, accounts: expected.accounts, dataSha256: sha256(expected.data) },
  };
}

function web3Instruction(expected: ExpectedInstruction): TransactionInstruction {
  return new TransactionInstruction({
    programId: new PublicKey(expected.programId),
    keys: expected.accounts.map((meta) => ({ pubkey: new PublicKey(meta.address), isSigner: meta.signer, isWritable: meta.writable })),
    data: Buffer.from(expected.data),
  });
}

function canonicalMessageGate(response: VersionedTransactionResponse, expected: ExpectedTx): Readonly<{ pass: boolean; observed: unknown; expected: unknown }> {
  const actual = response.transaction.message;
  const compiled = new TransactionMessage({
    payerKey: new PublicKey(expected.signer),
    recentBlockhash: actual.recentBlockhash,
    instructions: expected.orderedInstructions.map(web3Instruction),
  }).compileToV0Message(expected.lookupTable ? [expected.lookupTable] : []);
  const actualBytes = Buffer.from(actual.serialize());
  const expectedBytes = Buffer.from(compiled.serialize());
  const lookup = (message: typeof actual) => message.addressTableLookups.map((item) => ({ table: item.accountKey.toBase58(), writableIndexes: [...item.writableIndexes], readonlyIndexes: [...item.readonlyIndexes] }));
  return {
    pass: actualBytes.equals(expectedBytes),
    observed: { messageSha256: sha256(actualBytes), header: actual.header, staticAccountKeys: actual.staticAccountKeys.map((key) => key.toBase58()), lookups: lookup(actual), instructions: actual.compiledInstructions.map((ix) => ({ programIdIndex: ix.programIdIndex, accountKeyIndexes: [...ix.accountKeyIndexes], dataHex: Buffer.from(ix.data).toString("hex") })) },
    expected: { messageSha256: sha256(expectedBytes), header: compiled.header, staticAccountKeys: compiled.staticAccountKeys.map((key) => key.toBase58()), lookups: lookup(compiled), instructions: compiled.compiledInstructions.map((ix) => ({ programIdIndex: ix.programIdIndex, accountKeyIndexes: [...ix.accountKeyIndexes], dataHex: Buffer.from(ix.data).toString("hex") })) },
  };
}

function semanticTokenGate(name: TxName, response: VersionedTransactionResponse, item: FourMarketManifest["amounts"], accounts: Readonly<{ idle: string; userAsset: string; userLp: string; escrow: string; reserveLiquidity: string; reserveCollateral: string; strategyAssetAta: string }>, gates: Gate[]): void {
  const rows = tokenRows(response);
  const delta = (account: string): bigint => rows.find((row) => row.address === account)?.delta ?? 0n;
  const idle = delta(accounts.idle);
  const userAsset = delta(accounts.userAsset);
  const userLp = delta(accounts.userLp);
  const escrow = delta(accounts.escrow);
  const reserve = accounts.reserveLiquidity.length > 0 ? delta(accounts.reserveLiquidity) : 0n;
  const collateral = accounts.reserveCollateral.length > 0 ? delta(accounts.reserveCollateral) : 0n;
  const strategyAsset = accounts.strategyAssetAta.length > 0 ? delta(accounts.strategyAssetAta) : 0n;
  const nonZero = rows.filter(({ delta: value }) => value !== 0n).map(({ address: account, mint, delta: value }) => ({ address: account, mint, delta: value.toString() })).sort((left, right) => left.address.localeCompare(right.address));
  let pass = false;
  let expected: unknown = null;
  const managerAmountRaw = name === "managerMainRestorationWithdraw"
    ? item.restorationAssetRaw
    : item.managerAssetRaw;
  if (name === "userDeposit") {
    const event = eventPayload(response, "DepositVaultEvent");
    const minted = typeof event?.userAmountLpMinted === "bigint" ? event.userAmountLpMinted : null;
    const exactAddresses = new Set([accounts.userAsset, accounts.idle, accounts.userLp]);
    pass = minted !== null && minted > 0n && userAsset === -item.userDepositAssetRaw && idle === item.userDepositAssetRaw && userLp === minted && nonZero.every(({ address: account }) => exactAddresses.has(account));
    expected = { userAsset: -item.userDepositAssetRaw, idle: item.userDepositAssetRaw, userLp: "DepositVaultEvent.userAmountLpMinted", nonZeroAddresses: [...exactAddresses] };
  } else if (name.startsWith("manager") && name.endsWith("Deposit")) {
    const exactAddresses = new Set([accounts.idle, accounts.reserveLiquidity, accounts.reserveCollateral, accounts.strategyAssetAta]);
    pass = idle === -managerAmountRaw && reserve >= 0n && strategyAsset >= 0n && reserve + strategyAsset === managerAmountRaw && collateral > 0n && nonZero.every(({ address: account }) => exactAddresses.has(account));
    expected = { idle: -managerAmountRaw, reservePlusStrategyAsset: managerAmountRaw, reserve: ">=0", collateral: ">0", strategyAsset: ">=0 exact approved ATA dust", nonZeroAddresses: [...exactAddresses] };
  } else if (name.startsWith("manager") && name.endsWith("Withdraw")) {
    const exactAddresses = new Set([accounts.idle, accounts.reserveLiquidity, accounts.reserveCollateral, accounts.strategyAssetAta]);
    pass = idle > 0n && idle >= managerAmountRaw - 1n && reserve <= 0n && strategyAsset <= 0n && idle === -(reserve + strategyAsset) && collateral < 0n && nonZero.every(({ address: account }) => exactAddresses.has(account));
    expected = { idle: `>=${managerAmountRaw - 1n}; accrued yield allowed`, reservePlusStrategyAsset: "-idle", reserve: "<=0", collateral: "<0", strategyAsset: "<=0 exact approved ATA dust", nonZeroAddresses: [...exactAddresses] };
  } else if (name === "withdrawRequest") {
    const exactAddresses = new Set([accounts.userLp, accounts.escrow]);
    pass = userLp === -item.requestWithdrawLpRaw && escrow === item.requestWithdrawLpRaw && userAsset === 0n && idle === 0n && nonZero.every(({ address: account }) => exactAddresses.has(account));
    expected = { userLp: -item.requestWithdrawLpRaw, escrow: item.requestWithdrawLpRaw, userAsset: 0n, idle: 0n, nonZeroAddresses: [...exactAddresses] };
  } else if (name === "withdrawClaim") {
    const event = eventPayload(response, "WithdrawVaultEvent");
    const quote = typeof event?.userAmountAssetWithdrawn === "bigint" ? event.userAmountAssetWithdrawn : null;
    const exactAddresses = new Set([accounts.userAsset, accounts.idle, accounts.escrow]);
    pass = quote !== null && quote > 0n && userAsset === quote && idle === -quote && escrow === -item.requestWithdrawLpRaw && userLp === 0n && nonZero.every(({ address: account }) => exactAddresses.has(account));
    expected = { userAsset: "event quote >0", idle: "-event quote", escrow: -item.requestWithdrawLpRaw, userLp: 0n, nonZeroAddresses: [...exactAddresses] };
  }
  add(gates, `${name} canonical closed token accounting`, pass, { rows, nonZero, deltas: { idle, userAsset, userLp, escrow, reserve, collateral, strategyAsset } }, expected, `reconcile the exact ${name} quote/event/collateral transition and reject every unexplained token row`);
}

function managerStrategyEventGate(name: TxName, response: VersionedTransactionResponse, expected: ExpectedTx, gates: Gate[]): void {
  if (!expected.strategyId || !name.startsWith("manager")) return;
  const deposit = name.endsWith("Deposit");
  const eventName = deposit ? "DepositStrategyEvent" : "WithdrawStrategyEvent";
  const event = eventPayload(response, eventName);
  const strategy = partnerStrategyIdentity(expected.strategyId);
  const eventAmount = event?.[deposit ? "vaultAmountAssetDeposited" : "vaultAmountAssetWithdrawn"];
  const idleEffect = typeof event?.vaultAssetIdleAtaAmountBefore === "bigint" && typeof event.vaultAssetIdleAtaAmountAfter === "bigint"
    ? (deposit ? event.vaultAssetIdleAtaAmountBefore - event.vaultAssetIdleAtaAmountAfter : event.vaultAssetIdleAtaAmountAfter - event.vaultAssetIdleAtaAmountBefore)
    : null;
  const positionEffect = typeof event?.strategyPositionValueBefore === "bigint" && typeof event.strategyPositionValueAfter === "bigint"
    ? (deposit ? event.strategyPositionValueAfter - event.strategyPositionValueBefore : event.strategyPositionValueBefore - event.strategyPositionValueAfter)
    : null;
  const totalValueEffect = typeof event?.vaultAssetTotalValueBefore === "bigint" && typeof event.vaultAssetTotalValueAfter === "bigint"
    ? event.vaultAssetTotalValueAfter - event.vaultAssetTotalValueBefore
    : null;
  const strategyAssetDelta = tokenRows(response).find((row) => row.address === expected.strategyAssetAta)?.delta ?? 0n;
  const amountExact = typeof eventAmount === "bigint" && (deposit ? eventAmount === expected.amountRaw : eventAmount >= expected.amountRaw - 1n && eventAmount <= expected.amountRaw);
  const idleEffectExact = typeof eventAmount === "bigint" && idleEffect !== null && (deposit ? idleEffect === eventAmount : idleEffect > 0n && idleEffect >= eventAmount - 1n);
  const positionEffectExact = positionEffect !== null && positionEffect >= 0n;
  const conservationExact = idleEffect !== null && positionEffect !== null && totalValueEffect === (deposit ? positionEffect + strategyAssetDelta - idleEffect : idleEffect - positionEffect + strategyAssetDelta);
  const pass = event !== null
    && event.manager === PARTNER_ROUTE.squads.manager
    && event.vault === PARTNER_ROUTE.vault
    && event.strategy === strategy.reserve
    && event.strategyInitReceipt === strategy.voltr.strategyInitReceipt
    && event.adaptorProgram === PARTNER_ROUTE.programs.kaminoAdaptor
    && event.vaultAssetMint === PARTNER_ROUTE.asset.mint
    && amountExact
    && idleEffectExact
    && positionEffectExact
    && conservationExact
    && event.vaultLpSupplyInclFeesBefore === event.vaultLpSupplyInclFeesAfter;
  add(gates, `${name} exact Voltr strategy event and requested/actual position effect`, pass, { event, idleEffect, positionEffect, strategyAssetDelta, totalValueEffect }, { eventName, manager: PARTNER_ROUTE.squads.manager, vault: PARTNER_ROUTE.vault, strategy: strategy.reserve, strategyReceipt: strategy.voltr.strategyInitReceipt, adaptorProgram: PARTNER_ROUTE.programs.kaminoAdaptor, requestedAmountRaw: expected.amountRaw, eventAmountRaw: deposit ? expected.amountRaw : `${expected.amountRaw - 1n}..${expected.amountRaw}`, idleEffect: deposit ? "event amount" : ">=event amount-1; accrued yield allowed", positionEffect: ">=0", strategyAssetDelta: "exact approved strategy ATA delta", totalValueEffect: deposit ? "positionEffect + strategyAssetDelta - idleEffect" : "idleEffect - positionEffect + strategyAssetDelta", lpSupply: "unchanged" }, `reconcile the bounded requested amount, actual redeemed/deposited amount, monotonic position effect, and exact conservation from ${eventName}`);
}

function exactLamportGate(name: TxName, response: VersionedTransactionResponse, expected: ExpectedTx, accounts: Readonly<{ userLp: string; escrow: string; receipt: string }>, gates: Gate[]): void {
  if (!response.meta) {
    add(gates, `${name} exact fee/rent/refund accounting`, false, null, "successful transaction metadata", `reload ${name} with non-null confirmed metadata`);
    return;
  }
  const keys = transactionKeys(response);
  const rows = keys.map((account, index) => ({ address: account, delta: BigInt(response.meta!.postBalances[index]!) - BigInt(response.meta!.preBalances[index]!) })).filter(({ delta }) => delta !== 0n);
  const delta = (account: string): bigint => rows.find((row) => row.address === account)?.delta ?? 0n;
  const fee = BigInt(response.meta.fee);
  const payer = delta(expected.signer);
  const sum = rows.reduce((total, row) => total + row.delta, 0n);
  let pass = sum === -fee;
  let equation: unknown = { sum: -fee };
  let exactAllowed = new Set<string>([expected.signer]);
  if (name.startsWith("manager") && name.endsWith("Deposit")) {
    exactAllowed = new Set([expected.signer, expected.strategyAuth, expected.obligation]);
    const strategyAuth = delta(expected.strategyAuth);
    const obligation = delta(expected.obligation);
    pass = pass && payer === -fee && ((strategyAuth === 0n && obligation === 0n) || (strategyAuth < 0n && obligation === -strategyAuth));
    equation = { payer: -fee, strategyAuth: "0 or -new obligation rent", obligation: "0 or +new obligation rent" };
  } else if (name.startsWith("manager")) {
    exactAllowed = new Set([expected.signer, expected.strategyAuth, expected.obligation]);
    const strategyAuth = delta(expected.strategyAuth);
    const obligation = delta(expected.obligation);
    pass = pass
      && payer === -fee
      && ((strategyAuth === 0n && obligation === 0n)
        || (strategyAuth > 0n && obligation === -strategyAuth));
    equation = { payer: -fee, strategyAuth: "0 or +closed obligation rent", obligation: "0 or -closed obligation rent" };
  } else if (name === "userDeposit") {
    exactAllowed = new Set([expected.signer, accounts.userLp]);
    const rent = delta(accounts.userLp);
    pass = pass && rent >= 0n && payer === -(fee + rent);
    equation = { payer: "-(fee + created LP ATA rent)", userLpRent: ">=0" };
  } else if (name === "withdrawRequest") {
    exactAllowed = new Set([expected.signer, accounts.escrow, accounts.receipt]);
    const escrowRent = delta(accounts.escrow);
    const receiptRent = delta(accounts.receipt);
    pass = pass && escrowRent >= 0n && receiptRent > 0n && payer === -(fee + escrowRent + receiptRent);
    equation = { payer: "-(fee + optional escrow rent + receipt rent)", escrowRent: ">=0; zero when the idempotent ATA already exists", receiptRent: ">0" };
  } else if (name === "withdrawClaim") {
    exactAllowed = new Set([expected.signer, accounts.receipt]);
    const receiptRefund = delta(accounts.receipt);
    const escrow = delta(accounts.escrow);
    pass = pass && receiptRefund < 0n && escrow === 0n && payer === -(fee + receiptRefund);
    equation = { payer: "-(fee - receipt refund)", receiptRefund: "<0", escrowRent: 0n };
  }
  pass = pass && rows.every(({ address: account }) => exactAllowed.has(account));
  add(gates, `${name} exact closed fee/rent/refund accounting`, pass, { fee, rows, sum, payer }, { equation, nonZeroAddresses: [...exactAllowed] }, `explain the first non-canonical lamport row or rebuild ${name} with exact rent and fee semantics`);
}

async function verifyCommandOutput(
  manifestPath: string,
  output: JsonRecord,
  name: TxName,
  item: TxEvidence,
  expected: ExpectedTx,
  response: VersionedTransactionResponse,
  expectedLifecycleId: string,
  expectedRouteAuthorizationSha256: string,
  gates: Gate[],
): Promise<void> {
  let settlement: JsonRecord | null = null;
  try { settlement = confirmedSettlement(output, name); }
  catch (error) { add(gates, `${name} command settlement schema`, false, error instanceof Error ? error.message : String(error), "one successful confirmed settlement", `regenerate ${name} from the maintained confirmed command`); }
  if (settlement) {
    add(gates, `${name} command settlement binds chain transaction`, output.broadcast === true && settlement.signature === item.signature && settlement.confirmedSlot === item.slot && settlement.err === null && settlement.settlementCommitment === "confirmed", { broadcast: output.broadcast ?? null, signature: settlement.signature ?? null, slot: settlement.confirmedSlot ?? null, err: settlement.err ?? null, commitment: settlement.settlementCommitment ?? null }, { broadcast: true, signature: item.signature, slot: item.slot, err: null, commitment: "confirmed" }, `retain the exact successful confirmed ${name} command output`);
  }
  const preflight = record(output.preflight, `${name}.preflight`);
  const simulation = record(preflight.simulation, `${name}.preflight.simulation`);
  const transaction = record(preflight.transaction, `${name}.preflight.transaction`);
  const preflightGates = Array.isArray(preflight.gates) ? preflight.gates.map((entry) => record(entry, `${name}.preflight.gate`)) : [];
  const simulationPrestateSlot = simulation.prestateSlot;
  const simulationContextSlot = simulation.contextSlot;
  const preflightExact = preflight.broadcast === false
    && preflight.readyForBroadcast === true
    && preflight.failedGateCount === 0
    && preflightGates.length > 0
    && preflightGates.every((gate) => gate.pass === true)
    && typeof simulationPrestateSlot === "number"
    && Number.isSafeInteger(simulationPrestateSlot)
    && typeof simulationContextSlot === "number"
    && Number.isSafeInteger(simulationContextSlot)
    && simulationContextSlot >= simulationPrestateSlot
    && simulation.err === null
    && typeof simulation.unitsConsumed === "number"
    && simulation.unitsConsumed > 0
    && transaction.operation === expected.operation
    && integerString(transaction.amountRaw ?? transaction.amountLpRaw ?? transaction.amountLpEscrowed ?? transaction.amount, `${name}.preflight.transaction.amount`) === expected.amountRaw
    && transaction.expectedSignature === item.signature
    && transaction.canonicalMessageSha256 === item.messageSha256;
  const readback = record(output.readback, `${name}.readback`);
  const readbackGates = Array.isArray(readback.gates) ? readback.gates.map((entry) => record(entry, `${name}.readback.gate`)) : [];
  const readbackContextSlot = output.readbackContextSlot;
  const readbackExact = readback.failedGateCount === 0 && readbackGates.length > 0 && readbackGates.every((gate) => gate.pass === true) && typeof readbackContextSlot === "number" && Number.isSafeInteger(readbackContextSlot) && readbackContextSlot >= response.slot;
  add(gates, `${name} maintained preflight/simulation/readback contract`, preflightExact && readbackExact, { preflight: { broadcast: preflight.broadcast, readyForBroadcast: preflight.readyForBroadcast, failedGateCount: preflight.failedGateCount, gateCount: preflightGates.length, simulation, transaction }, readback: { failedGateCount: readback.failedGateCount, gateCount: readbackGates.length, contextSlot: readbackContextSlot } }, { preflight: { broadcast: false, readyForBroadcast: true, failedGateCount: 0, allNamedGatesPass: true, simulation: { err: null, contextSlot: ">=prestate", unitsConsumed: ">0" }, transaction: { operation: expected.operation, amountRaw: expected.amountRaw, expectedSignature: item.signature, canonicalMessageSha256: item.messageSha256 } }, readback: { failedGateCount: 0, allNamedGatesPass: true, contextSlot: `>=${response.slot}` } }, `regenerate ${name} through the maintained preflight and make its first failing readback gate pass`);
  const intent = record(output.intent, `${name}.intent`);
  const intentBaseKeys = ["schemaVersion", "kind", "operation", "routeId", "routeSpecSha256", "signerRole", "amountRaw", "nonce", "prestateSlot", "expiresAtUnix", "canonicalMessageSha256", "lifecycleId", "protectedPrestateSha256"] as const;
  exactKeys(intent, expected.policy === null ? [...intentBaseKeys, "user"] : [...intentBaseKeys, "guardian", "policy", "routeAuthorizationSha256"], `${name}.intent`);
  const observedIntentSha = executionIntentSha256(intent as unknown as ExecutionIntent);
  const amount = typeof intent.amountRaw === "string" && /^[0-9]+$/.test(intent.amountRaw) ? BigInt(intent.amountRaw) : null;
  const prestateSlot = typeof intent.prestateSlot === "string" && /^[0-9]+$/.test(intent.prestateSlot) ? BigInt(intent.prestateSlot) : null;
  const expiresAtUnix = typeof intent.expiresAtUnix === "string" && /^[0-9]+$/.test(intent.expiresAtUnix) ? BigInt(intent.expiresAtUnix) : null;
  const identityExact = expected.policy === null
    ? intent.signerRole === "user" && intent.user === expected.signer
    : intent.signerRole === "guardian" && intent.guardian === expected.signer && intent.policy === expected.policy;
  const receipt = expected.instruction.accounts.find(({ label }) => label === "requestWithdrawVaultReceipt")?.address ?? null;
  const expectedNonce = expected.policy !== null
    ? `${expected.operation === "manager-deposit" ? "deposit" : "withdraw"}:${expected.policy}:${item.signature}`
    : name === "withdrawClaim"
      ? `withdraw-claim:${receipt}:post-deadline`
      : `${expected.operation}:${expected.signer}:${expected.amountRaw}`;
  const landedBeforeExpiry = typeof response.blockTime === "number" && expiresAtUnix !== null && BigInt(response.blockTime) <= expiresAtUnix;
  const lifecycleId = typeof intent.lifecycleId === "string" ? intent.lifecycleId : null;
  const protectedPrestateSha256 = typeof intent.protectedPrestateSha256 === "string" ? intent.protectedPrestateSha256 : null;
  const routeAuthorizationSha256 = expected.policy === null ? null : typeof intent.routeAuthorizationSha256 === "string" ? intent.routeAuthorizationSha256 : null;
  const intentBindingExact = lifecycleId === expectedLifecycleId
    && /^[0-9a-f]{64}$/.test(lifecycleId ?? "")
    && protectedPrestateSha256 !== null
    && /^[0-9a-f]{64}$/.test(protectedPrestateSha256)
    && (expected.policy === null || routeAuthorizationSha256 === expectedRouteAuthorizationSha256);
  add(gates, `${name} execution intent exact schema, lifecycle, route authorization, freshness, nonce, and semantic hash`, intent.schemaVersion === 1 && intent.kind === "runtime" && intentBindingExact && output.intentSha256 === observedIntentSha && item.intentSha256 === observedIntentSha && typeof output.intentSha256 === "string" && /^[0-9a-f]{64}$/.test(output.intentSha256) && intent.routeId === PARTNER_FOUR_MARKET_ROUTE.id && intent.routeSpecSha256 === fourMarketRouteSpecSha256() && intent.operation === expected.operation && amount === expected.amountRaw && intent.canonicalMessageSha256 === item.messageSha256 && identityExact && intent.nonce === expectedNonce && prestateSlot !== null && prestateSlot > 0n && prestateSlot <= BigInt(response.slot) && landedBeforeExpiry, { manifestIntentSha256: item.intentSha256, outputIntentSha256: output.intentSha256 ?? null, observedIntentSha, lifecycleId, expectedLifecycleId, protectedPrestateSha256, routeAuthorizationSha256, expectedRouteAuthorizationSha256, routeId: intent.routeId ?? null, routeSpecSha256: intent.routeSpecSha256 ?? null, operation: intent.operation ?? null, amountRaw: amount, signerRole: intent.signerRole ?? null, user: intent.user ?? null, guardian: intent.guardian ?? null, policy: intent.policy ?? null, nonce: intent.nonce ?? null, prestateSlot, expiresAtUnix, blockTime: response.blockTime, messageSha256: intent.canonicalMessageSha256 ?? null }, { schemaVersion: 1, kind: "runtime", lifecycleId: expectedLifecycleId, protectedPrestateSha256: "lowercase SHA-256", routeAuthorizationSha256: expected.policy === null ? null : expectedRouteAuthorizationSha256, routeId: PARTNER_FOUR_MARKET_ROUTE.id, routeSpecSha256: fourMarketRouteSpecSha256(), operation: expected.operation, amountRaw: expected.amountRaw, signer: expected.signer, policy: expected.policy, nonce: expectedNonce, prestateSlot: `1..${response.slot}`, expiresAtUnix: `>= blockTime ${response.blockTime}`, messageSha256: item.messageSha256, intentSha256: observedIntentSha }, `rebuild ${name} from the exact lifecycle, route authorization, operation, and persisted canonical intent before expiry`);
  const persisted = verifyPersistedSignedIntent(manifestPath, output, name, item.signature, gates);
  if (persisted) {
    const signedMessage = Buffer.from(persisted.message.serialize());
    const chainMessage = Buffer.from(response.transaction.message.serialize());
    const persistedRoot = record(JSON.parse(readFileSync(resolve(stringField(output, "intentPath", `${name}.output`)), "utf8")), `${name}.persistedIntent`);
    const persistedIntent = record(persistedRoot.intent, `${name}.persistedIntent.intent`);
    add(gates, `${name} persisted wire equals confirmed chain wire`, signedMessage.equals(chainMessage) && canonicalJson(persistedIntent) === canonicalJson(intent), { persistedMessageSha256: sha256(signedMessage), chainMessageSha256: sha256(chainMessage), persistedIntentSha256: executionIntentSha256(persistedIntent as unknown as ExecutionIntent) }, { messageSha256: item.messageSha256, intentSha256: observedIntentSha }, `preserve the exact signed packet and intent before broadcast`);
    try {
      const evidence = validatedProtectedTransactionEvidence(output, persistedRoot, {
        label: name,
        lifecycleId: expectedLifecycleId,
        operation: expected.operation,
        expectedSigner: expected.signer,
        expectedSignature: item.signature,
        messageSha256: item.messageSha256,
        serializedTransactionSha256: sha256(Buffer.from(persisted.serialize())),
        intentSha256: observedIntentSha,
        intentProtectedPrestateSha256: shaField(intent, "protectedPrestateSha256", `${name}.intent`),
        confirmedSlot: response.slot,
      });
      const [preSignatureValid, settlementSignatureValid] = await Promise.all([
        verifyProtectedAttestationSignature(evidence.preSendAttestation, expected.signer),
        verifyProtectedAttestationSignature(evidence.settlementAttestation, expected.signer),
      ]);
      const manifestBindingExact = evidence.before.stateSha256 === item.protectedPrestateSha256
        && evidence.after.stateSha256 === item.protectedPoststateSha256
        && evidence.before.contextSlot === item.protectedBeforeContextSlot
        && evidence.after.contextSlot === item.protectedAfterContextSlot
        && evidence.preSendAttestation.attestationSha256 === item.protectedPreAttestationSha256
        && evidence.settlementAttestation.attestationSha256 === item.protectedSettlementAttestationSha256
        && prestateSlot !== null
        && prestateSlot <= BigInt(evidence.before.contextSlot);
      add(gates, `${name} signer-attested provider-observed protected account bytes`, preSignatureValid && settlementSignatureValid && manifestBindingExact, {
        signer: expected.signer,
        preSignatureValid,
        settlementSignatureValid,
        preStateSha256: evidence.before.stateSha256,
        postStateSha256: evidence.after.stateSha256,
        preContextSlot: evidence.before.contextSlot,
        confirmedSlot: response.slot,
        postContextSlot: evidence.after.contextSlot,
        preAttestationSha256: evidence.preSendAttestation.attestationSha256,
        settlementAttestationSha256: evidence.settlementAttestation.attestationSha256,
      }, {
        signer: expected.signer,
        signatures: "two valid Ed25519 signatures over exact canonical payloads",
        snapshots: "42 exact ordered account images with recomputed per-row and aggregate hashes",
        contexts: "pre <= confirmed transaction <= post",
        manifest: "exact snapshot and attestation digests",
      }, `regenerate ${name} from the maintained runtime; hash-only legacy outputs are not accepted`);
    } catch (error) {
      add(gates, `${name} signer-attested provider-observed protected account bytes`, false, error instanceof Error ? error.message : String(error), "exact recomputed snapshots plus fixed-signer pre-send and settlement attestations", `regenerate ${name}; do not copy legacy hash-only protectedState fields`);
    }
  }
}

async function verifyTransaction(connection: Connection, manifestPath: string, name: TxName, item: TxEvidence, expected: ExpectedTx, postReadAddresses: readonly string[], amounts: FourMarketManifest["amounts"], semanticAccounts: Readonly<{ idle: string; userAsset: string; userLp: string; escrow: string; receipt: string; reserveLiquidity: string; reserveCollateral: string; strategyAssetAta: string }>, expectedLifecycleId: string, expectedRouteAuthorizationSha256: string, gates: Gate[]): Promise<VersionedTransactionResponse | null> {
  const outputText = verifyRef(manifestPath, item, `transactions.${name}`, gates);
  let response: VersionedTransactionResponse | null = null;
  try { response = await connection.getTransaction(item.signature, { commitment: "confirmed", maxSupportedTransactionVersion: 0 }); } catch (error) { add(gates, `${name} RPC read`, false, error instanceof Error ? error.message : String(error), "confirmed transaction", "retry the exact signature against the pinned mainnet RPC"); return null; }
  add(gates, `${name} exists and succeeded`, response !== null && response.meta?.err === null, response ? { slot: response.slot, err: response.meta?.err ?? null } : null, { slot: item.slot, err: null }, `broadcast and confirm the canonical ${name} transaction once`);
  if (!response || response.meta?.err !== null) return response;
  const messageHash = sha256(response.transaction.message.serialize());
  add(gates, `${name} message hash`, messageHash === item.messageSha256, messageHash, item.messageSha256, `regenerate ${name} evidence from the exact signed message`);
  if (outputText !== null) {
    try {
      await verifyCommandOutput(manifestPath, record(JSON.parse(outputText), `${name} command output`), name, item, expected, response, expectedLifecycleId, expectedRouteAuthorizationSha256, gates);
    } catch (error) {
      add(gates, `${name} persisted signed intent envelope`, false, error instanceof Error ? error.message : String(error), "valid command output with persisted signed intent", "regenerate the confirmed command output and exact pre-send intent");
    }
  }
  add(gates, `${name} slot binding`, response.slot === item.slot, response.slot, item.slot, `record the transaction's confirmed slot, not a simulation slot`);
  const keys = transactionKeys(response);
  const signers = response.transaction.message.staticAccountKeys.slice(0, response.transaction.message.header.numRequiredSignatures).map((key) => key.toBase58());
  const programs = response.transaction.message.compiledInstructions.map((ix) => keys[ix.programIdIndex] ?? "<missing>");
  add(gates, `${name} canonical signer`, signers.length === 1 && signers[0] === expected.signer, signers, [expected.signer], `rebuild ${name} with the operation's fixed signer`);
  add(gates, `${name} approved top-level programs`, programs.includes(expected.requiredProgram) && programs.every((program) => expected.allowedPrograms.includes(program)), programs, expected.allowedPrograms, `rebuild ${name} from the approved canonical program graph`);
  add(gates, `${name} instruction count exact`, programs.length === expected.instructionCount, programs.length, expected.instructionCount, `remove extra instructions from the canonical ${name} packet`);
  try {
    const messageGate = canonicalMessageGate(response, expected);
    add(gates, `${name} complete canonical v0 message exact`, messageGate.pass, messageGate.observed, messageGate.expected, `rebuild ${name} with the exact blockhash-independent SDK packet, meta roles, compute bytes, and ALT indexes`);
  } catch (error) { add(gates, `${name} complete canonical v0 message exact`, false, error instanceof Error ? error.message : String(error), "byte-exact reconstructed v0 message", `fix the first canonical ${name} message difference before accepting evidence`); }
  const instructionGate = compareExpectedInstruction(response, expected.instruction);
  add(gates, `${name} canonical instruction accounts and data`, instructionGate.pass, instructionGate.observed, instructionGate.expected, `reconstruct the exact ${name} SDK instruction before signing`);
  for (const auxiliary of expected.auxiliaryInstructions) {
    const auxiliaryGate = compareExpectedInstruction(response, auxiliary);
    add(gates, `${name} auxiliary instruction accounts and data`, auxiliaryGate.pass, auxiliaryGate.observed, auxiliaryGate.expected, `reconstruct the exact auxiliary instruction for ${name}`);
  }
  const rows = tokenRows(response);
  add(gates, `${name} token rows closed`, rows.every((row) => expected.allowedTokenAccounts.includes(row.address) && expected.allowedMints.includes(row.mint)), rows.map(({ address: account, mint }) => ({ address: account, mint })), "only RouteSpec-derived token accounts and USDC/LP mints", `remove an unapproved token account or derive it from the canonical vault/user graph`);
  semanticTokenGate(name, response, amounts, semanticAccounts, gates);
  managerStrategyEventGate(name, response, expected, gates);
  exactLamportGate(name, response, expected, semanticAccounts, gates);
  try {
      const readback = await connection.getMultipleAccountsInfoAndContext(postReadAddresses.map((value) => new PublicKey(value)), { commitment: "confirmed", minContextSlot: response.slot });
      add(gates, `${name} post-state context anchored`, readback.context.slot >= response.slot && readback.value.every((account) => account !== null), { contextSlot: readback.context.slot, present: readback.value.map((account) => account !== null) }, { contextSlot: `>=${response.slot}`, present: true }, `read the canonical post-state at or after the confirmed transaction slot`);
  } catch (error) { add(gates, `${name} post-state read`, false, error instanceof Error ? error.message : String(error), `confirmed context >= ${response.slot}`, `rerun the post-state read with minContextSlot=${response.slot}`); }
  return response;
}

function eventPayload(response: VersionedTransactionResponse | null, name: string): JsonRecord | null {
  if (!response) return null;
  const events = parseTransactionEvents({ logMessages: response.meta?.logMessages ?? [] }).filter((event) => event.name === name);
  return events.length === 1 ? events[0]!.payload as unknown as JsonRecord : null;
}

function artifactHas(value: JsonRecord, keys: readonly string[]): boolean {
  return keys.every((key) => Object.prototype.hasOwnProperty.call(value, key));
}

function integerString(value: unknown, label: string): bigint {
  if (typeof value !== "string" || !/^-?[0-9]+$/.test(value)) throw new Error(`${label} must be an integer string`);
  return BigInt(value);
}

/** The maintained Rust/TypeScript restoration bridge serializes u64 fields as JSON numbers. */
function safeIntegerNumber(value: unknown, label: string): bigint {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${label} must be a non-negative safe integer number`);
  }
  return BigInt(value);
}

function scannerEvidence(
  value: JsonRecord,
  manifest: FourMarketManifest,
  requestEvent: JsonRecord | null,
  requestSignature: string,
  requestSlot: number,
  requestEventIndex: number,
  idleOriginRaw: bigint | null,
  gates: Gate[],
): VerifiedWithdrawalScan | null {
  try {
    exactKeys(value, ["verdict", "broadcast", "signerLoaded", "commitment", "routeId", "routeSpecSha256", "vault", "receiptProgram", "observationContextSlot", "receiptContextSlot", "idleContextSlot", "contextSlotsAligned", "rawQuery", "rawQuerySha256", "queryConfigSha256", "requestOrigin", "generationFingerprint", "receipts", "demand"], "withdrawalScanner");
    const queryProof = scannerQueryProof(value, "withdrawalScanner");
    const declaredOrigin = requestOrigin(value.requestOrigin, "withdrawalScanner.requestOrigin");
    if (!Array.isArray(value.receipts)) throw new Error("withdrawalScanner.receipts must be an array");
    const observationContextSlot = value.observationContextSlot;
    if (typeof observationContextSlot !== "number" || !Number.isSafeInteger(observationContextSlot) || observationContextSlot < requestSlot) throw new Error("withdrawalScanner observation slot must be at or after the request slot");
    const receipts = value.receipts.map((raw, index) => {
      const row = record(raw, `withdrawalScanner.receipts[${index}]`);
      exactKeys(row, ["receipt", "owner", "lamports", "dataBase64", "dataSha256", "vault", "user", "amountLpEscrowed", "amountAssetToWithdrawDecimalBits", "upperBoundAssetRaw", "withdrawableFromTs", "bump", "version", "observedContextSlot", "generationFingerprint"], `withdrawalScanner.receipts[${index}]`);
      const receipt = stringField(row, "receipt", `withdrawalScanner.receipts[${index}]`);
      const dataBase64 = stringField(row, "dataBase64", `withdrawalScanner.receipts[${index}]`);
      const data = Buffer.from(dataBase64, "base64");
      const owner = stringField(row, "owner", `withdrawalScanner.receipts[${index}]`);
      const lamports = row.lamports;
      if (data.toString("base64") !== dataBase64 || sha256(data) !== shaField(row, "dataSha256", `withdrawalScanner.receipts[${index}]`) || owner !== PARTNER_ROUTE.programs.voltrVault || typeof lamports !== "number" || !Number.isSafeInteger(lamports) || lamports <= 0) throw new Error(`withdrawalScanner receipt ${receipt} raw account envelope is not canonical`);
      const decoded = decodeReceipt({ address: receipt, owner, lamports, executable: false, data });
      if (!decoded) throw new Error(`withdrawalScanner receipt ${receipt} raw account bytes do not strictly decode`);
      const amountLpEscrowed = integerString(row.amountLpEscrowed, `withdrawalScanner.receipts[${index}].amountLpEscrowed`);
      const quoteBits = integerString(row.amountAssetToWithdrawDecimalBits, `withdrawalScanner.receipts[${index}].amountAssetToWithdrawDecimalBits`);
      const upperBoundAssetRaw = integerString(row.upperBoundAssetRaw, `withdrawalScanner.receipts[${index}].upperBoundAssetRaw`);
      const withdrawableFromTs = integerString(row.withdrawableFromTs, `withdrawalScanner.receipts[${index}].withdrawableFromTs`);
      const canonical = JSON.stringify({ receipt, vault: row.vault, user: row.user, amountLpEscrowed: amountLpEscrowed.toString(), amountAssetToWithdrawDecimalBits: quoteBits.toString(), withdrawableFromTs: withdrawableFromTs.toString(), bump: row.bump, version: row.version });
      if (row.vault !== decoded.vault || row.user !== decoded.user || amountLpEscrowed !== decoded.amountLpEscrowed || quoteBits !== decoded.amountAssetToWithdrawDecimalBits || withdrawableFromTs !== decoded.withdrawableFromTs || row.bump !== decoded.bump || row.version !== decoded.version || decoded.vault !== PARTNER_ROUTE.vault || amountLpEscrowed <= 0n || quoteBits <= 0n || upperBoundAssetRaw !== (quoteBits + FRACTION_SCALE - 1n) >> 48n || row.observedContextSlot !== observationContextSlot || row.version !== 0 || row.generationFingerprint !== sha256(canonical)) throw new Error(`withdrawalScanner receipt ${receipt} does not strictly decode/recompute`);
      return { receipt, user: row.user, dataSha256: shaField(row, "dataSha256", `withdrawalScanner.receipts[${index}]`), upperBoundAssetRaw, generationFingerprint: row.generationFingerprint as string, amountLpEscrowed, quoteBits, withdrawableFromTs };
    });
    const demand = record(value.demand, "withdrawalScanner.demand");
    exactKeys(demand, ["configuredIdleFloorRaw", "confirmedIdleRaw", "pendingWithdrawalUpperBoundRaw", "requiredIdleRaw", "idleShortfallRaw", "rounding"], "withdrawalScanner.demand");
    const configuredIdleFloorRaw = integerString(demand.configuredIdleFloorRaw, "withdrawalScanner.demand.configuredIdleFloorRaw");
    const confirmedIdleRaw = integerString(demand.confirmedIdleRaw, "withdrawalScanner.demand.confirmedIdleRaw");
    const pendingWithdrawalUpperBoundRaw = integerString(demand.pendingWithdrawalUpperBoundRaw, "withdrawalScanner.demand.pendingWithdrawalUpperBoundRaw");
    const requiredIdleRaw = integerString(demand.requiredIdleRaw, "withdrawalScanner.demand.requiredIdleRaw");
    const idleShortfallRaw = integerString(demand.idleShortfallRaw, "withdrawalScanner.demand.idleShortfallRaw");
    const recomputedPending = receipts.reduce((sum, row) => sum + row.upperBoundAssetRaw, 0n);
    const recomputedRequired = configuredIdleFloorRaw + recomputedPending;
    const recomputedShortfall = recomputedRequired > confirmedIdleRaw ? recomputedRequired - confirmedIdleRaw : 0n;
    const aggregateFingerprint = sha256(JSON.stringify({ routeSpecSha256: fourMarketRouteSpecSha256(), vault: PARTNER_ROUTE.vault, receiptContextSlot: value.receiptContextSlot, idleContextSlot: value.idleContextSlot, receipts: receipts.map(({ receipt, generationFingerprint }) => ({ receipt, fingerprint: generationFingerprint })), confirmedIdleRaw: confirmedIdleRaw.toString() }));
    const requestReceipt = typeof requestEvent?.requestWithdrawVaultReceipt === "string" ? requestEvent.requestWithdrawVaultReceipt : null;
    const requestRow = receipts.find(({ receipt }) => receipt === requestReceipt) ?? null;
    const requestDataSha256 = requestRow?.dataSha256 ?? null;
    const expectedOriginBase = requestRow !== null && requestEvent !== null && typeof requestDataSha256 === "string"
      ? { signature: requestSignature, eventIndex: requestEventIndex, receipt: requestRow.receipt, rawAccountSha256: requestDataSha256 }
      : null;
    const expectedOrigin: RequestOrigin | null = expectedOriginBase === null
      ? null
      : { ...expectedOriginBase, generationFingerprint: sha256(canonicalJson(expectedOriginBase)) };
    const requestLinked = requestRow !== null && requestEvent !== null && requestRow.user === manifest.identities.user && requestRow.amountLpEscrowed === manifest.amounts.requestWithdrawLpRaw && requestRow.quoteBits === requestEvent.amountAssetToWithdrawDecimalBits && requestRow.withdrawableFromTs === requestEvent.withdrawableFromTs && expectedOrigin !== null && sameRequestOrigin(declaredOrigin, expectedOrigin) && sameRequestOrigin(declaredOrigin, manifest.requestOrigin);
    const pass = value.verdict === "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS" && value.broadcast === false && value.signerLoaded === false && value.commitment === "confirmed" && value.routeId === manifest.routeId && value.routeSpecSha256 === manifest.routeSpecSha256 && value.vault === PARTNER_ROUTE.vault && value.receiptProgram === PARTNER_ROUTE.programs.voltrVault && value.receiptContextSlot === observationContextSlot && value.idleContextSlot === observationContextSlot && value.contextSlotsAligned === true && value.generationFingerprint === aggregateFingerprint && configuredIdleFloorRaw === PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw && pendingWithdrawalUpperBoundRaw === recomputedPending && requiredIdleRaw === recomputedRequired && idleShortfallRaw === recomputedShortfall && idleShortfallRaw > 0n && receipts.length === 1 && requestLinked && idleOriginRaw !== null && confirmedIdleRaw === idleOriginRaw;
    add(gates, "scanner exact query, decoded receipt, conservative math, and request generation origin", pass, { requestOrigin: declaredOrigin, expectedOrigin, queryProof, observationContextSlot, generationFingerprint: value.generationFingerprint, aggregateFingerprint, receipts, idleOriginRaw, demand: { configuredIdleFloorRaw, confirmedIdleRaw, pendingWithdrawalUpperBoundRaw, requiredIdleRaw, idleShortfallRaw } }, { requestOrigin: "request signature + event index + receipt raw account hash + generation fingerprint", rawQuerySha256: queryProof.rawQuerySha256, queryConfigSha256: queryProof.queryConfigSha256, requestReceipt: "the sole exact confirmed request event generation", observationContextSlot: `>=${requestSlot}`, alignedConfirmedSlots: true, confirmedIdleRaw: "exact prior confirmed idle post-balance", ceilEachU80F48: true, recomputedDemand: true }, "run the maintained confirmed scanner immediately after the exact request and before any restoration leg");
    return pass ? { verdict: "PARTNER_WITHDRAWAL_DEMAND_SCAN_PASS", routeId: manifest.routeId, routeSpecSha256: manifest.routeSpecSha256, vault: PARTNER_ROUTE.vault, observationContextSlot, generationFingerprint: aggregateFingerprint, requestOrigin: declaredOrigin, rawQuerySha256: queryProof.rawQuerySha256, queryConfigSha256: queryProof.queryConfigSha256, receipts: receipts.map(({ receipt, user, upperBoundAssetRaw, generationFingerprint }) => ({ receipt, user, upperBoundAssetRaw, generationFingerprint })), demand: { configuredIdleFloorRaw, confirmedIdleRaw, pendingWithdrawalUpperBoundRaw, requiredIdleRaw, idleShortfallRaw } } : null;
  } catch (error) {
    add(gates, "scanner exact query, decoded receipt, conservative math, and request generation origin", false, error instanceof Error ? error.message : String(error), "strict maintained scanner output", "regenerate the scanner artifact without hand-authored fields");
    return null;
  }
}

type RestorationProof = Readonly<{
  plan: ReturnType<typeof planWithdrawalRestoration>;
  confirmedLegs: readonly Readonly<{
    legId: string;
    strategyId: StrategyId;
    reserve: string;
    amountRaw: bigint;
    originId: string;
    transaction: TxEvidence;
    readbackContextSlot: number;
    idleRawAfter: bigint;
    remainingShortfallRaw: bigint;
  }>[];
  recomputations: readonly Readonly<{ afterLegId: string | null; contextSlot: number; confirmedIdleRaw: bigint; remainingShortfallRaw: bigint }>[];
  durableRows: readonly JsonRecord[];
}>;

function restorationEvidence(value: JsonRecord, manifest: FourMarketManifest, scan: VerifiedWithdrawalScan | null, gates: Gate[]): RestorationProof | null {
  try {
    exactKeys(value, ["schemaVersion", "evidenceType", "broadcast", "routeId", "routeSpecSha256", "scanGenerationFingerprint", "requestOrigin", "rawQuerySha256", "queryConfigSha256", "generation", "sources", "plan", "durableOutbox", "confirmedLegs", "shortfallRecomputations"], "restoration");
    if (value.schemaVersion !== 1 || value.evidenceType !== "backyard-voltr-withdrawal-restoration-confirmed" || value.broadcast !== true || !scan) throw new Error("restoration requires a passing scanner and exact confirmed evidence kind");
    if (value.routeId !== manifest.routeId || value.routeSpecSha256 !== manifest.routeSpecSha256 || value.scanGenerationFingerprint !== scan.generationFingerprint) throw new Error("restoration route/scan origin is not exact");
    const restorationOrigin = requestOrigin(value.requestOrigin, "restoration.requestOrigin");
    const restorationRawQuerySha256 = shaField(value, "rawQuerySha256", "restoration");
    const restorationQueryConfigSha256 = shaField(value, "queryConfigSha256", "restoration");
    if (!sameRequestOrigin(restorationOrigin, scan.requestOrigin) || !sameRequestOrigin(restorationOrigin, manifest.requestOrigin) || restorationRawQuerySha256 !== scan.rawQuerySha256 || restorationQueryConfigSha256 !== scan.queryConfigSha256) throw new Error("restoration request origin/query provenance does not equal the confirmed scanner and manifest");
    const generation = value.generation;
    if (typeof generation !== "number" || !Number.isSafeInteger(generation) || generation <= 0) throw new Error("restoration generation must be a positive safe integer");
    if (!Array.isArray(value.sources) || value.sources.length === 0) throw new Error("restoration sources must be nonempty");
    const sources: WithdrawalRestorationSource[] = value.sources.map((raw, index) => {
      const source = record(raw, `restoration.sources[${index}]`);
      exactKeys(source, ["strategyId", "reserve", "availableRaw", "netYieldLossBps", "unwindCostLamports", "observedContextSlot", "positionFingerprint"], `restoration.sources[${index}]`);
      const strategyId = stringField(source, "strategyId", `restoration.sources[${index}]`) as StrategyId;
      if (!REQUIRED_STRATEGIES.includes(strategyId)) throw new Error(`restoration source ${strategyId} is not approved`);
      const observedContextSlot = source.observedContextSlot;
      if (typeof observedContextSlot !== "number" || !Number.isSafeInteger(observedContextSlot)) throw new Error(`restoration source ${strategyId} has invalid context slot`);
      const positionFingerprint = shaField(source, "positionFingerprint", `restoration.sources[${index}]`);
      return { strategyId, reserve: stringField(source, "reserve", `restoration.sources[${index}]`), availableRaw: integerString(source.availableRaw, `restoration.sources[${index}].availableRaw`), netYieldLossBps: integerString(source.netYieldLossBps, `restoration.sources[${index}].netYieldLossBps`), unwindCostLamports: integerString(source.unwindCostLamports, `restoration.sources[${index}].unwindCostLamports`), observedContextSlot, positionFingerprint };
    });
    const requestCheckpoint = manifest.transactions.withdrawRequest;
    const plan = planWithdrawalRestoration(scan, sources, generation, {
      lifecycleId: manifest.lifecycleId,
      routeAuthorizationSha256: manifest.routeAuthorizationSha256,
      requestOrigin: restorationOrigin,
      protectedCheckpoint: {
        addressSetSha256: requestCheckpoint.protectedAddressSetSha256,
        stateSha256: requestCheckpoint.protectedPoststateSha256,
        contextSlot: requestCheckpoint.protectedAfterContextSlot,
      },
    });
    const suppliedPlan = record(value.plan, "restoration.plan");
    if (canonicalJson(suppliedPlan) !== canonicalJson(plan)) throw new Error("restoration plan does not equal the maintained deterministic planner output");
    if (plan.legs.length !== 1
      || plan.legs[0]?.strategyId !== "main"
      || plan.legs[0].reserve !== partnerStrategyIdentity("main").reserve
      || plan.legs[0].amountRaw !== manifest.amounts.restorationAssetRaw) {
      throw new Error("partner lifecycle restoration must be one exact Main withdrawal equal to the scanned idle shortfall");
    }
    if (!Array.isArray(value.confirmedLegs) || value.confirmedLegs.length !== plan.legs.length) throw new Error("restoration confirmed legs must cover the exact deterministic plan");
    const confirmedLegs = value.confirmedLegs.map((raw, index) => {
      const row = record(raw, `restoration.confirmedLegs[${index}]`);
      exactKeys(row, ["legId", "strategyId", "reserve", "amountRaw", "originId", "transaction", "readbackContextSlot", "idleRawAfter", "remainingShortfallRaw"], `restoration.confirmedLegs[${index}]`);
      const planned = plan.legs[index]!;
      const strategyId = stringField(row, "strategyId", `restoration.confirmedLegs[${index}]`) as StrategyId;
      const amountRaw = integerString(row.amountRaw, `restoration.confirmedLegs[${index}].amountRaw`);
      const readbackContextSlot = row.readbackContextSlot;
      if (row.legId !== planned.legId || strategyId !== planned.strategyId || row.reserve !== planned.reserve || amountRaw !== planned.amountRaw || row.originId !== plan.originId || typeof readbackContextSlot !== "number" || !Number.isSafeInteger(readbackContextSlot)) throw new Error(`restoration confirmed leg ${index} does not equal the deterministic plan`);
      const transaction = tx(row.transaction, `restoration.confirmedLegs[${index}].transaction`);
      if (canonicalJson(transaction) !== canonicalJson(manifest.transactions.managerMainRestorationWithdraw)) throw new Error("restoration confirmed leg is not the exact named lifecycle transaction");
      return { legId: planned.legId, strategyId, reserve: planned.reserve, amountRaw, originId: plan.originId, transaction, readbackContextSlot, idleRawAfter: integerString(row.idleRawAfter, `restoration.confirmedLegs[${index}].idleRawAfter`), remainingShortfallRaw: integerString(row.remainingShortfallRaw, `restoration.confirmedLegs[${index}].remainingShortfallRaw`) };
    });
    if (!Array.isArray(value.shortfallRecomputations) || value.shortfallRecomputations.length !== confirmedLegs.length + 1) throw new Error("restoration must retain the initial and every post-leg shortfall recomputation");
    const recomputations = value.shortfallRecomputations.map((raw, index) => {
      const row = record(raw, `restoration.shortfallRecomputations[${index}]`);
      exactKeys(row, ["afterLegId", "contextSlot", "confirmedIdleRaw", "remainingShortfallRaw"], `restoration.shortfallRecomputations[${index}]`);
      if ((index === 0 ? row.afterLegId !== null : row.afterLegId !== confirmedLegs[index - 1]!.legId) || typeof row.contextSlot !== "number" || !Number.isSafeInteger(row.contextSlot)) throw new Error(`restoration recomputation ${index} origin/context is not exact`);
      return { afterLegId: row.afterLegId as string | null, contextSlot: row.contextSlot, confirmedIdleRaw: integerString(row.confirmedIdleRaw, `restoration.shortfallRecomputations[${index}].confirmedIdleRaw`), remainingShortfallRaw: integerString(row.remainingShortfallRaw, `restoration.shortfallRecomputations[${index}].remainingShortfallRaw`) };
    });
    const outbox = record(value.durableOutbox, "restoration.durableOutbox");
    exactKeys(outbox, ["eventKind", "aggregateKind", "originId", "generation", "insertedLegCount", "duplicateLegCount", "rows", "ackCondition"], "restoration.durableOutbox");
    if (!Array.isArray(outbox.rows) || outbox.rows.length !== confirmedLegs.length) throw new Error("restoration durable outbox rows must cover every leg exactly once");
    const durableRows = outbox.rows.map((raw, index) => {
      const row = record(raw, `restoration.durableOutbox.rows[${index}]`);
      exactKeys(row, ["eventId", "legId", "dedupeKey", "state", "leaseFence", "managerIntentId", "expectedSignature", "confirmedSignature", "confirmedSlot", "readbackContextSlot", "oneSendOnly"], `restoration.durableOutbox.rows[${index}]`);
      const leg = confirmedLegs[index]!;
      const expectedDedupe = `backyard-voltr:${plan.originId}:${generation}:${leg.legId}`;
      if (row.legId !== leg.legId || row.dedupeKey !== expectedDedupe || row.state !== "acknowledged" || typeof row.eventId !== "number" || typeof row.leaseFence !== "number" || row.leaseFence <= 0 || row.expectedSignature !== leg.transaction.signature || row.confirmedSignature !== leg.transaction.signature || row.confirmedSlot !== leg.transaction.slot || row.readbackContextSlot !== leg.readbackContextSlot || row.oneSendOnly !== true || typeof row.managerIntentId !== "string" || row.managerIntentId.length !== 64) throw new Error(`restoration durable outbox row ${index} is not bound to its exact confirmed signed leg`);
      return row;
    });
    const outboxExact = outbox.eventKind === "backyard_voltr_manager_withdraw" && outbox.aggregateKind === "voltr_withdrawal_restoration" && outbox.originId === plan.originId && outbox.generation === generation && outbox.insertedLegCount === confirmedLegs.length && outbox.duplicateLegCount === 0 && outbox.ackCondition === "confirmed_manager_readback_and_recomputed_idle_shortfall";
    add(gates, "restoration deterministic plan and durable outbox contract exact", outboxExact, { plan, durableOutbox: outbox }, { eventKind: "backyard_voltr_manager_withdraw", aggregateKind: "voltr_withdrawal_restoration", originId: plan.originId, generation, exactlyOnceRows: confirmedLegs.length, ackCondition: "confirmed_manager_readback_and_recomputed_idle_shortfall" }, "persist the exact deterministic plan through the maintained store outbox before any leg broadcast");
    const sourceContractExact = durableRows.length === confirmedLegs.length
      && durableRows.every((row, index) => row.confirmedSignature === confirmedLegs[index]!.transaction.signature && row.confirmedSlot === confirmedLegs[index]!.transaction.slot)
      && restorationOrigin.signature === manifest.requestOrigin.signature;
    add(gates, "restoration source/origin contract exact", sourceContractExact, { artifactRows: durableRows.length, sources, generation, requestOrigin: restorationOrigin, rawQuerySha256: restorationRawQuerySha256, queryConfigSha256: restorationQueryConfigSha256 }, { oneRowPerConfirmedLeg: confirmedLegs.length, requestOrigin: manifest.requestOrigin, rawQuerySha256: scan.rawQuerySha256, queryConfigSha256: scan.queryConfigSha256 }, "persist the scanner query/origin tuple with the durable restoration generation and reconcile every acknowledged leg");
    return outboxExact ? { plan, confirmedLegs, recomputations, durableRows } : null;
  } catch (error) {
    add(gates, "restoration deterministic plan and durable outbox contract exact", false, error instanceof Error ? error.message : String(error), "strict deterministic plan + durable outbox + confirmed legs", "regenerate restoration from the passing scanner through the maintained planner/store boundary");
    return null;
  }
}

function verifyEarnAdapterEvidence(value: JsonRecord, manifest: FourMarketManifest, scan: VerifiedWithdrawalScan | null, responses: ReadonlyMap<TxName, VersionedTransactionResponse | null>, gates: Gate[]): void {
  try {
    exactKeys(value, ["schemaVersion", "evidenceType", "broadcast", "routeId", "routeSpecSha256", "executionKind", "priority", "normalOptimizationIntervalSeconds", "sourceBindings", "outboxContract", "movement", "sharedReplay"], "earnAdapter");
    if (!Array.isArray(value.sourceBindings)) throw new Error("earnAdapter.sourceBindings must be an array");
    const observedBindings = value.sourceBindings.map((raw, index) => {
      const row = record(raw, `earnAdapter.sourceBindings[${index}]`);
      exactKeys(row, ["path", "sha256"], `earnAdapter.sourceBindings[${index}]`);
      return { path: stringField(row, "path", `earnAdapter.sourceBindings[${index}]`), sha256: shaField(row, "sha256", `earnAdapter.sourceBindings[${index}]`) };
    });
    const currentBindings = EARN_ADAPTER_SOURCE_PATHS.map((path) => ({ path, sha256: sha256(readFileSync(resolve(REPOSITORY_ROOT, path))) }));
    const outbox = record(value.outboxContract, "earnAdapter.outboxContract");
    exactKeys(outbox, ["oneDurableMovement", "sourceWithdrawThenDestinationDeposit", "leaseFencing", "oneSend", "confirmedReconciliation", "recoveryKeepsMovementIdentity", "directKaminoExecutorUsed"], "earnAdapter.outboxContract");
    const movement = record(value.movement, "earnAdapter.movement");
    exactKeys(movement, ["movementId", "sourceStrategyId", "destinationStrategyId", "amountRaw", "sourceWithdrawSignature", "sourceWithdrawSlot", "idleReadbackContextSlot", "destinationDepositSignature", "destinationDepositSlot", "timerDecisionCount", "withdrawalDemandReservedRaw"], "earnAdapter.movement");
    const sourceStrategyId = stringField(movement, "sourceStrategyId", "earnAdapter.movement") as StrategyId;
    const destinationStrategyId = stringField(movement, "destinationStrategyId", "earnAdapter.movement") as StrategyId;
    if (!REQUIRED_STRATEGIES.includes(sourceStrategyId) || !REQUIRED_STRATEGIES.includes(destinationStrategyId) || sourceStrategyId === destinationStrategyId) throw new Error("Earn movement source/destination strategy ids are not exact approved distinct strategies");
    const txName = (strategyId: StrategyId, operation: "Deposit" | "Withdraw"): TxName => `manager${strategyId[0]!.toUpperCase()}${strategyId.slice(1)}${operation}` as TxName;
    const sourceTx = manifest.transactions[txName(sourceStrategyId, "Withdraw")];
    const destinationTx = manifest.transactions[txName(destinationStrategyId, "Deposit")];
    const sourceResponse = responses.get(txName(sourceStrategyId, "Withdraw")) ?? null;
    const destinationResponse = responses.get(txName(destinationStrategyId, "Deposit")) ?? null;
    const amountRaw = integerString(movement.amountRaw, "earnAdapter.movement.amountRaw");
    const idleReadbackContextSlot = movement.idleReadbackContextSlot;
    const movementId = shaField(movement, "movementId", "earnAdapter.movement");
    const confirmedIdleRaw = tokenPreAmount(sourceResponse, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta);
    if (!scan || confirmedIdleRaw === null) throw new Error("Earn replay requires the exact normal-movement prestate plus the separate confirmed withdrawal-demand probe");
    const sharedReplay = validateEarnSharedReplay(value.sharedReplay, {
      movementId,
      sourceStrategyId,
      destinationStrategyId,
      sourceReserve: partnerStrategyIdentity(sourceStrategyId).reserve,
      targetReserve: partnerStrategyIdentity(destinationStrategyId).reserve,
      amountRaw,
      expectedContextSlot: sourceTx.protectedBeforeContextSlot,
      expectedObservation: {
        configuredIdleFloorRaw: PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw,
        confirmedIdleRaw,
        withdrawalDemandRaw: 0n,
        requiredIdleRaw: PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIdleFloorRaw,
        idleShortfallRaw: 0n,
      },
      rustSourceBindings: currentBindings.filter(({ path }) => path.startsWith("crates/")),
    });
    const sourceIdleDelta = sourceResponse ? tokenDelta(sourceResponse, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta) : null;
    const destinationIdleDelta = destinationResponse ? tokenDelta(destinationResponse, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta) : null;
    const pass = value.schemaVersion === 1 && value.evidenceType === "backyard-voltr-shared-earn-adapter-confirmed" && value.broadcast === false && value.routeId === manifest.routeId && value.routeSpecSha256 === manifest.routeSpecSha256 && value.executionKind === "voltr-manager" && value.priority === "withdrawal-restoration-first" && integerString(value.normalOptimizationIntervalSeconds, "earnAdapter.normalOptimizationIntervalSeconds") === PARTNER_FOUR_MARKET_ROUTE.normalOptimizationIntervalSeconds && canonicalJson(observedBindings) === canonicalJson(currentBindings) && outbox.oneDurableMovement === true && outbox.sourceWithdrawThenDestinationDeposit === true && outbox.leaseFencing === true && outbox.oneSend === true && outbox.confirmedReconciliation === true && outbox.recoveryKeepsMovementIdentity === true && outbox.directKaminoExecutorUsed === false && amountRaw === manifest.amounts.managerAssetRaw && movement.sourceWithdrawSignature === sourceTx.signature && movement.sourceWithdrawSlot === sourceTx.slot && typeof idleReadbackContextSlot === "number" && Number.isSafeInteger(idleReadbackContextSlot) && idleReadbackContextSlot >= sourceTx.slot && movement.destinationDepositSignature === destinationTx.signature && movement.destinationDepositSlot === destinationTx.slot && destinationTx.slot > sourceTx.slot && sourceIdleDelta !== null && sourceIdleDelta > 0n && sourceIdleDelta <= amountRaw && destinationIdleDelta === -amountRaw && movement.timerDecisionCount === 1 && integerString(movement.withdrawalDemandReservedRaw, "earnAdapter.movement.withdrawalDemandReservedRaw") === 0n && sharedReplay.priorityProbe.withdrawalDemandRaw === scan.demand.pendingWithdrawalUpperBoundRaw.toString() && sharedReplay.priorityProbe.preRequestManagerPair.present === true && sharedReplay.priorityProbe.preRequestManagerPair.restoresLaterRequest === false;
    add(gates, "Earn adapter exact shared planner/source/outbox/movement proof", pass, { sourceBindings: observedBindings, outbox, movement, sharedReplay }, { sourceBindings: currentBindings, executionKind: "voltr-manager", priority: "withdrawal-restoration-first", intervalSeconds: 3_600n, directKaminoExecutorUsed: false, sourceWithdrawThenConfirmedIdleThenDestinationDeposit: true }, "add the thin maintained Voltr adapter and bind one confirmed two-leg movement to the shared planner/outbox sources");
    const replayPass = sharedReplay.planner.recomputed === true && sharedReplay.planner.selectedAmountRaw === amountRaw.toString() && sharedReplay.planner.selectedNotionalUsdMicros === amountRaw.toString() && sharedReplay.durable.movementId === movementId && sharedReplay.observation.contextSlot === sourceTx.protectedBeforeContextSlot && sharedReplay.observation.withdrawalDemandRaw === "0" && sharedReplay.normalOptimization.status === "eligible" && sharedReplay.priorityProbe.normalOptimization.status === "blocked" && sharedReplay.durable.replayed === true;
    add(gates, "Earn shared observation/planner decision and durable movement independently replayed", replayPass, { sharedReplay, movementId }, "live shared observation inputs + recomputed planner output + exact durable movement/outbox rows + confirmed leg advancement", "generate the read-only Earn replay envelope from the maintained observer/planner and durable Voltr outbox");
  } catch (error) {
    add(gates, "Earn adapter exact shared planner/source/outbox/movement proof", false, error instanceof Error ? error.message : String(error), { sourcePaths: EARN_ADAPTER_SOURCE_PATHS, intervalSeconds: 3_600, executionKind: "voltr-manager" }, "wire the missing thin Earn adapter or regenerate its exact source-bound proof");
  }
}

async function verifyFinalConservation(
  rpcUrl: string,
  value: JsonRecord,
  manifest: FourMarketManifest,
  claimSlot: number,
  gates: Gate[],
): Promise<void> {
  try {
    exactKeys(value, ["schemaVersion", "evidenceType", "broadcast", "routeId", "routeSpecSha256", "finalContextSlot", "activeReceipts", "conservation"], "finalReconciliation");
    if (value.schemaVersion !== 1 || value.evidenceType !== "backyard-voltr-final-current-conservation" || value.broadcast !== false || value.routeId !== manifest.routeId || value.routeSpecSha256 !== manifest.routeSpecSha256) throw new Error("final reconciliation schema/route is not exact");
    const scan = await scanWithdrawalDemand(undefined, 0, undefined, claimSlot);
    const minimumSlot = Math.max(claimSlot, scan.observationContextSlot);
    const baseAccounts = await deriveVoltrAccountsForStrategy(PARTNER_ROUTE, PARTNER_ROUTE.strategy.reserve);
    const addresses = [PARTNER_ROUTE.vault, baseAccounts.lpMint, baseAccounts.idleAta, PARTNER_ROUTE.asset.mint, ...REQUIRED_STRATEGIES.map((id) => partnerStrategyIdentity(id).voltr.strategyInitReceipt)];
    const state = await confirmedSnapshots(rpcUrl, addresses, minimumSlot);
    const vault = verifyVaultCurrentState({ route: PARTNER_ROUTE, accounts: baseAccounts, vault: state.accounts[0] ?? null, lpMint: state.accounts[1] ?? null, idleAta: state.accounts[2] ?? null, assetMint: state.accounts[3] ?? null });
    if (!vault.state || vault.failedGateCount !== 0) throw new Error("final current vault state does not pass the exact RouteSpec decoder");
    const lpMint = state.accounts[1] ? getMintDecoder().decode(state.accounts[1]!.data) : null;
    const idle = state.accounts[2] ? getTokenDecoder().decode(state.accounts[2]!.data) : null;
    if (!lpMint || !idle || idle.mint !== PARTNER_ROUTE.asset.mint || idle.owner !== PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAuth) throw new Error("final LP mint/idle ATA does not strictly decode");
    const strategyPositions = Object.fromEntries(REQUIRED_STRATEGIES.map((id, index) => {
      const snapshot = state.accounts[4 + index];
      if (!snapshot) throw new Error(`final ${id} strategy receipt is absent`);
      const receipt = getStrategyInitReceiptDecoder().decode(snapshot.data);
      const expected = partnerStrategyIdentity(id);
      if (receipt.vault !== PARTNER_ROUTE.vault || receipt.strategy !== expected.reserve || receipt.adaptorProgram !== PARTNER_ROUTE.programs.kaminoAdaptor) throw new Error(`final ${id} strategy receipt identity changed`);
      return [id, receipt.positionValue];
    })) as Record<StrategyId, bigint>;
    const sumPositions = REQUIRED_STRATEGIES.reduce((sum, id) => sum + strategyPositions[id], 0n);
    const accountingDifferenceRaw = vault.state.totalValueRaw - idle.amount - sumPositions;
    const conservation = record(value.conservation, "finalReconciliation.conservation");
    exactKeys(conservation, ["idleRaw", "strategyPositionsRaw", "lpSupplyRaw", "vaultTotalValueRaw", "accountingDifferenceRaw"], "finalReconciliation.conservation");
    const artifactPositions = record(conservation.strategyPositionsRaw, "finalReconciliation.conservation.strategyPositionsRaw");
    exactKeys(artifactPositions, REQUIRED_STRATEGIES, "finalReconciliation.conservation.strategyPositionsRaw");
    const activeReceipts = scan.receipts.map(({ receipt }) => receipt).slice().sort();
    const [lifecycleReceipt] = await findRequestWithdrawVaultReceiptPda({ vault: PARTNER_ROUTE.vault, userTransferAuthority: address(manifest.identities.user) }, { programAddress: PARTNER_ROUTE.programs.voltrVault });
    const artifactReceipts = Array.isArray(value.activeReceipts) && value.activeReceipts.every((item) => typeof item === "string") ? (value.activeReceipts as string[]).slice().sort() : [];
    const artifactContextSlot = value.finalContextSlot;
    const pass = typeof artifactContextSlot === "number" && Number.isSafeInteger(artifactContextSlot) && artifactContextSlot >= claimSlot && state.contextSlot >= artifactContextSlot && scan.observationContextSlot >= claimSlot && !activeReceipts.includes(lifecycleReceipt) && canonicalJson(artifactReceipts) === canonicalJson(activeReceipts) && integerString(conservation.idleRaw, "finalReconciliation.conservation.idleRaw") === idle.amount && integerString(conservation.lpSupplyRaw, "finalReconciliation.conservation.lpSupplyRaw") === lpMint.supply && integerString(conservation.vaultTotalValueRaw, "finalReconciliation.conservation.vaultTotalValueRaw") === vault.state.totalValueRaw && integerString(conservation.accountingDifferenceRaw, "finalReconciliation.conservation.accountingDifferenceRaw") === accountingDifferenceRaw && accountingDifferenceRaw === 0n && REQUIRED_STRATEGIES.every((id) => integerString(artifactPositions[id], `finalReconciliation.strategyPositionsRaw.${id}`) === strategyPositions[id]) && vault.state.lpSupplyRaw === lpMint.supply && vault.state.idleRaw === idle.amount;
    add(gates, "final current decoded conservation exact", pass, { artifactContextSlot, currentContextSlot: state.contextSlot, scanContextSlot: scan.observationContextSlot, activeReceipts, idleRaw: idle.amount, strategyPositionsRaw: strategyPositions, lpSupplyRaw: lpMint.supply, vaultTotalValueRaw: vault.state.totalValueRaw, accountingDifferenceRaw }, { contextSlot: `>= claim ${claimSlot}`, activeReceipts: artifactReceipts, accountingDifferenceRaw: 0n, vaultLpSupplyEqualsMint: true, vaultIdleEqualsAta: true }, "reload the current vault, all receipts, four positions, LP mint, and idle ATA in a claim-anchored confirmed context");
  } catch (error) {
    add(gates, "final current decoded conservation exact", false, error instanceof Error ? error.message : String(error), "claim-anchored current idle + four positions = vault total and exact LP/receipts", "produce a fresh final-current reconciliation after the successful claim");
  }
}

function verifyArtifactSemantics(name: ArtifactName, value: JsonRecord, manifest: FourMarketManifest, gates: Gate[], requestEvent: JsonRecord | null, requestSignature: string, requestSlot: number, requestEventIndex: number, idleOriginRaw: bigint | null, policyArtifact: ReturnType<typeof loadRuntimePolicyArtifact>['artifact'] | null): VerifiedWithdrawalScan | null {
  const routeBound = value.routeId === manifest.routeId && value.routeSpecSha256 === manifest.routeSpecSha256;
  add(gates, `artifacts.${name} route binding`, routeBound, { routeId: value.routeId ?? null, routeSpecSha256: value.routeSpecSha256 ?? null }, { routeId: manifest.routeId, routeSpecSha256: manifest.routeSpecSha256 }, `bind ${name} to the immutable four-market RouteSpec`);
  if (name === "instantWithdrawRejection") {
    exactKeys(value, ["verdict", "broadcast", "readyForBroadcast", "routeId", "routeSpecSha256", "intentSha256", "intent", "prestateContextSlot", "simulation", "transaction", "protectedState", "protectedSnapshotEvidence", "confirmationCommitment", "deployments", "rejectionReadbackContextSlot", "failedGateCount", "gates"], "instantWithdrawRejection");
    const intent = userRuntimeIntent(value.intent, "instantWithdrawRejection.intent");
    const simulation = record(value.simulation, "instantWithdrawRejection.simulation");
    exactKeys(simulation, ["prestateSlot", "contextSlot", "err", "unitsConsumed", "logsSha256"], "instantWithdrawRejection.simulation");
    const transaction = record(value.transaction, "instantWithdrawRejection.transaction");
    exactKeys(transaction, ["operation", "mode", "user", "vault", "amountLpRaw", "quoteAssetRaw", "withdrawalWaitingPeriodSeconds", "disabledOperations", "withdrawAll", "packetBytes", "feeLamports", "expectedSignature", "instruction", "canonicalMessageSha256", "serializedTransactionBase64", "serializedTransactionSha256", "serializedMessageBase64", "serializedMessageSha256", "simulationErrorCode", "rejectionLogs", "noEventCount", "rejectionReadback"], "instantWithdrawRejection.transaction");
    const named = Array.isArray(value.gates) ? value.gates.map((item) => record(item, "instantWithdrawRejection.gate")) : [];
    const logGate = named.find((gate) => gate.name === "simulation logs identify InstantWithdrawNotAllowed") ?? null;
    const logs = Array.isArray(logGate?.observed) && logGate.observed.every((line) => typeof line === "string") ? logGate.observed as string[] : [];
    const rejectionLogs = Array.isArray(transaction.rejectionLogs) && transaction.rejectionLogs.every((line) => typeof line === "string") ? transaction.rejectionLogs as string[] : [];
    const errorExact = canonicalJson(simulation.err) === canonicalJson({ InstructionError: [0, { Custom: 6015 }] });
    const logsExact = logs.length === 5
      && logs[0] === `Program ${PARTNER_ROUTE.programs.voltrVault} invoke [1]`
      && logs[1] === "Program log: Instruction: InstantWithdrawVault"
      && logs[2]?.includes("programs/voltr-vault/src/instructions/instant_withdraw_vault.rs:69") === true
      && logs[2]?.includes("Error Code: InstantWithdrawNotAllowed. Error Number: 6015.") === true
      && logs[4] === `Program ${PARTNER_ROUTE.programs.voltrVault} failed: custom program error: 0x177f`
      && typeof simulation.unitsConsumed === "number"
      && logs[3]?.includes(`consumed ${simulation.unitsConsumed} of`) === true
      && sha256(logs.join("\n")) === simulation.logsSha256
      && rejectionLogs.length === 2
      && rejectionLogs[0] === logs[2]
      && rejectionLogs[1] === logs[4];
    const wireBase64 = stringField(transaction, "serializedTransactionBase64", "instantWithdrawRejection.transaction");
    const wire = Buffer.from(wireBase64, "base64");
    const messageBase64 = stringField(transaction, "serializedMessageBase64", "instantWithdrawRejection.transaction");
    const messageBytes = Buffer.from(messageBase64, "base64");
    let decoded: VersionedTransaction | null = null;
    try { decoded = VersionedTransaction.deserialize(wire); } catch { /* exact packet gate below */ }
    const wireSignature = decoded?.signatures.length === 1 ? bs58.encode(decoded.signatures[0]!) : null;
    const decodedMessage = decoded ? Buffer.from(decoded.message.serialize()) : null;
    const packetExact = wire.toString("base64") === wireBase64
      && messageBytes.toString("base64") === messageBase64
      && typeof transaction.packetBytes === "number"
      && transaction.packetBytes === wire.length
      && wire.length <= 1_232
      && sha256(wire) === transaction.serializedTransactionSha256
      && sha256(messageBytes) === transaction.serializedMessageSha256
      && transaction.serializedMessageSha256 === transaction.canonicalMessageSha256
      && decodedMessage !== null
      && decodedMessage.equals(messageBytes)
      && wireSignature === transaction.expectedSignature;
    const deployments = record(value.deployments, "instantWithdrawRejection.deployments");
    exactKeys(deployments, ["before", "after"], "instantWithdrawRejection.deployments");
    const expectedDeployments = PARTNER_ROUTE.deployments.map((item) => ({ ...item, deployedSlot: item.deployedSlot.toString() }));
    const deploymentsExact = canonicalJson(deployments.before) === canonicalJson(expectedDeployments)
      && canonicalJson(deployments.after) === canonicalJson(expectedDeployments);
    const protectedState = record(value.protectedState, "instantWithdrawRejection.protectedState");
    const protectedEvidence = validatedProtectedSnapshotEnvelope(value.protectedSnapshotEvidence, protectedState, "instantWithdrawRejection");
    const rejectionReadback = record(transaction.rejectionReadback, "instantWithdrawRejection.transaction.rejectionReadback");
    exactKeys(rejectionReadback, ["contextSlot", "changedAccounts", "protectedState", "protectedSnapshotEvidence", "deployments"], "instantWithdrawRejection.transaction.rejectionReadback");
    const readbackProtected = record(rejectionReadback.protectedState, "instantWithdrawRejection.transaction.rejectionReadback.protectedState");
    const readbackProtectedEvidence = validatedProtectedSnapshotEnvelope(rejectionReadback.protectedSnapshotEvidence, readbackProtected, "instantWithdrawRejection.transaction.rejectionReadback");
    const protectedBytesUnchanged = canonicalJson(protectedEvidence.before.rows) === canonicalJson(protectedEvidence.after.rows)
      && canonicalJson(protectedEvidence.before.rows) === canonicalJson(readbackProtectedEvidence.before.rows)
      && canonicalJson(readbackProtectedEvidence.before.rows) === canonicalJson(readbackProtectedEvidence.after.rows);
    const contextsExact = typeof value.prestateContextSlot === "number"
      && Number.isSafeInteger(value.prestateContextSlot)
      && value.prestateContextSlot > 0
      && typeof simulation.prestateSlot === "number"
      && Number.isSafeInteger(simulation.prestateSlot)
      && simulation.prestateSlot > 0
      && typeof simulation.contextSlot === "number"
      && Number.isSafeInteger(simulation.contextSlot)
      && simulation.contextSlot > 0
      && typeof value.rejectionReadbackContextSlot === "number"
      && Number.isSafeInteger(value.rejectionReadbackContextSlot)
      && value.rejectionReadbackContextSlot > 0
      && typeof rejectionReadback.contextSlot === "number"
      && Number.isSafeInteger(rejectionReadback.contextSlot)
      && rejectionReadback.contextSlot > 0
      && value.prestateContextSlot <= simulation.prestateSlot
      && simulation.prestateSlot <= simulation.contextSlot
      && simulation.contextSlot <= value.rejectionReadbackContextSlot
      && value.rejectionReadbackContextSlot === rejectionReadback.contextSlot
      && protectedEvidence.before.contextSlot <= simulation.contextSlot
      && protectedEvidence.after.contextSlot >= simulation.contextSlot
      && readbackProtectedEvidence.after.contextSlot === rejectionReadback.contextSlot
      && protectedState.schemaVersion === 1
      && protectedState.addressSetSha256 === fourMarketProtectedAddressSetSha256()
      && protectedState.beforeSha256 === readbackProtected.beforeSha256
      && protectedState.beforeSha256 === protectedState.afterSha256
      && readbackProtected.beforeSha256 === readbackProtected.afterSha256
      && protectedState.addressSetSha256 === readbackProtected.addressSetSha256
      && protectedState.addressSetSha256 === manifest.transactions.withdrawRequest.protectedAddressSetSha256
      && Array.isArray(rejectionReadback.changedAccounts)
      && rejectionReadback.changedAccounts.length === 0
      && protectedBytesUnchanged;
    const pass = value.verdict === "PARTNER_INSTANT_WITHDRAW_REJECTION_PASS"
      && value.broadcast === false
      && value.readyForBroadcast === false
      && value.confirmationCommitment === "confirmed"
      && executionIntentSha256(intent) === value.intentSha256
      && intent.operation === "instant-withdraw"
      && intent.user === address(manifest.identities.user)
      && intent.amountRaw === integerString(transaction.amountLpRaw, "instantWithdrawRejection.transaction.amountLpRaw")
      && intent.canonicalMessageSha256 === transaction.canonicalMessageSha256
      && intent.protectedPrestateSha256 === protectedState.beforeSha256
      && value.failedGateCount === 0
      && named.length > 0
      && named.every((gate) => gate.pass === true)
      && transaction.operation === "instant-withdraw"
      && transaction.mode === "rejection"
      && transaction.user === manifest.identities.user
      && transaction.vault === PARTNER_ROUTE.vault
      && integerString(transaction.amountLpRaw, "instantWithdrawRejection.transaction.amountLpRaw") > 0n
      && integerString(transaction.quoteAssetRaw, "instantWithdrawRejection.transaction.quoteAssetRaw") > 0n
      && integerString(transaction.withdrawalWaitingPeriodSeconds, "instantWithdrawRejection.transaction.withdrawalWaitingPeriodSeconds") === 600n
      && transaction.disabledOperations === 0
      && transaction.withdrawAll === true
      && transaction.simulationErrorCode === 6015
      && transaction.noEventCount === 0
      && errorExact
      && logsExact
      && packetExact
      && deploymentsExact
      && contextsExact;
    add(gates, "600-second mode rejects canonical instant withdrawal with exact Custom 6015", pass, { verdict: value.verdict, broadcast: value.broadcast, readyForBroadcast: value.readyForBroadcast, transaction, simulation, logs, deployments, protectedState, rejectionReadback }, { verdict: "PARTNER_INSTANT_WITHDRAW_REJECTION_PASS", broadcast: false, readyForBroadcast: false, operation: "instant-withdraw", mode: "rejection", user: manifest.identities.user, vault: PARTNER_ROUTE.vault, wait: 600n, disabledOperations: 0, error: { InstructionError: [0, { Custom: 6015 }] }, source: "instant_withdraw_vault.rs:69", eventCount: 0, packet: "one canonical signed v0 packet", deployments: expectedDeployments }, "rerun the maintained rejection-only simulator; never add a wait=0 config update to the accepted packet");
  } else if (name === "prematureClaim") {
    exactKeys(value, ["verdict", "broadcast", "readyForBroadcast", "routeId", "routeSpecSha256", "intentSha256", "intent", "prestateContextSlot", "bankBlockTime", "errorCode", "requestOrigin", "simulation", "transaction", "protectedState", "protectedSnapshotEvidence", "deployments", "failedGateCount", "gates"], "prematureClaim");
    const intent = userRuntimeIntent(value.intent, "prematureClaim.intent");
    const simulation = record(value.simulation, "prematureClaim.simulation");
    exactKeys(simulation, ["prestateSlot", "contextSlot", "err", "unitsConsumed", "logsSha256"], "prematureClaim.simulation");
    const transaction = record(value.transaction, "prematureClaim.transaction");
    exactKeys(transaction, ["operation", "mode", "user", "vault", "receipt", "requestSignature", "requestSlot", "withdrawableFromTs", "amountLpEscrowed", "amountAssetToWithdrawDecimalBits", "packetBytes", "serializedPacketBase64", "serializedPacketSha256", "feeLamports", "expectedSignature", "instruction", "canonicalMessageSha256"], "prematureClaim.transaction");
    const named = Array.isArray(value.gates) ? value.gates.map((item) => record(item, "prematureClaim.gate")) : [];
    const logGate = named.find((gate) => gate.name === "premature claim logs identify WithdrawalNotYetAvailable") ?? null;
    const logs = Array.isArray(logGate?.observed) && logGate.observed.every((line) => typeof line === "string") ? logGate.observed as string[] : [];
    const errorExact = canonicalJson(simulation.err) === canonicalJson({ InstructionError: [0, { Custom: 6012 }] });
    const logExact = logs.length === 5 && logs[0] === `Program ${PARTNER_ROUTE.programs.voltrVault} invoke [1]` && logs[1] === "Program log: Instruction: WithdrawVault" && logs[2]?.includes("Error Code: WithdrawalNotYetAvailable. Error Number: 6012.") === true && logs[4] === `Program ${PARTNER_ROUTE.programs.voltrVault} failed: custom program error: 0x177c` && sha256(logs.join("\n")) === simulation.logsSha256;
    const deployments = record(value.deployments, "prematureClaim.deployments");
    exactKeys(deployments, ["before", "after"], "prematureClaim.deployments");
    const expectedDeployments = PARTNER_ROUTE.deployments.map((item) => ({ ...item, deployedSlot: item.deployedSlot.toString() }));
    const deploymentsExact = canonicalJson(deployments.before) === canonicalJson(expectedDeployments)
      && canonicalJson(deployments.after) === canonicalJson(expectedDeployments);
    const prematureOrigin = requestOrigin(value.requestOrigin, "prematureClaim.requestOrigin");
    const pass = value.verdict === "PARTNER_WITHDRAW_CLAIM_PREMATURE_REJECTION_PASS" && value.broadcast === false && value.readyForBroadcast === false && value.failedGateCount === 0 && value.errorCode === 6012 && sameRequestOrigin(prematureOrigin, manifest.requestOrigin) && transaction.operation === "withdraw-claim" && transaction.mode === "premature" && transaction.user === manifest.identities.user && transaction.vault === PARTNER_ROUTE.vault && transaction.receipt === requestEvent?.requestWithdrawVaultReceipt && transaction.requestSignature === requestSignature && transaction.requestSlot === requestSlot && integerString(transaction.withdrawableFromTs, "prematureClaim.transaction.withdrawableFromTs") === requestEvent?.withdrawableFromTs && integerString(transaction.amountLpEscrowed, "prematureClaim.transaction.amountLpEscrowed") === manifest.amounts.requestWithdrawLpRaw && integerString(transaction.amountAssetToWithdrawDecimalBits, "prematureClaim.transaction.amountAssetToWithdrawDecimalBits") === requestEvent?.amountAssetToWithdrawDecimalBits && typeof value.bankBlockTime === "number" && typeof requestEvent?.withdrawableFromTs === "bigint" && BigInt(value.bankBlockTime) < requestEvent.withdrawableFromTs && errorExact && logExact && deploymentsExact;
    add(gates, "premature claim exact Custom 6012/log/origin/deployment and simulation-only boundary", pass, { broadcast: value.broadcast, readyForBroadcast: value.readyForBroadcast, transaction, bankBlockTime: value.bankBlockTime, err: simulation.err, logs, logsSha256: simulation.logsSha256, deployments }, { requestSignature, requestSlot, error: { InstructionError: [0, { Custom: 6012 }] }, log: "exact WithdrawalNotYetAvailable 6012 path", deployments: expectedDeployments, broadcast: false }, "rerun premature mode against the exact live receipt before its 600-second deadline");
    const packetBase64 = typeof transaction.serializedPacketBase64 === "string" ? transaction.serializedPacketBase64 : null;
    const packetBytes = typeof transaction.packetBytes === "number" ? transaction.packetBytes : null;
    const packet = packetBase64 === null ? null : Buffer.from(packetBase64, "base64");
    const packetCanonical = packet !== null && packet.toString("base64") === packetBase64 && packetBytes === packet.length && sha256(packet) === transaction.serializedPacketSha256;
    let packetMessageSha256: string | null = null;
    let packetSignature: string | null = null;
    try {
      if (packet !== null) {
        const decoded = VersionedTransaction.deserialize(packet);
        packetMessageSha256 = sha256(decoded.message.serialize());
        packetSignature = decoded.signatures.length === 1 ? bs58.encode(decoded.signatures[0]!) : null;
      }
    } catch { /* falsifiable packet gate below */ }
    const protectedState = record(value.protectedState, "prematureClaim.protectedState");
    const protectedEvidence = validatedProtectedSnapshotEnvelope(value.protectedSnapshotEvidence, protectedState, "prematureClaim");
    const protectedExact = protectedState.schemaVersion === 1
      && shaField(protectedState, "beforeSha256", "prematureClaim.protectedState") === shaField(protectedState, "afterSha256", "prematureClaim.protectedState")
      && shaField(protectedState, "addressSetSha256", "prematureClaim.protectedState") === manifest.transactions.withdrawRequest.protectedAddressSetSha256
      && protectedEvidence.before.contextSlot <= (simulation.contextSlot as number)
      && protectedEvidence.after.contextSlot >= (simulation.contextSlot as number)
      && canonicalJson(protectedEvidence.before.rows) === canonicalJson(protectedEvidence.after.rows);
    const packetPass = packetCanonical && packetMessageSha256 === transaction.canonicalMessageSha256 && packetSignature === transaction.expectedSignature && protectedExact && executionIntentSha256(intent) === value.intentSha256 && intent.operation === "withdraw-claim" && intent.user === address(manifest.identities.user) && intent.amountRaw === integerString(transaction.amountLpEscrowed, "prematureClaim.transaction.amountLpEscrowed") && intent.canonicalMessageSha256 === transaction.canonicalMessageSha256 && intent.protectedPrestateSha256 === protectedState.beforeSha256;
    add(gates, "premature claim persisted packet/protected state independently replayable", packetPass, { packetBytes, serializedPacketSha256: transaction.serializedPacketSha256 ?? null, packetMessageSha256, packetSignature, protectedState }, { serializedPacketBase64: "canonical VersionedTransaction", packetSha256: "SHA-256(packet)", canonicalMessageSha256: transaction.canonicalMessageSha256, protectedState: "same exact address set and unchanged pre/post hash" }, "retain the exact unsigned/signed premature packet and protected-state envelope before the deadline; RPC historical trust remains provider-bound");
  } else if (name === "withdrawalScanner") {
    const scan = scannerEvidence(value, manifest, requestEvent, requestSignature, requestSlot, requestEventIndex, idleOriginRaw, gates);
    add(gates, "scanner query/origin provenance retained", scan !== null, scan ? { observationContextSlot: scan.observationContextSlot, generationFingerprint: scan.generationFingerprint, requestOrigin: scan.requestOrigin, rawQuerySha256: scan.rawQuerySha256, queryConfigSha256: scan.queryConfigSha256 } : null, "confirmed scanner response with exact raw-query/config hashes and request origin tuple", "retain the confirmed scanner response; RPC/provider archive completeness remains an explicit epistemic residual");
    return scan;
  } else if (name === "restoration") {
    add(gates, "restoration artifact carries strict planner/outbox/leg sections", artifactHas(value, ["sources", "plan", "durableOutbox", "confirmedLegs", "shortfallRecomputations"]), Object.keys(value).sort(), ["sources", "plan", "durableOutbox", "confirmedLegs", "shortfallRecomputations"], "produce the complete restoration artifact; semantic and chain verification runs after all artifacts load");
  } else if (name === "earnAdapter") {
    add(gates, "Earn adapter carries source/outbox/movement/replay sections", artifactHas(value, ["sourceBindings", "outboxContract", "movement", "sharedReplay"]), Object.keys(value).sort(), ["sourceBindings", "outboxContract", "movement", "sharedReplay"], "produce the complete source-bound Earn adapter evidence; exact verification runs after all artifacts load");
  } else if (name === "negativeMutations") {
    const mutations = value.mutations;
    const omitted = (operation: "deposit" | "withdraw") => {
      const count = operation === "deposit" ? 31 : 28;
      const constrained = new Set<number>(operation === "deposit" ? DEPOSIT_CONSTRAINED_INDEXES : WITHDRAW_CONSTRAINED_INDEXES);
      return Array.from({ length: count }, (_, index) => index).filter((index) => !constrained.has(index)).map((index) => `omitted-index-${index}`);
    };
    const expectedIds = REQUIRED_STRATEGIES.flatMap((strategyId) => (["deposit", "withdraw"] as const).flatMap((operation) => [...REQUIRED_NEGATIVE_MUTATIONS, ...omitted(operation)].map((mutation) => `${strategyId}:${operation}:${mutation}`)));
    const observedIds = Array.isArray(mutations) ? mutations.map((mutation) => {
      const row = record(mutation, "negativeMutations.mutation");
      exactKeys(row, ["id", "enforcementLayer", "recentBlockhash", "serializedMessageBase64", "serializedMessageSha256", "accepted", "broadcast", "simulationError", "preProtectedStateSha256", "postProtectedStateSha256", "preProtectedContextSlot", "postProtectedContextSlot"], "negativeMutations.mutation");
      if (typeof row.recentBlockhash !== "string" || row.recentBlockhash.length === 0) throw new Error("negativeMutations.mutation.recentBlockhash must be present");
      const serializedMessage = Buffer.from(stringField(row, "serializedMessageBase64", "negativeMutations.mutation"), "base64");
      if (serializedMessage.toString("base64") !== row.serializedMessageBase64 || sha256(serializedMessage) !== shaField(row, "serializedMessageSha256", "negativeMutations.mutation")) throw new Error("negativeMutations.mutation serialized message envelope is not canonical");
      shaField(row, "serializedMessageSha256", "negativeMutations.mutation");
      shaField(row, "preProtectedStateSha256", "negativeMutations.mutation");
      shaField(row, "postProtectedStateSha256", "negativeMutations.mutation");
      if (typeof row.preProtectedContextSlot !== "number" || !Number.isSafeInteger(row.preProtectedContextSlot) || row.preProtectedContextSlot <= 0 || typeof row.postProtectedContextSlot !== "number" || !Number.isSafeInteger(row.postProtectedContextSlot) || row.postProtectedContextSlot < row.preProtectedContextSlot) throw new Error("negativeMutations.mutation protected context slots are malformed");
      return stringField(row, "id", "negativeMutations.mutation");
    }) : [];
    add(gates, "negative mutation matrix exact and no-broadcast", value.broadcast === false && observedIds.length === expectedIds.length && observedIds.every((id, index) => id === expectedIds[index]) && (mutations as unknown[]).every((mutation) => { const row = record(mutation, "negativeMutations.mutation"); return typeof row.enforcementLayer === "string" && row.accepted === false && row.broadcast === false && canonicalJson(row.simulationError) !== "null" && canonicalJson(row.simulationError) !== canonicalJson({ kind: "simulation-not-run" }) && row.preProtectedStateSha256 === row.postProtectedStateSha256; }), { broadcast: value.broadcast, count: observedIds.length, ids: observedIds }, { broadcast: false, count: expectedIds.length, ids: expectedIds, accepted: false, simulationError: "actual non-null failed simulation error", protectedState: "unchanged" }, "generate every base and omitted-index mutation from the maintained canonical wrapper generator in exact strategy/direction/name order");
    let reconstruction: Readonly<{ pass: boolean; observed: unknown; expected: unknown }> = { pass: false, observed: "policy artifact unavailable", expected: "exact eight-policy artifact" };
    if (policyArtifact) {
      try { reconstruction = verifyNegativeMutationArtifact(value, policyArtifact, manifest.amounts.managerAssetRaw); }
      catch (error) { reconstruction = { pass: false, observed: error instanceof Error ? error.message : String(error), expected: "canonical mutation packet reconstruction" }; }
    }
    add(gates, "negative mutations source-bound producer-observed confirmed RPC envelopes and independently reconstructed wires", reconstruction.pass, reconstruction.observed, reconstruction.expected, "rebuild every exact mutation packet from the checked-out policy/source catalog and retain the producer-observed confirmed RPC error envelope without claiming historical replay");
  } else {
    const conservation = record(value.conservation, "finalReconciliation.conservation");
    add(gates, "final reconciliation schema is conservation-complete", typeof value.finalContextSlot === "number" && value.finalContextSlot > 0 && Array.isArray(value.activeReceipts) && artifactHas(conservation, ["idleRaw", "strategyPositionsRaw", "lpSupplyRaw", "vaultTotalValueRaw"]), { finalContextSlot: value.finalContextSlot, activeReceipts: value.activeReceipts, conservation }, "final confirmed readback with idle + four positions + LP supply + vault total", "reconcile all four strategy positions and vault totals at or after the final claim slot");
  }
  return null;
}

export async function verifyFourMarketLifecycle(evidencePath: string, commitment: Commitment = "confirmed") {
  const gates: Gate[] = [];
  if (commitment !== "confirmed") throw new Error("four-market verifier requires --commitment confirmed");
  let manifest: FourMarketManifest | null = null;
  try {
    const resolvedEvidencePath = resolve(evidencePath);
    const evidenceStat = lstatSync(resolvedEvidencePath);
    if (!evidenceStat.isFile() || evidenceStat.isSymbolicLink() || realpathSync(resolvedEvidencePath) !== resolvedEvidencePath) throw new Error("lifecycle manifest must be a regular non-symlink file");
    manifest = parseManifest(resolvedEvidencePath);
    add(gates, "strict lifecycle manifest schema", true, "schema v1", "exact schema v1", "keep the manifest schema stable");
  }
  catch (error) {
    add(gates, "strict lifecycle manifest schema", false, error instanceof Error ? error.message : String(error), "exact confirmed four-market manifest", "create the manifest with all 13 confirmed transaction refs and seven proof artifacts");
    for (const name of ["vault/four strategies", "authority/eight policies", "manager API", "user deposit and withdrawal", "withdrawal restoration", "Earn adapter", "confirmed lifecycle", "execution safety"] as const) add(gates, `required gate ${name}`, false, "manifest unavailable", "independently verified evidence", "produce the smallest missing evidence artifact");
    return { verdict: "BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_FAIL", broadcast: false, routeSpecSha256: fourMarketRouteSpecSha256(), commitment, evidencePath, failedGateCount: gates.filter(({ pass }) => !pass).length, gates } as const;
  }
  const rpcUrl = process.env.SOLANA_RPC_URL;
  if (!rpcUrl) { add(gates, "mainnet RPC configured", false, null, "SOLANA_RPC_URL", "run with the mounted non-secret RPC environment"); return { verdict: "BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_FAIL", broadcast: false, routeSpecSha256: fourMarketRouteSpecSha256(), commitment, evidencePath, failedGateCount: gates.filter(({ pass }) => !pass).length, gates } as const; }
  const connection = new Connection(rpcUrl, "confirmed");
  let genesis: string | null = null;
  try { genesis = await connection.getGenesisHash(); } catch { /* gate below */ }
  add(gates, "mainnet genesis", genesis === PARTNER_ROUTE.genesisHash, genesis, PARTNER_ROUTE.genesisHash, "point SOLANA_RPC_URL at Solana mainnet-beta");
  const identity = manifest.identities;
  const lifecycleSignatures = REQUIRED_TXS.map((name) => manifest.transactions[name].signature);
  const lifecycleSlots = REQUIRED_TXS.map((name) => manifest.transactions[name].slot);
  add(gates, "lifecycle signatures are unique", new Set(lifecycleSignatures).size === REQUIRED_TXS.length, lifecycleSignatures, "13 unique confirmed signatures", "rerun only the duplicated logical operation with a new exact persisted intent");
  add(gates, "lifecycle slots are strictly monotonic in required order", lifecycleSlots.every((slot, index) => index === 0 || slot > lifecycleSlots[index - 1]!), REQUIRED_TXS.map((name, index) => ({ operation: name, slot: lifecycleSlots[index] })), "user deposit < eight route legs < fallback Main allocation < request < named Main restoration < claim", "run the first out-of-order lifecycle step again rather than splicing unrelated history");
  add(gates, "manifest identity binding", identity.vault === PARTNER_ROUTE.vault && identity.lpMint === PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint && identity.settings === PARTNER_ROUTE.squads.settings && identity.manager === PARTNER_ROUTE.squads.manager && identity.guardian === PARTNER_ROUTE.squads.guardian && identity.assetMint === PARTNER_ROUTE.asset.mint, identity, { vault: PARTNER_ROUTE.vault, lpMint: PARTNER_FOUR_MARKET_ROUTE.commonVoltr.lpMint, settings: PARTNER_ROUTE.squads.settings, manager: PARTNER_ROUTE.squads.manager, guardian: PARTNER_ROUTE.squads.guardian, assetMint: PARTNER_ROUTE.asset.mint, user: "valid manifest-bound testing user" }, "regenerate evidence for the maintained vault, LP mint, and guardian");
  const artifactJson = verifyRef(resolve(evidencePath), manifest.policyCatalog, "policy catalog", gates);
  verifyRef(resolve(evidencePath), manifest.policyAuthorization, "policy authorization", gates);
  let policyResult: Awaited<ReturnType<typeof verifyExistingRuntimePolicies>> | null = null;
  let loadedPolicyCatalog: ReturnType<typeof loadRuntimePolicyArtifact> | null = null;
  try {
    const policyPath = resolveChild(resolve(evidencePath), manifest.policyCatalog.path);
    const authorizationPath = resolveChild(resolve(evidencePath), manifest.policyAuthorization.path);
    const loaded = loadRuntimePolicyArtifact(policyPath);
    loadedPolicyCatalog = loaded;
    add(gates, "policy catalog artifact hash", loaded.artifact.artifactSha256 === manifest.policyCatalog.artifactSha256 && loaded.artifact.routeSpecSha256 === fourMarketRouteSpecSha256(), { artifactSha256: loaded.artifact.artifactSha256, routeSpecSha256: loaded.artifact.routeSpecSha256 }, { artifactSha256: manifest.policyCatalog.artifactSha256, routeSpecSha256: fourMarketRouteSpecSha256() }, "regenerate and authorize the exact eight-policy catalog");
    const authorization = loadPolicyCatalogAuthorization(authorizationPath, policyPath, manifest.policyAuthorization.fileSha256);
    const effective = effectiveRouteAuthorizationDigest(loaded, authorization);
    add(gates, "policy catalog authorization, create hashes, effective route authorization, and checked-out source binding", authorization.authorization.authorizationSha256 === manifest.policyAuthorization.authorizationSha256 && authorization.authorization.entries.length === 8, { authorizationSha256: authorization.authorization.authorizationSha256, entryCount: authorization.authorization.entries.length, sourceAggregateSha256: authorization.authorization.sourceAggregateSha256, createDataSha256: authorization.authorization.entries.map(({ strategyId, operation, policyCreateDataSha256 }) => ({ strategyId, operation, policyCreateDataSha256 })), effectiveRouteAuthorizationSha256: effective.sha256 }, { authorizationSha256: manifest.policyAuthorization.authorizationSha256, routeAuthorizationSha256: manifest.routeAuthorizationSha256, entryCount: 8, sourceBinding: "exact current maintained policy sources" }, "regenerate one authorization only after the full catalog and source tree are frozen");
    add(gates, "effective route authorization digest exact", effective.sha256 === manifest.routeAuthorizationSha256, effective.sha256, manifest.routeAuthorizationSha256, "rebuild the manifest from the exact catalog plus authorization envelope; never copy a manager intent hash");
    const omittedInventory = policyOmittedIndexInventory(loaded);
    add(gates, "policy constrained and omitted-index inventory is complete", omittedInventory.length === 8 && omittedInventory.every(({ accountCount, constrained, omitted }) => constrained.length + omitted.length === accountCount && new Set([...constrained, ...omitted]).size === accountCount), omittedInventory, "all eight canonical inner vectors partitioned into exact constrained and named omitted indexes", "recompile the catalog and investigate the first omitted account whose index or role changed");
    const legacyPolicies = await verifyLegacyVoltrPolicyCatalog(rpcUrl, undefined, "finalized");
    add(gates, "immutable legacy policy generation remains exactly classified", legacyPolicies.failedGateCount === 0, { verdict: legacyPolicies.verdict, failedGateCount: legacyPolicies.failedGateCount }, { verdict: "PARTNER_LEGACY_VOLTR_POLICIES_CONFIRMED_PASS", failedGateCount: 0 }, "investigate any legacy account or creation-origin drift");
    policyResult = await verifyExistingRuntimePolicies(policyPath);
  } catch (error) { add(gates, "eight-policy on-chain readback", false, error instanceof Error ? error.message : String(error), "eight exact policies and terminal seed 50", "install or reconcile the exact eight-policy catalog before lifecycle proof"); }
  add(gates, "required eight-policy readback", policyResult?.verdict === "PARTNER_RUNTIME_POLICIES_FINALIZED_PASS" && policyResult.failedGateCount === 0 && policyResult.policies.length === 8, policyResult ? { verdict: policyResult.verdict, failedGateCount: policyResult.failedGateCount, count: policyResult.policies.length } : null, { verdict: "PARTNER_RUNTIME_POLICIES_FINALIZED_PASS", failedGateCount: 0, count: 8 }, "make all eight policy accounts and their origins pass independently");
  try {
    const minimumSlot = Math.max(...REQUIRED_TXS.map((name) => manifest.transactions[name].slot));
    const isolation = await verifyNonCatalogSquadsPoliciesIsolated(rpcUrl, 43n, 50n, minimumSlot, "confirmed", [{ firstSeed: 17n, lastSeed: 24n }]);
    gates.push(...isolation.gates.map((gate) => ({ ...gate, name: `Squads namespace: ${gate.name}` })));
    add(gates, "Squads Settings authority and every non-catalog policy isolated", isolation.failedGateCount === 0 && isolation.currentSeed >= 50n && isolation.policies.every((policy) => !policy.exists || !policy.programs.includes(PARTNER_ROUTE.programs.voltrVault)), { verdict: isolation.verdict, contextSlot: isolation.contextSlot, currentSeed: isolation.currentSeed, nonCatalogPolicies: isolation.policies }, { settings: { signer: PARTNER_ROUTE.setupAdmin, permissionsMask: 7, threshold: 1, timelock: 0, catalogTerminalSeed: 50n }, seeds: "17..24 immutable exact legacy catalog; 43..50 exact current catalog; every other live policy is generated-decoded and constrained away from Voltr" }, "repair the first unclassified policy that can authorize a Voltr instruction");
  } catch (error) {
    add(gates, "Squads Settings authority and every non-catalog policy isolated", false, error instanceof Error ? error.message : String(error), "full confirmed Settings plus every non-catalog policy through the current seed", "run the full Squads namespace decoder at or after the lifecycle slot");
  }

  let vaultGateCount = 0;
  try {
    const accounts = await deriveVoltrAccountsForStrategy(PARTNER_ROUTE, PARTNER_ROUTE.strategy.reserve);
    const state = await confirmedSnapshots(rpcUrl, [PARTNER_ROUTE.vault, accounts.lpMint, accounts.idleAta, PARTNER_ROUTE.asset.mint]);
    const vault = verifyVaultCurrentState({ route: PARTNER_ROUTE, accounts, vault: state.accounts[0] ?? null, lpMint: state.accounts[1] ?? null, idleAta: state.accounts[2] ?? null, assetMint: state.accounts[3] ?? null });
    vaultGateCount = vault.failedGateCount;
    gates.push(...vault.gates.map((gate) => ({ ...gate, name: `vault: ${gate.name}` })));
  } catch (error) { add(gates, "vault readback", false, error instanceof Error ? error.message : String(error), "exact 600-second Voltr vault", "repair the vault readback before proving strategies"); vaultGateCount = 1; }
  const strategyGraphs = new Map<StrategyId, ReserveGraph>();
  const strategyBuilders = new Map<StrategyId, Awaited<ReturnType<typeof createVoltrRouteBuilder>>>();
  const strategyAccounts = new Map<StrategyId, Awaited<ReturnType<typeof deriveVoltrAccountsForStrategy>>>();
  for (const strategyId of REQUIRED_STRATEGIES) {
    const expected = partnerStrategyIdentity(strategyId);
    const manifestRow = manifest.strategies.find(({ id }) => id === strategyId)!;
    add(gates, `${strategyId} frozen strategy identity`, manifestRow.reserve === expected.reserve && manifestRow.strategyReceipt === expected.voltr.strategyInitReceipt && manifestRow.strategyAssetAta === expected.voltr.strategyAssetAta, manifestRow, { reserve: expected.reserve, strategyReceipt: expected.voltr.strategyInitReceipt, strategyAssetAta: expected.voltr.strategyAssetAta }, `regenerate the ${strategyId} strategy evidence from the frozen reserve catalog`);
    try {
      const route = partnerBuilderRoute(strategyId);
      const accounts = await deriveVoltrAccountsForStrategy(route, expected.reserve);
      add(gates, `${strategyId} derived Voltr PDAs exact`, accounts.strategyInitReceipt === expected.voltr.strategyInitReceipt && accounts.strategyAuth === expected.voltr.strategyAuth && accounts.adaptorAddReceipt === PARTNER_FOUR_MARKET_ROUTE.commonVoltr.adaptorAddReceipt, { strategyInitReceipt: accounts.strategyInitReceipt, strategyAuth: accounts.strategyAuth, adaptorAddReceipt: accounts.adaptorAddReceipt }, { strategyInitReceipt: expected.voltr.strategyInitReceipt, strategyAuth: expected.voltr.strategyAuth, adaptorAddReceipt: PARTNER_FOUR_MARKET_ROUTE.commonVoltr.adaptorAddReceipt }, `stop if the pinned SDK derives a different ${strategyId} PDA`);
      const reserve = await loadMainReserveGraph(rpcUrl, route, accounts.strategyAuth, "confirmed");
      const graphExact = Object.entries(expected.graph).every(([key, value]) => reserve.graph[key as keyof ReserveGraph] === value);
      add(gates, `${strategyId} decoded reserve graph exact`, graphExact, reserve.graph, expected.graph, `refresh the frozen graph only through a new route-spec approval`);
      strategyGraphs.set(strategyId, reserve.graph);
      strategyAccounts.set(strategyId, accounts);
      strategyBuilders.set(strategyId, await createVoltrRouteBuilder(route, reserve.graph));
      const state = await confirmedSnapshots(rpcUrl, [accounts.strategyInitReceipt, reserve.graph.userMetadata, reserve.graph.obligation, reserve.graph.obligationFarm, manifestRow.strategyAssetAta, accounts.adaptorAddReceipt], reserve.contextSlot);
      const strategyGates = verifyStrategyBootstrap({ route, accounts, graph: reserve.graph, strategyReceipt: state.accounts[0] ?? null, userMetadata: state.accounts[1] ?? null, obligation: state.accounts[2] ?? null, obligationFarm: state.accounts[3] ?? null });
      gates.push(...strategyGates.map((gate) => ({ ...gate, name: `${strategyId}: ${gate.name}` })));
      const strategyReceipt = state.accounts[0] ? getStrategyInitReceiptDecoder().decode(state.accounts[0]!.data) : null;
      const obligationSnapshot = state.accounts[2] ?? null;
      if (!obligationSnapshot) {
        add(gates, `${strategyId} obligation is exact supply-only named-reserve position`, strategyReceipt?.positionValue === 0n, { obligation: null, strategyPositionRaw: strategyReceipt?.positionValue ?? null }, { obligation: "absent only while flat", strategyPositionRaw: 0n }, `restore or close the ${strategyId} obligation consistently with its strategy receipt`);
      } else {
        try {
          const obligation = Obligation.decode(Buffer.from(obligationSnapshot.data));
          const deposits = obligation.deposits.filter(({ depositedAmount }) => !depositedAmount.isZero()).map(({ depositReserve, depositedAmount, marketValueSf }) => ({ reserve: depositReserve.toString(), collateralRaw: depositedAmount.toString(), marketValueSf: marketValueSf.toString() }));
          const borrows = obligation.borrows.filter(({ borrowedAmountSf }) => !borrowedAmountSf.isZero()).map(({ borrowReserve, borrowedAmountSf }) => ({ reserve: borrowReserve.toString(), amountSf: borrowedAmountSf.toString() }));
          const flat = strategyReceipt?.positionValue === 0n;
          const exact = obligation.owner.toString() === accounts.strategyAuth && obligation.lendingMarket.toString() === expected.graph.lendingMarket && borrows.length === 0 && obligation.hasDebt === 0 && (flat ? deposits.length === 0 : deposits.length === 1 && deposits[0]!.reserve === expected.reserve && BigInt(deposits[0]!.collateralRaw) > 0n);
          add(gates, `${strategyId} obligation is exact supply-only named-reserve position`, exact, { owner: obligation.owner.toString(), lendingMarket: obligation.lendingMarket.toString(), strategyPositionRaw: strategyReceipt?.positionValue ?? null, deposits, borrows, hasDebt: obligation.hasDebt }, { owner: accounts.strategyAuth, lendingMarket: expected.graph.lendingMarket, deposits: flat ? [] : [{ reserve: expected.reserve, collateralRaw: ">0" }], borrows: [], hasDebt: 0 }, `remove a mixed deposit/borrow or reconcile the exact ${strategyId} collateral position`);
        } catch (error) {
          add(gates, `${strategyId} obligation is exact supply-only named-reserve position`, false, error instanceof Error ? error.message : String(error), "decoded supply-only obligation", `decode and reconcile the ${strategyId} obligation deposit/borrow sets`);
        }
      }
      gates.push(...verifyAdaptorReceipt(route, accounts.adaptorAddReceipt, state.accounts[5] ?? null).map((gate) => ({ ...gate, name: `${strategyId}: ${gate.name}` })));
      const ata = state.accounts[4];
      let decodedAta: ReturnType<ReturnType<typeof getTokenDecoder>["decode"]> | null = null;
      try { decodedAta = ata ? getTokenDecoder().decode(ata.data) : null; } catch { decodedAta = null; }
      add(gates, `${strategyId} strategy USDC ATA`, ata?.owner === PARTNER_ROUTE.programs.token && ata.address === expected.voltr.strategyAssetAta && decodedAta?.mint === PARTNER_ROUTE.asset.mint && decodedAta.owner === expected.voltr.strategyAuth, ata ? { address: ata.address, ownerProgram: ata.owner, mint: decodedAta?.mint ?? null, authority: decodedAta?.owner ?? null } : null, { address: expected.voltr.strategyAssetAta, ownerProgram: PARTNER_ROUTE.programs.token, mint: PARTNER_ROUTE.asset.mint, authority: expected.voltr.strategyAuth }, `create and confirm the exact ${strategyId} strategy USDC ATA`);
    } catch (error) { add(gates, `${strategyId} strategy graph readback`, false, error instanceof Error ? error.message : String(error), "exact active native-Kamino graph", `rerun the ${strategyId} strategy compatibility/bootstrap probe`); }
  }
  try {
    const minimumSlot = Math.max(...REQUIRED_TXS.map((name) => manifest.transactions[name].slot));
    const response = await connection.getProgramAccounts(new PublicKey(PARTNER_ROUTE.programs.voltrVault), {
      commitment: "confirmed",
      withContext: true,
      minContextSlot: minimumSlot,
      filters: [
        { memcmp: { offset: 0, bytes: bs58.encode(Uint8Array.from(getStrategyInitReceiptDiscriminatorBytes())) } },
        { memcmp: { offset: 8, bytes: PARTNER_ROUTE.vault } },
      ],
    });
    const decoded = response.value.map(({ pubkey, account }) => {
      const receipt = getStrategyInitReceiptDecoder().decode(account.data);
      return { address: pubkey.toBase58(), owner: account.owner.toBase58(), vault: receipt.vault, strategy: receipt.strategy, adaptorProgram: receipt.adaptorProgram, version: receipt.version };
    }).sort((left, right) => left.address.localeCompare(right.address));
    const expected = REQUIRED_STRATEGIES.map((id) => {
      const strategy = partnerStrategyIdentity(id);
      return { address: strategy.voltr.strategyInitReceipt, owner: PARTNER_ROUTE.programs.voltrVault, vault: PARTNER_ROUTE.vault, strategy: strategy.reserve, adaptorProgram: PARTNER_ROUTE.programs.kaminoAdaptor, version: 1 };
    }).sort((left, right) => left.address.localeCompare(right.address));
    add(gates, "current Voltr strategy receipt namespace is exactly four", response.context.slot >= minimumSlot && canonicalJson(decoded) === canonicalJson(expected), { contextSlot: response.context.slot, receipts: decoded }, { contextSlot: `>=${minimumSlot}`, receipts: expected }, "close an unexpected strategy or regenerate evidence only after the exact four-receipt namespace is restored");
  } catch (error) {
    add(gates, "current Voltr strategy receipt namespace is exactly four", false, error instanceof Error ? error.message : String(error), "confirmed discriminator+vault scan returns exactly the four frozen receipts", "run the complete Voltr program-account namespace scan at or after the lifecycle slot");
  }
  try {
    const deployments = await loadDeploymentIdentities(rpcUrl, PARTNER_ROUTE, undefined, "confirmed");
    gates.push(...verifyDeploymentIdentities(PARTNER_ROUTE, deployments.identities).map((gate) => ({ ...gate, name: `deployment: ${gate.name}` })));
  } catch (error) { add(gates, "approved deployment identities", false, error instanceof Error ? error.message : String(error), "unchanged Voltr/Kamino/Farms/K-Lend/Squads deployments", "refresh deployment identities and rerun without changing the route hash"); }

  let managerLookupTable: AddressLookupTableAccount | null = null;
  try {
    const minimumSlot = Math.max(...REQUIRED_TXS.map((name) => manifest.transactions[name].slot));
    const response = await connection.getAddressLookupTable(new PublicKey(PARTNER_ROUTE.lookupTable.address), { commitment: "confirmed", minContextSlot: minimumSlot });
    managerLookupTable = response.value;
    const orderedAddresses = response.value?.state.addresses.map((key) => key.toBase58()) ?? [];
    const identityExact = response.context.slot >= minimumSlot && response.value?.key.toBase58() === PARTNER_ROUTE.lookupTable.address && response.value.state.authority?.toBase58() === PARTNER_FOUR_MARKET_ROUTE.lookupTable.authority && orderedAddresses.length === PARTNER_FOUR_MARKET_ROUTE.lookupTable.addressCount && sha256(orderedAddresses.join("\n")) === PARTNER_FOUR_MARKET_ROUTE.lookupTable.orderedAddressesSha256;
    add(gates, "manager ALT current identity and ordering exact", identityExact, { contextSlot: response.context.slot, address: response.value?.key.toBase58() ?? null, authority: response.value?.state.authority?.toBase58() ?? null, addressCount: orderedAddresses.length, orderedAddressesSha256: sha256(orderedAddresses.join("\n")) }, { contextSlot: `>=${minimumSlot}`, address: PARTNER_ROUTE.lookupTable.address, authority: PARTNER_FOUR_MARKET_ROUTE.lookupTable.authority, addressCount: PARTNER_FOUR_MARKET_ROUTE.lookupTable.addressCount, orderedAddressesSha256: PARTNER_FOUR_MARKET_ROUTE.lookupTable.orderedAddressesSha256 }, "stop if the pinned ALT changed; do not accept different lookup indexes");
  } catch (error) { add(gates, "manager ALT current identity and ordering exact", false, error instanceof Error ? error.message : String(error), PARTNER_FOUR_MARKET_ROUTE.lookupTable, "reload the pinned ALT at or after the last lifecycle slot"); }

  const responses = new Map<TxName, VersionedTransactionResponse | null>();
  const expectedTransactions = new Map<TxName, ExpectedTx>();
  const mainBuilder = strategyBuilders.get("main");
  const mainGraph = strategyGraphs.get("main");
  const mainAccounts = strategyAccounts.get("main");
  let userAccounts: Awaited<ReturnType<NonNullable<typeof mainBuilder>["userAccounts"]>> | null = null;
  try { if (!mainBuilder) throw new Error("Main builder was not loaded"); userAccounts = await mainBuilder.userAccounts(address(identity.user)); }
  catch (error) { add(gates, "canonical user account derivation", false, error instanceof Error ? error.message : String(error), "RouteSpec-derived user ATAs and receipt PDA", "use a valid testing user and rerun canonical account derivation"); }
  const allPostRead = [identity.vault, identity.lpMint, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta, identity.assetMint, ...REQUIRED_STRATEGIES.flatMap((id) => [partnerStrategyIdentity(id).voltr.strategyAssetAta])];
  const expectedFor = async (name: TxName, managerAmountOverride?: bigint): Promise<ExpectedTx | null> => {
    const managerDescriptor = managerTransactionDescriptor(name);
    if (managerDescriptor) {
      const { strategyId, operation } = managerDescriptor;
      const builder = strategyBuilders.get(strategyId);
      const graph = strategyGraphs.get(strategyId);
      const account = strategyAccounts.get(strategyId);
      const entry = loadedPolicyCatalog?.artifact.policies.find((candidate) => candidate.strategyId === strategyId && candidate.operation === operation);
      if (!builder || !graph || !account || !entry) return null;
      const managerAmountRaw = managerAmountOverride
        ?? (name === "managerMainRestorationWithdraw" ? manifest.amounts.restorationAssetRaw : manifest.amounts.managerAssetRaw);
      const manager = createNoopSigner(address(PARTNER_ROUTE.squads.manager));
      const inner = operation === "deposit" ? await builder.strategy.deposit(manager, managerAmountRaw) : await builder.strategy.withdraw(manager, managerAmountRaw);
      const wrapper = buildManagerWrapperForVerification(operation, entry, inner.canonical, managerAmountRaw);
      const instruction = expectedInstructionFromWeb3(wrapper.instruction);
      const compute = expectedInstructionFromWeb3(ComputeBudgetProgram.setComputeUnitLimit({ units: MANAGER_COMPUTE_UNIT_LIMIT }));
      const heap = expectedInstructionFromWeb3(ComputeBudgetProgram.requestHeapFrame({ bytes: MANAGER_HEAP_FRAME_BYTES }));
      return { operation: operation === "deposit" ? "manager-deposit" : "manager-withdraw", amountRaw: managerAmountRaw, policy: entry.policy, strategyId, signer: PARTNER_ROUTE.squads.guardian, requiredProgram: PARTNER_ROUTE.squads.program, allowedPrograms: [ComputeBudgetProgram.programId.toBase58(), PARTNER_ROUTE.squads.program], instruction, auxiliaryInstructions: [], orderedInstructions: [compute, heap, instruction], lookupTable: managerLookupTable, allowedTokenAccounts: [PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta, partnerStrategyIdentity(strategyId).voltr.strategyAssetAta, graph.reserveLiquiditySupply, graph.reserveCollateralSupplyVault], allowedMints: [PARTNER_ROUTE.asset.mint, graph.reserveCollateralMint], reserveLiquidity: graph.reserveLiquiditySupply, reserveCollateral: graph.reserveCollateralSupplyVault, strategyAssetAta: partnerStrategyIdentity(strategyId).voltr.strategyAssetAta, strategyAuth: account.strategyAuth, obligation: graph.obligation, allowedLamportAccounts: [PARTNER_ROUTE.squads.guardian, account.strategyAuth, graph.obligation], instructionCount: 3 };
    }
    if (!mainBuilder || !userAccounts || !mainAccounts || !mainGraph) return null;
    const user = createNoopSigner(address(identity.user));
    let instruction: ExpectedInstruction;
    const allowedPrograms: string[] = [PARTNER_ROUTE.programs.voltrVault as string];
    let auxiliaryInstructions: ExpectedInstruction[] = [];
    if (name === "userDeposit") {
      instruction = expectedInstructionFromCanonical((await mainBuilder.user.deposit({ user }, manifest.amounts.userDepositAssetRaw)).canonical);
      const ata = await getCreateAssociatedTokenIdempotentInstructionAsync({ payer: user, ata: userAccounts.userLpAta, owner: address(identity.user), mint: userAccounts.lpMint, systemProgram: PARTNER_ROUTE.programs.system, tokenProgram: PARTNER_ROUTE.programs.token }, { programAddress: PARTNER_ROUTE.programs.associatedToken });
      auxiliaryInstructions = [expectedInstructionFromKit(ata)];
      allowedPrograms.push(PARTNER_ROUTE.programs.associatedToken);
    }
    else if (name === "withdrawRequest") {
      instruction = expectedInstructionFromCanonical((await mainBuilder.user.requestWithdraw({ user, payer: user }, manifest.amounts.requestWithdrawLpRaw, true)).canonical);
      const ata = await getCreateAssociatedTokenIdempotentInstructionAsync({ payer: user, ata: userAccounts.requestWithdrawLpAta, owner: userAccounts.requestWithdrawVaultReceipt, mint: userAccounts.lpMint, systemProgram: PARTNER_ROUTE.programs.system, tokenProgram: PARTNER_ROUTE.programs.token }, { programAddress: PARTNER_ROUTE.programs.associatedToken });
      auxiliaryInstructions = [expectedInstructionFromKit(ata)];
      allowedPrograms.push(PARTNER_ROUTE.programs.associatedToken);
    }
    else if (name === "withdrawClaim") instruction = expectedInstructionFromCanonical((await mainBuilder.user.claimWithdraw(user)).canonical);
    else return null;
    const tokenAccounts = [mainAccounts.idleAta, userAccounts.userAssetAta, userAccounts.userLpAta, userAccounts.requestWithdrawLpAta];
    const orderedInstructions = auxiliaryInstructions.length > 0 ? [...auxiliaryInstructions, instruction] : [instruction];
    const operation = ({ userDeposit: "user-deposit", withdrawRequest: "withdraw-request", withdrawClaim: "withdraw-claim" } as const)[name as "userDeposit" | "withdrawRequest" | "withdrawClaim"];
    const amountRaw = name === "userDeposit" ? manifest.amounts.userDepositAssetRaw : manifest.amounts.requestWithdrawLpRaw;
    return { operation, amountRaw, policy: null, strategyId: null, signer: identity.user, requiredProgram: PARTNER_ROUTE.programs.voltrVault, allowedPrograms, instruction, auxiliaryInstructions, orderedInstructions, lookupTable: null, allowedTokenAccounts: tokenAccounts, allowedMints: [PARTNER_ROUTE.asset.mint, identity.lpMint], reserveLiquidity: "", reserveCollateral: "", strategyAssetAta: "", strategyAuth: "", obligation: "", allowedLamportAccounts: [identity.user, userAccounts.userLpAta, userAccounts.requestWithdrawLpAta, userAccounts.requestWithdrawVaultReceipt], instructionCount: orderedInstructions.length };
  };
  for (const name of REQUIRED_TXS) {
    const expected = await expectedFor(name);
    if (!expected) { add(gates, `${name} canonical reconstruction`, false, "missing graph, policy artifact, or user accounts", "canonical SDK operation", `complete the canonical ${name} graph/policy inputs before rerunning`); continue; }
    expectedTransactions.set(name, expected);
    const semanticAccounts = name.startsWith("manager")
      ? { idle: PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta, userAsset: "<none>", userLp: "<none>", escrow: "<none>", receipt: "<none>", reserveLiquidity: expected.reserveLiquidity, reserveCollateral: expected.reserveCollateral, strategyAssetAta: expected.strategyAssetAta }
      : { idle: mainAccounts!.idleAta, userAsset: userAccounts!.userAssetAta, userLp: userAccounts!.userLpAta, escrow: userAccounts!.requestWithdrawLpAta, receipt: userAccounts!.requestWithdrawVaultReceipt, reserveLiquidity: "", reserveCollateral: "", strategyAssetAta: "" };
    responses.set(name, await verifyTransaction(connection, resolve(evidencePath), name, manifest.transactions[name], expected, allPostRead, manifest.amounts, semanticAccounts, manifest.lifecycleId, manifest.routeAuthorizationSha256, gates));
  }
  const depositEvent = eventPayload(responses.get("userDeposit") ?? null, "DepositVaultEvent");
  const depositLpMinted = typeof depositEvent?.userAmountLpMinted === "bigint" ? depositEvent.userAmountLpMinted : null;
  const depositValueDelta = typeof depositEvent?.vaultAssetTotalValueBefore === "bigint" && typeof depositEvent.vaultAssetTotalValueAfter === "bigint" ? depositEvent.vaultAssetTotalValueAfter - depositEvent.vaultAssetTotalValueBefore : null;
  const depositSupplyDelta = typeof depositEvent?.vaultLpSupplyInclFeesBefore === "bigint" && typeof depositEvent.vaultLpSupplyInclFeesAfter === "bigint" ? depositEvent.vaultLpSupplyInclFeesAfter - depositEvent.vaultLpSupplyInclFeesBefore : null;
  const depositDeadWeightDelta = typeof depositEvent?.vaultLpDeadWeightBefore === "bigint" && typeof depositEvent.vaultLpDeadWeightAfter === "bigint" ? depositEvent.vaultLpDeadWeightAfter - depositEvent.vaultLpDeadWeightBefore : null;
  add(gates, "user deposit event and LP economics exact", depositEvent !== null && depositEvent.user === identity.user && depositEvent.vault === PARTNER_ROUTE.vault && depositEvent.vaultAssetMint === PARTNER_ROUTE.asset.mint && depositEvent.userAmountAssetDeposited === manifest.amounts.userDepositAssetRaw && depositLpMinted !== null && depositLpMinted > 0n && depositValueDelta === manifest.amounts.userDepositAssetRaw && depositSupplyDelta !== null && depositDeadWeightDelta !== null && depositSupplyDelta === depositLpMinted + depositDeadWeightDelta && depositEvent.vaultLpTotalAccumulatedFeesBefore === depositEvent.vaultLpTotalAccumulatedFeesAfter, depositEvent, { user: identity.user, vault: PARTNER_ROUTE.vault, assetMint: PARTNER_ROUTE.asset.mint, assetDeposited: manifest.amounts.userDepositAssetRaw, lpMinted: ">0 and equals token delta", totalValueDelta: manifest.amounts.userDepositAssetRaw, supplyDelta: "lpMinted + deadWeightDelta", accumulatedFees: "unchanged" }, "reconcile the exact DepositVaultEvent against transaction metadata");
  const request = responses.get("withdrawRequest") ?? null;
  const requestEvent = eventPayload(request, "RequestWithdrawVaultEvent");
  const requested = requestEvent?.requestedTs;
  const deadline = requestEvent?.withdrawableFromTs;
  const requestQuoteBits = typeof requestEvent?.amountAssetToWithdrawDecimalBits === "bigint" ? requestEvent.amountAssetToWithdrawDecimalBits : null;
  const requestQuoteBitsExpected = requestEvent && typeof requestEvent.vaultAssetTotalValueUnlocked === "bigint" && typeof requestEvent.vaultLpSupplyInclFees === "bigint" && requestEvent.vaultLpSupplyInclFees > 0n
    ? manifest.amounts.requestWithdrawLpRaw * requestEvent.vaultAssetTotalValueUnlocked * FRACTION_SCALE / requestEvent.vaultLpSupplyInclFees
    : null;
  const requestQuoteRaw = requestQuoteBits === null ? null : requestQuoteBits >> 48n;
  add(gates, "withdrawal request event and fixed-point quote exact", requestEvent !== null && requestEvent.vault === PARTNER_ROUTE.vault && requestEvent.user === identity.user && requestEvent.vaultAssetMint === PARTNER_ROUTE.asset.mint && requestEvent.requestedAmount === manifest.amounts.requestWithdrawLpRaw && requestEvent.amountLpEscrowed === manifest.amounts.requestWithdrawLpRaw && requestEvent.isAmountInLp === true && requestEvent.isWithdrawAll === true && requestEvent.requestWithdrawVaultReceipt === userAccounts?.requestWithdrawVaultReceipt && requestQuoteBits !== null && requestQuoteBits > 0n && requestQuoteBits === requestQuoteBitsExpected, { event: requestEvent, requestQuoteBitsExpected }, { vault: PARTNER_ROUTE.vault, user: identity.user, assetMint: PARTNER_ROUTE.asset.mint, requestedAmount: manifest.amounts.requestWithdrawLpRaw, amountLpEscrowed: manifest.amounts.requestWithdrawLpRaw, isAmountInLp: true, isWithdrawAll: true, receipt: userAccounts?.requestWithdrawVaultReceipt ?? "derived PDA", quoteBits: "floor(lp * unlocked * 2^48 / supply)" }, "recompute and confirm the exact zero-fee U80F48 request quote from the event prestate");
  add(gates, "withdrawal request deadline is exactly 600 seconds", typeof requested === "bigint" && typeof deadline === "bigint" && deadline - requested === 600n, { requestedTs: requested ?? null, withdrawableFromTs: deadline ?? null }, 600n, "produce a fresh confirmed request with RouteSpec withdrawalWaitingPeriod=600");
  const claim = responses.get("withdrawClaim") ?? null;
  const claimEvent = eventPayload(claim, "WithdrawVaultEvent");
  const claimValueDelta = claimEvent && typeof claimEvent.vaultAssetTotalValueBefore === "bigint" && typeof claimEvent.vaultAssetTotalValueAfter === "bigint" ? claimEvent.vaultAssetTotalValueBefore - claimEvent.vaultAssetTotalValueAfter : null;
  const claimSupplyDelta = claimEvent && typeof claimEvent.vaultLpSupplyInclFeesBefore === "bigint" && typeof claimEvent.vaultLpSupplyInclFeesAfter === "bigint" ? claimEvent.vaultLpSupplyInclFeesBefore - claimEvent.vaultLpSupplyInclFeesAfter : null;
  add(gates, "withdrawal claim event, request quote, and vault accounting exact", claimEvent !== null && claimEvent.vault === PARTNER_ROUTE.vault && claimEvent.user === identity.user && claimEvent.vaultAssetMint === PARTNER_ROUTE.asset.mint && claimEvent.userAmountLpBurned === manifest.amounts.requestWithdrawLpRaw && requestQuoteRaw !== null && requestQuoteRaw > 0n && claimEvent.userAmountAssetWithdrawn === requestQuoteRaw && claimValueDelta === requestQuoteRaw && claimSupplyDelta === manifest.amounts.requestWithdrawLpRaw && claimEvent.vaultLpTotalAccumulatedFeesBefore === claimEvent.vaultLpTotalAccumulatedFeesAfter && claimEvent.vaultLpDeadWeightBefore === claimEvent.vaultLpDeadWeightAfter && typeof claimEvent.withdrawnTs === "bigint", { event: claimEvent, claimValueDelta, claimSupplyDelta }, { vault: PARTNER_ROUTE.vault, user: identity.user, assetMint: PARTNER_ROUTE.asset.mint, lpBurned: manifest.amounts.requestWithdrawLpRaw, assetWithdrawn: requestQuoteRaw, totalValueDelta: requestQuoteRaw, supplyDelta: manifest.amounts.requestWithdrawLpRaw, accumulatedFees: "unchanged", deadWeight: "unchanged", withdrawnTs: ">= request deadline" }, "bind claim payout, LP burn, vault totals, and fees to the exact request event quote");
  add(gates, "claim is at or after request deadline", typeof deadline === "bigint" && typeof claimEvent?.withdrawnTs === "bigint" && claimEvent.withdrawnTs >= deadline, { withdrawnTs: claimEvent?.withdrawnTs ?? null, deadline: deadline ?? null }, "withdrawnTs >= requestedTs + 600", "wait 600 seconds, then claim the exact receipt once");
  try {
    const claimBlockTime = claim ? await connection.getBlockTime(claim.slot) : null;
    add(gates, "claim confirmed bank time is at or after request deadline", claimBlockTime !== null && typeof deadline === "bigint" && BigInt(claimBlockTime) >= deadline, { claimBlockTime, deadline: deadline ?? null }, "blockTime >= request deadline", "wait until the confirmed bank clock reaches the exact receipt deadline");
  } catch (error) { add(gates, "claim confirmed bank time is at or after request deadline", false, error instanceof Error ? error.message : String(error), "confirmed block time >= deadline", "reload the claim block time by exact signature"); }

  const artifactValues = new Map<ArtifactName, JsonRecord>();
  let verifiedScan: VerifiedWithdrawalScan | null = null;
  const scannerIdleOriginRaw = tokenPostAmount(responses.get("managerMainFallbackDeposit") ?? null, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta);
  for (const name of REQUIRED_ARTIFACTS) {
    const source = verifyRef(resolve(evidencePath), manifest.artifacts[name], `artifacts.${name}`, gates);
    if (source === null) continue;
    try {
      const value = record(JSON.parse(source), `artifacts.${name}`);
      artifactValues.set(name, value);
      const noBroadcastRequired = name === "instantWithdrawRejection" || name === "prematureClaim" || name === "withdrawalScanner" || name === "earnAdapter" || name === "negativeMutations";
      add(gates, `artifacts.${name} schema`, !noBroadcastRequired || value.broadcast === false, value.broadcast ?? null, noBroadcastRequired ? false : "recorded proof", `regenerate ${name} with the required no-broadcast boundary`);
      const requestEventIndex = request ? parseTransactionEvents({ logMessages: request.meta?.logMessages ?? [] }).findIndex((event) => event.name === "RequestWithdrawVaultEvent") : -1;
      const result = verifyArtifactSemantics(name, value, manifest, gates, requestEvent, manifest.transactions.withdrawRequest.signature, manifest.transactions.withdrawRequest.slot, requestEventIndex, scannerIdleOriginRaw, loadedPolicyCatalog?.artifact ?? null);
      if (name === "withdrawalScanner") verifiedScan = result;
    }
    catch (error) { add(gates, `artifacts.${name} JSON schema`, false, error instanceof Error ? error.message : String(error), "JSON object", `write the named ${name} artifact with exact schema`); }
  }
  const instantRejectionValue = artifactValues.get("instantWithdrawRejection") ?? null;
  if (instantRejectionValue && mainBuilder) {
    try {
      const transaction = record(instantRejectionValue.transaction, "instantWithdrawRejection.transaction");
      const amountLpRaw = integerString(transaction.amountLpRaw, "instantWithdrawRejection.transaction.amountLpRaw");
      const user = createNoopSigner(address(identity.user));
      const expectedInstruction = expectedInstructionFromCanonical((await mainBuilder.user.instantWithdraw({ user }, amountLpRaw, true)).canonical);
      const reportedInstruction = reportedCanonicalInstruction(transaction.instruction, "instantWithdrawRejection.transaction.instruction");
      const wireBase64 = stringField(transaction, "serializedTransactionBase64", "instantWithdrawRejection.transaction");
      const wire = Buffer.from(wireBase64, "base64");
      const decoded = VersionedTransaction.deserialize(wire);
      const expectedMessage = new TransactionMessage({
        payerKey: new PublicKey(identity.user),
        recentBlockhash: decoded.message.recentBlockhash,
        instructions: [web3Instruction(expectedInstruction)],
      }).compileToV0Message([]);
      const exact = canonicalInstructionEqual(reportedInstruction, expectedInstruction)
        && decoded.message.addressTableLookups.length === 0
        && Buffer.from(decoded.message.serialize()).equals(Buffer.from(expectedMessage.serialize()))
        && decoded.signatures.length === 1
        && bs58.encode(decoded.signatures[0]!) === transaction.expectedSignature;
      add(gates, "instant rejection packet independently reconstructs the one canonical SDK instruction", exact, { instruction: reportedInstruction, messageSha256: sha256(decoded.message.serialize()), signature: decoded.signatures.length === 1 ? bs58.encode(decoded.signatures[0]!) : null }, { instruction: expectedInstruction, messageSha256: sha256(expectedMessage.serialize()), signer: identity.user, instructionCount: 1, lookupTables: 0 }, "rerun the maintained one-instruction rejection simulator without an admin config update or extra instruction");
      const landed = await connection.getTransaction(stringField(transaction, "expectedSignature", "instantWithdrawRejection.transaction"), { commitment: "confirmed", maxSupportedTransactionVersion: 0 });
      add(gates, "instant rejection expected signature never landed", landed === null, landed ? { slot: landed.slot, err: landed.meta?.err ?? null } : null, null, "never broadcast the rejection packet; if its signature landed, start a fresh lifecycle");
    } catch (error) {
      add(gates, "instant rejection packet independently reconstructs the one canonical SDK instruction", false, error instanceof Error ? error.message : String(error), "canonical one-instruction signed v0 packet", "regenerate the no-broadcast instant rejection artifact");
      add(gates, "instant rejection expected signature never landed", false, "packet/signature unavailable", null, "regenerate the artifact and query its exact expected signature");
    }
  } else {
    add(gates, "instant rejection packet independently reconstructs the one canonical SDK instruction", false, "artifact or Main builder unavailable", "canonical one-instruction signed v0 packet", "produce the instant-withdraw rejection artifact from the maintained runtime command");
    add(gates, "instant rejection expected signature never landed", false, "artifact unavailable", null, "produce and query the no-broadcast rejection packet");
  }
  const restorationValue = artifactValues.get("restoration") ?? null;
  const restoration = restorationValue ? restorationEvidence(restorationValue, manifest, verifiedScan, gates) : null;
  if (restoration) {
    let confirmedIdleRaw = verifiedScan!.demand.confirmedIdleRaw;
    let remainingShortfallRaw = verifiedScan!.demand.idleShortfallRaw;
    const initial = restoration.recomputations[0]!;
    add(gates, "restoration initial recomputation equals scanner", initial.afterLegId === null && initial.contextSlot >= verifiedScan!.observationContextSlot && initial.confirmedIdleRaw === confirmedIdleRaw && initial.remainingShortfallRaw === remainingShortfallRaw, initial, { contextSlot: `>=${verifiedScan!.observationContextSlot}`, confirmedIdleRaw, remainingShortfallRaw }, "start restoration from the exact passing scanner state");
    for (const [index, leg] of restoration.confirmedLegs.entries()) {
      const named = manifest.transactions.managerMainRestorationWithdraw;
      const response = responses.get("managerMainRestorationWithdraw") ?? null;
      const exactNamedLeg = index === 0
        && restoration.confirmedLegs.length === 1
        && leg.strategyId === "main"
        && leg.reserve === partnerStrategyIdentity("main").reserve
        && leg.amountRaw === manifest.amounts.restorationAssetRaw
        && canonicalJson(leg.transaction) === canonicalJson(named)
        && leg.transaction.slot > manifest.transactions.withdrawRequest.slot
        && leg.transaction.slot < manifest.transactions.withdrawClaim.slot
        && leg.readbackContextSlot >= leg.transaction.slot;
      add(gates, `restoration leg ${index} is the exact named lifecycle withdrawal`, exactNamedLeg, { leg, namedTransaction: named }, { strategyId: "main", amountRaw: manifest.amounts.restorationAssetRaw, transaction: named, order: "request < restoration < claim", readbackContextSlot: `>=${named.slot}` }, "use the single top-level managerMainRestorationWithdraw command output as the durable restoration leg; never hide an extra transaction between protected checkpoints");
      const actualIdleDelta = response ? tokenDelta(response, PARTNER_FOUR_MARKET_ROUTE.commonVoltr.idleAta) : null;
      if (actualIdleDelta !== null) confirmedIdleRaw += actualIdleDelta;
      remainingShortfallRaw = verifiedScan!.demand.requiredIdleRaw > confirmedIdleRaw ? verifiedScan!.demand.requiredIdleRaw - confirmedIdleRaw : 0n;
      const recomputed = restoration.recomputations[index + 1]!;
      add(gates, `restoration leg ${index} actual idle effect and shortfall recomputed`, actualIdleDelta !== null && actualIdleDelta > 0n && actualIdleDelta <= leg.amountRaw && recomputed.afterLegId === leg.legId && recomputed.contextSlot >= leg.transaction.slot && recomputed.contextSlot === leg.readbackContextSlot && recomputed.confirmedIdleRaw === confirmedIdleRaw && recomputed.remainingShortfallRaw === remainingShortfallRaw && leg.idleRawAfter === confirmedIdleRaw && leg.remainingShortfallRaw === remainingShortfallRaw, { actualIdleDelta, confirmedIdleRaw, remainingShortfallRaw, leg, recomputed }, { actualIdleDelta: `1..${leg.amountRaw}`, confirmedIdleRaw, remainingShortfallRaw, readbackContextSlot: `>=${leg.transaction.slot}` }, "use confirmed transaction metadata and re-read idle before advancing the outbox leg");
    }
    add(gates, "restoration stops only at zero recomputed shortfall", remainingShortfallRaw === 0n, remainingShortfallRaw, 0n, "add the next deterministic bounded source leg or leave claim unproven");
  } else {
    add(gates, "restoration confirmed chain legs", false, null, "deterministic plan and exact confirmed outbox legs", "complete the scanner-to-outbox-to-confirmed-manager restoration proof");
  }
  const earnValue = artifactValues.get("earnAdapter") ?? null;
  if (earnValue) verifyEarnAdapterEvidence(earnValue, manifest, verifiedScan, responses, gates);
  else add(gates, "Earn adapter exact shared planner/source/outbox/movement proof", false, null, EARN_ADAPTER_SOURCE_PATHS, "wire the missing thin Earn adapter and generate its evidence");
  const finalValue = artifactValues.get("finalReconciliation") ?? null;
  if (finalValue) await verifyFinalConservation(rpcUrl, finalValue, manifest, manifest.transactions.withdrawClaim.slot, gates);
  else add(gates, "final current decoded conservation exact", false, null, "claim-anchored current conservation artifact", "produce the final current readback after claim");
  const prematureValue = artifactValues.get("prematureClaim") ?? null;
  if (prematureValue) {
    try {
      const prematureTransaction = record(prematureValue.transaction, "prematureClaim.transaction");
      const prematureSimulation = record(prematureValue.simulation, "prematureClaim.simulation");
      const prematureSimulationSlot = prematureSimulation.contextSlot;
      if (typeof prematureSimulationSlot !== "number" || !Number.isSafeInteger(prematureSimulationSlot) || prematureSimulationSlot <= 0) throw new Error("premature claim simulation context slot is invalid");
      const reportedBankBlockTime = prematureValue.bankBlockTime;
      const observedBankBlockTime = await connection.getBlockTime(prematureSimulationSlot);
      const prematureDeadline = integerString(prematureTransaction.withdrawableFromTs, "prematureClaim.transaction.withdrawableFromTs");
      const bankTimeDelta = typeof reportedBankBlockTime === "number" && observedBankBlockTime !== null
        ? Math.abs(observedBankBlockTime - reportedBankBlockTime)
        : null;
      add(gates, "premature claim bank time is bound to its confirmed simulation slot", typeof reportedBankBlockTime === "number" && Number.isSafeInteger(reportedBankBlockTime) && observedBankBlockTime !== null && bankTimeDelta !== null && bankTimeDelta <= 10 && BigInt(reportedBankBlockTime) < prematureDeadline && BigInt(observedBankBlockTime) < prematureDeadline, { simulationContextSlot: prematureSimulationSlot, reportedBankBlockTime, observedBankBlockTime, bankTimeDelta, deadline: prematureDeadline }, { bothObservedTimes: `<${prematureDeadline}`, providerEstimateDriftSeconds: "<=10" }, "rerun the premature proof and retain the confirmed simulation context plus a bounded provider block-time estimate");
      const expectedSignature = stringField(prematureTransaction, "expectedSignature", "prematureClaim.transaction");
      const reportedInstruction = reportedCanonicalInstruction(prematureTransaction.instruction, "prematureClaim.transaction.instruction");
      const expectedClaimInstruction = expectedTransactions.get("withdrawClaim")?.instruction ?? null;
      const packetBytes = Buffer.from(stringField(prematureTransaction, "serializedPacketBase64", "prematureClaim.transaction"), "base64");
      const decoded = VersionedTransaction.deserialize(packetBytes);
      const expectedMessage = expectedClaimInstruction === null ? null : new TransactionMessage({
        payerKey: new PublicKey(manifest.identities.user),
        recentBlockhash: decoded.message.recentBlockhash,
        instructions: [web3Instruction(expectedClaimInstruction)],
      }).compileToV0Message([]);
      const packetCanonicalExact = expectedMessage !== null
        && decoded.message.addressTableLookups.length === 0
        && Buffer.from(decoded.message.serialize()).equals(Buffer.from(expectedMessage.serialize()));
      add(gates, "premature claim reported instruction exact", expectedClaimInstruction !== null && canonicalInstructionEqual(reportedInstruction, expectedClaimInstruction), { programId: reportedInstruction.programId, dataSha256: sha256(reportedInstruction.data), accounts: reportedInstruction.accounts }, expectedClaimInstruction ? { programId: expectedClaimInstruction.programId, dataSha256: sha256(expectedClaimInstruction.data), accounts: expectedClaimInstruction.accounts } : "canonical claim instruction", "rerun premature simulation with the exact current SDK-built claim instruction");
      add(gates, "premature claim canonical SDK packet exact", packetCanonicalExact, { messageSha256: sha256(decoded.message.serialize()), lookups: decoded.message.addressTableLookups.length, reportedInstruction }, expectedClaimInstruction ? { messageSha256: sha256(expectedMessage!.serialize()), lookups: 0, instruction: expectedClaimInstruction } : "canonical claim packet", "rerun premature simulation with the exact current SDK-built claim packet");
      const landed = await connection.getTransaction(expectedSignature, { commitment: "confirmed", maxSupportedTransactionVersion: 0 });
      add(gates, "premature claim expected signature never landed", landed === null, landed ? { slot: landed.slot, err: landed.meta?.err ?? null } : null, null, "never broadcast the premature simulation packet; if it landed, start a fresh lifecycle");
    } catch (error) { add(gates, "premature claim expected signature never landed", false, error instanceof Error ? error.message : String(error), "confirmed RPC returns null", "reload the exact premature expected signature"); }
  }
  let claimOrigin: RequestOrigin | null = null;
  try {
    const claimPath = resolveChild(resolve(evidencePath), manifest.transactions.withdrawClaim.path);
    const claimOutput = record(JSON.parse(readFileSync(claimPath, "utf8")), "withdrawClaim command output");
    claimOrigin = requestOrigin(claimOutput.requestOrigin, "withdrawClaim.requestOrigin");
  } catch { /* the continuity gate below remains falsifiable */ }
  const protectedChainExact = lifecycleSignatures.length === REQUIRED_TXS.length
    && new Set(REQUIRED_TXS.map((name) => manifest.transactions[name].protectedAddressSetSha256)).size === 1
    && REQUIRED_TXS.every((name, index) => index === 0 || manifest.transactions[REQUIRED_TXS[index - 1]!]!.protectedPoststateSha256 === manifest.transactions[name]!.protectedPrestateSha256 && manifest.transactions[REQUIRED_TXS[index - 1]!]!.protectedAfterContextSlot <= manifest.transactions[name]!.protectedBeforeContextSlot);
  const originContinuity = verifiedScan !== null
    && sameRequestOrigin(verifiedScan.requestOrigin, manifest.requestOrigin)
    && (claimOrigin === null || sameRequestOrigin(claimOrigin, manifest.requestOrigin));
  add(gates, "signer-attested provider-observed protected-state chain and receipt generation continuity", protectedChainExact && originContinuity, { lifecycleId: manifest.lifecycleId, protectedChainExact, originContinuity, requestOrigin: manifest.requestOrigin, scannerOrigin: verifiedScan?.requestOrigin ?? null, claimOrigin }, { lifecycleId: "one exact SHA-256 across all command outputs", protectedChain: "each fixed signer attests exact 42-account bytes; poststate N === prestate N+1 with monotonic confirmed contexts", requestOrigin: "same request signature/event index/receipt raw hash/generation through scanner, restoration, and claim" }, "regenerate the first lifecycle artifact whose attested account bytes or request-origin tuple differs; the proof is explicitly provider-observed, not historical account replay");
  const sourceContract = executionSourceContract();
  add(gates, "pre-broadcast durability, bounded byte-identical transport, and recovery source contract", sourceContract.pass, sourceContract.observed, sourceContract.expected, "keep the maintained persist-before-send, one logical intent, bounded byte-identical expected-signature recovery, maxRetries=0 per RPC call, and minContextSlot contract intact; runtime history/provider trust remains a residual");
  const failedGateCount = gates.filter(({ pass }) => !pass).length;
  return { verdict: failedGateCount === 0 ? "BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_PASS" : "BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_FAIL", broadcast: false, routeSpecSha256: fourMarketRouteSpecSha256(), commitment, evidencePath, failedGateCount, gates, epistemicResiduals: ["exact protected account images are fixed-signer attestations of confirmed-provider observations; ordinary RPC cannot independently replay all historical account bytes", "confirmed RPC responses and getProgramAccounts completeness remain provider/retention assumptions", "filesystem artifact hashes bind bytes but do not independently prove historical write-before-send ordering", "source-contract inspection binds the maintained implementation shape but not an unavailable crash/restart execution trace"], policyCatalog: artifactJson ? { path: manifest.policyCatalog.path, fileSha256: manifest.policyCatalog.fileSha256, artifactSha256: manifest.policyCatalog.artifactSha256 } : null, vaultGateCount } as const;
}
