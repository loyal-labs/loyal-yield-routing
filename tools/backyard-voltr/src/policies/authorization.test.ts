import { strict as assert } from "node:assert";
import { createHash } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import { earnAdapterSourcePaths } from "../runtime/earn-adapter.js";
import {
  loadPolicyCatalogAuthorization,
  policyCatalogAuthorizationPath,
  policyCatalogSourcePaths,
} from "./authorization.js";

const repository = resolve(import.meta.dirname, "../../../..");

type CommittedAuthorization = Readonly<{
  artifactPath: string;
  sourceBinding: Readonly<{ files: readonly Readonly<{ path: string; sha256: string }>[] }>;
}>;

function committedAuthorization(): CommittedAuthorization {
  return JSON.parse(readFileSync(policyCatalogAuthorizationPath(), "utf8")) as CommittedAuthorization;
}

// v24 kept binding deleted Rust crates and drifted for weeks while every
// authorization-gated command refused. Fail here instead, the moment a bound
// file moves or changes without a reviewed authorization refresh.
test("every bound source exists", () => {
  const missing = [...policyCatalogSourcePaths(), ...earnAdapterSourcePaths()]
    .filter((path) => !existsSync(resolve(repository, path)));
  assert.deepEqual(missing, []);
});

test("the committed authorization binds the checked-out sources", () => {
  const { sourceBinding } = committedAuthorization();
  assert.deepEqual(sourceBinding.files.map(({ path }) => path), [...policyCatalogSourcePaths()], "SOURCE_PATHS changed: write the next policy-catalog-authorization version (tools/backyard-voltr/README.md)");
  const drifted = sourceBinding.files
    .filter(({ path, sha256 }) => createHash("sha256").update(readFileSync(resolve(repository, path))).digest("hex") !== sha256)
    .map(({ path }) => path);
  assert.deepEqual(drifted, [], "bound sources changed: write the next policy-catalog-authorization version (tools/backyard-voltr/README.md)");
});

// Loading recompiles the catalog with the pinned Rust compiler (cargo
// --locked), so a cold toolchain needs more than the default test timeout.
test("the committed authorization loads against the recompiled catalog", { timeout: 900_000 }, () => {
  const authorizationPath = policyCatalogAuthorizationPath();
  loadPolicyCatalogAuthorization(authorizationPath, resolve(repository, committedAuthorization().artifactPath));
});
