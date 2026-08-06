#!/usr/bin/env bun
/**
 * List Render services for the active workspace as a plain-text table.
 *
 * Wraps `render services --output json`. Any extra arguments are passed
 * straight through to the CLI, so workspace filters keep working:
 *
 *   bun run render:services
 *   bun run render:services -- --include-previews
 *   bun run render:services -- -e evm-d9c3vo1kh4rs73c0p52g
 *
 * `--env` filters locally by environment *name*, which the CLI cannot do:
 * its `-e` flag takes environment IDs, and each project has its own
 * separately-identified "production" and "staging".
 *
 *   bun run render:services -- --env production
 *   bun run render:services -- --env staging,test
 *   bun run render:services -- --env none      # services outside any project
 *
 * `--commits` and `--metrics` add columns the CLI does not expose, read from
 * the Render REST API with the token `render login` already stored:
 *
 *   bun run render:services -- --commits
 *   bun run render:services -- --metrics --metrics-window 24h
 *
 * `--sort` orders by any column, on the number behind the cell rather than its
 * text, and switches on whichever columns it needs:
 *
 *   bun run render:services -- --sort -cpu
 *   bun run render:services -- --sort -drift,name
 */

import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";

type RenderResource = {
  id?: string;
  name?: string;
  type?: string;
  suspended?: string;
  suspenders?: string[];
  serviceDetails?: {
    plan?: string;
    region?: string;
    runtime?: string;
    env?: string;
  };
};

type RenderEntry = Record<string, unknown> & {
  project?: { name?: string };
  environment?: { name?: string };
};

/** Thrown when a required CLI is missing or unauthenticated. */
export class CliDependencyError extends Error {}

export type CliDependency = {
  /** Binary that must be on PATH. */
  command: string;
  /** How to install it, shown when the binary is missing. */
  installHint: string;
  /** Command proving the CLI is authorized; exit code is what counts. */
  authCheck?: { args: string[]; hint: string };
};

export const RENDER_CLI: CliDependency = {
  command: "render",
  installHint: "Install it with: brew install render",
  authCheck: {
    args: ["whoami"],
    hint: "Authorize it with: render login",
  },
};

/**
 * Verifies each CLI is installed and authorized before any real work starts,
 * so failures surface as one actionable message rather than mid-run.
 */
export function verifyCliDependencies(dependencies: CliDependency[]): void {
  for (const dependency of dependencies) {
    if (!Bun.which(dependency.command)) {
      throw new CliDependencyError(
        `Required CLI not found on PATH: ${dependency.command}\n` +
          `  ${dependency.installHint}`,
      );
    }

    const authCheck = dependency.authCheck;
    if (!authCheck) {
      continue;
    }

    const result = Bun.spawnSync([dependency.command, ...authCheck.args], {
      stdout: "pipe",
      stderr: "pipe",
    });

    if (result.exitCode !== 0) {
      const detail = new TextDecoder().decode(result.stderr).trim();
      throw new CliDependencyError(
        `${dependency.command} is not authorized ` +
          `(\`${dependency.command} ${authCheck.args.join(" ")}\` exited ` +
          `${result.exitCode}).\n  ${authCheck.hint}` +
          (detail ? `\n\n${detail}` : ""),
      );
    }
  }
}

const RENDER_API_BASE = "https://api.render.com/v1";
const RENDER_CLI_CONFIG = `${homedir()}/.render/cli.yaml`;

let cachedApiKey: string | undefined;

/**
 * Reuses the token `render login` already stored, so the API-only columns need
 * no separate secret. RENDER_API_KEY wins when set, for CI and for personal
 * keys that outlive a CLI session.
 */
function renderApiKey(): string {
  if (cachedApiKey) {
    return cachedApiKey;
  }

  const fromEnv = process.env.RENDER_API_KEY?.trim();
  if (fromEnv) {
    cachedApiKey = fromEnv;
    return cachedApiKey;
  }

  if (!existsSync(RENDER_CLI_CONFIG)) {
    throw new CliDependencyError(
      `No Render credential found: ${RENDER_CLI_CONFIG} is missing.\n` +
        "  Authorize the CLI with: render login, or set RENDER_API_KEY.",
    );
  }

  const config = Bun.YAML.parse(readFileSync(RENDER_CLI_CONFIG, "utf8")) as {
    api?: { key?: string; expires_at?: number };
  };

  const key = config.api?.key;
  if (!key) {
    throw new CliDependencyError(
      `No API key in ${RENDER_CLI_CONFIG}.\n` +
        "  Authorize the CLI with: render login, or set RENDER_API_KEY.",
    );
  }

  const expiresAt = config.api?.expires_at;
  if (typeof expiresAt === "number" && expiresAt * 1000 <= Date.now()) {
    throw new CliDependencyError(
      "The stored Render CLI session expired " +
        `(${new Date(expiresAt * 1000).toISOString()}).\n` +
        "  Refresh it with: render login, or set RENDER_API_KEY.",
    );
  }

  cachedApiKey = key;
  return cachedApiKey;
}

