# Earn Realtime Routing Verifier

Verify that Loyal Yield realtime can safely drive the first Earn frontend
invalidation integration without wiring frontend code yet.

## Goal

PASS only if the backend realtime layer can emit and route these private Earn
events to the correct user connection:

- autodeposit sweep requested, selected, and executed;
- Earn position changes from `user_yield_positions`;
- Earn transaction history changes;
- Earn onboarding state changes.

The Render SSE service must decide delivery from signed app-issued claims plus
durable `loyal_yield.realtime_events` routing columns, not from browser-trusted
subscription params. Pushed messages must remain invalidations, not canonical
balances or execution instructions.

## Required Checks

### 1. Private Routing Is Enforced

PASS only if private scopes/events cannot be broadcast with only a broad scope.

Check:

```sh
rg -n "event_matches_claims|private|requires.*wallet|settings_pda|smart_account_address" crates/loyal-yield-realtime-core crates/loyal-yield-realtime
```

Required:

- private scopes such as Earn/autodeposit/onboarding require at least one row
  identity key: `wallet_address`, `settings_pda`, or `smart_account_address`;
- token claims still require scope and user identity;
- global/public events, if any, are explicitly separated from private scopes.

### 2. Durable Event Schema Supports the Chosen Events

PASS only if a normal Yield migration adds or updates reusable DB helpers and/or
triggers so selected events write `loyal_yield.realtime_events` with routing
columns filled.

Required event coverage:

- `earn.autodeposit.sweep_requested`;
- `earn.autodeposit.sweep_selected`;
- `earn.autodeposit.sweep_executed`;
- `earn.position.changed`;
- `earn.transaction.recorded`;
- `earn.onboarding.changed`.

Each private event must include enough identity for routing, preferably
`settings_pda` plus `wallet_address`, and `smart_account_address` where the
source table has it.

### 3. Autodeposit Events Stay Backward-Compatible

PASS only if existing scheduled-slot notifications still wake the autodeposit
worker and existing SSE consumers can still process the prior
`autodeposit_slot_changed` / `scheduled_slot_*` shape, or an explicit compatible
mapping is provided.

Check:

```sh
rg -n "autodeposit_slot_changed|scheduled_slot_requested|scheduled_slot_selected|scheduled_slot_executed|balance_sweep_scheduled_slots_realtime_event" crates
```

### 4. Migration Verifier Covers New Objects

PASS only if `yield-migrations --check` schema validation knows about the new
triggers/functions/indexes that make these events real.

Check:

```sh
rg -n "earn_realtime|user_yield_positions|earn_deposit_onboarding_attempts|balance_sweep_executions|emit_.*realtime" crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs crates/loyal-yield-orchestrator/migrations
```

### 5. Local Verification

Run the relevant non-frontend checks:

```sh
cargo fmt --check
cargo test -p loyal-yield-realtime-core
cargo check -p loyal-yield-realtime-core -p loyal-yield-realtime -p loyal-yield-orchestrator --bin yield-migrations
```

If live Neon credentials are available, also run:

```sh
op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-yield-orchestrator --bin yield-migrations -- --check'
```

## Out of Scope

- No frontend app wiring.
- No Render deploy unless separately requested.
- No transaction execution changes.
- No pushed full financial state.

## Verdict

Return PASS only if every required check above passes. Otherwise return FAIL
with the exact missing event, unsafe routing gap, schema validation gap, or
command failure.
