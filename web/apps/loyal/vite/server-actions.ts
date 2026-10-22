import { readFileSync } from "node:fs";
import { relative } from "node:path";

import type { Plugin } from "vite";

export type ServerActionsOptions = {
  root: string;
  endpoint: string;
  headers: Record<string, string>;
};

const client = new URL("../src/next-compat/server-action.ts", import.meta.url).pathname;
const directive = /^(?:\s|\/\/[^\n]*\n|\/\*[\s\S]*?\*\/)*["']use server["']/;
const actionExport = /^export\s+async\s+function\s+(\w+)/gm;
const anyExport = /^export\s+(?!type\b|interface\b)/gm;

export function isServerActionModule(code: string): boolean {
  return directive.test(code);
}

export function serverActionsPlugin(options: ServerActionsOptions): Plugin {
  const stubs = new Map<string, number>();
  return {
    name: "loyal-server-actions",
    enforce: "pre",
    load(id) {
      const file = id.split("?")[0];
      if (!/\.tsx?$/.test(file) || !file.startsWith(options.root)) return null;
      const code = readFileSync(file, "utf8");
      if (!isServerActionModule(code)) return null;
      const names = [...code.matchAll(actionExport)].map((match) => match[1]);
      const exported = code.match(anyExport)?.length ?? 0;
      if (names.length !== exported) {
        this.error(`${file}: a "use server" module may export only async functions; found ${exported} exports, ${names.length} async functions`);
      }
      const module = relative(options.root, file).replace(/\.tsx?$/, "");
      stubs.set(module, names.length);
      const headers = JSON.stringify(options.headers);
      return [
        `import { callAction } from ${JSON.stringify(client)};`,
        ...names.map(
          (name) =>
            `export const ${name} = (...args) => callAction(${JSON.stringify(`${options.endpoint}/${module}/${name}`)}, args, ${headers});`,
        ),
      ].join("\n");
    },
    buildEnd() {
      const count = [...stubs.values()].reduce((sum, n) => sum + n, 0);
      console.log(`server actions: ${count} stubs from ${stubs.size} modules`);
    },
  };
}
