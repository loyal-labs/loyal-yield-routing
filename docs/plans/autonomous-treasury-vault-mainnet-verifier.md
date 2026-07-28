# Autonomous Treasury Vault Mainnet Verifier

This document is the fixed success test for the first Loyal autonomous treasury
vault implementation. Re-read and run it after every implementation cycle.
Implementation details may change when simulation or live evidence disproves an
assumption, but the required observable end state must not be weakened merely to
obtain a passing result.

PASS means one mainnet Squads v5 Smart Account has been created with the Loyal
deployment identity as its recoverable setup signer, its public addresses and
transaction evidence are recorded below, and seven independently decoded policies
allow the delegated `POLICY_KEYPAIR` to perform only the requested Kamino,
Meteora, and return-to-Mother operations. The complete modular dust test must pass
in order: Smart Account, Kamino, Meteora, then treasury returns.

This cycle stops before the final Mother-only signer handoff. Do not remove the
deployment signer or move Mother treasury capital until a human independently
reviews this verifier's evidence and explicitly authorizes the handoff.

## Durable Deployment Record

Update this public, non-secret record immediately after each confirmed creation.
Never record a private key, secret environment value, or serialized transaction.

| Field | Recorded value |
| --- | --- |
| Cluster and genesis hash | `mainnet-beta` / `5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d` |
| Created Settings PDA | `EEA8aso4Vky11KYsL66jsBujUW8quseSr9GinxAPpmvw` (account index `502127`) |
| Created autonomous vault PDA and vault index | `F7zuL14omw4JJfS1cvsWXVb3wh48dvsonMJgoc9tYu3e` / `0` |
| Smart Account creation signature and finalized slot | `3rgmeb6okghkbbeZcG2d5YAjpCKkCzxvSGUS9eRzvLPCr76mAFT5PHXHvDCgPTcvCahRPG7zhiSMyPM6GsfMFB7L` / `435664493` |
| Deployment signer public key | `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` |
| Delegated policy signer public key | `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` |
| Kamino operations policy PDA | `5nAGZKeGW7HhFhDedgqgDBrjEgjTZjWP3otwPuh6ujZZ` (signature `2Fo2bHtSz4SiUe73Wq1J636TWGi1CHcosY3EDdinhfVMev7QSu8gNZG4q2o8QPYG5CwVHp7SHgtVzmtWRiDDhwRy`, finalized slot `435667857`) |
| Kamino init-obligation policy PDA | `7Vc3JAuayMdLarLqnu6NwKzyuB7h4u89zLrJAJPfTDFg` (signature `BPmuN28sok8mSZeUxwk7szFLmKcrW6VudiEcMpFHYnoTojRMoKShSfshPrCpDjEqcBF9rE7xjmQn8nfQGUg7v5o`, finalized slot `435667975`) |
| Meteora add-liquidity policy PDA | `GHitZJYZbum2wVnQ9Hesg1qof6KUCKVxTf4zmHiMaKTe` (signature `4GSNwXcRLNaUWe5EBbCCChCX2tMMHqwnbM75rzZS8rXvVpsm62cFJEkf7yxwYzGFeJayzyh9AjBED51iL27QAxqP`, finalized slot `435676576`) |
| Meteora remove-liquidity policy PDA | `ErKkpfJBn7jKtofDqwsWQGeQYqxUATN3hi91VES76Cx5` (signature `62gfWACioUdx78qaFLxtjK5kYqwy7NoRqxc7N6YTNgFE74LemPqsqimw2h5Pyon83byGFi9zagUcCCS8dE3YL3ZX`, finalized slot `435676686`) |
| Meteora claim-fee policy PDA | `2ots3M1LejBMoLCYd1kSLtAGs1auvjGdosHNhoorN5xt` (signature `4swxiiB69BLAteHv62HgEW5x21AufPRy3JW6jQzEAq7qWnpSJPrJrbMETSZAb2xkdJh5dghZU1EET3NHSzyW4f8K`, finalized slot `435676792`) |
| LOYAL return policy PDA | `7p9fbAoAJU1LjbjRXhWqh1gABWTG7aeUe96ZK55kLQUq` (signature `29ZfuPzahTH7h2vP2BQeoKfk96j9cGcjfswUV3rCZrNs6SWCPXFnhT5JYZ5rj34S16B33LQn5qJpCTroJm9YDR2e`, finalized slot `435681682`) |
| USDC return policy PDA | `Hn4rztZmUYVRvdfUWso3zXAoQEY5efk6KWbJJPxXdn8k` (signature `63jYGMbXmpoWrtHJVp2UgdXi4ab1ed8RhVFhjct8GXZ8CJKocsYNaa2EpZyAhAi4piFuYeoQCbNjX3XaeWSH39FB`, finalized slot `435681753`) |
| Meteora position PDA(s) and supported bin bounds | `3SBxJxpCG2EbfLrvD5DgjKUarLgn5i643WYNnUdTYgCB`, bins `-237..=-168`; test ranges A `-207..=-199`, B `-211..=-195` |
| Vault LOYAL and USDC token accounts | LOYAL `ALqM7B2RPGkdJyafxWHqvsgqaPz8DgriekfKhrVZPT1o` / USDC `CovHF1eSvqKE7A4MkGq5FYCsKtZBue3TQN464diBqW8` |
| Mother LOYAL and USDC destination token accounts | LOYAL `2EKLyZKCfhZxhSW35jYRaKSig3saAq97JKK9j28yrUg5` / USDC `FujHwQAccqyq4yCLYBZ9v2XxvDN3bLckB21iH3LSNhyW` |
| Evidence/state file path, if used | `docs/plans/autonomous-treasury-vault-mainnet-state.json` |

