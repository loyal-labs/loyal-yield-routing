import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/postcss";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";

// This app renders apps/web's components unchanged; apps/web stays the Next
// app Vercel serves. Everything Next-specific those components import is
// answered here: next/* by src/next-compat, Node-only modules by src/overrides.
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

// apps/web modules that read the filesystem at request time, keyed by their
// absolute path, replaced by a build-time equivalent.
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

// Next inlines NEXT_PUBLIC_* into client code and nothing else.
const publicEnv = Object.fromEntries(
  Object.entries(process.env).filter(([key]) => key.startsWith("NEXT_PUBLIC_")),
);

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
    "process.env": JSON.stringify({ ...publicEnv, NODE_ENV: process.env.NODE_ENV ?? "production" }),
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