async function renderApi<T>(path: string, params?: URLSearchParams): Promise<T> {
  const url = `${RENDER_API_BASE}${path}${params ? `?${params}` : ""}`;
  const response = await fetch(url, {
    headers: {
      Authorization: `Bearer ${renderApiKey()}`,
      Accept: "application/json",
    },
  });

  if (response.status === 401 || response.status === 403) {
    throw new CliDependencyError(
      `Render API rejected the credential (${response.status}) on ${path}.\n` +
        "  Refresh it with: render login, or set RENDER_API_KEY.",
    );
  }

  if (!response.ok) {
    throw new Error(`Render API ${path} returned ${response.status}`);
  }

  return (await response.json()) as T;
}

const TYPE_LABELS: Record<string, string> = {
  background_worker: "worker",
  web_service: "web",
  private_service: "private",
  static_site: "static",
  cron_job: "cron",
};

// Datastores come back under their own key instead of `service`.
const RESOURCE_KEYS = ["service", "postgres", "redis", "keyValue"] as const;

/** Printed where a service simply has no reading, e.g. while suspended. */
const EMPTY_CELL = "-";

const DEFAULT_METRICS_WINDOW_MS = 60 * 60 * 1_000;
const DURATION_UNITS: Record<string, number> = {
  s: 1_000,
  m: 60_000,
  h: 3_600_000,
  d: 86_400_000,
};

/** Accepts `30s`, `15m`, `2h`, `1d`; a bare number is minutes. */
function parseDuration(value: string): number {
  const match = /^(\d+(?:\.\d+)?)([smhd])?$/.exec(value.trim());
  const amount = match ? Number(match[1]) : Number.NaN;

  if (!match || !(amount > 0)) {
    throw new Error(
      `--metrics-window expects a positive duration like 15m, 2h or 1d; ` +
        `got: ${value}`,
    );
  }

  return amount * DURATION_UNITS[match[2] ?? "m"];
}

type Options = {
  envFilter: Set<string> | undefined;
  showCommits: boolean;
  showMetrics: boolean;
  metricsWindowMs: number;
  refreshBaseline: boolean;
  sortKeys: SortKey[];
  passthrough: string[];
};

/**
 * Splits our own flags out of the argument list; everything else is forwarded
 * to the Render CLI untouched.
 */
function parseArgs(argv: string[]): Options {
  const envNames: string[] = [];
  const passthrough: string[] = [];
  const sortKeys: SortKey[] = [];
  let showCommits = false;
  let showMetrics = false;
  let metricsWindowMs = DEFAULT_METRICS_WINDOW_MS;
  let refreshBaseline = true;

  // Supports both `--flag value` and `--flag=value`.
  const valueOf = (arg: string, index: number, flag: string): string | undefined => {
    if (arg === flag) {
      const value = argv[index + 1];
      if (value === undefined) {
        throw new Error(`${flag} requires a value`);
      }
      return value;
    }
    return arg.startsWith(`${flag}=`) ? arg.slice(flag.length + 1) : undefined;
  };

  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];

    const envValue = valueOf(arg, index, "--env");
    if (envValue !== undefined) {
      envNames.push(envValue);
      if (arg === "--env") {
        index += 1;
      }
      continue;
    }

    const windowValue = valueOf(arg, index, "--metrics-window");
    if (windowValue !== undefined) {
      metricsWindowMs = parseDuration(windowValue);
      if (arg === "--metrics-window") {
        index += 1;
      }
      continue;
    }

    const sortValue = valueOf(arg, index, "--sort");
    if (sortValue !== undefined) {
      sortKeys.push(...parseSortKeys(sortValue));
      if (arg === "--sort") {
        index += 1;
      }
      continue;
    }

    if (arg === "--commits") {
      showCommits = true;
      continue;
    }

    if (arg === "--metrics") {
      showMetrics = true;
      continue;
    }

    if (arg === "--no-fetch") {
      refreshBaseline = false;
      continue;
    }

    passthrough.push(arg);
  }

  const names = envNames
    .flatMap((value) => value.split(","))
    .map((value) => value.trim().toLowerCase())
    .filter((value) => value.length > 0);

  return {
    envFilter: names.length > 0 ? new Set(names) : undefined,
    // Sorting by a column implies showing it; the order makes no sense
    // otherwise, and the numbers behind it have to be fetched either way.
    showCommits: showCommits || sortKeys.some((key) => key.spec.needs === "commits"),
    showMetrics: showMetrics || sortKeys.some((key) => key.spec.needs === "metrics"),
    metricsWindowMs,
    refreshBaseline,
    sortKeys,
    passthrough,
  };
}

