# Autonomous Vaults

`autonomous-vaults` is the Rust operator and verification crate for Loyal's
autonomous treasury vault. It creates a Squads smart account, installs a narrow
set of delegated policies, exercises each policy with dust, and records public
mainnet evidence so another operator can verify the result.

The policy builders live in
[`loyal-actions::autonomous_vaults`](../loyal-actions/src/autonomous_vaults/).
This crate owns mainnet orchestration, account setup, simulation, resumable
execution, finalized-RPC checks, and the public state record.

The configured policy set contains thirteen Squads policies in three families:

| Family | Policies | Delegated operations |
| --- | ---: | --- |
| Kamino | 2 | Deposit or withdraw USDC in two approved reserves; initialize the matching vanilla obligations |
| Meteora | 9 | Add liquidity, remove liquidity, and claim fees across three BinArray shards in one LOYAL/USDC DLMM pool |
| Return to Mother | 2 | Send LOYAL or USDC from vault index `0` to the Mother treasury |

All thirteen policies authorize `POLICY_KEYPAIR` with threshold `1`, time lock `0`,
no start time, and no expiration. The policy signer may execute an installed
policy. It cannot create, update, or remove policies merely because it is named
inside them.

## Current mainnet account

The recorded deployment and a fresh read-only RPC inspection agree on this
control state:

| Field | Current value |
| --- | --- |
| Squads Settings PDA | `EEA8aso4Vky11KYsL66jsBujUW8quseSr9GinxAPpmvw` |
| Autonomous vault PDA | `F7zuL14omw4JJfS1cvsWXVb3wh48dvsonMJgoc9tYu3e` |
| Vault index | `0` |
| Settings threshold | `1` |
| Current Settings signer | Deployment key `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5`, full permissions |
| Delegated policy signer | `62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5` |
| Mother vault | `AQyyTwCKemeeMu8ZPZFxrXMbVwAYTSbBhi1w4PBrhvYE` |
| Mother status | Return-policy destination; atomic Settings handoff is verified but not broadcast |

Mother is not yet a Settings signer. It is Squads v4 vault index `0` for multisig
`Gv27nnaXR8UanJmjPZ4MLS81eqee2DfzJSv7C8PkQTEC`, owned by program
`SQDS4ep65T869zMMBKyuUq6aD6EgTu8psMjkvj52pCf`. The final handoff will add that
exact vault with full permissions and remove the deployment key in one child
Settings transaction. The exact transaction has been decoded, packet-checked,
and successfully simulated without broadcast. Until human approval, the
deployment key remains the sole Settings signer and Mother remains the only
allowed owner destination in the LOYAL and USDC return policies. Run `inspect`
and `verify-all` before relying on this section because on-chain signer state can
change after documentation is published.

## Mainnet transaction evidence

The links below are the complete finalized mainnet trail for the current canary
vault. They come from
[`autonomous-treasury-vault-mainnet-state.json`](../../docs/plans/autonomous-treasury-vault-mainnet-state.json),
and `verify-all` independently checks every recorded signature and finalized
slot against RPC before it prints `overall_verdict=PASS`.

Solscan may label generated or nested Squads instructions differently as its IDL
support changes. Verify the transaction result, slot, signer set, program IDs,
account addresses, inner instructions, and pre/post balances rather than relying
only on an explorer's instruction label.

### Smart Account and policy creation

