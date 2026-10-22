import { execSync } from "node:child_process";

export function resolveReleaseVersion(): string {
  return (
    process.env.VERCEL_GIT_COMMIT_SHA?.slice(0, 7) ||
    execSync("git rev-parse --short HEAD").toString().trim()
  );
}
