# Generalized cross-mint mainnet evidence — 2026-08-16 UTC

This is the sanitized, tracked evidence index for
`docs/plans/cross-mint-opt-in-policy-mainnet-verifier.md`. Public Solana
addresses and signatures are included; secret material, RPC URLs, API keys,
signed wire bytes, and production-user state are not.

## Verdict and boundary

`PASS_READY_FOR_LOYAL_APPS_WIRING`

The Rust policy/action surface, monitor, store, planner, fleet worker, resumable
mainnet verifier, and recovery behavior are implemented and verified. This does
not mean the feature is deployed or wired into `loyal-apps`; those are the next
integration steps.

## Policy actually tested

The capability is two immutable source-sharded Squads policies, which is the
minimum that fits Solana's 1,232-byte transaction limit:

| Policy | Source mints | Create bytes | Jupiter constraints |
|---|---|---:|---|
| Classic SPL | USDC, USDT, USDS | 1,148 | `route_v2` index 0 and `shared_accounts_route_v2` index 1 |
| Token-2022 | CASH, USDG, PYUSD | 1,148 | `route_v2` index 0 and `shared_accounts_route_v2` index 1 |

The unsharded six-source create measures 1,298 bytes and therefore cannot be a
valid Solana packet. Each fitted policy has three per-source Daily spending
limits, a `1_000_000` raw-unit cap per source mint, a nonzero maximum slippage
of 50 bps, zero platform fee, the exact vault authority, and output custody
restricted to the six canonical vault ATAs. The same policy bytes cover every
non-self destination and both current Jupiter V2 dialects; no policy update was
performed between swaps.

The policy is intentionally a custody and blast-radius envelope, not a quote
oracle. Jupiter validates its internal route graph; the worker must certify
fresh economics and reject self-swaps before execution. A compromised delegated
signer can still choose a poor structurally valid trade until the per-source
Daily cap is exhausted. Jupiter's upgrade authority is also part of the trust
boundary.

## Fresh immutable-policy 30-pair matrix

- Smart-account settings: `9t7rKkB2ixQAR6xafDY43LE28gFawSFTw6myAL9bjMuQ`
- Vault: `J69e6JnYeVvqjPfG6vJ8kYXpoNnXgWaqQMbYRd33Bsk2`
- Classic policy: `494SHD3wJTB4EjXutju5k9K3Dn5C1r63MRZicYp42Zp5`, seed 1, finalized slot 439603447, create signature `47bdKvTLYPNNWnCt3pebmXxm2ZiRhty41j2QAEsTUsyDCoqnHLewXx7gy4fqDWPXgG775S9wTZ79gZpaoVq15p4U`
- Token-2022 policy: `F63VmRzExRMwHdJDVt2GNd88jX8f2rcuPDuKQC9CusK4`, seed 2, finalized slot 439603481, create signature `xcKSy7E3WcvmDdVquPPjXu85LMmmW5ULSLURXuu9vtBXaaSaYLzxvj9hCFLj6akPip8RYSDboFGELfyLWCBYrTo`
- Result: 30/30 ordered non-self pairs sent, finalized, landed-wire checked, and reconciled against movement-attributed source debit and target credit.
- Route shape: 18 `route_v2`, 12 `shared_accounts_route_v2`, 20 two-hop routes, at most 25 unique accounts, 794 policy-wrapped bytes, and 150,043 simulated CU.
- Value: 300,000 aggregate raw source units debited across the 30 low-value swaps.
- Cleanup: all six vault ATAs closed, both policies removed and read back as absent, eight finalized cleanup stages, `pending = null`.
- Idempotency: a terminal rerun completed 30/30 with cleanup true in 1.01 seconds.
- Ignored resumable artifact: `.agents/cross-mint-mainnet-generalized-policy-set.json`, SHA-256 `624a41caff13fe85a0f07fe45b61261731038e409c497ccd2489c5f95be1e130`.

