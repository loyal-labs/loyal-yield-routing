#!/usr/bin/env bun

import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SQL } from "bun";

import { releaseAutodepositLotClaim } from "./execute-autodeposit-policy";

const USDC_MINT = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const CLAIM_TOKEN = "autodeposit-release-selector-verification";
const RETRY_DELAY_SECONDS = 6 * 60 * 60;

const databaseUrl = process.env.AUTODEPOSIT_SELECTOR_DATABASE_URL;
const triggerBinary = process.env.AUTODEPOSIT_SELECTOR_TRIGGER_BINARY;
if (!databaseUrl || !triggerBinary) {
  throw new Error(
    "AUTODEPOSIT_SELECTOR_DATABASE_URL and AUTODEPOSIT_SELECTOR_TRIGGER_BINARY are required"
  );
}

const client = new SQL(databaseUrl);
const scratch = mkdtempSync(join(tmpdir(), "autodeposit-release-selector-"));
const invocationLog = join(scratch, "executor-invocations.log");
const executorStub = join(scratch, "executor-stub.sh");
writeFileSync(invocationLog, "", "utf8");
writeFileSync(
  executorStub,
  `#!/bin/sh\nprintf '%s\\n' "$*" >> "${invocationLog}"\nexit 0\n`,
  "utf8"
);
chmodSync(executorStub, 0o700);

function expect(condition: boolean, message: string, detail?: unknown): void {
  if (!condition) {
    throw new Error(
      `${message}${detail === undefined ? "" : `: ${JSON.stringify(detail)}`}`
    );
  }
  console.log(`PASS: ${message}`);
}

async function runTrigger(): Promise<void> {
  const process = Bun.spawn(
    [
      triggerBinary,
      "--postgres-url",
      databaseUrl,
      "--once",
      "--disable-realtime-listen",
      "--execute-eligible",
      "--executor-command",
      executorStub,
      "--execute-limit",
      "10",
    ],
    {
      env: {
        ...globalThis.process.env,
        OBSERVABILITY_ENABLED: "false",
        RUST_LOG: "warn",
      },
      stderr: "pipe",
      stdout: "pipe",
    }
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    process.exited,
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
  ]);
  expect(exitCode === 0, "production trigger completed one selector cycle", {
    exitCode,
    stdout: stdout.slice(0, 400),
    stderr: stderr.slice(0, 400),
  });
}

