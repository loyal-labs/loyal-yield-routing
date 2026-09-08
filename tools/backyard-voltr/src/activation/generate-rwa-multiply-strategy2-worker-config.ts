import { chmodSync, existsSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { address } from "@solana/kit";

import { buildStrategyTwoWorkerPolicyConfig } from "../policies/rwa-multiply-strategy2-worker-config.js";
import type { StrategyTwoIdentity } from "../domain/rwa-multiply-strategy2-route-spec.js";

function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function cliValue(flag: string): string {
  const index = process.argv.indexOf(flag);
  const value = index >= 0 ? process.argv[index + 1] ?? "" : "";
  invariant(value.length > 0 && !value.startsWith("--"), `${flag} requires a value`);
  return value;
}

async function main(): Promise<void> {
  const seedJournal = resolve(cliValue("--seed-journal"));
  const output = resolve(cliValue("--output"));
  invariant(seedJournal.endsWith(".json") && existsSync(seedJournal),
    "--seed-journal PATH.json must exist");
  invariant(output.endsWith(".json") && existsSync(dirname(output)),
    "--output PATH.json must be under an existing directory");
  const identity: StrategyTwoIdentity = {
    config: address(cliValue("--config")),
    delegatedSigner: address(cliValue("--delegated-signer")),
  };
  const rendered = await buildStrategyTwoWorkerPolicyConfig(seedJournal, identity);
  writeFileSync(output, `${JSON.stringify(rendered, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  chmodSync(output, 0o600);
  // Only report paths and derived public identities; the seed journal itself
  // contains no key material and no secret values are loaded by this tool.
  console.log(JSON.stringify({
    verdict: "PASS_STRATEGY_TWO_WORKER_CONFIG_GENERATED",
    output,
    source: rendered.source,
    policySeeds: rendered.policies.map(({ seed }) => seed),
  }, null, 2));
}

try {
  await main();
} catch (error) {
  console.error(JSON.stringify({
    verdict: "BLOCKED",
    blocker: error instanceof Error ? error.message : String(error),
  }));
  process.exitCode = 1;
}
