// Go structs to OpenAPI to TypeScript, so a client never hand-copies a response shape.
//
//   bun scripts/web/openapi-types.ts          regenerate web/packages/api/src/generated from loyal-web
//   bun scripts/web/openapi-types.ts --check  fail when the committed output differs
//
// `generate` is shared with web/apps/admin/scripts/api-types.ts.
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, relative } from "node:path";

const root = join(import.meta.dir, "../..");
const web = join(root, "web");
const workers = join(root, "go/workers");
const openapiTs = join(web, "apps/loyal/node_modules/.bin/openapi-ts");

type Schema = { $ref?: string };
export type Operation = { responses?: Record<string, { content?: Record<string, { schema?: Schema }> }> };
export type Openapi = { paths?: Record<string, Record<string, Operation>> };

function run(command: string, args: string[], cwd: string): string {
  const result = spawnSync(command, args, { cwd, encoding: "utf8", env: { ...process.env, GOTOOLCHAIN: "local" } });
  if (result.status !== 0) throw new Error(`${command} ${args.join(" ")}: ${result.stderr}`);
  return result.stdout;
}

export function responseType(operation: Operation): string | undefined {
  return operation.responses?.["200"]?.content?.["application/json"]?.schema?.$ref?.split("/").pop();
}

function files(dir: string): string[] {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile())
    .map((entry) => relative(dir, join(entry.parentPath, entry.name)))
    .sort();
}

export type GenerateOptions = {
  cmd: string;
  out: string;
  check: boolean;
  extra?: (openapi: Openapi) => Record<string, string>;
};

export function openapiOf(cmd: string): Openapi {
  return JSON.parse(run("go", ["run", `./cmd/${cmd}`, "openapi"], workers)) as Openapi;
}

export function generate({ cmd, out, check, extra }: GenerateOptions): Openapi {
  const scratch = mkdtempSync(join(tmpdir(), "openapi-types-"));
  const dest = check ? join(scratch, "out") : out;
  const spec = join(scratch, "openapi.json");
  const openapi = openapiOf(cmd);
  writeFileSync(spec, JSON.stringify(openapi));
  mkdirSync(dest, { recursive: true });
  run(openapiTs, ["-i", spec, "-o", dest, "-p", "@hey-api/typescript", "-s", "--no-log-file"], web);
  for (const [name, content] of Object.entries(extra?.(openapi) ?? {})) writeFileSync(join(dest, name), content);
  if (check) {
    const stale = files(dest).filter((name) => !files(out).includes(name) || readFileSync(join(dest, name), "utf8") !== readFileSync(join(out, name), "utf8"));
    const extraneous = files(out).filter((name) => !files(dest).includes(name));
    if (stale.length + extraneous.length > 0) {
      throw new Error(`${relative(root, out)} differs from \`${cmd} openapi\` (${[...stale, ...extraneous].join(", ")}): run bun scripts/web/openapi-types.ts and commit`);
    }
  }
  return openapi;
}

function routeMaps(openapi: Openapi): Record<string, string> {
  const maps: Record<"get" | "post", string[]> = { get: [], post: [] };
  for (const [path, item] of Object.entries(openapi.paths ?? {})) {
    for (const method of ["get", "post"] as const) {
      const operation = item[method];
      if (!operation) continue;
      const type = responseType(operation);
      if (!type) throw new Error(`${method.toUpperCase()} ${path}: a route must return a named struct as 200 application/json`);
      maps[method].push(`  ${JSON.stringify(path)}: Types.${type};`);
    }
  }
  const body = (method: "get" | "post") => (maps[method].length ? `{\n${maps[method].join("\n")}\n}` : "Record<never, never>");
  return {
    "routes.gen.ts": `import type * as Types from "./types.gen";\n\nexport type Get = ${body("get")};\n\nexport type Post = ${body("post")};\n`,
  };
}

if (import.meta.main) {
  try {
    const openapi = generate({ cmd: "loyal-web", out: join(web, "packages/api/src/generated"), check: process.argv.includes("--check"), extra: routeMaps });
    console.log(`api types: ${Object.keys(openapi.paths ?? {}).length} paths`);
  } catch (error) {
    console.error(error instanceof Error ? error.message : error);
    process.exit(1);
  }
}
