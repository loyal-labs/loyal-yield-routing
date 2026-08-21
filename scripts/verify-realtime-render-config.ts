type RenderService = {
  id: string;
  name: string;
  type: string;
  serviceDetails?: {
    runtime?: string;
    healthCheckPath?: string;
    numInstances?: number;
    envSpecificDetails?: {
      dockerCommand?: string;
    };
  };
};

type RenderDeploy = {
  id: string;
  status: string;
  image?: {
    ref?: string;
    sha?: string;
    registryCredential?: string;
  };
  finishedAt?: string;
};

type RenderEnvVar = {
  key: string;
  value?: string;
};

const API_BASE = "https://api.render.com/v1";
const DEFAULT_REALTIME_SERVICE_ID = "srv-d966hcpkh4rs73da0j4g";
const DEFAULT_AUTODEPOSIT_SERVICE_ID = "srv-d8lplql7vvec73f1it6g";
const LIGHT_WORKER_PREFIX =
  "ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-";

function printHelp() {
  console.log(`Usage: bun run verify:realtime:render-config

Required env:
  RENDER_API_KEY

Optional env:
  REALTIME_RENDER_SERVICE_ID      default ${DEFAULT_REALTIME_SERVICE_ID}
  AUTODEPOSIT_RENDER_SERVICE_ID   default ${DEFAULT_AUTODEPOSIT_SERVICE_ID}
  EXPECTED_LIGHT_WORKER_IMAGE     exact ghcr.io/.../light-workers:sha-<commit>

Checks live Render service shape without printing secrets.`);
}

