//! Privacy-safe metrics derived from SQLx's existing tracing events.

use std::time::Duration;

use opentelemetry::{
    metrics::{Histogram, Meter},
    KeyValue,
};
use tracing::{
    field::{Field, Visit},
    Event, Level, Metadata, Subscriber,
};
use tracing_subscriber::{layer::Context, Layer};

const SQLX_QUERY_TARGET: &str = "sqlx::query";
const SQLX_POOL_ACQUIRE_TARGET: &str = "sqlx::pool::acquire";
const DATABASE_QUERY_METRICS_TARGET: &str = "loyal.observability.database_query";
const DATABASE_SYSTEM_NAME: &str = "postgresql";
const DATABASE_OPERATION_NAME: &str = "OTHER";
const DATABASE_DURATION_BOUNDARIES_SECONDS: [f64; 9] =
    [0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0, 10.0];

/// Database queries that may be exported as bounded metric attributes.
///
/// Adding a variant is an explicit privacy and cardinality review point. Runtime
/// SQL, parameters, and caller-provided names cannot enter the metric API.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u64)]
pub enum DatabaseQuery {
    FleetOpportunityPlannerLoadSources = 1,
    FleetOpportunityPlannerLoadSourcesWithoutQueue = 2,
}

impl DatabaseQuery {
    fn from_id(id: u64) -> Option<Self> {
        match id {
            1 => Some(Self::FleetOpportunityPlannerLoadSources),
            2 => Some(Self::FleetOpportunityPlannerLoadSourcesWithoutQueue),
            _ => None,
        }
    }

    fn metric_name(self) -> &'static str {
        match self {
            Self::FleetOpportunityPlannerLoadSources => "fleet_opportunity_planner.load_sources",
            Self::FleetOpportunityPlannerLoadSourcesWithoutQueue => {
                "fleet_opportunity_planner.load_sources_without_queue"
            }
        }
    }
}

/// A bounded phase of a named database query.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u64)]
pub enum DatabaseQueryPhase {
    Fetch = 1,
    Decode = 2,
    Total = 3,
}

impl DatabaseQueryPhase {
    fn from_id(id: u64) -> Option<Self> {
        match id {
            1 => Some(Self::Fetch),
            2 => Some(Self::Decode),
            3 => Some(Self::Total),
            _ => None,
        }
    }

    fn metric_name(self) -> &'static str {
        match self {
            Self::Fetch => "fetch",
            Self::Decode => "decode",
            Self::Total => "total",
        }
    }
}

/// Records a phase of a known database query without accepting dynamic labels.
pub fn record_database_query_phase_duration(
    query: DatabaseQuery,
    phase: DatabaseQueryPhase,
    duration: Duration,
) {
    tracing::event!(
        target: DATABASE_QUERY_METRICS_TARGET,
        Level::DEBUG,
        query_id = query as u64,
        phase_id = phase as u64,
        elapsed_secs = duration.as_secs_f64(),
    );
}

#[derive(Clone, Default)]
pub(crate) struct SqlxMetrics {
    operation_duration: Option<Histogram<f64>>,
    connection_wait_time: Option<Histogram<f64>>,
    named_query_phase_duration: Option<Histogram<f64>>,
}

impl SqlxMetrics {
    pub(crate) fn new(meter: &Meter) -> Self {
        Self {
            operation_duration: Some(
                meter
                    .f64_histogram("db.client.operation.duration")
                    .with_description("Duration of database client operations")
                    .with_unit("s")
                    .with_boundaries(DATABASE_DURATION_BOUNDARIES_SECONDS.to_vec())
                    .build(),
            ),
            connection_wait_time: Some(
                meter
                    .f64_histogram("db.client.connection.wait_time")
                    .with_description("Time spent waiting for a database connection")
                    .with_unit("s")
                    .with_boundaries(DATABASE_DURATION_BOUNDARIES_SECONDS.to_vec())
                    .build(),
            ),
            named_query_phase_duration: Some(
                meter
                    .f64_histogram("loyal.db.query.phase.duration")
                    .with_description("Duration of a bounded phase of a named database query")
                    .with_unit("s")
                    .with_boundaries(DATABASE_DURATION_BOUNDARIES_SECONDS.to_vec())
                    .build(),
            ),
        }
    }

