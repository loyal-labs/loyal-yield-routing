# Fleet opportunity planner source-count verifier

Run this verifier from a clean checkout:

```sh
bash scripts/verify-fleet-opportunity-planner-source-count.sh
```

Required conditions:

1. Neither fleet-source SQL variant performs a correlated anti-join from
   `planning_vaults` back into the materialized `sources` CTE.
2. `no_positive_current_source_vault_count` is derived from the already-returned
   eligible, active-exclusion, and distinct-source-vault counts with checked
   arithmetic.
3. Focused tests prove the normal, zero-source, duplicate-source, and invalid
   partition cases.
4. Formatting, the focused tests, and the planner binary check pass.
5. A read-only production verifier confirms that the repeated `sources` rescan
   is gone, the planner's live completeness partition still passes, and
   execution time plus temporary reads are materially lower than the recorded
   2.54-second / 1,008,793-temp-block baseline.

Run condition 5 with the normal 1Password environment mounted:

```sh
op run --env-file=.env.1password -- \
  bash scripts/explain-fleet-opportunity-planner-source-count.sh
```

Verdict: `PASS` only when the script passes and condition 5 is recorded from a
read-only production transaction. Otherwise the verdict is `FAIL`, with the
failed condition reported.
