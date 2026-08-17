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

const expectedCount = Number.parseInt(
  requiredEnv("EXPECTED_ACTIVE_ROUTE_POLICY_COUNT"),
  10,
);
if (!Number.isSafeInteger(expectedCount) || expectedCount <= 0) {
  throw new Error(
    "EXPECTED_ACTIVE_ROUTE_POLICY_COUNT must be a positive integer",
  );
}

const sql = neon(requiredEnv("NEON_DATABASE_URL"));
const rpcUrl = requiredEnv("SOLANA_RPC_URL");
const policies = await sql`
  SELECT
    policy.policy_account,
    (
      SELECT count(*)::BIGINT
      FROM loyal_yield.managed_vaults vault
      WHERE vault.active
        AND vault.active_policy_id = policy.id
    ) AS active_route_vault_count
  FROM loyal_yield.route_policies policy
  WHERE policy.active
  ORDER BY policy.policy_account
`;

if (policies.length !== expectedCount) {
  throw new Error(
    `active route-policy count changed: expected ${expectedCount}, got ${policies.length}`,
  );
}

const policyAccounts = policies.map((row) => String(row.policy_account));
const absentPolicyAccounts: string[] = [];
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
    throw new Error(
      `finalized RPC rejected account batch: ${payload.error.message ?? "unknown"}`,
    );
  }
  const slot = payload.result?.context?.slot;
  const values = payload.result?.value;
  if (
    !Number.isSafeInteger(slot) ||
    !Array.isArray(values) ||
    values.length !== batch.length
  ) {
    throw new Error("finalized RPC returned an incomplete account batch");
  }
  minimumFinalizedSlot = Math.min(minimumFinalizedSlot, slot!);
  values.forEach((account, index) => {
    if (!account) {
      absentPolicyAccounts.push(batch[index]!);
      return;
    }
    if (account.owner !== SQUADS_PROGRAM_ID) {
      throw new Error(
        `active route policy has the wrong finalized owner: ${batch[index]}`,
      );
    }
    if (!account.data?.[0]) {
      throw new Error(
        `active route policy has empty finalized data: ${batch[index]}`,
      );
    }
  });
}

const expectedEligibleCount = expectedCount - absentPolicyAccounts.length;
if (expectedEligibleCount <= 0) {
  throw new Error("finalized route-policy audit left no active policies");
}

const absentPolicySet = new Set(absentPolicyAccounts);
const eligiblePolicyAccounts = policyAccounts.filter(
  (account) => !absentPolicySet.has(account),
);
const expectedDeactivatedVaultCount = policies.reduce(
  (total, policy) =>
    absentPolicySet.has(String(policy.policy_account))
      ? total + Number(policy.active_route_vault_count)
      : total,
  0,
);

const [
  ,
  initialFence,
  deactivatedVaults,
  clearedSetupReferences,
  deactivated,
  promotion,
  gate,
  finalFence,
] = await sql.transaction([
  sql`
      LOCK TABLE loyal_yield.route_policies, loyal_yield.managed_vaults
      IN SHARE ROW EXCLUSIVE MODE
    `,
  sql`
      SELECT 1 / CASE WHEN (
        SELECT count(*)
        FROM loyal_yield.route_policies
        WHERE active
      ) = ${expectedCount} THEN 1 ELSE 0 END AS count_fence
    `,
  sql`
      UPDATE loyal_yield.managed_vaults vault
      SET active = FALSE,
          setup_policy_id = NULL,
          last_seen_at = now(),
          last_reconciled_at = now(),
          last_reconciled_slot = ${minimumFinalizedSlot}
      FROM loyal_yield.route_policies policy
      WHERE vault.active
        AND vault.active_policy_id = policy.id
        AND policy.policy_account = ANY(${absentPolicyAccounts}::TEXT[])
      RETURNING vault.id
    `,
  sql`
      UPDATE loyal_yield.managed_vaults vault
      SET setup_policy_id = NULL,
          last_seen_at = now()
      FROM loyal_yield.route_policies policy
      WHERE vault.active
        AND vault.setup_policy_id = policy.id
        AND policy.policy_account = ANY(${absentPolicyAccounts}::TEXT[])
      RETURNING vault.id
    `,
  sql`
      UPDATE loyal_yield.route_policies policy
      SET active = FALSE,
          last_seen_at = now()
      WHERE policy.active
        AND policy.policy_account = ANY(${absentPolicyAccounts}::TEXT[])
        AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.managed_vaults vault
          WHERE vault.active
            AND vault.active_policy_id = policy.id
        )
      RETURNING policy.id
    `,
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
  sql`
      SELECT 1 / CASE WHEN
        (SELECT count(*) FROM loyal_yield.route_policies WHERE active) = ${expectedEligibleCount}
        AND (
          SELECT count(*)
          FROM loyal_yield.route_policies
          WHERE active
            AND policy_account = ANY(${eligiblePolicyAccounts}::TEXT[])
            AND cluster = ${CLUSTER}
            AND source_commitment = 'finalized'
            AND finalized_eligible
        ) = ${expectedEligibleCount}
        AND NOT EXISTS (
          SELECT 1
          FROM loyal_yield.managed_vaults vault
          JOIN loyal_yield.route_policies policy
            ON policy.id IN (vault.active_policy_id, vault.setup_policy_id)
          WHERE vault.active
            AND policy.policy_account = ANY(${absentPolicyAccounts}::TEXT[])
        )
        AND EXISTS (
          SELECT 1
          FROM loyal_yield.cross_mint_movement_controls
          WHERE cluster = ${CLUSTER}
            AND NOT start_new_movements
            AND continue_or_recover_existing
        )
      THEN 1 ELSE 0 END AS invariant_fence
    `,
]);

if (Number(initialFence[0]?.count_fence) !== 1) {
  throw new Error("active route-policy count fence did not execute");
}
if (Number(finalFence[0]?.invariant_fence) !== 1) {
  throw new Error("route-policy promotion invariant fence did not execute");
}

if (deactivatedVaults.length !== expectedDeactivatedVaultCount) {
  throw new Error(
    `finalized-absent vault count changed during promotion: expected ${expectedDeactivatedVaultCount}, deactivated ${deactivatedVaults.length}`,
  );
}
if (deactivated.length !== absentPolicyAccounts.length) {
  throw new Error(
    `finalized-absent orphan count changed during promotion: expected ${absentPolicyAccounts.length}, deactivated ${deactivated.length}`,
  );
}

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

if (String(continuity?.active_count) !== String(expectedEligibleCount)) {
  throw new Error("active route-policy count changed during promotion");
}
if (String(continuity?.eligible_count) !== String(expectedEligibleCount)) {
  throw new Error("not every active route policy is eligible after promotion");
}
if (
  gateReadback?.start_new_movements !== false ||
  gateReadback?.continue_or_recover_existing !== true
) {
  throw new Error(
    "cross-mint movement gate is not in the safe release posture",
  );
}

console.log(
  [
    "production_route_policy_baseline=PASS",
    `active=${expectedEligibleCount}`,
    `deactivatedFinalizedAbsentVaults=${deactivatedVaults.length}`,
    `clearedAbsentSetupReferences=${clearedSetupReferences.length}`,
    `deactivatedFinalizedAbsentOrphans=${deactivated.length}`,
    `promoted=${promotion.length}`,
    `gateInserted=${gate.length}`,
    `minimumFinalizedSlot=${minimumFinalizedSlot}`,
    "startNewMovements=false",
    "continueOrRecoverExisting=true",
  ].join(" "),
);
