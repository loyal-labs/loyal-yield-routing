# Workers v2 simplification pass

Base: routing `4c57a712cfd1b067de05f9a7a51b55bc6e206c0b`.
Runtime proof: `85e92d0831d7e9aa0a66f45c54830a7ca16ab5f3`, CI 37183509997.
Root integration: `codex/workers-v2-simplify`, neighboring isolated worktree.
User requested fast Sol subagents at low reasoning. Use native `gpt-6.1-sol`,
low reasoning; root reviews and steers. No external GLM provider is involved.

## Outcome and constraints

This pass must eliminate demonstrated translation layers or duplicate mechanics,
not merely move code into smaller files. Physical source lines are a supporting
metric; fewer representations, parsing paths and decisions are the primary
criteria. Never shorten financial guards to meet a line-count target.

Preserve docs/workers-v2/contracts.md contracts 1-9. No schemas, dependencies,
production inspection/secrets, live transactions, migration resources, Apps edits,
main merges, deployments or service retirement. Retain family-specific custody,
exact signed wires, ambiguous recovery, desired intent and telemetry release
frontiers. Shared decoding does not grant authority or collapse journals.

## Lanes

| Lane | Implemented deletion target | Owned paths |
| --- | --- | --- |
| A | Remove Autodeposit's internal numeric process-exit round trip; return typed family outcomes directly | internal/autodeposit Go source/tests only |
| B | Replace duplicate Squads policy binary decoding with one bounded mechanical implementation, preserving each consumer's acceptance rules | fleet/revalidator.go and directly related policy tests; multiply/policymatch.go and directly related policy tests; new internal/squadspolicy if needed |
| C | Simplify Multiply recovery health: compute reused evidence predicates once in one coherent statement; remove duplicated claim-coverage logic | multiply/worker.go, existing runtime_health tests and a narrowly owned health-store helper if justified |

Lane task files contain exact boundaries. Agents work in separate worktrees,
never commit, push, spawn agents, or use connected secrets/resources. Root owns
integration and all shared entrypoint changes. No lane owns another's source file.
Send an early signature/deletion checkpoint before substantial implementation.

## Review and acceptance

1. Review each actual diff and trace retained outcomes against existing source.
2. Require a before/after deletion ledger: removed translation/duplicate logic,
   retained compatibility and concrete tests, including deliberately rejected input.
3. Run family semantic tests, targeted race tests and actual registered database
   gates for changed SQL/recovery, with root-serialized disposable fixture access.
4. Shared ABI changes require independent fixture/parity and authority negatives;
   a common implementation must preserve consumer-specific rejection behavior.
5. Run the combined verifier and affected connected financial/recovery proofs.
6. Record measured source delta and remaining complexity; commit/push only isolated
   branches after review. Production gates remain separate.

The initial baseline contains 57,301 physical lines in 186 non-test Go files under
internal. This includes comments and SQL and is not a semantic complexity score.
No percentage reduction is promised. Cosmetic relocation alone is not acceptance.
