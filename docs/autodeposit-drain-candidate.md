# Autodeposit shutdown candidate

The production trigger currently blocks in a shell executor and has no owned
child shutdown. This candidate installs SIGTERM/SIGINT admission inhibition,
owns one private executor process group, and waits/reaps that group on shutdown
or shell exit. Interrupted execution retains the original durable recovery
contract; it does not classify a transaction outcome or release claims/wires.

The candidate is based on deployed revision
`d4e11dd5c1f0d762371b8b49e5506b573b597731`. Render deployment
`dep-dapog2e7bikc73eq0fgg`, source digest `7c27ccaefa15762379e94c145628fc59cc27a9857a77ff9171d9a3fe888a1235`,
and the `--execute-eligible` command were refreshed read-only on 2026-10-04.
The source is still enabled. API identity does not prove effective executor
bindings or native process termination.

The branch workflow compiles the actual Linux production entrypoint, runs the
existing outcome tests and real process probe as UID10001, and publishes only
the dedicated candidate image. Its runtime inherits the original image and
replaces only `/usr/local/bin/balance-sweep-autodeposit-trigger`. Original Bun,
executor scripts, libraries, other binaries, environment and command remain.
The OCI revision identifies the overlay; the inherited image version continues
to identify the retained runtime. Publication does not deploy any service.

Retain the original digest for rollback. Before release, verify actual executor
group confinement, Linux image/ELF identity, production-entrypoint shutdown with
an isolated database and unsigned executor, queued database transaction
settlement, and exclusive recovery ownership. Cancellation alone is not proof
that admitted SQL rolled back. No setsid/daemon escape is covered by the group
probe. Unknown transaction outcomes and reservations must remain durable.

This patch supplies process shutdown, not a recovery-only startup mode or an
ATA monitor drain. The ATA monitor's reconciliation and replay handoff remains
a separate requirement.
