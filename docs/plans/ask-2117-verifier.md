# ASK-2117 reserve monitor verifier

Run this verifier from `/Users/zotho/Dev/loyal/service-fixes/loyal-yield-routing` against the working tree. Treat the implementation as untrusted and try to prove each required condition false.

## Required conditions

1. **Bounded live ingress**
   - `rg -n 'Unbounded(Sender|Receiver)<AccountUpdateEvent>|unbounded_channel\(\)' crates/kamino-reserve-monitor/src` finds no account-update transport backed by an unbounded channel.
   - A focused test demonstrates that a full live-event channel applies backpressure instead of accepting unlimited account payloads or silently dropping them.

2. **Durable history is not coalesced**
   - The source-to-Timescale path remains FIFO and does not replace one `AccountUpdate` with another merely because both belong to the same reserve.
   - Distinct account hashes at the same reserve and slot remain distinct Timescale events under the existing deduplication contract.
   - LaserStream reconnects calculate replay start from the latest durably handled slot with overlap; merely receiving or enqueueing an update must not advance that cursor.
   - The gRPC client performs one physical subscription attempt at a time; stream errors and clean closures return to the outer retry loop instead of reconnecting from an SDK receive-side cursor.
   - A focused test closes the stream cleanly and proves the next request is rebuilt from the latest durable slot with overlap.

3. **Verification work is latest-wins**
   - Every successfully persisted valid live update marks its reserve dirty for confirmation.
   - At most one confirmed RPC batch is in flight.
   - Multiple updates for one reserve while a batch is in flight produce one pending retry for the newest generation, rather than queued refresh jobs.
   - A confirmation result for an older generation is rejected and cannot publish over the newer candidate.
   - Focused scheduler tests cover coalescing, updates during an in-flight request, stale-result rejection, and retry after a failed request.

4. **The whole catalogue is a safety sweep, not a one-second hot loop**
   - Live dirty reserves trigger prompt batched verification.
   - A periodic full-catalogue sweep remains for quiet reserves and missed notifications.
   - Defaults and `render.yaml` do not force a one-second full-catalogue refresh.

5. **Operational evidence exists**
   - Status telemetry exposes bounded event-channel depth/capacity, dirty verification count, and whether a verification batch is in flight.
   - Backpressure is visible through a counter or warning rather than being silent.

6. **Repository checks pass**
   - `cargo fmt --all -- --check`
   - `cargo test -p kamino-reserve-monitor --lib`
   - `cargo check -p kamino-reserve-monitor`
   - The attributable diff contains no Cyrillic characters.

## Nice to have

- No changes outside the Kamino monitor, its Render command, and this verifier document.
- Existing user changes remain untouched.

## Verdict

Report `PASS` only when every required condition is supported by command output, focused tests, or a direct source trace. Otherwise report `FAIL` with each false or unproven condition and continue the plan-do-verify loop.
