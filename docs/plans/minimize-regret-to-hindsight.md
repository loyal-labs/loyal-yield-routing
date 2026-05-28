# Minimize Regret To Hindsight

## Goal

The live routing strategy cannot match hindsight APY by construction. The hindsight path sees future reserve APY, future route costs, and future reserve eligibility. The live system should use hindsight as a benchmark and measure the gap as regret.

The objective is a future-blind strategy that stays near the hindsight path by learning which APY moves are likely to persist enough to pay for the transaction.

## Historical Data

Collect reserve data at the same cadence the strategy can act, or faster. For a five-minute router, the raw stream should capture one-minute to five-minute observations without lossy aggregation.

| Category | Fields to store |
| --- | --- |
| Reserve state | Supply APY, borrow APY, utilization, deposit TVL, supplied amount, borrowed amount, available liquidity, reserve mint, market, caps when available |
| Oracle and timing | Oracle price, confidence, fetch timestamp, observed latency, source freshness |
| Routing quotes | Jupiter quote for plausible cross-mint pairs, expected output, price impact, route used, quote staleness |
| Execution | Simulation result, priority fee estimate, compute estimate, realized output for executed swaps, failure reason |
| Labels | Forward APY over five minutes, 10 minutes, 20 minutes, one hour, three hours, and six hours |
| Benchmark labels | Hindsight-best reserve, realized regret for each action, whether a switch beat staying after costs |

The most useful label is whether the later data shows that a switch was worth making. A high current APY alone is too weak as a training target.

## Hypotheses

### APY Spikes Have Persistence Signatures

Some high-APY prints last for one snapshot. Others are early signals of a regime that can be monetized. The strategy should classify spike quality before routing capital.

The classifier should look at current APY, recent slope, 20-minute mean, six-hour mean, volatility, TVL, utilization, spread over the current reserve, spread over the second-best reserve, reserve identity, market identity, and time since the spike began.

### Short Windows Find Opportunity

The 20-minute signal reacted better than the six-hour signal in the five-minute backtest, but it only closed part of the gap to hindsight. A live policy should use short windows for entry detection and longer windows for confidence or baseline allocation.

### Spread Matters More Than Raw APY

A high APY is attractive only when the expected future edge beats the current reserve by enough to cover lamport costs, Jupiter quote loss, failed-execution risk, and a safety buffer.

### Markets Have Different Spike Quality

Some markets appear to produce persistent APY regimes. Others produce transient spikes. The strategy should learn market-level and reserve-level priors before treating a new high APY print as worth a rebalance.

### Execution Costs Set Frequency

Even small switching costs matter when the expected edge lasts only a few minutes. The decision rule should evaluate expected net value over the likely holding period.

## Instruments

### Live Reserve Tape

Store an append-only stream of reserve observations, quote observations, and decision inputs. The raw tape should make every live decision replayable without calling the API again.

In this repo, `crates/loyal-yield-router` is the lean TimescaleDB boundary for routing inputs. It connects to the existing Kamino TimescaleDB, queries reserve rows, catches up by the durable cursor `(observed_at, slot, reserve)`, and listens for reserve-update notifications. Quant policy, eligibility, scoring, shadow decisions, and offset persistence should live in separate router or strategy crates that consume these rows.

### Hindsight Oracle

Run an offline dynamic program after the fact to compute the best possible path with realized APY and realized costs. The oracle should report strategy APY, hindsight APY, regret, missed switches, bad switches, and cost drag.

### Online Policy Simulator

Replay history as if the strategy was blind to the future. The simulator should compare short trailing means from 10 minutes through one hour, exponentially weighted moving averages, volatility-adjusted APY, persistence classifiers, dynamic switching thresholds, and expected holding horizons from 20 minutes through six hours.

### Decision Explainer

For every simulated or live rebalance, log the current reserve, candidate reserve, expected APY edge, expected holding period, estimated switching cost, net expected gain, model confidence, realized result, and hindsight decision. This turns missed APY into cases we can inspect.

## Strategy Shape

The likely production strategy is a baseline allocator plus a spike detector plus a cost-aware online learner.

The baseline allocator chooses a stable home reserve using a three-hour to six-hour signal. The spike detector watches faster 10-minute to 20-minute APY acceleration. A persistence model estimates whether the spike can justify a move. The cost model executes only when the expected edge clears costs and an uncertainty buffer.

Decision rule:

```text
switch if:
  expected_future_net_value(candidate)
  >
  expected_future_net_value(current)
  + switching_cost
  + uncertainty_buffer
```

Expected future value should be evaluated over a predicted holding period instead of only the next five-minute interval.

## 0 To 1 Implementation Plan

The first implementation should be a rules-based expected-edge router. The system should observe every five minutes, score every eligible reserve, and execute only when the candidate reserve has enough expected edge to pay for costs over the likely holding period.