export function runRenderServices(passthroughArgs: string[] = []): RenderEntry[] {
  const result = Bun.spawnSync(
    ["render", "services", "--output", "json", ...passthroughArgs],
    { stdout: "pipe", stderr: "pipe" },
  );

  const stderr = new TextDecoder().decode(result.stderr).trim();

  if (result.exitCode !== 0) {
    throw new Error(
      `render services failed with exit code ${result.exitCode}` +
        (stderr ? `:\n${stderr}` : ""),
    );
  }

  const stdout = new TextDecoder().decode(result.stdout).trim();
  if (!stdout) {
    return [];
  }

  const parsed: unknown = JSON.parse(stdout);
  if (!Array.isArray(parsed)) {
    throw new Error("Unexpected render CLI output: expected a JSON array");
  }

  return parsed as RenderEntry[];
}

export function resourceOf(entry: RenderEntry): RenderResource | undefined {
  for (const key of RESOURCE_KEYS) {
    const candidate = entry[key];
    if (candidate && typeof candidate === "object") {
      return candidate as RenderResource;
    }
  }
  return undefined;
}

function statusOf(resource: RenderResource): string {
  const suspended = resource.suspended;
  if (!suspended || suspended === "not_suspended") {
    return "active";
  }
  const suspenders = resource.suspenders ?? [];
  return suspenders.length > 0
    ? `${suspended} (${suspenders.join(", ")})`
    : suspended;
}

type RenderDeploy = {
  id?: string;
  status?: string;
  commit?: { id?: string } | null;
  image?: { ref?: string } | null;
};

/** How far back to look for the deploy that is actually serving traffic. */
const DEPLOY_LOOKBACK = 20;
const IMAGE_COMMIT_TAG = /:sha-([0-9a-f]{7,40})$/;

/** Only services have deploys; datastores share the listing but have none. */
function hasDeploys(serviceId: string): boolean {
  return serviceId.startsWith("srv-");
}

function normalizeDeploys(payload: unknown): RenderDeploy[] {
  if (!Array.isArray(payload)) {
    return [];
  }
  return payload.map((entry) =>
    entry && typeof entry === "object" && "deploy" in entry
      ? ((entry as { deploy: RenderDeploy }).deploy ?? {})
      : (entry as RenderDeploy),
  );
}

/**
 * Workers here run prebuilt images tagged `sha-<commit>`, so the tag is the
 * commit. Git-backed services report the commit directly instead.
 */
function deployedCommit(deploy: RenderDeploy): string | undefined {
  const fromCommit = deploy.commit?.id;
  if (fromCommit) {
    return fromCommit;
  }
  return IMAGE_COMMIT_TAG.exec(deploy.image?.ref ?? "")?.[1];
}

/**
 * Returns the newest deploy that reached `live` for each service, which is the
 * code actually running: the newest deploy overall may be mid-build or failed.
 */
async function fetchLiveDeploys(
  serviceIds: string[],
): Promise<Map<string, RenderDeploy>> {
  const results = await Promise.all(
    serviceIds.filter(hasDeploys).map(async (serviceId) => {
      const payload = await renderApi<unknown>(
        `/services/${serviceId}/deploys`,
        new URLSearchParams({ limit: String(DEPLOY_LOOKBACK) }),
      );
      const deploys = normalizeDeploys(payload);
      const live = deploys.find((deploy) => deploy.status === "live");
      return [serviceId, live] as const;
    }),
  );

  const byService = new Map<string, RenderDeploy>();
  for (const [serviceId, deploy] of results) {
    if (deploy) {
      byService.set(serviceId, deploy);
    }
  }
  return byService;
}

const BASELINE_REF = "origin/main";

type CommitInfo = {
  committedAt?: Date;
  /** Commits in BASELINE_REF that the deployed commit does not have. */
  behind?: number;
  /** Commits the deployed commit has that BASELINE_REF does not. */
  ahead?: number;
};

function git(args: string[]): { stdout: string; ok: boolean } {
  const result = Bun.spawnSync(["git", ...args], {
    stdout: "pipe",
    stderr: "pipe",
  });
  return {
    stdout: new TextDecoder().decode(result.stdout).trim(),
    ok: result.exitCode === 0,
  };
}

function commitDate(revision: string): Date | undefined {
  const { stdout, ok } = git(["log", "-1", "--format=%ct", revision]);
  if (!ok || !stdout) {
    return undefined;
  }
  return new Date(Number(stdout) * 1000);
}

/**
 * Resolves BASELINE_REF, refreshing it first so "behind" counts reflect what is
 * on the remote right now rather than whenever this checkout last fetched.
 */
function resolveBaseline(refresh: boolean): { commit: string; committedAt?: Date } {
  if (!git(["rev-parse", "--git-dir"]).ok) {
    throw new Error(
      "--commits needs a git checkout to compare deployed commits against; " +
        "run it from the repository.",
    );
  }

  if (refresh && !git(["fetch", "origin", "main", "--quiet"]).ok) {
    console.warn(
      `Warning: could not fetch origin/main; comparing against the local ` +
        `${BASELINE_REF} as last fetched.`,
    );
  }

  const { stdout, ok } = git(["rev-parse", BASELINE_REF]);
  if (!ok || !stdout) {
    throw new Error(
      `Cannot resolve ${BASELINE_REF} in this checkout, so deployed commits ` +
        "have nothing to compare against.",
    );
  }

  return { commit: stdout, committedAt: commitDate(stdout) };
}

