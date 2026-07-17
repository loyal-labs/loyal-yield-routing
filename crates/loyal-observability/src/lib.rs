//! Privacy-safe operational error reporting for Loyal Rust services.
//!
//! The OTLP layer created by this crate exports only events emitted through
//! [`OperationalError::emit`]. Regular `tracing` events remain in the local
//! formatting layer and are not copied to the remote collector.

#![forbid(unsafe_code)]

use std::{
    env,
    error::Error,
    fmt::{self, Display, Formatter},
};

use opentelemetry::KeyValue;
use opentelemetry_appender_tracing::layer::OpenTelemetryTracingBridge;
use opentelemetry_otlp::{LogExporter, Protocol, WithExportConfig};
use opentelemetry_sdk::{logs::SdkLoggerProvider, Resource};
use tracing::Level;
use tracing_subscriber::{filter, layer::SubscriberExt, util::SubscriberInitExt, EnvFilter, Layer};

/// The only `tracing` target exported by this crate's OTLP layer.
const OPERATIONAL_ERROR_TARGET: &str = "loyal.observability.operational_error";

/// Enables the remote OTLP exporter when set to a truthy value.
pub const ENABLED_ENV: &str = "LOYAL_OBSERVABILITY_ENABLED";

/// Sets `deployment.environment.name` on exported records.
pub const ENVIRONMENT_ENV: &str = "LOYAL_OBSERVABILITY_ENVIRONMENT";

/// Overrides the service version discovered from `RENDER_GIT_COMMIT`.
pub const SERVICE_VERSION_ENV: &str = "LOYAL_OBSERVABILITY_SERVICE_VERSION";

const OTLP_ENDPOINT_ENV: &str = "OTEL_EXPORTER_OTLP_ENDPOINT";
const OTLP_LOGS_ENDPOINT_ENV: &str = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT";

/// Non-secret configuration for the observability subscriber.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ObservabilityConfig {
    /// Whether operational errors should be exported through OTLP.
    pub enabled: bool,
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
        let enabled = parse_enabled(env::var(ENABLED_ENV).ok().as_deref())?;
        let service_name = non_empty_env("RENDER_SERVICE_NAME").unwrap_or(default_service_name);

        if service_name.trim().is_empty() {
            return Err(InitError::InvalidConfig(
                "service name must not be empty".to_owned(),
            ));
        }

        if enabled && !otlp_endpoint_is_configured() {
            return Err(InitError::InvalidConfig(format!(
                "{ENABLED_ENV} is enabled but neither {OTLP_LOGS_ENDPOINT_ENV} nor {OTLP_ENDPOINT_ENV} is set"
            )));
        }

        Ok(Self {
            enabled,
            service_name,
            deployment_environment: non_empty_env(ENVIRONMENT_ENV)
                .unwrap_or_else(|| "unknown".to_owned()),
            service_version: non_empty_env(SERVICE_VERSION_ENV)
                .or_else(|| non_empty_env("RENDER_GIT_COMMIT")),
            service_instance_id: non_empty_env("RENDER_INSTANCE_ID"),
            render_service_id: non_empty_env("RENDER_SERVICE_ID"),
            stdout_filter: non_empty_env("RUST_LOG").unwrap_or_else(|| "info".to_owned()),
        })
    }
}

