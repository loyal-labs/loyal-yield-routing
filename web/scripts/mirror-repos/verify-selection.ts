import assert from "node:assert/strict";

import { mirrors } from "./config";
import { selectMirrorsForChangedPaths } from "./selection";

const selected = (paths: string[]) =>
  selectMirrorsForChangedPaths(paths).map((mirror) => mirror.repo);

assert.deepEqual(selected(["web/apps/mobile/app/index.tsx"]), [
  "loyal-labs/loyal-mobile",
]);
assert.deepEqual(selected(["web/apps/web/src/app/page.tsx"]), [
  "loyal-labs/loyal-webapp",
]);
assert.deepEqual(selected(["web/packages/shared/src/index.ts"]), [
  "loyal-labs/loyal-webapp",
  "loyal-labs/loyal-telegram-app",
  "loyal-labs/loyal-mobile",
  "loyal-labs/loyal-extension",
  "loyal-labs/loyal-packages",
]);
assert.deepEqual(
  selected(["packages/other/index.ts", "go/workers/main.go", "web/bun.lock"]),
  []
);
for (const changed of [
  ".github/workflows/sync-mirror-repos.yml",
  "web/scripts/mirror-repos/sync.ts",
]) {
  assert.deepEqual(
    selected([changed]),
    mirrors.map((mirror) => mirror.repo)
  );
}
assert.deepEqual(selected(["web/apps/mobile/a.ts", "web/apps/mobile/b.ts"]), [
  "loyal-labs/loyal-mobile",
]);
console.log("Mirror publication selection boundaries passed.");
