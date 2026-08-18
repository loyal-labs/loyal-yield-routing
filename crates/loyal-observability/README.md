# loyal-observability

A shared crate for exporting privacy-safe logs, metrics, and traces from Loyal Rust services to an OTLP-compatible backend, currently ClickStack.

Current service integrations initialize this crate and emit bounded `OperationalError` records. They do not yet record `WorkflowMetrics` or `WorkflowSpan` signals; those APIs are available for explicit adoption by individual workflows.

When remote observability is enabled, SQLx's existing tracing events are also
converted automatically into privacy-safe database duration metrics. Services do
not need to wrap store calls or pass metric handles through their database layer.

## Signals

| ClickStack data source | Crate API | Exported data |
| --- | --- | --- |
| Logs | `OperationalError` | Explicit operational failures with stable codes and optional wallet correlation |
| Metrics | `WorkflowMetrics` | Low-cardinality execution counts and duration histograms |
| Metrics | automatic SQLx layer | PostgreSQL operation and connection-acquisition duration histograms |
| Traces | `WorkflowSpan` | Nested workflow operations with duration, outcome, and error status |

Regular `tracing` events and spans are not exported remotely. The OTLP layers accept only the bounded targets created by this crate. All regular events still use the local formatting layer controlled by `RUST_LOG`.

`OBSERVABILITY_ENABLED` controls all three remote exporters together. A service emits workflow metrics or traces only after its code explicitly calls `WorkflowMetrics` or `WorkflowSpan`.

## SQLx database metrics

The metrics subscriber listens only to SQLx's `sqlx::query` and
`sqlx::pool::acquire` tracing targets. It extracts the numeric durations already
measured by SQLx and emits:

| Metric | Type | Unit | Attributes |
| --- | --- | --- | --- |
| `db.client.operation.duration` | Histogram | `s` | `db.system.name=postgresql`, `db.operation.name` |
| `db.client.connection.wait_time` | Histogram | `s` | `db.system.name=postgresql` |

`db.operation.name` is deliberately fixed to `OTHER`. SQLx exposes the specific
operation only inside its query payload, and this layer does not inspect that
payload. SQL statements, bind values, SQLx query summaries, row values, and all
other event fields are discarded rather than copied to metric attributes or
remote logs. If per-operation breakdowns become necessary, callers should add a
separate bounded static operation label instead of parsing SQL here.

SQLx emits query timing events at `DEBUG` for normal statements and `WARN` for
slow statements. The dedicated per-layer filter observes both without enabling
unrelated debug events or changing the local `RUST_LOG` output. SQLx emits slow
pool acquisitions by default. `NeonSqlClient` also enables SQLx's normal
acquisition event at `DEBUG`, so every successful acquisition contributes to
`db.client.connection.wait_time` without changing local log verbosity.

The histograms use OpenTelemetry's recommended database duration boundaries:
1 ms, 5 ms, 10 ms, 50 ms, 100 ms, 500 ms, 1 s, 5 s, and 10 s. Exact normalized
statement diagnosis remains the database's responsibility through tools such as
PostgreSQL `pg_stat_statements`; these client metrics answer whether a service's
database calls or connection acquisition are becoming slow.

## Initialization

Initialization must replace the binary's existing `tracing_subscriber::fmt().init()` call:

```rust
use loyal_observability::init_from_env;

fn main() -> anyhow::Result<()> {
    let observability = init_from_env("loyal-yield-orchestrator")?;

    // Keep the guard alive until shutdown.
    run_service(&observability)?;

    observability.shutdown()?;
    Ok(())
}
```

The guard owns all enabled OpenTelemetry providers. Its `Drop` implementation shuts them down and flushes queued telemetry. Controlled shutdown flows can call `force_flush()` or `shutdown()` explicitly.

Dispatch any startup-safe probe that promises `secretsLoaded: false` before calling `init_from_env`. When remote export is enabled, initialization reads the exporter endpoint and ingestion key.

## Operational error logs

```rust
use loyal_observability::OperationalError;

OperationalError::new(
    "route_execution_failed",
    "execute_route",
    "yield route execution failed",
)
.retryable(true)
.recovery_required(false)
.emit();
```

Each operational event contains:

- `error_code`: a stable machine-readable code;
- `loyal.error.code`: the same stable code under the shared ClickStack alert attribute;
- `operation`: a stable operation name;
- `message`: a short operator-facing description;
- `retryable`: whether retrying is expected to be safe;
- `recovery_required`: whether an operator or repair flow must take action;
- `loyal.wallet.address`: an optional raw wallet address.

The `error_code`, `operation`, and `message` fields accept only `&'static str`. Runtime errors and user data cannot be passed accidentally.

## Workflow metrics

`WorkflowMetrics` exports two instruments:

