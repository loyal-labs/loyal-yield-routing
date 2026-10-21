import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/postcss";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";

const here = (path: string) => fileURLToPath(new URL(path, import.meta.url));
const nextApp = here("../web/");

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

const overrides: Record<string, string> = {
  [`${nextApp}src/features/blog/data.ts`]: here("src/overrides/blog-data.ts"),
};

function overridesPlugin(): Plugin {
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

const publicEnv = Object.fromEntries(
  Object.entries(process.env).filter(([key]) => key.startsWith("NEXT_PUBLIC_")),
);

// layout.tsx tints the favicon by VERCEL_ENV, which Next reads on the server.
// Here the build stands in for the server: a prod build is Vercel's production.
const vercelEnv =
  process.env.VERCEL_ENV ??
  ((process.env.NEXT_PUBLIC_APP_ENVIRONMENT ?? "prod") === "prod" ? "production" : "development");

const apiOrigin = process.env.LOYAL_WEB_ORIGIN || "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [
    overridesPlugin(),
    tanstackRouter({ target: "react", autoCodeSplitting: true }),
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
  publicDir: here("../web/public"),
  build: {
    outDir: "dist",
    // The Solana libraries use Node's Buffer as a global; Next's webpack
    // provides it, so here every free Buffer becomes an import of `buffer`.
    rolldownOptions: { transform: { inject: { Buffer: ["buffer", "Buffer"] } } },
  },
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    proxy: {
      "/api": { target: apiOrigin, changeOrigin: true },
      "/ingest": { target: apiOrigin, changeOrigin: true },
      "/readyz": { target: apiOrigin },
    },
  },
});
