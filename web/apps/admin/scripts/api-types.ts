import { spawnSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const admin = join(import.meta.dir, "..");
const workers = join(admin, "../../../go/workers");
const spec = join(admin, "openapi.json");
const out = join(admin, "src/api.gen");

function run(command: string, args: string[], cwd: string): string {
  const result = spawnSync(command, args, { cwd, encoding: "utf8", env: { ...process.env, GOTOOLCHAIN: "local" } });
  if (result.status !== 0) throw new Error(`${command} ${args.join(" ")}: ${result.stderr}`);
  return result.stdout;
}

type Schema = { $ref?: string };
type Operation = { responses?: Record<string, { content?: Record<string, { schema?: Schema }> }> };
const openapi = JSON.parse(run("go", ["run", "./cmd/loyal-admin", "openapi"], workers)) as {
  paths?: Record<string, { get?: Operation }>;
};
writeFileSync(spec, JSON.stringify(openapi));
mkdirSync(out, { recursive: true });
run(join(admin, "../loyal/node_modules/.bin/openapi-ts"), ["-i", spec, "-o", out, "-p", "@hey-api/typescript", "-s", "--no-log-file"], admin);

const rows = Object.entries(openapi.paths ?? {}).flatMap(([path, item]) => {
  if (!path.startsWith("/api/admin/page") || !item.get) return [];
  const ref = item.get.responses?.["200"]?.content?.["application/json"]?.schema?.$ref;
  if (!ref) throw new Error(`${path}: a page loader must return a named struct`);
  return [`  ${JSON.stringify(path.slice("/api/admin/page".length) || "/")}: Types.${ref.split("/").pop()};`];
});
writeFileSync(
  join(out, "pages.ts"),
  `import type * as Types from "./types.gen";\n\nexport type Pages = {\n${rows.join("\n")}\n};\n`,
);
console.log(`api types: ${rows.length} page loaders`);
