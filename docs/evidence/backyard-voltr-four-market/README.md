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

The current source freeze is
`policy-catalog-authorization-v7.json`: file SHA-256
`4d372de507c54f00fd3d70c7d055af64cd7afc73966de1e97168ee34f82290f6`,
authorization SHA-256
`23fecae0d2d0a239645b33bee30117eedb5f44a5a096ecab7678f7e302525c55`,
and effective route authorization SHA-256
`00232644ed6643b8f7a2d8af37c0c34e01589d38453310a5ccf8aba05053c8b6`.
Authorization v6 and its lifecycle outputs are historical only.

Any earlier wait-0 or alternate-timeout simulation is retained only as
historical diagnostic evidence. It is non-authorizing and cannot satisfy the
current 600-second lifecycle or partner-readiness verdict.
