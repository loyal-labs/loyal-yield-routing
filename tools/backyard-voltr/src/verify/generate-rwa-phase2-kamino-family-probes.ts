import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Connection, PublicKey } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";
import {
  buildPhaseTwoKaminoLaneOperations,
  PHASE_TWO_REPRESENTATIVE_LANES,
  resolutionLanes,
  type ResolvedLane,
} from "../policies/rwa-multiply-phase2-kamino.js";

const repositoryRoot = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const resolutionPath = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-resolution-v1.json");
const output = resolve(repositoryRoot, "docs/evidence/backyard-rwa-go/policy-kamino-family-probes-v1.json");

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function accountsFor(lane: ResolvedLane) {
  return [
    lane.resolved.lendingMarket,
    lane.resolved.collateralReserve.address,
    lane.resolved.debtReserve.address,
    lane.resolved.obligation,
    lane.resolved.collateralCustody.address,
    lane.resolved.debtCustody.address,
  ];
}

const resolution = JSON.parse(readFileSync(resolutionPath, "utf8")) as Record<string, unknown>;
const contextSlot = resolution.contextSlot;
invariant(typeof contextSlot === "number" && Number.isSafeInteger(contextSlot) && contextSlot > 0,
  "resolution artifact has no confirmed context slot");
const lanes = resolutionLanes(resolution);
invariant(lanes.length === 11 && lanes.every((lane) => lane.exact),
  "resolution artifact does not contain eleven exact lanes");
// Build all 44 operation layouts before selecting representative current-state
// probes.  This is intentionally not a direct Kamino transaction: the vault
// PDA signer exists only when Squads invokes it, so a direct signed wire would
// be a false proof rather than useful policy evidence.
const allLaneOperations = lanes.map((lane) => ({
  lane: lane.key,
  operations: buildPhaseTwoKaminoLaneOperations(lane),
}));
invariant(allLaneOperations.every(({ operations }) => operations.length === 4),
  "one or more resolved lanes does not build all four KLend operations");
const representative = PHASE_TWO_REPRESENTATIVE_LANES.map((key) => {
  const lane = lanes.find((candidate) => candidate.key === key);
  invariant(lane !== undefined, `representative lane ${key} is absent`);
  return lane;
});
invariant(new Set(representative.map((lane) => lane.key.split("/")[0])).size === 5,
  "representatives do not cover the five requested market families");

const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
const connection = new Connection(rpcUrl, "confirmed");
invariant(await connection.getGenesisHash() === RWA_MULTIPLY_ROUTE.genesisHash, "RPC is not mainnet-beta");

const evidence = [];
for (const lane of representative) {
  const addresses = accountsFor(lane);
  const read = await connection.getMultipleAccountsInfoAndContext(
    addresses.map((value) => new PublicKey(value)),
    { commitment: "confirmed", minContextSlot: contextSlot },
  );
  const [market, collateralReserve, debtReserve, maybeObligation, collateralCustody, debtCustody] = read.value;
  const obligation = maybeObligation ?? null;
  invariant(market?.owner.toBase58() === lane.resolved.klendProgram,
    `${lane.key} market owner drifted`);
  invariant(collateralReserve?.owner.toBase58() === lane.resolved.klendProgram,
    `${lane.key} collateral reserve owner drifted`);
  invariant(debtReserve?.owner.toBase58() === lane.resolved.klendProgram,
    `${lane.key} debt reserve owner drifted`);
  invariant(collateralCustody?.owner.toBase58() === lane.resolved.collateralReserve.liquidityTokenProgram,
    `${lane.key} collateral custody owner drifted`);
  invariant(debtCustody?.owner.toBase58() === lane.resolved.debtReserve.liquidityTokenProgram,
    `${lane.key} debt custody owner drifted`);
  if (obligation !== null) invariant(obligation.owner.toBase58() === lane.resolved.klendProgram,
    `${lane.key} derived obligation is occupied by a non-KLend account`);
  const operations = buildPhaseTwoKaminoLaneOperations(lane);
  evidence.push({
    lane: lane.key,
    contextSlot: read.context.slot,
    exactCurrentAccounts: {
      marketSha256: sha256(market.data),
      collateralReserveSha256: sha256(collateralReserve.data),
      debtReserveSha256: sha256(debtReserve.data),
      collateralCustodySha256: sha256(collateralCustody.data),
      debtCustodySha256: sha256(debtCustody.data),
      obligation: obligation === null ? { status: "absent-derived-pda" } : {
        status: "klend-owned", dataSha256: sha256(obligation.data),
      },
    },
    operations,
  });
}

const outputValue = {
  schema: "loyal-backyard-rwa-kamino-family-probes/v1",
  verdict: "PASS_KAMINO_FAMILIES_PROBED",
  broadcast: false,
  commitment: "confirmed",
  resolutionContextSlot: contextSlot,
  policySeedBefore: resolution.policySeedBefore,
  representativeLanes: PHASE_TWO_REPRESENTATIVE_LANES,
  allLaneOperations,
  representativeEvidence: evidence,
};
writeFileSync(output, `${JSON.stringify(outputValue, null, 2)}\n`, { flag: "w" });
console.log(JSON.stringify({ output, verdict: outputValue.verdict, representativeCount: evidence.length }));
