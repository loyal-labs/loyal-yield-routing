# Earn Max referral fee evidence, November 19, 2026

The audited Voltr vault is `HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA`. Three successful mainnet receipts establish how issuance, performance activation and a later NAV observation affected it. Transaction indices below are zero-based positions in finalized blocks.

| Event | Slot / transaction index | Observed result |
| --- | --- | --- |
| User deposit before performance activation | 471283980 / 52 | 3,534.21 USDC deposited, 9,396,942 raw LP minted to the depositor. The aggregate LP fee accumulator stayed at 73,763. [Receipt](https://solscan.io/tx/Q3m7XFmCjMwWdY8TTJBNKBzPDqVCYawh6HJ44SBQCSqQetAoVvDdSsMbryu69YcFFYwS11J67tJ8jg4dG87wv89) |
| Admin performance fee activation | 471406249 / 715 | Voltr changed the admin performance fee from 0 to 1,500 bps. [Receipt](https://solscan.io/tx/Y8cNCX2jG9BWwNyPj83eyqzyuDhYuBzyfWxXPz43dbHQYUTKaW3tVWwJtVAPutcnXszEE25Lrs57vwiYUCdWfkG) |
| Zero-capital strategy deposit publishing NAV | 471419563 / 202 | Value rose from 78,841,076,738 to 78,842,013,598 USDC micro-units. Fee-inclusive supply stayed at 210,456,986 raw LP and the high-water mark stayed at 105,664,007,182,493,780 in its fixed-point representation. The report produced no additional fee shares. [Receipt](https://solscan.io/tx/pFTyRHueuVM41inJoKttMGNYj6aGB2Y7NRu5qGgATyXrbCG8KTHK2Kj1jQW3Pg6PbGicabxBZJmnCPWnJxBgZHE) |

## Where the issuance fee goes

For the observed deposit, the prior asset value was 75,820,513,625 USDC micro-units and the fee-inclusive supply was 202,442,907 raw LP. With a 40 bps issuance fee, Voltr's SDK computes the user mint as follows for an established vault. All divisions below round down.

```text
D = 3,534,210,000
V = 75,820,513,625
S = 202,442,907

minted LP = floor(D × S × 9,960 / ((V + D) × 10,000 − D × 9,960))
          = 9,396,942
minted LP with zero issuance fee = floor(D × S / V) = 9,436,440
```

The deposited assets entered the vault in full. Its effective supply rose by exactly the depositor's mint. Dead weight stayed at 1,000 and the fee accumulator stayed at 73,763. The issuance charge reduced the minted LP by 39,498 raw units relative to a no-fee deposit, benefiting the existing shares. The high-water mark also rose on this deposit without an accumulator increase. A higher high-water mark alone therefore cannot establish an admin fee assessment. [Pinned Voltr SDK issuance calculation](https://unpkg.com/@voltr/vault-sdk@2.1.1/src/extensions/math.ts) · [Voltr fee and high-water-mark documentation](https://github.com/voltrxyz/docs/blob/5a8db0bfe4b27bcd893ea092a9f6884a1d355df4/vault-owners/fees-and-accounting.mdx)

## Funding boundary

At finalized slot 471422408 the program-owned vault account held 73,763 raw admin fee LP, zero manager LP and zero protocol LP. The account configured 1,500 bps admin performance and 40 bps issuance. All other performance, management and redemption rates were zero. The matching numerical balance in the earlier deposit does not prove continuity, historical ownership or receipt into a treasury. A Voltr harvest reports each recipient's aggregate LP. It provides no assessment dates, fee-source lots or holder weights. These individual receipts establish no positive per-referral funding balance. [Pinned Voltr harvest event layout](https://unpkg.com/@voltr/vault-sdk@2.1.1/src/generated/types/harvestFeeEvent.ts)

A future accounting period can start from a finalized harvest with the admin mint into its verified treasury account and a verified zero post-harvest admin accumulator. Exclude all shares collected at that opening anchor. For every subsequent assessment:

1. Capture complete finalized vault activity and historical fee schedules in block, transaction and instruction order. Record the pre- and post-invocation supply and corroborate the admin accumulator movement. Quarantine any interval with a coverage gap, a mixed fee source that cannot be decomposed, or ambiguous instruction effects.
2. At the assessment boundary, prove each qualifying friend's own eligible LP and total fee-inclusive supply. Exclude requested LP in withdrawal escrow; eligibility resumes on confirmed return. Resolve LP changes within the same block and transaction using instruction order. An end-of-slot position alone cannot establish that weight.
3. Reconcile the later harvest and actual treasury mint to outstanding assessment lots. Allocate at most each friend's own share, preserve integer remainders and identify already-consumed lots across retries. Any settlement in USDC additionally requires the observed proceeds or an explicitly defined conversion.

The current capital evidence is transaction-granular and does not prove ordering for every same-slot or same-transaction assessment. Standard transaction metadata does not include arbitrary vault-account byte snapshots at each invocation. A verified opening plus prospective finalized account updates, ordered receipts and unambiguous holder movements can support future funded allocations. The read-only receipt audit makes no payments or ledger entries.
