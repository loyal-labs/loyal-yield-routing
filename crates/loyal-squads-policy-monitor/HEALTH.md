# Optional native health file (version 1)

Exact source base: `0614fe5e697137a003aea6ad90811b92f70cff49`.
Set `POLICY_MONITOR_HEALTH_PATH=/run/squads-health/native.json`; unset means
no health writer. The parent supplies a private directory on tmpfs, owned by
root or the container UID, writable by that UID. No directory is created.
Secure root-owned `/run` ancestors are accepted. Group/world-writable ancestry
is rejected, except root-owned sticky *ancestors* such as `/tmp`; the final
directory must be private. Every component is opened relative to a held
no-follow directory descriptor. Existing targets must be regular files owned
by the container UID. The helper uses exclusive mode-0600 temporary files,
write + sync + directory-relative atomic rename. It never follows a target
symlink. Do not give the writer a directory shared with an untrusted same UID.
Tmpfs provisioning and mount identity are the parent's responsibility.

No CLI argument/default changes: original mainnet/confirmed/shadow defaults,
`--once`, fallback behavior and shadow ignoring `NEON_DATABASE_URL` are retained.
Stdout events remain untouched. Ping payload stays empty, interval stays 60s,
including Tokio's original immediate first tick and Delay missed-tick behavior.
No new RPC, database, signing or watchdog calls.

The JSON contains only the following observations:

- `schema`, `version`: `loyal_squads_policy_monitor_health`, `1`.
- `pid`, `process_start_ticks`: Linux `/proc/self/stat` field 22; null outside
  Linux or if unavailable. `runtime_epoch_unix_ns` distinguishes monitor runtime
  instances, including within one process. All counters reset in a new runtime.
- `native_process_tick`, `snapshot_monotonic_ms`: writer-thread activity, once
  per second. These are **not receiver or upstream health evidence**.
- `connection_generation`: monotonic connection-attempt counter in this runtime;
  `connected`, `reconnecting`, `subscription_acked` describe actual transitions.
  Ack requires JSON-RPC 2.0, request ID 1, integer subscription result, no error
  or method. Ack is cleared on every connection exit and attempt.
- `validated_pong_count`, `current_generation_pong_count`: increment only on
  received empty Pong with an outstanding successfully sent original Ping and
  a current-connection valid subscription ack. One Pong consumes one outstanding
  Ping; pre-ack Pong, wrong payload and unsolicited duplicate prove nothing.
  Empty payload cannot distinguish delayed responses to identical Pings; this is
  transport evidence, not an authenticated business-health proof.
- `last_pong_generation`, `last_pong_unix_ms`, `last_pong_monotonic_ms`: original
  receive arrival, retained through quiet periods and reconnects. Compare the
  generation to the current generation. Monotonic milliseconds are relative to
  this runtime's Instant epoch, not portable across processes or containers.
- `protocol_notification_count`, `last_protocol_notification_slot`: structurally
  parsed transaction notifications on the current acknowledged subscription.
  This does not imply successful decode, output, or finalized business activity.
- `last_finalized_output_slot`: updated only after a notification emits at least
  one output and all processing/sink writes succeed under finalized commitment.
  Confirmed observations, duplicates, quiet connections and failed sink writes
  cannot advance it. It is the last emitted slot, not a fabricated watermark.
- `writer_failed`: sticky in this runtime after any publication failure, even
  when subsequent publication succeeds.

Filesystem work runs on a separate standard thread. The receive/output loop
only copies tiny state under a mutex; the writer never holds it during IO.
Failures print fixed, secret-free stderr diagnostics and never fail business
processing. Writer failures attempt to remove the stale file; if that cannot
succeed, the old file remains and **must be rejected for staleness**. Startup
validation/thread failure leaves reporting unavailable. No values, addresses,
URLs, signatures, notification bodies or log payloads are serialized.

A sidecar must reject missing/malformed/version-mismatched, stale, wrong-process,
writer-failed, reconnecting/unacked files and Pongs from older generations. Match
PID + Linux start ticks to the monitored process and reset its own comparisons
on runtime epoch changes. Track *counter advances* with the reader's own clock;
never refresh heartbeat age from file mtime or native_process_tick. Neither a
fresh file nor a socket alone proves upstream or business liveness. This patch
provides observations only; freshness thresholds, independent receiver/stall
watchdog, sidecar verdict and migration acceptance belong to the parent.

Focused Linux CI (Rust 1.89, existing image pipeline):

```sh
cargo check --locked -p loyal-squads-policy-monitor
cargo test --locked -p loyal-squads-policy-monitor --test health_writer --test native_health
```

Tests use only local files and real loopback WebSockets. They cover quiet Pong
arrival preservation, invalid ack/Pong, reconnect invalidation, atomic files,
symlink/ancestry refusal, writer failure and runtime restart, plus finalized
output versus failed sink completion. No ignored E2E or provider access needed.
`libc` was already locked transitively; the sole dependency/lock change makes it
available directly for secure openat/fstatat/renameat/unlinkat and UID checks.
No usable token-IO atomic helper exists in this exact source base.
