import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/postcss";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { type Plugin, type UserConfig } from "vite";

import { resolveVercelEnv } from "../web/src/lib/vercel-env";
import { type ServerActionsOptions, serverActionsPlugin } from "./vite/server-actions";
import { serverOnlyPlugin } from "./vite/server-only";

const here = (path: string) => fileURLToPath(new URL(path, import.meta.url));

const compat = {
  "next/link": here("src/next-compat/link.tsx"),
  "next/image": here("src/next-compat/image.tsx"),
  "next/navigation": here("src/next-compat/navigation.ts"),
  "next/dynamic": here("src/next-compat/dynamic.tsx"),
  "next/font/google": here("src/next-compat/font-google.ts"),
  "geist/font/sans": here("src/next-compat/geist-sans.ts"),
  "geist/font/mono": here("src/next-compat/geist-mono.ts"),
  "server-only": here("src/next-compat/server-only.ts"),
};

export type NextSpaOptions = {
  nextApp: string;
  overrides?: Record<string, string>;
  serverActions?: ServerActionsOptions;
  serverOnly?: { mode: "error" | "warn"; root: string };
  devPort: number;
  devProxy: string[];
  router?: boolean;
};

function overridesPlugin(overrides: Record<string, string>): Plugin {
  return {
    name: "loyal-overrides",
    enforce: "pre",
    async resolveId(source, importer, options) {
      const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
      const target = resolved && overrides[resolved.id];
      return target ? { id: target } : null;
    },
  };
}

function sharedDepsPlugin(): Plugin {
  const anchor = here("src/main.tsx");
  const toolchain = here(".");
  return {
    name: "loyal-shared-deps",
    enforce: "pre",
    async resolveId(source, importer, options) {
      if (source !== "@tanstack/react-router" || !importer || importer.startsWith(toolchain)) return null;
      return this.resolve(source, anchor, { ...options, skipSelf: true });
    },
  };
}

export function nextSpa(options: NextSpaOptions): UserConfig {
  const { nextApp } = options;
  const publicEnv = Object.fromEntries(
    Object.entries(process.env).filter(([key]) => key.startsWith("NEXT_PUBLIC_")),
  );

  const vercelEnv = resolveVercelEnv(process.env);

  const apiOrigin = process.env.LOYAL_WEB_ORIGIN || "http://127.0.0.1:8080";

  return {
    plugins: [
      ...(options.serverActions ? [serverActionsPlugin(options.serverActions)] : []),
      ...(options.serverOnly ? [serverOnlyPlugin(options.serverOnly)] : []),
      overridesPlugin(options.overrides ?? {}),
      sharedDepsPlugin(),
      ...(options.router === false ? [] : [tanstackRouter({ target: "react", autoCodeSplitting: true })]),
      react(),
    ],
    resolve: {
      alias: compat,
      tsconfigPaths: true,
      dedupe: ["react", "react-dom"],
    },
    css: {
      postcss: { plugins: [tailwindcss({ base: nextApp })] },
    },
    define: {
      "process.env": JSON.stringify({ ...publicEnv, VERCEL_ENV: vercelEnv, NODE_ENV: process.env.NODE_ENV ?? "production" }),
    },
    publicDir: `${nextApp}public`,
    build: {
      outDir: "dist",
      // The Solana libraries use Node's Buffer as a global; Next's webpack
      // provides it, so here every free Buffer becomes an import of `buffer`.
      rolldownOptions: { transform: { inject: { Buffer: ["buffer", "Buffer"] } } },
    },
    server: {
      host: "127.0.0.1",
      port: options.devPort,
      strictPort: true,
      proxy: Object.fromEntries(
        options.devProxy.map((prefix) => [prefix, prefix === "/readyz" ? { target: apiOrigin } : { target: apiOrigin, changeOrigin: true }]),
      ),
    },
  };
}
