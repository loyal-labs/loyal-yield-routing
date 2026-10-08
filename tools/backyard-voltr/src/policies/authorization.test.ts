import { strict as assert } from "node:assert";
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

// v24 kept binding deleted Rust crates and drifted for weeks while every
// authorization-gated command refused. Fail here instead, the moment a bound
// file moves or changes without a reviewed authorization refresh.
test("every bound source exists", () => {
  const missing = [...policyCatalogSourcePaths(), ...earnAdapterSourcePaths()]
    .filter((path) => !existsSync(resolve(repository, path)));
  assert.deepEqual(missing, []);
});

test("the committed authorization matches the checked-out sources", () => {
  const authorizationPath = policyCatalogAuthorizationPath();
  const { artifactPath } = JSON.parse(readFileSync(authorizationPath, "utf8")) as { artifactPath: string };
  assert.doesNotThrow(
    () => loadPolicyCatalogAuthorization(authorizationPath, resolve(repository, artifactPath)),
    "a bound source changed: write the next policy-catalog-authorization version (tools/backyard-voltr/README.md)",
  );
});
