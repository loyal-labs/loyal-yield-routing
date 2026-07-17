//! Privacy-safe logs, metrics, and traces for Loyal Rust services.
//!
//! Remote log and trace layers accept only events and spans created by this
//! crate's bounded APIs. Regular `tracing` data remains in the local formatting
//! layer and is not copied to the remote collector.

#![forbid(unsafe_code)]

mod actor;
mod workflow;

use std::{
    env,
    error::Error,
    fmt::{self, Display, Formatter},
};

use opentelemetry::{metrics::MeterProvider as _, trace::TracerProvider as _, KeyValue};
use opentelemetry_appender_tracing::layer::OpenTelemetryTracingBridge;
use opentelemetry_otlp::{LogExporter, MetricExporter, Protocol, SpanExporter, WithExportConfig};
use opentelemetry_sdk::{
    logs::SdkLoggerProvider, metrics::SdkMeterProvider, trace::SdkTracerProvider, Resource,
};
use tracing::Level;
use tracing_subscriber::{filter, layer::SubscriberExt, util::SubscriberInitExt, EnvFilter, Layer};

pub use actor::{
    derive_observability_actor_id, derive_observability_actor_id_from_env, ObservabilityActorId,
    ACTOR_HMAC_SECRET_ENV,
};
use workflow::WORKFLOW_TRACE_TARGET;
pub use workflow::{WorkflowMetrics, WorkflowOutcome, WorkflowSpan};

/// The only `tracing` target exported by this crate's OTLP layer.
const OPERATIONAL_ERROR_TARGET: &str = "loyal.observability.operational_error";

/// Enables the remote OTLP exporter when set to a truthy value.
pub const ENABLED_ENV: &str = "LOYAL_OBSERVABILITY_ENABLED";

/// Enables OTLP metrics in addition to operational error logs.
pub const METRICS_ENABLED_ENV: &str = "LOYAL_OBSERVABILITY_METRICS_ENABLED";

/// Enables OTLP traces in addition to operational error logs.
pub const TRACES_ENABLED_ENV: &str = "LOYAL_OBSERVABILITY_TRACES_ENABLED";

/// Sets `deployment.environment.name` on exported records.
pub const ENVIRONMENT_ENV: &str = "LOYAL_OBSERVABILITY_ENVIRONMENT";

/// Overrides the service version discovered from `RENDER_GIT_COMMIT`.
pub const SERVICE_VERSION_ENV: &str = "LOYAL_OBSERVABILITY_SERVICE_VERSION";

const OTLP_ENDPOINT_ENV: &str = "OTEL_EXPORTER_OTLP_ENDPOINT";
const OTLP_LOGS_ENDPOINT_ENV: &str = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT";
const OTLP_METRICS_ENDPOINT_ENV: &str = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT";
const OTLP_TRACES_ENDPOINT_ENV: &str = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT";

/// Non-secret configuration for the observability subscriber.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ObservabilityConfig {
    /// Whether operational errors should be exported through OTLP.
    pub enabled: bool,
    /// Whether low-cardinality workflow metrics should be exported through OTLP.
    pub metrics_enabled: bool,
    /// Whether workflow spans should be exported through OTLP.
    pub traces_enabled: bool,
    /// Logical service name used by ClickStack and other OTLP backends.
    pub service_name: String,
    /// Deployment environment, for example `production` or `staging`.
    pub deployment_environment: String,
    /// Immutable deployed version, normally the image or Git SHA.
    pub service_version: Option<String>,
    /// Runtime instance identifier.
    pub service_instance_id: Option<String>,
    /// Render service identifier, recorded as `render.service.id`.
    pub render_service_id: Option<String>,
    /// Filter for the local formatting layer. Defaults to `RUST_LOG` or `info`.
    pub stdout_filter: String,
}

