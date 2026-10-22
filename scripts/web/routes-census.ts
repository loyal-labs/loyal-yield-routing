// A client path implies a server route. This census reads both sides and fails when they drift.
//
//   bun scripts/web/routes-census.ts --check
//
// Servers: the Next routes of web/apps/{web,telegram} and the paths of `loyal-web openapi`.
// Clients: path templates in web/apps/mobile built as `${env.<base>}/api/...`, and string
// arguments of helpers that build such a URL from a parameter.
// Fails on (a) a client path no server answers, (b) a Next or Go route under /mobile/ outside
// mobile-twins.txt and GO_MOBILE_ROUTES, (c) a hand-written exported type named like a generated one.
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";

import ts from "../../web/node_modules/typescript/lib/typescript.js";

import { type Openapi, openapiOf } from "./openapi-types";

const root = join(import.meta.dir, "../..");
const web = join(root, "web");
const mobile = join(web, "apps/mobile");
const twinsFile = join(import.meta.dir, "mobile-twins.txt");

// What each base variable of web/apps/mobile/src/config/env.ts reaches. The Go server
// (`loyal-web`) answers behind the same host as the Next app it replaces.
const BASES: Record<string, { next: NextApp[]; go: boolean }> = {
  apiBaseUrl: { next: ["telegram"], go: true },
  earnApiBaseUrl: { next: ["web"], go: true },
};
const THIRD_PARTY_BASES = new Set(["gridAuthBaseUrl"]);

// Content routes Go serves on their Telegram paths, with no web twin.
const GO_MOBILE_ROUTES = new Set(["/api/mobile/dapps", "/api/mobile/library", "/api/mobile/tokens/{mint}"]);

// Hand-written client code whose exported types must not repeat a generated one.
const CLIENT_TYPE_DIRS = [join(mobile, "src"), join(web, "apps/loyal/src")];

type NextApp = "web" | "telegram";
type Server = { origin: string; path: string; methods: string[] };
type ClientPath = { base: string; method?: string; path: string; at: string };

const errors: string[] = [];
const fail = (message: string) => errors.push(message);

function walk(dir: string, keep: (file: string) => boolean): string[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && !entry.parentPath.includes("node_modules"))
    .map((entry) => join(entry.parentPath, entry.name))
    .filter(keep)
    .sort();
}

function segments(path: string): string[] {
  return path.split("/").filter(Boolean);
}

function nextRoutes(app: NextApp): Server[] {
  const appDir = join(web, `apps/${app}/src/app`);
  return walk(join(appDir, "api"), (file) => /\/route\.tsx?$/.test(file)).map((file) => {
    const path = `/${relative(appDir, join(file, "..")).split("/").filter((part) => !/^\(.*\)$/.test(part)).join("/")}`;
    const source = readFileSync(file, "utf8");
    const methods = ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"].filter((method) =>
      new RegExp(`export\\s+(async\\s+)?(function|const)\\s+${method}\\b|export\\s*\\{[^}]*\\b${method}\\b`).test(source),
    );
    return { origin: `${app} app`, path, methods };
  });
}

function goRoutes(openapi: Openapi): Server[] {
  return Object.entries(openapi.paths ?? {}).map(([path, item]) => ({
    origin: "Go",
    path,
    methods: Object.keys(item).map((method) => method.toUpperCase()),
  }));
}

function isDynamic(segment: string): boolean {
  return /^\[.*\]$/.test(segment) || /^\{.*\}$/.test(segment);
}