function loadCommitInfo(
  commits: Iterable<string>,
  baseline: string,
): Map<string, CommitInfo> {
  const info = new Map<string, CommitInfo>();

  for (const commit of new Set(commits)) {
    // Deploys can name commits this checkout never fetched; those stay unknown
    // rather than failing the whole table.
    if (!git(["cat-file", "-e", `${commit}^{commit}`]).ok) {
      info.set(commit, {});
      continue;
    }

    const counts = git([
      "rev-list",
      "--left-right",
      "--count",
      `${commit}...${baseline}`,
    ]);
    const [ahead, behind] = counts.ok
      ? counts.stdout.split(/\s+/).map(Number)
      : [undefined, undefined];

    info.set(commit, {
      committedAt: commitDate(commit),
      ahead,
      behind,
    });
  }

  return info;
}

/** `-2` is two commits behind origin/main; `+1 -2` has also diverged from it. */
function formatDrift(info: CommitInfo | undefined): string {
  if (!info || info.behind === undefined || info.ahead === undefined) {
    return "?";
  }
  if (info.ahead === 0 && info.behind === 0) {
    return "0";
  }
  return [info.ahead > 0 ? `+${info.ahead}` : "", info.behind > 0 ? `-${info.behind}` : ""]
    .filter((part) => part.length > 0)
    .join(" ");
}

function formatUtc(date: Date | undefined): string {
  if (!date) {
    return EMPTY_CELL;
  }
  return date.toISOString().slice(0, 16).replace("T", " ");
}

type MetricSeries = {
  labels?: Array<{ field?: string; value?: string }>;
  unit?: string;
  values?: Array<{ timestamp?: string; value?: number }>;
};

type ServiceMetrics = {
  cpu?: number;
  cpuLimit?: number;
  memory?: number;
  memoryLimit?: number;
  /** MB transferred per hour. */
  bandwidth?: number;
  /** HTTP requests per hour, across every status code. */
  requests?: number;
  instances?: number;
};

const HOUR_SECONDS = 3_600;
/** Bandwidth and request counts arrive in hourly buckets; the last one is
 * still filling, so a short window may contain no closed bucket at all. */
const RATE_WINDOW_MS = 3 * 60 * 60 * 1_000;
const MIN_RESOLUTION_SECONDS = 60;
const TARGET_SAMPLES = 60;

function seriesResource(series: MetricSeries): string | undefined {
  return series.labels?.find((label) => label.field === "resource")?.value;
}

function groupByResource(all: MetricSeries[]): Map<string, MetricSeries[]> {
  const grouped = new Map<string, MetricSeries[]>();
  for (const series of all) {
    const resource = seriesResource(series);
    if (!resource) {
      continue;
    }
    const existing = grouped.get(resource);
    if (existing) {
      existing.push(series);
    } else {
      grouped.set(resource, [series]);
    }
  }
  return grouped;
}

async function fetchMetric(
  metric: string,
  resourceIds: string[],
  start: Date,
  end: Date,
  resolutionSeconds: number,
): Promise<Map<string, MetricSeries[]>> {
  const params = new URLSearchParams({
    startTime: start.toISOString(),
    endTime: end.toISOString(),
    resolutionSeconds: String(resolutionSeconds),
  });
  for (const resourceId of resourceIds) {
    params.append("resource", resourceId);
  }

  const payload = await renderApi<MetricSeries[]>(`/metrics/${metric}`, params);
  return groupByResource(Array.isArray(payload) ? payload : []);
}

/**
 * Mean of every sample across every instance, i.e. what one instance typically
 * used over the window. Keeps the percentage comparable to the per-instance
 * limit that Render reports.
 */
function meanSample(series: MetricSeries[] | undefined): number | undefined {
  let total = 0;
  let count = 0;
  for (const entry of series ?? []) {
    for (const point of entry.values ?? []) {
      if (typeof point.value === "number") {
        total += point.value;
        count += 1;
      }
    }
  }
  return count > 0 ? total / count : undefined;
}

function maxSample(series: MetricSeries[] | undefined): number | undefined {
  let max: number | undefined;
  for (const entry of series ?? []) {
    for (const point of entry.values ?? []) {
      if (typeof point.value === "number" && (max === undefined || point.value > max)) {
        max = point.value;
      }
    }
  }
  return max;
}

function lastSample(series: MetricSeries[] | undefined): number | undefined {
  let latest: { at: number; value: number } | undefined;
  for (const entry of series ?? []) {
    for (const point of entry.values ?? []) {
      const at = Date.parse(point.timestamp ?? "");
      if (typeof point.value !== "number" || Number.isNaN(at)) {
        continue;
      }
      if (!latest || at > latest.at) {
        latest = { at, value: point.value };
      }
    }
  }
  return latest?.value;
}

