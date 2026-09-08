import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
} from "node:fs";
import { basename, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
export const COMPILER_TARGET_DIR_NAME = "target/backyard-voltr-compilers";

export type CompilerProvenance = Readonly<{
  compilerBinarySha256: string;
  compilerBinaryPath: string;
  compilerTargetDir: string;
  compilerSourceTreeSha256: string;
}>;

export type CompilerBuildPlan = Readonly<{
  compilerBinary: string;
  compilerTargetDir: string;
  compilerBinaryPath: string;
  buildArgs: readonly string[];
}>;

export type RustCompilerRun<T> = Readonly<{
  output: T;
  compiler: CompilerProvenance;
}>;

type SourceTreeEntry = Readonly<{
  relativePath: string;
  sha256: string;
}>;

function sha256(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

function sourceFiles(root: string): string[] {
  const files: string[] = [resolve(root, "crates/loyal-actions/Cargo.toml")];
  const sourceRoot = resolve(root, "crates/loyal-actions/src");
  const visit = (directory: string) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = join(directory, entry.name);
      if (entry.isDirectory()) {
        visit(path);
      } else if (entry.isFile()) {
        files.push(path);
      }
    }
  };
  visit(sourceRoot);
  return files;
}

export function compilerSourceTreeEntries(repositoryRoot: string = REPOSITORY_ROOT): readonly SourceTreeEntry[] {
  const root = resolve(repositoryRoot);
  return sourceFiles(root)
    .map((path) => ({
      relativePath: relative(root, path).split(sep).join("/"),
      sha256: sha256(readFileSync(path)),
    }))
    .sort((left, right) => left.relativePath.localeCompare(right.relativePath));
}

export function compilerSourceTreeSha256(repositoryRoot: string = REPOSITORY_ROOT): string {
  return sha256(Buffer.from(JSON.stringify(compilerSourceTreeEntries(repositoryRoot))));
}

export function compilerBuildPlan(
  compilerBinary: string,
  repositoryRoot: string = REPOSITORY_ROOT,
): CompilerBuildPlan {
  if (compilerBinary !== basename(compilerBinary) || compilerBinary.length === 0) {
    throw new Error(`invalid loyal-actions compiler binary: ${compilerBinary}`);
  }
  const root = resolve(repositoryRoot);
  const compilerTargetDir = resolve(root, COMPILER_TARGET_DIR_NAME);
  const compilerBinaryPath = resolve(
    compilerTargetDir,
    "debug",
    process.platform === "win32" ? `${compilerBinary}.exe` : compilerBinary,
  );
  return {
    compilerBinary,
    compilerTargetDir,
    compilerBinaryPath,
    buildArgs: ["build", "--quiet", "-p", "loyal-actions", "--bin", compilerBinary],
  };
}

export function runRustCompiler<T>(
  input: Readonly<{
    compilerBinary: string;
    args?: readonly string[];
    input?: string | Uint8Array;
    cwd?: string;
    maxBuffer?: number;
    label: string;
  }>,
): RustCompilerRun<T> {
  const repositoryRoot = resolve(input.cwd ?? REPOSITORY_ROOT);
  const plan = compilerBuildPlan(input.compilerBinary, repositoryRoot);
  mkdirSync(plan.compilerTargetDir, { recursive: true });

  // Deliberately override inherited CARGO_TARGET_DIR: several worktrees share
  // .phase3-recovery/target, so Cargo could otherwise reuse another checkout's
  // loyal-actions binary while deciding this checkout is already fresh.
  const build = spawnSync("cargo", [...plan.buildArgs], {
    cwd: repositoryRoot,
    encoding: "utf8",
    maxBuffer: input.maxBuffer ?? 16 * 1024 * 1024,
    env: { ...process.env, CARGO_TARGET_DIR: plan.compilerTargetDir },
  });
  if (build.error) throw build.error;
  if (build.status !== 0) {
    const detail = build.stderr.trim() || build.stdout.trim() || `exit ${build.status}`;
    throw new Error(`${input.label} build failed: ${detail}`);
  }
  if (!existsSync(plan.compilerBinaryPath)) {
    throw new Error(`${input.label} did not produce ${plan.compilerBinaryPath}`);
  }

  const compiler = {
    compilerBinarySha256: sha256(readFileSync(plan.compilerBinaryPath)),
    compilerBinaryPath: plan.compilerBinaryPath,
    compilerTargetDir: plan.compilerTargetDir,
    compilerSourceTreeSha256: compilerSourceTreeSha256(repositoryRoot),
  } satisfies CompilerProvenance;
  const run = spawnSync(plan.compilerBinaryPath, [...(input.args ?? [])], {
    cwd: repositoryRoot,
    encoding: "utf8",
    input: input.input,
    maxBuffer: input.maxBuffer ?? 16 * 1024 * 1024,
    env: process.env,
  });
  if (run.error) throw run.error;
  if (run.status !== 0) {
    const detail = run.stderr.trim() || run.stdout.trim() || `exit ${run.status}`;
    throw new Error(`${input.label} failed: ${detail}`);
  }
  let output: T;
  try {
    output = JSON.parse(run.stdout) as T;
  } catch {
    throw new Error(`${input.label} returned non-JSON output`);
  }
  return { output, compiler };
}
