# Voltr RWA selector: implementation plan

Status: implementation authorized by the user. Local shadow and safety work is
under way; see [implementation status](voltr-rwa-selector-implementation.md) for
completed changes and remaining gates. Live rollout is not completed or
authorized by this document alone. This replaces
the architecture proposed in the accompanying discussion. Existing activation
contracts, installed policies and operational limits retain their authority.

## 1. Outcome and first-release boundary

Run one Go worker that observes the vault's real holdings, chooses one worthwhile
next action, executes through the existing transaction machinery, and reconciles.
An engineer should be able to explain every owned balance and every proposed
transaction without knowing the project's implementation history.

The selector pilot supports one active leveraged loop plus idle USDC, using the
reviewed PRIME/USDC, syrupUSDC/USDC and ONyc/USDC bindings as each becomes fully
admitted. Each lane uses its execution-tested leverage setting. Missing policy,
funding, recreation or exit prerequisites make a lane unavailable for entry.
Discovery of a market does not admit it for execution.

This pilot can keep a good existing position when the lane closes to new capital,
or rotate to another worthwhile executable lane. It cannot keep A and place
overflow into B simultaneously. Productive overflow requires a separate extension
for multiple positions, aggregate NAV and partial withdrawals. The existing
all-eleven-lane activation contract is not reduced or fulfilled by this pilot.
Any live pilot must fit the applicable authorized scope and limits; resolve an
actual scope mismatch before activation. A pilot PASS cannot waive existing gates.

## 2. Three kinds of information, with one owner each

| Information | Owner | Rule |
| --- | --- | --- |
| Holdings, debt and withdrawal receipts | Confirmed chain observations | Database snapshots are projections; desired allocation never selects which owned assets count. |
| A transaction that may have executed | Existing operation journal | Recover its exact transaction before planning new execution. Preserve signed bytes and historical evidence. |
| Work we have committed to finish | Existing route row | Persist only the bounded intent needed across restarts; no second switch journal or portfolio ledger. |

Use collateral, debt, cash, custody, deposit, borrow, repay and withdraw throughout
new code. A route supplies exact accounts, token units, policy bindings and limits.
Keep genuine protocol differences in the existing protocol modules. Translate
historical action names once when decoding old journal rows, preserving their
original identity and signed transaction; do not translate live actions back and
forth through PRIME terminology.

Keep Voltr's reported book, tracked custody and independently observed strategy
assets distinct. Their reconciliation is a real protocol requirement. Simplifying
names must not remove the accounting identities or double-count staged cash.
Observe residual assets and obligations until reconciled, including those belonging
to a previous lane. Unexpected exposure prevents new allocation.

The pending intent needs source, original admitted amount/cost bounds, reason,
policy/observation references, existing budget reservation references and progress
that cannot be recovered from the transaction journal. The existing budget module
owns remaining headroom; the intent must not maintain a second remaining balance.
A candidate destination is advisory until entry admission. Persist the intent once
in the route row; derive the next step from actual holdings.

## 3. One worker loop

```text
Respect durable recovery holds; recover any outstanding transaction first.
Observe holdings, withdrawal receipts, protocol state and approved limits.
If required evidence or accounting is inconsistent: hold with a concrete reason.

Choose one next action:
  1. Reduce unsafe exposure when a valid repayment path exists.
  2. Restore a genuine withdrawal shortfall.
  3. Continue an already-admitted unwind.
  4. Invest eligible idle capital or start a worthwhile economic move.
  5. Otherwise hold.

Revalidate the action using fresh chain state, policy admission and simulation.
Persist the exact transaction before broadcast.
Execute, confirm, reconcile actual effects and satisfy required NAV reporting.
Repeat.
```

The pure decision function returns one next action or a hold reason. Existing
helpers build and execute that action. Preserve the lease/fencing, single
nonterminal-operation invariant, persist-before-send and exact-effect recovery.
Keep existing accounting gates and permitted emergency ordering authoritative.
An unavailable economic API does not block independent checks using fresh chain
evidence; it also never permits bypassing stale oracle, custody or policy gates.
Unsafe exposure without an executable repayment path requires an explicit hold
and operational escalation, not a claim of autonomous recovery.

