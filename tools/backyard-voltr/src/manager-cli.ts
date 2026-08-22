import { writeFileSync } from "node:fs";
import { resolve } from "node:path";

import type { PartnerStrategyId } from "./domain/route-spec.js";
import {
  executeManagerOperation,
  reconcileConfirmedManagerOperation,
  simulateManagerOperation,
  type ManagerOperation,
} from "./runtime/manager.js";

function valueAfter(flag: string): string | null {
  const index = process.argv.indexOf(flag);
  return index < 0 ? null : process.argv[index + 1] ?? null;
}

function strategyId(): PartnerStrategyId {
  const value = valueAfter("--strategy-id");
  if (value !== "main" && value !== "onre" && value !== "prime" && value !== "maple") {
    throw new Error("--strategy-id must be main|onre|prime|maple");
  }
  return value;
}

function managerOperation(): ManagerOperation {
  const value = valueAfter("--operation");
  if (value !== "deposit" && value !== "withdraw") {
    throw new Error("--operation must be deposit|withdraw");
  }
  return value;
}

function amountRaw(): bigint {
  const value = valueAfter("--amount-raw");
  if (value === null) throw new Error("--amount-raw is required");
  try {
    return BigInt(value);
  } catch {
    throw new Error("--amount-raw must be an integer");
  }
}

function json(value: unknown): string {
  return `${JSON.stringify(value, (_key, entry) => {
    if (typeof entry === "bigint") return entry.toString();
    if (entry instanceof Uint8Array) return Buffer.from(entry).toString("base64");
    return entry;
  }, 2)}\n`;
}

async function main(): Promise<void> {
  const command = process.argv[2];
  let result: unknown;
  if (command === "simulate") {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("simulate requires --artifact");
    result = await simulateManagerOperation(strategyId(), managerOperation(), amountRaw(), artifact);
  } else if (command === "execute") {
    const artifact = valueAfter("--artifact");
    if (!artifact) throw new Error("execute requires --artifact");
    result = await executeManagerOperation({
      strategyId: strategyId(),
      operation: managerOperation(),
      amountRaw: amountRaw(),
      artifactPath: artifact,
      authorizationPath: valueAfter("--authorization"),
      confirmAuthorizationSha256: valueAfter("--confirm-authorization-sha256"),
      confirmRouteAuthorizationSha256: valueAfter("--confirm-route-authorization-sha256"),
      lifecycleId: valueAfter("--lifecycle-id"),
      confirmVault: valueAfter("--confirm-vault"),
      confirmArtifactSha256: valueAfter("--confirm-artifact-sha256"),
      confirmAmountRaw: valueAfter("--confirm-amount-raw"),
      confirmWrapperDataSha256: valueAfter("--confirm-wrapper-data-sha256"),
      intentPath: valueAfter("--intent-path"),
    });
  } else if (command === "reconcile") {
    result = await reconcileConfirmedManagerOperation({
      strategyId: strategyId(),
      operation: managerOperation(),
      signature: valueAfter("--signature") ?? "",
    });
  } else {
    throw new Error("manager CLI requires simulate|execute|reconcile");
  }

  const serialized = json(result);
  const output = valueAfter("--out");
  if (output) {
    const path = resolve(output);
    writeFileSync(path, serialized, { mode: 0o600 });
    process.stdout.write(json({
      wrote: path,
      verdict: (result as { verdict?: string }).verdict ?? "OUTPUT_WRITTEN",
    }));
  } else {
    process.stdout.write(serialized);
  }
}

main().catch((error) => {
  process.stderr.write(json({
    verdict: "ERROR",
    broadcast: false,
    error: error instanceof Error ? error.message : String(error),
  }));
  process.exitCode = 1;
});
