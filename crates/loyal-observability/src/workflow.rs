//! Privacy-safe workflow metrics and traces.

use std::time::Duration;

use opentelemetry::{
    metrics::{Counter, Histogram, Meter},
    trace::Status,
    KeyValue,
};
use tracing::{span::Entered, Span};
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::ObservabilityWalletAddress;

pub(crate) const WORKFLOW_TRACE_TARGET: &str = "loyal.observability.workflow";

/// A bounded workflow result suitable for trace and metric attributes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WorkflowOutcome {
    /// The operation completed successfully.
    Succeeded,
    /// The operation failed.
    Failed,
    /// The operation was intentionally skipped because no work was required.
    Skipped,
}

impl WorkflowOutcome {
    fn as_str(self) -> &'static str {
        match self {
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Skipped => "skipped",
        }
    }
}

/// Low-cardinality workflow metrics exported through the OTLP metrics pipeline.
///
/// Wallet addresses are intentionally excluded from metrics because they would
/// create a high-cardinality dimension. Use logs and traces for wallet-level
/// correlation.
#[derive(Clone, Default)]
pub struct WorkflowMetrics {
    executions: Option<Counter<u64>>,
    duration: Option<Histogram<f64>>,
}

impl WorkflowMetrics {
    pub(crate) fn new(meter: &Meter) -> Self {
        Self {
            executions: Some(
                meter
                    .u64_counter("loyal.workflow.executions")
                    .with_description("Number of completed Loyal workflow operations")
                    .with_unit("{execution}")
                    .build(),
            ),
            duration: Some(
                meter
                    .f64_histogram("loyal.workflow.duration")
                    .with_description("Duration of completed Loyal workflow operations")
                    .with_unit("s")
                    .build(),
            ),
        }
    }

    /// Records one completed workflow operation and its duration.
    ///
    /// `workflow` and `operation` must be stable names rather than runtime IDs.
    pub fn record_execution(
        &self,
        workflow: &'static str,
        operation: &'static str,
        outcome: WorkflowOutcome,
        duration: Duration,
    ) {
        let attributes = [
            KeyValue::new("loyal.workflow.name", workflow),
            KeyValue::new("loyal.workflow.operation", operation),
            KeyValue::new("loyal.workflow.outcome", outcome.as_str()),
        ];

        if let Some(executions) = &self.executions {
            executions.add(1, &attributes);
        }
        if let Some(duration_histogram) = &self.duration {
            duration_histogram.record(duration.as_secs_f64(), &attributes);
        }
    }
}

/// A privacy-safe span for one operation in a longer workflow.
///
/// Create child spans while the parent span is entered or use
/// `tracing::Instrument` with [`Self::span`] to propagate the parent through an
/// async future.
#[derive(Clone, Debug)]
pub struct WorkflowSpan {
    span: Span,
}

impl WorkflowSpan {
    /// Creates a span with stable workflow and operation attributes.
    ///
    /// The exported OpenTelemetry span name is `operation`. Use names such as
    /// `autodeposit.run` or `reconcile.compare_balances`.
    pub fn new(workflow: &'static str, operation: &'static str) -> Self {
        Self {
            span: tracing::info_span!(
                target: WORKFLOW_TRACE_TARGET,
                "loyal.workflow.operation",
                otel.name = operation,
                otel.kind = "internal",
                loyal.workflow.name = workflow,
                loyal.workflow.operation = operation,
                loyal.workflow.outcome = tracing::field::Empty,
                error.type = tracing::field::Empty,
                loyal.wallet.address = tracing::field::Empty,
            ),
        }
    }

    /// Attaches the raw wallet address to this span.
    pub fn wallet_address(self, wallet_address: &ObservabilityWalletAddress) -> Self {
        self.span
            .record("loyal.wallet.address", wallet_address.as_str());
        self
    }

    /// Returns the underlying `tracing` span for sync or async instrumentation.
    pub fn span(&self) -> &Span {
        &self.span
    }

    /// Enters this span for a synchronous scope.
    pub fn enter(&self) -> Entered<'_> {
        self.span.enter()
    }

    /// Returns the underlying span by value for `tracing::Instrument`.
    pub fn into_span(self) -> Span {
        self.span
    }

    /// Marks the span as successfully completed.
    pub fn succeeded(&self) {
        self.record_outcome(WorkflowOutcome::Succeeded);
        self.span.set_status(Status::Ok);
    }

    /// Marks the span as intentionally skipped because no work was required.
    pub fn skipped(&self) {
        self.record_outcome(WorkflowOutcome::Skipped);
        self.span.set_status(Status::Ok);
    }

    /// Marks the span as failed using a stable, non-user-specific error code.
    pub fn failed(&self, error_code: &'static str) {
        self.record_outcome(WorkflowOutcome::Failed);
        self.span.record("error.type", error_code);
        self.span.set_status(Status::error(error_code));
    }

    /// Marks the span from a result without recording or formatting its error.
    ///
    /// The stable `error_code` is used only when `result` is `Err`.
    pub fn finish_from_result<T, E>(
        &self,
        result: &Result<T, E>,
        error_code: &'static str,
    ) -> WorkflowOutcome {
        if result.is_ok() {
            self.succeeded();
            WorkflowOutcome::Succeeded
        } else {
            self.failed(error_code);
            WorkflowOutcome::Failed
        }
    }

    fn record_outcome(&self, outcome: WorkflowOutcome) {
        self.span.record("loyal.workflow.outcome", outcome.as_str());
    }
}