The current `DEPLOYMENT_PK` and `POLICY_KEYPAIR` entries resolve to the same
public key. Delegated tests must therefore prove use of the policy execution path,
but cannot claim cryptographic separation between the setup and delegated
identities during this pre-handoff cycle. After the reviewed handoff removes this
key from Settings and leaves it only on the policies, the roles become distinct
on chain.

## Completed Kamino Mainnet Evidence

All signatures below were simulated before send, finalized, and followed by fresh
RPC account decoding. The complete exact before/after maps are stored in the
public evidence file named above.

| Step | Finalized evidence |
| --- | --- |
| Vault USDC and metadata setup | `4sbDSBSTtbqvx2npJbU9uBQ3qB9er5H8bh4YVubLGmkkRtQKK9FB6ECm4F2TWNwur5nFH7umx6L7nodS4SZSven6` / slot `435669390`; deployment USDC `5,000,000 -> 4,000,000`, vault USDC `0 -> 1,000,000` raw |
| Initial obligation 0 / 1 | `rSec96LAJbGHxXW2FbhgxbAYTmW9hXGgnq9iRjNbiNRU8fNQa14RX47cyeF9Sw5HawDXL74aCorJgt9FbM6RiAg` / `435669703`; `47JUVsERuahegT34HhcBNm4Nwn57EjnwdGTMeuNGhqPhNCxb4KmFtbsqvg1sSwtDXmDc8EVZNVBoDGwaEzWrNZwF` / `435669781` |
| Persistent farm setup | `26XXGxJqN9XA2ccS49dYYvHev844NHG6Vjp1J5sd1aX4vBeoKbXcG37ujUtwBveQFECTJ1C5ZG1v6GgJT9cUsj4C` / slot `435670192`; each 920-byte farm account holds `7,294,080` lamports |
| Reserve 0 deposit / partial / full | `5DapUJsxSGjLHNTfBPPGJhCPm3wQV4GSzMzksKA54q7DYXghtEHzidCJ3rzWHyXqWhZiyKwchX7XwM2VhemUFkwU` / `435670774`; `jFkuacLed3hYyChtmn4ruGH8tQ97uxVuDUAw8mwogRWDmHJNGPYeTQZi127LWL5DTiZdx97G2tCzw4bp7p4cnxt` / `435670880`; `5xjbqaFdTvihkp3sMt4huk1bbRH9mG8K1GCoQ2DSGzHo1Ng3yYvNTK8o3DjSmzdiJhRjnvvG5mTcoL62wW9t5Uvj` / `435670972` |
| Reserve 0 deltas and recreation | Vault USDC `1,000,000 -> 900,000 -> 949,999 -> 999,999`; collateral `0 -> 95,407 -> 47,704 -> 0`; full exit closed the obligation and refunded exactly `24,165,120` lamports; delegated recreation `2KPc97xpz2GwgcXfmfAopsqkkLqGJR6dDL3fBmnEtxeuJMj5neupLiEPXJw4D1jR3LZiadBUzjnjDvd2mKuLPDCu` / `435671286` |
| Reserve 1 deposit / partial / full | `5f5jiWapx9VNL1jcnjdmd4KZC6q68jyzenk6WScu1QFipvwjjrBPfvWWE86rKzZVk5PVoVb21R1jr1YqhtVjw6v6` / `435671395`; `pcM3feiNyjiYsy3E6Vd7gBuX64Jcb7TgaT99xMfrKjYvRMwLhar6pRqxtQSwwKpUtwFfSpHzRUkvavvmWNj6gty` / `435671480`; `2pyZPcAv66zj3oQRsdMuJg3SU8WgfyYu8H8xmrVHUU3q9B8RQi3ejTixB3L222JHxaB5hJyE1tPvcp58q6FkM9zu` / `435671565` |
| Reserve 1 deltas and recreation | Vault USDC `999,999 -> 899,999 -> 949,998 -> 999,997`; collateral `0 -> 83,812 -> 41,906 -> 0`; full exit closed the obligation and refunded exactly `24,165,120` lamports; delegated recreation `2KnNa1f5YbQRUGYAjW9jWBHGFc18JNjNC7uNFdsxucWB3aQme9DGeRYTwxZSL8WLjeAmjo3sNZaprrf9iLF9k5FR` / `435671650` |
| Final Kamino snapshot | Both obligations exist with zero deposits and `24,165,120` lamports each; both farm users persist; vault USDC is `999,997` raw. Kamino canary cost: `3` raw USDC units. |