/**
 * Counters (bandwidth, requests) are per-bucket totals split across series such
 * as status code, so they are summed per timestamp and then averaged over the
 * buckets that have already closed. Buckets still filling would read low.
 */
function meanClosedBucket(
  series: MetricSeries[] | undefined,
  resolutionSeconds: number,
  now: number,
): number | undefined {
  const perBucket = new Map<number, number>();

  for (const entry of series ?? []) {
    for (const point of entry.values ?? []) {
      const at = Date.parse(point.timestamp ?? "");
      if (typeof point.value !== "number" || Number.isNaN(at)) {
        continue;
      }
      if (at + resolutionSeconds * 1_000 > now) {
        continue;
      }
      perBucket.set(at, (perBucket.get(at) ?? 0) + point.value);
    }
  }

  if (perBucket.size === 0) {
    return undefined;
  }
  const totals = [...perBucket.values()];
  return totals.reduce((sum, value) => sum + value, 0) / totals.length;
}

async function collectMetrics(
  resourceIds: string[],
  windowMs: number,
): Promise<Map<string, ServiceMetrics>> {
  const end = new Date();
  const start = new Date(end.getTime() - windowMs);
  const resolutionSeconds = Math.max(
    MIN_RESOLUTION_SECONDS,
    Math.round(windowMs / 1_000 / TARGET_SAMPLES),
  );
  const rateStart = new Date(end.getTime() - Math.max(windowMs, RATE_WINDOW_MS));

  const gauge = (metric: string) =>
    fetchMetric(metric, resourceIds, start, end, resolutionSeconds);
  const counter = (metric: string) =>
    fetchMetric(metric, resourceIds, rateStart, end, HOUR_SECONDS);

  const [cpu, cpuLimit, memory, memoryLimit, instances, bandwidth, requests] =
    await Promise.all([
      gauge("cpu"),
      gauge("cpu-limit"),
      gauge("memory"),
      gauge("memory-limit"),
      gauge("instance-count"),
      counter("bandwidth"),
      counter("http-requests"),
    ]);

  const now = end.getTime();
  const metrics = new Map<string, ServiceMetrics>();

  for (const resourceId of resourceIds) {
    metrics.set(resourceId, {
      cpu: meanSample(cpu.get(resourceId)),
      cpuLimit: maxSample(cpuLimit.get(resourceId)),
      memory: meanSample(memory.get(resourceId)),
      memoryLimit: maxSample(memoryLimit.get(resourceId)),
      instances: lastSample(instances.get(resourceId)),
      bandwidth: meanClosedBucket(bandwidth.get(resourceId), HOUR_SECONDS, now),
      requests: meanClosedBucket(requests.get(resourceId), HOUR_SECONDS, now),
    });
  }

  return metrics;
}

function formatPercent(share: number): string {
  const percent = share * 100;
  if (percent >= 10) {
    return `${percent.toFixed(0)}%`;
  }
  return `${percent.toFixed(percent >= 1 ? 1 : 2)}%`;
}

/** Trims trailing zeros so plan limits read as `1` and `0.5`, not `1.000`. */
function formatNumber(value: number, decimals: number): string {
  return value
    .toFixed(decimals)
    .replace(/(\.\d*?)0+$/, "$1")
    .replace(/\.$/, "");
}

/** Keeps small readings legible: idle workers sit well under 0.01 cores. */
function formatCores(cores: number): string {
  if (cores >= 1) {
    return formatNumber(cores, 2);
  }
  return formatNumber(cores, cores >= 0.01 ? 3 : 4);
}

function formatWindow(ms: number): string {
  const minutes = ms / 60_000;
  return minutes >= 60 && minutes % 60 === 0
    ? `${formatNumber(minutes / 60, 0)}h`
    : `${formatNumber(minutes, 1)}m`;
}

const BYTE_UNITS = ["B", "Ki", "Mi", "Gi", "Ti"];

