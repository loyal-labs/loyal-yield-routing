#!/usr/bin/env bun
/**
 * Build worker images at the current commit, then redeploy the named Render
 * services onto that image, in the order given.
 *
 *   bun run render:services:redeploy loyal-kamino-reserve-monitor,loyal-fleet-route-executor
 *
 * Steps:
 *   1. Verify local HEAD matches origin/main (the workflow builds from main,
 *      so a mismatch would deploy something other than what is checked out).
 *   2. Resolve each service name to its Render service ID and its GHCR image
 *      repository, taken from render.yaml.
 *   3. Trigger `worker-images.yml` on main and wait for it to finish.
 *   4. Deploy each service onto `<repo>:sha-<HEAD>`, one at a time, stopping
 *      at the first failure.
 *
 * Flags:
 *   --dry-run      Run the preflight and print the plan; change nothing.
 *   --skip-build   Skip the workflow and deploy images already built for HEAD.
 *   --yes          Skip the confirmation prompt.
 *
 * Requires the `git`, `render`, and `gh` CLIs; all are checked, and render and
 * gh are checked for authorization, before anything else runs.
 */

import { existsSync, readFileSync } from "node:fs";

import {
  CliDependencyError,
  RENDER_CLI,
  resourceOf,
  runRenderServices,
  verifyCliDependencies,
  type CliDependency,
} from "./render-services-table.ts";

const WORKFLOW = "worker-images.yml";
const WORKFLOW_REF = "main";
const RENDER_BLUEPRINT = "render.yaml";
const RUN_DISCOVERY_TIMEOUT_MS = 120_000;
const RUN_DISCOVERY_INTERVAL_MS = 3_000;

type Plan = {
  serviceNames: string[];
  dryRun: boolean;
  skipBuild: boolean;
  assumeYes: boolean;
};

type Target = {
  name: string;
  serviceId: string;
  image: string;
};

class UserError extends Error {}

const GIT_CLI: CliDependency = {
  command: "git",
  installHint: "Install it with: xcode-select --install",
};

const GH_CLI: CliDependency = {
  command: "gh",
  installHint: "Install it with: brew install gh",
  authCheck: {
    args: ["auth", "status"],
    hint: "Authorize it with: gh auth login",
  },
};


function run(command: string[]): {
  stdout: string;
  stderr: string;
  exitCode: number;
} {
  const result = Bun.spawnSync(command, { stdout: "pipe", stderr: "pipe" });
  const decoder = new TextDecoder();
  const stdout = decoder.decode(result.stdout).trim();
  const stderr = decoder.decode(result.stderr).trim();

  if (result.exitCode !== 0) {
    throw new UserError(
      `Command failed (exit ${result.exitCode}): ${command.join(" ")}` +
        (stderr ? `\n${stderr}` : ""),
    );
  }

  return { stdout, stderr, exitCode: result.exitCode };
}

/** Streams child output to the terminal instead of capturing it. */
function runStreaming(command: string[]): number {
  console.log(`\n$ ${command.join(" ")}`);
  const result = Bun.spawnSync(command, {
    stdout: "inherit",
    stderr: "inherit",
    stdin: "inherit",
  });
  return result.exitCode;
}

function parseArgs(argv: string[]): Plan {
  const serviceNames: string[] = [];
  let dryRun = false;
  let skipBuild = false;
  let assumeYes = false;

  for (const arg of argv) {
    switch (arg) {
      case "--dry-run":
        dryRun = true;
        break;
      case "--skip-build":
        skipBuild = true;
        break;
      case "--yes":
      case "-y":
        assumeYes = true;
        break;
      default:
        if (arg.startsWith("-")) {
          throw new UserError(`Unknown flag: ${arg}`);
        }
        // Names may arrive as one comma-separated argument or several.
        serviceNames.push(
          ...arg
            .split(",")
            .map((name) => name.trim())
            .filter((name) => name.length > 0),
        );
    }
  }

  if (serviceNames.length === 0) {
    throw new UserError(
      "Pass at least one service name, e.g.\n" +
        "  bun run render:services:redeploy loyal-kamino-reserve-monitor,loyal-fleet-route-executor",
    );
  }

  const duplicates = serviceNames.filter(
    (name, index) => serviceNames.indexOf(name) !== index,
  );
  if (duplicates.length > 0) {
    throw new UserError(
      `Service listed more than once: ${[...new Set(duplicates)].join(", ")}`,
    );
  }

  return { serviceNames, dryRun, skipBuild, assumeYes };
}