impl ObservabilityConfig {
    /// Reads non-secret service metadata from the process environment.
    ///
    /// The OTLP exporter itself reads the standard `OTEL_EXPORTER_OTLP_*`
    /// variables. In particular, authentication headers remain outside this
    /// config so they cannot be exposed through its `Debug` implementation.
    pub fn from_env(default_service_name: impl Into<String>) -> Result<Self, InitError> {
        let default_service_name = default_service_name.into();
        let enabled = parse_enabled(ENABLED_ENV, env::var(ENABLED_ENV).ok().as_deref())?;
        let metrics_enabled = parse_enabled(
            METRICS_ENABLED_ENV,
            env::var(METRICS_ENABLED_ENV).ok().as_deref(),
        )?;
        let traces_enabled = parse_enabled(
            TRACES_ENABLED_ENV,
            env::var(TRACES_ENABLED_ENV).ok().as_deref(),
        )?;
        let service_name = non_empty_env("RENDER_SERVICE_NAME").unwrap_or(default_service_name);

        if service_name.trim().is_empty() {
            return Err(InitError::InvalidConfig(
                "service name must not be empty".to_owned(),
            ));
        }

        let config = Self {
            enabled,
            metrics_enabled,
            traces_enabled,
            service_name,
            deployment_environment: non_empty_env(ENVIRONMENT_ENV)
                .unwrap_or_else(|| "unknown".to_owned()),
            service_version: non_empty_env(SERVICE_VERSION_ENV)
                .or_else(|| non_empty_env("RENDER_GIT_COMMIT")),
            service_instance_id: non_empty_env("RENDER_INSTANCE_ID"),
            render_service_id: non_empty_env("RENDER_SERVICE_ID"),
            stdout_filter: non_empty_env("RUST_LOG").unwrap_or_else(|| "info".to_owned()),
        };

        validate_config(&config)?;
        validate_export_endpoints(&config)?;
        Ok(config)
    }
}

/// A deliberately small, privacy-safe error record.
///
/// Text fields require static strings so runtime errors, wallet addresses,
/// request bodies, transaction payloads, and other user data cannot be passed
/// accidentally. Add only stable classifications and operator-facing summaries.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct OperationalError {
    actor_id: Option<ObservabilityActorId>,
    code: &'static str,
    operation: &'static str,
    summary: &'static str,
    retryable: bool,
    recovery_required: bool,
}

impl OperationalError {
    /// Creates an operational error with stable, non-user-specific fields.
    pub const fn new(code: &'static str, operation: &'static str, summary: &'static str) -> Self {
        Self {
            actor_id: None,
            code,
            operation,
            summary,
            retryable: false,
            recovery_required: false,
        }
    }

    /// Marks whether retrying the failed operation is expected to be safe.
    pub const fn retryable(mut self, retryable: bool) -> Self {
        self.retryable = retryable;
        self
    }

    /// Marks whether an operator or a repair flow must take action.
    pub const fn recovery_required(mut self, recovery_required: bool) -> Self {
        self.recovery_required = recovery_required;
        self
    }

    /// Attaches a pseudonymous actor ID without retaining the raw wallet address.
    pub fn actor_id(mut self, actor_id: ObservabilityActorId) -> Self {
        self.actor_id = Some(actor_id);
        self
    }

    /// Emits this record to local logs and, when enabled, the filtered OTLP layer.
    pub fn emit(self) {
        if let Some(actor_id) = self.actor_id {
            tracing::event!(
                name: "loyal.operational_error",
                target: OPERATIONAL_ERROR_TARGET,
                Level::ERROR,
                {
                    loyal.actor.id = actor_id.as_str(),
                    error_code = self.code,
                    operation = self.operation,
                    retryable = self.retryable,
                    recovery_required = self.recovery_required,
                    message = self.summary,
                }
            );
        } else {
            tracing::error!(
                name: "loyal.operational_error",
                target: OPERATIONAL_ERROR_TARGET,
                error_code = self.code,
                operation = self.operation,
                retryable = self.retryable,
                recovery_required = self.recovery_required,
                message = self.summary,
            );
        }
    }
}

