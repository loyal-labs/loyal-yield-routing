# Strategy journal association

## Finalized chain evidence

`strategy-reset-history.json` was read from mainnet with finalized commitment at slot 447423787. SHA256: `3319db2f57baf743be2471d72a1e9d34168a81ebc0378071f7a7f28e1d968a64`.

The old vault claim finalized at 445730288. The actual strategy-two receipt bootstrap is transaction `4XcP5vPdaWdeWPzrQ3n5q694meYqXSvRJx1BVPNC45fy5U6bpN2p3Pc8g1NLPGT6gwt88VrqvfV4SqJWCUfFgQSj`, slot 446086069, September 11 2026 06:14:07 UTC. It created receipt `5bw4VYzpZXsk9SUNyWwJkb4fEx1DS8eNMFB6Qb4MUfhE` from zero lamports, initialized it against config `DCpR24Eb6xCWxDyaZvCBTkadkxCB2vkqJN1EfYNWtLxY`, and left both its USDC ATA and Squads USDC at zero. The earlier September 9 phase-one artifact is evidence of the repair/claim, not of the later receipt bootstrap.

A separate read-only production shadow at slot 447425207 observes strategy NAV zero, ticket consumed sequence zero, strategy/Squads USDC zero, and Voltr idle 23 raw USDC. Before migration, the old journal incorrectly describes staged custody 793417 and armed NAV 3793417, producing `custody_transient_mismatch`.

## Exact journal inventory

`strategy-one-journal-inventory.json.gz` contains 1717 terminal confirmed Backyard operations captured read-only. Its canonical SHA256 is `391298ab914ab798685f3c8d1345fb4a06acc02606b0df9cb75a2aebfcbe046a`: sort by operation_id using C collation; join operation_id, route_key, engine_version, action, status, confirmed_slot decimal, transaction_signature using `|`; join rows with LF and no trailing LF; hash UTF-8 bytes. Maximum confirmed slot is 444157954.

Migration 80 rejects any altered/missing/extra old tuple, unexpected later unscoped confirmed operation, active lease, or nonterminal operation. It appends row-bound association evidence to existing JSON. It preserves every original status, signature, wire, timestamp, effect and recovery reason. The accounting CTE excludes only the exact retired association, keeping unknown, malformed, copied or subsequently mutated metadata visible. Migration 78 remains a separate disposition of one failed restore; association never declares failed work reconciled.

## Verification level

Local PostgreSQL replay of the complete inventory and all rejection/preservation cases passes. The Go worker suite and Rust store compile pass. **Not yet applied to production; no production transaction was signed or sent.**