Do not retarget an unresolved transaction. Withdrawal demand can interrupt future
investment at a reconciled boundary; it cannot erase a pending submission or
abandon a required accounting or risk-reduction step. Intent expiry prevents new
risk, while authorized reconciliation and required safe unwinding remain possible.

## 4. Withdrawal and switching semantics

Preserve the user's actual withdrawal requirement. Compute the shortfall against
usable Voltr idle USDC, counting existing claims and other commitments once.
Do not substitute total strategy value for withdrawal demand.

First avoid any unwind when idle covers claims. For the pilot, an uncovered
request may use the existing proven full-unwind recipe: represent that as the
chosen action and include its cost explicitly. Partial unwinds are additional
behavior to prove. Set any idle buffer from the intended withdrawal service level
and observed demand; borrowed cash and repayment reserves are not free capital.

For an economic rotation, first admit a bounded source unwind. Finish its safe
steps without restarting the plan on every rate tick. Before entering another
lane, prove source debt is zero and no source collateral exposure remains except
explicitly permitted, bounded and valued residuals. Observe all remaining custody
and reconcile NAV. An empty obligation may remain open; do not close it merely
to simplify the planner. Then refresh
destination economics, prerequisites, capacity and quotes; enter an admitted lane
or leave funds safely idle. Never edit the manifest to hide the old position.

Full exits can close Kamino obligations. Admission must include the approved
prerequisite-recreation path for later entry. A one-way A-to-B migration does not
establish continuous rotation.

## 5. Small economic decision, existing data

Reuse `kamino.latest_verified_reserve_updates` and the existing collector. Add a
bounded, cached Kamino batch-stats enrichment adapter for native token yield and
eligible incentives. Reuse useful scanner parsing and arithmetic after checking
their units and current contracts; keep artifact-writing research CLIs outside
the worker. Maintain one production economic rule implementation in Go. Historical
TypeScript scenarios can supply fixtures; do not maintain two authoritative engines.

Compare the expected whole-vault outcome of keeping actual holdings with the
outcome after a proposed move. Include destination capacity and idle remainder,
actual debt, borrowing cost after our proposed utilization change, setup costs,
the actual bounded transaction sequence, swaps, fees and an uncertainty margin.
Use consistent rate conventions over one explicit evaluation horizon. Do not
infer recurring RWA yield by annualizing an unexplained market-price premium.

Known zero, unknown and unlimited are distinct values. Missing essential economic
evidence blocks economic entry. Unproven incentives contribute no assumed reward;
their omission is recorded. Validate feed identity, timestamp and eligibility.

Require a persistent economic advantage and sufficient net dollar benefit to
justify switching. Check capacity immediately before execution; it need not have
been open throughout the economic persistence window. Closed-to-entry is not a
reason by itself to exit an otherwise acceptable position. Quote only credible
moves at their actual size. Choose polling and thresholds from the shadow run;
keep the faster safety/withdrawal path independent of slow enrichment requests.

Entry admission includes the exact pair's restrictions, current liquidity and
swap depth, a funded exit recipe, and remaining policy/spend room for that exit.
Reuse existing durable budget accounting. It reserves our permitted spending;
it cannot reserve future Kamino or swap liquidity. Revalidate every step.

## 6. Ordered changes with visible completion