/// Owns the OTLP providers and flushes them when dropped.
///
/// Keep this value alive until the service has finished shutting down.
pub struct ObservabilityGuard {
    providers: Providers,
    workflow_metrics: WorkflowMetrics,
}

impl ObservabilityGuard {
    /// Returns whether operational error log export is enabled.
    pub fn is_enabled(&self) -> bool {
        self.providers.logger.is_some()
    }

    /// Returns whether the metrics exporter is enabled.
    pub fn metrics_enabled(&self) -> bool {
        self.providers.meter.is_some()
    }

    /// Returns whether the trace exporter is enabled.
    pub fn traces_enabled(&self) -> bool {
        self.providers.tracer.is_some()
    }

    /// Returns the low-cardinality workflow metric instruments.
    ///
    /// The returned handle is a no-op when metrics are disabled.
    pub fn workflow_metrics(&self) -> WorkflowMetrics {
        self.workflow_metrics.clone()
    }

    /// Flushes queued records without shutting the provider down.
    pub fn force_flush(&self) -> Result<(), LifecycleError> {
        force_flush_providers(&self.providers)
    }

    /// Flushes queued records and shuts the provider down immediately.
    pub fn shutdown(mut self) -> Result<(), LifecycleError> {
        shutdown_providers(&mut self.providers)
    }
}

impl Drop for ObservabilityGuard {
    fn drop(&mut self) {
        let _ = shutdown_providers(&mut self.providers);
    }
}

#[derive(Default)]
struct Providers {
    logger: Option<SdkLoggerProvider>,
    meter: Option<SdkMeterProvider>,
    tracer: Option<SdkTracerProvider>,
}

/// Initializes local structured logging and the optional OTLP exporter.
pub fn init_from_env(
    default_service_name: impl Into<String>,
) -> Result<ObservabilityGuard, InitError> {
    init(ObservabilityConfig::from_env(default_service_name)?)
}

