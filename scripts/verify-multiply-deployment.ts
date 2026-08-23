#!/usr/bin/env bun

import { resolve } from "node:path";

const PASS = "PASS_DEPLOYED_RWA_MULTIPLY_WORKER";
const FAIL = "FAIL_DEPLOYED_RWA_MULTIPLY_WORKER";
const BLOCKED = "BLOCKED_DEPLOYED_RWA_MULTIPLY_WORKER";
const RELEASE_PASS = "PASS_RWA_MULTIPLY_RELEASE_CANDIDATE";
const API_BASE = "https://api.render.com/v1";
const OWNER_ID = "tea-d5339hogjchc73et8pg0";
const ENVIRONMENT_ID = "evm-d8kgt4r7uimc73b1ul1g";
const REGISTRY_CREDENTIAL_ID = "rgc-d8kic4bs9h5c73d37l40";
const SERVICE_NAME = "loyal-multiply-route-worker";
const IMAGE = "ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-e6cc09c22e85c4813ab485f016b6ccb6881b10f8";
const COMMAND = "/usr/local/bin/multiply-route-worker run";
const PREDEPLOY = "/usr/local/bin/yield-migrations --apply";
const ROOT = resolve(import.meta.dir, "..");

type Json = Record<string, unknown>;

function emit(verdict: string, condition: string, evidence: Json, exitCode: number): never {
  process.stdout.write(`${JSON.stringify({ verdict, condition, evidence }, null, 2)}\n`);
  process.exit(exitCode);
}

function fail(condition: string, evidence: Json = {}): never {
  return emit(FAIL, condition, evidence, 2);
}

function blocked(condition: string, evidence: Json = {}): never {
  return emit(BLOCKED, condition, evidence, 2);
}

function requiredEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) blocked("terminal_environment_missing", { variable: name, resume: "run through the mounted 1Password environment" });
  return value;
}

async function renderFetch<T>(path: string): Promise<T> {
  const response = await fetch(`${API_BASE}${path}`, {
    headers: {
      Authorization: `Bearer ${requiredEnv("RENDER_API_KEY")}`,
      Accept: "application/json",
    },
    signal: AbortSignal.timeout(20_000),
  });
  if (!response.ok) blocked("render_api_unavailable", { path, status: response.status, resume: "restore Render API access and rerun" });
  return await response.json() as T;
}

function unwrap<T>(value: unknown, key: string): T {
  if (value && typeof value === "object" && key in value) return (value as Record<string, T>)[key];
  return value as T;
}

function equal(actual: unknown, expected: unknown, condition: string): void {
  if (actual !== expected) fail(condition, { expected, actual });
}