| Change | Existing home | Completion evidence |
| --- | --- | --- |
| Establish one release baseline | Strategy-two manifest, activation evidence, worker deployment | One compatible source/manifest/program/policy/database/image set; current lifecycle canary and accounting reconciled. |
| Normalize vocabulary and compatibility | `state.go`, `decide.go`, `route_runtime.go`, journal decoder in `store.go` | New decisions/builders use canonical actions; historical operations still decode and recover with unchanged effects. |
| Retire historical incident exceptions | Existing journal resolution metadata and a focused migration if needed | Evidence-backed resolved disposition, preserving original failure/history; ordinary unresolved-operation guard works without an embedded signature exception. |
| Separate deployment limits from campaign history | `config.go`, manifest validation, existing budget module | Canary and ordinary operation share admission logic; spent/reserved values and window/scope continuity survive migration/restart. No new headroom or authority appears. |
| Keep withdrawal demand truthful | `decide.go`, withdrawal observation | Covered claims cause no unwind; uncovered claims choose an explicit proven unwind; amounts remain truthful through reconciliation. |
| Add economic choice and dynamic lane progression | One pure planner beside `decide.go`, existing route row, observation and worker modules | Source assets remain observed; bounded moves resume; destination entry is freshly admitted; A-to-B-to-A works. |

Source warning: the research reviewed newer `fleet/integration` work at `0058abf`
alongside an older dirty checkout and an older suspended image. Resolve the
authoritative implementation baseline before code changes. Do not assemble a
release by assuming those generations are interchangeable. The September 15
read-only observations are research evidence, not future deployment readiness.

Make vocabulary/compatibility changes behavior-preserving. Review withdrawal
changes and dynamic selection separately from that cleanup. Never delete an
incident exception or change budget configuration before its replacement and
state migration are demonstrated. Program identity, custody constraints and hard
safety bounds remain reviewed controls, not freely adjustable tuning parameters.

Data enrichment and shadow decisions can proceed while deployment prerequisites
are completed. Do not require a general refactor of unrelated fleet workers,
all-market support, a new adaptor, or an SDK-wide upgrade for this selector unless
a concrete compatibility check proves that dependency necessary.

## 7. Verification and delivery

Use the existing test and verifier surfaces; add focused cases protecting behavior.
Preserve applicable ABI/Squads checks when those surfaces change. This plan does
not create another verification framework or redefine earlier contracts' PASS.

| Scenario | Required result |
| --- | --- |
| Current lane full; position remains appropriate | Keep it; prevent unsupported additional entry. |
| Destination partly available | Compare invested amount plus idle remainder; never apply its return to the entire vault. |
| A to B to A, then user withdrawal | Prerequisites recreated where needed; holdings, receipt/custody and NAV reconcile at each stage. |
| Destination closes after source unwind | Fresh alternate admission or idle; no stranded unaccounted assets or stale destination promise. |
| Withdrawal arrives during rotation | Finish/recover the current transaction, then serve claims before new investment. |
| Restart after signing or ambiguous broadcast | Recover the exact transaction; no duplicate effect or reset spending allowance. |
| Economic feed missing; chain evidence usable | No economic entry/switch; independent permitted safety/withdrawal work remains available. |
| Incoherent accounting or no executable safe exit | Existing hold/escalation remains visible; no fabricated NAV or bypassed gate. |
| Historical rows and limits migrated | Old transactions remain recoverable, resolved incidents auditable, and spent/reserved bounds conserved. |

Deliver in this order: compatible operating baseline; small code/data cleanup;
live read-only shadow decisions; bounded selector canary; then authorized worker
rollout. Use controlled scenarios for capacity races that cannot be reliably
induced live, and label them separately from real transaction evidence.

Report source checks, simulation, deployment and live reconciliation separately.
The selector is live-complete only after current-image repeat rotation, withdrawal
and recovery evidence pass within the approved envelope. A running process,
plausible ranking, or historical lifecycle alone is insufficient.

## 8. Review questions for every change

- Does this model a real balance, obligation, user request or unresolved action?
- Can it live in an existing owner instead of adding another stateful component?
- Is route-specific code required by the protocol, or inherited from a prototype?
- Are compatibility and old incident handling confined to explicit boundaries?
- Can the next transaction be explained from current facts and one bounded intent?
- Does this change preserve recovery, accounting and the approved spending limits?

If a proposed abstraction cannot answer those questions, leave it out. The target
is a direct, auditable money-moving loop with fewer special cases.
