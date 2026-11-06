import {
  AddressLookupTableAccount,
  AddressLookupTableProgram,
  type Connection,
  PublicKey,
  SystemProgram,
  type TransactionInstruction,
} from "@solana/web3.js";
import {
  compilePreparedOperation,
  generated,
  instructionsToSynchronousTransactionDetailsV2,
  pda,
} from "@loyal-labs/loyal-smart-accounts-core";
import { instructions } from "@loyal-labs/loyal-smart-accounts-core/internal";
import BN from "bn.js";

import type { SmartAccountVaultsClient } from "../client";
import type { VoltrPlan, VoltrWithdrawalConsent } from "./plans";
import {
  deriveEarnMaxVoltrAuthority,
  EARN_MAX_VOLTR_ACCOUNT_INDEX,
  VOLTR_PROGRAM_ID,
  voltrUserAccounts,
} from "./program";

export function voltrWithdrawalFence(input: {
  authority: PublicKey;
  owner: PublicKey;
  settingsPda: PublicKey;
  programId: PublicKey;
  currentPolicySeed: bigint;
  receiptBytes?: Uint8Array;
}): { before: TransactionInstruction[]; after: TransactionInstruction[] } {
  const {
    authority,
    owner,
    settingsPda,
    programId,
    currentPolicySeed,
    receiptBytes,
  } = input;
  const seed = currentPolicySeed + BigInt(1);
  if (seed > BigInt(Number.MAX_SAFE_INTEGER))
    throw new EarnMaxVoltrPrepareError("invalid_consent");
  const policy = pda.getPolicyPda({
    settingsPda,
    programId,
    policySeed: Number(seed),
  })[0];
  const guard = SystemProgram.transfer({
    fromPubkey: authority,
    toPubkey: owner,
    lamports: 0,
  });
  const constraints: generated.AccountConstraint[] = [];
  if (receiptBytes) {
    if (receiptBytes.length !== 112)
      throw new EarnMaxVoltrPrepareError("invalid_consent");
    constraints.push({
      accountIndex: 2,
      owner: VOLTR_PROGRAM_ID,
      accountConstraint: {
        __kind: "AccountData",
        fields: [
          [
            {
              dataOffset: new BN(72),
              dataValue: {
                __kind: "U8Slice",
                fields: [receiptBytes.slice(72, 104)],
              },
              operator: generated.DataOperator.Equals,
            },
          ],
        ],
      },
    });
    guard.keys.push({
      pubkey: voltrUserAccounts(authority).receipt,
      isSigner: false,
      isWritable: false,
    });
  }
  const create = instructions.executeSettingsTransactionSync({
    settingsPda,
    signers: [owner],
    feePayer: owner,
    programId,
    remainingAccounts: [{ pubkey: policy, isSigner: false, isWritable: true }],
    actions: [
      {
        __kind: "PolicyCreate",
        seed: new BN(seed.toString()),
        signers: [{ key: owner, permissions: { mask: 7 } }],
        threshold: 1,
        timeLock: 0,
        startTimestamp: null,
        expirationArgs: null,
        policyCreationPayload: {
          __kind: "ProgramInteraction",
          fields: [
            {
              accountIndex: EARN_MAX_VOLTR_ACCOUNT_INDEX,
              preHook: null,
              postHook: null,
              spendingLimits: [],
              instructionsConstraints: [
                {
                  programId: SystemProgram.programId,
                  accountConstraints: constraints,
                  dataConstraints: [
                    {
                      dataOffset: new BN(0),
                      dataValue: { __kind: "U8Slice", fields: [guard.data] },
                      operator: generated.DataOperator.Equals,
                    },
                  ],
                },
              ],
            },
          ],
        },
      },
    ],
  });
  const remove = instructions.executeSettingsTransactionSync({
    settingsPda,
    signers: [owner],
    feePayer: owner,
    programId,
    remainingAccounts: [{ pubkey: policy, isSigner: false, isWritable: true }],
    actions: [{ __kind: "PolicyRemove", policy }],
  });
  if (!receiptBytes) return { before: [create], after: [remove] };
  const compiled = instructionsToSynchronousTransactionDetailsV2({
    vaultPda: authority,
    members: [owner],
    transaction_instructions: [guard],
  });
  const check = instructions.executePolicyPayloadSync({
    feePayer: owner,
    policy,
    accountIndex: EARN_MAX_VOLTR_ACCOUNT_INDEX,
    numSigners: 1,
    programId,
    instruction_accounts: compiled.accounts,
    policyPayload: {
      __kind: "ProgramInteraction",
      fields: [
        {
          instructionConstraintIndices: Uint8Array.of(0),
          transactionPayload: {
            __kind: "SyncTransaction",
            fields: [
              {
                accountIndex: EARN_MAX_VOLTR_ACCOUNT_INDEX,
                instructions: compiled.instructions,
              },
            ],
          },
        },
      ],
    },
  });
  return { before: [create, check], after: [remove] };
}

