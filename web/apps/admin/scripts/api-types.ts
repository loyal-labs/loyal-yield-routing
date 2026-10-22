import { join } from "node:path";
import { writeFileSync } from "node:fs";

import { generate, responseType } from "../../../../scripts/web/openapi-types";

const out = join(import.meta.dir, "../src/api.gen");

const openapi = generate({ cmd: "loyal-admin", out, check: false });
const rows = Object.entries(openapi.paths ?? {}).flatMap(([path, item]) => {
  if (!path.startsWith("/api/admin/page") || !item.get) return [];
  const type = responseType(item.get);
  if (!type) throw new Error(`${path}: a page loader must return a named struct`);
  return [`  ${JSON.stringify(path.slice("/api/admin/page".length) || "/")}: Types.${type};`];
});
writeFileSync(
  join(out, "pages.ts"),
  `import type * as Types from "./types.gen";\n\nexport type Pages = {\n${rows.join("\n")}\n};\n`,
);
console.log(`api types: ${rows.length} page loaders`);
