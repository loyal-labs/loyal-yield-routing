// Scaffold an app's src/routes: loyal from routes.table.ts, admin from the
// pages under web/apps/admin/src/app.
//
//   bun scripts/web/gen-routes.ts [--app loyal|admin]          write route files that do not exist yet
//   bun scripts/web/gen-routes.ts [--app loyal|admin] --check  fail on a row without a route file,
//                                                              or a Next page/layout without a row
//
// A scaffolded file belongs to its row's lane afterwards; this never rewrites
// an existing file.
import { existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";

import { type RouteRow, routes } from "../../web/apps/loyal/src/routes.table";

const root = join(import.meta.dir, "../..");
const appIndex = process.argv.indexOf("--app");
const app = appIndex === -1 ? "loyal" : process.argv[appIndex + 1];
if (app !== "loyal" && app !== "admin") throw new Error(`--app loyal|admin, got ${app}`);
const nextApp = join(root, app === "loyal" ? "web/apps/web/src/app" : "web/apps/admin/src/app");
const routesDir = join(root, `web/apps/${app}/src/routes`);
const adminGroup = "(admin)";

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

function adminRows(): RouteRow[] {
  return nextFiles(nextApp)
    .filter((file) => file !== "layout.tsx")
    .map((file) => {
      const kind = file.endsWith("layout.tsx") ? "layout" : "page";
      const segments = dirname(file).split("/").filter((segment) => segment !== ".");
      const path = `/${segments.map((segment) => (segment === adminGroup ? "_admin" : segment.replace(/^\[(\w+)\]$/, "$$$1"))).join("/")}`;
      return { next: file, path: path === "/" ? "/" : path, kind, host: "app", owner: "G2" } as RouteRow;
    });
}

function adminSource(row: RouteRow): string {
  const module = `@/app/${row.next.replace(/\.tsx$/, "")}`;
  const pageKey = row.next.startsWith(`${adminGroup}/`)
    ? `/${dirname(row.next).split("/").slice(1).map((segment) => segment.replace(/^\[(\w+)\]$/, "{$1}")).join("/")}`
    : null;
  const head = [`import { createFileRoute${row.kind === "layout" ? ", Outlet" : ""} } from "@tanstack/react-router";`, "", `import Source from "${module}";`];
  if (row.kind === "layout") {
    return `${head.join("\n")}\n\nexport const Route = createFileRoute("${routeId(row)}")({\n  component: () => (\n    <Source>\n      <Outlet />\n    </Source>\n  ),\n});\n`;
  }
  if (!pageKey) {
    return `${head.join("\n")}\n\nexport const Route = createFileRoute("${routeId(row)}")({\n  component: Source,\n});\n`;
  }
  return `${head.join("\n")}

import type { ComponentProps } from "react";
import { pageData, type PageProps } from "@/lib/page-data";

type Props = PageProps<"${pageKey}", typeof Source>;

export const propsMatchView: unknown extends Props ? true : Props extends ComponentProps<typeof Source> ? true : never = true;

export const Route = createFileRoute("${routeId(row)}")({
  loaderDeps: ({ search }) => ({ search }),
  loader: ({ location }) => pageData<Props>(location),
  component: function Page() {
    return <Source {...Route.useLoaderData()} />;
  },
});
`;
}

function nextFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return name === "api" ? [] : nextFiles(path);
    return name === "page.tsx" || name === "layout.tsx" ? [relative(nextApp, path)] : [];
  });
}

const problems: string[] = [];
const table = app === "loyal" ? routes : adminRows();
const rows = new Set(table.map((row) => row.next));
for (const file of nextFiles(nextApp)) {
  if (file !== "layout.tsx" && !rows.has(file)) problems.push(`no row for ${relative(root, join(nextApp, file))}`);
}

const check = process.argv.includes("--check");
for (const row of table) {
  const file = routeFile(row);
  if (existsSync(file)) continue;
  if (check) {
    problems.push(`no route file ${relative(root, file)} for ${row.next}`);
    continue;
  }
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, app === "loyal" ? source(row) : adminSource(row));
  console.log(`wrote ${relative(root, file)}`);
}

if (problems.length) {
  console.error(problems.join("\n"));
  process.exit(1);
}