const protectedClaims = new WeakMap<object, VoltrWithdrawalConsent>();

export const EARN_MAX_VOLTR_LOOKUP_TABLE = new PublicKey(
  "GERDuMq2KXct84sG9JUxuRguqfcPvn71gYXg2p3LLHv4"
);

export async function readEarnMaxVoltrLookupTable(
  connection: Connection
): Promise<AddressLookupTableAccount> {
  const snapshot = await connection.getAccountInfoAndContext(
    EARN_MAX_VOLTR_LOOKUP_TABLE,
    { commitment: "confirmed" }
  );
  const account = snapshot.value;
  if (
    !account ||
    account.executable ||
    !account.owner.equals(AddressLookupTableProgram.programId)
  )
    throw new EarnMaxVoltrPrepareError("lookup_table_unavailable");
  const table = new AddressLookupTableAccount({
    key: EARN_MAX_VOLTR_LOOKUP_TABLE,
    state: AddressLookupTableAccount.deserialize(account.data),
  });
  if (
    !table.isActive() ||
    table.state.lastExtendedSlot >= snapshot.context.slot
  )
    throw new EarnMaxVoltrPrepareError("lookup_table_unavailable");
  return table;
}

export function protectedVoltrClaimConsent(
  prepared: object
): VoltrWithdrawalConsent | null {
  return protectedClaims.get(prepared) ?? null;
}

export type EarnMaxVoltrPrepareErrorCode =
  | "multi_signer_unsupported"
  | "fee_budget_exceeded"
  | "invalid_consent"
  | "lookup_table_unavailable"
  | "transaction_too_large";

const PREPARE_ERROR_MESSAGES: Record<EarnMaxVoltrPrepareErrorCode, string> = {
  multi_signer_unsupported:
    "Earn MAX needs a single-signer smart account without a time lock.",
  transaction_too_large: "Earn MAX transaction is too large.",
  fee_budget_exceeded:
    "The claim network fee exceeds your approval or could not be verified.",
  invalid_consent: "Review and approve the withdrawal again.",
  lookup_table_unavailable:
    "Withdrawal preparation is unavailable. Check status before trying again.",
};

export class EarnMaxVoltrPrepareError extends Error {
  readonly code: EarnMaxVoltrPrepareErrorCode;

  constructor(code: EarnMaxVoltrPrepareErrorCode) {
    super(PREPARE_ERROR_MESSAGES[code]);
    this.name = "EarnMaxVoltrPrepareError";
    this.code = code;
  }
}

export type VoltrPlanBuilder = (context: {
  authority: PublicKey;
  owner: PublicKey;
}) => Promise<VoltrPlan>;

