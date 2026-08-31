use loyal_observability::OperationalError;
use loyal_yield_store::EarnReconciliationHealthSnapshot;
use opentelemetry::{
    metrics::{Gauge, Meter},
    KeyValue,
};

pub const EARN_RECONCILIATION_JOB_FAILED: &str = "earn_reconciliation_job_failed";
pub const EARN_RECONCILIATION_CONSUMER_FAILED: &str = "earn_reconciliation_consumer_failed";
pub const EARN_RECONCILIATION_HEALTH_SNAPSHOT_FAILED: &str =
    "earn_reconciliation_health_snapshot_failed";
pub const AUTODEPOSIT_RECONCILIATION_REQUEST_FAILED: &str =
    "autodeposit_reconciliation_request_failed";
pub const AUTODEPOSIT_RECONCILIATION_RPC_BEHIND: &str = "autodeposit_reconciliation_rpc_behind";
pub const AUTODEPOSIT_RECONCILIATION_CONSUMER_FAILED: &str =
    "autodeposit_reconciliation_consumer_failed";

/// Service-owned, low-cardinality gauges backed by the shared OTLP transport.
#[derive(Clone)]
pub struct EarnMonitorMetrics {
    cursor_slot: Gauge<u64>,
    stream_frontier_slot: Gauge<u64>,
    pending_jobs: Gauge<u64>,
    failed_pending_jobs: Gauge<u64>,
    oldest_pending_age_seconds: Gauge<u64>,
    attributes: [KeyValue; 2],
}

impl EarnMonitorMetrics {
    pub fn new(meter: &Meter, consumer: &'static str, cluster: impl Into<String>) -> Self {
        Self {
            cursor_slot: meter
                .u64_gauge("loyal.laserstream.cursor.slot")
                .with_description("Latest watched event committed to the LaserStream replay cursor")
                .with_unit("{slot}")
                .build(),
            stream_frontier_slot: meter
                .u64_gauge("loyal.laserstream.stream_frontier.slot")
                .with_description(
                    "Latest confirmed slot-progress update processed from LaserStream",
                )
                .with_unit("{slot}")
                .build(),
            pending_jobs: meter
                .u64_gauge("loyal.earn.reconciliation.pending")
                .with_description("Durable Earn reconciliation jobs awaiting completion")
                .with_unit("{job}")
                .build(),
            failed_pending_jobs: meter
                .u64_gauge("loyal.earn.reconciliation.failed_pending")
                .with_description("Pending Earn reconciliation jobs with a recorded error")
                .with_unit("{job}")
                .build(),
            oldest_pending_age_seconds: meter
                .u64_gauge("loyal.earn.reconciliation.oldest_pending_age")
                .with_description("Age of the oldest pending Earn reconciliation job")
                .with_unit("s")
                .build(),
            attributes: [
                KeyValue::new("loyal.laserstream.consumer", consumer),
                KeyValue::new("solana.cluster", cluster.into()),
            ],
        }
    }

    pub fn record(&self, snapshot: &EarnReconciliationHealthSnapshot) {
        self.cursor_slot
            .record(snapshot.cursor_slot, &self.attributes);
        self.pending_jobs
            .record(snapshot.pending_jobs, &self.attributes);
        self.failed_pending_jobs
            .record(snapshot.failed_pending_jobs, &self.attributes);
        self.oldest_pending_age_seconds
            .record(snapshot.oldest_pending_age_seconds, &self.attributes);
    }

    pub fn record_stream_frontier(&self, slot: u64) {
        self.stream_frontier_slot.record(slot, &self.attributes);
    }
}

