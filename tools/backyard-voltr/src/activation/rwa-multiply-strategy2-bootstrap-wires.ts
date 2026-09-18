import {
  AccountRole,
  createNoopSigner,
  getAddressEncoder,
  type Address,
  type Instruction,
} from "@solana/kit";
import { getCreateAssociatedTokenIdempotentInstructionAsync } from "@solana-program/token";
import {
  getInitializeStrategyInstructionAsync,
  getUpdateVaultConfigInstructionAsync,
  VaultConfigField,
} from "@voltr/vault-sdk";

import { deriveStrategyTwoVoltrAccounts } from "../domain/rwa-multiply-strategy2-route-spec.js";
import type { RwaMultiplyRouteSpec } from "../domain/rwa-multiply-route-spec.js";
import {
  deriveRwaMultiplyVoltrAccounts,
  initializeRwaAdaptorConfigInstruction,
  initializeRwaAdaptorReportTicketInstruction,
  initializeRemainingAccounts,
  RWA_ADAPTOR_DISCRIMINATORS,
} from "../integrations/rwa-multiply-voltr.js";

/**
 * The wire builder lives in its own module because the CLI entry
 * (rwa-multiply-strategy2-bootstrap.ts) runs `main()` at import time; the
 * install-contract test imports this builder to pin the wire C account roles.
 */
function invariant(value: unknown, message: string): asserts value {
  if (!value) throw new Error(message);
}

function appendAccounts(
  instruction: Instruction,
  accounts: readonly Readonly<{ address: Address; role: AccountRole }>[],
): Instruction {
  return { ...instruction, accounts: [...(instruction.accounts ?? []), ...accounts] };
}

/**
 * The three ordered bootstrap wires. Account privileges are message-wide, so
 * initializeConfig (config WRITABLE_SIGNER) can never share a transaction with
 * initializeReportTicket (config READONLY), and wire C is the custody ATA plus
 * the manager round-trip (BAqg -> initializeStrategy -> ST999). Every signer is
 * a no-op signer: address/role shape is what matters here, never key material.
 */
export async function buildBootstrapWires(route: RwaMultiplyRouteSpec) {
  const admin = createNoopSigner(route.setupAdmin);
  const configKeypair = createNoopSigner(route.customAdaptor.strategyConfig);
  const settingsSigner = createNoopSigner(route.customAdaptor.settingsSigner);
  const accounts = await deriveRwaMultiplyVoltrAccounts(route);
  const strategyAccounts = await deriveStrategyTwoVoltrAccounts(route.customAdaptor.strategyConfig);
  invariant(strategyAccounts.reportTicket === accounts.reportTicket
    && strategyAccounts.strategyAuth === accounts.strategyAuth
    && strategyAccounts.strategyInitReceipt === accounts.strategyInitReceipt,
  "strategy-two derivation drifted from the route-derived Voltr accounts");

  const addressEncoder = getAddressEncoder();
  // The adaptor's initialize handler requires only the Voltr manager to sign
  // and validates the remaining accounts by key, so the appended six reuse the
  // shared read-only list; the Squads vault PDA can never sign (ST999).
  const initializeStrategy = appendAccounts(await getInitializeStrategyInstructionAsync({
    payer: admin,
    manager: settingsSigner,
    vault: route.vault.address,
    strategy: route.customAdaptor.strategyConfig,
    adaptorAddReceipt: accounts.adaptorAddReceipt,
    strategyInitReceipt: accounts.strategyInitReceipt,
    vaultStrategyAuth: accounts.strategyAuth,
    adaptorProgram: route.customAdaptor.program,
    instructionDiscriminator: RWA_ADAPTOR_DISCRIMINATORS.initialize,
    additionalArgs: null,
  }, { programAddress: route.programs.voltr }), initializeRemainingAccounts(route));
  const custodyAta = await getCreateAssociatedTokenIdempotentInstructionAsync({
    payer: admin,
    ata: accounts.strategyAssetAta,
    owner: accounts.strategyAuth,
    mint: route.assets.assetMint,
    systemProgram: route.programs.system,
    tokenProgram: route.assets.tokenProgram,
  }, { programAddress: route.assets.associatedTokenProgram });
  const handoffToSettingsSigner = await getUpdateVaultConfigInstructionAsync({
    admin,
    vault: route.vault.address,
    field: VaultConfigField.Manager,
    data: addressEncoder.encode(route.customAdaptor.settingsSigner),
  }, { programAddress: route.programs.voltr });
  const restoreManager = await getUpdateVaultConfigInstructionAsync({
    admin,
    vault: route.vault.address,
    field: VaultConfigField.Manager,
    data: addressEncoder.encode(route.squads.vault),
  }, { programAddress: route.programs.voltr });
  return {
    accounts,
    wireA: [initializeRwaAdaptorConfigInstruction(admin, configKeypair, accounts, route)],
    wireB: [await initializeRwaAdaptorReportTicketInstruction(admin, route)],
    wireC: [custodyAta, handoffToSettingsSigner, initializeStrategy, restoreManager],
  } as const;
}