    fn record_operation(&self, duration_seconds: f64) {
        if let Some(histogram) = &self.operation_duration {
            histogram.record(
                duration_seconds,
                &[
                    KeyValue::new("db.system.name", DATABASE_SYSTEM_NAME),
                    // SQLx exposes operation details only inside its query
                    // payload. Keep the standard attribute bounded without
                    // inspecting that payload.
                    KeyValue::new("db.operation.name", DATABASE_OPERATION_NAME),
                ],
            );
        }
    }

    fn record_connection_wait(&self, duration_seconds: f64) {
        if let Some(histogram) = &self.connection_wait_time {
            histogram.record(
                duration_seconds,
                &[KeyValue::new("db.system.name", DATABASE_SYSTEM_NAME)],
            );
        }
    }

    fn record_named_query_phase(
        &self,
        query: DatabaseQuery,
        phase: DatabaseQueryPhase,
        duration_seconds: f64,
    ) {
        if let Some(histogram) = &self.named_query_phase_duration {
            histogram.record(
                duration_seconds,
                &[
                    KeyValue::new("db.system.name", DATABASE_SYSTEM_NAME),
                    KeyValue::new("loyal.db.query.name", query.metric_name()),
                    KeyValue::new("loyal.db.query.phase", phase.metric_name()),
                ],
            );
        }
    }
}

pub(crate) struct SqlxMetricsLayer {
    metrics: SqlxMetrics,
}

impl SqlxMetricsLayer {
    pub(crate) fn new(metrics: SqlxMetrics) -> Self {
        Self { metrics }
    }
}

impl<S> Layer<S> for SqlxMetricsLayer
where
    S: Subscriber,
{
    fn on_event(&self, event: &Event<'_>, _context: Context<'_, S>) {
        let target = event.metadata().target();
        if target != SQLX_QUERY_TARGET
            && target != SQLX_POOL_ACQUIRE_TARGET
            && target != DATABASE_QUERY_METRICS_TARGET
        {
            return;
        }

        let mut fields = SqlxEventFields::default();
        event.record(&mut fields);

        match target {
            SQLX_QUERY_TARGET => {
                if let Some(duration_seconds) = valid_duration(fields.operation_duration_seconds) {
                    self.metrics.record_operation(duration_seconds);
                }
            }
            SQLX_POOL_ACQUIRE_TARGET => {
                if let Some(duration_seconds) = valid_duration(fields.connection_wait_seconds) {
                    self.metrics.record_connection_wait(duration_seconds);
                }
            }
            DATABASE_QUERY_METRICS_TARGET => {
                if let (Some(query), Some(phase), Some(duration_seconds)) = (
                    fields.query_id.and_then(DatabaseQuery::from_id),
                    fields.phase_id.and_then(DatabaseQueryPhase::from_id),
                    valid_duration(fields.operation_duration_seconds),
                ) {
                    self.metrics
                        .record_named_query_phase(query, phase, duration_seconds);
                }
            }
            _ => {}
        }
    }
}

pub(crate) fn is_database_metrics_event(metadata: &Metadata<'_>) -> bool {
    matches!(
        metadata.target(),
        SQLX_QUERY_TARGET | SQLX_POOL_ACQUIRE_TARGET | DATABASE_QUERY_METRICS_TARGET
    )
}

fn valid_duration(duration: Option<f64>) -> Option<f64> {
    duration.filter(|value| value.is_finite() && *value >= 0.0)
}

#[derive(Default)]
struct SqlxEventFields {
    operation_duration_seconds: Option<f64>,
    connection_wait_seconds: Option<f64>,
    query_id: Option<u64>,
    phase_id: Option<u64>,
}

impl Visit for SqlxEventFields {
    fn record_f64(&mut self, field: &Field, value: f64) {
        match field.name() {
            "elapsed_secs" => self.operation_duration_seconds = Some(value),
            // SQLx 0.8.6 emits the first spelling. Accept the corrected spelling
            // as well so an upstream typo fix does not silently drop metrics.
            "aquired_after_secs" | "acquired_after_secs" => {
                self.connection_wait_seconds = Some(value);
            }
            _ => {}
        }
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        match field.name() {
            "query_id" => self.query_id = Some(value),
            "phase_id" => self.phase_id = Some(value),
            _ => {}
        }
    }

    fn record_debug(&mut self, _field: &Field, _value: &dyn std::fmt::Debug) {
        // Intentionally ignore every debug-formatted field. In particular,
        // SQLx carries query summaries and statements in its event payload.
    }
}