/// Initializes local structured logging and the optional OTLP exporter.
///
/// This installs the process-global `tracing` subscriber. A binary adopting the
/// crate must replace its existing subscriber initialization with this call.
pub fn init(config: ObservabilityConfig) -> Result<ObservabilityGuard, InitError> {
    validate_config(&config)?;
    validate_export_endpoints(&config)?;

    let stdout_filter = EnvFilter::try_new(&config.stdout_filter)
        .map_err(|error| InitError::InvalidFilter(error.to_string()))?;
    let fmt_layer = tracing_subscriber::fmt::layer().with_filter(stdout_filter);

    if !config.enabled {
        tracing_subscriber::registry()
            .with(fmt_layer)
            .try_init()
            .map_err(|error| InitError::Subscriber(error.to_string()))?;
        return Ok(ObservabilityGuard {
            providers: Providers::default(),
            workflow_metrics: WorkflowMetrics::default(),
        });
    }

    let shared_resource = resource(&config);
    let log_exporter = LogExporter::builder()
        .with_http()
        .with_protocol(Protocol::HttpBinary)
        .build()
        .map_err(|error| InitError::Exporter(format!("logs: {error}")))?;
    let logger_provider = SdkLoggerProvider::builder()
        .with_resource(shared_resource.clone())
        .with_batch_exporter(log_exporter)
        .build();

    let (meter_provider, workflow_metrics) = if config.metrics_enabled {
        let metric_exporter = MetricExporter::builder()
            .with_http()
            .with_protocol(Protocol::HttpBinary)
            .build()
            .map_err(|error| InitError::Exporter(format!("metrics: {error}")))?;
        let provider = SdkMeterProvider::builder()
            .with_resource(shared_resource.clone())
            .with_periodic_exporter(metric_exporter)
            .build();
        let meter = provider.meter("loyal-observability");
        let metrics = WorkflowMetrics::new(&meter);
        (Some(provider), metrics)
    } else {
        (None, WorkflowMetrics::default())
    };

    let tracer_provider = if config.traces_enabled {
        let span_exporter = SpanExporter::builder()
            .with_http()
            .with_protocol(Protocol::HttpBinary)
            .build()
            .map_err(|error| InitError::Exporter(format!("traces: {error}")))?;
        Some(
            SdkTracerProvider::builder()
                .with_resource(shared_resource)
                .with_batch_exporter(span_exporter)
                .build(),
        )
    } else {
        None
    };

    let mut providers = Providers {
        logger: Some(logger_provider),
        meter: meter_provider,
        tracer: tracer_provider,
    };

    let operational_errors_only = filter::filter_fn(|metadata| {
        metadata.target() == OPERATIONAL_ERROR_TARGET && *metadata.level() == Level::ERROR
    });
    let log_layer = OpenTelemetryTracingBridge::new(
        providers
            .logger
            .as_ref()
            .expect("logger provider is installed when observability is enabled"),
    )
    .with_filter(operational_errors_only);
    let trace_layer = providers.tracer.as_ref().map(|provider| {
        let workflow_traces_only =
            filter::filter_fn(|metadata| metadata.target() == WORKFLOW_TRACE_TARGET);
        tracing_opentelemetry::layer()
            .with_tracer(provider.tracer("loyal-observability"))
            .with_filter(workflow_traces_only)
    });

    if let Err(error) = tracing_subscriber::registry()
        .with(fmt_layer)
        .with(log_layer)
        .with(trace_layer)
        .try_init()
    {
        let _ = shutdown_providers(&mut providers);
        return Err(InitError::Subscriber(error.to_string()));
    }

    Ok(ObservabilityGuard {
        providers,
        workflow_metrics,
    })
}

/// Failure while configuring or installing the subscriber.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum InitError {
    /// A configuration value is missing or invalid.
    InvalidConfig(String),
    /// `RUST_LOG` or the explicit stdout filter is invalid.
    InvalidFilter(String),
    /// The OTLP exporter could not be built.
    Exporter(String),
    /// Another global subscriber is already installed or installation failed.
    Subscriber(String),
}

impl Display for InitError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfig(message) => {
                write!(formatter, "invalid observability config: {message}")
            }
            Self::InvalidFilter(message) => write!(formatter, "invalid log filter: {message}"),
            Self::Exporter(message) => {
                write!(formatter, "failed to build OTLP exporter: {message}")
            }
            Self::Subscriber(message) => {
                write!(formatter, "failed to install tracing subscriber: {message}")
            }
        }
    }
}

impl Error for InitError {}

/// Failure while flushing or shutting down the OTLP provider.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LifecycleError(String);

impl Display for LifecycleError {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        write!(formatter, "observability lifecycle failed: {}", self.0)
    }
}

impl Error for LifecycleError {}

fn validate_config(config: &ObservabilityConfig) -> Result<(), InitError> {
    if config.service_name.trim().is_empty() {
        return Err(InitError::InvalidConfig(
            "service name must not be empty".to_owned(),
        ));
    }
    if config.deployment_environment.trim().is_empty() {
        return Err(InitError::InvalidConfig(
            "deployment environment must not be empty".to_owned(),
        ));
    }
    if !config.enabled && config.metrics_enabled {
        return Err(InitError::InvalidConfig(format!(
            "{METRICS_ENABLED_ENV} requires {ENABLED_ENV}"
        )));
    }
    if !config.enabled && config.traces_enabled {
        return Err(InitError::InvalidConfig(format!(
            "{TRACES_ENABLED_ENV} requires {ENABLED_ENV}"
        )));
    }
    Ok(())
}

