# Workers v2 implementation

All work stays on the assigned rewrite branch. Do not merge/push main, deploy,
run migrations against connected resources, submit chain transactions, or read
production secrets. Use saved fixtures and explicitly disposable local databases.
Do not read `.env*`, key material, unrelated agent logs or another lane's worktree.

Root owns shared packages, module dependencies, commands, schema changes and
integration. Each lane edits its allowlisted feature files only; request shared
changes in its final report. Do not spawn nested agents. Root commits reviewed
work; do not change git metadata from an implementation lane.

Read `docs/workers-v2/contracts.md` and the assigned task before implementation.
Reuse existing code. Keep family SQL and typed lifecycle logic together. No
generic workflow/operation/executor, ORM, plugin registry, global utility package,
or speculative interfaces. Planner/observer never receive signing capabilities.
Use concrete constructors and consumer-owned capability interfaces when needed.

Amounts remain integer and unit-tagged; unknown is not zero. Preserve desired
user intent, legacy IDs, financial event ordering and complete/partial semantics.
Persist exact signed wire before send, recover ambiguous attempts before fresh
work, and reserve owned custody/capacity until its family's proof permits release.
Database fencing cannot revoke already signed chain transactions.

IO accepts context and deadlines. The runtime owns bounded concurrency, cancellation
and joining. Explicit short database transactions; no network IO under long locks.
Never retry an ambiguous broadcast through a general retry wrapper.

Implement behavior before adding tests. Tests must assert financial, recovery,
authority, ordering or external compatibility outcomes, not source strings or
struct/default mirrors. Separate legacy parity from intentional corrections.

Use `GOTOOLCHAIN=local`, Go 1.25.1 language baseline and existing dependency pins.
Root owns dependency downloads/adjustments. No live opt-in tests or frontend builds.
Use package-focused `go test`, gofmt and vet; root runs the combined verifier.
Report actual commands/results and remaining gaps. Never call a scaffold complete.
