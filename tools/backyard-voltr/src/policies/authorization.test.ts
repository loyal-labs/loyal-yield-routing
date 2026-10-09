import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import { loadPolicyCatalogAuthorization, policyCatalogAuthorizationPath } from "./authorization.js";

const repository = resolve(import.meta.dirname, "../../../..");

// Loading recompiles the catalog with the pinned Rust compiler (cargo
// --locked), so a cold toolchain needs more than the default test timeout.
test("the committed authorization loads against the recompiled catalog", { timeout: 900_000 }, () => {
  const authorizationPath = policyCatalogAuthorizationPath();
  const { artifactPath } = JSON.parse(readFileSync(authorizationPath, "utf8")) as { artifactPath: string };
  loadPolicyCatalogAuthorization(authorizationPath, resolve(repository, artifactPath));
});
