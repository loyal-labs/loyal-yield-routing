//! Low-cardinality success metrics for the durable Earn rebalance pipeline.

use std::time::Duration;

#[cfg(test)]
use opentelemetry::metrics::Meter;

use crate::{WorkflowMetrics, WorkflowOutcome};

/// Stable workflow name shared by every Earn rebalance worker.
pub const EARN_REBALANCE_WORKFLOW: &str = "earn.rebalance";

/// A durable forward transition in the Earn rebalance pipeline.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EarnRebalanceStage {
    AtaObservationPersisted,
    OpportunityPublished,
    RouteRevalidated,
    RouteExecutionHandoffPersisted,
    RouteConfirmed,
    RouteReconciled,
}

impl EarnRebalanceStage {
    pub const ALL: [Self; 6] = [
        Self::AtaObservationPersisted,
        Self::OpportunityPublished,
        Self::RouteRevalidated,
        Self::RouteExecutionHandoffPersisted,
        Self::RouteConfirmed,
        Self::RouteReconciled,
    ];

    pub const fn operation(self) -> &'static str {
        match self {
            Self::AtaObservationPersisted => "ata.observation_persisted",
            Self::OpportunityPublished => "opportunity.published",
            Self::RouteRevalidated => "route.revalidated",
            Self::RouteExecutionHandoffPersisted => "route.execution_handoff_persisted",
            Self::RouteConfirmed => "route.confirmed",
            Self::RouteReconciled => "route.reconciled",
        }
    }
}

/// Typed facade which can only emit successful durable Earn transitions.
#[derive(Clone, Default)]
pub struct EarnRebalanceMetrics {
    pub(crate) workflow: WorkflowMetrics,
}

impl EarnRebalanceMetrics {
    #[cfg(test)]
    pub(crate) fn new(meter: &Meter) -> Self {
        Self {
            workflow: WorkflowMetrics::new(meter),
        }
    }

    /// Records exactly one durable stage success.
    pub fn record_success(&self, stage: EarnRebalanceStage, duration: Duration) {
        self.workflow.record_execution(
            EARN_REBALANCE_WORKFLOW,
            stage.operation(),
            WorkflowOutcome::Succeeded,
            duration,
        );
    }

    /// Records a known number of independently persisted successes.
    pub fn record_successes(&self, stage: EarnRebalanceStage, count: usize, duration: Duration) {
        for _ in 0..count {
            self.record_success(stage, duration);
        }
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use opentelemetry::metrics::MeterProvider as _;
    use opentelemetry_sdk::metrics::{
        data::{AggregatedMetrics, MetricData, ResourceMetrics},
        InMemoryMetricExporter, SdkMeterProvider,
    };

    use super::*;

    #[test]
    fn earn_rebalance_success_metrics_contract() {
        let exporter = InMemoryMetricExporter::default();
        let provider = SdkMeterProvider::builder()
            .with_periodic_exporter(exporter.clone())
            .build();
        let metrics = EarnRebalanceMetrics::new(&provider.meter("earn-rebalance-test"));

        for stage in EarnRebalanceStage::ALL {
            metrics.record_success(stage, Duration::from_millis(25));
        }
        provider.force_flush().expect("test metrics should flush");
        let exported = exporter
            .get_finished_metrics()
            .expect("test metrics should be readable");

        let mut counter_operations = BTreeSet::new();
        let mut histogram_operations = BTreeSet::new();
        for metric in exported
            .iter()
            .flat_map(ResourceMetrics::scope_metrics)
            .flat_map(|scope| scope.metrics())
        {
            match (metric.name(), metric.data()) {
                ("loyal.workflow.executions", AggregatedMetrics::U64(MetricData::Sum(sum))) => {
                    for point in sum.data_points() {
                        assert_eq!(point.value(), 1);
                        counter_operations
                            .insert(assert_bounded_success_attributes(point.attributes()));
                    }
                }
                (
                    "loyal.workflow.duration",
                    AggregatedMetrics::F64(MetricData::Histogram(histogram)),
                ) => {
                    for point in histogram.data_points() {
                        assert_eq!(point.count(), 1);
                        histogram_operations
                            .insert(assert_bounded_success_attributes(point.attributes()));
                    }
                }
                _ => {}
            }
        }

        let expected = EarnRebalanceStage::ALL
            .into_iter()
            .map(|stage| stage.operation().to_owned())
            .collect::<BTreeSet<_>>();
        assert_eq!(counter_operations, expected);
        assert_eq!(histogram_operations, expected);
        provider.shutdown().expect("test provider should shut down");
    }

    fn assert_bounded_success_attributes<'a>(
        attributes: impl Iterator<Item = &'a opentelemetry::KeyValue>,
    ) -> String {
        let attributes = attributes
            .map(|attribute| {
                (
                    attribute.key.as_str().to_owned(),
                    attribute.value.as_str().into_owned(),
                )
            })
            .collect::<Vec<_>>();
        assert_eq!(attributes.len(), 3);
        assert!(attributes.iter().any(|(key, value)| {
            key == "loyal.workflow.name" && value == EARN_REBALANCE_WORKFLOW
        }));
        assert!(attributes
            .iter()
            .any(|(key, value)| { key == "loyal.workflow.outcome" && value == "succeeded" }));
        attributes
            .into_iter()
            .find_map(|(key, value)| (key == "loyal.workflow.operation").then_some(value))
            .expect("operation attribute should exist")
    }
}
