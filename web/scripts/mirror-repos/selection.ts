import { mirrors } from "./config";
import type { MirrorConfig } from "./config";

const generatedAppMirrorKinds = new Set([
  "next-app",
  "telegram-app",
  "expo-app",
  "wxt-extension",
]);

const generatorPaths = ["scripts/mirror-repos"];

const ignoredRootFiles = new Set([
  "bun.lock",
  "package-lock.json",
  "pnpm-lock.yaml",
  "yarn.lock",
]);

function hasPathPrefix(filePath: string, prefix: string): boolean {
  return filePath === prefix || filePath.startsWith(`${prefix}/`);
}

function addGeneratedAppMirrors(selected: Set<MirrorConfig>): void {
  for (const mirror of mirrors) {
    if (generatedAppMirrorKinds.has(mirror.kind)) {
      selected.add(mirror);
    }
  }
}

function addMirrorByRepo(
  selected: Set<MirrorConfig>,
  repo: MirrorConfig["repo"]
): void {
  const mirror = mirrors.find((candidate) => candidate.repo === repo);
  if (!mirror) {
    throw new Error(`Unknown mirror repo ${repo}`);
  }
  selected.add(mirror);
}

export function selectMirrorsForChangedPaths(
  changedPaths: string[]
): MirrorConfig[] {
  const selected = new Set<MirrorConfig>();

  for (const changedPath of changedPaths) {
    if (changedPath === ".github/workflows/sync-mirror-repos.yml") {
      return [...mirrors];
    }
    // Git diff paths are repository-relative even when invoked from web/.
    if (!changedPath.startsWith("web/")) continue;
    const filePath = changedPath.slice("web/".length);
    if (ignoredRootFiles.has(filePath)) {
      continue;
    }

    if (generatorPaths.some((prefix) => hasPathPrefix(filePath, prefix))) {
      return [...mirrors];
    }

    if (hasPathPrefix(filePath, "packages")) {
      addGeneratedAppMirrors(selected);
      addMirrorByRepo(selected, "loyal-labs/loyal-packages");
      continue;
    }

    if (hasPathPrefix(filePath, "sdk")) {
      addGeneratedAppMirrors(selected);
      addMirrorByRepo(selected, "loyal-labs/loyal-sdk");
      continue;
    }

    for (const mirror of mirrors) {
      if (hasPathPrefix(filePath, mirror.source)) {
        selected.add(mirror);
      }
    }
  }

  return mirrors.filter((mirror) => selected.has(mirror));
}