/// A deliberately small, privacy-safe error record.
///
/// Text fields require static strings so runtime errors, wallet addresses,
/// request bodies, transaction payloads, and other user data cannot be passed
/// accidentally. Add only stable classifications and operator-facing summaries.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct OperationalError {
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

    /// Emits this record to local logs and, when enabled, the filtered OTLP layer.
    pub fn emit(self) {
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

/// Owns the OTLP logger provider and flushes it when dropped.
///
/// Keep this value alive until the service has finished shutting down.
pub struct ObservabilityGuard {
    provider: Option<SdkLoggerProvider>,
}

impl ObservabilityGuard {
    /// Returns whether a remote exporter was installed.
    pub fn is_enabled(&self) -> bool {
        self.provider.is_some()
    }

    /// Flushes queued records without shutting the provider down.
    pub fn force_flush(&self) -> Result<(), LifecycleError> {
        if let Some(provider) = &self.provider {
            provider
                .force_flush()
                .map_err(|error| LifecycleError(error.to_string()))?;
        }
        Ok(())
    }

    /// Flushes queued records and shuts the provider down immediately.
    pub fn shutdown(mut self) -> Result<(), LifecycleError> {
        shutdown_provider(&mut self.provider)
    }
}

impl Drop for ObservabilityGuard {
    fn drop(&mut self) {
        let _ = shutdown_provider(&mut self.provider);
    }
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

    let stdout_filter = EnvFilter::try_new(&config.stdout_filter)
        .map_err(|error| InitError::InvalidFilter(error.to_string()))?;
    let fmt_layer = tracing_subscriber::fmt::layer().with_filter(stdout_filter);

    if !config.enabled {
        tracing_subscriber::registry()
            .with(fmt_layer)
            .try_init()
            .map_err(|error| InitError::Subscriber(error.to_string()))?;
        return Ok(ObservabilityGuard { provider: None });
    }

    if !otlp_endpoint_is_configured() {
        return Err(InitError::InvalidConfig(format!(
            "remote export is enabled but neither {OTLP_LOGS_ENDPOINT_ENV} nor {OTLP_ENDPOINT_ENV} is set"
        )));
    }

    let exporter = LogExporter::builder()
        .with_http()
        .with_protocol(Protocol::HttpBinary)
        .build()
        .map_err(|error| InitError::Exporter(error.to_string()))?;
    let provider = SdkLoggerProvider::builder()
        .with_resource(resource(&config))
        .with_batch_exporter(exporter)
        .build();
    let operational_errors_only = filter::filter_fn(|metadata| {
        metadata.target() == OPERATIONAL_ERROR_TARGET && *metadata.level() == Level::ERROR
    });
    let otlp_layer =
        OpenTelemetryTracingBridge::new(&provider).with_filter(operational_errors_only);

    if let Err(error) = tracing_subscriber::registry()
        .with(fmt_layer)
        .with(otlp_layer)
        .try_init()
    {
        let _ = provider.shutdown();
        return Err(InitError::Subscriber(error.to_string()));
    }

    Ok(ObservabilityGuard {
        provider: Some(provider),
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
                write!(formatter, "failed to build OTLP log exporter: {message}")
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
        write!(
            formatter,
            "failed to flush observability records: {}",
            self.0
        )
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
    Ok(())
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

fn parse_enabled(value: Option<&str>) -> Result<bool, InitError> {
    match value.map(str::trim).map(str::to_ascii_lowercase).as_deref() {
        None | Some("") | Some("0" | "false" | "no" | "off") => Ok(false),
        Some("1" | "true" | "yes" | "on") => Ok(true),
        Some(value) => Err(InitError::InvalidConfig(format!(
            "{ENABLED_ENV} must be one of true/false, 1/0, yes/no, or on/off; got {value:?}"
        ))),
    }
}

fn non_empty_env(name: &str) -> Option<String> {
    env::var(name)
        .ok()
        .map(|value| value.trim().to_owned())
        .filter(|value| !value.is_empty())
}

fn otlp_endpoint_is_configured() -> bool {
    non_empty_env(OTLP_LOGS_ENDPOINT_ENV).is_some() || non_empty_env(OTLP_ENDPOINT_ENV).is_some()
}

fn shutdown_provider(provider: &mut Option<SdkLoggerProvider>) -> Result<(), LifecycleError> {
    if let Some(provider) = provider.take() {
        provider
            .shutdown()
            .map_err(|error| LifecycleError(error.to_string()))?;
    }
    Ok(())
}
