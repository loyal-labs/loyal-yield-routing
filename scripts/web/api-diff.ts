import { isDeepStrictEqual } from "node:util";

const [base, reference] = process.argv.slice(2);
if (!base || !reference) throw new Error("Usage: bun scripts/web/api-diff.ts <Go origin> <Next origin>");

async function read(origin: string, nonce: string) {
  const url = new URL(`/api/earn/stats?migration-check=${nonce}`, origin);
  const response = await fetch(url, {
    redirect: "error",
    signal: AbortSignal.timeout(15_000),
    headers: { "Cache-Control": "no-cache" },
  });
  if (!response.ok) throw new Error(`${url.origin}: HTTP ${response.status}`);
  return { response, body: await response.json() };
}

let matched = false;
for (let attempt = 0; attempt < 3; attempt++) {
  const nonce = `${Date.now()}-${attempt}`;
  const [go, next] = await Promise.all([read(base, nonce), read(reference, nonce)]);
  if (go.response.headers.get("X-Loyal-API-Source") !== "go") {
    throw new Error("The test host is still proxying stats; install WEB_APP_DATABASE_URL before comparing.");
  }
  if (typeof go.body.refreshedAt !== "string" || typeof next.body.refreshedAt !== "string") {
    throw new Error("Stats response is missing refreshedAt");
  }
  if (go.body.refreshedAt !== next.body.refreshedAt) continue;
  if (!isDeepStrictEqual(go.body, next.body)) {
    throw new Error(`Stats JSON differs for snapshot ${go.body.refreshedAt}`);
  }
  if (go.response.headers.get("Cache-Control")?.split(",")[0] !== "public") {
    throw new Error("Go stats response is missing public cache control");
  }
  console.log(`PASS /api/earn/stats: equal JSON at ${go.body.refreshedAt}; served by Go`);
  matched = true;
  break;
}
if (!matched) throw new Error("No matching snapshot timestamp in three reads; parity remains unverified.");