export async function prepareEarnMaxVoltr(input: {
  build: VoltrPlanBuilder;
  client: SmartAccountVaultsClient;
  connection: Connection;
  owner: PublicKey;
  programId: PublicKey;
  settingsPda: PublicKey;
  addressLookupTableAccounts?: AddressLookupTableAccount[];
}) {
  const { build, client, owner, programId, settingsPda } = input;
  const settings = await client.sdk.smartAccounts.queries.fetchSettings(
    settingsPda
  );
  if (Number(settings.threshold) > 1 || Number(settings.timeLock) > 0) {
    throw new EarnMaxVoltrPrepareError("multi_signer_unsupported");
  }
  const authority = deriveEarnMaxVoltrAuthority(settingsPda, programId);
  const {
    outer,
    vault,
    after,
    claimProtection,
    lifecycleFence,
    withdrawalReceiptBytes,
  } = await build({
    authority,
    owner,
  });
  const needsLifecycleFence =
    lifecycleFence ||
    vault.some(
      (instruction) =>
        instruction.programId.equals(VOLTR_PROGRAM_ID) &&
        [
          [248, 225, 47, 22, 116, 144, 23, 143],
          [231, 54, 14, 6, 223, 124, 127, 238],
        ].some((discriminator) =>
          discriminator.every((byte, index) => instruction.data[index] === byte)
        )
    );
  const tables = input.addressLookupTableAccounts ?? [];
  const defaultTable =
    claimProtection || needsLifecycleFence
      ? await readEarnMaxVoltrLookupTable(input.connection)
      : null;
  const lookupTables = defaultTable
    ? [
        defaultTable,
        ...tables.filter((table) => !table.key.equals(defaultTable.key)),
      ]
    : tables;
  const currentPolicySeed =
    settings.policySeed === null
      ? BigInt(0)
      : BigInt(settings.policySeed.toString());
  if (
    claimProtection &&
    (claimProtection.settings !== settingsPda.toBase58() ||
      BigInt(claimProtection.consent.policySeed) !== currentPolicySeed ||
      claimProtection.expiresAt <= BigInt(Math.floor(Date.now() / 1000)))
  ) {
    throw new EarnMaxVoltrPrepareError("invalid_consent");
  }
  const prepared = await client.prepareCustomInstructionSync({
    accountIndex: EARN_MAX_VOLTR_ACCOUNT_INDEX,
    feePayer: owner,
    instructions: vault,
    settingsPda,
    signer: owner,
    addressLookupTableAccounts: lookupTables,
  });
  if (!prepared) {
    throw new EarnMaxVoltrPrepareError("transaction_too_large");
  }
  const fence =
    claimProtection || needsLifecycleFence
      ? voltrWithdrawalFence({
          authority,
          owner,
          settingsPda,
          programId,
          currentPolicySeed,
          receiptBytes: claimProtection?.receiptBytes ?? withdrawalReceiptBytes,
        })
      : { before: [], after: [] };
  const result = {
    ...prepared,
    instructions: [
      ...outer,
      ...fence.before,
      ...prepared.instructions,
      ...fence.after,
      ...(after ?? []),
    ],
  };
  try {
    const transaction = compilePreparedOperation({
      prepared: result,
      blockhash: "11111111111111111111111111111111",
    });
    if (transaction.serialize().length > 1232) {
      throw new EarnMaxVoltrPrepareError("transaction_too_large");
    }
  } catch {
    throw new EarnMaxVoltrPrepareError("transaction_too_large");
  }
  if (claimProtection) {
    const { blockhash } = await claimProtection.connection.getLatestBlockhash(
      "confirmed"
    );
    const transaction = compilePreparedOperation({
      prepared: result,
      blockhash,
    });
    const { value } = await claimProtection.connection.getFeeForMessage(
      transaction.message,
      "confirmed"
    );
    if (
      value === null ||
      !Number.isSafeInteger(value) ||
      BigInt(value) > claimProtection.maxFeeLamports
    ) {
      throw new EarnMaxVoltrPrepareError("fee_budget_exceeded");
    }
    protectedClaims.set(result, { ...claimProtection.consent });
  }
  return result;
}