## Completed Meteora Setup Evidence

Both transactions below were simulated before send, finalized, and followed by
fresh RPC decoding. The autonomous vault paid the inner account rent; the setup
signer paid only the outer network fee and transferred deployment-funded dust.

| Step | Finalized evidence |
| --- | --- |
| Acquire deployment LOYAL dust | `mZN1UyqrmzFDfwEMazqXtaByymoNsoTHD32sJPrGTFyt8Dibaf94rnQ1ubVGUyJXEhUoY7i85uSNtGGFRf75tTr` / slot `435676056`; deployment USDC `4,000,000 -> 3,999,000`, LOYAL `0 -> 7,553` raw |
| Persistent Meteora account setup | `5a4P7N7jcfiuRUA3vUK6oDnPAZ1x3dptnApryLCGV2mS745Xw3SP1HzMJZ2UH5nQATVgLuymfkQsFmzyF7hsd3oQ` / slot `435676421`; position initialized with `57,406,080` lamports, LOYAL ATA with `2,039,280` lamports, vault LOYAL `0 -> 5,000` raw; position liquidity and pending fees remain zero |

## Meteora Delegated Execution Evidence

| Step | Finalized evidence |
| --- | --- |
| Add range A `-207..=-199` | `5A9LamN9rXnaNH4rQmSvFs4ndUjcFPk5g41kgfQfkgS5KK27fahSbbVhwozowdnXXgY51WMrvhyitG51EYrQUei3` / slot `435677665`; delegated add policy path; vault LOYAL `5,000 -> 4,003`, USDC `999,997 -> 999,862`; pool reserves rose by the exact `997` LOYAL and `135` USDC raw principal; position has 9 nonzero liquidity bins and retained `57,406,080` lamports |
| Remove range A `-207..=-199` | `5DGCxGYnQrPjihAmfF2bb14ogNwisfYDbopFq5mx7DtE1EiUNZD1b6Ece961xZfukBFLv7iEHtQkEHo9DMwvSNh6` / slot `435678149`; delegated remove policy path at 10,000 BPS; exact `997` LOYAL and `135` USDC raw returned to the vault; all liquidity shares returned to zero while the position, its `57,406,080` lamports, vault ATAs, and three approved BinArrays persisted |
| Add range B `-211..=-195` | `5tj5Mwj9CqBqfKLQq6FkVWV9NTc7Dn5xukQVmSHByaRi6CCwfsMSygEKUikqAS4KAmrqJvKeqkgUcCAL6ycVbCzV` / slot `435678273`; delegated add policy path; vault LOYAL `5,000 -> 4,004`, USDC `999,997 -> 999,861`; pool reserves rose by the exact `996` LOYAL and `136` USDC raw principal; position has 17 nonzero liquidity bins and retained its rent |
| Direct DLMM fee-generation swap | `3FdcS5sKLMZFrcx1wyaoKXcxVAJYcET1uhhbyfHHxwPjoh9MQQSmQmLPcAMzQyRpHNDaWr4WkyFoR4CK8WxTKXFP` / slot `435678802`; deployment-controlled direct pool swap, not Jupiter or vault authority; `100` USDC raw produced `654` LOYAL raw with quoted fee `6`; pool and deployment deltas matched exactly while vault balances and position rent were unchanged |
| Remove range B `-211..=-195` | `3LneqLDQcv4BNfdEGZJpq4B1FiJZ1oycfHUKFCbEa52rrLAWuzX3MUMoNJGNLksw5jJRu73Ecx7Y2Hhje6EyggPQ` / slot `435678997`; delegated remove policy path at 10,000 BPS; vault received `342` LOYAL and `230` USDC raw principal, all shares returned to zero, and the position settled `6` raw USDC as pending fees without closing or refunding rent |
| Claim all settled fees | `59NR6kDEURYxpRcPtPptCmx5EYun7tbLVyUDgk1SgJP7AgiNAuCp6Mxsw3DV2bEhjh3HRAuWfe4PpVo7vkYjYzXV` / slot `435679335`; delegated claim-fee policy path over the precreated position's full `-237..=-168` bounds; exactly `6` raw USDC moved from the pool reserve to the vault (`1,000,091 -> 1,000,097`), pending fees became zero, and the position, vault ATAs, approved BinArrays, and all rent persisted |