#[cfg(test)]
mod tests {
    use opentelemetry::metrics::MeterProvider as _;
    use opentelemetry_sdk::metrics::{
        data::{AggregatedMetrics, HistogramDataPoint, MetricData, ResourceMetrics},
        InMemoryMetricExporter, SdkMeterProvider,
    };
    use tracing::Level;
    use tracing_subscriber::{filter, layer::SubscriberExt, registry, EnvFilter, Layer as _};

    use super::*;

    fn collect_metrics(emit: impl FnOnce()) -> Vec<ResourceMetrics> {
        let exporter = InMemoryMetricExporter::default();
        let provider = SdkMeterProvider::builder()
            .with_periodic_exporter(exporter.clone())
            .build();
        let meter = provider.meter("loyal-observability-sqlx-metrics-test");
        let stdout_layer = tracing_subscriber::fmt::layer()
            .with_writer(std::io::sink)
            .with_filter(EnvFilter::new("warn"));
        let layer = SqlxMetricsLayer::new(SqlxMetrics::new(&meter))
            .with_filter(filter::filter_fn(is_database_metrics_event));
        let subscriber = registry().with(stdout_layer).with(layer);

        tracing::subscriber::with_default(subscriber, emit);
        provider.force_flush().expect("test metrics should flush");
        let metrics = exporter
            .get_finished_metrics()
            .expect("test metrics should be readable");
        provider.shutdown().expect("test provider should shut down");
        metrics
    }