/** Fails unless HEAD is exactly origin/main, so the built image matches HEAD. */
function verifyHeadMatchesOriginMain(): string {
  const head = run(["git", "rev-parse", "HEAD"]).stdout;

  run(["git", "fetch", "origin", "main", "--quiet"]);
  const originMain = run(["git", "rev-parse", "origin/main"]).stdout;

  if (head !== originMain) {
    const branch = run(["git", "rev-parse", "--abbrev-ref", "HEAD"]).stdout;
    throw new UserError(
      "Local HEAD does not match origin/main; refusing to deploy.\n" +
        `  HEAD (${branch}): ${head}\n` +
        `  origin/main:      ${originMain}\n` +
        `The ${WORKFLOW} workflow builds from ${WORKFLOW_REF}, so the deployed ` +
        "image would not match this checkout. Push or rebase first.",
    );
  }

  const dirty = run(["git", "status", "--porcelain"]).stdout;
  if (dirty) {
    console.warn(
      "Warning: working tree has uncommitted changes. They are NOT included " +
        "in the built image, which is built from origin/main.",
    );
  }

  return head;
}

/**
 * Maps service name to GHCR image repository using render.yaml, which is the
 * source of truth for which Dockerfile a service is built from. The Render API
 * does not report the currently pinned image, so the blueprint is used instead.
 */
function loadImageRepositories(): Map<string, string> {
  if (!existsSync(RENDER_BLUEPRINT)) {
    throw new UserError(
      `${RENDER_BLUEPRINT} not found. Run this from the repository root.`,
    );
  }

  const blueprint = Bun.YAML.parse(
    readFileSync(RENDER_BLUEPRINT, "utf8"),
  ) as {
    projects?: Array<{
      environments?: Array<{
        services?: Array<{ name?: string; image?: { url?: string } }>;
      }>;
    }>;
  };

  const repositories = new Map<string, string>();

  for (const project of blueprint.projects ?? []) {
    for (const environment of project.environments ?? []) {
      for (const service of environment.services ?? []) {
        const url = service.image?.url;
        if (!service.name || !url) {
          continue;
        }
        // Strip the pinned `:sha-<commit>` tag; only the repository is reused.
        const separator = url.lastIndexOf(":");
        repositories.set(
          service.name,
          separator === -1 ? url : url.slice(0, separator),
        );
      }
    }
  }

  return repositories;
}

/** Reuses the listing helper behind `bun run render:services`. */
function loadServiceIds(): Map<string, string> {
  const ids = new Map<string, string>();

  for (const entry of runRenderServices()) {
    const resource = resourceOf(entry);
    if (resource?.name && resource.id) {
      ids.set(resource.name, resource.id);
    }
  }

  return ids;
}

function resolveTargets(serviceNames: string[], commit: string): Target[] {
  const serviceIds = loadServiceIds();
  const repositories = loadImageRepositories();

  const unknown = serviceNames.filter((name) => !serviceIds.has(name));
  if (unknown.length > 0) {
    throw new UserError(
      `Unknown Render service(s): ${unknown.join(", ")}\n` +
        `Known services:\n  ${[...serviceIds.keys()].sort().join("\n  ")}`,
    );
  }

  const notImageBacked = serviceNames.filter((name) => !repositories.has(name));
  if (notImageBacked.length > 0) {
    throw new UserError(
      `No image URL in ${RENDER_BLUEPRINT} for: ${notImageBacked.join(", ")}\n` +
        "Only prebuilt-image services can be redeployed by this script.",
    );
  }

  return serviceNames.map((name) => ({
    name,
    serviceId: serviceIds.get(name)!,
    image: `${repositories.get(name)}:sha-${commit}`,
  }));
}

