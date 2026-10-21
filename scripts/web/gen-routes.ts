// Scaffold web/apps/loyal/src/routes from routes.table.ts.
//
//   bun scripts/web/gen-routes.ts          write route files that do not exist yet
//   bun scripts/web/gen-routes.ts --check  fail on a row without a route file,
//                                          or a Next page/layout without a row
//
// A scaffolded file belongs to its row's lane afterwards; this never rewrites
// an existing file.
import { existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";

import { type RouteRow, routes } from "../../web/apps/loyal/src/routes.table";

const root = join(import.meta.dir, "../..");
const nextApp = join(root, "web/apps/web/src/app");
const routesDir = join(root, "web/apps/loyal/src/routes");

function routeFile(row: RouteRow): string {
  const segments = row.path === "/" ? [] : row.path.slice(1).split("/");
  return join(routesDir, ...segments, row.kind === "layout" ? "route.tsx" : "index.tsx");
}

function routeId(row: RouteRow): string {
  return row.kind === "layout" ? row.path : row.path === "/" ? "/" : `${row.path}/`;
}

function source(row: RouteRow): string {
  const file = readFileSync(join(nextApp, row.next), "utf8");
  const module = `@/app/${row.next.replace(/\.tsx$/, "")}`;
  const hasMetadata = /export const metadata\b/.test(file);
  const compat = relative(dirname(routeFile(row)), join(root, "web/apps/loyal/src/next-compat/metadata")).replace(/^(?!\.)/, "./");
  const imports = [
    `import { createFileRoute${row.kind === "layout" ? ", Outlet" : ""} } from "@tanstack/react-router";`,
    "",
    hasMetadata ? `import Source, * as source from "${module}";` : `import Source from "${module}";`,
    ...(hasMetadata ? ["", `import { pageHead } from "${compat}";`] : []),
  ];
  const component = row.kind === "layout" ? "component: () => (\n    <Source>\n      <Outlet />\n    </Source>\n  )," : "component: Source,";
  const head = hasMetadata ? "\n  head: () => pageHead(source.metadata)," : "";
  return `${imports.join("\n")}\n\nexport const Route = createFileRoute("${routeId(row)}")({\n  ${component}${head}\n});\n`;
}

function nextFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return name === "api" ? [] : nextFiles(path);
    return name === "page.tsx" || name === "layout.tsx" ? [relative(nextApp, path)] : [];
  });
}

const problems: string[] = [];
const rows = new Set(routes.map((row) => row.next));
for (const file of nextFiles(nextApp)) {
  if (file !== "layout.tsx" && !rows.has(file)) problems.push(`no row for apps/web/src/app/${file}`);
}

const check = process.argv.includes("--check");
for (const row of routes) {
  const file = routeFile(row);
  if (existsSync(file)) continue;
  if (check) {
    problems.push(`no route file ${relative(root, file)} for ${row.next}`);
    continue;
  }
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, source(row));
  console.log(`wrote ${relative(root, file)}`);
}

if (problems.length) {
  console.error(problems.join("\n"));
  process.exit(1);
}