function formatBytes(bytes: number): string {
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)}${BYTE_UNITS[unit]}`;
}

/** Percent of the limit first, then the raw reading the percent came from. */
function formatAgainstLimit(
  used: number | undefined,
  limit: number | undefined,
  format: (value: number) => string,
): string {
  if (used === undefined) {
    return EMPTY_CELL;
  }
  if (limit === undefined || limit === 0) {
    return format(used);
  }
  return `${formatPercent(used / limit)} (${format(used)}/${format(limit)})`;
}

type Row = {
  name: string;
  id: string;
  type: string;
  plan: string;
  region: string;
  status: string;
  project: string;
  environment: string;
  commit?: string;
  drift?: string;
  committed?: string;
  cpu?: string;
  memory?: string;
  network?: string;
  requests?: string;
  instances?: string;
};

function toRow(entry: RenderEntry): Row | undefined {
  const resource = resourceOf(entry);
  if (!resource?.id) {
    return undefined;
  }

  const details = resource.serviceDetails ?? {};
  const type = resource.type ?? "";

  return {
    name: resource.name ?? "",
    id: resource.id,
    type: TYPE_LABELS[type] ?? type,
    plan: details.plan ?? "",
    region: details.region ?? "",
    status: statusOf(resource),
    project: entry.project?.name ?? "",
    environment: entry.environment?.name ?? "",
  };
}

type Column = {
  header: string;
  key: keyof Row;
  /** Dropped when no row has a reading, e.g. requests outside web services. */
  optional?: boolean;
};

const BASE_COLUMNS: Column[] = [
  { header: "NAME", key: "name" },
  { header: "SERVICE ID", key: "id" },
  { header: "TYPE", key: "type" },
  { header: "PLAN", key: "plan" },
  { header: "REGION", key: "region" },
  { header: "STATUS", key: "status" },
];

const COMMIT_COLUMNS: Column[] = [
  { header: "COMMIT", key: "commit" },
  { header: "DRIFT", key: "drift" },
  { header: "COMMITTED(UTC)", key: "committed" },
];

const METRIC_COLUMNS: Column[] = [
  { header: "CPU", key: "cpu" },
  { header: "MEM", key: "memory" },
  { header: "NET", key: "network" },
  { header: "REQ", key: "requests", optional: true },
  { header: "INST", key: "instances" },
];

const TAIL_COLUMNS: Column[] = [
  { header: "PROJECT", key: "project" },
  { header: "ENV", key: "environment" },
];

/**
 * The numbers behind the formatted cells. Sorting uses these so `1174.4MB/h`
 * outranks `0.9MB/h` and `-102` outranks `-2`, which string cells would not.
 */
type Readings = {
  /** Share of the CPU limit, or absolute cores when no limit is reported. */
  cpu?: number;
  /** Share of the memory limit, or absolute bytes when no limit is reported. */
  memory?: number;
  network?: number;
  requests?: number;
  instances?: number;
  behind?: number;
  committedAt?: number;
};

type ReadingsByService = Map<string, Readings>;

type SortSpec = {
  key: string;
  /** Which optional column set the key needs; those are switched on for it. */
  needs?: "commits" | "metrics";
  aliases?: string[];
  value: (row: Row, readings: Readings | undefined) => number | string | undefined;
};

const SORT_SPECS: SortSpec[] = [
  { key: "name", value: (row) => row.name },
  { key: "id", value: (row) => row.id },
  { key: "type", value: (row) => row.type },
  { key: "plan", value: (row) => row.plan },
  { key: "region", value: (row) => row.region },
  { key: "status", value: (row) => row.status },
  { key: "project", value: (row) => row.project },
  { key: "env", aliases: ["environment"], value: (row) => row.environment },
  { key: "drift", needs: "commits", value: (_row, readings) => readings?.behind },
  {
    key: "committed",
    aliases: ["commit", "committed(utc)"],
    needs: "commits",
    value: (_row, readings) => readings?.committedAt,
  },
  { key: "cpu", needs: "metrics", value: (_row, readings) => readings?.cpu },
  {
    key: "mem",
    aliases: ["memory"],
    needs: "metrics",
    value: (_row, readings) => readings?.memory,
  },
  {
    key: "net",
    aliases: ["network"],
    needs: "metrics",
    value: (_row, readings) => readings?.network,
  },
  {
    key: "req",
    aliases: ["requests"],
    needs: "metrics",
    value: (_row, readings) => readings?.requests,
  },
  {
    key: "inst",
    aliases: ["instances"],
    needs: "metrics",
    value: (_row, readings) => readings?.instances,
  },
];

const SORT_SPECS_BY_KEY = new Map<string, SortSpec>(
  SORT_SPECS.flatMap((spec) =>
    [spec.key, ...(spec.aliases ?? [])].map((key) => [key, spec] as const),
  ),
);

type SortKey = { spec: SortSpec; descending: boolean };

/** Accepts `cpu`, `-cpu` for descending, and `-cpu,name` to break ties. */
function parseSortKeys(value: string): SortKey[] {
  return value
    .split(",")
    .map((entry) => entry.trim().toLowerCase())
    .filter((entry) => entry.length > 0)
    .map((entry) => {
      const descending = entry.startsWith("-");
      const name = descending ? entry.slice(1) : entry;
      const spec = SORT_SPECS_BY_KEY.get(name);

      if (!spec) {
        throw new Error(
          `--sort does not know the column: ${name}\n` +
            `  Sortable columns: ${SORT_SPECS.map((each) => each.key).join(", ")}`,
        );
      }

      return { spec, descending };
    });
}

function compareReadings(
  left: number | string,
  right: number | string,
): number {
  if (typeof left === "number" && typeof right === "number") {
    return left - right;
  }
  return String(left).localeCompare(String(right));
}

/**
 * Rows with no reading sink to the bottom in both directions: a suspended
 * service has no CPU number, which is not the same as the lowest one.
 */
function compareBySortKeys(
  keys: SortKey[],
  readings: ReadingsByService,
): (left: Row, right: Row) => number {
  return (left, right) => {
    for (const { spec, descending } of keys) {
      const leftValue = spec.value(left, readings.get(left.id));
      const rightValue = spec.value(right, readings.get(right.id));

      if (leftValue === undefined || rightValue === undefined) {
        if (leftValue === rightValue) {
          continue;
        }
        return leftValue === undefined ? 1 : -1;
      }

      const order = compareReadings(leftValue, rightValue);
      if (order !== 0) {
        return descending ? -order : order;
      }
    }
    return 0;
  };
}

function columnsFor(options: Options, rows: Row[]): Column[] {
  return [
    ...BASE_COLUMNS,
    ...(options.showCommits ? COMMIT_COLUMNS : []),
    ...(options.showMetrics ? METRIC_COLUMNS : []),
    ...TAIL_COLUMNS,
  ].filter(
    (column) =>
      !column.optional ||
      rows.some((row) => {
        const cell = row[column.key];
        return cell !== undefined && cell !== "" && cell !== EMPTY_CELL;
      }),
  );
}

function renderTable(rows: Row[], columns: Column[]): string {
  const body = rows.map((row) => columns.map((column) => row[column.key] ?? ""));

  const widths = columns.map((column, index) =>
    body.reduce(
      (width, cells) => Math.max(width, cells[index].length),
      column.header.length,
    ),
  );

  const line = (cells: string[]) =>
    cells
      .map((cell, index) =>
        index === cells.length - 1 ? cell : cell.padEnd(widths[index]),
      )
      .join("  ")
      .trimEnd();

  return [
    line(columns.map((column) => column.header)),
    line(widths.map((width) => "-".repeat(width))),
    ...body.map(line),
  ].join("\n");
}

const SHORT_COMMIT_LENGTH = 12;

const HELP_INDENT = " ".repeat(20);
const HELP_WIDTH = 58;

/** Keeps the generated column list inside the help text's second column. */
function wrapList(items: string[], indent: string, width: number): string {
  const lines: string[] = [];
  let current = "";

  for (const item of items) {
    const candidate = current ? `${current}, ${item}` : item;
    if (current && candidate.length > width) {
      lines.push(`${current},`);
      current = item;
    } else {
      current = candidate;
    }
  }

  if (current) {
    lines.push(current);
  }
  return lines.join(`\n${indent}`);
}

/**
 * Fills in the commit each service is actually running, and how far that commit
 * has fallen behind the branch deploys are built from.
 */
async function addCommits(
  rows: Row[],
  refreshBaseline: boolean,
  readings: ReadingsByService,
): Promise<{ commit: string; committedAt?: Date }> {
  const baseline = resolveBaseline(refreshBaseline);
  const deploys = await fetchLiveDeploys(rows.map((row) => row.id));

  const commits = new Map<string, string>();
  for (const row of rows) {
    const deploy = deploys.get(row.id);
    const commit = deploy ? deployedCommit(deploy) : undefined;
    if (commit) {
      commits.set(row.id, commit);
    }
  }

  const info = loadCommitInfo(commits.values(), baseline.commit);

  for (const row of rows) {
    const commit = commits.get(row.id);
    if (!commit) {
      row.commit = EMPTY_CELL;
      row.drift = EMPTY_CELL;
      row.committed = EMPTY_CELL;
      continue;
    }

    const commitInfo = info.get(commit);
    row.commit = commit.slice(0, SHORT_COMMIT_LENGTH);
    row.drift = formatDrift(commitInfo);
    row.committed = formatUtc(commitInfo?.committedAt);

    readings.set(row.id, {
      ...readings.get(row.id),
      behind: commitInfo?.behind,
      committedAt: commitInfo?.committedAt?.getTime(),
    });
  }

  return baseline;
}

/** The share of a limit when Render reports one, else the raw reading. */
function shareOfLimit(
  used: number | undefined,
  limit: number | undefined,
): number | undefined {
  if (used === undefined) {
    return undefined;
  }
  return limit ? used / limit : used;
}

async function addMetrics(
  rows: Row[],
  windowMs: number,
  readings: ReadingsByService,
): Promise<void> {
  const metrics = await collectMetrics(
    rows.map((row) => row.id),
    windowMs,
  );

  for (const row of rows) {
    const reading = metrics.get(row.id) ?? {};
    row.cpu = formatAgainstLimit(reading.cpu, reading.cpuLimit, formatCores);
    row.memory = formatAgainstLimit(
      reading.memory,
      reading.memoryLimit,
      formatBytes,
    );
    row.network =
      reading.bandwidth === undefined
        ? EMPTY_CELL
        : `${reading.bandwidth.toFixed(1)}MB/h`;
    row.requests =
      reading.requests === undefined
        ? EMPTY_CELL
        : `${Math.round(reading.requests)}/h`;
    row.instances =
      reading.instances === undefined
        ? EMPTY_CELL
        : String(Math.round(reading.instances));

    readings.set(row.id, {
      ...readings.get(row.id),
      // Sorted the way the cell reads: percent of limit first.
      cpu: shareOfLimit(reading.cpu, reading.cpuLimit),
      memory: shareOfLimit(reading.memory, reading.memoryLimit),
      network: reading.bandwidth,
      requests: reading.requests,
      instances: reading.instances,
    });
  }
}

const USAGE = `Usage: bun run render:services [-- <options>]

