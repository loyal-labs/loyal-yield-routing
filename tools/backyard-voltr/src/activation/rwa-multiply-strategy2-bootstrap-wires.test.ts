import assert from "node:assert/strict";
import { test } from "node:test";

import { Buffer } from "node:buffer";

import { AccountRole } from "@solana/kit";
import { Keypair } from "@solana/web3.js";
import {
  getVaultDecoder,
  getVaultDiscriminatorBytes,
  getVaultEncoder,
  getVaultSize,
  type Vault,
} from "@voltr/vault-sdk";

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
import {
  decodeVaultNeutralityView,
  VAULT_NEUTRALITY_IGNORED_FIELDS,
  vaultNeutralityVerdict,
  type VaultNeutralityView,
} from "./rwa-multiply-strategy2-vault-neutrality.js";

function throwawayIdentity(): StrategyTwoIdentity {
  return {
    config: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["config"],
    delegatedSigner: Keypair.generate().publicKey.toBase58() as StrategyTwoIdentity["delegatedSigner"],
  };
}

/**
 * A zeroed Vault image with the real discriminator, so every fixture below can
 * be built by decoding it and re-encoding a mutated copy through the SDK codec.
 */
function emptyVaultBytes(): Uint8Array {
  const bytes = new Uint8Array(getVaultSize());
  bytes.set(getVaultDiscriminatorBytes(), 0);
  return bytes;
}

function vaultData(mutate: (vault: Vault) => Partial<Vault>): Uint8Array {
  const decoded = getVaultDecoder().decode(emptyVaultBytes());
  const encoded = getVaultEncoder().encode({ ...decoded, ...mutate(decoded) });
  return new Uint8Array(encoded);
}

function neutralityView(data: Uint8Array): VaultNeutralityView {
  const view = decodeVaultNeutralityView(data);
  assert.ok(view, "fixture vault did not decode");
  return view;
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

/**
 * Voltr stamps lastUpdatedTs on every config write, so raw vault bytes can
 * never survive wire C. The gate instead compares decoded state with that one
 * field removed, and still requires the manager restored to the Squads vault.
 */
test("wire C vault neutrality ignores only lastUpdatedTs and demands the restored manager", () => {
  assert.deepEqual([...VAULT_NEUTRALITY_IGNORED_FIELDS], ["lastUpdatedTs"]);
  const squadsVault = Keypair.generate().publicKey.toBase58();
  const settingsSigner = Keypair.generate().publicKey.toBase58();
  const stamp = 1_700_000_000n;
  const before = neutralityView(vaultData(() => ({ manager: squadsVault, lastUpdatedTs: stamp })));
  const verdictFor = (data: Uint8Array) => vaultNeutralityVerdict({
    before,
    after: neutralityView(data),
    expectedManager: squadsVault,
  });

  // Every non-manager fixture keeps the manager restored so only the mutated
  // field can decide the verdict.
  const restored = (mutate: (vault: Vault) => Partial<Vault>): Uint8Array =>
    vaultData((vault) => ({ manager: squadsVault, lastUpdatedTs: stamp + 100n, ...mutate(vault) }));

  // Only the stamp moved: neutral, whether it advances or holds.
  assert.deepEqual(verdictFor(vaultData(() => ({ manager: squadsVault, lastUpdatedTs: stamp + 100n }))),
    { neutral: true, reason: null });
  assert.equal(verdictFor(vaultData(() => ({ manager: squadsVault, lastUpdatedTs: stamp }))).neutral, true);

  const refuses = (data: Uint8Array, pattern: RegExp) => {
    const verdict = verdictFor(data);
    assert.equal(verdict.neutral, false, `expected refusal, got ${JSON.stringify(verdict)}`);
    assert.match(verdict.reason ?? "", pattern);
  };

  // The manager must come back to the Squads vault (ST999), and the wire only
  // ever starts from it.
  refuses(vaultData(() => ({ manager: settingsSigner, lastUpdatedTs: stamp + 100n })),
    /left the Voltr manager/);
  assert.equal(vaultNeutralityVerdict({
    before: neutralityView(vaultData(() => ({ manager: settingsSigner }))),
    after: neutralityView(vaultData(() => ({ manager: squadsVault, lastUpdatedTs: stamp }))),
    expectedManager: squadsVault,
  }).neutral, false);

  // Economic and config fields are never excused, including the nested stamp.
  refuses(restored((vault) => ({
    feeConfiguration: { ...vault.feeConfiguration,
      managerPerformanceFee: vault.feeConfiguration.managerPerformanceFee + 1 },
  })), /other than lastUpdatedTs/);
  refuses(restored((vault) => ({
    asset: { ...vault.asset, totalValue: vault.asset.totalValue + 1n },
  })), /other than lastUpdatedTs/);
  refuses(restored((vault) => ({
    vaultConfiguration: { ...vault.vaultConfiguration,
      lockedProfitDegradationDuration: vault.vaultConfiguration.lockedProfitDegradationDuration + 1n },
  })), /other than lastUpdatedTs/);
  refuses(restored((vault) => ({
    highWaterMark: { ...vault.highWaterMark, lastUpdatedTs: vault.highWaterMark.lastUpdatedTs + 1n },
  })), /other than lastUpdatedTs/);

  // A regressing stamp and undecodable bytes are refusals too.
  refuses(vaultData(() => ({ manager: squadsVault, lastUpdatedTs: stamp - 1n })), /regressed/);
  const verdict = vaultNeutralityVerdict({ before, after: null, expectedManager: squadsVault });
  assert.equal(verdict.neutral, false);
  assert.match(verdict.reason ?? "", /did not decode/);
});
