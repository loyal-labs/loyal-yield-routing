# ASK-2211 coalesced Autodeposit reconciliation verifier

Run `scripts/verify-ask-2211-coalesced-autodeposit-reconciliation.sh` from a clean
checkout with PostgreSQL development tools available.

The verifier must try to disprove all required properties below.

1. A burst of finalized LaserStream account notifications for one Autodeposit
   target leaves exactly one durable reconciliation request. Its requested slot
   is the greatest observed slot.
2. Claiming and completing that request records the snapshot slot as processed.
   A newer notification that arrives while the request is claimed remains ready
   afterward and is not lost.
3. Two different targets can be claimed at the same time by different workers.
   A lease prevents two workers from claiming the same target.
4. The request table has bounded cardinality, one row per target. It contains no
   raw account bytes, transaction JSON, event JSON, or append-only account
   history.
5. The LaserStream replay cursor advances in the same database transaction that
   records every applicable Autodeposit reconciliation request.
6. Ordinary Earn reconciliation no longer performs an Autodeposit account
   snapshot for every Earn job. A dedicated Autodeposit worker performs one
   finalized batched account snapshot per claimed target request.
7. Discovering the recurring delegation for a new target schedules its first
   snapshot without depending on a later LaserStream notification. Sibling
   policy-discovery account updates from the same transaction and vault create
   one durable discovery job, not one job per account.
8. The account-only LaserStream subscription remains account-only. No transaction
   subscription is introduced.
9. Earn reconciliation runs enough independent consumers that unrelated vaults
   are not serialized behind one global worker.
10. Existing focused Rust tests, formatting, and compilation pass.

Required verdict: print one PASS or FAIL line for each section. Print the final
line `PASS: ASK-2211 Autodeposit reconciliation is coalesced and bounded` only
when every required property passes. Any missing prerequisite or skipped check is
a failure.