### Signed-Unsent Meteora Boundary Matrix

Fresh mainnet simulations were signed by `POLICY_KEYPAIR`, executed through the
live Squads policy path, and never sent. A fresh state snapshot after the matrix
exactly matched the snapshot before it.

| Case | Observed result |
| --- | --- |
| Canonical range B | PASS through Squads and outer DLMM; 861-byte packet |
| Noncontinuous BinArrays `-4/-2` | DLMM rejected with `AccountNotEnoughKeys` / 3005; 861 bytes |
| Duplicate BinArrays `-3/-3` | DLMM rejected with `AccountNotEnoughKeys` / 3005; 828 bytes |
| Range below the position lower bound | DLMM rejected with `InvalidBinId` / 6001; 861 bytes |
| Inverted range | DLMM rejected with an arithmetic-overflow SBF panic in `weight_to_amounts.rs`; 861 bytes |
| Atomic canonical add then remove at 10,001 BPS | first outer DLMM call succeeded; second rejected with `InvalidBps` / 6017; the whole 1,014-byte transaction rolled back |

The live DLMM's nested event CPI invokes the same program at depth 3. The gate
therefore counts only outer DLMM calls at depth 2 and proves their order relative
to top-level Squads calls. The inverted-range behavior is safe for funds because
the transaction fails atomically, but it is a Meteora program panic rather than a
structured validation error. ProgramInteraction policies constrain selected
program, account, and data fields; semantically inert trailing accounts or bytes
are not themselves a privilege escalation, while the DLMM program remains the
final validator for dynamic range relationships.

## Treasury Return Evidence

| Step | Finalized evidence |
| --- | --- |
| Return LOYAL dust to Mother | `3Sw4KhdsW4qJEzyCCvZFLM8dPcjc1raiTzuTdHVRgjD3VV3jv1yRDRU2S7jUTGLSLXrMvo3knsTzCQvZXaUUoxKx` / slot `435681841`; delegated LOYAL SpendingLimit path sent exactly `1` raw LOYAL; vault `4,346 -> 4,345`, Mother `6,751,866,978,094 -> 6,751,866,978,095`, vault lamports unchanged, and remaining lifetime allowance became `18,446,744,073,709,551,614` |
| Return realized USDC revenue to Mother | `2UhK9Nw8vRb2pab87FTcDZmY5Gty1p4GfRK35cB5vUNLR73ywQzHVqznURk7cyGggvKT7vTZHV9dgbpcXPHRHLyj` / slot `435681933`; delegated USDC SpendingLimit path sent all `6` raw USDC realized from the Meteora fee proof; vault `1,000,097 -> 1,000,091`, Mother `41,000 -> 41,006`, vault lamports unchanged, and remaining lifetime allowance became `18,446,744,073,709,551,609` |

## Safety And Signer Boundaries

- Load `DEPLOYMENT_PK`, `POLICY_KEYPAIR`, and RPC configuration only through
  `op run --env-file=.env.1password -- sh -c '<command>'`.
- Logs, command arguments, tracked files, state files, and chat must contain public
  keys and signatures only. They must never contain secret key material.