| Metric | Type | Unit |
| --- | --- | --- |
| `loyal.workflow.executions` | Counter | `{execution}` |
| `loyal.workflow.duration` | Histogram | `s` |

Both instruments use only these bounded attributes:

- `loyal.workflow.name`;
- `loyal.workflow.operation`;
- `loyal.workflow.outcome`: `succeeded`, `failed`, or `skipped`.

Wallet addresses are deliberately excluded from metrics because they would create a high-cardinality dimension.

```rust
use std::time::Instant;

use loyal_observability::{ObservabilityGuard, WorkflowOutcome};

fn run_reconciliation(observability: &ObservabilityGuard) -> anyhow::Result<()> {
    let started_at = Instant::now();
    let result = reconcile_accounts();
    let outcome = if result.is_ok() {
        WorkflowOutcome::Succeeded
    } else {
        WorkflowOutcome::Failed
    };

    observability.workflow_metrics().record_execution(
        "reconcile",
        "reconcile.run",
        outcome,
        started_at.elapsed(),
    );

    result
}

# fn reconcile_accounts() -> anyhow::Result<()> { Ok(()) }
```

The returned metrics handle is a no-op when `OBSERVABILITY_ENABLED` is false, so call sites do not need their own feature branch.

## Workflow traces

`WorkflowSpan` creates only privacy-safe spans with stable workflow and operation names. It records duration automatically when the span closes. Use `succeeded()`, `skipped()`, `failed()`, or `finish_from_result()` once before closing the span. `finish_from_result()` inspects only whether a result succeeded and never formats or exports the contained error.

### Synchronous nesting

Create a child while its parent is entered:

```rust
use loyal_observability::WorkflowSpan;

let reconciliation = WorkflowSpan::new("reconcile", "reconcile.run");
let _reconciliation_guard = reconciliation.enter();

let result = (|| -> anyhow::Result<()> {
    {
        let load_expected = WorkflowSpan::new("reconcile", "reconcile.load_expected");
        let _load_guard = load_expected.enter();
        let result = load_expected_balances();
        load_expected.finish_from_result(&result, "load_expected_failed");
        result?;
    }

    {
        let compare = WorkflowSpan::new("reconcile", "reconcile.compare_balances");
        let _compare_guard = compare.enter();
        let result = compare_balances();
        compare.finish_from_result(&result, "balance_comparison_failed");
        result?;
    }

    Ok(())
})();

reconciliation.finish_from_result(&result, "reconcile_failed");
result?;

# fn load_expected_balances() -> anyhow::Result<()> { Ok(()) }
# fn compare_balances() -> anyhow::Result<()> { Ok(()) }
# Ok::<(), anyhow::Error>(())
```

### Async auto-deposit chain

Use `tracing::Instrument` to keep the parent active while an async future runs. Create child spans inside the instrumented future so ClickStack receives one connected trace:

```rust
use std::time::Instant;

use loyal_observability::{
    ObservabilityGuard, WorkflowSpan,
};
use tracing::Instrument;

async fn run_autodeposit(
    observability: &ObservabilityGuard,
) -> anyhow::Result<()> {
    let started_at = Instant::now();
    let run = WorkflowSpan::new("autodeposit", "autodeposit.run");

    let result = async {
        let evaluate = WorkflowSpan::new(
            "autodeposit",
            "autodeposit.evaluate_candidate",
        );
        let candidate_result = evaluate_candidate()
            .instrument(evaluate.span().clone())
            .await;
        evaluate.finish_from_result(
            &candidate_result,
            "candidate_evaluation_failed",
        );
        let candidate = candidate_result?;

        let submit = WorkflowSpan::new(
            "autodeposit",
            "autodeposit.submit_transaction",
        );
        let submit_result = submit_transaction(candidate)
            .instrument(submit.span().clone())
            .await;
        submit.finish_from_result(
            &submit_result,
            "transaction_submission_failed",
        );
        submit_result?;

        let confirm = WorkflowSpan::new(
            "autodeposit",
            "autodeposit.confirm_transaction",
        );
        let confirm_result = confirm_transaction()
            .instrument(confirm.span().clone())
            .await;
        confirm.finish_from_result(
            &confirm_result,
            "transaction_confirmation_failed",
        );
        confirm_result?;

        Ok(())
    }
    .instrument(run.span().clone())
    .await;

    let outcome = run.finish_from_result(&result, "autodeposit_failed");

    observability.workflow_metrics().record_execution(
        "autodeposit",
        "autodeposit.run",
        outcome,
        started_at.elapsed(),
    );

    result
}

# async fn evaluate_candidate() -> anyhow::Result<u64> { Ok(1) }
# async fn submit_transaction(_: u64) -> anyhow::Result<()> { Ok(()) }
# async fn confirm_transaction() -> anyhow::Result<()> { Ok(()) }
```

This produces a trace shaped like:

