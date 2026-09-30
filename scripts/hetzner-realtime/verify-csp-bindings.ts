import { pathToFileURL } from "node:url";
import { resolve } from "node:path";

// Configuration probe only; intentionally does not build/render the frontend.
const clientRoot = resolve(process.argv[2] ?? "../hetzner-client");
const capturedUrl = "https://build.fixture.invalid/events";
const runtimeUrl = "https://runtime.fixture.invalid/events";
process.env.REALTIME_EVENTS_URL = capturedUrl;
const { default: config } = await import(pathToFileURL(`${clientRoot}/apps/web/next.config.ts`).href);
const { resolveEarnRealtimeEventsUrl } = await import(pathToFileURL(`${clientRoot}/apps/web/src/lib/core/config/earn-realtime.ts`).href);
const headers = JSON.stringify(await config.headers());
process.env.REALTIME_EVENTS_URL = runtimeUrl;
const runtime = resolveEarnRealtimeEventsUrl(process.env, "prod");
if (!headers.includes("https://build.fixture.invalid") ||
    headers.includes("https://runtime.fixture.invalid") ||
    !headers.includes("https://loyal-yield-realtime.onrender.com") ||
    !headers.includes('Content-Security-Policy-Report-Only') || runtime !== runtimeUrl) {
  throw new Error("CSP configuration binding contract failed");
}
console.log(JSON.stringify({
  scope: "realtime_csp_configuration",
  service_id: "srv-d966hcpkh4rs73da0j4g",
  collected_at: new Date().toISOString(),
  source_identity: "a6f15023325fd157c436167b6f477cd3b7565b2e",
  target_identity: "local-uncommitted-patch",
  measurements: { captured_origin: new URL(capturedUrl).origin, runtime_origin: new URL(runtime).origin,
    render_origin_retained: true, policy_header: "Content-Security-Policy-Report-Only" },
  verdict: "PASS",
  limitations: ["Config module capture only; no Next build, HTTP response, browser, CSP enforcement or client delivery proof.",
    "Runtime origin changes require matching build binding and redeployment."],
}));