- Verify mainnet genesis hash before any send. Every live transaction must be
  simulated first, explicitly require `CONFIRM_MAINNET=1`, confirm at finalized
  commitment, and reload the affected accounts from RPC.
- The deployment identity may perform arbitrary recoverable setup. Every protected
  protocol action and both Mother-return transfers must be signed by the delegated
  `POLICY_KEYPAIR`, not the deployment key.
- Use only deployment-funded dust. Do not debit Mother. Per protocol canary, move
  at most 1 USDC and 10 LOYAL; spend no more than 0.5 SOL total without new human
  approval. LOYAL acquisition uses a locally built, deployment-controlled swap
  against the fixed Meteora pool and is not an autonomous-vault policy action.
- The final Settings remain recoverable for testing. Mother signer installation
  and deployment-signer removal are explicitly out of scope for this verifier.

## Required Policy Manifest

Independent decoding of on-chain policy accounts must show seven distinct policy
PDAs, each with `POLICY_KEYPAIR` as the sole policy signer, threshold 1, time lock
0, and no Settings-state expiration.

1. **Kamino operations:** exactly the current K-Lend program's V2 reserve deposit
   and V2 reserve withdrawal instructions. Pin the autonomous vault, its USDC token
   account, the two approved market-specific obligation PDAs, and these exact
   market/reserve pairs: `47tfy...TAv8` / `AYL4L...GVR2Z` and
   `7u3He...5PfF` / `D6q6w...gJ59`. No init or refresh instruction is present.
2. **Kamino init obligation:** exactly `init_obligation` for the two approved
   markets and their vault-derived vanilla obligation PDAs. Pin the vault as owner
   and payer, the vault UserMetadata PDA, default seed accounts, Rent, and System.
   No deposit or withdrawal instruction is present.
3. **Meteora add liquidity:** only `add_liquidity_by_strategy2` for pool
   `c29DVknA5DZUCH6U5ujo1EGfiKKXZrUk6yk56yJxLrm`, approved vault-owned
   position(s), vault LOYAL/USDC token accounts, exact pool reserves and mints,
   token programs, event authority, and valid pool BinArrays.
4. **Meteora remove liquidity:** only `remove_liquidity_by_range2` for that same
   pool and approved position(s), with outputs fixed to the vault token accounts.
   Valid removal BPS are accepted; no close-position instruction is authorized.
5. **Meteora claim fees:** only `claim_fee2` for the same pool and position(s),
   with LOYAL and USDC fees fixed to the vault token accounts. No swap, reward,
   operator, resize, or position-close instruction is authorized.
6. **Return LOYAL:** a Squads SpendingLimit policy for the LOYAL mint whose only
   destination owner is Mother vault
   `AQyyTwCKemeeMu8ZPZFxrXMbVwAYTSbBhi1w4PBrhvYE`.
7. **Return USDC:** the equivalent policy for native USDC and the same Mother.

The two return policies are intentionally effectively unlimited: `OneTime`,
`max_per_period = u64::MAX`, no restrictive per-use maximum, no accumulation,
and no expiration. ProgramInteraction venue policies may omit embedded quantity
limits; their security boundary is the exact program, pool/reserve, position,
mint, ownership, and destination graph.

## Dynamic Meteora Range Requirement

The Meteora pool address is constant, but the implementation must not freeze all
future deployments to one liquidity distribution. The live proof must use two
different valid bin selections or strategy ranges in the approved precreated
position set:

1. add dust liquidity using range/strategy A;
2. remove it without closing the position;
3. add again using a demonstrably different range/strategy B;
4. remove again and verify the same approved position accounts persist.

The implementation must record the supported immutable position bounds and prove
that both A and B fall within them. If one position cannot safely support both,
precreate a small explicit set of allowed positions before policy creation. The
policy must never accept another pool, arbitrary destination token accounts, or a
position owned by anyone other than the autonomous vault.

## Modular Live Verification

Run each module to PASS before beginning the next. A failure stops subsequent live
modules until fixed and reverified.

### Module 1 — Smart Account

- Create exactly one new mainnet Squads v5 Settings account and vault using
  `DEPLOYMENT_PK`.
- Immediately write Settings PDA, vault PDA/index, deployment and delegated public
  keys, creation signature, and finalized slot into the Durable Deployment Record.
- Independent RPC derivation and account decoding agree with the recorded values.
- Rerunning after a partial failure resumes from the recorded account and cannot
  silently create a second Smart Account.