```text
autodeposit.run
├── autodeposit.evaluate_candidate
├── autodeposit.submit_transaction
└── autodeposit.confirm_transaction
```

A reconciliation trace can use the same pattern:

```text
reconcile.run
├── reconcile.load_expected
├── reconcile.load_observed
├── reconcile.compare_balances
└── reconcile.apply_repair
```

Record only stable error codes with `WorkflowSpan::failed`. Do not pass formatted runtime errors or payloads as the error code.

## Wallet correlation

Operational errors and workflow spans can carry the user's raw wallet address,
exported as `loyal.wallet.address`. It is stored verbatim, so a stored event is
directly linkable to on-chain identity and history. Treat telemetry carrying it
as operator-only and restrict dashboard access accordingly.

```rust
use loyal_observability::{ObservabilityWalletAddress, OperationalError, WorkflowSpan};

if let Some(wallet) = ObservabilityWalletAddress::new(wallet_address) {
    OperationalError::new(
        "autodeposit_failed",
        "autodeposit.run",
        "auto-deposit workflow failed",
    )
    .wallet_address(wallet.clone())
    .emit();

    let trace = WorkflowSpan::new("autodeposit", "autodeposit.run")
        .wallet_address(&wallet);
    // Use the trace span here.
}

# let wallet_address = "11111111111111111111111111111111";
```

Only surrounding whitespace is removed. Case is preserved because Solana base58
addresses are case-sensitive. An empty or whitespace-only address yields `None`
and `loyal.wallet.address` is omitted.

## Privacy and cardinality rules

Do not include the following data in logs, metric attributes, span attributes, names, or statuses:

- private keys, tokens, or authorization header values;
- user identifiers other than the wallet address;
- transaction payloads, account data, or request and response bodies;
- complete `anyhow::Error` or `Debug` output;
- SQL or query parameter values.

Use stable workflow names, operation names, outcomes, and error codes. Do not use transaction signatures, wallet addresses, route IDs, or timestamps as metric attributes. Wallet addresses belong on logs and traces only, never on metrics, where they would explode cardinality.

## Environment variables

All remote export is disabled by default. One switch enables or disables operational error logs, workflow metrics, and workflow traces together.

| Variable | Purpose |
| --- | --- |
| `OBSERVABILITY_ENABLED` | Enables operational error logs, workflow metrics, and workflow traces together |
| `OBSERVABILITY_ENVIRONMENT` | Sets `deployment.environment.name`; defaults to `unknown` |
| `OBSERVABILITY_SERVICE_VERSION` | Optional service-version override |
| `LOYAL_IMAGE_VERSION` | Immutable version embedded by Loyal worker-image builds |
| `OBSERVABILITY_OTLP_ENDPOINT` | Sets the shared base HTTP OTLP endpoint for logs, metrics, and traces |
| `OBSERVABILITY_INGESTION_API_KEY` | Server-only ClickStack ingestion key used as the `authorization` header |
| `OTEL_METRIC_EXPORT_INTERVAL` | Metric export interval in milliseconds; defaults to `60000` |
| `OTEL_TRACES_SAMPLER` | Trace sampler; defaults to `parentbased_always_on` |
| `OTEL_TRACES_SAMPLER_ARG` | Ratio for `traceidratio` or `parentbased_traceidratio` |
| `RUST_LOG` | Configures only the local formatting layer; defaults to `warn` |

Treat `OBSERVABILITY_OTLP_ENDPOINT` as the collector base URL. The exporter replaces any existing path, query, or fragment with `/v1/logs`, `/v1/metrics`, or `/v1/traces` for the corresponding signal.

Store `OBSERVABILITY_INGESTION_API_KEY` as a secret environment variable. The crate constructs the `authorization` header internally and does not expose the key through configuration `Debug` output or logs. Do not use `NEXT_PUBLIC_*` variables for server-side telemetry secrets.

When `OBSERVABILITY_ENABLED=true`, both `OBSERVABILITY_OTLP_ENDPOINT` and `OBSERVABILITY_INGESTION_API_KEY` are required. Missing either value is a startup error.

Render metadata is discovered automatically:

- `RENDER_SERVICE_NAME` maps to `service.name`;
- `RENDER_INSTANCE_ID` maps to `service.instance.id`;
- `RENDER_SERVICE_ID` maps to `render.service.id`;
- `service.version` resolves from `OBSERVABILITY_SERVICE_VERSION`, then
  `LOYAL_IMAGE_VERSION`, then `RENDER_GIT_COMMIT`. Image-backed workers normally
  use the embedded image version because Render does not expose their deployed
  image tag as a documented runtime variable.

## Verification

```sh
cargo test -p loyal-observability --locked
cargo check -p loyal-observability --locked
cargo clippy -p loyal-observability --locked -- -D warnings
cargo fmt -p loyal-observability -- --check
```

Binary integration, Render environment changes, and ClickStack canary verification are separate deployment steps.
