import { fileURLToPath } from "node:url";

import { defineConfig } from "vite";

import { nextSpa } from "./vite.preset";

const here = (path: string) => fileURLToPath(new URL(path, import.meta.url));
const nextApp = here("../web/");

export default defineConfig(
  nextSpa({
    nextApp,
    overrides: { [`${nextApp}src/features/blog/data.ts`]: here("src/overrides/blog-data.ts") },
    devPort: 5173,
    devProxy: ["/api", "/ingest", "/readyz"],
  }),
);