    fn histogram_points<'a>(
        metrics: &'a [ResourceMetrics],
        metric_name: &str,
    ) -> Vec<&'a HistogramDataPoint<f64>> {
        metrics
            .iter()
            .flat_map(ResourceMetrics::scope_metrics)
            .flat_map(|scope| scope.metrics())
            .filter(|metric| metric.name() == metric_name)
            .flat_map(|metric| match metric.data() {
                AggregatedMetrics::F64(MetricData::Histogram(histogram)) => {
                    histogram.data_points().collect::<Vec<_>>()
                }
                _ => Vec::new(),
            })
            .collect()
    }

    fn attribute<'a>(point: &'a HistogramDataPoint<f64>, key: &str) -> Option<String> {
        point
            .attributes()
            .find(|attribute| attribute.key.as_str() == key)
            .map(|attribute| attribute.value.as_str().into_owned())
    }

    #[test]
    fn sqlx_metrics_query_event_records_standard_histogram() {
        let metrics = collect_metrics(|| {
            tracing::event!(
                target: SQLX_QUERY_TARGET,
                Level::DEBUG,
                message = "SELECT accounts FROM ledger",
                elapsed_secs = 0.025_f64,
            );
        });
        let points = histogram_points(&metrics, "db.client.operation.duration");

        assert_eq!(points.len(), 1);
        assert_eq!(points[0].count(), 1);
        assert_eq!(points[0].sum(), 0.025);
        assert_eq!(
            points[0].bounds().collect::<Vec<_>>(),
            DATABASE_DURATION_BOUNDARIES_SECONDS
        );
        assert_eq!(
            attribute(points[0], "db.system.name").as_deref(),
            Some("postgresql")
        );
        assert_eq!(
            attribute(points[0], "db.operation.name").as_deref(),
            Some("OTHER")
        );
    }

    #[test]
    fn sqlx_metrics_slow_query_event_is_captured() {
        let metrics = collect_metrics(|| {
            tracing::event!(
                target: SQLX_QUERY_TARGET,
                Level::WARN,
                message = "UPDATE accounts SET balance",
                elapsed_secs = 1.25_f64,
                slow_threshold_seconds = 1.0_f64,
            );
        });
        let points = histogram_points(&metrics, "db.client.operation.duration");

        assert_eq!(points.len(), 1);
        assert_eq!(points[0].sum(), 1.25);
        assert_eq!(
            attribute(points[0], "db.operation.name").as_deref(),
            Some("OTHER")
        );
    }

    #[test]
    fn sqlx_metrics_pool_acquire_event_records_wait_histogram() {
        let metrics = collect_metrics(|| {
            tracing::event!(
                target: SQLX_POOL_ACQUIRE_TARGET,
                Level::DEBUG,
                message = "acquired connection",
                aquired_after_secs = 0.075_f64,
            );
        });
        let points = histogram_points(&metrics, "db.client.connection.wait_time");

        assert_eq!(points.len(), 1);
        assert_eq!(points[0].sum(), 0.075);
        assert_eq!(
            attribute(points[0], "db.system.name").as_deref(),
            Some("postgresql")
        );
        assert_eq!(points[0].attributes().count(), 1);
    }

    #[test]
    fn named_query_phase_records_only_bounded_labels() {
        let metrics = collect_metrics(|| {
            record_database_query_phase_duration(
                DatabaseQuery::FleetOpportunityPlannerLoadSources,
                DatabaseQueryPhase::Fetch,
                Duration::from_millis(100),
            );
            record_database_query_phase_duration(
                DatabaseQuery::FleetOpportunityPlannerLoadSources,
                DatabaseQueryPhase::Decode,
                Duration::from_millis(125),
            );
            record_database_query_phase_duration(
                DatabaseQuery::FleetOpportunityPlannerLoadSources,
                DatabaseQueryPhase::Total,
                Duration::from_millis(150),
            );
        });
        let points = histogram_points(&metrics, "loyal.db.query.phase.duration");
        let decode = points
            .iter()
            .find(|point| attribute(point, "loyal.db.query.phase").as_deref() == Some("decode"))
            .expect("decode phase should be present");

        assert_eq!(points.len(), 3);
        assert_eq!(decode.count(), 1);
        assert_eq!(decode.sum(), 0.125);
        assert_eq!(
            attribute(decode, "loyal.db.query.name").as_deref(),
            Some("fleet_opportunity_planner.load_sources")
        );
        assert_eq!(
            attribute(decode, "loyal.db.query.phase").as_deref(),
            Some("decode")
        );
        assert_eq!(decode.attributes().count(), 3);
    }

    #[test]
    fn sqlx_metrics_ignores_unrelated_and_malformed_events() {
        let metrics = collect_metrics(|| {
            tracing::event!(
                target: "loyal.unrelated",
                Level::DEBUG,
                message = "SELECT should not be observed",
                elapsed_secs = 99.0_f64,
            );
            tracing::event!(
                target: SQLX_QUERY_TARGET,
                Level::DEBUG,
                message = "SELECT missing duration",
            );
            tracing::event!(
                target: SQLX_POOL_ACQUIRE_TARGET,
                Level::DEBUG,
                message = "negative duration",
                aquired_after_secs = -1.0_f64,
            );
            tracing::event!(
                target: DATABASE_QUERY_METRICS_TARGET,
                Level::DEBUG,
                query_id = 999_u64,
                phase_id = DatabaseQueryPhase::Total as u64,
                elapsed_secs = 1.0_f64,
            );
        });

        assert!(histogram_points(&metrics, "db.client.operation.duration").is_empty());
        assert!(histogram_points(&metrics, "db.client.connection.wait_time").is_empty());
        assert!(histogram_points(&metrics, "loyal.db.query.phase.duration").is_empty());
    }

    #[test]
    fn sqlx_metrics_bounds_operations_and_never_exports_event_payloads() {
        struct PanicOnDebug;

        impl std::fmt::Debug for PanicOnDebug {
            fn fmt(&self, _formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                panic!("SQLx event payload must not be formatted or inspected")
            }
        }

        const FORBIDDEN: &str = "super_secret_customer_table";
        let metrics = collect_metrics(|| {
            tracing::event!(
                target: SQLX_QUERY_TARGET,
                Level::DEBUG,
                message = ?PanicOnDebug,
                elapsed_secs = 0.5_f64,
                sensitive_payload = FORBIDDEN,
            );
        });
        let points = histogram_points(&metrics, "db.client.operation.duration");

        assert_eq!(points.len(), 1);
        assert_eq!(
            attribute(points[0], "db.operation.name").as_deref(),
            Some("OTHER")
        );
        assert_eq!(points[0].attributes().count(), 2);
        assert!(points[0].attributes().all(|attribute| {
            !attribute.key.as_str().contains(FORBIDDEN)
                && !attribute.value.as_str().contains(FORBIDDEN)
        }));
    }

    #[test]
    fn sqlx_metrics_rejects_non_finite_durations() {
        let metrics = collect_metrics(|| {
            tracing::event!(
                target: SQLX_QUERY_TARGET,
                Level::DEBUG,
                elapsed_secs = f64::NAN,
            );
            tracing::event!(
                target: SQLX_POOL_ACQUIRE_TARGET,
                Level::DEBUG,
                aquired_after_secs = f64::INFINITY,
            );
        });

        assert!(histogram_points(&metrics, "db.client.operation.duration").is_empty());
        assert!(histogram_points(&metrics, "db.client.connection.wait_time").is_empty());
    }
}
