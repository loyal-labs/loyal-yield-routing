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

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WorkflowOutcome {
    Succeeded,
    Failed,
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

#[derive(Clone, Debug)]
pub struct WorkflowSpan {
    span: Span,
}

impl WorkflowSpan {
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

    pub fn wallet_address(self, wallet_address: &ObservabilityWalletAddress) -> Self {
        self.span
            .record("loyal.wallet.address", wallet_address.as_str());
        self
    }

    pub fn span(&self) -> &Span {
        &self.span
    }

    pub fn enter(&self) -> Entered<'_> {
        self.span.enter()
    }

    pub fn into_span(self) -> Span {
        self.span
    }

    pub fn succeeded(&self) {
        self.record_outcome(WorkflowOutcome::Succeeded);
        self.span.set_status(Status::Ok);
    }

    pub fn skipped(&self) {
        self.record_outcome(WorkflowOutcome::Skipped);
        self.span.set_status(Status::Ok);
    }

    pub fn failed(&self, error_code: &'static str) {
        self.record_outcome(WorkflowOutcome::Failed);
        self.span.record("error.type", error_code);
        self.span.set_status(Status::error(error_code));
    }

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
