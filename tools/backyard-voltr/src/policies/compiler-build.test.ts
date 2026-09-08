import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

import { compileCustomPolicyArtifact } from "./rwa-multiply-custom.js";
import {
  assertCompilerBinaryAtExec,
  compilerBuildPlan,
  compilerSpawnEnvironment,
  REPOSITORY_ROOT,
} from "./compiler-build.js";

test("compiler helper ignores shared CARGO_TARGET_DIR", () => {
  const previous = process.env.CARGO_TARGET_DIR;
  process.env.CARGO_TARGET_DIR = "/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target";
  try {
    const plan = compilerBuildPlan("compile-voltr-custom-policy");
    assert.equal(
      plan.compilerTargetDir,
      `${REPOSITORY_ROOT}/target/backyard-voltr-compilers`,
    );
    assert.notEqual(plan.compilerTargetDir, process.env.CARGO_TARGET_DIR);
    assert.deepEqual(plan.buildArgs, [
      "build",
      "-p",
      "loyal-actions",
      "--bin",
      "compile-voltr-custom-policy",
      "--message-format=json-render-diagnostics",
    ]);
  } finally {
    if (previous === undefined) {
      delete process.env.CARGO_TARGET_DIR;
    } else {
      process.env.CARGO_TARGET_DIR = previous;
    }
  }
});

test("compiler provenance is present and stable across consecutive compiles", async () => {
  const previous = process.env.CARGO_TARGET_DIR;
  process.env.CARGO_TARGET_DIR = "/Users/user/loyal/loyal-yield-routing/.phase3-recovery/target";
  try {
    const first = await compileCustomPolicyArtifact(61n);
    const second = await compileCustomPolicyArtifact(61n);
    for (const artifact of [first, second]) {
      assert.match(artifact.compiler.compilerBinarySha256, /^[0-9a-f]{64}$/);
      assert.match(artifact.compiler.compilerBinarySha256AtExec, /^[0-9a-f]{64}$/);
      assert.equal(
        artifact.compiler.compilerBinarySha256AtExec,
        artifact.compiler.compilerBinarySha256,
      );
      assert.match(artifact.compiler.compilerSourceTreeSha256, /^[0-9a-f]{64}$/);
      assert.ok(artifact.compiler.compilerBinaryPath.startsWith(
        `${REPOSITORY_ROOT}/target/backyard-voltr-compilers/`,
      ));
      assert.equal(
        artifact.compiler.compilerTargetDir,
        `${REPOSITORY_ROOT}/target/backyard-voltr-compilers`,
      );
    }
    assert.deepEqual(first.compiler, second.compiler);
  } finally {
    if (previous === undefined) {
      delete process.env.CARGO_TARGET_DIR;
    } else {
      process.env.CARGO_TARGET_DIR = previous;
    }
  }
});

test("compiler spawn environment scrubs inherited target and Rust wrapper settings", () => {
  const environment = compilerSpawnEnvironment({
    ...process.env,
    CARGO_BUILD_TARGET: "x86_64-unknown-linux-gnu",
    CARGO_BUILD_TARGET_DIR: "/shared/target",
    CARGO_TARGET_DIR: "/shared/target",
    CARGO_BUILD_RUSTFLAGS: "--cfg inherited",
    RUSTFLAGS: "--cfg inherited",
    RUSTC_WRAPPER: "/tmp/wrapper",
    RUSTC_WORKSPACE_WRAPPER: "/tmp/workspace-wrapper",
  }, "/tmp/hxtk-checkout-target");
  for (const key of [
    "CARGO_BUILD_TARGET",
    "CARGO_BUILD_TARGET_DIR",
    "CARGO_TARGET_DIR",
    "CARGO_BUILD_RUSTFLAGS",
    "RUSTFLAGS",
    "RUSTC_WRAPPER",
    "RUSTC_WORKSPACE_WRAPPER",
  ]) assert.equal(environment[key], key === "CARGO_TARGET_DIR" ? "/tmp/hxtk-checkout-target" : undefined);
});

test("exec-time compiler hash mismatch refuses a tampered binary", () => {
  const root = mkdtempSync(join("/tmp", "compiler-build-"));
  try {
    const binary = join(root, "fake-compiler");
    writeFileSync(binary, "built", { mode: 0o700 });
    const builtHash = createHash("sha256").update("built").digest("hex");
    writeFileSync(binary, "tampered");
    assert.throws(
      () => assertCompilerBinaryAtExec(binary, builtHash),
      /COMPILER_BINARY_HASH_MISMATCH/,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
