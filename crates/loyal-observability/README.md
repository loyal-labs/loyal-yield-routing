# loyal-observability

A shared crate for sending privacy-safe operational errors from Loyal Rust services to an OTLP-compatible backend, currently ClickStack.

Current scope: the crate is part of the workspace, but no binary uses it yet. This change does not modify Render configuration, environment variables, or ClickStack. Production worker behavior remains unchanged.

## Exported data

The OTLP layer accepts only events with the `loyal.observability.operational_error` target emitted by `OperationalError::emit`. Other `tracing` events continue to use the local formatting layer (`stdout`) and are not exported remotely.

Each event contains a small, fixed set of fields:

- `error_code`: a stable machine-readable code;
- `operation`: a stable operation name;
- `message`: a short operator-facing description;
- `retryable`: whether retrying is expected to be safe;
- `recovery_required`: whether an operator or repair flow must take action.

The `error_code`, `operation`, and `message` fields accept only `&'static str`. This is intentional: runtime error text, wallet addresses, transaction payloads, request or response bodies, SQL, and secrets cannot be passed accidentally.

Do not include the following data in an operational error:

- private keys, tokens, or authorization header values;
- wallet or user identifiers;
- transactions, account data, or request and response bodies;
- complete `anyhow::Error` or `Debug` output;
- SQL or query parameter values.

## Future binary integration

Initialization must replace the binary's existing `tracing_subscriber::fmt().init()` call:

```rust
use loyal_observability::{init_from_env, OperationalError};

fn main() -> anyhow::Result<()> {
    let _observability = init_from_env("loyal-yield-orchestrator")?;

    // ...

    OperationalError::new(
        "route_execution_failed",
        "execute_route",
        "yield route execution failed",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();

    Ok(())
}
```

The guard must remain alive until the process finishes shutting down. Its `Drop` implementation shuts down the provider and flushes queued records. Controlled shutdown flows can also call `force_flush()` or `shutdown()` explicitly.

## Environment variables

The crate is disabled by default.

| Variable | Purpose |
| --- | --- |
| `LOYAL_OBSERVABILITY_ENABLED` | Enables the OTLP exporter when set to `true` or `1` |
| `LOYAL_OBSERVABILITY_ENVIRONMENT` | Sets `deployment.environment.name`; defaults to `unknown` |
| `LOYAL_OBSERVABILITY_SERVICE_VERSION` | Sets the service version; falls back to `RENDER_GIT_COMMIT` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Sets the base HTTP OTLP endpoint; the exporter appends `/v1/logs` |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | Sets the complete logs endpoint and takes precedence over the general endpoint |
| `OTEL_EXPORTER_OTLP_LOGS_HEADERS` | Sets logs request headers, for example `authorization=<ingestion-key>` |
| `RUST_LOG` | Configures only the local formatting layer; defaults to `info` |

Render metadata is discovered automatically:

- `RENDER_SERVICE_NAME` maps to `service.name`;
- `RENDER_INSTANCE_ID` maps to `service.instance.id`;
- `RENDER_SERVICE_ID` maps to `render.service.id`;
- `RENDER_GIT_COMMIT` maps to `service.version` unless explicitly overridden.

Store the ingestion key in a secret environment variable and pass it through `OTEL_EXPORTER_OTLP_LOGS_HEADERS`. The crate does not read it into its own configuration or expose it through `Debug` output or logs. Do not use `NEXT_PUBLIC_*` variables for server-side telemetry secrets.

When `LOYAL_OBSERVABILITY_ENABLED` is enabled, the absence of both endpoint variables is a startup error. An exporter configuration error must not silently fall back to localhost.

## Verification

```sh
cargo check -p loyal-observability
cargo fmt -p loyal-observability -- --check
```

The next independent step is to integrate the crate into selected binaries. Only after that should Render environment variables and secrets be added and a canary event verified in ClickStack.