### Module 2 — Kamino Policy Creation

- Create the Kamino operations and init-obligation policies separately.
- Simulate before send; after finalization, independently decode both policy
  accounts and compare every program, signer, account constraint, data
  discriminator, seed, threshold, and expiration field with the required manifest.
- Mutation tests reject wrong reserve, market, obligation, owner/payer, vault USDC
  account, instruction discriminator, and nondelegated signer.

### Module 3 — Delegated Kamino Execution

- Begin with the selected obligation absent or prove a full exit closes it.
- `POLICY_KEYPAIR` successfully initializes each approved obligation through only
  the init policy.
- For each approved reserve, `POLICY_KEYPAIR` deposits dust USDC, partially
  withdraws, then fully withdraws. Fresh RPC balances prove funds return only to
  the vault USDC account.
- Full exit proves whether the obligation closes and records any rent refund. If
  it closes, the delegated key recreates it through the init policy; if K-Lend
  keeps it open, fresh RPC reads prove the same obligation persists with zero
  deposited amount and no rent refund.
- Public refresh/farm setup may occur outside policy execution, but no protected
  deposit, withdrawal, or init is signed by `DEPLOYMENT_PK`.

### Module 4 — Meteora Policy Creation And Setup

- Precreate the vault LOYAL/USDC token accounts, approved position PDA(s), and all
  BinArrays required for the recorded supported bounds.
- Create add, remove, and claim policies separately and record their PDAs.
- Independent decode proves the exact fixed pool/account graph and dynamic range
  fields described above. Wrong pool, position, owner, reserve, mint, destination,
  program, and any close-position discriminator are rejected.

### Module 5 — Delegated Meteora Execution

- `POLICY_KEYPAIR` performs add A, remove A, add B, and remove B using deployment-
  funded dust and no vault swap authority.
- A separate deployment-controlled dust swap may generate nonzero fees. The
  delegated key claims them through only the claim-fee policy.
- RPC deltas prove all principal and claimed LOYAL/USDC land in the vault token
  accounts. The position PDA(s), token accounts, and BinArrays still exist after
  full removal and fee claim; no rent is refunded by an unauthorized close.

### Module 6 — Effectively Unlimited Return To Mother

- Precreate or verify Mother's correct LOYAL and USDC token accounts.
- `POLICY_KEYPAIR` sends a nonzero dust amount of each mint from the autonomous
  vault to Mother using the corresponding SpendingLimit policy.
- Independent RPC balance deltas match exact raw amounts. Wrong mint, wrong Mother
  owner, external destination, and nondelegated signer fail.
- Return all remaining realized Kamino yield and Meteora fee dust to Mother before
  final evidence collection, retaining only protocol rent and explicitly recorded
  test residue.

## Static And Independent Evidence

Before live sends, run the smallest relevant policy tests and packet measurements.
Required gates are `bun run test:squads`, focused tests for each new Meteora and
return-policy surface, and `bun run test:squads:e2e` when Kamino composition,
heap/compute, or historical replay behavior changes. Policy creation and execution
transactions must fit the current packet/account limits or fail before send.

Final verification must use fresh RPC reads rather than trusting builder output or
the execution state file. It must print a per-module `PASS` or `FAIL`, list every
recorded public address and signature, compare decoded on-chain policy state with
this manifest, report exact raw-token and lamport deltas, and end with one overall
verdict.

Overall verdict is **PASS only when every required module and independent evidence
condition above passes**. `PENDING`, skipped live behavior, simulation-only
evidence, an unrecorded Smart Account address, or a policy created but not executed
is FAIL.

## Final Gate Record — 2026-07-27

- `cargo test -p autonomous-vaults`: PASS, 5/5.
- `cargo test -p loyal-actions autonomous_vaults::returns::tests`: PASS, 2/2.
- focused `autonomous_vaults_meteora` harness: PASS, 2/2.
- `bun run test:squads`: PASS, including autonomous Kamino 1/1 and Meteora 2/2.
- `bun run test:squads:e2e`: PASS, historical Kamino replay 1/1.
- signed-unsent live Meteora boundary matrix: PASS, six cases, state unchanged.
- `autonomous-vaults verify-all`: PASS against fresh mainnet RPC reads, all seven
  policy accounts, 22 finalized live steps, exact return transaction metadata,
  and all recorded signatures and slots; final output ended
  `overall_verdict=PASS`.
