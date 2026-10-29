import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(
  new URL("../../web/apps/web/package.json", import.meta.url)
);
const { chromium } = require("@playwright/test");
const html = readFileSync(
  new URL("../../web/apps/loyal/index.html", import.meta.url),
  "utf8"
);
const browser = await chromium.launch({
  ...(process.env.CHROMIUM_PATH
    ? { executablePath: process.env.CHROMIUM_PATH }
    : {}),
});
try {
  for (const scenario of [
    "new-release",
    "still-missing",
    "storage-denied",
    "running-wallet",
    "blocked-analytics",
  ]) {
    const context = await browser.newContext();
    const page = await context.newPage();
    let documents = 0;
    let releaseApp;
    const analyticsBlocked = new Promise((resolve) => {
      releaseApp = resolve;
    });
    if (scenario === "blocked-analytics") {
      await page.route("https://static.cloudflareinsights.com/**", async (route) => {
        await route.abort("blockedbyclient");
      });
      await page.exposeFunction("analyticsFailed", releaseApp);
    }
    if (scenario === "storage-denied") {
      await page.addInitScript(() => {
        Object.defineProperty(window, "sessionStorage", {
          get() {
            throw new Error("denied");
          },
        });
      });
    }
    await page.route("http://loyal.test/**", async (route) => {
      const path = new URL(route.request().url()).pathname;
      if (path === "/app") {
        documents++;
        const entry =
          scenario === "running-wallet" ||
          scenario === "blocked-analytics" ||
          (scenario === "new-release" && documents > 1)
            ? "current"
            : "missing";
        await route.fulfill({
          contentType: "text/html",
          body: html
            .replace("/src/main.tsx", `/assets/${entry}.js`)
            .replace(
              "</head>",
              scenario === "blocked-analytics"
                ? '<script type="module" src="https://static.cloudflareinsights.com/beacon.min.js" onerror="analyticsFailed()"></script></head>'
                : "</head>"
            ),
        });
      } else if (path === "/assets/current.js") {
        if (scenario === "blocked-analytics") await analyticsBlocked;
        await route.fulfill({
          contentType: "text/javascript",
          body: 'document.getElementById("root").innerHTML = "<input aria-label=Amount value=42><p>Wallet loaded</p>";',
        });
      } else {
        await route.fulfill({
          status: 404,
          contentType: "text/plain",
          body: "not found",
        });
      }
    });
    await page.goto("http://loyal.test/app");
    if (
      scenario === "new-release" ||
      scenario === "running-wallet" ||
      scenario === "blocked-analytics"
    ) {
      await page.getByText("Wallet loaded").waitFor();
      assert.equal(documents, scenario === "new-release" ? 2 : 1);
      await page.evaluate(() => {
        window.dispatchEvent(new Event("vite:preloadError"));
        const script = document.createElement("script");
        script.type = "module";
        script.src = "/assets/missing-later.js";
        document.body.append(script);
      });
      await page.waitForTimeout(300);
      assert.equal(documents, scenario === "new-release" ? 2 : 1);
      assert.equal(await page.getByLabel("Amount").inputValue(), "42");
      assert.equal(await page.locator("#app-load-error").isVisible(), false);
    } else {
      await page.getByRole("alert").waitFor();
      await page.waitForTimeout(300);
      assert.equal(documents, scenario === "storage-denied" ? 1 : 2);
      assert.equal(
        await page.getByRole("link", { name: "Reload app" }).isVisible(),
        true
      );
    }
    console.log(`${scenario}: passed`);
    await context.close();
  }
} finally {
  await browser.close();
}