async function verifyReleaseCandidate(): Promise<Json> {
  const child = Bun.spawn(
    [process.execPath, resolve(ROOT, "scripts/verify-multiply-production.ts")],
    { cwd: ROOT, stdout: "pipe", stderr: "pipe", env: process.env },
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  let result: Record<string, unknown> | undefined;
  try {
    result = JSON.parse(stdout) as Record<string, unknown>;
  } catch {
    fail("release_candidate_verifier_not_json", { exitCode, stderrTail: stderr.split(/\r?\n/).slice(-10).join("\n") });
  }
  if (exitCode !== 0 || result.verdict !== RELEASE_PASS) {
    fail("release_candidate_verifier_failed", { exitCode, verdict: result.verdict, condition: result.condition });
  }
  return { verdict: result.verdict, condition: result.condition };
}

type Service = {
  id: string;
  name: string;
  type?: string;
  autoDeploy?: string;
  autoDeployTrigger?: string;
  environmentId?: string;
  imagePath?: string;
  suspended?: string;
  registryCredential?: { id?: string; name?: string };
  serviceDetails?: {
    runtime?: string;
    numInstances?: number;
    plan?: string;
    region?: string;
    maxShutdownDelaySeconds?: number;
    envSpecificDetails?: { dockerCommand?: string; preDeployCommand?: string };
  };
};

type Deploy = {
  id: string;
  status?: string;
  createdAt?: string;
  finishedAt?: string;
  image?: { ref?: string; sha?: string; registryCredential?: string };
};

type EnvVar = { key: string; value?: string };
type Instance = { id: string; createdAt?: string };
type Log = { message: string; timestamp: string; labels?: Array<{ name: string; value: string }> };
type Logs = { logs?: Log[] };

function query(parameters: Record<string, string>): string {
  return new URLSearchParams(parameters).toString();
}

async function verifyDeployment(): Promise<Json> {
  const listed = await renderFetch<unknown[]>("/services?limit=100");
  const services = listed.map((entry) => unwrap<Service>(entry, "service"));
  const matches = services.filter((service) => service.name === SERVICE_NAME);
  if (matches.length !== 1) fail("multiply_render_service_count_drift", { count: matches.length });
  const service = matches[0]!;

  equal(service.type, "background_worker", "multiply_render_type_drift");
  equal(service.environmentId, ENVIRONMENT_ID, "multiply_render_environment_drift");
  equal(service.autoDeploy, "no", "multiply_render_auto_deploy_enabled");
  equal(service.autoDeployTrigger, "off", "multiply_render_auto_deploy_trigger_enabled");
  equal(service.imagePath, IMAGE, "multiply_render_image_drift");
  equal(service.registryCredential?.id, REGISTRY_CREDENTIAL_ID, "multiply_render_registry_id_drift");
  equal(service.registryCredential?.name, "loyal-ghcr", "multiply_render_registry_name_drift");
  equal(service.suspended, "not_suspended", "multiply_render_suspended");
  equal(service.serviceDetails?.runtime, "image", "multiply_render_runtime_drift");
  equal(service.serviceDetails?.numInstances, 1, "multiply_render_instance_count_drift");
  equal(service.serviceDetails?.plan, "starter", "multiply_render_plan_drift");
  equal(service.serviceDetails?.region, "oregon", "multiply_render_region_drift");
  equal(service.serviceDetails?.maxShutdownDelaySeconds, 60, "multiply_render_shutdown_drift");
  equal(service.serviceDetails?.envSpecificDetails?.dockerCommand, COMMAND, "multiply_render_command_drift");
  equal(service.serviceDetails?.envSpecificDetails?.preDeployCommand, PREDEPLOY, "multiply_render_predeploy_drift");

  const deployPayload = await renderFetch<unknown[]>(`/services/${service.id}/deploys?limit=1`);
  const deploy = deployPayload[0] ? unwrap<Deploy>(deployPayload[0], "deploy") : undefined;
  if (!deploy) fail("multiply_render_deploy_missing");
  equal(deploy.status, "live", "multiply_render_deploy_not_live");
  equal(deploy.image?.ref, IMAGE, "multiply_render_deploy_image_drift");
  equal(deploy.image?.registryCredential, "loyal-ghcr", "multiply_render_deploy_registry_drift");
  if (!deploy.image?.sha?.startsWith("sha256:")) fail("multiply_render_deploy_digest_missing");

  const envPayload = await renderFetch<unknown[]>(`/services/${service.id}/env-vars?limit=100`);
  const variables = envPayload.map((entry) => unwrap<EnvVar>(entry, "envVar"));
  const env = new Map(variables.map((variable) => [variable.key, variable.value]));
  const expectedKeys = [
    "NEON_DATABASE_URL",
    "OBSERVABILITY_ENABLED",
    "OBSERVABILITY_ENVIRONMENT",
    "OBSERVABILITY_INGESTION_API_KEY",
    "OBSERVABILITY_OTLP_ENDPOINT",
    "POLICY_KEYPAIR",
    "RUST_LOG",
    "SOLANA_RPC_URL",
  ];
  const actualKeys = [...env.keys()].sort();
  if (JSON.stringify(actualKeys) !== JSON.stringify(expectedKeys)) fail("multiply_render_env_key_drift", { actualKeys, expectedKeys });
  equal(env.get("NEON_DATABASE_URL"), requiredEnv("NEON_DATABASE_URL"), "multiply_render_neon_secret_drift");
  equal(env.get("SOLANA_RPC_URL"), requiredEnv("SOLANA_RPC_URL"), "multiply_render_rpc_secret_drift");
  equal(env.get("POLICY_KEYPAIR"), requiredEnv("POLICY_KEYPAIR"), "multiply_render_policy_secret_drift");
  equal(env.get("OBSERVABILITY_ENABLED"), "true", "multiply_render_observability_disabled");
  equal(env.get("OBSERVABILITY_ENVIRONMENT"), "production", "multiply_render_observability_environment_drift");
  equal(env.get("OBSERVABILITY_OTLP_ENDPOINT"), "https://loyal-clickstack.onrender.com", "multiply_render_otlp_endpoint_drift");
  equal(env.get("RUST_LOG"), "warn,loyal_fleet_worker=info", "multiply_render_log_filter_drift");
  if (!env.get("OBSERVABILITY_INGESTION_API_KEY")) fail("multiply_render_ingestion_secret_missing");
  if (env.has("SOLANA_TESTING_PK")) fail("multiply_render_setup_authority_exposed");

  const instancePayload = await renderFetch<unknown[]>(`/services/${service.id}/instances`);
  const instances = instancePayload.map((entry) => unwrap<Instance>(entry, "instance"));
  if (instances.length !== 1 || !instances[0]?.id) fail("multiply_render_live_instance_drift", { count: instances.length });

  const deployStart = new Date(deploy.createdAt ?? 0);
  if (!Number.isFinite(deployStart.getTime())) fail("multiply_render_deploy_time_missing");
  const migrationLogs = await renderFetch<Logs>(`/logs?${query({
    ownerId: OWNER_ID,
    resource: service.id,
    startTime: new Date(deployStart.getTime() - 5_000).toISOString(),
    direction: "forward",
    text: "migration 53",
    limit: "20",
  })}`);
  if (!(migrationLogs.logs ?? []).some((log) => log.message === "migration 53 multiply_production_engine already applied")) {
    fail("multiply_render_migration_53_log_missing");
  }

  const windowStart = new Date(Date.now() - 120_000).toISOString();
  const errorLogs = await renderFetch<Logs>(`/logs?${query({
    ownerId: OWNER_ID,
    resource: service.id,
    startTime: windowStart,
    direction: "forward",
    level: "error",
    limit: "100",
  })}`);
  if ((errorLogs.logs ?? []).length !== 0) fail("multiply_render_error_logs_present", { count: errorLogs.logs?.length });

  const routeLogs = await renderFetch<Logs>(`/logs?${query({
    ownerId: OWNER_ID,
    resource: service.id,
    startTime: windowStart,
    direction: "forward",
    text: "route_complete",
    limit: "100",
  })}`);
  const safeTicks = (routeLogs.logs ?? []).filter((log) =>
    log.message.includes('"condition":"route_complete"')
    && log.message.includes('"operationId":null')
    && log.message.includes('"signature":null')
  );
  const firstTick = new Date(safeTicks[0]?.timestamp ?? 0).getTime();
  const lastTick = new Date(safeTicks.at(-1)?.timestamp ?? 0).getTime();
  if (safeTicks.length < 2 || lastTick - firstTick < 30_000) {
    fail("multiply_render_stable_no_send_window_missing", { tickCount: safeTicks.length, spanMilliseconds: lastTick - firstTick });
  }

  return {
    serviceId: service.id,
    deployId: deploy.id,
    image: deploy.image?.ref,
    digest: deploy.image?.sha,
    registryCredential: deploy.image?.registryCredential,
    instanceId: instances[0]!.id,
    migration53: "already_applied",
    errorLogCount: 0,
    safeRouteCompleteTicks: safeTicks.length,
    safeWindowMilliseconds: lastTick - firstTick,
  };
}

const releaseCandidate = await verifyReleaseCandidate();
const deployment = await verifyDeployment();
process.stdout.write(`${JSON.stringify({
  verdict: PASS,
  condition: "production_worker_deployed_live_and_non_mutating_readback_reconciled",
  evidence: { marker: PASS, releaseCandidate, deployment },
}, null, 2)}\n`);
