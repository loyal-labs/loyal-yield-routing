import assert from "node:assert/strict";
import { test } from "node:test";

import { compileCustomPolicyArtifact } from "./rwa-multiply-custom.js";
import {
  compilerBuildPlan,
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
    assert.equal(
      plan.compilerBinaryPath,
      `${plan.compilerTargetDir}/debug/compile-voltr-custom-policy`,
    );
    assert.deepEqual(plan.buildArgs, [
      "build",
      "--quiet",
      "-p",
      "loyal-actions",
      "--bin",
      "compile-voltr-custom-policy",
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
