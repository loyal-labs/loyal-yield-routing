import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

import { Keypair } from "@solana/web3.js";

import {
  rwaMultiplyStrategyTwoTarget,
  type StrategyTwoIdentity,
} from "../domain/rwa-multiply-strategy2-route-spec.js";
import { deriveRwaMultiplyVoltrAccounts } from "../integrations/rwa-multiply-voltr.js";
import { reconcileStrategyTwoBootstrap } from "./rwa-multiply-strategy2-bootstrap-reconcile.js";

test("pending bootstrap journal reconciles a landed account without a second send", async () => {
  const identity: StrategyTwoIdentity = {
    config: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["config"],
    delegatedSigner: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["delegatedSigner"],
  };
  const route = (await rwaMultiplyStrategyTwoTarget(identity, 140n)).route;
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const directory = mkdtempSync("/tmp/rwa-multiply-bootstrap-reconcile-");
  const journal = join(directory, "wire-a.json");
  let sendAttempts = 0;
  try {
    writeFileSync(`${journal}.pending`, JSON.stringify({
      schema: "loyal-rwa-multiply-strategy-two-bootstrap-wire/v1",
      phase: "A",
      config: route.customAdaptor.strategyConfig,
      transaction: {
        expectedSignature: "already-finalized-signature",
        wireSha256: "a".repeat(64),
        configSha256Before: null,
        vaultSha256Before: "b".repeat(64),
      },
    }));
    const connection = {
      getSignatureStatuses: async () => ({
        context: { slot: 22 },
        value: [{ err: null, confirmationStatus: "finalized", slot: 21 }],
      }),
      getMultipleAccountsInfo: async () => [
        { owner: route.customAdaptor.program, data: Buffer.alloc(472, 7) },
        null,
        null,
        { owner: route.programs.voltr, data: Buffer.from([8]) },
      ],
      sendRawTransaction: async () => {
        sendAttempts += 1;
        throw new Error("reconciliation must not send");
      },
    } as unknown as Parameters<typeof reconcileStrategyTwoBootstrap>[0]["connection"];

    const result = await reconcileStrategyTwoBootstrap({
      connection,
      route,
      accounts,
      phase: "A",
      journal,
    });

    assert.equal(result.signature, "already-finalized-signature");
    assert.equal(result.finalizedSlot, 21);
    assert.equal(result.finalizedContextSlot, 22);
    assert.equal(sendAttempts, 0);
    assert.equal(JSON.parse(readFileSync(journal, "utf8")).verdict, "FINALIZED_RECONCILED");
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