List Render services for the active workspace as a table.

Options:
  --env <names>     Filter by environment name, comma-separated. Names are
                    matched case-insensitively; "none" selects services that
                    belong to no project.
  --commits         Add the commit each service is running (from its live
                    deploy), its DRIFT against ${BASELINE_REF} (\`-2\` is two
                    commits behind, \`+1 -2\` has also diverged, \`?\` is not in
                    this checkout), and the commit's UTC timestamp. The
                    ${BASELINE_REF} HEAD is printed once in the footer.
  --no-fetch        With --commits, skip \`git fetch origin main\` and compare
                    against ${BASELINE_REF} as this checkout last saw it.
  --metrics         Add CPU, memory, network, request and instance columns.
                    CPU and memory show the share of the instance's limit plus
                    the reading behind it; network and requests are per hour.
  --metrics-window <duration>
                    Averaging window for --metrics: 15m, 2h, 1d. Bare numbers
                    are minutes. Default 1h. Network and request rates always
                    use whole hourly buckets, so they cover at least 3h.
  --sort <columns>  Sort by column, comma-separated for tiebreakers. Prefix a
                    column with "-" to sort it descending. Sorting a --commits
                    or --metrics column switches that set of columns on.
                    Columns sort by the number behind the cell, and rows with
                    no reading sink to the bottom either way.
                    Sortable: ${wrapList(
                      SORT_SPECS.map((spec) => spec.key),
                      HELP_INDENT,
                      HELP_WIDTH - "Sortable: ".length,
                    )}
  -h, --help        Show this help.

Any other option is passed through to \`render services\`, including
--include-previews and -e <environment-id>.

Examples:
  bun run render:services
  bun run render:services -- --env production
  bun run render:services -- --env staging,test
  bun run render:services -- --include-previews
  bun run render:services -- --env production --commits
  bun run render:services -- --env production --metrics --metrics-window 24h
  bun run render:services -- --env production --sort -mem
  bun run render:services -- --env production --sort -drift,name

Requires the render CLI, authorized via \`render login\`. --commits and
--metrics additionally call the Render REST API, reusing that CLI session
unless RENDER_API_KEY is set.`;

async function main(): Promise<void> {
  const argv = Bun.argv.slice(2);

  if (argv.includes("--help") || argv.includes("-h")) {
    console.log(USAGE);
    return;
  }

  const options = parseArgs(argv);
  const { envFilter } = options;

  verifyCliDependencies([RENDER_CLI]);

  const entries = runRenderServices(options.passthrough);
  const rows = entries
    .map(toRow)
    .filter((row): row is Row => row !== undefined)
    .filter((row) => {
      if (!envFilter) {
        return true;
      }
      // Services outside any project have no environment; `none` selects them.
      const name = row.environment === "" ? "none" : row.environment.toLowerCase();
      return envFilter.has(name);
    })
    .sort(
      (a, b) =>
        a.project.localeCompare(b.project) ||
        a.environment.localeCompare(b.environment) ||
        a.name.localeCompare(b.name),
    );

  if (rows.length === 0) {
    console.log(
      envFilter
        ? `No Render services matched environment: ${[...envFilter].join(", ")}`
        : "No Render services found for the active workspace.",
    );
    return;
  }

  // Only the rows that survived the filter are enriched, so a narrow --env
  // keeps the API work proportional to what is printed.
  const readings: ReadingsByService = new Map();
  const baseline = options.showCommits
    ? await addCommits(rows, options.refreshBaseline, readings)
    : undefined;

  if (options.showMetrics) {
    await addMetrics(rows, options.metricsWindowMs, readings);
  }

  if (options.sortKeys.length > 0) {
    // Array.sort is stable, so the project/environment/name order above stays
    // the tiebreaker for rows the requested keys cannot separate.
    rows.sort(compareBySortKeys(options.sortKeys, readings));
  }

  console.log(renderTable(rows, columnsFor(options, rows)));
  console.log(`\n${rows.length} service${rows.length === 1 ? "" : "s"}`);

  if (baseline) {
    console.log(
      `${BASELINE_REF} HEAD: ${baseline.commit.slice(0, SHORT_COMMIT_LENGTH)} ` +
        `(${formatUtc(baseline.committedAt)} UTC)`,
    );
  }

  if (options.showMetrics) {
    console.log(
      `metrics: CPU/memory/instances averaged over the last ` +
        `${formatWindow(options.metricsWindowMs)}; network and requests over ` +
        "whole hours",
    );
  }
}

// Guarded so the exported helpers can be imported without printing a table.
if (import.meta.main) {
  try {
    await main();
  } catch (error) {
    console.error(error instanceof Error ? error.message : error);
    process.exit(1);
  }
}
