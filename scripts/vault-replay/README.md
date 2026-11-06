# Native vault replay

These test-only runners use `solders==0.29.0` / LiteSVM and the real Go worker
compiler. They never start Node or a Solana validator. External networking must
be disabled at execution; every runner asserts the hard cgroup limits before
loading a bank. They seed hypothetical balances, use local test signatures and
do not submit transactions to a network.

Create a private temporary working directory. Set `VAULT_REPLAY_WORKDIR` to it,
install `requirements.txt` into its `deps/`, and compile the test driver there:

```
go test -p 1 -c ./internal/backyard -o "$VAULT_REPLAY_WORKDIR/offline-probe"
```

Run from `go/workers` for compilation. Compile and execute sequentially under
CPUQuota=50%, MemoryMax=768M, MemorySwapMax=0, TasksMax=64, RuntimeMaxSec=180,
GOMAXPROCS=1 and GOMEMLIMIT=512MiB. Add PrivateNetwork=yes for offline work.
The dependency install is a separate bounded preparation step. Do not run a
replay until those limits are in effect; never retry a resource failure uncapped.

Runner arguments are `CAPTURE.json EQUITY LEVERAGE`. Equity is 10000, 50000 or
100000; leverage is 1.5 or 2. The capture contains the route, its real account
bank, program versions and unsigned instruction templates. The driver compiles
with current source for every withdrawal portion and validates its local signed
wire. It applies the compute and heap limits from that unchanged V1 wire.

- `withdrawal.py`: seeded leveraged holding, complete protected unwind. Set
  `VAULT_REPLAY_CARRY_STRESS=1` to charge six hours at 100% APR upfront.
- `entry_exit.py`: real capped purchase wires first; all their pool changes
  survive into subsequent pricing/sale. Requires the capture's `buy` wire.
- `compare_quotes.py`: repayment-first quote calibration. The AUTO fixture's
  existing Manifest residual leg is used; no improved liquidity is seeded.

Outputs and subprocess inputs/logs stay in the private working directory.
`HELD_BEFORE_SEND` or `PRICE_REFUSED` is a protection outcome, not proof that a
withdrawal completed. `TEST_FAILED`, `EXECUTION_FAILED` and `STEP_LIMIT` fail.
Stop at the first unexpected failure. A complete strategy unwind is not a user
claim, a live borrow-capacity test, or a promise of future liquidity.

See `docs/evidence/investment-exit-admission-2026-11-06.md` for results and limits.
The machine-readable evidence manifest records the fixture and source hashes.
