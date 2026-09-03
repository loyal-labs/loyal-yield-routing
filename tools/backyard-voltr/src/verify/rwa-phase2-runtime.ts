import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const SCHEMA = "loyal-backyard-rwa-phase2-runtime-verifier/v1";
const CONTRACT = "docs/plans/backyard-rwa-phase2-runtime-verifier.md";
const MANIFEST = "docs/manifests/backyard-rwa-v1.json";
const LIFECYCLE = "docs/evidence/backyard-rwa-go/phase2-runtime/lifecycle-v1.json";
const SIGNED_UNSENT = "docs/evidence/backyard-rwa-go/phase2-runtime/signed-unsent-v1.json";
const SELECTION = "docs/evidence/backyard-rwa-go/phase2-runtime/selection-v1.json";
const COMPILED = "docs/evidence/backyard-rwa-go/policy-compiled-v1.json";
const COMMAND = "bun run --cwd tools/backyard-voltr verify:rwa-phase2-runtime";

type Verdict = "PASS" | "FAIL" | "BLOCKED";
type Json = Record<string, unknown>;

type Check = {
  id: string;
  verdict: Verdict;
  condition: string;
  evidence: Json;
  resumeCondition: string | null;
};

function path(relative: string): string {
  return resolve(ROOT, relative);
}

function read(relative: string): string {
  return readFileSync(path(relative), "utf8");
}

function json(relative: string): Json | null {
  if (!existsSync(path(relative))) return null;
  try {
    const value = JSON.parse(read(relative)) as unknown;
    return value !== null && typeof value === "object" && !Array.isArray(value)
      ? value as Json
      : null;
  } catch {
    return null;
  }
}

function sha256(relative: string): string | null {
  return existsSync(path(relative))
    ? createHash("sha256").update(readFileSync(path(relative))).digest("hex")
    : null;
}

function equalJson(left: unknown, right: unknown): boolean {
  return JSON.stringify(left) === JSON.stringify(right);
}

function arrayOfObjects(value: unknown): Json[] {
  return Array.isArray(value)
    ? value.filter((entry): entry is Json => entry !== null && typeof entry === "object" && !Array.isArray(entry))
    : [];
}

function check(
  id: string,
  condition: string,
  pass: boolean,
  evidence: Json,
  resumeCondition: string,
): Check {
  return {
    id,
    verdict: pass ? "PASS" : "FAIL",
    condition,
    evidence,
    resumeCondition: pass ? null : resumeCondition,
  };
}