Every row below used one of the two policy creates above. Debit and credit are
finalized movement-attributed deltas, not quotes or total ATA balances.

| Pair | Dialect | Debit -> credit | Finalized slot | Swap signature |
|---|---|---:|---:|---|
| `CASH->PYUSD` | `route_v2` | 10000 -> 9997 | 439603552 | `R69CZcpKnsTeSmwEbbJAHE1jaUZRyTUQwrCCj5JdgjTCorNE4FzbtX8zkaxLsaC8pxXykq4qxd77fYviqFEuPSM` |
| `CASH->USDC` | `route_v2` | 10000 -> 9998 | 439603590 | `615vQqHNkjfjDH8ECTMtKCFWZcKLLTmV2bdbh5Xsmf2m9cVnqfdKTudWc8CgptzxzPktQAVX7s5Eio47A3ibbEd1` |
| `CASH->USDG` | `route_v2` | 10000 -> 9996 | 439603516 | `4fsrm5Q5rzwaSe6FJvokvZpDdFT5UcjkGR9zQkLGJpjPh82t1z2eoX2XgnhLDgxxq1A9X9eCPaCsGFHSV6Yfqbfd` |
| `CASH->USDS` | `route_v2` | 10000 -> 9998 | 439603664 | `4Y4E5UxWswxdzBmp4i98ZmCmxkKLohCw3pmr12yABjDAPWxjA1xtDEoPgjLFiuxj8qjoR4icqWPXKSzBfdK3jGzJ` |
| `CASH->USDT` | `route_v2` | 10000 -> 10007 | 439603626 | `UT3CKPrGTU4pXpHRtdSebb3QtxGBHTEkt9RULpNHAFobWCjcHtgRvm9JNdsmvNnzo2UepQNHMQvimFAWuF6xeWZ` |
| `PYUSD->CASH` | `route_v2` | 10000 -> 10000 | 439603885 | `8s44VEDBkJYAXxSsSWcC4nteJCs7LSwLvHoEhAegRxVoAY9p12BCHY6L4mo79uEYCVrNTtZaZBt5dnJcr3Su4TF` |
| `PYUSD->USDC` | `route_v2` | 10000 -> 9999 | 439603960 | `2qiAqvQ3cMVAZim8bXfasnhmN794bcNeojx1U4fPYa61i9YSPEVEr5dkvDfEEgFd46z1evVAPtWzkJA7FegAa18N` |
| `PYUSD->USDG` | `shared_accounts_route_v2` | 10000 -> 9997 | 439603923 | `3CEPNU99STFE2A9PtbnwkUBYj4bJpv2UA5JuUENHjN5kJVkpQWjFXYSgNv8nHDQ6261soBoegxYyv8JfzbmsSg11` |
| `PYUSD->USDS` | `shared_accounts_route_v2` | 10000 -> 9999 | 439604037 | `3Xi1br7QqGqptTA3SLEApwjK9y9Pth767rHGnmiEMqikHVucefg8pubTsUCWYcEs5XoWotSULikYxoHT3KvJjjyu` |
| `PYUSD->USDT` | `shared_accounts_route_v2` | 10000 -> 10008 | 439603997 | `3PJYADSQuC8shLvo8fVNPv3B5R6wszGL33gewqhASsRQZqS8TpEPS1smcCLQnKsykhMVBtXusuY6ioQ2N8mQkmug` |
| `USDC->CASH` | `route_v2` | 10000 -> 10001 | 439604075 | `3Xomgb15R9muKPs8MCn846dsRsftjXXgtYq6NVVjMc3H9dGSqr3Eensj7TA5rNsHqK8Y8L5EDRPXMXkqnsoe1Qtu` |
| `USDC->PYUSD` | `route_v2` | 10000 -> 9999 | 439604149 | `D2TYRH4PnETVez3AYzuSnSHZEMHLBHbbQXxiG4EtBogHVkK1bTbjHryt1fFmj4vw1PQJhBaiRmHjWRHf63A27uk` |
| `USDC->USDG` | `route_v2` | 10000 -> 9998 | 439604111 | `3qK5YEmDvmFYvLdaZxdk6aTNEiexCD327dLEw5iiW7q36RJK9nC6wh9AASpmEWFZ6nbHeeWhoqh2LagvWY9bgz7m` |
| `USDC->USDS` | `route_v2` | 10000 -> 10000 | 439604222 | `3SfJYWdrKzB89muHTC9DDXDsXHAaYHAGqdZMNff7qLTkzQ5vbBccDdMwspUHLNrEnXaHEnTEFiqwsu8iEhERba9g` |
| `USDC->USDT` | `route_v2` | 10000 -> 10009 | 439604185 | `2wY7gNSFuLwpa2V7u1qckxD1tzvrfVUueCwBnuQRZqeNzHyaf8zgXUM2UwQWW8TSu6SrinboevWMKLVBsdtcLTZJ` |
| `USDG->CASH` | `route_v2` | 10000 -> 10000 | 439603701 | `2w42cFW99WpfrLESmU2ckYLzAhbY6uyJQZkEq3npqd7f2qTqqshQXshZp1W4C7E4zVymwU2qXJVbw75fAvqWeXoL` |
| `USDG->PYUSD` | `shared_accounts_route_v2` | 10000 -> 9998 | 439603737 | `5UswQPiLM8ywaC9WaBNQgZfxg7a2pFFCscRrq9Bp3auMHYsfgWhWfSkj82ZX9ELeB8ohAT4tRnbJNngCMg3wZPRR` |
| `USDG->USDC` | `route_v2` | 10000 -> 9999 | 439603774 | `SvJgF7wkMWXFzPPft6fEvtrs8o848K6muTxNXFmSHnPodX9wZVawFj8vwSy1KYhTGv72weDQrEFzbnW25NFkqgu` |
| `USDG->USDS` | `shared_accounts_route_v2` | 10000 -> 9999 | 439603848 | `5Nz3zhsdcC4aXen3UFf3pnM9RPNPDAEA67ejPPCXr39FFpSWVsBGRwM3RnUHEbXDnXVDta6tDxEQWRfikEcwiAQg` |
| `USDG->USDT` | `shared_accounts_route_v2` | 10000 -> 10008 | 439603810 | `2JsrWYeFxcx8tYaCqEidPDusy4ZhXHEmWqsVW4hbYzFmXjz9CbeotEv4NfS6Zj9BStyZyp7cP5V837eQM3CRy6y1` |
| `USDS->CASH` | `route_v2` | 10000 -> 10000 | 439604443 | `2ZS4kYbdNLzh2dwQFJ8ighJj1bwijwYcPMU1ZJ3DwxMg8uLNjZrh36KWqFVGWxuxiiFuuRibKqMdYmoTY7bd8Z9T` |
| `USDS->PYUSD` | `shared_accounts_route_v2` | 10000 -> 9998 | 439604518 | `4YGKtea2SyoXnMZXZDTxyPZmNW7vSrKLPdueKwkHTHwhNDTD1XqbcXAMaBDvgtJ1A6hAizBK6CzjVvUYnc5yHJSy` |
| `USDS->USDC` | `route_v2` | 10000 -> 9999 | 439604555 | `24KuaDba39HQjLwktgMdeWuBHUNSHPppyHYi6vxChcWb7EenndrH3D2wXoArWygsU8sb9Fu6zqDrJmsWtSHcBsiv` |
| `USDS->USDG` | `shared_accounts_route_v2` | 10000 -> 9997 | 439604480 | `sXiLpuQ4ovkRgdRaRn9T8fosdc4haRgq1DvvqFefkU92C5soGMZ6ZSn8WWeoX72atmYkwvZG9JiJSeV4nisPMKt` |
| `USDS->USDT` | `shared_accounts_route_v2` | 10000 -> 10008 | 439604591 | `5FNvvthdKKi8ZcjDG7VKeqre5tpnfKtBSLnME4hQzr9xmgRTEMhTz3Bk8YxSCbeupqY32TPMEPggQoyCHkAWG4qm` |
| `USDT->CASH` | `route_v2` | 10000 -> 9991 | 439604260 | `5Hbatmq2Ykiub66MB3DWYXtLvvqcYS21UFXxjRLs43HnZPgHPHvG6eXRqTgq9c575suQbkBCkwiSvE9o7an3LBmw` |
| `USDT->PYUSD` | `shared_accounts_route_v2` | 10000 -> 9989 | 439604331 | `3zUT3xUTYQSVMwbVuwyBbKw7iLv9wvmTiod1UhgN4JTAXEkV8sh7w2kHXFLf6m74hH8vH8T8MnugNGdx15HhihoP` |
| `USDT->USDC` | `route_v2` | 10000 -> 9990 | 439604369 | `kZLNF6QdDu5zTpJAWTwZ8tfyR4M2n5YpFFcFTyxqYYXsPSm6i9EvR7E96NAz6bkjQRmxfrfXt5eG6V4KXdX4Xsh` |
| `USDT->USDG` | `shared_accounts_route_v2` | 10000 -> 9988 | 439604295 | `3SHSr73j2N3w82KRdsnWpMEDzTs4wsqQWGcyDLi4amVHsdx3yXQNNqCXARuC89Ecec9FTX9Cq9FPFixafuosC22Q` |
| `USDT->USDS` | `shared_accounts_route_v2` | 10000 -> 9990 | 439604407 | `2KYRTMxko3REqGX3truYnHkFm6VQaMkNQtLqGNZ4gGPGYb9PaYqgq9xzK1GS5vWdndYLZmbBcbWNwnLQ1H7HqgnM` |

