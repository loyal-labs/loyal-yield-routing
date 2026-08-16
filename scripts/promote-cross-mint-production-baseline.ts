#!/usr/bin/env bun

import { neon } from "@neondatabase/serverless";

const SQUADS_PROGRAM_ID = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
const CLUSTER = "mainnet-beta";
const BATCH_SIZE = 100;

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} must be set`);
  }
  return value;
}

if (requiredEnv("CONFIRM_PRODUCTION_ROUTE_POLICY_PROMOTION") !== "1") {
  throw new Error("CONFIRM_PRODUCTION_ROUTE_POLICY_PROMOTION=1 is required");
}

const expectedCount = Number.parseInt(requiredEnv("EXPECTED_ACTIVE_ROUTE_POLICY_COUNT"), 10);
if (!Number.isSafeInteger(expectedCount) || expectedCount <= 0) {
  throw new Error("EXPECTED_ACTIVE_ROUTE_POLICY_COUNT must be a positive integer");
}

const sql = neon(requiredEnv("NEON_DATABASE_URL"));
const rpcUrl = requiredEnv("SOLANA_RPC_URL");
const policies = await sql`
  SELECT policy_account
  FROM loyal_yield.route_policies
  WHERE active
  ORDER BY policy_account
`;

if (policies.length !== expectedCount) {
  throw new Error(
    `active route-policy count changed: expected ${expectedCount}, got ${policies.length}`,
  );
}

const policyAccounts = policies.map((row) => String(row.policy_account));
let minimumFinalizedSlot = Number.MAX_SAFE_INTEGER;

for (let offset = 0; offset < policyAccounts.length; offset += BATCH_SIZE) {
  const batch = policyAccounts.slice(offset, offset + BATCH_SIZE);
  const response = await fetch(rpcUrl, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: offset / BATCH_SIZE + 1,
      method: "getMultipleAccounts",
      params: [batch, { commitment: "finalized", encoding: "base64" }],
    }),
  });
  if (!response.ok) {
    throw new Error(`finalized RPC account batch returned ${response.status}`);
  }
  const payload = (await response.json()) as {
    error?: { message?: string };
    result?: {
      context?: { slot?: number };
      value?: Array<{ owner?: string; data?: [string, string] } | null>;
    };
  };
  if (payload.error) {
    throw new Error(`finalized RPC rejected account batch: ${payload.error.message ?? "unknown"}`);
  }
  const slot = payload.result?.context?.slot;
  const values = payload.result?.value;
  if (!Number.isSafeInteger(slot) || !Array.isArray(values) || values.length !== batch.length) {
    throw new Error("finalized RPC returned an incomplete account batch");
  }
  minimumFinalizedSlot = Math.min(minimumFinalizedSlot, slot!);
  values.forEach((account, index) => {
    if (!account) {
      throw new Error(`active route policy is absent at finalized commitment: ${batch[index]}`);
    }
    if (account.owner !== SQUADS_PROGRAM_ID) {
      throw new Error(`active route policy has the wrong finalized owner: ${batch[index]}`);
    }
    if (!account.data?.[0]) {
      throw new Error(`active route policy has empty finalized data: ${batch[index]}`);
    }
  });
}

const [promotion, gate] = await sql.transaction([
  sql`
    UPDATE loyal_yield.route_policies
    SET cluster = ${CLUSTER},
        source_commitment = 'finalized',
        finalized_eligible = TRUE,
        last_seen_at = now()
    WHERE active
      AND cluster = 'unknown'
      AND source_commitment = 'unknown'
    RETURNING id
  `,
  sql`
    INSERT INTO loyal_yield.cross_mint_movement_controls
      (cluster, start_new_movements, continue_or_recover_existing, updated_by)
    VALUES (${CLUSTER}, FALSE, TRUE, 'cross-mint-orchestration-release')
    ON CONFLICT (cluster) DO NOTHING
    RETURNING cluster
  `,
]);

const [continuity] = await sql`
  SELECT
    count(*) FILTER (WHERE active)::BIGINT AS active_count,
    count(*) FILTER (
      WHERE active
        AND cluster = ${CLUSTER}
        AND source_commitment = 'finalized'
        AND finalized_eligible
    )::BIGINT AS eligible_count
  FROM loyal_yield.route_policies
`;
const [gateReadback] = await sql`
  SELECT start_new_movements, continue_or_recover_existing
  FROM loyal_yield.cross_mint_movement_controls
  WHERE cluster = ${CLUSTER}
`;

if (String(continuity?.active_count) !== String(expectedCount)) {
  throw new Error("active route-policy count changed during promotion");
}
if (String(continuity?.eligible_count) !== String(expectedCount)) {
  throw new Error("not every active route policy is eligible after promotion");
}
if (gateReadback?.start_new_movements !== false || gateReadback?.continue_or_recover_existing !== true) {
  throw new Error("cross-mint movement gate is not in the safe release posture");
}

console.log(
  [
    "production_route_policy_baseline=PASS",
    `active=${expectedCount}`,
    `promoted=${promotion.length}`,
    `gateInserted=${gate.length}`,
    `minimumFinalizedSlot=${minimumFinalizedSlot}`,
    "startNewMovements=false",
    "continueOrRecoverExisting=true",
  ].join(" "),
);
