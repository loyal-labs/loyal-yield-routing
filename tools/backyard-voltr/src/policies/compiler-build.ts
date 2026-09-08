import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  closeSync,
  constants,
  fstatSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
} from "node:fs";
import { basename, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
export const COMPILER_TARGET_DIR_NAME = "target/backyard-voltr-compilers";

export type CompilerProvenance = Readonly<{
  compilerBinarySha256: string;
  compilerBinarySha256AtExec: string;
  compilerBinaryPath: string;
  compilerTargetDir: string;
  compilerSourceTreeSha256: string;
}>;

export type CompilerBuildPlan = Readonly<{
  compilerBinary: string;
  compilerTargetDir: string;
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
  return {
    compilerBinary,
    compilerTargetDir,
    buildArgs: [
      "build",
      "-p",
      "loyal-actions",
      "--bin",
      compilerBinary,
      "--message-format=json-render-diagnostics",
    ],
  };
}

const SCRUBBED_CARGO_ENV_KEYS = [
  "CARGO_BUILD_TARGET",
  "CARGO_BUILD_TARGET_DIR",
  "CARGO_TARGET_DIR",
  "CARGO_BUILD_RUSTFLAGS",
  "RUSTFLAGS",
  "RUSTC_WRAPPER",
  "RUSTC_WORKSPACE_WRAPPER",
] as const;

export function compilerSpawnEnvironment(
  environment: NodeJS.ProcessEnv,
  compilerTargetDir: string,
): NodeJS.ProcessEnv {
  const childEnvironment = { ...environment };
  for (const key of SCRUBBED_CARGO_ENV_KEYS) delete childEnvironment[key];
  childEnvironment.CARGO_TARGET_DIR = compilerTargetDir;
  return childEnvironment;
}

function compilerArtifactExecutable(
  stdout: string,
  compilerBinary: string,
  repositoryRoot: string,
  compilerTargetDir: string,
): string {
  for (const line of stdout.split(/\r?\n/)) {
    if (line.trim().length === 0) continue;
    let message: unknown;
    try {
      message = JSON.parse(line);
    } catch {
      continue;
    }
    if (!message || typeof message !== "object") continue;
    const record = message as Record<string, unknown>;
    const target = record.target && typeof record.target === "object"
      ? record.target as Record<string, unknown>
      : null;
    if (record.reason !== "compiler-artifact"
      || target?.name !== compilerBinary
      || typeof record.executable !== "string") {
      continue;
    }
    const executable = resolve(repositoryRoot, record.executable);
    const targetPrefix = `${compilerTargetDir}${sep}`;
    if (!executable.startsWith(targetPrefix)) {
      throw new Error(`compiler artifact escaped the per-checkout target directory: ${executable}`);
    }
    return executable;
  }
  throw new Error(`cargo did not report an executable compiler-artifact for ${compilerBinary}`);
}

function currentUid(): number {
  const uid = typeof process.getuid === "function" ? process.getuid() : undefined;
  if (uid === undefined) throw new Error("cannot verify compiler binary ownership without a uid");
  return uid;
}

function compilerBinarySha256FromDescriptor(path: string): string {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const stat = fstatSync(fd);
    if (!stat.isFile()) throw new Error(`compiler binary ${path} is not a regular file`);
    if (stat.uid !== currentUid()) throw new Error(`compiler binary ${path} is not owned by the current uid`);
    if ((stat.mode & 0o022) !== 0) throw new Error(`compiler binary ${path} is group/other-writable`);
    const digest = createHash("sha256");
    const buffer = Buffer.alloc(64 * 1024);
    let offset = 0;
    while (offset < stat.size) {
      const count = readSync(fd, buffer, 0, Math.min(buffer.length, stat.size - offset), offset);
      if (count === 0) throw new Error(`compiler binary ${path} changed while hashing`);
      digest.update(buffer.subarray(0, count));
      offset += count;
    }
    const after = fstatSync(fd);
    if (after.size !== stat.size) throw new Error(`compiler binary ${path} changed while hashing`);
    return digest.digest("hex");
  } finally {
    closeSync(fd);
  }
}

export function assertCompilerBinaryAtExec(path: string, expectedSha256: string): string {
  const actualSha256 = compilerBinarySha256FromDescriptor(path);
  if (actualSha256 !== expectedSha256) {
    throw new Error(
      `COMPILER_BINARY_HASH_MISMATCH: expected ${expectedSha256}, found ${actualSha256} before execution`,
    );
  }
  return actualSha256;
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
  const childEnvironment = compilerSpawnEnvironment(process.env, plan.compilerTargetDir);

  // Deliberately scrub inherited Cargo/Rust target and wrapper settings:
  // several worktrees share .phase3-recovery/target, so Cargo must build and
  // execute only from this checkout's compiler target directory.
  const build = spawnSync("cargo", [...plan.buildArgs], {
    cwd: repositoryRoot,
    encoding: "utf8",
    maxBuffer: input.maxBuffer ?? 16 * 1024 * 1024,
    env: childEnvironment,
  });
  if (build.error) throw build.error;
  if (build.status !== 0) {
    const detail = build.stderr.trim() || build.stdout.trim() || `exit ${build.status}`;
    throw new Error(`${input.label} build failed: ${detail}`);
  }
  const compilerBinaryPath = compilerArtifactExecutable(
    build.stdout,
    input.compilerBinary,
    repositoryRoot,
    plan.compilerTargetDir,
  );
  const compilerBinarySha256 = compilerBinarySha256FromDescriptor(compilerBinaryPath);

  const compiler = {
    compilerBinarySha256,
    compilerBinarySha256AtExec: assertCompilerBinaryAtExec(compilerBinaryPath, compilerBinarySha256),
    compilerBinaryPath,
    compilerTargetDir: plan.compilerTargetDir,
    compilerSourceTreeSha256: compilerSourceTreeSha256(repositoryRoot),
  } satisfies CompilerProvenance;
  const run = spawnSync(compilerBinaryPath, [...(input.args ?? [])], {
    cwd: repositoryRoot,
    encoding: "utf8",
    input: input.input,
    maxBuffer: input.maxBuffer ?? 16 * 1024 * 1024,
    env: childEnvironment,
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