function answers(server: Server, client: ClientPath): boolean {
  const have = segments(server.path);
  const want = segments(client.path);
  for (let i = 0; i < have.length; i++) {
    if (/^\[\[?\.\.\./.test(have[i])) return i <= want.length;
    if (i >= want.length) return false;
    if (isDynamic(have[i]) || have[i] === want[i]) continue;
    const pattern = new RegExp(`^${want[i].split("{}").map((part) => part.replace(/[.*+?^$()|\\]/g, "\\$&")).join("[^/]+")}$`);
    if (!pattern.test(have[i])) return false;
  }
  return have.length === want.length;
}

function mobileSegment(path: string): boolean {
  return segments(path).includes("mobile");
}

function stringValue(node: ts.Node): string | undefined {
  return ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node) ? node.text : undefined;
}

function envBase(node: ts.Node): string | undefined {
  return ts.isPropertyAccessExpression(node) && node.expression.getText() === "env" ? node.name.text : undefined;
}

function callMethod(node: ts.Node): string | undefined {
  const call = node.parent;
  if (!ts.isCallExpression(call) || call.arguments[0] !== node) return undefined;
  const options = call.arguments[1];
  if (!options) return /fetch/i.test(call.expression.getText()) ? "GET" : undefined;
  if (!ts.isObjectLiteralExpression(options)) return undefined;
  for (const property of options.properties) {
    if (ts.isPropertyAssignment(property) && property.name.getText() === "method") return stringValue(property.initializer)?.toUpperCase();
  }
  return options.properties.some(ts.isSpreadAssignment) ? undefined : "GET";
}

type Helper = { name: string; base: string; method?: string; parameter: number };

function clientPaths(): ClientPath[] {
  const files = ["src", "app"].flatMap((dir) =>
    walk(join(mobile, dir), (file) => /\.tsx?$/.test(file) && !/(__tests__|\.test\.|\/config\/env\.ts$)/.test(file)),
  );
  const sources = files.map((file) => ({
    file,
    sf: ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true),
  }));
  const at = (sf: ts.SourceFile, node: ts.Node) => `${relative(root, sf.fileName)}:${sf.getLineAndCharacterOfPosition(node.getStart()).line + 1}`;
  const helpers: Helper[] = [];
  const paths: ClientPath[] = [];

  for (const { sf } of sources) {
    const consumed = new Set<ts.Node>();
    const uses: ts.Node[] = [];
    const visit = (node: ts.Node) => {
      if (ts.isTemplateExpression(node) && node.head.text === "" && envBase(node.templateSpans[0].expression)) {
        const base = envBase(node.templateSpans[0].expression)!;
        const [first, second] = node.templateSpans;
        const dynamic = first.literal.text === "" && second !== undefined && ts.isIdentifier(second.expression);
        consumed.add(node.templateSpans[0].expression);
        if (!BASES[base]) {
          if (!THIRD_PARTY_BASES.has(base)) fail(`${at(sf, node)}: env.${base} is a base the census does not know: add it to BASES or THIRD_PARTY_BASES in scripts/web/routes-census.ts`);
        } else if (dynamic && !ts.isCallExpression(node.parent)) {
          // A dynamic path that is no call argument is an asset URL, not a request.
        } else if (dynamic) {
          let fn: ts.Node | undefined = node;
          while (fn && !ts.isFunctionDeclaration(fn)) fn = fn.parent;
          const parameter = fn && ts.isFunctionDeclaration(fn) ? fn.parameters.findIndex((p) => p.name.getText() === second.expression.getText()) : -1;
          if (!fn || !ts.isFunctionDeclaration(fn) || parameter < 0 || !fn.name) {
            fail(`${at(sf, node)}: \`\${env.${base}}\${...}\` builds a URL the census cannot read: pass the path as a parameter of a named function, or write the path inline`);
          } else {
            helpers.push({ name: fn.name.text, base, method: callMethod(node), parameter });
          }
        } else {
          const path = (first.literal.text + node.templateSpans.slice(1).map((span) => `{}${span.literal.text}`).join("")).split("?")[0];
          if (!path.startsWith("/")) fail(`${at(sf, node)}: \`\${env.${base}}\` is followed by no static path: write the path inline so the census can read it`);
          else paths.push({ base, method: callMethod(node), path, at: at(sf, node) });
        }
      }
      const use = ts.isPropertyAccessExpression(node) ? envBase(node) : undefined;
      if (use && /BaseUrl$/.test(use)) uses.push(node);
      ts.forEachChild(node, visit);
    };
    visit(sf);
    for (const use of uses) {
      if (!consumed.has(use)) fail(`${at(sf, use)}: env.${envBase(use)} is used outside a \`\${env.${envBase(use)}}/api/...\` template: build the URL inline so the census can read it`);
    }
  }

  for (const { sf } of sources) {
    const visit = (node: ts.Node) => {
      if (ts.isCallExpression(node) && ts.isIdentifier(node.expression)) {
        for (const helper of helpers.filter((h) => h.name === node.expression.getText())) {
          const arg = node.arguments[helper.parameter];
          let path = arg && stringValue(arg);
          if (arg && path === undefined && ts.isIdentifier(arg)) {
            for (const statement of sf.statements) {
              if (!ts.isVariableStatement(statement)) continue;
              for (const declaration of statement.declarationList.declarations) {
                if (declaration.name.getText() === arg.text && declaration.initializer) path = stringValue(declaration.initializer);
              }
            }
          }
          if (path === undefined) fail(`${at(sf, node)}: ${helper.name}() takes the path of a request: pass a string literal or a top-level string const of the same file`);
          else paths.push({ base: helper.base, method: helper.method, path, at: at(sf, node) });
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(sf);
  }
  return paths;
}

function twins(): Set<string> {
  return new Set(
    readFileSync(twinsFile, "utf8")
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line && !line.startsWith("#")),
  );
}

function checkTwins(next: Record<NextApp, Server[]>): void {
  const listed = twins();
  const existing = new Set<string>();
  for (const app of Object.keys(next) as NextApp[]) {
    for (const route of next[app]) {
      if (!mobileSegment(route.path)) continue;
      const line = `${app} ${route.path}`;
      existing.add(line);
      if (!listed.has(line)) {
        fail(`new Next route under /mobile/: ${line}: Go serves mobile on the web paths, so serve this from Go on its web path and delete the Next route (scripts/web/mobile-twins.txt may only shrink)`);
      }
    }
  }
  for (const line of listed) {
    if (!existing.has(line)) fail(`scripts/web/mobile-twins.txt lists \`${line}\`, which no longer exists: delete that line`);
  }
  const base = spawnSync("git", ["show", "origin/main:scripts/web/mobile-twins.txt"], { cwd: root, encoding: "utf8" });
  if (base.status === 0) {
    const before = new Set(base.stdout.split("\n").map((line) => line.trim()));
    for (const line of listed) {
      if (!before.has(line)) fail(`scripts/web/mobile-twins.txt gained \`${line}\`: the list may only shrink, so delete the line and the Next route`);
    }
  }
}

function checkGoMobile(go: Server[]): void {
  for (const route of go) {
    if (mobileSegment(route.path) && !GO_MOBILE_ROUTES.has(route.path)) {
      fail(`Go serves ${route.path} under /mobile/: serve it on the web path the web client uses, or for a content route add it to GO_MOBILE_ROUTES in scripts/web/routes-census.ts`);
    }
  }
}

function checkGeneratedTypes(): void {
  const generated = join(web, "packages/api/src/generated/types.gen.ts");
  const names = new Set([...readFileSync(generated, "utf8").matchAll(/^export type (\w+)/gm)].map((match) => match[1]));
  names.delete("ClientOptions");
  for (const dir of CLIENT_TYPE_DIRS) {
    for (const file of walk(dir, (f) => /\.tsx?$/.test(f) && !/\.gen\.ts$|\/api\.gen\//.test(f))) {
      const sf = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true);
      for (const statement of sf.statements) {
        if ((ts.isTypeAliasDeclaration(statement) || ts.isInterfaceDeclaration(statement)) && names.has(statement.name.text) && statement.modifiers?.some((m) => m.kind === ts.SyntaxKind.ExportKeyword)) {
          const line = sf.getLineAndCharacterOfPosition(statement.getStart()).line + 1;
          fail(`${relative(root, file)}:${line}: exported type ${statement.name.text} repeats one generated in @loyal-labs/api: delete it and \`import type { ${statement.name.text} } from "@loyal-labs/api"\``);
        }
      }
    }
  }
}

const next: Record<NextApp, Server[]> = { web: nextRoutes("web"), telegram: nextRoutes("telegram") };
const go = goRoutes(openapiOf("loyal-web"));
const paths = clientPaths();

const seen = new Set<string>();
let matched = 0;
let total = 0;
for (const client of paths) {
  const key = `${client.base} ${client.method ?? "*"} ${client.path}`;
  if (seen.has(key)) continue;
  seen.add(key);
  total++;
  const servers = [...BASES[client.base].next.flatMap((app) => next[app]), ...(BASES[client.base].go ? go : [])];
  const byPath = servers.filter((server) => answers(server, client));
  if (byPath.length === 0) {
    fail(`${client.at}: ${client.method ?? ""} ${client.path} (env.${client.base}) is answered by no server (${[...BASES[client.base].next.map((app) => `${app} app`), "Go"].join(", ")}): add the route or delete the call`);
  } else if (client.method && !byPath.some((server) => server.methods.includes(client.method!))) {
    fail(`${client.at}: ${client.path} (env.${client.base}) is served (${byPath.map((s) => s.origin).join(", ")}) without ${client.method}: export ${client.method} from the route or change the call`);
  } else {
    matched++;
  }
}
checkTwins(next);
checkGoMobile(go);
checkGeneratedTypes();

if (errors.length > 0) {
  console.error(errors.map((error) => `routes census: ${error}`).join("\n"));
  process.exit(1);
}
console.log(`routes census: ${next.web.length + next.telegram.length} Next + ${go.length} Go server routes, ${total} client paths, ${matched} matched`);
