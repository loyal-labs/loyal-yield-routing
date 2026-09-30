# Backyard V2 handoff fixture (synthetic only)

Run from repo root:

```
python3 scripts/hetzner-backyard-handoff-fixture/run.py --pg-bindir /opt/homebrew/opt/postgresql@17/bin --ack-isolated
```

Creates/removes a private-socket PostgreSQL cluster, clears inherited credentials,
uses offline Go modules, runs only TestBackyardFinancialHandoffFixture under race.
No external RPC, signer, OP, funding, production DB or passing lease-suite rerun.

Actual Worker.Run/Tick, LoadNonterminal, AdvanceNonterminal, lease fencing,
ReservePhase3, build authorization, wire binding and repriced broadcast-intent
methods run against PostgreSQL. Existing test-only AuthorizePhase3Build and
budgetBuildRPC are reused. The wire is deliberately unsigned; the RPC transport
checks exact bytes and durable intent before returning cancellation/lost response.
Fresh DB/Worker objects with a new deployment ID exercise recovery twice without
resend; a returning predecessor is refused by actual DB methods. RAW lines report
shutdown duration, send count, fencing tokens, complete retained budget/journal.
JSON output hashes all Go inputs plus this runner/schema/docs.

Boundary: projected schema only (0051/0053 lease coherence/FK, wire identity,
0070 lifecycle submission evidence and nonterminal/signature uniqueness subset).
Not full schema replay; omits full action/lane, snapshot, reconciliation, custody,
policy and pilot constraints. Does not change migration 71 or diagnose its known
fresh-schema blocker. Context cancellation is the worker's SIGTERM cancellation
path, not an actual OS signal. Restart means fresh connections/runtime, not exec,
host reboot or power loss. Observation, new admissions, latch and signing are
outside the supplied narrow tickRuntime; no full productionTickRuntime claim.

Local Darwin execution blocker: initdb fails shmget(size=56) with ENOSPC, including
shared_memory_type=mmap. Minimum dependency is one available local SysV shared
memory ID/segment, or an independent isolated PostgreSQL-capable rehearsal host.
Do not remove another process's segments. Race compilation succeeds with this
case skipped; that is not DB evidence. The parent subsequently executed both
cases successfully in the isolated Linux adaptation below. Other financial worker families and chain receipt/custody,
expiry settlement, Linux SIGTERM/process restart remain unproven.

## Linux disposable-container adaptation

Go 1.25.1 is required by go.mod. Dockerfile uses public golang:1.25.1-bookworm
and postgres:17-bookworm tags; record resolved image identities from build logs
and lane-b-image-inspect.json. Tags are not asserted immutable. Dependencies use
proxy.golang.org, sum.golang.org and deployed go.sum; go mod verify runs at build.
Runtime is nonroot, read-only, network-none, no ports, volumes or host IPC.

Create the allowlisted archive locally (contains current identity patch):

```
python3 scripts/hetzner-backyard-handoff-fixture/package.py /tmp/backyard-handoff-linux.tar.gz
```

Parent uploads archive and adjacent manifest to its authorized disposable host
5.161.121.229 using its existing SSH identity, checks the archive SHA256, extracts
into a new private lane-b directory, then from that extracted root runs:

```
sh scripts/hetzner-backyard-handoff-fixture/linux-parent-run.sh
```

Only image loyal-lane-b-handoff:fixture and container
loyal-lane-b-handoff-fixture belong to this command. A preexisting name causes
failure; no unrelated containers or IPC resources are removed. Builder timeout
600s; runtime timeout 300s, Go case timeout 90s. Retain lane-b-result.json,
lane-b-stderr.log, lane-b-exit.txt, image inspect and source archive manifest.
Parent Linux execution completed with exit 0 against the reviewed source archive
`80c2d464dfbf191f02ef30bd89317bf156182c817b5bb4cae66dfa4780659d1c`.
Both cancellation and lost-response cases observed one synthetic send, fenced
predecessor rejection, and unchanged unknown-wire journal/reservations after
successor recovery. Raw output, exit status and image metadata are retained in
the canonical migration package under `rehearsal/backyard-linux-raw-artifacts/`;
its outer archive SHA256 is
`c66e18e04fde679c49dbbcc22eddd7318769224883de604bc301a0a78891b559`.
These measurements do not prove actual OS signal/exec recovery, a valid on-chain
wire, receipt/custody reconciliation or other financial families. The rehearsal
host is disposable; this example address is not accepted production placement.

Packaging provenance: package.py requires HEAD to equal or descend from deployed
source f821a78f6a5c0507eb1dada10fd300df1277f639. It records that baseline as
`deployed_source_commit`, exact HEAD as `checkout_commit`, and allowlisted byte
hashes including `code_hashes`. HEAD alone does not identify uncommitted patches.
Optional `--checkout-commit <full SHA>` pins the caller's expected build checkout.
This packaging-only revision does not invalidate or extend the behavioral scope
of the separately retained parent-tested archive 80c2d464...d1c.

## Lane B actual SIGTERM extension (pending parent execution)

The current archive adds `os-sigterm` as a third case. A sanitized child test
process opens only the runner's private socket/database and runs Worker.Run.
Its fake transport checks exact persisted synthetic wire and committed broadcast
intent, prints its actual lease token, then blocks until request cancellation.
The parent sends OS SIGTERM, requires exit status zero within five seconds and
reaps the child before acquiring the successor lease. The helper uses
signal.NotifyContext, matching the production command's signal mechanism without
invoking its signer/config startup. The successor runs two recovery ticks; the
existing retained journal/reservation checks and stale predecessor write/release
refusals apply to this case too. Successor is an in-process runtime, not another
exec. The predecessor refusal is checked using its captured token after exit.
No cryptographic signature validity, chain receipt, expiry, full-family drain,
reboot, power-loss durability or production acceptance is claimed.

Both parent and helper reject non-runner DSNs before connection (fixed database,
no authority/user, only `/tmp/backyard-handoff-*/socket`). PostgreSQL now retains
its default fsync setting; runtime storage remains disposable tmpfs. The runner
requires three RAW cases, exit zero and no skipped cases for its narrow verdict.
Earlier retained two-case evidence does not certify this extension.

Exact parent recipe from this worktree (no host action performed by Lane B):

```sh
python3 scripts/hetzner-backyard-handoff-fixture/package.py /tmp/backyard-sigterm.tar.gz --checkout-commit "$(git rev-parse HEAD)"
shasum -a 256 /tmp/backyard-sigterm.tar.gz /tmp/backyard-sigterm.tar.gz.manifest.json
```

Upload both files through the parent's authorized transport, verify both hashes,
extract into a fresh private directory, and run the existing
`linux-parent-run.sh` from the extracted root. Keep the source archive/manifest,
image inspect, result JSON, stderr and exit files together. Packaging normalizes
archive ownership, modes, timestamps and gzip metadata; identical inputs produce
identical archive bytes. Public base-image tags still require recording resolved
identities. No prior rehearsal address is assumed current or authorized.

Local checks: targeted Go compilation/no-DB skip checks and the existing unsigned
bridge serialization test; Python syntax and shell syntax. These provide no
PostgreSQL or OS-signal measurement. Parent Linux execution is outstanding.
