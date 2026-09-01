import { Connection } from "@solana/web3.js";
import { chmodSync, existsSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";

import { compileCurrentPhaseOnePrimeUsdcPolicies } from "../policies/rwa-multiply-phase1-policies.js";

async function main() {
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim() || "https://api.mainnet-beta.solana.com";
  const output = await compileCurrentPhaseOnePrimeUsdcPolicies(new Connection(rpcUrl, "finalized"));
  const outIndex = process.argv.indexOf("--out");
  const bindingsIndex = process.argv.indexOf("--bindings-out");
  const out = outIndex >= 0 ? resolve(process.argv[outIndex + 1] ?? "") : "";
  const bindingsOut = bindingsIndex >= 0 ? resolve(process.argv[bindingsIndex + 1] ?? "") : "";
  for (const path of [out, bindingsOut]) {
    if (!path) continue;
    if (!path.endsWith(".json") || !existsSync(dirname(path))) {
      throw new Error(`output must be a .json file under an existing directory: ${path}`);
    }
    writeFileSync(path, `${JSON.stringify(path === out ? output.compilerInput : output, null, 2)}\n`, {
      flag: "wx",
      mode: 0o600,
    });
    chmodSync(path, 0o600);
  }
  console.log(JSON.stringify(output, null, 2));
}

main().catch((error) => {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
});