fn validate_export_endpoints(config: &ObservabilityConfig) -> Result<(), InitError> {
    if !config.enabled {
        return Ok(());
    }

    require_signal_endpoint("logs", OTLP_LOGS_ENDPOINT_ENV)?;
    if config.metrics_enabled {
        require_signal_endpoint("metrics", OTLP_METRICS_ENDPOINT_ENV)?;
    }
    if config.traces_enabled {
        require_signal_endpoint("traces", OTLP_TRACES_ENDPOINT_ENV)?;
    }
    Ok(())
}

fn require_signal_endpoint(signal: &str, signal_endpoint_env: &str) -> Result<(), InitError> {
    if non_empty_env(signal_endpoint_env).is_some() || non_empty_env(OTLP_ENDPOINT_ENV).is_some() {
        return Ok(());
    }

    Err(InitError::InvalidConfig(format!(
        "{signal} export is enabled but neither {signal_endpoint_env} nor {OTLP_ENDPOINT_ENV} is set"
    )))
}

fn resource(config: &ObservabilityConfig) -> Resource {
    let mut builder = Resource::builder()
        .with_service_name(config.service_name.clone())
        .with_attribute(KeyValue::new("service.namespace", "loyal"))
        .with_attribute(KeyValue::new(
            "deployment.environment.name",
            config.deployment_environment.clone(),
        ));

    if let Some(version) = &config.service_version {
        builder = builder.with_attribute(KeyValue::new("service.version", version.clone()));
    }
    if let Some(instance_id) = &config.service_instance_id {
        builder = builder.with_attribute(KeyValue::new("service.instance.id", instance_id.clone()));
    }
    if let Some(service_id) = &config.render_service_id {
        builder = builder.with_attribute(KeyValue::new("render.service.id", service_id.clone()));
    }

    builder.build()
}

fn parse_enabled(name: &str, value: Option<&str>) -> Result<bool, InitError> {
    match value.map(str::trim).map(str::to_ascii_lowercase).as_deref() {
        None | Some("") | Some("0" | "false" | "no" | "off") => Ok(false),
        Some("1" | "true" | "yes" | "on") => Ok(true),
        Some(value) => Err(InitError::InvalidConfig(format!(
            "{name} must be one of true/false, 1/0, yes/no, or on/off; got {value:?}"
        ))),
    }
}

fn non_empty_env(name: &str) -> Option<String> {
    env::var(name)
        .ok()
        .map(|value| value.trim().to_owned())
        .filter(|value| !value.is_empty())
}

fn force_flush_providers(providers: &Providers) -> Result<(), LifecycleError> {
    let mut errors = Vec::new();

    if let Some(provider) = &providers.logger {
        if let Err(error) = provider.force_flush() {
            errors.push(format!("logs: {error}"));
        }
    }
    if let Some(provider) = &providers.meter {
        if let Err(error) = provider.force_flush() {
            errors.push(format!("metrics: {error}"));
        }
    }
    if let Some(provider) = &providers.tracer {
        if let Err(error) = provider.force_flush() {
            errors.push(format!("traces: {error}"));
        }
    }

    lifecycle_result(errors)
}

fn shutdown_providers(providers: &mut Providers) -> Result<(), LifecycleError> {
    let mut errors = Vec::new();

    if let Some(provider) = providers.tracer.take() {
        if let Err(error) = provider.shutdown() {
            errors.push(format!("traces: {error}"));
        }
    }
    if let Some(provider) = providers.meter.take() {
        if let Err(error) = provider.shutdown() {
            errors.push(format!("metrics: {error}"));
        }
    }
    if let Some(provider) = providers.logger.take() {
        if let Err(error) = provider.shutdown() {
            errors.push(format!("logs: {error}"));
        }
    }

    lifecycle_result(errors)
}

fn lifecycle_result(errors: Vec<String>) -> Result<(), LifecycleError> {
    if errors.is_empty() {
        Ok(())
    } else {
        Err(LifecycleError(errors.join("; ")))
    }
}