function env(name: string): string {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} must be set`);
  }
  return value;
}

function optionalEnv(name: string, fallback: string): string {
  return process.env[name] || fallback;
}

async function renderFetch<T>(path: string): Promise<T> {
  const response = await fetch(`${API_BASE}${path}`, {
    headers: {
      Authorization: `Bearer ${env("RENDER_API_KEY")}`,
      Accept: "application/json",
    },
  });
  if (!response.ok) {
    throw new Error(`Render ${path} returned ${response.status}`);
  }
  return (await response.json()) as T;
}

function normalizeDeploys(payload: unknown): RenderDeploy[] {
  if (!Array.isArray(payload)) {
    return [];
  }
  return payload
    .map((entry) => {
      if (entry && typeof entry === "object" && "deploy" in entry) {
        return (entry as { deploy: RenderDeploy }).deploy;
      }
      return entry as RenderDeploy;
    })
    .filter((entry) => entry && typeof entry.id === "string");
}

function normalizeEnvVars(payload: unknown): RenderEnvVar[] {
  if (!Array.isArray(payload)) {
    return [];
  }
  return payload
    .map((entry) => {
      if (entry && typeof entry === "object" && "envVar" in entry) {
        return (entry as { envVar: RenderEnvVar }).envVar;
      }
      return entry as RenderEnvVar;
    })
    .filter((entry) => entry && typeof entry.key === "string");
}

function requireEqual(actual: unknown, expected: unknown, label: string) {
  if (actual !== expected) {
    throw new Error(`${label}: expected ${expected}, got ${String(actual)}`);
  }
}

function requireTruthy(value: unknown, label: string) {
  if (!value) {
    throw new Error(`${label} missing`);
  }
}

function requireEnvValue(
  vars: Map<string, RenderEnvVar>,
  key: string,
  expected: string
) {
  requireEqual(vars.get(key)?.value, expected, `Render env ${key}`);
}

function safeHostFromUrl(raw: string | undefined): string {
  if (!raw) {
    throw new Error("NEON_DATABASE_URL value missing from Render readback");
  }
  const parsed = new URL(raw);
  const host = parsed.host;
  if (host.includes("-pooler.")) {
    throw new Error(`NEON_DATABASE_URL must be direct, got pooled host ${host}`);
  }
  return host;
}

async function latestDeploy(serviceId: string): Promise<RenderDeploy> {
  const payload = await renderFetch<unknown>(
    `/services/${serviceId}/deploys?limit=1`
  );
  const [deploy] = normalizeDeploys(payload);
  if (!deploy) {
    throw new Error(`no deploys returned for ${serviceId}`);
  }
  return deploy;
}

async function envVars(serviceId: string): Promise<Map<string, RenderEnvVar>> {
  const payload = await renderFetch<unknown>(
    `/services/${serviceId}/env-vars?limit=100`
  );
  return new Map(normalizeEnvVars(payload).map((item) => [item.key, item]));
}

function checkImage(deploy: RenderDeploy, label: string) {
  requireEqual(deploy.status, "live", `${label} latest deploy status`);
  const expected = process.env.EXPECTED_LIGHT_WORKER_IMAGE;
  const image = deploy.image?.ref || "";
  if (expected) {
    requireEqual(image, expected, `${label} image`);
  } else if (!image.startsWith(LIGHT_WORKER_PREFIX)) {
    throw new Error(`${label} image is not immutable light-worker tag: ${image}`);
  }
  if (deploy.image?.registryCredential !== "loyal-ghcr") {
    throw new Error(`${label} must use loyal-ghcr registry credential`);
  }
}

async function checkRealtimeService(serviceId: string) {
  const service = await renderFetch<RenderService>(`/services/${serviceId}`);
  requireEqual(service.name, "loyal-yield-realtime", "realtime name");
  requireEqual(service.type, "web_service", "realtime type");
  requireEqual(service.serviceDetails?.runtime, "image", "realtime runtime");
  requireEqual(
    service.serviceDetails?.envSpecificDetails?.dockerCommand,
    "/usr/local/bin/loyal-yield-realtime",
    "realtime command"
  );
  requireEqual(service.serviceDetails?.healthCheckPath, "/healthz", "health");
  requireEqual(service.serviceDetails?.numInstances, 1, "realtime instances");

  const deploy = await latestDeploy(serviceId);
  checkImage(deploy, "realtime");

  const vars = await envVars(serviceId);
  requireTruthy(vars.get("REALTIME_AUTH_SECRET"), "REALTIME_AUTH_SECRET");
  requireEnvValue(
    vars,
    "REALTIME_ALLOWED_ORIGINS",
    "https://askloyal.com,https://www.askloyal.com"
  );
  requireEnvValue(
    vars,
    "REALTIME_ALLOWED_VERCEL_PREVIEW_PROJECT",
    "loyal-frontend"
  );
  requireEnvValue(
    vars,
    "REALTIME_ALLOWED_VERCEL_PREVIEW_TEAM",
    "loyal-team"
  );
  requireEnvValue(vars, "REALTIME_HEARTBEAT_SECONDS", "15");
  requireEnvValue(vars, "REALTIME_CATCH_UP_LIMIT", "500");
  requireEnvValue(vars, "REALTIME_CLIENT_BUFFER", "1024");
  requireEnvValue(vars, "REALTIME_MAX_TOKEN_LIFETIME_SECONDS", "300");
  requireEnvValue(vars, "REALTIME_RETENTION_DAYS", "7");
  requireEnvValue(vars, "REALTIME_RETENTION_BATCH_SIZE", "1000");
  requireEnvValue(vars, "REALTIME_RETENTION_INTERVAL_SECONDS", "3600");
  requireEnvValue(vars, "REALTIME_READY_MAX_LAG", "1000");
  const neonHost = safeHostFromUrl(vars.get("NEON_DATABASE_URL")?.value);

  console.log(
    [
      "realtime=PASS",
      `service=${service.id}`,
      `deploy=${deploy.id}`,
      `image=${deploy.image?.ref}`,
      `digest=${deploy.image?.sha}`,
      `neonHost=${neonHost}`,
    ].join(" ")
  );
}

async function checkAutodepositService(serviceId: string) {
  const service = await renderFetch<RenderService>(`/services/${serviceId}`);
  requireEqual(
    service.name,
    "loyal-balance-sweep-autodeposit-trigger",
    "autodeposit name"
  );
  requireEqual(service.type, "background_worker", "autodeposit type");
  requireEqual(service.serviceDetails?.runtime, "image", "autodeposit runtime");
  requireEqual(
    service.serviceDetails?.envSpecificDetails?.dockerCommand,
    "/usr/local/bin/balance-sweep-autodeposit-trigger --execute-eligible",
    "autodeposit command"
  );

  const deploy = await latestDeploy(serviceId);
  checkImage(deploy, "autodeposit");

  const vars = await envVars(serviceId);
  const executor = vars.get("BALANCE_SWEEP_EXECUTOR_COMMAND")?.value || "";
  if (!executor.includes("--require-lot-claim")) {
    throw new Error("autodeposit executor command must include --require-lot-claim");
  }
  requireEqual(
    vars.get("BALANCE_SWEEP_EXECUTE_ELIGIBLE")?.value,
    "true",
    "autodeposit execute env"
  );
  requireEnvValue(
    vars,
    "BALANCE_SWEEP_REALTIME_DEBOUNCE_MILLISECONDS",
    "250"
  );
  requireEnvValue(
    vars,
    "BALANCE_SWEEP_REALTIME_CHANNEL",
    "loyal_yield_autodeposit_wakeup"
  );
  const neonHost = safeHostFromUrl(vars.get("NEON_DATABASE_URL")?.value);

  console.log(
    [
      "autodeposit=PASS",
      `service=${service.id}`,
      `deploy=${deploy.id}`,
      `image=${deploy.image?.ref}`,
      `digest=${deploy.image?.sha}`,
      `neonHost=${neonHost}`,
    ].join(" ")
  );
}

async function main() {
  if (process.argv.includes("--help") || process.argv.includes("-h")) {
    printHelp();
    return;
  }

  await checkRealtimeService(
    optionalEnv("REALTIME_RENDER_SERVICE_ID", DEFAULT_REALTIME_SERVICE_ID)
  );
  await checkAutodepositService(
    optionalEnv("AUTODEPOSIT_RENDER_SERVICE_ID", DEFAULT_AUTODEPOSIT_SERVICE_ID)
  );
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exit(1);
});