## Historical withdraw -> swap -> deposit matrix

- Smart-account settings: `5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6`
- Vault: `ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh`
- Classic swap policy: `88BJpG4RGH1M1Dy3GPtcJNhZMPusStNsH6U5X8G3ujMu`, seed 5, finalized slot 439608235, create signature `3BMDAc4Ph3HQNJh4PTvt7ztL6JBnaFyFdF7e9TifFTmXGN6sUmc1vmvKeRW15bVfqQ4nSmEjzcYL8SqpzVVMUEfV`
- Token-2022 swap policy: `38ZCh9tq4qnFCQHLGrGDpicowN5upo7zenrffVWhTE1Q`, seed 6, finalized slot 439608269, create signature `31kJmgvKD91JV1HC5R2rLZH3AkqEfbU4NBMz2mJtKhE48u2kXjRw2ThZphPPffon3SppCbCgbGUie2ScNcQwxoJe`
- Result: 10/10 history-derived directions finalized and reconciled through four existing Earn policy shards plus the same two generalized swap policy shapes.
- Cleanup: 13 finalized stages, nine Kamino auxiliary accounts with explicit terminal dispositions, and `pending = null`.
- Idempotency: a terminal rerun completed 10/10 with cleanup true in 1.55 seconds.
- Ignored resumable artifact: `.agents/cross-mint-mainnet-generalized-historical-routes.json`, SHA-256 `0e407d805ca2e6d3eb613b789d030bc4dfaeb47a44e9506d96d03f8f08ca05a0`.

