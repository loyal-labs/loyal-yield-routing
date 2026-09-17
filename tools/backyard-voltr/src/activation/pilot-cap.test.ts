import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { gunzipSync } from "node:zlib";
import { createNoopSigner, address } from "@solana/kit";
import { getVaultDecoder, getVaultEncoder, getUpdateVaultConfigInstructionDataDecoder, VaultConfigField } from "@voltr/vault-sdk";
import { PILOT_CAP_RAW, pilotCapInstruction, verifyPilotCapChange } from "./pilot-cap.js";
import type { AccountSnapshot } from "../integrations/solana-compat.js";

const evidence = JSON.parse(gunzipSync(readFileSync(new URL("../../../../docs/evidence/voltr-selector-2026-09-16/pilot-flat-finalized-447471906.json.gz", import.meta.url))).toString());
const raw = evidence.evidence.accounts.find((a: { Address: string }) => a.Address === "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA");
const before: AccountSnapshot = { address: raw.Address, owner: raw.Owner, lamports: raw.Lamports, executable: raw.Executable, data: Buffer.from(raw.Data, "base64") };
const decoded = getVaultDecoder().decode(before.data);
const capped = { ...decoded, lastUpdatedTs: decoded.lastUpdatedTs + 1n, vaultConfiguration: { ...decoded.vaultConfiguration, maxCap: PILOT_CAP_RAW } };
const after = { ...before, data: Uint8Array.from(getVaultEncoder().encode(capped)) };

test("cap-only update preserves the entire captured vault state", () => {
  expect(() => verifyPilotCapChange(before, after)).not.toThrow();
  for (const data of [
    getVaultEncoder().encode({ ...capped, feeConfiguration: { ...capped.feeConfiguration, adminPerformanceFee: capped.feeConfiguration.adminPerformanceFee + 1 } }),
    getVaultEncoder().encode({ ...capped, highWaterMark: { ...capped.highWaterMark, highestAssetPerLpDecimalBits: capped.highWaterMark.highestAssetPerLpDecimalBits + 1n } }),
    getVaultEncoder().encode({ ...capped, deadWeight: capped.deadWeight + 1n }),
    getVaultEncoder().encode({ ...capped, asset: { ...capped.asset, totalValue: capped.asset.totalValue + 1n } }),
  ]) expect(() => verifyPilotCapChange(before, { ...after, data: Uint8Array.from(data) })).toThrow();
  expect(() => verifyPilotCapChange(before, { ...after, lamports: after.lamports - 1 })).toThrow();
  expect(() => verifyPilotCapChange(before, before)).toThrow();
  expect(() => verifyPilotCapChange(before, { ...after, data: Uint8Array.from(getVaultEncoder().encode({ ...capped, lastUpdatedTs: decoded.lastUpdatedTs - 1n })) })).toThrow();
  const over = { ...before, data: Uint8Array.from(getVaultEncoder().encode({ ...decoded, asset: { ...decoded.asset, totalValue: PILOT_CAP_RAW + 1n } })) };
  expect(() => verifyPilotCapChange(over, after)).toThrow();
});

test("cap instruction uses only approved authority and MaxCap field", async () => {
  const admin = createNoopSigner(address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"));
  const ix = await pilotCapInstruction(admin);
  const data = getUpdateVaultConfigInstructionDataDecoder().decode(ix.data!);
  expect(data.field).toBe(VaultConfigField.MaxCap);
  expect(Buffer.from(data.data).readBigUInt64LE()).toBe(PILOT_CAP_RAW);
  expect(ix.accounts?.filter(a => a.address === admin.address)).toHaveLength(1);
  await expect(pilotCapInstruction(createNoopSigner(address(before.address)))).rejects.toThrow();
});

test("an existing attempt is reconciled without preparing or sending again", async () => {
  const { mkdtempSync, writeFileSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");
  const { updatePilotCap } = await import("./pilot-cap.js");
  const dir = mkdtempSync(join(tmpdir(), "pilot-cap-recovery-"));
  const journal = join(dir, "attempt.json");
  const { PublicKey, TransactionMessage, VersionedTransaction } = await import("@solana/web3.js");
  const { toWeb3Instruction } = await import("../integrations/solana-compat.js");
  const { createHash } = await import("node:crypto");
  const { default: bs58 } = await import("bs58");
  const admin = address("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ");
  const message = new TransactionMessage({ payerKey: new PublicKey(admin), recentBlockhash: new PublicKey(new Uint8Array(32).fill(7)).toBase58(),
    instructions: [toWeb3Instruction(await pilotCapInstruction(createNoopSigner(admin)))] }).compileToV0Message();
  const wireTx = new VersionedTransaction(message);
  wireTx.signatures[0] = new Uint8Array(64).fill(1);
  const wire = wireTx.serialize();
  const expectedSignature = bs58.encode(wireTx.signatures[0]!);
  writeFileSync(journal, JSON.stringify({ schema: "pilot-cap-attempt/1", vault: before.address,
    capRaw: PILOT_CAP_RAW.toString(), expectedSignature, wireBase64: Buffer.from(wire).toString("base64"), wireSha256: createHash("sha256").update(wire).digest("hex") }));
  const methods: string[] = [];
  let failStatus = false;
  const server = Bun.serve({ port: 0, async fetch(req) {
    const call = await req.json() as { id: number; method: string };
    methods.push(call.method);
    const result = call.method === "getGenesisHash" ? "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
      : { context: { slot: 1 }, value: [failStatus ? { slot: 1, confirmations: null, confirmationStatus: "finalized", err: { InstructionError: [0, "InvalidArgument"] } } : null] };
    return Response.json({ jsonrpc: "2.0", id: call.id, result });
  }});
  const oldRpc = process.env.SOLANA_RPC_URL; const oldConfirm = process.env.CONFIRM_MAINNET;
  try {
    process.env.SOLANA_RPC_URL = `http://127.0.0.1:${server.port}`; process.env.CONFIRM_MAINNET = "1";
    expect(await updatePilotCap(true, journal)).toMatchObject({ broadcast: "unknown", expectedSignature });
    failStatus = true;
    expect(await updatePilotCap(true, journal)).toMatchObject({ verdict: "PILOT_CAP_ATTEMPT_FAILED", broadcast: true, expectedSignature });
    expect(methods).toEqual(["getGenesisHash", "getSignatureStatuses", "getGenesisHash", "getSignatureStatuses"]);
  } finally {
    if (oldRpc === undefined) delete process.env.SOLANA_RPC_URL; else process.env.SOLANA_RPC_URL = oldRpc;
    if (oldConfirm === undefined) delete process.env.CONFIRM_MAINNET; else process.env.CONFIRM_MAINNET = oldConfirm;
    server.stop(true); rmSync(dir, { recursive: true });
  }
});
