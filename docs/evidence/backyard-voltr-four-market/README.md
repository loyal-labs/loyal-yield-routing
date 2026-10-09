# Backyard Voltr four-market evidence

This directory is the maintained evidence boundary for the Main, OnRe, Prime,
and Maple Voltr router. The first artifact is generated without signing or
broadcasting:

```sh
op run --env-file=.env.1password -- sh -c '
  test -n "${SOLANA_RPC_URL:-}" || exit 20
  bun tools/backyard-voltr/src/cli.ts verify compatibility \
    --commitment confirmed \
    --approval docs/evidence/backyard-voltr-four-market/compatibility-verifier-approval-v1.json \
    --confirm-approval-sha256 6f1a09150094205341638d897b8d87caabf53670f6694cb835a80b9b18a1c7b1 \
    --out docs/evidence/backyard-voltr-four-market/compatibility-v1.json
'
```

The supplied approval digest is an external operator input, not a digest the
verifier is allowed to choose. It binds the complete maintained Backyard tool
surface plus the behavior-critical Rust, manifest, and lock files and the
independently verified baseline policy artifact. Any source or approval drift
refuses before RPC access.

`BACKYARD_VOLTR_FOUR_MARKET_COMPATIBILITY_PASS` proves only live graph,
instruction, packet, ALT, deployment, policy-topology, and atomic-bootstrap
compatibility. The only final partner-readiness token is
`BACKYARD_VOLTR_FOUR_MARKET_CONFIRMED_PASS` from the fixed verifier after the
complete confirmed lifecycle exists. Per-strategy `bootstrapReady` means only
that its exact initialized accounts are present; `lifecycleReady` remains false
until manager deposit/withdraw and user withdrawal evidence pass.

## Accepted withdrawal contract

The maintained partner contract is request/claim-only with a 600-second
withdrawal waiting period. The direct Voltr instant-withdraw packet is a
no-broadcast compatibility probe: with `withdrawalWaitingPeriod = 600` and
`disabledOperations = 0`, it must simulate with exact `Custom 6015 /
InstantWithdrawNotAllowed`, emit no instant-withdraw event, and never be
executed. There is no `execute-instant-withdraw` command.

The current lifecycle proof consists of 13 confirmed transactions plus seven
exact proof artifacts: instant rejection, premature claim, withdrawal scan,
restoration, Earn adapter, negative mutations, and final reconciliation. The
confirmed transactions cover the user deposit, eight route round trips, Main
fallback allocation, request, named Main restoration, and claim. The two
rejection artifacts are simulation-only evidence, not additional confirmed
transactions and never permission to broadcast those packets.

Every accepted confirmed output contains exact ordered protected account
images, a fixed-signer pre-send attestation retained in the persist-before-send
file, and a linked confirmed-settlement attestation. The verifier recomputes
all row and aggregate hashes and verifies Ed25519 signatures against the exact
user or guardian. This is explicitly signer-attested evidence of
confirmed-provider observations; ordinary RPC cannot replay all historical
account bytes independently.

The current authorization is `policy-catalog-authorization-v28.json`: file
SHA-256 `11f749600d73c1d9f5a82fb5e474891e5bd35263afb017e35f404c60092417a5`,
authorization SHA-256
`981ca752b00d896e04f9f9c56b0d349a56d369c1f88e138ebfb005bb7ca0ecc7`,
and effective route authorization SHA-256
`66d9d7d6379201768fd79629016a1fb4e261c860027c14f0122edc5aa881999d`.
It authorizes the same `runtime-policy-catalog-v2.json` entries, seeds,
policy PDAs, and create/execution data hashes as v24 to v27. It binds the
policy bytes only: the artifact hashes and each entry's on-chain data hashes,
checked against a fresh compile of the catalog. v23 to v27 also pinned the
SHA-256 of every source file around the catalog, so any unrelated Go or
TypeScript edit forced a new version while the policies stayed identical; v28
drops that source binding. v23 to v27 are historical only.

Any earlier wait-0 or alternate-timeout simulation is retained only as
historical diagnostic evidence. It is non-authorizing and cannot satisfy the
current 600-second lifecycle or partner-readiness verdict.