The amount column is finalized withdraw credit -> finalized swap target credit ->
finalized target deposit debit. The fourth transaction is the cleanup withdrawal
that proves the deposited target position is recoverable.

| Pair | Historical evidence | Amounts | Withdraw | Swap | Deposit | Cleanup withdraw |
|---|---|---:|---|---|---|---|
| `PYUSD->USDC` | `safe_substitution` | 9999 -> 9998 -> 9998 | `5ddtKedn5PUXCaffkHEGdEfKDFBJA8tvfvw4Sc1rx9AzsA4PY9F8y9jUEuPHNoVqhxVJ5sAK6EQQbAhtfLdUUrc8` | `33t8VywQY9gwTGANNrHgaGY8BJFM2GC4enA4SnJw77W8tU3EjakuKaQoLCTrskm7SCnDWYZjzyRR1ijxHRynWhY5` | `5PDqYgoY3WwKJwk2jnTjJUzBkVMFLznqVoiy1yu2f9Ak51pfjfoMUcpkGYxMvLBEXQzbX9jmWdD7nZc7Jue2hWqc` | `xv4oE6XPaAeV1FhCTXzHytrQrkLbdQrL9ezDt9BHE2ivoyRHWM5znwuSMDtDUey4oBUevAffk1dStLgkaAEYCZu` |
| `PYUSD->USDG` | `exact_historical_endpoints` | 9999 -> 9996 -> 9996 | `2JrsEqSMxEfS4ng235W4QwbzUdif4k5qgEChZNGHkZnx2cmM3SRsFyoeAnvHAjph4MSZcQ1HWt3MzkNc63RFNRaC` | `5pxKPpsU1AuZPw6AAQHXThxLTk6QV7Hz8yQ5kvvU5Lb5veoZ3tgxyKiKLcZowXAFdZ2zepQu9E7nFviyibN1ideK` | `2BWV5tBSiWEd7LmrDVouTkogKGi1uZsiknhn7PRTXGpWC257RBCLjEKNmkK7mLVdfhsKDpRy9Au6ztphUoRGZLsS` | `3x5QhqkvCwcnJuWqZWk6uzsd1tmHXcLZ8WqdvYqki1Np7yMfUuhkUbYJu2CUGT9TcrgpUYiuaPosnF6Qj1hoSZST` |
| `USDC->PYUSD` | `safe_substitution` | 9999 -> 9998 -> 9998 | `L5LabCsG1vCsAnK3BAPFhPocFViWCQpD7G4vt9dZy7zbjzHaKXZg8sCKJuYVXnCQ6wJY9Rx6JfCn73evLuiPMeH` | `5LtHon3JvoQo4aupGHrPBwkXCfpoRNxRX2eNKtvcRbHVWJyfco783rKwuyt3TZHW2fVbLXFH5o15YL6gufBAmM19` | `3XypZ6tdYRMQxpJkjrg9ZskU62rHANCeUWFoq3bT4RWewXmoNhZLChJRnY97MNhUPtNhu8pjzB3Rmoj2B13c3k28` | `LN3GX4He3ngr8Y1HKVRHmeRiDodxZjqDgeFQUpMXVG9LdCqjuxgf2qicnqJq9CVy2yun6yAJ2nWaK4och9onjfa` |
| `USDC->USDG` | `exact_historical_endpoints` | 9998 -> 9996 -> 9996 | `3gy1BAweH4KafbCQ2UWWhcpzeuZFQEtCeSX78U46XSx9JkZ1UFDnSLwhDjZYTn5geF4AQRTZ2Xhb4vcVANDfPUAW` | `5kF332AMJoNNkLj7ViRf5HgdjXEh7HDgyR3WxGeiLDtVKVcUmucUAca8udoriwkoXKBB4PBYzzQkCFJAVeY3kVCX` | `ZgRhcGLE3vw2PQ5Mgv7nT7Vqi9NDa5bAmjxpaoy8JB2m14gpz9n7pWDhBuWMT1F89KsT2tun67xwmP9RBqnPVL4` | `3AQwyJr5Ff1c6K9kMa8jTSDSHTk4P3zfiQYD4Bb5tWXkvcDZ3VXLVLeuJExtoP5aDJEocoU72CUP2q3RK9gQWy4Z` |
| `USDC->USDS` | `safe_substitution` | 9999 -> 9999 -> 9999 | `4iA6Qop3nXhxCQ35626ZQghAGTECEcFEigWuPSW2QxBPPSdpzWQpqxVzVWcvV4rmqd2sKkfNGhnBjd9AeS9xNyWe` | `3G9BZfs93XUBoC7LZzqYoMi34KJ55sbG2raD492ivQY8ARGwTMhJRM1E4WqUMCcitWxguDfZDBYVV2rEbUWTwtbz` | `5EJyHebJGYUxZfRSzacyYHAQkZN1odWXn8L5tZxdsZUv3AUjKiFBpECFzKqiskMbbc8M49rHA4fTzxRjo6swE4xy` | `x8rptwgHgn44PUqwdcb7BGt1HS3z86fGZU4soSt9vzV67vkxST2MzRyGgqW6jeJeazUTBT6ajpWgbbnhABintD6` |
| `USDG->USDC` | `exact_historical_endpoints` | 9999 -> 9998 -> 9998 | `56sNRz7UPeJ52GbMGsFbYx6TjHtE8SLYUZs8G4fRTHZik2WTAoX7Cn6jtQECmmyZBXA4UC9n7QH7Y5hdxRfyoQHu` | `4yTNXwDBeoZjNMQpuTXzCyrRWMw6qetK54Q6SzMNByAC2BTGST5yJYpm2nhGsJ6DszPidHt1mTenfk3Y5vnNrmed` | `2v1avmXfF69N2FbfAHYeofqR77DvAGZU46MK6zmwUWWew2mLpxdb5CirkbsavvXqHq9CPmAJpVKw1neHAoph6ExK` | `3cMzhHAqyQvj9FGVr7wbxYypjdfvQTdpjS1AfhugZNtCUdUs4hinB1KouSELUqmmCYMfMgvAgoupv2PZmKPgqjTL` |
| `USDG->USDS` | `exact_historical_endpoints` | 9999 -> 9998 -> 9998 | `4b8wjyHvJbzCHrY4uQUgqVSmYHMbrhvebVNeQyChwc27xa2S3bkdqsFg4wzSkoWpd7HCG6EaRVzk73vxj63MhweK` | `5kFMyfu9aqp23VCvbtAZVDmpg4ArnUQaLo5AxJazuhPNEwaD51tHrkoKEF9GgPPUG22NAsCWSTotaGHqxiR7nb1N` | `jnf265GKDSUKC1M2eaWn2BsHCV7uzoAdFPSxs95WzesvhmULtKqrCPiYN4e4adAVDvvpgvqFDzfUuLVVyQhvNz3` | `2eKqy2MNsveJCKShLicv3rjk2dQAm7sidYuARkmTU15qKkuagEnXygkaGE3mvf83SqPSQPkJasvakY87pFGaPy6b` |
| `USDS->PYUSD` | `direction_only_inference` | 9999 -> 9997 -> 9997 | `2g5XRk35V1wbv6UuXx54uUe28yMwcGb4GoZ9oaCGiiorm3W82xaQz21GwyoshMbPtoHW1oNEf53AfJnEsUK9nESA` | `BDrmoRxirYh7GhubqrX6mDcdXmwpWtLtPB4KpueCfLhP1Cmo2P8UDxiYG3d8DgKhJsHGefqXBJz2yZtzvwUcCya` | `2poSTsoYBb9VefqC14A2txYe66oZBWJGT4UXczJq9Cv2kJ9LX1jtJCPnuKqZ7qFsuU9oWxKdHKE2UEYG8kcnESCM` | `3tRFVMytbAu1WnSMAJVqWs1XTdrKu1bjfC9L73eYaKYc45zFq12g9yjXzqdjM1gATFMLnpkLM6jpVkp2zZ9R98yV` |
| `USDS->USDC` | `safe_substitution` | 9999 -> 9998 -> 9998 | `C2emZ5N82sVnXVmXNfDA3pkrFBwsvqmrTDzwTDrM6Janig8rPKePEp9wsTr9ZG6Cvs3ZcaUZLCNfVMGfB3amvys` | `5cjStKhQCkcYXgZhxutd4eUEbbq4xTf6cifRVdT54FprzXcLFHMpAcLP9z7Wi4dYY74HFcXQat8Ry62ofoWXg5q6` | `4b72AkQ6tHQpqvQMR1pJc4PtBs3ti4P46WshDXGcXdiTt26AWTqjMvvRJugyyiin7ADCBLdAiJaABa8Rdi2vshwf` | `3qjox3VJxicgqwHvKLsVwYYKXbMX3vnNpMUfhYk69FY7jEMReZeu5RLZZrM1VxzquBRgk5GtpS97CHQcZhSZbPJi` |
| `USDS->USDG` | `safe_substitution_current_capacity` | 9999 -> 9996 -> 9996 | `35oDFx4i1wZDcV76mXY3bfqER1tA6HsnDPAaXB66CWSNuDWt84WSZsFqr5Mwmwnd85yNS818Fozf7zYqcGphyuSi` | `oVo2M5b2jkx3kHixicg57LReh9dPYyxDRWhU6X1E9swfZpRyK7yTrsWUS3EQYTNLiEeEnfmQ7qcv4WB2GkM4U2p` | `3i4z7VWcPXckNxHvVtxhcPC25ru8W1RaDDLAheoTrhnsn9D4ST7kfrZk225qBphP9jCGv6ooV6Dvk7evyoCc7hT3` | `Wd6ZBfNwA5okUmbcRhhkirPwM2ykBu5wRFQjFKTaGnqVBsbiSeNv6nyouAivVGHzKFGxuonf86d7W38nw6YKoSC` |