type WorkflowRun = {
  databaseId: number;
  status: string;
  conclusion: string;
};

function listWorkflowRuns(commit: string): WorkflowRun[] {
  const { stdout } = run([
    "gh",
    "run",
    "list",
    "--workflow",
    WORKFLOW,
    "--commit",
    commit,
    "--json",
    "databaseId,status,conclusion",
  ]);
  return JSON.parse(stdout || "[]") as WorkflowRun[];
}

function listWorkflowRunIds(commit: string): number[] {
  return listWorkflowRuns(commit).map((entry) => entry.databaseId);
}

async function sleep(ms: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Triggers the workflow and returns the ID of the run it created. Runs that
 * already existed for this commit are ignored so a re-run is not mistaken for
 * the new one.
 */
async function triggerWorkflow(commit: string): Promise<number> {
  const before = new Set(listWorkflowRunIds(commit));

  run(["gh", "workflow", "run", WORKFLOW, "--ref", WORKFLOW_REF]);
  console.log(`Triggered ${WORKFLOW} on ${WORKFLOW_REF} (${commit.slice(0, 12)})`);

  const deadline = Date.now() + RUN_DISCOVERY_TIMEOUT_MS;
  while (Date.now() < deadline) {
    await sleep(RUN_DISCOVERY_INTERVAL_MS);
    const created = listWorkflowRunIds(commit).filter((id) => !before.has(id));
    if (created.length > 0) {
      // Newest first from gh; with several, the highest ID is the new run.
      return Math.max(...created);
    }
    console.log("Waiting for the workflow run to appear...");
  }

  throw new UserError(
    `Timed out after ${RUN_DISCOVERY_TIMEOUT_MS / 1000}s waiting for a ` +
      `${WORKFLOW} run for ${commit}. Check: gh run list --workflow ${WORKFLOW}`,
  );
}

function watchWorkflowRun(runId: number): void {
  const exitCode = runStreaming([
    "gh",
    "run",
    "watch",
    String(runId),
    "--exit-status",
  ]);

  if (exitCode !== 0) {
    throw new UserError(
      `Image build run ${runId} did not succeed; no services were deployed.\n` +
        `Logs: gh run view ${runId} --log-failed`,
    );
  }
  console.log(`\nImage build run ${runId} succeeded.`);
}

/**
 * Confirms images for this commit already exist, by checking that the build
 * workflow succeeded for it. The registry itself is not queried: reading GHCR
 * needs either a container daemon or a `read:packages` token, and the workflow
 * is the only thing that publishes these tags anyway.
 */
function verifyImagesAlreadyBuilt(commit: string): void {
  const succeeded = listWorkflowRuns(commit).filter(
    (entry) => entry.status === "completed" && entry.conclusion === "success",
  );

  if (succeeded.length === 0) {
    throw new UserError(
      `No successful ${WORKFLOW} run found for ${commit.slice(0, 12)}, ` +
        "so its images were probably never published.\n" +
        "Drop --skip-build to build them now, or check: " +
        `gh run list --workflow ${WORKFLOW} --commit ${commit}`,
    );
  }

  console.log(
    `Found ${succeeded.length} successful ${WORKFLOW} run(s) for ` +
      `${commit.slice(0, 12)} (latest: ${succeeded[0].databaseId}).`,
  );
}

async function confirm(targets: Target[]): Promise<boolean> {
  if (!process.stdin.isTTY) {
    throw new UserError(
      "Refusing to deploy without a TTY to confirm on; pass --yes.",
    );
  }

  process.stdout.write(
    `\nDeploy ${targets.length} service(s) in this order? [y/N] `,
  );
  for await (const line of console) {
    return line.trim().toLowerCase() === "y";
  }
  return false;
}

function deploy(target: Target, position: number, total: number): void {
  console.log(`\n[${position}/${total}] ${target.name} (${target.serviceId})`);

  const exitCode = runStreaming([
    "render",
    "deploys",
    "create",
    target.serviceId,
    "--image",
    target.image,
    "--wait",
    "--confirm",
    "--output",
    "text",
  ]);

  if (exitCode !== 0) {
    throw new UserError(
      `Deploy failed for ${target.name}; stopping. ` +
        `${total - position} service(s) were not deployed.`,
    );
  }
}

function printPlan(targets: Target[], commit: string, plan: Plan): void {
  console.log(`Commit:  ${commit}`);
  console.log(`Build:   ${plan.skipBuild ? "skipped (--skip-build)" : WORKFLOW}`);
  console.log("Deploy order:");
  targets.forEach((target, index) => {
    console.log(`  ${index + 1}. ${target.name}  ${target.serviceId}`);
    console.log(`     ${target.image}`);
  });
}

const USAGE = `Usage: bun run render:services:redeploy <service>[,<service>...] [options]

Build worker images at the current commit, then redeploy the named Render
services onto that image, in the order given.

Steps:
  1. Verify local HEAD matches origin/main.
  2. Resolve each name to its service ID and its GHCR image repository,
     the latter from ${RENDER_BLUEPRINT}.
  3. Trigger ${WORKFLOW} on ${WORKFLOW_REF} and wait for it.
  4. Deploy each service onto <repo>:sha-<HEAD>, one at a time, stopping at
     the first failure.

Options:
  --dry-run         Run the preflight and print the plan; change nothing.
  --skip-build      Skip the workflow and deploy images already built for
                    HEAD, verified via a successful ${WORKFLOW} run.
  -y, --yes         Skip the confirmation prompt. Required without a TTY.
  -h, --help        Show this help.

Examples:
  bun run render:services:redeploy loyal-kamino-reserve-monitor
  bun run render:services:redeploy loyal-fleet-route-executor,loyal-fleet-route-confirmer
  bun run render:services:redeploy loyal-balance-sweep-ata-projector --dry-run

Requires the git, render, and gh CLIs; render and gh must be authorized.
Run \`bun run render:services\` to list deployable service names.`;

async function main(): Promise<void> {
  const argv = Bun.argv.slice(2);

  if (argv.includes("--help") || argv.includes("-h")) {
    console.log(USAGE);
    return;
  }

  const plan = parseArgs(argv);

  // gh both builds images and confirms --skip-build has some; only a plain
  // dry run reaches neither.
  const needsGh = !plan.dryRun || plan.skipBuild;
  verifyCliDependencies([GIT_CLI, RENDER_CLI, ...(needsGh ? [GH_CLI] : [])]);

  const commit = verifyHeadMatchesOriginMain();
  const targets = resolveTargets(plan.serviceNames, commit);

  printPlan(targets, commit, plan);

  // Read-only, so it runs before the prompt: no point confirming a deploy
  // whose images were never published.
  if (plan.skipBuild) {
    console.log("\nSkipping image build; checking images exist for HEAD...");
    verifyImagesAlreadyBuilt(commit);
  }

  if (plan.dryRun) {
    console.log("\nDry run: nothing was triggered or deployed.");
    return;
  }

  if (!plan.assumeYes && !(await confirm(targets))) {
    console.log("Aborted.");
    process.exitCode = 1;
    return;
  }

  if (!plan.skipBuild) {
    watchWorkflowRun(await triggerWorkflow(commit));
  }

  targets.forEach((target, index) => {
    deploy(target, index + 1, targets.length);
  });

  console.log(`\nDeployed ${targets.length} service(s) at ${commit.slice(0, 12)}.`);
}

try {
  await main();
} catch (error) {
  if (error instanceof UserError || error instanceof CliDependencyError) {
    console.error(`\n${error.message}`);
    process.exit(1);
  }
  throw error;
}
