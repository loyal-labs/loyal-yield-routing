# Realtime relocation contract

`NEON_DATABASE_URL` remains a direct PostgreSQL connection; pooled Neon URLs
are rejected before connecting. Authentication, token rotation, event IDs and
replay semantics remain unchanged.

`REALTIME_RETENTION_CLEANUP_ENABLED` accepts only `true` or `false`, defaulting
to `true` for existing deployments. Set every overlapping target reader to
`false` while the source owns cleanup. Stop/disable source cleanup and verify
its process is inactive before assigning `true` to exactly one successor.
The switch is an operator ownership control, not a distributed lock; an old
source binary does not participate in a new locking protocol.

SIGTERM/SIGINT closes SSE streams without emitting an event ID or changing the
client cursor. Clients reconnect using existing Last-Event-ID replay. HTTP drain
is bounded to 45 seconds; the outer stop grace must remain at least 60 seconds.
No proxy buffering or caching; forward Authorization, Origin and Last-Event-ID.
Proxy stream timeouts must exceed REALTIME_HEARTBEAT_SECONDS (default 15).

Events admission (high-water lookup and replay, including pool acquisition) is
cancelled on shutdown and bounded to 10 seconds. Every pooled SQL connection
also has a 10-second statement timeout to bound server-side work after an
admission future is dropped. A shutdown cancellation returns 503 before SSE
headers and does not emit a database-failure operational alert.

The original smoke writes durable events and still uses Neon HTTP; do not run
it for this rehearsal. The guarded copy at
`scripts/hetzner-realtime/verify-sse-fixture.ts` uses Bun SQL over direct TCP.
It requires REALTIME_FIXTURE_ONLY=true, loopback HTTP and a loopback PostgreSQL
DB named realtime_fixture (or realtime_fixture_<suffix>). URL query options
are rejected. Connection parameters are explicit and do not inherit PGHOST or
PGDATABASE; localhost is pinned to 127.0.0.1. The same reserved SQL connection
checks current_database(), server address/port and the database comment
`loyal-realtime-disposable-fixture` before every write. Unmarked DBs are
rejected. HTTP redirects are forbidden. Use only synthetic credentials.

## Reproducible local HTTP harness

Run from the assigned realtime worktree. PostgreSQL shared memory and loopback
networking may require sandbox escalation. Prerequisites: PostgreSQL 17 (full
server tools, not libpq alone), Bun, Python 3, and the offline Cargo cache.
The wrapper rebuilds the crate and runs the nine existing Rust tests. Supply
an output directory that does not already exist:

```sh
cd /Users/user/loyal/.worktrees/hetzner-realtime
bash scripts/hetzner-realtime/run-scratch.sh /private/tmp/realtime-review-001
```

`run-scratch.sh` creates a fresh private cluster and OS-selected loopback DB/HTTP
ports; it never attaches to an existing service. It emits the exact extracted
fixture DDL, non-secret environment inputs, build/test logs, raw SSE transcript,
server logs, SQL identity/negative probes, measurements and cleanup results.
Its EXIT/INT/TERM trap stops PostgreSQL, stops verifier children, and removes
the scratch cluster. Evidence logs are retained. A failed run does not produce
a PASS evidence report. Each run uses a new evidence directory.

The wrapper re-execs with only PATH/HOME for the Rust build/cache. Runtime
server and smoke children receive an explicit PATH plus validated loopback
NEON/REALTIME URLs, synthetic HMAC key and fixture flags; no inherited OTEL,
RPC, signing keys, PGOPTIONS/PGSERVICE or HTTP proxy bindings. Every psql
invocation uses only PATH and PGPASSFILE=/dev/null (-X disables psqlrc, -w
prevents prompting). Cluster tools additionally use LC_ALL=C for macOS startup.
Ports must be explicit integers 1..65535. Before the first write the actual
current_database, server address/port and disposable marker must match.
Standalone verifier invocations also use these explicit child allowlists.

`prepare-fixture.py` extracts the existing event table and event functions
from the pinned migrations and adds only the protocol columns used by the SSE
fixture. It does not apply financial migrations. Authentication and wallet/
vault identifiers are synthetic; the allowed-origin header is fixture.invalid.
All HTTP traffic is loopback; redirects and urllib proxies are disabled.

The harness tests real direct-SQL emission and received event IDs, rejected
auth/origin, concurrent-client isolation, expiry and Last-Event-ID replay, including an event emitted while the server
is down and replayed after a SIGTERM/restart.
It compares exact expired-row counts with cleanup disabled/enabled. A real
PostgreSQL table lock tests admission timeout and cancellation. A temporary
fixture-only RLS delay stalls replay SELECTs before SIGTERM. The role/policy
are removed and every realtime child is stopped. An unmarked local database
must fail before writes, and denied.fixture.invalid PGHOST/PGDATABASE settings
must not redirect the explicit SQL transport. Twelve offline unsafe-input
checks reject missing/zero ports, non-loopback endpoints and URL overrides.

`evidence.json` uses the shared supporting collector envelope. Its identities
include the source commit, SHA-256 of every harness/crate/migration input, and
SHA-256 of the rebuilt runtime binary before and after tests. It hashes each raw output artifact and
refuses evidence if inputs changed during the run. These are content bindings,
not signed attestations or production artifact identities. V should rerun and
compare the source hashes and their independently generated measurements.
The harness proves neither cross-instance cleanup exclusivity nor an actual
mobile UI, browser, HTTPS proxy or production cutover.

## CSP binding gate

App next.config.ts captures the CSP origin when the config/build is evaluated;
the server token configuration reads REALTIME_EVENTS_URL at runtime. Set the
same binding for build and runtime. Changing the runtime origin needs a new
build/deploy for headers to match; retaining Render supports transition and
rollback but does not admit arbitrary later runtime origins. The focused
configuration probe verifies that capture/runtime divergence, the old origin,
and the `Content-Security-Policy-Report-Only` header. It runs no frontend build.
Report-Only reports violations; it does not block disallowed browser requests.
Neither that probe nor loopback SSE tests constitute browser enforcement or
rendering proof. Actual Vercel headers, HTTPS proxy delivery/reconnect, long-
lived browser sessions, mobile client bindings, and exclusive cleanup handoff
remain parent-owned deployment/cutover gates.