function main() {
  const contract = existsSync(path(CONTRACT)) ? read(CONTRACT) : "";
  const manifest = json(MANIFEST);
  const activation = manifest?.runtimeActivation;
  const activationRecord = activation !== null && typeof activation === "object" && !Array.isArray(activation)
    ? activation as Json
    : null;
  const selectedLane = typeof activationRecord?.selectedLane === "string"
    ? activationRecord.selectedLane
    : null;
  const runtimeRoutes = Array.isArray(activationRecord?.runtimeRoutes)
    ? activationRecord.runtimeRoutes.filter((value): value is string => typeof value === "string")
    : [];
  const supportedLanes = Array.isArray(manifest?.supportedLanes)
    ? manifest.supportedLanes.filter((value): value is string => typeof value === "string")
    : [];
  const binding = activationRecord?.selectedLaneBinding !== null && typeof activationRecord?.selectedLaneBinding === "object" && !Array.isArray(activationRecord?.selectedLaneBinding)
    ? activationRecord.selectedLaneBinding as Json
    : null;
  const selection = json(SELECTION);
  const selectedCandidate = arrayOfObjects(selection?.candidates).find((candidate) => candidate.lane === selectedLane) ?? null;
  const compiled = json(COMPILED);
  const compiledPolicies = arrayOfObjects(compiled?.policies);
  const selectionEvidenceHash = typeof activationRecord?.selectionEvidence === "object" && activationRecord.selectionEvidence !== null
    ? (activationRecord.selectionEvidence as Json).sha256
    : null;
  const selectedPolicyBindings = arrayOfObjects(selectedCandidate?.policyBindings);
  const selected = selectedLane !== null && selectedLane !== "Prime/PRIME/USDC" &&
    supportedLanes.includes(selectedLane) && runtimeRoutes.length === 2 &&
    new Set(runtimeRoutes).size === 2 && runtimeRoutes.includes("Prime/PRIME/USDC") &&
    runtimeRoutes.includes(selectedLane) && selection?.schema === "loyal-backyard-rwa-phase2-runtime-selection/v1" &&
    selection?.verdict === "PASS_SELECTED" && selection?.broadcast === false &&
    selection?.selectedLane === selectedLane && equalJson(selection?.runtimeRoutes, runtimeRoutes) &&
    selectionEvidenceHash === sha256(SELECTION) && selectedCandidate?.eligible === true &&
    binding?.lane === selectedLane && equalJson(binding?.graph, selectedCandidate?.graph) &&
    equalJson(binding?.obligation, selectedCandidate?.obligation) &&
    equalJson(binding?.reserveSafety, selectedCandidate?.reserveSafety);
  const kaminoPolicies = arrayOfObjects(binding?.kaminoPolicies);
  const kaminoPolicyChecks = ["deposit", "borrow", "repay", "withdraw"].map((operation) => {
    const manifestPolicy = kaminoPolicies.find((value) => value.operation === operation);
    const compiledPolicy = compiledPolicies.find((value) => {
      const operations = Array.isArray(value.operations) ? value.operations : [];
      return value.logicalName === `lane/${selectedLane ?? ""}` && operations.includes(operation);
    });
    const constraint = arrayOfObjects(compiledPolicy?.constraints)[0];
    const accountPubkeys = arrayOfObjects(constraint?.accountPubkeys).flatMap((entry) => Array.isArray(entry.pubkeys) ? entry.pubkeys : []);
    const live = selectedPolicyBindings.find((value) => value.seed === manifestPolicy?.seed && value.policy === manifestPolicy?.policy);
    return manifestPolicy !== undefined && compiledPolicy !== undefined && constraint !== undefined &&
      manifestPolicy.programId === constraint.programId && equalJson(manifestPolicy.accountPubkeys, accountPubkeys) &&
      equalJson(manifestPolicy.data, constraint.data) && manifestPolicy.packetBytes === compiledPolicy.createPacketBytes &&
      manifestPolicy.liveAccountDataSha256 === live?.accountDataSha256;
  });
  const jupiterEdges = arrayOfObjects(binding?.jupiterEdges);
  const jupiterEdgeChecks = ["entry", "exit"].map((side) => {
    const quote = selectedCandidate?.quotes !== null && typeof selectedCandidate?.quotes === "object"
      ? (selectedCandidate.quotes as Json)[side]
      : null;
    const quoteRecord = quote !== null && typeof quote === "object" && !Array.isArray(quote) ? quote as Json : null;
    const edge = jupiterEdges.find((value) => value.edge === quoteRecord?.key);
    const source = quoteRecord?.source !== null && typeof quoteRecord?.source === "object" ? quoteRecord.source as Json : null;
    const destination = quoteRecord?.destination !== null && typeof quoteRecord?.destination === "object" ? quoteRecord.destination as Json : null;
    const edgePolicies = selectedPolicyBindings.filter((value) => Array.isArray(value.swapEdges) && value.swapEdges.some((item) => {
      const swap = item !== null && typeof item === "object" && !Array.isArray(item) ? item as Json : null;
      return swap?.from === source?.symbol && swap?.to === destination?.symbol;
    }));
    const policy = edgePolicies[0];
    const quoteValues = quoteRecord?.quote !== null && typeof quoteRecord?.quote === "object" ? quoteRecord.quote as Json : null;
    const instruction = quoteRecord?.instruction !== null && typeof quoteRecord?.instruction === "object" ? quoteRecord.instruction as Json : null;
    const packet = quoteRecord?.packet !== null && typeof quoteRecord?.packet === "object" ? quoteRecord.packet as Json : null;
    return edge !== undefined && policy !== undefined && edge.policy === policy.policy &&
      edge.seed === policy.seed && edge.liveAccountDataSha256 === policy.accountDataSha256 &&
      edge.sourceMint === source?.mint && edge.destinationMint === destination?.mint &&
      edge.sourceCustody === source?.ata && edge.destinationCustody === destination?.ata &&
      edge.programId === instruction?.programId &&
      edge.inAmountRaw === quoteValues?.inAmountRaw && edge.outAmountRaw === quoteValues?.outAmountRaw &&
      edge.otherAmountThresholdRaw === quoteValues?.otherAmountThresholdRaw &&
      edge.instructionDataSha256 === instruction?.dataSha256 && edge.packetBytes === packet?.packetBytes &&
      edge.packetSha256 === packet?.packetSha256 && Array.isArray(edge.lookupTables) && equalJson(edge.lookupTables, quoteRecord?.lookupTables);
  });
  const completeBinding = kaminoPolicies.length === 4 && new Set(kaminoPolicies.map((value) => value.operation)).size === 4 &&
    kaminoPolicyChecks.every(Boolean) && jupiterEdges.length === 2 && new Set(jupiterEdges.map((value) => value.edge)).size === 2 &&
    jupiterEdgeChecks.every(Boolean);

  const goConfig = existsSync(path("go/backyard-rwa-worker/internal/backyardrwa/config.go"))
    ? read("go/backyard-rwa-worker/internal/backyardrwa/config.go")
    : "";
  const goState = existsSync(path("go/backyard-rwa-worker/internal/backyardrwa/state.go"))
    ? read("go/backyard-rwa-worker/internal/backyardrwa/state.go")
    : "";
  const goRoutes = existsSync(path("go/backyard-rwa-worker/internal/backyardrwa/route_runtime.go"))
    ? read("go/backyard-rwa-worker/internal/backyardrwa/route_runtime.go")
    : "";
  const goObserve = existsSync(path("go/backyard-rwa-worker/internal/backyardrwa/route_observe.go"))
    ? read("go/backyard-rwa-worker/internal/backyardrwa/route_observe.go")
    : "";
  const migration = existsSync(path("crates/loyal-yield-store/migrations/0072_backyard_rwa_phase2_route_neutral_actions.sql"))
    ? read("crates/loyal-yield-store/migrations/0072_backyard_rwa_phase2_route_neutral_actions.sql")
    : "";
  const exactTwoRouteRuntime = selected && selectedLane !== null &&
    goConfig.includes(selectedLane) && goRoutes.includes(selectedLane.split("/")[1] ?? selectedLane) &&
    goRoutes.includes("runtime lane %q is not installed") && goObserve.includes("cutoverDrain = true") &&
    goConfig.includes("Phase2TransactionCapRaw int64 = 1_000_000") &&
    migration.includes("multiply_operations_backyard_phase2_strategy_scope") &&
    !/optimizer|apy.?score|caller.?selected/i.test(goConfig + goState + goRoutes);

  const signedUnsent = json(SIGNED_UNSENT);
  const signedTransactions = arrayOfObjects(signedUnsent?.transactions);
  const signedUnsentComplete = signedUnsent?.schema === "loyal-backyard-rwa-phase2-runtime-signed-unsent/v1" &&
    signedUnsent?.verdict === "PASS" && signedUnsent?.broadcast === false && signedUnsent?.signedUnsent === true &&
    signedUnsent?.selectedLane === selectedLane && signedUnsent?.signatureAbsentOnChain === true &&
    signedUnsent?.chainPreStateSha256 === signedUnsent?.chainPostStateSha256 && signedTransactions.length >= 8 &&
    signedTransactions.every((row) => typeof row.signature === "string" && typeof row.transactionSha256 === "string" &&
      typeof row.packetBytes === "number" && row.packetBytes <= 1232 && row.simulationPassed === true);

  const lifecycle = json(LIFECYCLE);
  const deployment = lifecycle?.deployment !== null && typeof lifecycle?.deployment === "object" && !Array.isArray(lifecycle?.deployment) ? lifecycle.deployment as Json : null;
  const terminal = lifecycle?.terminal !== null && typeof lifecycle?.terminal === "object" && !Array.isArray(lifecycle?.terminal) ? lifecycle.terminal as Json : null;
  const operations = arrayOfObjects(lifecycle?.operations);
  const moneyActions = new Set(["VOLTR_ALLOCATE_TO_SQUADS", "SWAP_STABLE_TO_COLLATERAL_STEP", "OPEN_ROUTE_STEP", "DELEVER_ROUTE_STEP", "SWAP_COLLATERAL_TO_STABLE_STEP", "STAGE_SQUADS_TO_VOLTR", "VOLTR_RESTORE_IDLE"]);
  const requiredActions = ["VOLTR_ALLOCATE_TO_SQUADS", "SWAP_STABLE_TO_COLLATERAL_STEP", "OPEN_ROUTE_STEP", "DELEVER_ROUTE_STEP", "SWAP_COLLATERAL_TO_STABLE_STEP", "STAGE_SQUADS_TO_VOLTR", "VOLTR_RESTORE_IDLE", "REPORT_NAV"];
  const operationRowsExact = operations.length >= requiredActions.length && requiredActions.every((action) => operations.some((row) => row.action === action)) &&
    operations.every((row) => typeof row.operationId === "string" && row.strategyKey === selectedLane && row.status === "reconciled" &&
      typeof row.confirmedSlot === "number" && row.confirmedSlot > 0 && row.imageDigest === deployment?.imageDigest &&
      row.leaseOwner === deployment?.leaseOwner && row.fencingToken === deployment?.fencingToken &&
      (!moneyActions.has(String(row.action)) || (typeof row.amountRaw === "string" && /^\d+$/.test(row.amountRaw) && BigInt(row.amountRaw) <= 1_000_000n)) &&
      (row.action === "HOLD" || (typeof row.signature === "string" && row.signature.length > 40)));
  const confirmedSlots = operations.map((row) => row.confirmedSlot).filter((slot): slot is number => typeof slot === "number");
  const monotonic = confirmedSlots.every((slot, index) => index === 0 || slot >= confirmedSlots[index - 1]!);
  const deploymentExact = deployment?.serviceId === "srv-dabkt0ojo6nc7381o9fg" && typeof deployment?.deployId === "string" &&
    typeof deployment?.sourceCommit === "string" && /^[0-9a-f]{40}$/.test(deployment.sourceCommit) &&
    typeof deployment?.imageDigest === "string" && deployment.imageDigest.startsWith("sha256:") &&
    deployment?.oneWriter === true && deployment?.priorLeaseExpired === true && typeof deployment?.leaseOwner === "string" &&
    typeof deployment?.fencingToken === "number";
  const terminalExact = terminal?.commitment === "finalized" && terminal?.primeCollateralRaw === "0" && terminal?.primeDebtRaw === "0" &&
    terminal?.primeCustodyRaw === "0" && terminal?.selectedCollateralRaw === "0" && terminal?.selectedDebtRaw === "0" &&
    terminal?.selectedCustodyRaw === "0" && terminal?.squadsUSDCraw === "0" && terminal?.voltrStrategyUSDCraw === "0" &&
    typeof terminal?.voltrIdleUSDCraw === "string" && terminal?.unintendedResidue === false && terminal?.nonterminalOperationCount === 0;
  const liveComplete = lifecycle?.schema === "loyal-backyard-rwa-phase2-runtime-lifecycle/v1" && lifecycle?.broadcast === true &&
    lifecycle?.terminalReconciliation === "finalized" && lifecycle?.selectedLane === selectedLane && lifecycle?.goOriginated === true &&
    lifecycle?.withdrawalPreemptedNewRisk === true && lifecycle?.cumulativeAuthorizedUSDCraw !== undefined &&
    /^\d+$/.test(String(lifecycle.cumulativeAuthorizedUSDCraw)) && BigInt(String(lifecycle.cumulativeAuthorizedUSDCraw)) <= 10_000_000n &&
    deploymentExact && operationRowsExact && monotonic && terminalExact;

  const checks: Check[] = [
    check(
      "R00_contract",
      "The path-pinned Phase 2 runtime contract is present and retains its hard exclusions.",
      contract.includes("Status: approved implementation contract v1.") &&
        contract.includes("No consumer webapp") && contract.includes("No optimizer"),
      { path: CONTRACT, sha256: sha256(CONTRACT) },
      "Restore the approved path-pinned contract without weakening its conditions.",
    ),
    check(
      "R01_selected_route",
      "Fresh authoritative evidence selects and freezes exactly one installed non-PRIME representative.",
      selected && completeBinding,
      { manifest: MANIFEST, selection: SELECTION, compiled: COMPILED, selectedLane, runtimeRoutes, supportedLaneCount: supportedLanes.length, kaminoPolicyCount: kaminoPolicies.length, jupiterEdgeCount: jupiterEdges.length },
      "Run the read-only current-state candidate comparison, choose one safe installed representative, and freeze exactly PRIME plus that lane in runtimeActivation.",
    ),
    check(
      "R02_exact_go_runtime",
      "The Go worker supports exactly PRIME/USDC and the selected representative and fails closed for all others.",
      exactTwoRouteRuntime,
      { selectedLane, sourceInspected: ["config.go", "state.go", "route_runtime.go", "route_observe.go", "0072 migration"] },
      "Implement the typed two-route Go runtime without adding selection or optimizer behavior.",
    ),
    check(
      "R03_verification_ladder",
      "The selected lane has a fresh exact signed-unsent entry, HOLD, unwind, return, and NAV ladder with no broadcast.",
      signedUnsentComplete,
      { path: SIGNED_UNSENT, sha256: sha256(SIGNED_UNSENT), transactionCount: signedTransactions.length, selectedLane },
      "Generate the current-chain signed-unsent selected-lane lifecycle evidence and prove every signature absent on chain.",
    ),
    check(
      "R04_immutable_deploy",
      "The committed immutable image is deployed with one fenced writer after lease handoff.",
      deploymentExact,
      { path: LIFECYCLE, deployment },
      "Deploy the committed immutable Go image and retain service, deploy, digest, and lease-handoff readback.",
    ),
    check(
      "R05_live_lifecycle",
      "One bounded Go-originated selected-lane lifecycle is durably reconciled through finalized terminal custody.",
      liveComplete,
      { path: LIFECYCLE, sha256: sha256(LIFECYCLE), selectedLane, operationCount: operations.length, terminal },
      "Complete the bounded deployed-worker lifecycle and independently retain its finalized operation and custody reconciliation.",
    ),
    check(
      "R06_phase1_withdrawal_safety",
      "Phase 1 is flat before cutover and withdrawal demand preempts new Phase 2 risk.",
      terminalExact && lifecycle?.phaseOneRegressionPassed === true && lifecycle?.withdrawalPreemptedNewRisk === true,
      { phaseOneRegressionPassed: lifecycle?.phaseOneRegressionPassed ?? null, withdrawalPreemptedNewRisk: lifecycle?.withdrawalPreemptedNewRisk ?? null, terminal },
      "Prove finalized flat PRIME cutover state plus the selected-lane withdrawal-priority lifecycle.",
    ),
    check(
      "R07_handoff_and_coverage",
      "The handoff and maintained checks identify the exact two-route runtime and its exclusions.",
      existsSync(path("docs/backyard-rwa-partner-handoff.md")) && read("docs/backyard-rwa-partner-handoff.md").includes(selectedLane ?? "__missing__") &&
        lifecycle?.promotedChecks !== undefined && Array.isArray(lifecycle.promotedChecks) && lifecycle.promotedChecks.length > 0,
      { handoff: "docs/backyard-rwa-partner-handoff.md", promotedChecks: lifecycle?.promotedChecks ?? null },
      "Update the partner handoff and record promoted stable checks after the live lifecycle closes.",
    ),
  ];
  const firstFailure = checks.find((entry) => entry.verdict === "FAIL") ?? null;
  const verdict: Verdict = firstFailure === null ? "PASS" : "FAIL";
  console.log(JSON.stringify({
    schema: SCHEMA,
    verdict,
    broadcast: false,
    commitment: "confirmed",
    contractPath: CONTRACT,
    manifestPath: MANIFEST,
    selectedLane,
    checks,
    firstFailure,
    blocker: null,
    resumeCommand: COMMAND,
  }, null, 2));
  process.exitCode = verdict === "PASS" ? 0 : 1;
}

try {
  main();
} catch (error) {
  const message = error instanceof Error ? error.message : String(error);
  console.log(JSON.stringify({
    schema: SCHEMA,
    verdict: "FAIL",
    broadcast: false,
    commitment: "confirmed",
    firstFailure: {
      id: "VERIFIER_INTERNAL_ERROR",
      verdict: "FAIL",
      condition: "The verifier evaluates the contract without hidden prerequisites.",
      evidence: { message },
      resumeCondition: "Fix the verifier defect and rerun the sole command.",
    },
    blocker: null,
    resumeCommand: COMMAND,
  }, null, 2));
  process.exitCode = 1;
}
