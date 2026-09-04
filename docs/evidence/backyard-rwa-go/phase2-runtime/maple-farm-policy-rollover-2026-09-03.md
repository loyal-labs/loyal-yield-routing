# Maple farm policy rollover — 2026-09-03

Verdict: PASS

This is a forward-only, hookless replacement of the Maple syrupUSDC/USDC Kamino borrow and repay policies. No prior policy or account was closed, refunded, or mutated.

## Finalized input state

- Settings: `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`
- authority: `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`
- delegate: `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`
- pre-rollover policy seed: `136`
- guard slot: `444132932`
- debt farm: `87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y`
- obligation debt-farm user: `CcUorNoacydFVu7SHmhsA1qi9CcEu8K5YFvuS8unAzgr`
- both target policy accounts were absent before activation

Each signed packet was simulated with `sigVerify=true` and `replaceRecentBlockhash=false`. Each was sent once only after a passing simulation and the required finalized Settings seed/account-absence guard.

## Borrow replacement

- seed: `137`
- policy: `2m7DpWN1d7UC8iMZyipGzo5SRaBz9Buqhw1VJUTMpLSV`
- signature: `4METFCKGCGhmP5F5zvnTkuUFaeRyKPqY1huS6LqcPm5UkFqJSYJo8Ta2BS3a8H5VYCzrTXKRPypoyTqYz6iRjtsQ`
- finalized slot: `444132990`
- simulation: PASS, `48,462` compute units
- finalized account owner: `SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG`
- account bytes: `1,400`
- account-data SHA-256: `6f97d7928d7927d65b588644d2e0506bc86b2173f2f525edf087474e28631a94`
- account indices 12/13/14: obligation farm user / debt farm / Farms program

## Repay replacement

- seed: `138`
- policy: `AjjV5p7BPCxqaf92EsUjx2bavkTuhjHwiBJMvk8Gh8Uo`
- signature: `A62tzyk564T9TsgnfDk4cN3ejjgS3RDt4pixDvTikTrKxcEKZDy3AuqrJZu8eT4XcE6anS3GMg2GP9PN8u5Vvn4`
- finalized slot: `444133080`
- simulation: PASS, `44,398` compute units
- finalized account owner: `SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG`
- account bytes: `1,250`
- account-data SHA-256: `4bb7136fdeaa094aaf7e39cd0595434e1e9e09586c496303236f5d4ecc169f11`
- account indices 9/10/12: obligation farm user / debt farm / Farms program; market authority remains index 11

## Policy semantics

Both policies contain exactly one KLend ProgramInteraction constraint, no pre-hooks, no post-hooks, and no spending-limit policy. The instruction discriminator is pinned at offset 0 and the instruction amount is bounded by `u64 <= 1_000_000_000_000` at offset 8. The Phase 2 Go runtime remains the tighter execution-envelope owner at no more than `1,000,000` raw USDC per transaction and `10,000,000` raw USDC cumulative.

Final readback at slots `444133285`–`444133286` showed both signatures finalized successfully, both policy accounts present, and Settings seed `138`.
