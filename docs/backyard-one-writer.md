# Backyard: one writer, one path per tick

## One writer

Backyard has one writer: the `loyal-engine` process that holds the family
advisory lock (`pg_advisory_lock(hashtextextended('loyal-family:backyard', 0))`)
on `BACKYARD_DATABASE_URL`, a direct session for the life of the process. A
second process waits for the lock before it reads or writes anything; when the
holder's session ends it takes over. Losing the session cancels the process,
which exits so its supervisor restarts it into the wait.

Nothing below the lock checks who the writer is: there is no route lease,
fencing token, lease expiry, `state_version` compare-and-swap or planning
generation. The one-shot operator writes (`selector-evaluate --execute`,
`settle-manual-restore`) take the same lock without waiting, so they refuse
while the worker runs. `exit-lane` and `clear-hold` write their own rows
(`backyard_lane_exits`, `backyard_manual_recovery_latches`) and need no lock.

What the lock does not cover: a session the database drops while the
process has not noticed yet (a silent network partition; a terminated
backend notifies the client and cancels the process). The one-nonterminal
partial unique index still keeps one operation in flight, every status
transition is a compare on the row's status, and a signed wire resent by two
processes is the same transaction; but once that operation is terminal the
unaware process could decide a fresh one from its own observation. Writes do
not go through the lock's session.

## The tick

1. The manual recovery latch: set, the tick does nothing.
2. An operation in flight: it advances (land, or reconcile once final), and
   the tick ends.
3. Observe one confirmed batch, compute `target()`, `Decide` once.
4. A hold ends the tick (below). Otherwise prepare the decided action's
   request from the same batch (Jupiter quote, Hastra price, step sizing).
5. Journal the decision as a `decided` operation.
6. Sign the request's message, simulate that exact signed wire, and persist
   it with its simulation and expected effects in one statement
   (`PersistSigned`): the write-ahead record.
7. Land it: broadcast intent before the first send, then the same bytes until
   they confirm, fail on chain or expire. A crash after step 6 leaves a
   `signed` row; the next process lands exactly those bytes
   (`chain.OwnSignedWire` owns them by their hash).
8. Once final, reconcile the receipt against the persisted effects, once.

Nothing re-decides, re-reads the chain before signing, or re-checks the wire
it just encoded. The simulation of the signed wire, the Squads policy, KLend
and the request's own minimum output refuse a wrong transaction. A
simulation refusal is the `chain_refused` hold: the row fails with the
simulation error and the next tick decides again.

## Holds

A hold is not a journal row; only transactions are. Why a tick did not move
the vault is `loyal_backyard_hold{lane,reason}` on `/metrics` (`at_target`,
`no_room`, `stale`, `chain_refused`, `stopped`, docs/backyard-one-copy.md; no
series while it acts or lands an operation; a tick that fails leaves the last
value) and one log line when it changes, which carries the cause. `no_room` still counts on
`loyal_backyard_step_no_room_total`, where `LoyalBackyardSafetyStepNoRoom`
pages.

A manual recovery stop is the latch row alone: the decision writes it, a
latched tick reads it and writes nothing, `loyal-engine backyard clear-hold
--reason "<text>"` clears it and keeps the reason on the row. The previous
binary also derived a stop from the journal's newest `HOLD_MANUAL_RECOVERY`
and `HOLD_CLEARED` rows when no latch row was active; this binary reads only
the latch row (0076 backfilled it).

Custody is vault cash (`docs/backyard-atomic-step.md`): the vault's one
PYUSD account, shared by AUTO/AUTO/PYUSD, Ethena/USDe/PYUSD and
Prime/PRIME/PYUSD, is counted once in NAV and belongs to no lane. A step's
conversion excess left there is taken whole by the next step of a lane that
owes PYUSD; when a withdrawal needs it or no lane that steps owes it, it
converts to USDC.

A safety step (debt 0, maturity, hard LTV) the chain has no withdraw room for
holds `no_room` and keeps the lane's other rules waiting. The withdraw room
is known only from the step's refreshed simulation, not from the batch
`target()` reads, so letting other rules proceed would need the target to
guess it; the page is the answer, and only outside cash can repay then.

## Deploying

Deploy with no Backyard operation in flight (`loyal-engine backyard
in-flight` prints 0); `deploy/hetzner/activate-backyard.sh` waits for that,
stops the old process, then starts the new one.

1. Before the new binary starts: `loyal-migrate -db yield -to 104 up`
   (`--migrations-applied` for `scripts/deploy-workers.sh`). 0104 only
   relaxes the finalized-failure lifecycle check (it no longer requires the
   old bind record); the running binary is unaffected. It holds an exclusive
   lock on `multiply_operations` while it validates the table (seconds).
2. Deploy the new binary.
3. Once it is live: `loyal-migrate -db yield up` applies 0105 (drops the move
   machines' old route-state keys, which no binary since the target binary
   reads) and 0106 (deletes the HOLD rows). Applying them early is harmless
   (the previous binary reads none of those keys and writes new HOLD rows);
   after 0105 a rollback can go to the target binary (#361) but not to the
   atomic-step binary (#360).

Before the deploy, check the previous binary's journal-derived latch finds no
stop (a `HOLD_MANUAL_RECOVERY` row newer than the newest `HOLD_CLEARED`):
this binary would not see one.

A rollback to the target binary (#361) needs no operation in flight: it
cannot send a row this binary signed (it has no bind record, so it holds
that row as `operation_not_bound` every tick and decides nothing). The
activation script's rollback stops the new binary and refuses to start the
old one while `in-flight` is not 0. #361 reads the latch row this binary
writes; a latch this binary cleared can re-arm under #361 if the journal
still ends in a genuine stop.
