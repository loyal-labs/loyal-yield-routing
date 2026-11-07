// Everything named here ships in public JS. Next inlined only the NEXT_PUBLIC_* names the code read;
// copying every one from the build environment published a Helius key in the bundle (Oct-Nov 2026).
const PUBLIC_ENV_NAMES = [
  "NEXT_PUBLIC_APP_ENVIRONMENT",
  "NEXT_PUBLIC_APP_URL",
  "NEXT_PUBLIC_DASHBOARD_URL",
  "NEXT_PUBLIC_DATADOG_APPLICATION_ID",
  "NEXT_PUBLIC_DATADOG_CLIENT_TOKEN",
  "NEXT_PUBLIC_DEMO_RECIPE",
  "NEXT_PUBLIC_FLAGS_MANIFEST_URL",
  "NEXT_PUBLIC_GIT_BRANCH",
  "NEXT_PUBLIC_GIT_COMMIT_HASH",
  "NEXT_PUBLIC_JUPITER_API_KEY",
  "NEXT_PUBLIC_MIXPANEL_PROXY_PATH",
  "NEXT_PUBLIC_MIXPANEL_TOKEN",
  "NEXT_PUBLIC_PRIVY_APP_ID",
  "NEXT_PUBLIC_SKILLS_ENABLED",
  "NEXT_PUBLIC_SOLANA_ENV",
  "NEXT_PUBLIC_USERCENTRICS_SETTINGS_ID",
] as const;

export function publicEnv(env: Record<string, string | undefined>): Record<string, string> {
  const shipped: Record<string, string> = {};
  for (const name of PUBLIC_ENV_NAMES) {
    const value = env[name];
    if (value !== undefined) shipped[name] = value;
  }
  return shipped;
}
