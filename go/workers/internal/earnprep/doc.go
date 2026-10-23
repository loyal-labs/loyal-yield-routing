// Package earnprep holds the transaction pieces two or more Earn families build, each a mirror of
// the TS builder in web/packages (CL = smart-account-vaults/src/client.ts):
//
//   - ExecuteMemberSync: execute_transaction_sync_v2 with SyncPayload::Transaction, signed by the
//     wallet as the settings' member. CL:8017-8026 (deposit), CL:8830 (withdraw), via
//     loyal-smart-accounts-core/src/utils/compileToSynchronousMessageV2.ts and
//     loyal-smart-accounts-core/src/internal/instructions/executeTransactionSyncV2.ts.
//   - SyncExecuteUnits: the compute unit headroom a sync execute adds, CL:5878-5899.
//   - CompileV0: TransactionMessage.compileToV0Message with lookup tables.
//   - ParsePolicySigner: the deployment policy signer public key,
//     web/apps/web/src/lib/yield-optimization/deployment-policy-signer.server.ts:34-63.
//   - Sponsor: the sponsored smart account setup cosigner,
//     web/apps/web/src/features/smart-accounts/server/onchain.ts:51-65.
//
// The idempotent ATA create and compute budget instructions are programs/spl
// (CreateIdempotentATA, SetComputeUnitLimit, SetComputeUnitPrice).
package earnprep