pub fn emit_earn_reconciliation_job_failed() {
    OperationalError::new(
        EARN_RECONCILIATION_JOB_FAILED,
        "process_earn_reconciliation_job",
        "Earn reconciliation job failed and was retained for retry",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();
}

pub fn emit_earn_reconciliation_consumer_failed() {
    OperationalError::new(
        EARN_RECONCILIATION_CONSUMER_FAILED,
        "run_earn_reconciliation_consumer",
        "Durable Earn reconciliation consumer failed",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();
}

pub fn emit_autodeposit_reconciliation_request_failed() {
    OperationalError::new(
        AUTODEPOSIT_RECONCILIATION_REQUEST_FAILED,
        "process_autodeposit_reconciliation_request",
        "Autodeposit reconciliation request failed and was retained for retry",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();
}

pub fn emit_autodeposit_reconciliation_rpc_behind() {
    OperationalError::new(
        AUTODEPOSIT_RECONCILIATION_RPC_BEHIND,
        "process_autodeposit_reconciliation_request",
        "Autodeposit reconciliation RPC stayed behind beyond the retry horizon",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();
}

pub fn emit_autodeposit_reconciliation_consumer_failed() {
    OperationalError::new(
        AUTODEPOSIT_RECONCILIATION_CONSUMER_FAILED,
        "run_autodeposit_reconciliation_consumer",
        "Autodeposit reconciliation consumer failed",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();
}

pub fn emit_earn_reconciliation_health_snapshot_failed() {
    OperationalError::new(
        EARN_RECONCILIATION_HEALTH_SNAPSHOT_FAILED,
        "load_earn_reconciliation_health_snapshot",
        "Earn reconciliation health snapshot could not be loaded",
    )
    .retryable(true)
    .recovery_required(false)
    .emit();
}

#[cfg(test)]
mod tests {
    use std::{
        collections::BTreeMap,
        time::{Duration, Instant},
    };

    use opentelemetry::metrics::MeterProvider as _;
    use opentelemetry_sdk::metrics::{
        data::{AggregatedMetrics, MetricData, ResourceMetrics},
        InMemoryMetricExporter, SdkMeterProvider,
    };

    use super::*;

    #[test]
    fn monitor_observability_records_authoritative_gauges_with_bounded_attributes() {
        let exporter = InMemoryMetricExporter::default();
        let provider = SdkMeterProvider::builder()
            .with_periodic_exporter(exporter.clone())
            .build();
        let meter = provider.meter("ask-2200-monitor-observability-test");
        let metrics = EarnMonitorMetrics::new(&meter, "earn-smart-account", "mainnet");
        metrics.record(&EarnReconciliationHealthSnapshot {
            cursor_slot: 440_700_000,
            pending_jobs: 3,
            failed_pending_jobs: 1,
            oldest_pending_age_seconds: 120,
        });
        metrics.record_stream_frontier(440_700_032);

        provider.force_flush().expect("metrics should flush");
        let exported = exporter
            .get_finished_metrics()
            .expect("metrics should be readable");
        provider.shutdown().expect("provider should shut down");

        let expected = BTreeMap::from([
            ("loyal.earn.reconciliation.failed_pending", 1),
            ("loyal.earn.reconciliation.oldest_pending_age", 120),
            ("loyal.earn.reconciliation.pending", 3),
            ("loyal.laserstream.cursor.slot", 440_700_000),
            ("loyal.laserstream.stream_frontier.slot", 440_700_032),
        ]);
        let actual = gauge_values(&exported);
        assert_eq!(actual, expected);
    }

    #[test]
    fn monitor_observability_error_codes_are_stable() {
        assert_eq!(
            EARN_RECONCILIATION_JOB_FAILED,
            "earn_reconciliation_job_failed"
        );
        assert_eq!(
            EARN_RECONCILIATION_CONSUMER_FAILED,
            "earn_reconciliation_consumer_failed"
        );
        assert_eq!(
            AUTODEPOSIT_RECONCILIATION_REQUEST_FAILED,
            "autodeposit_reconciliation_request_failed"
        );
        assert_eq!(
            AUTODEPOSIT_RECONCILIATION_RPC_BEHIND,
            "autodeposit_reconciliation_rpc_behind"
        );
        assert_eq!(
            AUTODEPOSIT_RECONCILIATION_CONSUMER_FAILED,
            "autodeposit_reconciliation_consumer_failed"
        );
    }

    #[test]
    fn local_metrics_and_error_emission_stay_non_blocking() {
        const METRIC_RECORDINGS: usize = 10_000;
        const ERROR_EMISSIONS: usize = 6_000;
        const LOCAL_OVERHEAD_BUDGET: Duration = Duration::from_secs(1);

        let exporter = InMemoryMetricExporter::default();
        let provider = SdkMeterProvider::builder()
            .with_periodic_exporter(exporter)
            .build();
        let meter = provider.meter("ask-2200-monitor-observability-performance-test");
        let metrics = EarnMonitorMetrics::new(&meter, "earn-smart-account", "mainnet");
        let snapshot = EarnReconciliationHealthSnapshot {
            cursor_slot: 440_700_000,
            pending_jobs: 3,
            failed_pending_jobs: 1,
            oldest_pending_age_seconds: 120,
        };

        let metric_started_at = Instant::now();
        for _ in 0..METRIC_RECORDINGS {
            metrics.record(&snapshot);
        }
        let metric_elapsed = metric_started_at.elapsed();
        assert!(
            metric_elapsed <= LOCAL_OVERHEAD_BUDGET,
            "{METRIC_RECORDINGS} four-gauge recordings took {metric_elapsed:?}"
        );

        let error_started_at = Instant::now();
        for _ in 0..(ERROR_EMISSIONS / 6) {
            emit_earn_reconciliation_job_failed();
            emit_earn_reconciliation_consumer_failed();
            emit_earn_reconciliation_health_snapshot_failed();
            emit_autodeposit_reconciliation_request_failed();
            emit_autodeposit_reconciliation_rpc_behind();
            emit_autodeposit_reconciliation_consumer_failed();
        }
        let error_elapsed = error_started_at.elapsed();
        assert!(
            error_elapsed <= LOCAL_OVERHEAD_BUDGET,
            "{ERROR_EMISSIONS} operational-error emissions took {error_elapsed:?}"
        );
        eprintln!(
            "ASK-2200 local telemetry overhead: metric_recordings={METRIC_RECORDINGS}, metric_ms={:.3}, error_emissions={ERROR_EMISSIONS}, error_ms={:.3}",
            metric_elapsed.as_secs_f64() * 1000.0,
            error_elapsed.as_secs_f64() * 1000.0,
        );

        provider.shutdown().expect("provider should shut down");
    }

    fn gauge_values(metrics: &[ResourceMetrics]) -> BTreeMap<&str, u64> {
        metrics
            .iter()
            .flat_map(ResourceMetrics::scope_metrics)
            .flat_map(|scope| scope.metrics())
            .map(|metric| {
                let AggregatedMetrics::U64(MetricData::Gauge(gauge)) = metric.data() else {
                    panic!("{} is not a u64 gauge", metric.name());
                };
                let point = gauge
                    .data_points()
                    .next()
                    .unwrap_or_else(|| panic!("{} has no data point", metric.name()));
                let attributes = point
                    .attributes()
                    .map(|attribute| attribute.key.as_str())
                    .collect::<Vec<_>>();
                assert_eq!(
                    attributes,
                    ["loyal.laserstream.consumer", "solana.cluster"],
                    "{} has an unbounded or unexpected attribute",
                    metric.name()
                );
                (metric.name(), point.value())
            })
            .collect()
    }
}
