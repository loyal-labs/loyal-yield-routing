import {
  DEPOSIT_STRATEGY_DISCRIMINATOR,
  DEPOSIT_VAULT_DISCRIMINATOR,
  HARVEST_FEE_DISCRIMINATOR,
  UPDATE_VAULT_CONFIG_DISCRIMINATOR,
  VOLTR_VAULT_PROGRAM_ADDRESS,
  WITHDRAW_STRATEGY_DISCRIMINATOR,
  parseTransactionEvents,
  type ParsedVoltrEvent,
} from "@voltr/vault-sdk";
import bs58 from "bs58";

import { RWA_MULTIPLY_ROUTE } from "../domain/rwa-multiply-route-spec.js";

type Instruction = { programIdIndex: number; accounts: number[]; data: string; stackHeight?: number };
export type Receipt = {
  slot: number;
  transaction: { signatures: string[]; message: { accountKeys: string[]; instructions: Instruction[] } };
  meta: {
    err: unknown;
    logMessages: string[] | null;
    loadedAddresses?: { writable: string[]; readonly: string[] };
    innerInstructions: { index: number; instructions: Instruction[] }[] | null;
  } | null;
};

function describe(event: ParsedVoltrEvent) {
  switch (event.name) {
    case "DepositStrategyEvent":
    case "WithdrawStrategyEvent": {
      const p = event.payload;
      return {
        event: event.name,
        direction: event.name === "DepositStrategyEvent" ? "deposit" : "withdraw",
        strategyCapitalRaw:
          "vaultAmountAssetDeposited" in p ? p.vaultAmountAssetDeposited : p.vaultAmountAssetWithdrawn,
        valueBefore: p.vaultAssetTotalValueBefore,
        valueAfter: p.vaultAssetTotalValueAfter,
        effectiveSupplyBefore: p.vaultLpSupplyInclFeesBefore,
        effectiveSupplyAfter: p.vaultLpSupplyInclFeesAfter,
        effectiveSupplyDelta: p.vaultLpSupplyInclFeesAfter - p.vaultLpSupplyInclFeesBefore,
        highWaterMarkBefore: p.vaultHighestAssetPerLpDecimalBitsBefore,
        highWaterMarkAfter: p.vaultHighestAssetPerLpDecimalBitsAfter,
      };
    }
    case "DepositVaultEvent": {
      const p = event.payload;
      return {
        event: event.name,
        depositedAssetRaw: p.userAmountAssetDeposited,
        mintedUserLp: p.userAmountLpMinted,
        valueBefore: p.vaultAssetTotalValueBefore,
        valueAfter: p.vaultAssetTotalValueAfter,
        effectiveSupplyBefore: p.vaultLpSupplyInclFeesBefore,
        effectiveSupplyAfter: p.vaultLpSupplyInclFeesAfter,
        accumulatedFeesBefore: p.vaultLpTotalAccumulatedFeesBefore,
        accumulatedFeesAfter: p.vaultLpTotalAccumulatedFeesAfter,
        accumulatedFeesDelta: p.vaultLpTotalAccumulatedFeesAfter - p.vaultLpTotalAccumulatedFeesBefore,
        effectiveSupplyDeltaLessUserMint:
          p.vaultLpSupplyInclFeesAfter - p.vaultLpSupplyInclFeesBefore - p.userAmountLpMinted,
        deadWeightBefore: p.vaultLpDeadWeightBefore,
        deadWeightAfter: p.vaultLpDeadWeightAfter,
        highWaterMarkBefore: p.vaultHighestAssetPerLpDecimalBitsBefore,
        highWaterMarkAfter: p.vaultHighestAssetPerLpDecimalBitsAfter,
      };
    }
    case "HarvestFeeEvent": {
      const p = event.payload;
      return {
        event: event.name,
        harvestedAdminLp: p.amountLpAdminFees,
        harvestedManagerLp: p.amountLpManagerFees,
        harvestedProtocolLp: p.amountLpProtocolFees,
      };
    }
    case "UpdateVaultConfigEvent":
      return {
        event: event.name,
        field: event.payload.field,
        before: event.payload.oldValue,
        after: event.payload.newValue,
      };
    default:
      throw Error(`Unsupported Earn Max event: ${event.name}`);
  }
}

