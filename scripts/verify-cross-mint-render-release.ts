#!/usr/bin/env bun

import { neon } from "@neondatabase/serverless";

type RenderService = {
  id: string;
  name: string;
  environmentId?: string;
  imagePath?: string;
  suspended?: string;
  registryCredential?: { name?: string };
  serviceDetails?: {
    runtime?: string;
    envSpecificDetails?: {
      dockerCommand?: string;
      preDeployCommand?: string;
    };
  };
};

type RenderDeploy = {
  id: string;
  status: string;
  image?: { ref?: string; sha?: string; registryCredential?: string };
};

type RenderEnvVar = { key: string; value?: string };

const API_BASE = "https://api.render.com/v1";
const PRODUCTION_ENVIRONMENT_ID = "evm-d8kgt4r7uimc73b1ul1g";
const PREDEPLOY = "/usr/local/bin/yield-migrations --apply";

const services = [
  {
    name: "loyal-squads-policy-monitor",
    command:
      "/usr/local/bin/loyal-squads-policy-monitor --cluster mainnet --commitment finalized",
    env: { NEON_DATABASE_URL: null, HELIUS_API_KEY: null },
  },
  {
    name: "loyal-fleet-opportunity-planner",
    command:
      "/usr/local/bin/fleet-opportunity-planner --json --poll-interval-seconds 1 --full-sweep-interval-seconds 30 --dirty-batch-size 256 --max-opportunities-per-wave 128",
    env: {
      EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER: "true",
      EARN_ROUTER_CROSS_MINT_MAX_VALUE_LOSS_BPS: "50",
    },
  },
  {
    name: "loyal-fleet-health-projector",
    command:
      "/usr/local/bin/fleet-health-projector --cluster mainnet-beta --refresh-interval-seconds 5 --lease-seconds 15",
    env: {},
  },
  {
    name: "loyal-fleet-route-revalidator",
    command:
      "/usr/local/bin/same-mint-reserve-swap --fleet-worker revalidate --concurrency 16 --fused-execute-concurrency 8 --poll-interval-milliseconds 250",
    env: {
      JUPITER_API_KEY: null,
      EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER: "true",
      EARN_ROUTER_CROSS_MINT_MAX_SLIPPAGE_BPS: "50",
      EARN_ROUTER_CROSS_MINT_MAX_VALUE_LOSS_BPS: "50",
    },
  },
  {
    name: "loyal-fleet-route-executor",
    command:
      "/usr/local/bin/same-mint-reserve-swap --fleet-worker execute --concurrency 4 --poll-interval-milliseconds 250",
    env: {
      JUPITER_API_KEY: null,
      EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER: "true",
      EARN_ROUTER_CROSS_MINT_MAX_SLIPPAGE_BPS: "50",
      EARN_ROUTER_CROSS_MINT_MAX_VALUE_LOSS_BPS: "50",
    },
  },
  {
    name: "loyal-fleet-route-confirmer",
    command:
      "/usr/local/bin/fleet-route-confirmer --execute --batch-size 128 --broadcast-concurrency 16 --poll-interval-milliseconds 1000",
    env: {},
  },
  {
    name: "loyal-fleet-route-reconciler",
    command:
      "/usr/local/bin/same-mint-reserve-swap --fleet-reconciler --concurrency 64 --batch-size 32 --poll-interval-milliseconds 250 --position-sweep-interval-seconds 300",
    env: {},
  },
] as const;

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} must be set`);
  }
  return value;
}

async function renderFetch<T>(path: string): Promise<T> {
  const response = await fetch(`${API_BASE}${path}`, {
    headers: {
      Authorization: `Bearer ${requiredEnv("RENDER_API_KEY")}`,
      Accept: "application/json",
    },
  });
  if (!response.ok) {
    throw new Error(`Render ${path} returned ${response.status}`);
  }
  return (await response.json()) as T;
}

function unwrap<T>(entry: unknown, key: string): T {
  if (entry && typeof entry === "object" && key in entry) {
    return (entry as Record<string, T>)[key];
  }
  return entry as T;
}

async function latestDeploy(serviceId: string): Promise<RenderDeploy> {
  const payload = await renderFetch<unknown[]>(
    `/services/${serviceId}/deploys?limit=1`,
  );
  const deploy = payload[0]
    ? unwrap<RenderDeploy>(payload[0], "deploy")
    : undefined;
  if (!deploy) {
    throw new Error(`no deploy found for ${serviceId}`);
  }
  return deploy;
}

async function envVars(serviceId: string): Promise<Map<string, RenderEnvVar>> {
  const payload = await renderFetch<unknown[]>(
    `/services/${serviceId}/env-vars?limit=100`,
  );
  const vars = payload.map((entry) => unwrap<RenderEnvVar>(entry, "envVar"));
  return new Map(vars.map((item) => [item.key, item]));
}

function equal(actual: unknown, expected: unknown, label: string): void {
  if (actual !== expected) {
    throw new Error(`${label}: expected ${expected}, got ${String(actual)}`);
  }
}

async function verifyRender(expectedImage: string): Promise<void> {
  const listed = await renderFetch<unknown[]>("/services?limit=100");
  const liveServices = listed.map((entry) => unwrap<RenderService>(entry, "service"));
  const byName = new Map(liveServices.map((service) => [service.name, service]));

  for (const expected of services) {
    const service = byName.get(expected.name);
    if (!service) {
      throw new Error(`Render service missing: ${expected.name}`);
    }
    equal(service.environmentId, PRODUCTION_ENVIRONMENT_ID, `${expected.name} environment`);
    equal(service.serviceDetails?.runtime, "image", `${expected.name} runtime`);
    equal(service.suspended, "not_suspended", `${expected.name} suspension`);
    equal(service.registryCredential?.name, "loyal-ghcr", `${expected.name} registry`);
    equal(
      service.serviceDetails?.envSpecificDetails?.dockerCommand,
      expected.command,
      `${expected.name} command`,
    );
    equal(
      service.serviceDetails?.envSpecificDetails?.preDeployCommand,
      PREDEPLOY,
      `${expected.name} predeploy`,
    );

    const deploy = await latestDeploy(service.id);
    equal(deploy.status, "live", `${expected.name} deploy status`);
    equal(deploy.image?.ref, expectedImage, `${expected.name} deploy image`);
    equal(deploy.image?.registryCredential, "loyal-ghcr", `${expected.name} deploy registry`);
    if (!deploy.image?.sha?.startsWith("sha256:")) {
      throw new Error(`${expected.name} deploy has no immutable image digest`);
    }

    const vars = await envVars(service.id);
    for (const [key, value] of Object.entries(expected.env)) {
      const actual = vars.get(key)?.value;
      if (value === null ? !actual : actual !== value) {
        throw new Error(`${expected.name} has invalid or missing ${key}`);
      }
    }

    console.log(
      `${expected.name}=PASS service=${service.id} deploy=${deploy.id} digest=${deploy.image.sha}`,
    );
  }
}

async function verifyDatabase(): Promise<void> {
  const sql = neon(requiredEnv("NEON_DATABASE_URL"));
  const migrations = await sql`
    SELECT version, name
    FROM loyal_yield.schema_migrations
    WHERE version IN (35, 36, 37)
    ORDER BY version
  `;
  equal(migrations.length, 3, "production cross-mint migrations");
  equal(migrations[0]?.name, "durable_cross_mint_movements", "migration 35");
  equal(migrations[1]?.name, "cross_mint_swap_policies", "migration 36");
  equal(migrations[2]?.name, "cross_mint_vault_opt_ins", "migration 37");

  const [gate] = await sql`
    SELECT start_new_movements, continue_or_recover_existing
    FROM loyal_yield.cross_mint_movement_controls
    WHERE cluster = 'mainnet-beta'
  `;
  equal(gate?.start_new_movements, false, "cross-mint start gate");
  equal(gate?.continue_or_recover_existing, true, "cross-mint recovery gate");

  const [policies] = await sql`
    SELECT
      count(*) FILTER (WHERE active)::BIGINT AS active_count,
      count(*) FILTER (
        WHERE active
          AND cluster = 'mainnet-beta'
      )::BIGINT AS attributed_count,
      count(*) FILTER (
        WHERE active
          AND cluster = 'mainnet-beta'
          AND source_commitment = 'finalized'
          AND finalized_eligible
      )::BIGINT AS eligible_count,
      count(*) FILTER (
        WHERE active
          AND cluster = 'mainnet-beta'
          AND (
            source_commitment <> 'finalized'
            OR NOT finalized_eligible
          )
      )::BIGINT AS invalid_attributed_count
    FROM loyal_yield.route_policies
  `;
  equal(
    String(policies?.invalid_attributed_count),
    "0",
    "attributed active Earn policy eligibility",
  );

  const [movements] = await sql`
    SELECT count(*)::BIGINT AS active_count
    FROM loyal_yield.rebalance_decisions
    WHERE movement_route = 'cross_mint_jupiter'
      AND terminal_outcome IS NULL
  `;
  equal(String(movements?.active_count), "0", "active cross-mint movement count");

  const [enrollments] = await sql`
    SELECT count(*)::BIGINT AS enrollment_count
    FROM loyal_yield.cross_mint_vault_opt_ins
    WHERE cluster = 'mainnet-beta'
  `;

  console.log(
    `production_database=PASS activeEarnPolicies=${policies?.active_count} attributedEarnPolicies=${policies?.attributed_count} eligibleEarnPolicies=${policies?.eligible_count} autoswapEnrollments=${enrollments?.enrollment_count} activeCrossMintMovements=0 startNewMovements=false`,
  );
}

const expectedImage = requiredEnv("EXPECTED_LIGHT_WORKER_IMAGE");
if (!/^ghcr\.io\/loyal-labs\/loyal-yield-routing\/light-workers:sha-[0-9a-f]{40}$/.test(expectedImage)) {
  throw new Error("EXPECTED_LIGHT_WORKER_IMAGE must be an immutable full-commit tag");
}

await verifyRender(expectedImage);
await verifyDatabase();
console.log("PASS_READY_FOR_FRONTEND_WIRING");
