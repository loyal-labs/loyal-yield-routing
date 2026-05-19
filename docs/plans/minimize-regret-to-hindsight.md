# Minimize Regret To Hindsight

## Goal

The live routing strategy cannot match hindsight APY by construction. The hindsight path sees future reserve APY, future route costs, and future reserve eligibility. The live system should use hindsight as a benchmark and measure the gap as regret.

The objective is a future-blind strategy that stays near the hindsight path by learning which APY moves are likely to last long enough to pay for the transaction.

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

### Hindsight Oracle

Run an offline dynamic program after the fact to compute the best possible path with realized APY and realized costs. The oracle should report strategy APY, hindsight APY, regret, missed switches, bad switches, and cost drag.

### Online Policy Simulator

Replay history as if the strategy was blind to the future. The simulator should compare short trailing means from 10 minutes through one hour, exponentially weighted moving averages, volatility-adjusted APY, persistence classifiers, dynamic switching thresholds, and expected holding horizons from 20 minutes through six hours.

### Decision Explainer

For every simulated or live rebalance, log the current reserve, candidate reserve, expected APY edge, expected holding period, estimated switching cost, net expected gain, model confidence, realized result, and hindsight decision. This turns missed APY into cases we can inspect.

## Strategy Shape

The likely production strategy is a baseline allocator plus a spike detector plus a cost-aware online learner.

The baseline allocator chooses a stable home reserve using a three-hour to six-hour signal. The spike detector watches faster 10-minute to 20-minute APY acceleration. A persistence model estimates whether the spike will last long enough to justify a move. The cost model executes only when the expected edge clears costs and an uncertainty buffer.

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

## Hindsight Imitation Dataset

Build a hindsight-imitation table with one row per timestamp. Each row should include the reserve chosen by hindsight, the 20-minute policy, the six-hour policy, the current-APY policy, and the candidate model. It should also include the realized regret of each choice.

The model can then learn from hindsight labels while using only features that existed at the decision timestamp.

## Success Criteria

Judge the strategy by regret and APY together. Track live-policy APY, hindsight APY on the same window, regret in basis points, share of hindsight switches captured, bad-switch rate, missed persistent-spike rate, cost drag, stale-reserve time, cross-mint quote loss, and failed-execution rate.

The near-term test is whether a short-window signal with persistence filtering and expected-hold cost modeling closes more of the gap than the plain 20-minute trailing mean. If it does, the next step is a simple persistence classifier and a dynamic switching threshold.
