import { readFileSync } from "node:fs";
import { relative } from "node:path";

import type { Plugin } from "vite";

import { isServerActionModule } from "./server-actions";

const serverSpecifier =
  /^(?:server-only|next\/(?:headers|cache|server)|drizzle-orm(?:\/.*)?|@neondatabase\/serverless|@loyal-labs\/db-adapter-neon(?:\/.*)?|node:.*|crypto|fs|path)$/;
const valueImport = /^\s*import\s+(?!type\b)(?:[^'"]*?\sfrom\s+)?["']([^"']+)["']/gm;
const exportName =
  /^export\s+(?:async\s+)?(?:function\*?|const|let|var|class)\s+(\w+)|^export\s+\{([^}]*)\}/gm;

function serverImports(code: string): string[] {
  return [...code.matchAll(valueImport)].map((match) => match[1]).filter((spec) => serverSpecifier.test(spec));
}

function stub(file: string, code: string): string {
  const names = new Set<string>();
  for (const match of code.matchAll(exportName)) {
    if (match[1]) names.add(match[1]);
    for (const part of match[2]?.split(",") ?? []) {
      const name = part.trim().replace(/^type\s+/, "").split(/\s+as\s+/).pop()?.trim();
      if (name && !part.trim().startsWith("type ")) names.add(name);
    }
  }
  const fail = `() => { throw new Error(${JSON.stringify(`server module in the client bundle: ${file}`)}); }`;
  const local = [...code.matchAll(valueImport)].map((match) => match[1]).filter((spec) => /^(?:\.|@\/)/.test(spec));
  return [
    ...local.map((spec) => `import ${JSON.stringify(spec)};`),
    `const serverOnly = new Proxy(${fail}, { get: ${fail}, apply: ${fail} });`,
    ...[...names].map((name) => `export const ${name} = serverOnly;`),
    /^export\s+default\b/m.test(code) ? "export default serverOnly;" : "",
  ].join("\n");
}

export function serverOnlyPlugin(options: { mode: "error" | "warn"; root: string }): Plugin {
  const server = new Map<string, string[]>();
  return {
    name: "loyal-server-only",
    enforce: "pre",
    load(id) {
      const file = id.split("?")[0];
      if (!/\.tsx?$/.test(file) || !file.startsWith(options.root) || file.includes("/node_modules/")) return null;
      const code = readFileSync(file, "utf8");
      if (isServerActionModule(code)) return null;
      const imports = serverImports(code);
      if (!imports.length) return null;
      server.set(file, imports);
      return stub(file, code);
    },
    buildEnd() {
      if (!server.size) return;
      const pages = [...this.getModuleIds()].filter((id) => id.startsWith(options.root) && /\/app\/(?:.*\/)?(?:page|layout)\.tsx$/.test(id));
      const lines: string[] = [];
      for (const page of pages.sort()) {
        const found = new Set<string>();
        const seen = new Set<string>();
        const walk = (id: string) => {
          if (seen.has(id)) return;
          seen.add(id);
          if (server.has(id)) found.add(id);
          const info = this.getModuleInfo(id);
          for (const next of info?.importedIds ?? []) walk(next);
        };
        walk(page);
        if (!found.size) continue;
        lines.push(relative(options.root, page));
        for (const id of [...found].sort()) lines.push(`  ${relative(options.root, id)}  (${server.get(id)!.join(", ")})`);
      }
      const report = `server modules still imported by client pages (${server.size} modules):\n${lines.join("\n")}`;
      if (options.mode === "error") this.error(report);
      console.warn(report);
    },
  };
}
