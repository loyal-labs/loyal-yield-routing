# ASK-2143 Verifier: ALT Read-Only RPC Recovery

Run this verifier from a clean checkout of the ASK-2143 implementation:

```sh
bash scripts/verify-ask-2143-alt-rpc-recovery.sh
```

## Required end state

The verifier returns `PASS` only when all of these are true:

1. A local fake Solana JSON-RPC endpoint returns HTTP 500 and then a valid
   response. The read-only RPC retry path returns the valid response without
   surfacing the first error to the provisioner fatal boundary.
2. The isolated endpoint also accepts a request and closes the connection
   without a response, reproducing Reqwest's `Request`/Hyper incomplete-message
   transport error. The provisioner classifies it as retryable and recovers on
   the next valid response.
3. HTTP 400 is not retried. The original typed Solana client error is returned
   after one request.
4. Retry delay grows but is capped; the test uses short injected durations and
   makes no external network request.
5. The shared-catalog finalized slot read, finalized shared-account bundle read,
   and provisioning-request finalized slot read use the narrow read-only retry
   boundary.
6. Provisioning-request RPC and family inputs are obtained before
   `lease_next_lookup_table_provisioning_request`. A dependency failure therefore
   cannot claim a request or consume its attempt budget.
7. `run_operation_batch` itself is not placed behind the RPC retry helper. An
   admitted `JoinSet` can never be replayed by this change.
8. Focused tests, formatting, and the provisioner binary check pass.

## Explicitly outside this verifier

- Neon retry supervision.
- Signed transaction retry behavior.
- Generic worker-supervisor infrastructure.
- The separately suspended semantic ALT alert monitor.
- Production deployment or alert-rule mutation.

## Verdict

`PASS` only if the verifier script exits zero and prints
`PASS: ASK-2143 read-only RPC recovery`. Any missing test, request-transport
regression, direct coordinator RPC read, planning input loaded after a lease,
whole-batch retry, compiler failure, or test failure is `FAIL`.
