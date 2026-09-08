import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmodSync,
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  mkdtempSync,
  mkdirSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
  fsyncSync,
  writeSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { basename, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
export const COMPILER_TARGET_DIR_NAME = "target/backyard-voltr-compilers";

export type CompilerProvenance = Readonly<{
  compilerBinarySha256: string;
  compilerBinarySha256AtExec: string;
  compilerExecMode: "fd" | "private-copy";
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

function assertCompilerDescriptor(path: string, fd: number): ReturnType<typeof fstatSync> {
  const stat = fstatSync(fd);
  if (!stat.isFile()) throw new Error(`compiler binary ${path} is not a regular file`);
  if (stat.uid !== currentUid()) throw new Error(`compiler binary ${path} is not owned by the current uid`);
  if ((stat.mode & 0o022) !== 0) throw new Error(`compiler binary ${path} is group/other-writable`);
  return stat;
}

function compilerBinarySha256FromOpenDescriptor(path: string, fd: number): string {
  const stat = assertCompilerDescriptor(path, fd);
  const size = Number(stat.size);
  const digest = createHash("sha256");
  const buffer = Buffer.alloc(64 * 1024);
  let offset = 0;
  while (offset < size) {
    const count = readSync(fd, buffer, 0, Math.min(buffer.length, size - offset), offset);
    if (count === 0) throw new Error(`compiler binary ${path} changed while hashing`);
    digest.update(buffer.subarray(0, count));
    offset += count;
  }
  const after = assertCompilerDescriptor(path, fd);
  if (after.ino !== stat.ino || Number(after.size) !== size) {
    throw new Error(`compiler binary ${path} changed while hashing`);
  }
  return digest.digest("hex");
}

function compilerBinaryBytesFromOpenDescriptor(path: string, fd: number): Buffer {
  const stat = assertCompilerDescriptor(path, fd);
  const size = Number(stat.size);
  const bytes = Buffer.alloc(size);
  let offset = 0;
  while (offset < bytes.length) {
    const count = readSync(fd, bytes, offset, bytes.length - offset, offset);
    if (count === 0) throw new Error(`compiler binary ${path} changed while reading`);
    offset += count;
  }
  const after = assertCompilerDescriptor(path, fd);
  if (after.ino !== stat.ino || Number(after.size) !== size) {
    throw new Error(`compiler binary ${path} changed while reading`);
  }
  return bytes;
}

function compilerBinarySha256FromDescriptor(path: string): string {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    return compilerBinarySha256FromOpenDescriptor(path, fd);
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
  const compilerFd = openSync(compilerBinaryPath, constants.O_RDONLY | constants.O_NOFOLLOW);
  let compilerBinarySha256: string;
  let compilerBinarySha256AtExec: string;
  let compilerExecMode: CompilerProvenance["compilerExecMode"];
  let executedCompilerPath = compilerBinaryPath;
  let run: ReturnType<typeof spawnSync>;
  try {
    compilerBinarySha256 = compilerBinarySha256FromOpenDescriptor(compilerBinaryPath, compilerFd);
    const sourceTreeHash = compilerSourceTreeSha256(repositoryRoot);
    // Keep the verified descriptor open across the child execution. On this
    // macOS/Bun combination, direct `/dev/fd/3` execution returns EACCES, so
    // the private-copy fallback below is the supported mode here.
    const fdRun = spawnSync("/dev/fd/3", [...(input.args ?? [])], {
      cwd: repositoryRoot,
      encoding: "utf8",
      input: input.input,
      maxBuffer: input.maxBuffer ?? 16 * 1024 * 1024,
      env: childEnvironment,
      stdio: ["pipe", "pipe", "pipe", compilerFd],
    });
    if (fdRun.error && ["EACCES", "ENOENT", "ENOTSUP"].includes((fdRun.error as NodeJS.ErrnoException).code ?? "")) {
      const bytes = compilerBinaryBytesFromOpenDescriptor(compilerBinaryPath, compilerFd);
      const privateDirectory = mkdtempSync(join(tmpdir(), "loyal-voltr-compiler-"));
      chmodSync(privateDirectory, 0o700);
      const privatePath = join(privateDirectory, basename(compilerBinaryPath));
      const privateFd = openSync(
        privatePath,
        constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW,
        0o700,
      );
      try {
        let offset = 0;
        while (offset < bytes.length) {
          const written = writeSync(privateFd, bytes, offset, bytes.length - offset);
          if (written === 0) throw new Error("private compiler copy write made no progress");
          offset += written;
        }
        fsyncSync(privateFd);
      } finally {
        closeSync(privateFd);
      }
      const privateHash = compilerBinarySha256FromDescriptor(privatePath);
      if (privateHash !== compilerBinarySha256) {
        throw new Error(`COMPILER_BINARY_HASH_MISMATCH: private copy ${privatePath} differs from the verified descriptor`);
      }
      const privateInode = lstatSync(privatePath).ino;
      // The copy lives in a mode-0700 directory. The residual same-uid window
      // is rechecked by inode and hash immediately after the child exits.
      run = spawnSync(privatePath, [...(input.args ?? [])], {
        cwd: repositoryRoot,
        encoding: "utf8",
        input: input.input,
        maxBuffer: input.maxBuffer ?? 16 * 1024 * 1024,
        env: childEnvironment,
      });
      const afterInode = lstatSync(privatePath).ino;
      compilerBinarySha256AtExec = compilerBinarySha256FromDescriptor(privatePath);
      if (afterInode !== privateInode || compilerBinarySha256AtExec !== compilerBinarySha256) {
        throw new Error(`COMPILER_BINARY_HASH_MISMATCH: private compiler copy changed during execution`);
      }
      executedCompilerPath = privatePath;
      compilerExecMode = "private-copy";
    } else {
      run = fdRun;
      compilerBinarySha256AtExec = compilerBinarySha256FromOpenDescriptor(compilerBinaryPath, compilerFd);
      if (compilerBinarySha256AtExec !== compilerBinarySha256) {
        throw new Error(`COMPILER_BINARY_HASH_MISMATCH: verified compiler descriptor changed during execution`);
      }
      compilerExecMode = "fd";
    }
    if (run.error) throw run.error;
    if (run.status !== 0) {
      const detail = String(run.stderr ?? "").trim() || String(run.stdout ?? "").trim() || `exit ${run.status}`;
      throw new Error(`${input.label} failed: ${detail}`);
    }
    const compiler = {
      compilerBinarySha256,
      compilerBinarySha256AtExec,
      compilerExecMode,
      compilerBinaryPath: executedCompilerPath,
      compilerTargetDir: plan.compilerTargetDir,
      compilerSourceTreeSha256: sourceTreeHash,
    } satisfies CompilerProvenance;
    let output: T;
    try {
      output = JSON.parse(String(run.stdout ?? "")) as T;
    } catch {
      throw new Error(`${input.label} returned non-JSON output`);
    }
    return { output, compiler };
  } finally {
    closeSync(compilerFd);
  }
}
