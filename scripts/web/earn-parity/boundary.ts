import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { plugin } from "bun";

import { PublicKey } from "../../../web/node_modules/@solana/web3.js";

import type { RecordedFrom } from "./translate";
import { RPC_ENDPOINT, type RecordedCall } from "./transport";

export const WEB_SRC = resolve(import.meta.dir, "../../../web/apps/web/src");

const STUBBED = [
  "features/identity/server/auth-session",
  "features/smart-accounts/server/service",
  "features/observability/lifecycle.server",
  "lib/core/config/server",
  "lib/solana/rpc-endpoints.server",
  "lib/solana/rpc-rate-limit",
  "lib/yield-optimization/yield-deposit-repository.server",
  "lib/yield-optimization/earn-reserve-target.server",
  "lib/yield-optimization/earn-autodeposit-repository.server",
].map((path) => `${WEB_SRC}/${path}.ts`);

export type Fixture = {
  route: string;
  branch: string;
  processEnv: Record<string, string>;
  recordedFrom?: RecordedFrom;
  request: { body: unknown };
  principal: { walletAddress: string; settingsPda: string; smartAccountAddress: string };
  serverEnv: Record<string, unknown>;
  boundary: Record<string, unknown>;
  transport: RecordedCall[];
};

export function revive(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(revive);
  if (value && typeof value === "object") {
    const tagged = value as Record<string, unknown>;
    if (typeof tagged.$bigint === "string") return BigInt(tagged.$bigint);
    if (typeof tagged.$date === "string") return new Date(tagged.$date);
    if (typeof tagged.$pubkey === "string") return new PublicKey(tagged.$pubkey);
    return Object.fromEntries(Object.entries(tagged).map(([key, inner]) => [key, revive(inner)]));
  }
  return value;
}

function strictEnv(env: Record<string, unknown>): Record<string, unknown> {
  return new Proxy(env, {
    get(target, key) {
      if (typeof key === "symbol" || key === "then" || key in target) return Reflect.get(target, key);
      throw new Error(`earn-parity: fixture serverEnv has no ${key}`);
    },
  });
}

export function installBoundary(fixture: Fixture): void {
  const fixed: Record<string, (...args: unknown[]) => unknown> = {
    resolveAuthenticatedPrincipalFromRequest: async () => fixture.principal,
    assertAuthenticatedWalletControlsSettings: async () => undefined,
    isSmartAccountProvisioningError: () => false,
    getServerEnv: () => strictEnv(fixture.serverEnv),
    getServerSolanaEndpoints: () => ({ rpcEndpoint: RPC_ENDPOINT, websocketEndpoint: "wss://rpc.earn-parity.invalid/" }),
    getFrontendSolanaRpcFetch: (fetchImpl) => fetchImpl,
    withRequestLifecycle: (args) => (args as { run: (lifecycle: null) => unknown }).run(null),
  };
  (globalThis as Record<string, unknown>).__earnParityBoundary = (name: string) =>
    fixed[name] ??
    (async () => {
      if (!(name in fixture.boundary)) throw new Error(`earn-parity: fixture boundary has no ${name}`);
      return revive(fixture.boundary[name]);
    });
  plugin({
    name: "earn-parity-boundary",
    setup(build) {
      build.onLoad({ filter: /\/node_modules\/server-only\/[^/]+\.js$/ }, () => ({ contents: "export {};", loader: "js" }));
      for (const path of STUBBED) {
        build.onLoad({ filter: new RegExp(`^${escapeRegExp(path)}$`) }, () => ({ contents: stubSource(path), loader: "js" }));
      }
    },
  });
}

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&");
}

function stubSource(path: string): string {
  const source = readFileSync(path, "utf8");
  const lines = ["const boundary = globalThis.__earnParityBoundary;"];
  for (const match of source.matchAll(/^export\s+(?:async\s+)?(function\*?|const|let|class)\s+(\w+)(.*)$/gm)) {
    const [, kind, name, rest] = match;
    const literal = kind === "const" && rest.match(/^\s*=\s*(BigInt\([\d_]+\)|[\d_]+n?|"[^"]*");/);
    lines.push(literal ? `export const ${name} = ${literal[1]};` : `export const ${name} = boundary(${JSON.stringify(name)});`);
  }
  for (const match of source.matchAll(/^export\s*\{([^}]*)\}/gm)) {
    for (const entry of match[1].split(",").map((part) => part.trim()).filter((part) => part && !part.startsWith("type "))) {
      const name = entry.split(/\s+as\s+/).pop()!;
      lines.push(`export const ${name} = boundary(${JSON.stringify(name)});`);
    }
  }
  return lines.join("\n");
}
