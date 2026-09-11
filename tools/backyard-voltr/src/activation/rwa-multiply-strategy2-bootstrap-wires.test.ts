import assert from "node:assert/strict";
import { test } from "node:test";

import { Buffer } from "node:buffer";

import { AccountRole } from "@solana/kit";
import { Keypair } from "@solana/web3.js";

import {
  rwaMultiplyStrategyTwoTarget,
  type StrategyTwoIdentity,
} from "../domain/rwa-multiply-strategy2-route-spec.js";
import {
  initializeRemainingAccounts,
  RWA_ADAPTOR_DISCRIMINATORS,
} from "../integrations/rwa-multiply-voltr.js";
import { STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE } from "../policies/rwa-multiply-strategy2-seed-journal.js";
import { buildBootstrapWires } from "./rwa-multiply-strategy2-bootstrap-wires.js";

function throwawayIdentity(): StrategyTwoIdentity {
  return {
    config: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["config"],
    delegatedSigner: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["delegatedSigner"],
  };
}

function isSigner(role: AccountRole): boolean {
  return role === AccountRole.READONLY_SIGNER || role === AccountRole.WRITABLE_SIGNER;
}

/**
 * Wire C's initializeStrategy must not ask the Squads vault PDA (ST999) to
 * sign: the adaptor's initialize handler requires only accounts[1], the Voltr
 * manager, to sign and validates the remaining accounts by key. Appending the
 * vault as READONLY_SIGNER made the signed pre-send simulation of wire C fail
 * with SignatureFailure, so the appended six are pinned to the shared
 * read-only helper and the only signing key on the wire is the setup admin.
 */
test("wire C initializeStrategy signs only with the admin/manager key and appends the squads vault read-only",
  { timeout: 120_000 },
  async () => {
  const { route } = await rwaMultiplyStrategyTwoTarget(
    throwawayIdentity(), STRATEGY_TWO_FIRST_POLICY_SEED_BEFORE);
  const { wireC } = await buildBootstrapWires(route);
  const [custodyAta, handoffToSettingsSigner, initializeStrategy, restoreManager] = wireC;

  assert.equal(initializeStrategy.programAddress, route.programs.voltr);
  assert.ok(Buffer.from(initializeStrategy.data ?? [])
    .indexOf(Buffer.from(RWA_ADAPTOR_DISCRIMINATORS.initialize)) >= 0,
  "the initializeStrategy instruction does not carry the adaptor initialize discriminator");

  const accounts = initializeStrategy.accounts ?? [];
  // Order and roles of the appended remaining accounts match the shared helper
  // exactly, so no appended account is a signer.
  assert.deepEqual(
    accounts.slice(-6).map((row) => ({ address: row.address, role: row.role })),
    initializeRemainingAccounts(route).map((row) => ({ address: row.address, role: row.role })),
  );

  // Exactly two signers, both the same key (BAqg): the Voltr payer/admin and
  // the Voltr manager the handoff instruction installed just before this call.
  const signers = accounts.filter((row) => isSigner(row.role));
  assert.deepEqual(signers.map((row) => ({ address: row.address, role: row.role })), [
    { address: route.setupAdmin, role: AccountRole.WRITABLE_SIGNER },
    { address: route.customAdaptor.settingsSigner, role: AccountRole.READONLY_SIGNER },
  ]);
  assert.equal(signers[0]!.address, signers[1]!.address);

  const vaultRows = accounts.filter((row) => row.address === route.squads.vault);
  assert.equal(vaultRows.length, 1);
  assert.equal(vaultRows[0]!.role, AccountRole.READONLY);

  // No instruction on wire C asks any key but the setup admin to sign, so the
  // operator send path needs no additionalSigners beyond the fee payer.
  for (const instruction of [custodyAta, handoffToSettingsSigner, initializeStrategy, restoreManager]) {
    for (const row of instruction.accounts ?? []) {
      if (isSigner(row.role)) assert.equal(row.address, route.setupAdmin);
    }
  }
});