| Step | Finalized transaction | Slot | What to verify |
| --- | --- | ---: | --- |
| Create Squads Smart Account | [3rgm…FB7L](https://solscan.io/tx/3rgmeb6okghkbbeZcG2d5YAjpCKkCzxvSGUS9eRzvLPCr76mAFT5PHXHvDCgPTcvCahRPG7zhiSMyPM6GsfMFB7L) | `435664493` | Creates Settings `EEA8…Ppmvw` and vault `F7zu…tYu3e` |
| Create Kamino operations policy | [2Fo2…hwRy](https://solscan.io/tx/2Fo2bHtSz4SiUe73Wq1J636TWGi1CHcosY3EDdinhfVMev7QSu8gNZG4q2o8QPYG5CwVHp7SHgtVzmtWRiDDhwRy) | `435667857` | Installs policy seed `1` for approved deposits and withdrawals |
| Create Kamino init-obligation policy | [BPmu…v5o](https://solscan.io/tx/BPmuN28sok8mSZeUxwk7szFLmKcrW6VudiEcMpFHYnoTojRMoKShSfshPrCpDjEqcBF9rE7xjmQn8nfQGUg7v5o) | `435667975` | Installs policy seed `2` for the two approved obligations |
| Create Meteora add-liquidity policy | [4GSN…AxqP](https://solscan.io/tx/4GSNwXcRLNaUWe5EBbCCChCX2tMMHqwnbM75rzZS8rXvVpsm62cFJEkf7yxwYzGFeJayzyh9AjBED51iL27QAxqP) | `435676576` | Installs policy seed `3` for the fixed LOYAL/USDC pool |
| Create Meteora remove-liquidity policy | [62gf…L3ZX](https://solscan.io/tx/62gfWACioUdx78qaFLxtjK5kYqwy7NoRqxc7N6YTNgFE74LemPqsqimw2h5Pyon83byGFi9zagUcCCS8dE3YL3ZX) | `435676686` | Installs policy seed `4`; it does not authorize position closure |
| Create Meteora claim-fees policy | [4swx…W4f8K](https://solscan.io/tx/4swxiiB69BLAteHv62HgEW5x21AufPRy3JW6jQzEAq7qWnpSJPrJrbMETSZAb2xkdJh5dghZU1EET3NHSzyW4f8K) | `435676792` | Installs policy seed `5` with vault-only fee destinations |
| Create LOYAL return policy | [29Zf…YDR2e](https://solscan.io/tx/29ZfuPzahTH7h2vP2BQeoKfk96j9cGcjfswUV3rCZrNs6SWCPXFnhT5JYZ5rj34S16B33LQn5qJpCTroJm9YDR2e) | `435681682` | Installs policy seed `6`, LOYAL mint, Mother-only destination |
| Create USDC return policy | [63jY…H39FB](https://solscan.io/tx/63jYGMbXmpoWrtHJVp2UgdXi4ab1ed8RhVFhjct8GXZ8CJKocsYNaa2EpZyAhAi4piFuYeoQCbNjX3XaeWSH39FB) | `435681753` | Installs policy seed `7`, USDC mint, Mother-only destination |
| Create Meteora shard 1 add policy | [5ywM…mJ1v](https://solscan.io/tx/5ywMqjwJPxcnowCvQKdWPq4hF53VAqJAij6modfVHTyseF1hN22X6mHEvj9csqGwpeRBcrhie2Q6kaSMHaxpmJ1v) | `435845448` | Installs seed `8` for lower BinArrays `-2/-1` |
| Create Meteora shard 1 remove policy | [2uDe…v8Bw](https://solscan.io/tx/2uDeDRNfkRw8tB28DjHfkYZprtjFQWzx8uMWTK8JMsbNAJhoMDem7XyASyGQbXV5hy6UWe45rE3RLRCWZH7Av8Bw) | `435845483` | Installs seed `9` for lower BinArrays `-2/-1` |
| Create Meteora shard 1 claim policy | [2GHE…TupP](https://solscan.io/tx/2GHE7uDf97uh7XDzJykBzgBVa7eSzXJQbGiTRDu5XEAymUnGnFvhPE8yh6qceWnoB1DyHDNwRub8J9XHTP5YTupP) | `435845517` | Installs seed `10` for lower BinArrays `-2/-1` |
| Create Meteora shard 2 add policy | [2HbQ…DfmG](https://solscan.io/tx/2HbQG96SAhDGgdeLTAwfjA7KwVt7vkjAgY2vuFohLM5ozcYqPNhpoU4bpfz8Gp1faVRu2U5ZnUt6evETefg8DfmG) | `435845738` | Installs seed `11` for lower BinArray `0` |
| Create Meteora shard 2 remove policy | [3BsV…RTPP](https://solscan.io/tx/3BsVoyUsK3dgWDdKxN2ED9VJacvdEiLNVtG9iTKC4No7MBmE1W87fGehzAUv5ghutYghweUF7GZHz6JwEo7WRTPP) | `435845774` | Installs seed `12` for lower BinArray `0` |
| Create Meteora shard 2 claim policy | [5r6F…i8Kf](https://solscan.io/tx/5r6FcmFNDFxSNecSGLNL8FAgrRKgMpPoYgTBKdiAiDs3PrwbLEYoxUfduZtT65fQdiSMSBSU4wXK94AatuF4i8Kf) | `435846167` | Installs seed `13`; Settings policy seed ends at `13` |

### Kamino setup and delegated execution

These transactions are in execution order. Reserve `0` is
`AYL4…GVR2Z`; reserve `1` is `D6q6…gJ59`.

| Step | Finalized transaction | Slot | What to verify |
| --- | --- | ---: | --- |
| Create vault USDC account and UserMetadata | [4sbD…ven6](https://solscan.io/tx/4sbDSBSTtbqvx2npJbU9uBQ3qB9er5H8bh4YVubLGmkkRtQKK9FB6ECm4F2TWNwur5nFH7umx6L7nodS4SZSven6) | `435669390` | Funds the vault with `1,000,000` raw USDC and creates persistent metadata |
| Initialize obligation `0` | [rSec…RiAg](https://solscan.io/tx/rSec96LAJbGHxXW2FbhgxbAYTmW9hXGgnq9iRjNbiNRU8fNQa14RX47cyeF9Sw5HawDXL74aCorJgt9FbM6RiAg) | `435669703` | Delegated execution through policy seed `2` |
| Initialize obligation `1` | [47JU…NZwF](https://solscan.io/tx/47JUVsERuahegT34HhcBNm4Nwn57EjnwdGTMeuNGhqPhNCxb4KmFtbsqvg1sSwtDXmDc8EVZNVBoDGwaEzWrNZwF) | `435669781` | Delegated execution through policy seed `2` |
| Create persistent farm-user accounts | [26XX…sj4C](https://solscan.io/tx/26XXGxJqN9XA2ccS49dYYvHev844NHG6Vjp1J5sd1aX4vBeoKbXcG37ujUtwBveQFECTJ1C5ZG1v6GgJT9cUsj4C) | `435670192` | Creates both farm-user accounts before value movement |
| Deposit to reserve `0` | [5Dap…FkwU](https://solscan.io/tx/5DapUJsxSGjLHNTfBPPGJhCPm3wQV4GSzMzksKA54q7DYXghtEHzidCJ3rzWHyXqWhZiyKwchX7XwM2VhemUFkwU) | `435670774` | Vault USDC `1,000,000 -> 900,000`; obligation collateral `0 -> 95,407` |
| Partially withdraw reserve `0` | [jFku…cnxt](https://solscan.io/tx/jFkuacLed3hYyChtmn4ruGH8tQ97uxVuDUAw8mwogRWDmHJNGPYeTQZi127LWL5DTiZdx97G2tCzw4bp7p4cnxt) | `435670880` | Vault USDC `900,000 -> 949,999`; collateral `95,407 -> 47,704` |
| Fully withdraw reserve `0` | [5xjb…t5Uvj](https://solscan.io/tx/5xjbqaFdTvihkp3sMt4huk1bbRH9mG8K1GCoQ2DSGzHo1Ng3yYvNTK8o3DjSmzdiJhRjnvvG5mTcoL62wW9t5Uvj) | `435670972` | Returns remaining USDC, closes obligation, refunds `24,165,120` lamports to vault |
| Reinitialize obligation `0` | [2KPc…LPDCu](https://solscan.io/tx/2KPc97xpz2GwgcXfmfAopsqkkLqGJR6dDL3fBmnEtxeuJMj5neupLiEPXJw4D1jR3LZiadBUzjnjDvd2mKuLPDCu) | `435671286` | Proves the delegated init policy can restore the closed account |
| Deposit to reserve `1` | [5f5j…jw6v6](https://solscan.io/tx/5f5jiWapx9VNL1jcnjdmd4KZC6q68jyzenk6WScu1QFipvwjjrBPfvWWE86rKzZVk5PVoVb21R1jr1YqhtVjw6v6) | `435671395` | Vault USDC `999,999 -> 899,999`; obligation collateral `0 -> 83,812` |
| Partially withdraw reserve `1` | [pcM3…6gty](https://solscan.io/tx/pcM3feiNyjiYsy3E6Vd7gBuX64Jcb7TgaT99xMfrKjYvRMwLhar6pRqxtQSwwKpUtwFfSpHzRUkvavvmWNj6gty) | `435671480` | Vault USDC `899,999 -> 949,998`; collateral `83,812 -> 41,906` |
| Fully withdraw reserve `1` | [2pyZ…M9zu](https://solscan.io/tx/2pyZPcAv66zj3oQRsdMuJg3SU8WgfyYu8H8xmrVHUU3q9B8RQi3ejTixB3L222JHxaB5hJyE1tPvcp58q6FkM9zu) | `435671565` | Returns remaining USDC, closes obligation, refunds `24,165,120` lamports to vault |
| Reinitialize obligation `1` | [2KnN…k5FR](https://solscan.io/tx/2KnNa1f5YbQRUGYAjW9jWBHGFc18JNjNC7uNFdsxucWB3aQme9DGeRYTwxZSL8WLjeAmjo3sNZaprrf9iLF9k5FR) | `435671650` | Restores the second persistent zero-deposit obligation |

### Meteora setup and delegated execution

| Step | Finalized transaction | Slot | What to verify |
| --- | --- | ---: | --- |
| Acquire deployment-funded LOYAL dust | [mZN1…5tTr](https://solscan.io/tx/mZN1UyqrmzFDfwEMazqXtaByymoNsoTHD32sJPrGTFyt8Dibaf94rnQ1ubVGUyJXEhUoY7i85uSNtGGFRf75tTr) | `435676056` | Direct fixed-pool setup swap: `1,000` raw USDC for `7,553` raw LOYAL |
| Create vault LOYAL account and PositionV2 | [5a4P…d3oQ](https://solscan.io/tx/5a4P7N7jcfiuRUA3vUK6oDnPAZ1x3dptnApryLCGV2mS745Xw3SP1HzMJZ2UH5nQATVgLuymfkQsFmzyF7hsd3oQ) | `435676421` | Creates the persistent position, transfers `5,000` raw LOYAL, leaves liquidity and fees at zero |
| Expand PositionV2 upper bound to bin `-77` | [5cRW…Q9Qe](https://solscan.io/tx/5cRWiptcTaVFJztCWbavVSM98JaqUZcdcLYvHGzz3k3xueUR9wkyxivNhWi7bpaUNMcGgp7XEJubt98gAYXYQ9Qe) | `435702536` | First bounded Settings-authorized resize; width `70 -> 161`, position rent `57,406,080 -> 128,342,400` lamports |
| Expand PositionV2 upper bound to bin `0` | [4Ahi…Pq28s](https://solscan.io/tx/4AhiCbEsNdSAHP4iRViEmzH3dSiEMFhe7ejKrV3TF6Gb3LgE1pXJB6xqKB4SrTcndNLcHASsE2fSqzcq1swPq28s) | `435702575` | Second bounded resize; final bounds `-237..=0`, width `238`, rent `188,365,440` lamports, liquidity and pending fees remain zero |
| Add liquidity with range A | [5A9L…Uei3](https://solscan.io/tx/5A9LamN9rXnaNH4rQmSvFs4ndUjcFPk5g41kgfQfkgS5KK27fahSbbVhwozowdnXXgY51WMrvhyitG51EYrQUei3) | `435677665` | Delegated add for `-207..=-199`; pool receives `997` LOYAL and `135` USDC raw |
| Remove 100% of range A | [5DGC…SNh6](https://solscan.io/tx/5DGCxGYnQrPjihAmfF2bb14ogNwisfYDbopFq5mx7DtE1EiUNZD1b6Ece961xZfukBFLv7iEHtQkEHo9DMwvSNh6) | `435678149` | Returns the exact principal while the position and its rent persist |
| Add liquidity with range B | [5tj5…bCzV](https://solscan.io/tx/5tj5Mwj9CqBqfKLQq6FkVWV9NTc7Dn5xukQVmSHByaRi6CCwfsMSygEKUikqAS4KAmrqJvKeqkgUcCAL6ycVbCzV) | `435678273` | Delegated add for different range `-211..=-195`; position has 17 active bins |
| Generate a visible fee | [3Fdc…KXFP](https://solscan.io/tx/3FdcS5sKLMZFrcx1wyaoKXcxVAJYcET1uhhbyfHHxwPjoh9MQQSmQmLPcAMzQyRpHNDaWr4WkyFoR4CK8WxTKXFP) | `435678802` | Deployment-controlled `100` raw USDC swap generates `6` raw USDC in quoted fees |
| Remove 100% of range B | [3Lne…ggPQ](https://solscan.io/tx/3LneqLDQcv4BNfdEGZJpq4B1FiJZ1oycfHUKFCbEa52rrLAWuzX3MUMoNJGNLksw5jJRu73Ecx7Y2Hhje6EyggPQ) | `435678997` | Liquidity returns to zero; position records `6` raw pending USDC fees and stays open |
| Claim all settled fees | [59NR…YzXV](https://solscan.io/tx/59NR6kDEURYxpRcPtPptCmx5EYun7tbLVyUDgk1SgJP7AgiNAuCp6Mxsw3DV2bEhjh3HRAuWfe4PpVo7vkYjYzXV) | `435679335` | Moves exactly `6` raw USDC to the vault; pending fees return to zero |

### Return-to-Mother execution

| Step | Finalized transaction | Slot | What to verify |
| --- | --- | ---: | --- |
| Return LOYAL dust | [3Sw4…UoxKx](https://solscan.io/tx/3Sw4KhdsW4qJEzyCCvZFLM8dPcjc1raiTzuTdHVRgjD3VV3jv1yRDRU2S7jUTGLSLXrMvo3knsTzCQvZXaUUoxKx) | `435681841` | SpendingLimit sends `1` raw LOYAL: vault `4,346 -> 4,345`, Mother `6,751,866,978,094 -> 6,751,866,978,095` |
| Return realized USDC fees | [2UhK…HLyj](https://solscan.io/tx/2UhK9Nw8vRb2pab87FTcDZmY5Gty1p4GfRK35cB5vUNLR73ywQzHVqznURk7cyGggvKT7vTZHV9dgbpcXPHRHLyj) | `435681933` | SpendingLimit sends all `6` raw fee units: vault `1,000,097 -> 1,000,091`, Mother `41,000 -> 41,006` |

## Security model

An allowed transaction passes three separate checks. Before policy creation, the
Rust planner loads finalized RPC state and checks the approved program and its
account graph, including each market, reserve, pool, mint, position, and BinArray.
During delegated execution, Squads checks the program ID, selected account
addresses, instruction discriminator, and constrained instruction fields. Kamino
or Meteora then checks relationships represented in protocol state. Examples
include reserve-owned supply vaults, obligation-to-market bindings, position
bounds, active-bin slippage, and removal basis points.

The policy does not duplicate every protocol check. Constraints that only repeat
program-owned state increase policy size without adding an independent trust
boundary. The sections below identify which layer owns each check.

The policies contain no embedded permission to create arbitrary token accounts,
close positions, close vault token accounts, swap through another venue, or send
funds to an arbitrary owner.

## Policy family 1: Kamino

Kamino uses two ProgramInteraction policies. Keeping obligation initialization
separate from value movement makes the normal deposit and withdrawal permission
smaller and easier to inspect.

### Approved markets and reserves

Only these market/reserve pairs are accepted by the planner:

| Index | Market | Reserve |
| ---: | --- | --- |
| `0` | `47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8` | `AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z` |
| `1` | `7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF` | `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59` |

Both reserves must use native USDC as their liquidity mint. The planner reads
each reserve at finalized commitment and derives one vanilla obligation for the
autonomous vault and market.

### Kamino operations policy

Policy seed `1` permits exactly two K-Lend instruction families:

- `deposit_reserve_liquidity_and_obligation_collateral_v2`
- `withdraw_obligation_collateral_and_redeem_reserve_collateral_v2`

Deposit and withdrawal are separate instruction constraints inside the same
policy. The delegated signer supplies the amount at execution time.

The Squads constraint covers these K-Lend account indexes:

| Index | Constraint | Reason |
| ---: | --- | --- |
| `0` | Exact autonomous vault PDA | The vault is the K-Lend owner and inner signer |
| `1` | Allowlisted vault-derived obligation PDAs | Prevents use of an unrelated obligation |
| `2` | Allowlisted Kamino markets | Limits execution to the two approved markets |
| `4` | Allowlisted Kamino reserves | Limits execution to the two approved reserves |
| `5` | Native USDC mint | Prevents depositing or withdrawing another asset |
| `9` | Exact vault USDC associated token account | Deposit source and withdrawal destination stay in vault custody |

The data constraint fixes bytes `0..8` to the deposit or withdrawal
discriminator. The amount is intentionally dynamic.

K-Lend validates the remaining reserve-owned graph. That check covers the
lending-market authority, liquidity supply, collateral mint and supply, both
token programs, the instructions sysvar, farm accounts, and Farms program. It
also binds each obligation and reserve to its market.

The collateral-supply account deserves special attention. Deposit account `8`
is `reserve_destination_deposit_collateral`, a reserve-owned K-Lend vault. It is
not controlled by the autonomous vault and must not have a Squads token-authority
constraint against the vault. K-Lend pins it to
`reserve.collateral.supply_vault`. On withdrawal, the same reserve-owned account
appears at index `6` as `reserve_source_collateral`.

Squads stores separate allowlists for these accounts; it does not encode the
market/reserve combinations as tuples. K-Lend rejects a cross-market combination
because the selected reserve and obligation must belong to the selected market.
The planner also verifies the two canonical pairs before it creates the policy.

### Kamino init-obligation policy

Policy seed `2` permits only `init_obligation`. It contains no deposit or
withdrawal discriminator.

| Index | Constraint |
| ---: | --- |
| `0` | Owner is the autonomous vault PDA |
| `1` | Payer is the autonomous vault PDA |
| `2` | Obligation is one of the two vault-and-market-derived vanilla obligation PDAs |
| `3` | Lending market is one of the two approved markets |
| `4` | Seed account 1 is the default public key |
| `5` | Seed account 2 is the default public key |
| `6` | Exact vault-derived Kamino UserMetadata PDA |
| `7` | Rent sysvar |
| `8` | System Program |

The data prefix fixes the K-Lend discriminator and the two zero bytes used by the
reviewed vanilla-obligation tag and ID. The policy cannot initialize a tagged or
arbitrarily seeded obligation.

### Kamino account lifetime

Before final signer handoff, setup creates the vault USDC token account and
UserMetadata. It also creates both obligations plus any required farm-user
accounts. A full K-Lend exit can close an obligation and refund its rent to the
vault. The farm-user account persists. `reinit-kamino-obligation` recreates the
exact approved obligation through policy seed `2`; no new deposit policy is
needed.

## Policy family 2: Meteora

Meteora starts with three ProgramInteraction policies. The expanded position
uses nine: add, remove, and claim policies for each of three BinArray shards.
The split is necessary because one full-envelope add policy is larger than
Solana's `1,232`-byte packet limit. A separate policy for each operation and
shard keeps every instruction family independently inspectable.

### Fixed pool and account graph

| Role | Address |
| --- | --- |
| DLMM program | `LBUZKhRxPF3XUpBCjp4YzTKgLccjZhTSDM9YuVaPwxo` |
| LOYAL/USDC pool | `c29DVknA5DZUCH6U5ujo1EGfiKKXZrUk6yk56yJxLrm` |
| LOYAL mint | `LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta` |
| LOYAL reserve | `3EXf7KbNWrBytuGK9Y7efHYM7rt837Wg9vArjd8QXgjx` |
| USDC reserve | `75MuUNzEGpRr2cKF4QU1EVn627iYqQ2LPqGosd7dNLJ5` |
| Event authority | `D1ZN9Wj1fRSUQfCjhvnu1hqDMT7hzjzBBpi12nVniYD6` |
| Memo program | `MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr` |

The planner also requires a live pool with LOYAL as token X, USDC as token Y,
the listed reserves, bin step `100`, and active status. It verifies both mints and
reserves as classic Tokenkeg accounts.

The current setup has one vault-owned PositionV2. Its immutable PDA creation
seeds are lower bin `-237` and initial width `70`; resizing does not change the
PDA. The live account was subsequently expanded from `-237..=-168` to
`-237..=0`, an approximate physical price envelope of `$0.0946–$1.00`.

The first three delegated policies were created before that resize. They still
authorize the original three reviewed BinArray candidates; expanding the
physical position did not silently broaden their permissions. Two live test
ranges prove that the same persistent position supports different allocations:

- range A: `-207..=-199`;
- range B: `-211..=-195`.

Generation 2 keeps those policies and adds six more. It does not remove or
replace a working policy, so there is no permission gap during installation.

| Shard | Policy seeds | Allowed lower BinArray indexes | Allowed upper BinArray indexes |
| ---: | --- | --- | --- |
| `0` | add `3`, remove `4`, claim `5` | `-4`, `-3` | `-3`, `-2` |
| `1` | add `8`, remove `9`, claim `10` | `-2`, `-1` | `-1`, `0` |
| `2` | add `11`, remove `12`, claim `13` | `0` | `1` |

These shards cover every possible two-BinArray execution window for physical
bins `-237..=0`: `(-4,-3)`, `(-3,-2)`, `(-2,-1)`, `(-1,0)`, and `(0,1)`.
BinArray `1` is required even though the physical position ends at bin `0`
because Meteora PositionV2 instructions always supply the selected lower array
and its successor. All six BinArrays already exist for this pool; the upgrade
fails closed if any is absent or has the wrong pool, owner, or index.

The range remains dynamic in instruction data. At execution time the Rust
planner derives the lower BinArray index from the requested range, selects the
corresponding finalized shard, and rejects a range spanning more than one
two-array window. Each lower and upper account slot has its own allowlist.
Meteora then rejects noncontinuous or duplicate array combinations and any range
outside the live position. A delegated call cannot substitute another pool,
position, destination, or BinArray PDA.

### Meteora add-liquidity policy

Policy seeds `3`, `8`, and `11` permit only
`add_liquidity_by_strategy2`; they differ only in their BinArray allowlists.

| Index | Constraint |
| ---: | --- |
| `0` | Approved vault-owned PositionV2 PDA |
| `1` | Exact LOYAL/USDC pool |
| `2` | Reviewed bitmap-extension sentinel |
| `3`, `4` | Vault token X/Y accounts derived for LOYAL and USDC |
| `5`, `6` | Exact pool LOYAL and USDC reserves |
| `7`, `8` | LOYAL mint and native USDC mint |
| `9` | Autonomous vault sender |
| `10`, `11` | Classic SPL Token program for both assets |
| `12` | Meteora event authority |
| `13` | Meteora DLMM program |
| `14`, `15` | Allowlisted first and second BinArray slots |

The instruction-data constraints are:

| Offset | Rule |
| ---: | --- |
| `0` | Exact `add_liquidity_by_strategy2` discriminator |
| `28` | `max_active_bin_slippage <= 3` |
| `40` | Exact two-sided `SpotBalanced` strategy tag and zeroed strategy parameters |
| `105` | Exact empty classic-token `RemainingAccountsInfo` encoding |

LOYAL amount, USDC amount, active bin, and range are dynamic. The Rust builder
requires the range to remain within the approved position and fit one shard; it
does not require the selected range to contain the current active bin. This is
what permits one-sided or ahead-of-market deployment anywhere in the physical
position. Meteora rechecks the observed active bin against the policy-capped
slippage, plus the position, range, and supplied BinArrays on-chain.

### Meteora remove-liquidity policy

Policy seeds `4`, `9`, and `12` permit only
`remove_liquidity_by_range2`; they differ only in their BinArray allowlists.

| Index | Constraint |
| ---: | --- |
| `0` | Approved PositionV2 PDA |
| `1` | Exact LOYAL/USDC pool |
| `2` | Reviewed bitmap-extension sentinel |
| `3`, `4` | Vault-owned output accounts for token X/Y |
| `5`, `6` | Exact pool reserves |
| `7`, `8` | Reviewed token X/Y mint pair |
| `9` | Autonomous vault sender |
| `10`, `11` | Classic SPL Token program |
| `12` | Memo program |
| `13` | Meteora event authority |
| `14` | Meteora DLMM program |
| `15`, `16` | Range-covering BinArray allowlists |

The policy fixes the discriminator at offset `0` and the empty classic-token
`RemainingAccountsInfo` at offset `18`. Range and removal BPS remain dynamic.
The Rust execution builder accepts ranges inside the live `-237..=0` position,
limits each call to one two-BinArray window, and accepts BPS from `1` through
`10,000`; Meteora enforces the position and removal rules on-chain.

This policy does not include a close-position discriminator. Removing `100%` of
liquidity leaves the position, vault token accounts, and BinArrays in place, so
the delegated signer can add liquidity again without recreating accounts.

### Meteora claim-fee policy

Policy seeds `5`, `10`, and `13` permit only `claim_fee2`; they differ only in
their BinArray allowlists.

| Index | Constraint |
| ---: | --- |
| `0` | Exact LOYAL/USDC pool |
| `1` | Approved PositionV2 PDA |
| `2` | Autonomous vault sender |
| `3`, `4` | Exact pool reserves |
| `5`, `6` | Vault fee destinations for token X/Y |
| `7`, `8` | Fixed LOYAL/USDC mint pair |
| `9`, `10` | Classic SPL Token program |
| `11` | Memo program |
| `12` | Meteora event authority |
| `13` | Meteora DLMM program |
| `14`, `15` | BinArray slots approved for the claim range |

The policy fixes the `claim_fee2` discriminator at offset `0` and the empty
classic-token `RemainingAccountsInfo` at offset `16`. The claim range is dynamic
but must fit the approved position and selected BinArrays. Fees can land only in
the vault's canonical LOYAL and USDC token accounts.

The claim policy does not authorize swaps, rewards, operator actions, position
resizing, token-account closure, or position closure.

Claiming the full expanded position is an orchestration concern, not a broader
policy. The operator issues four separately simulated and resumable calls:

- `-237..=-168` through shard `0`;
- `-167..=-98` through shard `0`;
- `-97..=-28` through shard `1`;
- `-27..=0` through shard `1`.

The final call must leave both pending-fee totals at zero. Each claim cycle uses
a finalized-slot identifier in its step names, so a later fee cycle cannot be
mistaken for an already completed one.

### Meteora setup and fee generation

`setup-meteora-accounts` creates the canonical vault token accounts and the
approved PositionV2 before final signer handoff. `expand-meteora-position` then
uses the current Settings signer to expand that same position in separately
simulated transactions capped at 91 added bins each. The autonomous vault is the
inner rent payer. BinArrays are derived from the fixed pool and are not disposable
vault accounts.

The deployment signer controls the dust-acquisition and fee-generation swaps.
Those setup/test operations are outside the delegated policy permissions. The CLI
constructs the direct Meteora swap locally with a fixed pool, mints, reserves,
canonical deployment token accounts, and fixed input amount. It does not sign a
serialized transaction returned by an external service.

## Policy family 3: return to Mother

The return path uses two Squads SpendingLimit policies rather than a broad token
program interaction policy:

| Seed | Mint | Only destination owner |
| ---: | --- | --- |
| `6` | LOYAL | `AQyyTwCKemeeMu8ZPZFxrXMbVwAYTSbBhi1w4PBrhvYE` |
| `7` | Native USDC | `AQyyTwCKemeeMu8ZPZFxrXMbVwAYTSbBhi1w4PBrhvYE` |

Each policy has the same limit shape:

| Field | Value |
| --- | --- |
| Source vault index | `0` |
| Mint | LOYAL or USDC, depending on the policy |
| Destination allowlist | Mother only |
| Period | `OneTime` |
| Maximum per period | `u64::MAX` |
| Maximum per use | `0`; exact-quantity enforcement is disabled |
| Start and expiration | None |
| Accumulate unused limit | No |

This is intentionally an effectively unlimited return path. It allows the
delegated signer to sweep realized Kamino proceeds, withdrawn principal, Meteora
principal, and claimed fees back to Mother without creating a new policy for each
amount.

Execution derives the classic associated token accounts for the autonomous vault
and Mother from the constrained mint and owners. The CLI verifies that both mints
are six-decimal Tokenkeg mints and that all four canonical token accounts already
exist. A call with another mint, destination owner, source vault index, or signer
does not satisfy the policy.

## Setup and handoff order

The intended order is:

1. Create the Squads Settings account and vault index `0` with the deployment
   identity as the recoverable Settings signer.
2. Create the two Kamino policies and test initialization, deposit, partial
   withdrawal, full withdrawal, and reinitialization through the policy signer.
3. Precreate the Meteora token accounts and position, expand it to `-237..=0`,
   create all nine Meteora policies, and test add A, remove A, add B, fee
   generation, remove B, claim, both upper-range policy shards, and adversarial
   boundaries.
4. Verify Mother's canonical LOYAL and USDC token accounts, create both return
   policies, and test a nonzero transfer of each mint through the policy signer.
5. Run independent RPC verification and obtain human approval.
6. Add Mother as the Settings signer and remove the deployment signer, leaving
   Mother as the only Settings signer. The delegated policy signer remains
   attached to the thirteen precreated policies.

The current verifier proves step 6 with a signed-unsent transaction but does not
broadcast it. Signer handoff still requires separate human approval after
reviewing the recorded addresses, policy bytes, transaction signatures, balance
deltas, and `handoff_ready=PASS transaction_sent=false` output.

The autonomous vault is the token owner and can be the inner payer when Squads
signs for its PDA. A Solana PDA cannot sign the outer transaction or act as its
top-level fee payer. The CLI therefore uses a keypair relayer for network fees;
that relayer does not gain policy authority over vault funds.

## Running the operator

Signer-loading commands must use the repository's mounted 1Password environment.
Never put a private key in a source file, `.env` file, command argument, log, or
documentation.

```sh
op run --env-file=.env.1password -- sh -c \
  'cargo run -p autonomous-vaults -- inspect'
```

Read-only inspection is the safe starting point. A mutating command also requires
`CONFIRM_MAINNET=1`, simulates the signed transaction before sending, waits for
finalized commitment, and reloads the affected account from RPC.

To claim all Meteora fees from the active position and put only the exact
finalized token deltas back into the pool, run:

```sh
op run --env-file=.env.1password -- sh -c \
  'CONFIRM_MAINNET=1 bun run vault:meteora -- compound-meteora-fees'
```

The command claims every policy-sized position chunk, records each signature
before broadcast, waits for finalization, and derives the reinvestment budget
from the vault's finalized token-account deltas. Pre-existing vault inventory is
excluded. LOYAL is placed immediately above the active bin and USDC immediately
below it; every add is simulated again and fails closed if the active bin moves.
Interrupted cycles resume from their persisted steps, while a later invocation
starts a new cycle so newly accrued fees can be compounded independently.

The main policy commands are:

```text
simulate-kamino-operations-policy
create-kamino-operations-policy
simulate-kamino-init-policy
create-kamino-init-policy

simulate-meteora-add-policy
create-meteora-add-policy
simulate-meteora-remove-policy
create-meteora-remove-policy
simulate-meteora-claim-policy
create-meteora-claim-policy

inspect-meteora-policy-upgrade
simulate-meteora-policy-upgrade
upgrade-meteora-policies

simulate-signer-handoff-readiness

simulate-return-loyal-policy
create-return-loyal-policy
simulate-return-usdc-policy
create-return-usdc-policy
```

Use `inspect-kamino`, `inspect-meteora`, `inspect-returns`, and `verify-all` to
decode live policy accounts and compare them with the expected manifest. The
complete execution sequence and acceptance rules live in
[`docs/plans/autonomous-treasury-vault-mainnet-verifier.md`](../../docs/plans/autonomous-treasury-vault-mainnet-verifier.md).
The public, non-secret deployment record lives in
[`docs/plans/autonomous-treasury-vault-mainnet-state.json`](../../docs/plans/autonomous-treasury-vault-mainnet-state.json).

## Verification commands

Run the policy proof surface after changing account indexes, instruction bytes,
or policy composition:

```sh
bun run test:squads
cargo test -p autonomous-vaults
cargo test -p loyal-actions
```

Run the historical Kamino replay when a change affects Kamino route composition,
heap or compute assumptions, or replay-sensitive behavior:

```sh
bun run test:squads:e2e
```

The tests independently decode the generated policies, mutate protected fields,
measure packet size, and execute representative flows through Squads and the
protocol mocks. Mainnet acceptance still requires fresh RPC decoding and exact
transaction balance evidence; local tests do not replace that check.

The current signed-unsent mainnet matrix also exercises the production builders
against the expanded range. An atomic add then full remove passed for
`-100..=-90` through policy seeds `8/9` in a `1,014`-byte transaction, and for
`0..=0` through seeds `11/12` in another `1,014`-byte transaction. Fee claims
passed through seeds `10` and `13` in `804`-byte transactions. The simulations
reached DLMM, sent no transaction, and left the finalized empty position
unchanged.
