import { neon } from "@neondatabase/serverless";
import { Connection, PublicKey } from "@solana/web3.js";

import {
  historicalIdleVaultRecoveryAction,
  projectIdleVaultBalance,
  type IdleVaultProjection,
} from "./autodeposit-idle-vault-handoff";

type RecoveryRow = {
  completed_at: string | null;
  execution_id: string;
  managed_vault_id: string;
  mint: string;
  token_account: string;
  vault_pubkey: string;
};

function requireEnv(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} is required`);
  }
  return value;
}

async function loadCandidates(databaseUrl: string): Promise<RecoveryRow[]> {
  const sql = neon(databaseUrl);
  return (await sql`
    SELECT DISTINCT ON (execution.id)
      execution.id::text AS execution_id,
      execution.completed_at::text,
      vault.id::text AS managed_vault_id,
      target.vault_pubkey,
      execution.token_mint AS mint,
      COALESCE(execution.destination_token_ata, execution.destination_vault_ata)
        AS token_account
    FROM loyal_yield.balance_sweep_executions AS execution
    JOIN loyal_yield.balance_sweep_targets AS target
      ON target.id = execution.target_id
    JOIN loyal_yield.managed_vaults AS vault
      ON vault.settings = target.settings
     AND vault.vault_index = target.vault_index
     AND vault.vault_pubkey = target.vault_pubkey
    WHERE (
        execution.completion_failure_code = 'kamino_top_up_failed'
        OR execution.decoded_evidence->>'status' =
          'partial_executed_pull_top_up_blocked'
      )
      AND execution.decoded_evidence->>'idleVaultDepositDecisionId' IS NULL
    ORDER BY execution.id
  `) as RecoveryRow[];
}

async function persistProjection(
  databaseUrl: string,
  projection: IdleVaultProjection,
): Promise<void> {
  const sql = neon(databaseUrl);
  await sql`
    INSERT INTO loyal_yield.vault_idle_token_balances_current (
      vault_id,
      mint,
      amount_raw,
      owner,
      token_account,
      observed_slot,
      observed_at,
      source_commitment,
      updated_at
    ) VALUES (
      ${projection.vaultId.toString()},
      ${projection.mint},
      ${projection.amountRaw.toString()},
      ${projection.owner},
      ${projection.tokenAccount},
      ${projection.observedSlot.toString()},
      ${new Date().toISOString()},
      'finalized',
      now()
    )
    ON CONFLICT (vault_id, mint) DO UPDATE SET
      amount_raw = CASE
        WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
        THEN EXCLUDED.amount_raw
        ELSE loyal_yield.vault_idle_token_balances_current.amount_raw
      END,
      owner = CASE
        WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
        THEN EXCLUDED.owner
        ELSE loyal_yield.vault_idle_token_balances_current.owner
      END,
      token_account = CASE
        WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
        THEN EXCLUDED.token_account
        ELSE loyal_yield.vault_idle_token_balances_current.token_account
      END,
      observed_slot = GREATEST(
        loyal_yield.vault_idle_token_balances_current.observed_slot,
        EXCLUDED.observed_slot
      ),
      observed_at = CASE
        WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
        THEN EXCLUDED.observed_at
        ELSE loyal_yield.vault_idle_token_balances_current.observed_at
      END,
      source_commitment = CASE
        WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
        THEN EXCLUDED.source_commitment
        ELSE loyal_yield.vault_idle_token_balances_current.source_commitment
      END,
      updated_at = CASE
        WHEN EXCLUDED.observed_slot >= loyal_yield.vault_idle_token_balances_current.observed_slot
        THEN now()
        ELSE loyal_yield.vault_idle_token_balances_current.updated_at
      END
  `;
}

async function main(): Promise<void> {
  const applyProjections = Bun.argv.slice(2).includes("--apply-projections");
  const databaseUrl = requireEnv("NEON_DATABASE_URL");
  const connection = new Connection(requireEnv("SOLANA_RPC_URL"), "finalized");
  const candidates = await loadCandidates(databaseUrl);
  const results = [];

  for (const candidate of candidates) {
    const response = await connection.getParsedAccountInfo(
      new PublicKey(candidate.token_account),
      "finalized",
    );
    const account = response.value;
    if (account && !("parsed" in account.data)) {
      throw new Error(
        `Historical recovery token account ${candidate.token_account} is not parsed`,
      );
    }
    const info = (account && "parsed" in account.data
      ? account.data.parsed.info
      : {}) as {
      mint?: string;
      owner?: string;
      tokenAmount?: { amount?: string };
    };
    if (
      account &&
      (info.mint !== candidate.mint || info.owner !== candidate.vault_pubkey)
    ) {
      throw new Error(
        `Historical recovery token account ${candidate.token_account} does not match its vault or mint`,
      );
    }
    const amountRaw = account ? info.tokenAmount?.amount : "0";
    if (amountRaw === undefined) {
      throw new Error(
        `Historical recovery token account ${candidate.token_account} has no raw amount`,
      );
    }
    const projection = projectIdleVaultBalance(null, {
      amountRaw: BigInt(amountRaw),
      mint: candidate.mint,
      observedSlot: BigInt(response.context.slot),
      owner: candidate.vault_pubkey,
      tokenAccount: candidate.token_account,
      vaultId: BigInt(candidate.managed_vault_id),
    });
    const action = historicalIdleVaultRecoveryAction({
      alreadyRecovered: candidate.completed_at !== null,
      amountRaw: projection.amountRaw,
      executionId: BigInt(candidate.execution_id),
    });
    if (action === "project" && applyProjections) {
      await persistProjection(databaseUrl, projection);
    }
    results.push({
      executionId: candidate.execution_id,
      action,
      amountRaw: projection.amountRaw.toString(),
      observedSlot: projection.observedSlot.toString(),
      projectionApplied: action === "project" && applyProjections,
    });
  }

  console.log(
    JSON.stringify(
      {
        mode: applyProjections ? "apply_projections" : "read_only",
        sendsTransactions: false,
        candidates: results,
      },
      null,
      2,
    ),
  );
}

if (import.meta.main) {
  main().catch((error) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  });
}