## Current-state and repository gates

- Current Jupiter read-only matrix: 30/30 fresh strict builds and finalized
  simulations, 18 `route_v2`, 12 `shared_accounts_route_v2`, 20 two-hop routes,
  at most 25 accounts, 645 raw bytes, 794 wrapped bytes, and zero sends.
- Safe topology capture at `2026-08-16 16:55:50.469349 UTC`: 24 reserves,
  478 different-reserve topologies, all 30 mint directions, exactly two
  1,148-byte generalized swap policies, digest
  `901ceebbc54b8a29e0c0a743272baa119ba8c3ef116f7cdd01f844d3a6694d2b`,
  and zero sends.
- The current disposable PostgreSQL verifier passed migration 36's one-row-per-
  policy catalog, both-shard activation, ambiguity/removal behavior, immutable
  manifest binding, movement recovery, crash windows, and same-mint regression.
- Production Rust package tests, orchestrator tests, strict Clippy with
  `-D warnings`, `cargo fmt --check`, `bun run test:squads`, and the ignored
  `bun run test:squads:e2e` historical replay passed.
- `bun run lint`, `bun run build`, frozen dependency install, and
  `git diff --check` passed.

## Verifier incident retained in the implementation

The first historical-route attempt reached a dependent Token-2022 policy-create
simulation through an RPC bank that had not observed a prerequisite finalized
policy and returned Squads 6024 (`MissingAccount`). The failing transaction was
not broadcast. Dependent simulations now use the prerequisite's finalized slot
as `minContextSlot`; the resumed run completed all ten routes, and its terminal
rerun remained idempotent. This was an RPC consistency bug, not a packet-fit
failure.

## Non-claims

- No production smart account, policy, database, worker, or user funds were
  changed.
- No `loyal-apps` TypeScript SDK or UI toggle was changed in this repository.
- Mainnet success proves the tested Jupiter and Kamino shapes at the recorded
  slots; runtime admission must continue to use finalized policy readback and a
  fresh certified build.
