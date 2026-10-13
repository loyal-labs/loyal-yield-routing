# Backyard: one copy of every fact

P4 of the de-layer plan. Each fact Backyard decides on has one source in
the code; every other place derives it.

## Lanes

`earnLanes` (`go/workers/internal/backyard/route_catalog.go`) is the one list
of lanes: each entry holds the lane's chain facts (`RuntimeRoute`), how its
KLend legs are authorized (basic families, its own AUTO policy, or split
per-leg policies) and its role (exit-only, initializer, overflow of). These
derive from it:

- `runtimeRoute`, every lane predicate, the account view (`viewAddresses`)
  and the tick's fixed read (`routeFixedAddresses`);
- the policy literals' lane sets (`backyardPolicies`), the basic swap leg and
  the Jupiter venue restriction (Maple quotes Manifest only);
- the Kamino observer's pinned AUTO-market reserves
  (`backyard.ObservationReserves`, read by `observer/kamino`);
- the manifest: `RouteManifest.SHA256` is the hash of every lane's facts
  (decision evidence and NAV report digests bind it), and a flat vault's
  view prefers the registry's first lane.

Gone: the legacy `PRIME/USDC` alias (its obligation 9suF… is
Prime/PRIME/USDC's and held no deposit or borrow at finalized slot
456210521), its split policies, packets and `*_PRIME_*` action names; the
embedded manifest JSON and its identity checks; the SQL lane lists (0107);
the Rust mirror `EARN_MAX_SUPPLEMENTAL_OBSERVATION_RESERVES` (no Rust runs
for Backyard); `testdata/installed-policies.json`. A build finds each policy
literal among the policies installed on chain (`observeInstalledPolicies`);
tests install every literal on synthetic policy accounts.

One builder makes the AUTO lanes' and the Hastra policy:
`internal/policy` (`LoopConstraints`, `HastraConstraints`), which the
`loyal-engine policy` developer loop also installs and checks.

## Valuation

Every value is the refreshed simulation's. The tick's batch is one bank
captured after refreshing every valued lane's reserves and each one's overflow
lane (`withOverflowRoutes`), so NAV, LTV, the payoff bound a full repay is
sized on and the overflow room read the same capture; its freshness is its
slot, within the observation window of the view. The selector values a
destination from the same kind of capture of that lane (its refreshed
reserves and the USDC reference), and a held lane's step from the tick's
own batch; only network fees read a price elsewhere, SOL's. One price age
holds every valuation (`kaminoOracleMaxAgeSecs`); the reserve health window
is the adaptor's report window (`adaptorMaxReportAgeSlots`).

## Selector pricing

The selector prices a lane from the transactions the executor would send
(`selector_price.go`), never from a model of them:

- a leverage step is the up step `assembleStep` builds (borrow, convert,
  deposit in one transaction) and the down step that unwinds it; its cost is
  the origination fee, each conversion's quoted loss at the batch's prices
  and the two transactions' fees (`lanePricer.priceStep`). The level decision
  prices an up step this way;
- a flat vault's entry is the entry swap step's own quote, the deposit, the
  up step to 1.5x when the live spread rule levers the lane (the level
  decision still judges that step's gain against its cost), and the
  unwind: the down step and the atomic exit step that withdraws the
  collateral and sells it to USDC (`lanePricer.priceEntry`). Its cost is
  one expected figure (`MoveQuote.CostRaw`); execution bounds (a conversion's
  minimum output) stay on the transactions, simulated before they are sent.
  The entry is sized by the deposit room the tick's observation gives the
  lane (`topupDepositRoomUSDC`), and a lane whose obligation is absent
  without an initializer is no entry.
- a step the executor cannot assemble at the priced size (key budget, a
  policy leg cap) is `leverage_cost_unavailable(step_build)`, an outage that
  keeps the stored approval like any other.

Gone: the multi-leg entry recipe (twenty-odd separately priced messages with
receipt-rounding and debt-accrual windows), the six-transaction step pricer,
the per-token budget price observations and their reconstructed midpoints,
the bound and expected costs side by side, and the empty "source" recipe of a
flat vault.

A refresh the node could not run retries next tick; one Kamino rejects three
times in a row falls to the raw batch, whose health hold latches (an empty
lane still values as cash). An overflow lane nothing holds is refreshed with
the valued lanes; when Kamino refuses only its refresh, the valued lanes'
capture stands without it, its room is unknown, and the refusal never counts
toward that latch.

## Term

`debtTermEnd` is the one derivation of a borrow's maturity (its own term or
the reserve's market-wide maturity, unknown without a borrow time). The
keep-alive is the minimum up step: target() finds its room in the batch and
the chain refuses a borrow past the term or the room.

## Holds

A tick ends in a transaction or one of five holds; why goes in the hold's
cause and the log line, never the reason:

| Hold | Meaning |
|---|---|
| `at_target` | the vault is where target() puts it |
| `no_room` | the chain has no room for the move the target wants |
| `stale` | an input is not current or could not be read; the next tick observes again |
| `chain_refused` | the decided transaction's signed simulation failed (a policy, a spending limit, KLend, the adaptor's report age, a minimum output); the row fails with the simulation error |
| `stopped` | the operator's manual recovery latch |

The gauge `loyal_backyard_hold{lane,reason}` and the journal
(`recovery_reason` `hold:<reason>`, the cause in the row's `budgetHold`)
carry only these. A worker exit on a hold (after a broadcast, or joined with
a store fault) is counted under its cause on `loyal_family_failed_total`, so
the page names the check.

The policy literals are pinned by name (`policy_installed_test.go`): each is
the literal installed on chain, so a builder change that would leave a leg
without its installed account fails there.

## Deploying

Migration 0107 only drops the three lane-listing CHECK constraints on
`multiply_operations`; apply it before the binary (`loyal-migrate -db yield
-to 107 up`). The running binary is unaffected. Deploy with no operation in
flight, as before.
