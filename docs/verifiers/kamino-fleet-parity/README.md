# Kamino planner + revalidator replacement parity

This is the offline acceptance boundary for replacing both Rust services:

- `fleet-opportunity-planner`; and
- the complete `same-mint-reserve-swap --fleet-worker revalidate` lane,
  including `same_mint` and `cross_mint_jupiter`

with one Go process. It does not authorize replacing the retained Rust executor,
confirmer, reconciler, health projector, or ALT provisioner.

## Commands

```sh
# Exact frozen Rust/Go parity for the complete immutable market epoch.
scripts/verify-kamino-market-epoch-parity.sh

# Read-only audit of the current monitor frontier (run with Timescale env).
scripts/audit-kamino-market-epoch-production.sh

# Fast proof that the complete-replacement comparator catches protected drift.
scripts/verify-kamino-planner-revalidator-parity.sh --self-test

# Compare artifacts generated elsewhere from this same frozen contract.
scripts/verify-kamino-planner-revalidator-parity.sh \
  --compare /path/to/rust.json /path/to/go.json

# Run the complete disposable-PostgreSQL lifecycle and both artifact producers.
scripts/verify-kamino-planner-revalidator-parity.sh --audit-current
```

The epoch-specific producers are implemented as
`kamino-market-epoch-reference` and `loyal-kamino-market-epoch`. The complete
planner/revalidator producers are:

- `crates/loyal-yield-orchestrator/src/bin/kamino-fleet-parity-reference.rs`
- `go/kamino-fleet-planner/cmd/loyal-kamino-fleet-parity/main.go`

Both the narrower epoch gate and full `--audit-current` gate are expected to
pass. Before comparing frozen artifacts, `--audit-current` runs
`scripts/verify-kamino-fleet-planner-e2e.sh`: real Go publication/revalidation
integration tests plus the retained Rust store lifecycle verifier over fresh
databases in one disposable PostgreSQL server. It requires signed submission,
confirmation, reconciliation, and exactly-once completion subchecks to pass.
The artifact cases are parity fixtures, not substitutes for that lifecycle.

## Required artifact evidence

Both producers receive the same contract bytes, frozen clock, fresh disposable
PostgreSQL database, and deterministic loopback RPC endpoint. Neither producer
may load production credentials or contact an external service.

The comparator requires exact equality for:

1. the complete, Rust-deserializable `ImmutableMarketEpoch`, including mint
   coverage and a multi-reserve material frontier;
2. capacity-aware planner output, canonical `execution_plan`, and the complete
   `rebalance_opportunity_idempotency_key`;
3. fresh and fused route-revalidation output, including route and requirements
   fingerprints, ALT addresses, compiled packet hash/size, simulation result,
   current opportunity fence, market-epoch fence, and queue transition;
4. fail-closed outcomes for missing ALT, oversized packet, simulation failure,
   stale market epoch, changed opportunity fence, and lost lease.

Separately, the full local verifier requires the durable lifecycle through
retained execution, confirmation, reconciliation, and completion without
duplicate capital movement. It also starts side-effect-free role probes for the unfiltered retained
executor, confirmer, reconciler, and ALT provisioner. It does not start either
replaced Rust planner/revalidator role.

The Go artifact additionally must prove that exactly one service process owns
`opportunity_planner` and `route_revalidator` and that neither replaced Rust
role was started. Planner-to-revalidator evidence remains typed and in-process.
The only child-process exception is the pure `loyal-klend-proxy` KLend
proxy: typed JSON over stdin/stdout, official KLend builders, a pinned binary
digest, and no network, database, signer, or broadcast capability.

## Negative controls

The comparator self-test starts from equal synthetic Rust/Go evidence and then
independently mutates planner economics, opportunity identity, epoch frontier,
route fingerprint, ALT selection, packet evidence, simulation evidence, queue
transition, required negative cases, process topology, network isolation, and
durable lifecycle evidence. Every mutation must make the comparison fail.

A green self-test proves the gate detects those classes of drift. It does not
prove the candidate is ready; only a green `--audit-current` result does that.
