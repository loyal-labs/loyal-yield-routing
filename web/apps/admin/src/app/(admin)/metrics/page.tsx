import { Suspense, use } from "react";

import type { Pages } from "@/api.gen/pages";
import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";
import type { Deferred } from "@/lib/page-data";

import { EarnRebalanceLatency } from "./earn-rebalance-latency";
import { MetricsLatencySkeleton } from "./metrics-loading-skeleton";
import { MetricsDashboardProgressive } from "./metrics-progressive-page";

function MetricsLatencySection({ latency }: { latency: Deferred<Pages["/metrics"]>["data"] }) {
  const data = use(latency);

  return (
    <section
      data-progressive-section="metrics-latency"
      data-progressive-state="ready"
    >
      <EarnRebalanceLatency data={data} />
    </section>
  );
}

export default function MetricsPage({ data }: Deferred<Pages["/metrics"]>) {
  return (
    <PageContainer className="max-w-7xl">
      <SectionHeader
        title="Metrics"
        breadcrumbs={[{ label: "Metrics" }]}
        subtitle="Production p95 latency in two-hour UTC buckets over the last 7 days, busiest operations first."
      />
      <div className="space-y-10" data-progressive-page="/metrics">
        <Suspense
          fallback={
            <section
              data-progressive-section="metrics-latency"
              data-progressive-state="loading"
            >
              <MetricsLatencySkeleton />
            </section>
          }
        >
          <MetricsLatencySection latency={data} />
        </Suspense>
        <MetricsDashboardProgressive />
      </div>
    </PageContainer>
  );
}
