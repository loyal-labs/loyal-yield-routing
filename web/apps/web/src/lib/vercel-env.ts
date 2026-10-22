export function resolveVercelEnv(
  env: Readonly<Record<string, string | undefined>>
): string {
  return (
    env.VERCEL_ENV ??
    ((env.NEXT_PUBLIC_APP_ENVIRONMENT ?? "prod") === "prod"
      ? "production"
      : "development")
  );
}

export function faviconForEnv(vercelEnv: string | undefined): string {
  return vercelEnv === "production"
    ? "/icon.svg"
    : vercelEnv === "preview"
      ? "/icon-preview.svg"
      : "/icon-dev.svg";
}
