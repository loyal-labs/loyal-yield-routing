# Rust crate boundaries

The Rust workers are retired; the workers run on the Go engine in
`go/workers`. What remains in `crates/` is on-chain code, its SDK and tools,
the Squads proof surface, and the realtime gateway:

```text
loyal-hub-abi ─────┬──> loyal-hub-swap-program, mock-yield-protocols-program
                   └──> loyal-actions <── loyal-solana-env
loyal-actions ─────┬──> loyal-hub-cli
                   └──> squads-test-harness (tests only)
loyal-solana-env ──> loyal-voltr-rwa-nav-adaptor-deployer
loyal-voltr-rwa-nav-adaptor (on-chain program, no workspace deps)

loyal-observability, loyal-yield-realtime-core ──> loyal-yield-realtime
```

Route action, policy-byte, account-planning and protocol topology contracts
belong in `loyal-actions`. `loyal-yield-realtime` depends only on
`loyal-observability` and `loyal-yield-realtime-core`; keep it that way so its
image builds without the Solana stack.

Depend on the smallest crate that owns the contract. Do not reintroduce a
shared worker or store crate; durable SQL and worker logic belong to the Go
families that own them.