try {
  const [policy] = await client`
    INSERT INTO loyal_yield.route_policies (
      settings, authority, policy_seed, policy_account, vault_index,
      vault_pubkey, delegated_signers, threshold, route_modes, stable_mints,
      kamino_markets, kamino_liquidity_mints, active, last_seen_slot,
      last_seen_signature
    ) VALUES (
      'selector-settings', 'selector-authority', 1, 'selector-route-policy', 1,
      'selector-vault', ARRAY['selector-signer'], 1,
      ARRAY['same_mint_kamino'], ARRAY[${USDC_MINT}], ARRAY['selector-market'],
      ARRAY[${USDC_MINT}], true, 1, 'selector-signature'
    )
    RETURNING id
  `;
  await client`
    INSERT INTO loyal_yield.managed_vaults (
      settings, vault_index, vault_pubkey, active_policy_id, active
    ) VALUES ('selector-settings', 1, 'selector-vault', ${policy.id}, true)
  `;
  const [target] = await client`
    INSERT INTO loyal_yield.balance_sweep_targets (
      settings, authority, policy_seed, policy_account, vault_index,
      vault_pubkey, wallet, wallet_usdc_ata, vault_usdc_ata,
      wallet_token_ata, vault_token_ata, token_mint, delegated_signers,
      threshold, max_amount_per_period, wallet_balance_floor_raw,
      lifecycle_status, active, last_seen_slot, last_seen_signature
    ) VALUES (
      'selector-settings', 'selector-authority', 2, 'selector-sweep-policy', 1,
      'selector-vault', 'selector-wallet', 'selector-wallet-ata',
      'selector-vault-ata', 'selector-wallet-ata', 'selector-vault-ata',
      ${USDC_MINT}, ARRAY['selector-signer'], 1, 1000000, 0, 'active', true,
      1, 'selector-target-signature'
    )
    RETURNING id
  `;
  await client`
    INSERT INTO loyal_yield.balance_sweep_wallet_balances_current (
      target_id, wallet, wallet_usdc_ata, wallet_token_ata, amount_raw, mint,
      observed_slot, source, source_commitment
    ) VALUES (
      ${target.id}, 'selector-wallet', 'selector-wallet-ata',
      'selector-wallet-ata', 1000000, ${USDC_MINT}, 1, 'verification',
      'finalized'
    )
  `;
  await client`
    INSERT INTO loyal_yield.balance_sweep_wallet_balance_events (
      event_id, target_id, wallet, wallet_usdc_ata, wallet_token_ata,
      mint, previous_amount_raw, amount_raw, delta_amount_raw, observed_slot,
      observed_at, source, source_commitment
    ) VALUES (
      1, ${target.id}, 'selector-wallet', 'selector-wallet-ata',
      'selector-wallet-ata', ${USDC_MINT}, 0, 1000000, 1000000, 1, now(),
      'verification', 'finalized'
    )
  `;
  await client`
    INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
    VALUES ('balance_sweep_autodeposit_trigger', 1)
    ON CONFLICT (consumer_name) DO UPDATE SET last_event_id = EXCLUDED.last_event_id
  `;
  const [slot] = await client`
    INSERT INTO loyal_yield.balance_sweep_scheduled_slots (
      target_id, token_mint, eligible_after, status
    ) VALUES (${target.id}, ${USDC_MINT}, now(), 'scheduled')
    RETURNING id
  `;
  const [lot] = await client`
    INSERT INTO loyal_yield.balance_sweep_surplus_lots (
      target_id, source_event_id, original_amount_raw, remaining_amount_raw,
      classification, eligible_after, status, confidence, reason,
      scheduled_slot_id
    ) VALUES (
      ${target.id}, 1, 1000000, 0, 'unknown', now(), 'consumed', 'verified',
      'selector lifecycle verification', ${slot.id}
    )
    RETURNING id
  `;
  await client`
    INSERT INTO loyal_yield.balance_sweep_lot_claims (
      claim_token, target_id, amount_raw, status, stale_check_event_id
    ) VALUES (${CLAIM_TOKEN}, ${target.id}, 1000000, 'selected', 1)
  `;
  await client`
    INSERT INTO loyal_yield.balance_sweep_lot_claim_items (
      claim_token, lot_id, amount_raw
    ) VALUES (${CLAIM_TOKEN}, ${lot.id}, 1000000)
  `;
  await client`
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET status = 'selected', claim_token = ${CLAIM_TOKEN}
    WHERE id = ${slot.id}
  `;

  await releaseAutodepositLotClaim({
    claimToken: CLAIM_TOKEN,
    databaseUrl,
    lastError: "vault_drained selector verification",
    neon: (() => client) as never,
    pauseTargetForMissingDelegate: false,
    retryDelaySeconds: RETRY_DELAY_SECONDS,
  });

  const [released] = await client`
    SELECT
      slot.status::text AS slot_status,
      slot.claim_token,
      slot.eligible_after AS slot_eligible_after,
      lot.status::text AS lot_status,
      lot.remaining_amount_raw,
      lot.eligible_after AS lot_eligible_after,
      claim.status::text AS claim_status
    FROM loyal_yield.balance_sweep_scheduled_slots AS slot
    JOIN loyal_yield.balance_sweep_surplus_lots AS lot
      ON lot.scheduled_slot_id = slot.id
    JOIN loyal_yield.balance_sweep_lot_claim_items AS item
      ON item.lot_id = lot.id
    JOIN loyal_yield.balance_sweep_lot_claims AS claim
      ON claim.claim_token = item.claim_token
    WHERE slot.id = ${slot.id}
  `;
  expect(
    released.slot_status === "scheduled" && released.claim_token === null,
    "release requeues the owning slot instead of stranding it",
    released
  );
  expect(
    released.lot_status === "open" &&
      BigInt(released.remaining_amount_raw) === BigInt(1000000) &&
      released.claim_status === "released",
    "release restores the lot and terminalizes the old claim",
    released
  );
  expect(
    new Date(released.slot_eligible_after).getTime() ===
      new Date(released.lot_eligible_after).getTime(),
    "slot and lot share one retry deadline",
    released
  );

  await runTrigger();
  expect(
    readFileSync(invocationLog, "utf8").trim() === "",
    "production selector skips the requeued slot before six hours"
  );

  await client`
    UPDATE loyal_yield.balance_sweep_scheduled_slots
    SET eligible_after = now() - interval '1 second'
    WHERE id = ${slot.id}
  `;
  await client`
    UPDATE loyal_yield.balance_sweep_surplus_lots
    SET eligible_after = now() - interval '1 second'
    WHERE id = ${lot.id}
  `;
  await runTrigger();
  const invocations = readFileSync(invocationLog, "utf8")
    .split("\n")
    .filter(Boolean);
  expect(
    invocations.length === 1 &&
      invocations[0].includes(`--target-id ${target.id}`) &&
      invocations[0].includes(`--scheduled-slot-id ${slot.id}`),
    "production selector executes the same slot after the deadline",
    invocations
  );
  console.log("PASS: autodeposit release-to-selector lifecycle verification");
} finally {
  await client.close();
  rmSync(scratch, { recursive: true, force: true });
}
