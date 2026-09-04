import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { generated as squadsGenerated } from "@loyal-labs/loyal-smart-accounts-core";
import { Connection, PublicKey, type AccountInfo } from "@solana/web3.js";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

type Json = Record<string, unknown>;
type SettingsState = { policySeed: { toString(): string } | null };

const ROOT = resolve(fileURLToPath(new URL("../../../..", import.meta.url)));
const MANIFEST = "docs/manifests/backyard-rwa-v1.json";
const SELECTION = "docs/evidence/backyard-rwa-go/phase2-runtime/selection-v1.json";
const FARM_ROLLOVER = "docs/evidence/backyard-rwa-go/phase2-runtime/maple-farm-policy-rollover-2026-09-03.md";
const OUTPUT = "docs/evidence/backyard-rwa-go/phase2-runtime/current-policy-rollovers-v1.json";
const Settings = (squadsGenerated as unknown as {
  Settings: { fromAccountInfo(info: AccountInfo<Buffer>): readonly [SettingsState, number] };
}).Settings;
const sha256 = (value: Uint8Array | string) => createHash("sha256").update(value).digest("hex");
const fileSha256 = (relative: string) => sha256(readFileSync(resolve(ROOT, relative)));
function object(value: unknown, label: string): Json {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} is not an object`);
  return value as Json;
}

async function main() {
  if (existsSync(resolve(ROOT, OUTPUT))) throw new Error(`${OUTPUT} already exists; current rollover evidence is immutable`);
  const rpcUrl = process.env.SOLANA_RPC_URL?.trim();
  if (!rpcUrl) throw new Error("SOLANA_RPC_URL is required");
  const connection = new Connection(rpcUrl, "finalized");
  if (await connection.getGenesisHash() !== RWA_MULTIPLY_ROUTE.genesisHash) throw new Error("RPC is not mainnet-beta");

  const manifest = object(JSON.parse(readFileSync(resolve(ROOT, MANIFEST), "utf8")), "manifest");
  const activation = object(manifest.runtimeActivation, "runtimeActivation");
  const binding = object(activation.selectedLaneBinding, "selectedLaneBinding");
  const kamino = (binding.kaminoPolicies as unknown[]).map((value) => object(value, "Kamino policy"));
  const jupiter = (binding.jupiterEdges as unknown[]).map((value) => object(value, "Jupiter policy"));
  const rolled = [
    kamino.find((value) => value.operation === "borrow"),
    kamino.find((value) => value.operation === "repay"),
    jupiter.find((value) => value.edge === "syrupUSDC->USDC"),
  ];
  if (rolled.some((value) => value === undefined)) throw new Error("manifest lacks the three forward rollover bindings");
  const policies = rolled as Json[];
  if (policies.map((value) => String(value.seed)).join(",") !== "137,138,139") throw new Error("forward rollover seeds are not exactly 137,138,139");

  const addresses = [RWA_MULTIPLY_ROUTE.squads.settings, ...policies.map((value) => String(value.policy))].map((value) => new PublicKey(value));
  const readback = await connection.getMultipleAccountsInfoAndContext(addresses, { commitment: "finalized" });
  const [settingsInfo, ...policyInfos] = readback.value;
  if (settingsInfo?.owner.toBase58() !== RWA_MULTIPLY_ROUTE.squads.program) throw new Error("Settings owner is not Squads");
  const settingsSeed = Settings.fromAccountInfo(settingsInfo)[0].policySeed?.toString() ?? "0";
  if (settingsSeed !== "139") throw new Error(`finalized Settings seed is ${settingsSeed}, expected 139`);
  const rows = policies.map((policy, index) => {
    const info = policyInfos[index];
    if (info?.owner.toBase58() !== RWA_MULTIPLY_ROUTE.squads.program) throw new Error(`policy seed ${String(policy.seed)} is absent or not Squads-owned`);
    const liveAccountDataSha256 = sha256(info.data);
    if (liveAccountDataSha256 !== policy.liveAccountDataSha256) throw new Error(`policy seed ${String(policy.seed)} data hash drifted`);
    return { binding: policy, owner: info.owner.toBase58(), dataLength: info.data.length, liveAccountDataSha256 };
  });
  const evidence = {
    schema: "loyal-backyard-rwa-phase2-current-policy-rollovers/v1",
    verdict: "PASS",
    broadcast: false,
    cluster: "mainnet-beta",
    commitment: "finalized",
    selectedLane: activation.selectedLane,
    selection: { path: SELECTION, sha256: fileSha256(SELECTION), role: "identity-valid original selection; not replayed" },
    settings: { address: RWA_MULTIPLY_ROUTE.squads.settings, owner: settingsInfo.owner.toBase58(), policySeed: settingsSeed },
    readbackSlot: readback.context.slot,
    policies: rows,
    sources: [
      { path: FARM_ROLLOVER, sha256: fileSha256(FARM_ROLLOVER), seeds: ["137", "138"] },
      { path: "crates/loyal-actions/src/bin/compile_backyard_rwa_maple_exit_policy.rs", sha256: fileSha256("crates/loyal-actions/src/bin/compile_backyard_rwa_maple_exit_policy.rs"), seeds: ["139"] },
    ],
  };
  mkdirSync(dirname(resolve(ROOT, OUTPUT)), { recursive: true });
  writeFileSync(resolve(ROOT, OUTPUT), `${JSON.stringify(evidence, null, 2)}\n`, { flag: "wx", mode: 0o600 });
  console.log(JSON.stringify({ verdict: "PASS", output: OUTPUT, readbackSlot: readback.context.slot, settingsSeed }));
}

await main();
