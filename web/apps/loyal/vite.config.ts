import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { defineConfig } from "vite";

import { WorkspaceSkeleton } from "../web/src/components/wallet-workspace/facelift/workspace-skeleton";

import { nextSpa } from "./vite.preset";

const here = (path: string) => fileURLToPath(new URL(path, import.meta.url));
const nextApp = here("../web/");

const preset = nextSpa({
  nextApp,
  overrides: {
    [`${nextApp}src/features/blog/data.ts`]: here("src/overrides/blog-data.ts"),
  },
  devPort: 5173,
  devProxy: ["/api", "/ingest", "/readyz"],
});

export default defineConfig({
  ...preset,
  build: {
    ...preset.build,
    rolldownOptions: {
      ...preset.build?.rolldownOptions,
      output: {
        // UI module names can match blocker filters; optional trackers stay blockable.
        chunkFileNames: (chunk) =>
          /^(x-pixel|mixpanel\.|datadog)/.test(chunk.name)
            ? "assets/[name]-[hash].js"
            : "assets/[hash].js",
      },
    },
  },
  plugins: [
    ...(preset.plugins ?? []),
    {
      name: "loyal-app-skeleton",
      transformIndexHtml(html) {
        const css = readFileSync(
          `${nextApp}src/components/wallet-workspace/facelift/workspace-skeleton.css`,
          "utf8"
        );
        return html
          .replace("<!-- app-loading-styles -->", `<style>${css}</style>`)
          .replace(
            "<!-- app-loading-markup -->",
            renderToStaticMarkup(createElement(WorkspaceSkeleton))
          );
      },
    },
  ],
});
