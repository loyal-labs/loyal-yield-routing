# ATA monitor owned shutdown candidate

The deployed28645 monitor aborts observations/reconciliation during cleanup,
does not own its APY task, and drops queued rechecks on channel closure. Aborting
its provider wrapper can also detach the underlying provider task. This
candidate owns that task tree, closes input, joins admitted queue work and all
reconciliation/APY/recheck tasks, then closes the database pools.

Ordinary session rebuilds use the same queue-draining path, bounded at45 seconds.
The process uses a separate OS-thread watchdog, starting its50-second deadline
when shutdown is observed/requested. Completion and expiry share one mutex;
late completion cannot claim success. A stalled runtime or failed join exits
nonzero and retains original durable recovery custody. Unexpected provider
task failures and observation-task errors now propagate rather than becoming
successful cleanup/reconnect results. This stricter failure behavior requires
the existing replay/restart regression and real database shutdown rehearsal.

No SQL, lease duration/owner, journal/accounting, mode, secret binding, cursor,
strategy, reconciliation retry or executor configuration changes are made.
Pending rechecks retain their existing delays, retries and maximum attempts.
The first-session finalized-tip-minus825 command remains unchanged and needs
its separate actual replay coverage proof at transfer/restart.

`scripts/ata-monitor-shutdown-probe` imports the production shutdown module and
exercises real process signals, owned nested-task cancellation and a watchdog
while the Tokio runtime is blocked. Local Darwin execution passed those three
components. The final hardened run also held stderr locked while Tokio was
blocked and exited nonzero at50.027s. Expiry uses the POSIX
[`_exit` process termination path](https://pubs.opengroup.org/onlinepubs/9799919799/functions/_exit.html)
without acquiring stderr or invoking userspace exit hooks. This is not Linux evidence,
production-entrypoint shutdown, actual DB transaction settlement, whole-family
recovery custody, source exclusion or migration acceptance.

The branch workflow builds Linux, runs existing replay tests and the real
component probe as UID10001, and publishes a dedicated one-binary image overlay
on original70996. It preserves the deployed runtime libraries and other binaries.
The OCI revision identifies the candidate; inherited image version28645 still
identifies the retained runtime. Publication does not deploy any service.

Before deployment: finish independent review, verify exact current source and
Linux artifact/ABI, exercise actual entrypoint with isolated DB/unsigned state
and in-flight work, prove source transactions/claims and producer-child closure,
retain unknown outcomes and original reservations, and admit one recovery owner.
The existing frozen ten-setting target preparation remains inactive and bound to
the original image; it is not authority for this candidate generation.
