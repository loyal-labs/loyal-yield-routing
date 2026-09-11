type Query = (
  strings: TemplateStringsArray,
  ...values: unknown[]
) => Promise<unknown[]>;

export type AccountedAutodepositRecovery = {
  executionId: string;
  dedupeKey: string;
  signature: string;
  confirmedSlot: bigint;
  walletPostPullRaw: bigint;
  vaultPostPullRaw: bigint;
};

export class AccountedAutodepositConflictError extends Error {}

/**
 * Finish only the durable links when the confirmed deposit is already accounted.
 * A later withdrawal/rebalance may have closed the ATA and changed the position:
 * do not read today's token balance or replay the position finalizer in this case.
 * All evidence and ownership checks plus link writes are one atomic statement.
 */
export async function reconcileAccountedAutodeposit(args: {
  sql: Query;
  claimToken: string;
  leaseToken: string;
  targetId: bigint;
  scheduledSlotId: bigint;
  pullAttemptId: string;
}): Promise<AccountedAutodepositRecovery | null> {
  const rows = await args.sql`
    WITH owned_claim AS MATERIALIZED (
      SELECT * FROM loyal_yield.balance_sweep_lot_claims
      WHERE claim_token = ${args.claimToken} AND target_id = ${args.targetId.toString()}::bigint
        AND status = 'selected' AND autodeposit_executor_lease_token = ${args.leaseToken}
        AND autodeposit_executor_lease_expires_at > now()
      FOR UPDATE
    ), evidence AS MATERIALIZED (
      SELECT claim.claim_token, slot.id AS scheduled_slot_id,
             execution.id AS execution_id, execution.dedupe_key,
             execution.source_post_balance_raw, execution.destination_post_balance_raw,
             deposit.id AS deposit_id, position.id AS position_id,
             top_up.signature, top_up.confirmed_slot
      FROM owned_claim AS claim
      JOIN loyal_yield.balance_sweep_scheduled_slots AS slot
        ON slot.claim_token = claim.claim_token AND slot.target_id = claim.target_id
      JOIN loyal_yield.balance_sweep_transaction_attempts AS pull
        ON pull.claim_token = claim.claim_token AND pull.target_id = claim.target_id
       AND pull.scheduled_slot_id = slot.id AND pull.id = ${args.pullAttemptId}::bigint
       AND pull.operation_kind = 'pull' AND pull.attempt_state = 'confirmed'
       AND pull.confirmed_slot IS NOT NULL AND pull.amount_raw = claim.amount_raw
      JOIN loyal_yield.balance_sweep_transaction_attempts AS top_up
        ON top_up.claim_token = claim.claim_token AND top_up.target_id = claim.target_id
       AND top_up.scheduled_slot_id = slot.id AND top_up.operation_kind = 'top_up'
       AND top_up.attempt_state = 'confirmed' AND top_up.confirmed_slot >= pull.confirmed_slot
       AND top_up.amount_raw = claim.amount_raw
      JOIN loyal_yield.balance_sweep_executions AS execution
        ON execution.id = top_up.execution_id AND execution.target_id = claim.target_id
       AND execution.signature = pull.signature AND execution.slot = pull.confirmed_slot
       AND execution.amount_raw = claim.amount_raw
      JOIN loyal_yield.user_yield_position_deposits AS deposit
        ON deposit.deposit_signature = top_up.signature
       AND deposit.confirmed_slot = top_up.confirmed_slot
       AND deposit.balance_sweep_execution_id = execution.id
       AND deposit.balance_sweep_scheduled_slot_id = slot.id
       AND deposit.principal_amount_raw = claim.amount_raw
       AND deposit.settings = claim.autodeposit_deposit_plan #>> '{target,settings}'
       AND deposit.vault_index = (claim.autodeposit_deposit_plan #>> '{target,vaultIndex}')::smallint
       AND deposit.vault_pubkey = claim.autodeposit_deposit_plan #>> '{target,vaultPubkey}'
       AND deposit.wallet_address = claim.autodeposit_deposit_plan #>> '{target,wallet}'
       AND deposit.target_reserve = claim.autodeposit_deposit_plan ->> 'reserve'
       AND deposit.liquidity_mint = claim.autodeposit_deposit_plan ->> 'liquidityMint'
      JOIN loyal_yield.user_yield_position_holding_events AS event
        ON event.source_signature = top_up.signature AND event.source_deposit_id = deposit.id
       AND event.event_type IN ('deposit_initialized', 'deposit_top_up')
       AND event.principal_delta_raw = claim.amount_raw
       AND event.observed_slot >= top_up.confirmed_slot
      JOIN loyal_yield.user_yield_positions AS position
        ON position.id = event.position_id AND position.settings = deposit.settings
       AND position.vault_index = deposit.vault_index AND position.vault_pubkey = deposit.vault_pubkey
       AND position.wallet_address = deposit.wallet_address
       AND position.initial_reserve = deposit.target_reserve
      WHERE claim.claim_token = ${args.claimToken} AND claim.target_id = ${args.targetId.toString()}::bigint
        AND slot.id = ${args.scheduledSlotId.toString()}::bigint
        AND claim.status = 'selected' AND slot.status = 'selected'
        AND claim.autodeposit_executor_lease_token = ${args.leaseToken}
        AND claim.autodeposit_executor_lease_expires_at > now()
        AND claim.amount_raw > 0
        AND claim.amount_raw = (claim.autodeposit_deposit_plan ->> 'amountRaw')::bigint
        AND claim.target_id = (claim.autodeposit_deposit_plan #>> '{target,id}')::bigint
        AND (claim.execution_id IS NULL OR claim.execution_id = execution.id)
        AND (slot.execution_id IS NULL OR slot.execution_id = execution.id)
        AND (execution.yield_deposit_id IS NULL OR execution.yield_deposit_id = deposit.id)
        AND (execution.yield_position_id IS NULL OR execution.yield_position_id = position.id)
        AND (execution.kamino_deposit_signature IS NULL OR execution.kamino_deposit_signature = top_up.signature)
        AND claim.amount_raw = (
          SELECT sum(item.amount_raw) FROM loyal_yield.balance_sweep_lot_claim_items AS item
          WHERE item.claim_token = claim.claim_token
        )
        AND NOT EXISTS (
          SELECT 1 FROM loyal_yield.balance_sweep_execution_lots AS linked
          LEFT JOIN loyal_yield.balance_sweep_lot_claim_items AS item
            ON item.claim_token = claim.claim_token AND item.lot_id = linked.lot_id
          WHERE linked.execution_id = execution.id
            AND (item.lot_id IS NULL OR linked.amount_raw <> item.amount_raw)
        )
        AND NOT EXISTS (
          SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts AS newer
          WHERE newer.claim_token = claim.claim_token AND newer.operation_kind = 'top_up'
            AND newer.attempt_number > top_up.attempt_number
        )
      FOR UPDATE OF slot, execution
    ), completed_execution AS (
      UPDATE loyal_yield.balance_sweep_executions AS execution
      SET yield_deposit_id = evidence.deposit_id, yield_position_id = evidence.position_id,
          kamino_deposit_signature = evidence.signature,
          completed_at = COALESCE(execution.completed_at, now()), completion_failure_code = NULL,
          decoded_evidence = COALESCE(execution.decoded_evidence, '{}'::jsonb) ||
            jsonb_build_object('status', 'executed', 'recoverySource', 'persisted_accounted_deposit',
              'kaminoDepositSignature', evidence.signature, 'kaminoDepositSlot', evidence.confirmed_slot::text),
          decoded_at = now()
      FROM evidence WHERE execution.id = evidence.execution_id
      RETURNING execution.id
    ), execution_lots AS (
      INSERT INTO loyal_yield.balance_sweep_execution_lots (execution_id, lot_id, amount_raw)
      SELECT evidence.execution_id, item.lot_id, item.amount_raw
      FROM evidence JOIN completed_execution ON completed_execution.id = evidence.execution_id
      JOIN loyal_yield.balance_sweep_lot_claim_items AS item ON item.claim_token = evidence.claim_token
      ON CONFLICT (execution_id, lot_id) DO NOTHING
    ), completed_slot AS (
      UPDATE loyal_yield.balance_sweep_scheduled_slots AS slot
      SET status = 'executed', execution_id = evidence.execution_id, last_error = NULL, updated_at = now()
      FROM evidence JOIN completed_execution ON completed_execution.id = evidence.execution_id
      WHERE slot.id = evidence.scheduled_slot_id
      RETURNING slot.id
    ), completed_claim AS (
      UPDATE loyal_yield.balance_sweep_lot_claims AS claim
      SET status = 'executed', execution_id = evidence.execution_id,
          autodeposit_executor_lease_token = NULL, autodeposit_executor_lease_expires_at = NULL,
          updated_at = now()
      FROM evidence JOIN completed_slot ON completed_slot.id = evidence.scheduled_slot_id
      WHERE claim.claim_token = evidence.claim_token
      RETURNING claim.claim_token
    )
    SELECT 'completed' AS status, jsonb_build_object(
      'execution_id', evidence.execution_id::text, 'dedupe_key', evidence.dedupe_key,
      'signature', evidence.signature, 'confirmed_slot', evidence.confirmed_slot::text,
      'source_post_balance_raw', evidence.source_post_balance_raw::text,
      'destination_post_balance_raw', evidence.destination_post_balance_raw::text
    ) AS completion FROM evidence
    JOIN completed_claim ON completed_claim.claim_token = evidence.claim_token
    UNION ALL
    SELECT 'accounting_conflict', NULL::jsonb
    WHERE NOT EXISTS (SELECT 1 FROM completed_claim) AND EXISTS (
      SELECT 1 FROM owned_claim AS claim
      JOIN loyal_yield.balance_sweep_transaction_attempts AS top_up
        ON top_up.claim_token = claim.claim_token AND top_up.operation_kind = 'top_up'
      WHERE claim.claim_token = ${args.claimToken} AND claim.target_id = ${args.targetId.toString()}::bigint
        AND claim.status = 'selected' AND claim.autodeposit_executor_lease_token = ${args.leaseToken}
        AND claim.autodeposit_executor_lease_expires_at > now()
        AND (
          EXISTS (SELECT 1 FROM loyal_yield.user_yield_position_deposits AS deposit
            WHERE deposit.deposit_signature = top_up.signature)
          OR EXISTS (SELECT 1 FROM loyal_yield.user_yield_position_holding_events AS event
            WHERE event.source_signature = top_up.signature)
          OR EXISTS (SELECT 1 FROM loyal_yield.user_yield_positions AS position
            WHERE top_up.attempt_state = 'confirmed'
              AND position.settings = claim.autodeposit_deposit_plan #>> '{target,settings}'
              AND position.vault_index = (claim.autodeposit_deposit_plan #>> '{target,vaultIndex}')::smallint
              AND (position.last_confirmed_slot > top_up.confirmed_slot
                OR (position.current_observed_slot >= top_up.confirmed_slot
                  AND (position.status = 'closed'
                    OR position.current_reserve IS DISTINCT FROM claim.autodeposit_deposit_plan ->> 'reserve'))))
        )
    )
  `;
  const result = rows[0] as Record<string, unknown> | undefined;
  if (!result) return null;
  if (result.status !== "completed") {
    throw new AccountedAutodepositConflictError(
      `Autodeposit accounting evidence conflicts for target ${args.targetId}, scheduled slot ${args.scheduledSlotId}; refusing historical position finalization.`
    );
  }
  const row = result.completion as Record<string, unknown>;
  return {
    executionId: String(row.execution_id),
    dedupeKey: String(row.dedupe_key),
    signature: String(row.signature),
    confirmedSlot: BigInt(String(row.confirmed_slot)),
    walletPostPullRaw: BigInt(String(row.source_post_balance_raw)),
    vaultPostPullRaw: BigInt(String(row.destination_post_balance_raw)),
  };
}

export type AutodepositExecutorOutcome =
  | "completed"
  | "deferred"
  | "recovery_pending"
  | "noop";

// The trigger opts into this protocol. Standalone callers retain exit-zero success.
export function setAutodepositExecutorOutcome(outcome: AutodepositExecutorOutcome): void {
  const configured = process.env[`AUTODEPOSIT_${outcome.toUpperCase()}_EXIT_CODE`];
  const code = configured === undefined ? 0 : Number(configured);
  if (!Number.isInteger(code) || code < 0 || code > 255) {
    throw new Error(`Invalid autodeposit ${outcome} exit code configuration.`);
  }
  process.exitCode = code;
}
