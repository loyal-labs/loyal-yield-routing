import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Reserve } from "@kamino-finance/klend-sdk";
import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import { resolveCurrentRwaMultiplyCatalog } from "../policies/rwa-multiply-catalog-resolver.js";
import { resolveFreshJupiterEdge } from "../policies/rwa-multiply-jupiter-headers.js";

type Json = Record<string, unknown>;
const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const COMPILED_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/policy-compiled-v1.json");
const OUTPUT_PATH = resolve(ROOT, "docs/evidence/backyard-rwa-go/phase2-runtime/selection-v1.json");
const CANDIDATES = [
  { lane: "OnRe/ONyc/USDC", collateral: "ONyc" },
  { lane: "Maple/syrupUSDC/USDC", collateral: "syrupUSDC" },
] as const;
const PACKET_LIMIT = 1_232;
const ONE_USDC_RAW = 1_000_000n;

function invariant(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
function object(value: unknown, label: string): Json { invariant(value !== null && typeof value === "object" && !Array.isArray(value), `${label} is not an object`); return value as Json; }
function sha256(value: Uint8Array): string { return createHash("sha256").update(value).digest("hex"); }
function bigint(value: { toString(): string }): bigint { return BigInt(value.toString()); }
function ceilScaledFraction(value: { toString(): string }): bigint {
  const raw = bigint(value); const scale = 1n << 60n;
  return (raw + scale - 1n) / scale;
}

async function main() {
  invariant(!existsSync(OUTPUT_PATH), `${OUTPUT_PATH} already exists; selection evidence is immutable`);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  invariant(rpcUrl, "SOLANA_RPC_URL is required for read-only Phase 2 selection");
  const connection = new Connection(rpcUrl, "confirmed");
  invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");
  const resolution = await resolveCurrentRwaMultiplyCatalog(connection);
  const compiled = object(JSON.parse(readFileSync(COMPILED_PATH, "utf8")), "compiled catalog");
  invariant(compiled.phase === "phase2" && Array.isArray(compiled.policies), "compiled Phase 2 catalog is absent");
  const compiledPolicies = compiled.policies.map((value) => object(value, "compiled policy"));
  const rows = [];
  for (const candidate of CANDIDATES) {
    const lane = resolution.lanes.find((value) => value.key === candidate.lane);
    invariant(lane?.exact, `${candidate.lane} current graph is not exact`);
    const graph = lane.resolved;
    const addresses = [graph.collateralReserve.address, graph.debtReserve.address, graph.obligation];
    const accountRead = await connection.getMultipleAccountsInfoAndContext(addresses.map((value) => new PublicKey(value)), { commitment: "confirmed", minContextSlot: resolution.contextSlot });
    invariant(accountRead.value[0] && accountRead.value[1] && accountRead.value[2], `${candidate.lane} reserve or obligation disappeared`);
    const collateral = Reserve.decode(accountRead.value[0].data);
    const debt = Reserve.decode(accountRead.value[1].data);
    const collateralBorrowedRaw = ceilScaledFraction(collateral.liquidity.borrowedAmountSf);
    const debtBorrowedRaw = ceilScaledFraction(debt.liquidity.borrowedAmountSf);
    const collateralSupplyRaw = bigint(collateral.liquidity.availableAmount) + collateralBorrowedRaw;
    const collateralDepositLimitRaw = bigint(collateral.config.depositLimit);
    const debtBorrowLimitRaw = bigint(debt.config.borrowLimit);
    const debtAvailableRaw = bigint(debt.liquidity.availableAmount);
    const depositHeadroomRaw = collateralDepositLimitRaw > collateralSupplyRaw ? collateralDepositLimitRaw - collateralSupplyRaw : 0n;
    const borrowHeadroomRaw = debtBorrowLimitRaw > debtBorrowedRaw ? debtBorrowLimitRaw - debtBorrowedRaw : 0n;
    const entry = await resolveFreshJupiterEdge(connection, `USDC->${candidate.collateral}`);
    const exit = await resolveFreshJupiterEdge(connection, `${candidate.collateral}->USDC`);
    const quote = object(entry.quote, `${candidate.lane} entry quote`);
    const quotedCollateralRaw = BigInt(String(quote.otherAmountThresholdRaw));
    const lanePolicies = compiledPolicies.filter((policy) => policy.logicalName === `lane/${candidate.lane}`);
    const swapPolicies = compiledPolicies.filter((policy) => Array.isArray(policy.swapEdges) && policy.swapEdges.some((value) => {
      const edge = object(value, "compiled swap edge");
      return (edge.from === "USDC" && edge.to === candidate.collateral) || (edge.from === candidate.collateral && edge.to === "USDC");
    }));
    invariant(lanePolicies.length === 4 && swapPolicies.length === 2, `${candidate.lane} does not have exactly four Kamino and two Jupiter policies`);
    const policies = [...lanePolicies, ...swapPolicies];
    const policyRead = await connection.getMultipleAccountsInfoAndContext(policies.map((policy) => new PublicKey(String(policy.policy))), { commitment: "confirmed", minContextSlot: accountRead.context.slot });
    const policyBindings = policies.map((policy, index) => {
      const info = policyRead.value[index];
      invariant(info?.owner.toBase58() === RWA_MULTIPLY_ROUTE.squads.program, `${candidate.lane} policy ${String(policy.policy)} is absent or has the wrong owner`);
      return { logicalName: policy.logicalName, seed: policy.seed, policy: policy.policy, operations: policy.operations,
        swapEdges: policy.swapEdges, accountDataSha256: sha256(info.data) };
    });
    const entryPacketBytes = Number(object(entry.packet, `${candidate.lane} entry packet`).packetBytes);
    const exitPacketBytes = Number(object(exit.packet, `${candidate.lane} exit packet`).packetBytes);
    const reasons = [] as string[];
    if (entry.pass !== true || exit.pass !== true) reasons.push("fresh entry or exit quote did not resolve");
    if (entryPacketBytes > PACKET_LIMIT || exitPacketBytes > PACKET_LIMIT) reasons.push("fresh legacy packet exceeds 1232 bytes");
    if (Number(collateral.config.status) !== 0 || Number(debt.config.status) !== 0) reasons.push("reserve is not active");
    if (depositHeadroomRaw < quotedCollateralRaw) reasons.push("collateral deposit headroom is below the one-USDC quote threshold");
    if (debtAvailableRaw < ONE_USDC_RAW / 2n || borrowHeadroomRaw < ONE_USDC_RAW / 2n) reasons.push("USDC debt headroom is below the bounded lifecycle requirement");
    rows.push({
      lane: candidate.lane, eligible: reasons.length === 0, rejectionReasons: reasons,
      context: { resolutionSlot: resolution.contextSlot, reserveSlot: accountRead.context.slot, policySlot: policyRead.context.slot },
      graph, obligation: { address: graph.obligation, dataSha256: sha256(accountRead.value[2].data) },
      reserveSafety: {
        collateral: { status: Number(collateral.config.status), loanToValuePct: Number(collateral.config.loanToValuePct), liquidationThresholdPct: Number(collateral.config.liquidationThresholdPct), depositLimitRaw: collateralDepositLimitRaw.toString(), totalSupplyUpperBoundRaw: collateralSupplyRaw.toString(), depositHeadroomRaw: depositHeadroomRaw.toString() },
        debt: { status: Number(debt.config.status), borrowLimitRaw: debtBorrowLimitRaw.toString(), borrowedRawCeil: debtBorrowedRaw.toString(), borrowHeadroomRaw: borrowHeadroomRaw.toString(), availableRaw: debtAvailableRaw.toString(), utilizationLimitBlockBorrowingAbovePct: Number(debt.config.utilizationLimitBlockBorrowingAbovePct) },
      },
      quotes: { entry, exit }, policyBindings,
    });
  }
  const selected = rows.find((row) => row.eligible)?.lane ?? null;
  const evidence = {
    schema: "loyal-backyard-rwa-phase2-runtime-selection/v1", generatedAt: new Date().toISOString(),
    verdict: selected ? "PASS_SELECTED" : "BLOCKED_NO_SAFE_CANDIDATE", broadcast: false,
    cluster: "mainnet-beta", commitment: "confirmed", packetLimitBytes: PACKET_LIMIT,
    transactionCapRaw: ONE_USDC_RAW.toString(), policySeed: resolution.policySeedBefore,
    selectionRule: "first eligible same-USDC candidate in fixed OnRe then Maple order; eligibility requires exact current graph and policies, active reserves, bounded capacity, and both fresh legacy packets <=1232 bytes",
    selectedLane: selected, runtimeRoutes: selected ? ["Prime/PRIME/USDC", selected] : ["Prime/PRIME/USDC"], candidates: rows,
    resumeCondition: selected ? null : "Wait for a same-USDC installed candidate with exact current graph, bounded capacity, and buildable entry and exit packets.",
  };
  mkdirSync(dirname(OUTPUT_PATH), { recursive: true });
  writeFileSync(OUTPUT_PATH, `${JSON.stringify(evidence, null, 2)}\n`, { flag: "wx" });
  console.log(JSON.stringify({ verdict: evidence.verdict, selectedLane: selected, output: OUTPUT_PATH }));
}

await main();