const discriminators = {
  DepositStrategyEvent: DEPOSIT_STRATEGY_DISCRIMINATOR,
  WithdrawStrategyEvent: WITHDRAW_STRATEGY_DISCRIMINATOR,
  DepositVaultEvent: DEPOSIT_VAULT_DISCRIMINATOR,
  HarvestFeeEvent: HARVEST_FEE_DISCRIMINATOR,
  UpdateVaultConfigEvent: UPDATE_VAULT_CONFIG_DISCRIMINATOR,
} as const;

type Frame = {
  program: string;
  instruction: Instruction;
  outer: number;
  inner: number | null;
  isEarnMax: boolean;
  emitted: boolean;
};

export function feeReceipt(tx: Receipt, signature: string, blockSignatures: string[]) {
  const m = tx.meta;
  const txIndex = blockSignatures.indexOf(signature);
  if (
    !m ||
    m.err !== null ||
    !m.logMessages ||
    !m.innerInstructions ||
    txIndex < 0 ||
    blockSignatures.lastIndexOf(signature) !== txIndex ||
    tx.transaction.signatures[0] !== signature
  )
    throw Error("Missing successful transaction or matching block inclusion");

  const { accountKeys, instructions } = tx.transaction.message;
  const keys = [
    ...accountKeys,
    ...(m.loadedAddresses?.writable ?? []),
    ...(m.loadedAddresses?.readonly ?? []),
  ];
  const groups = new Map<number, Instruction[]>();
  for (const { index, instructions: inner } of m.innerInstructions) {
    if (!Number.isInteger(index) || !instructions[index] || groups.has(index))
      throw Error("Invalid inner instruction group");
    groups.set(index, inner);
  }
  const calls = instructions.flatMap((instruction, outer) => [
    { instruction, outer, inner: null as number | null, depth: 1 },
    ...(groups.get(outer) ?? []).map((instruction, inner) => ({
      instruction,
      outer,
      inner,
      depth: instruction.stackHeight,
    })),
  ]);
  const stack: Frame[] = [];
  let called = 0;
  const evidence = [];
  for (const [logIndex, line] of m.logMessages.entries()) {
    const invoke = /^Program (\S+) invoke \[(\d+)\]$/.exec(line);
    if (invoke) {
      const call = calls[called++];
      if (
        !call ||
        call.depth !== stack.length + 1 ||
        call.depth !== Number(invoke[2]) ||
        keys[call.instruction.programIdIndex] !== invoke[1]
      )
        throw Error("Instruction and invocation traces disagree");
      const program = keys[call.instruction.programIdIndex]!;
      stack.push({
        ...call,
        program,
        isEarnMax:
          program === VOLTR_VAULT_PROGRAM_ADDRESS &&
          call.instruction.accounts.some((i) => keys[i] === RWA_MULTIPLY_ROUTE.vault.address),
        emitted: false,
      });
      continue;
    }
    const success = /^Program (\S+) success$/.exec(line);
    if (success) {
      const call = stack.pop();
      if (!call || call.program !== success[1] || (call.isEarnMax && !call.emitted))
        throw Error("Missing Earn Max event or mismatched return");
      continue;
    }
    if (line.startsWith("Log truncated") || /^Program \S+ failed:/.test(line))
      throw Error("Unusable invocation trace");
    const call = stack.at(-1);
    if (!call?.isEarnMax || !line.startsWith("Program data: ")) continue;
    const [event] = parseTransactionEvents({ logMessages: [line] });
    if (
      !event ||
      !("vault" in event.payload) ||
      event.payload.vault !== RWA_MULTIPLY_ROUTE.vault.address ||
      !Object.hasOwn(discriminators, event.name) ||
      call.emitted
    )
      throw Error("Unexpected or ambiguous Earn Max event");
    const disc = discriminators[event.name as keyof typeof discriminators];
    const data = bs58.decode(call.instruction.data);
    if (!Buffer.from(data.subarray(0, 8)).equals(disc))
      throw Error("Instruction discriminator and event disagree");
    call.emitted = true;
    evidence.push({ outer: call.outer, inner: call.inner, logIndex, ...describe(event) });
  }
  if (stack.length || called !== calls.length || !evidence.length)
    throw Error("Incomplete or absent Earn Max trace");
  return { signature, slot: tx.slot, transactionIndex: txIndex, events: evidence };
}