| Step | Build | Done when |
| --- | --- | --- |
| 1 | Live reserve tape with raw Kamino reserve metrics, current smart-account position, and timestamped fetch metadata | A full day can be replayed from local data without calling Kamino again |
| 2 | Quote tape for cross-mint candidates with Jupiter output amount, price impact, route, context slot, and quote age | Every cross-mint decision can explain the exact cost used by the scorer |
| 3 | Eligibility filter for stablecoin allowlist, TVL floor, APY cap, available liquidity, reserve caps, stale data, and quote availability | The router produces a bounded candidate set before it scores yield |
| 4 | Expected-edge scorer using 20-minute EWMA, one-hour mean, six-hour mean, volatility penalty, staleness penalty, transaction cost, priority fee, and Jupiter quote loss | Each candidate has a net expected value over the configured holding period |
| 5 | Shadow runner that logs the best reserve, current reserve, switch decision, skipped reason, expected edge, and expected cost every five minutes | The team can inspect one week of decisions before funds move |
| 6 | Execution MVP with a 20-minute rebalance cooldown, same-mint moves enabled first, and cross-mint moves behind a larger edge threshold | The smart account can move funds through the approved Kamino reserves while preserving the guardrails |
| 7 | Daily hindsight audit on the same tape | The report shows strategy APY, hindsight APY, regret, bad switches, missed switches, and cost drag |

## MVP Policy

Start with a simple policy that can be explained line by line. Monitor every five minutes. Use a one-hour expected holding period. Keep a 20-minute cooldown after any successful rebalance unless the current reserve becomes ineligible.

The candidate APY estimate should blend three signals: a 20-minute EWMA for fresh movement, a one-hour mean for short-term persistence, and a six-hour mean for stability. Penalize candidates with high short-window volatility, stale observations, low available liquidity, or missing quotes. Same-mint moves should use only the reserve-change transaction cost. Cross-mint moves should use the live Jupiter quote loss plus transaction and priority fees.

The switch test should be:

```text
gross_edge_usd =
  position_value
  * (exp((candidate_predicted_apy - current_predicted_apy) * holding_period_years) - 1)

switch if:
  gross_edge_usd > all_switching_costs_usd + uncertainty_buffer_usd
```

The first uncertainty buffer can be conservative and static. A good starting point is the larger of two values: twice the estimated execution cost, or the value of 10 annualized basis points over the holding period. This should reduce churn while we collect live evidence.

## Data Contracts

The live tape should store small append-only records. These records are enough to replay the router, compute hindsight, and explain every decision.

| Record | Required fields |
| --- | --- |
| `reserve_snapshot` | Timestamp, slot, market, reserve, mint, supply APY, borrow APY, utilization, TVL, total supply, total borrow, available liquidity, caps, oracle price, source latency |
| `quote_snapshot` | Timestamp, input mint, output mint, input amount, output amount, price impact, route labels, context slot, quote latency, quote age, route availability |
| `position_snapshot` | Timestamp, smart account, current reserve, current mint, deposited amount, estimated USD value, last rebalance timestamp |
| `decision_snapshot` | Timestamp, current reserve, candidate reserve, predicted current APY, predicted candidate APY, holding period, expected gross edge, estimated costs, buffer, decision, skipped reason |
| `execution_snapshot` | Timestamp, transaction signature, simulated result, landed result, compute units, priority fee, base fee, realized output, failure reason |
| `hindsight_snapshot` | Timestamp, hindsight reserve, live-policy reserve, hindsight value, live-policy value, regret, missed-switch flag, bad-switch flag |

## V2 Database Boundary

The V2 database boundary lives in `crates/loyal-yield-router` instead of the production action SDK. `TimescaleRouterClient` uses SQLx for explicit Timescale/Postgres reads from the existing Kamino schema, exposes latest-reserve and historical-update queries, returns typed reserve rows with their `(observed_at, slot, reserve)` cursor, and provides a `LISTEN/NOTIFY` stream that catches up missed rows before yielding live notifications. This crate should not own strategy defaults such as stablecoin allowlists, APY caps, leader selection, scoring, decision snapshots, or execution behavior.

## Research Backlog

The first research loop should tune the rules before adding a model. Sweep the holding period across 20 minutes, one hour, three hours, and six hours. Sweep short signals from 10 minutes through one hour. Test whether the one-hour expected-hold scorer beats the plain 20-minute trailing mean from the five-minute backtest.

After that, research market priors and spike persistence. The key question is whether certain markets or reserves produce APY spikes with durable follow-through. If the answer is yes, replace the hand-tuned predicted APY blend with a small persistence model trained on the hindsight-imitation table.

Cross-mint routing should stay behind a stricter gate until there is enough live quote history. The first production policy can prefer same-mint routing and allow cross-mint moves only when the live quote loss is small and the expected edge remains positive after a larger buffer.

## Hindsight Imitation Dataset

Build a hindsight-imitation table with one row per timestamp. Each row should include the reserve chosen by hindsight, the 20-minute policy, the six-hour policy, the current-APY policy, and the candidate model. It should also include the realized regret of each choice.

The model can then learn from hindsight labels while using only features that existed at the decision timestamp.

## Success Criteria

Judge the strategy by regret and APY together. Track live-policy APY, hindsight APY on the same window, regret in basis points, share of hindsight switches captured, bad-switch rate, missed persistent-spike rate, cost drag, stale-reserve time, cross-mint quote loss, and failed-execution rate.

The near-term test is whether a short-window signal with persistence filtering and expected-hold cost modeling closes more of the gap than the plain 20-minute trailing mean. If it does, the next step is a simple persistence classifier and a dynamic switching threshold.
