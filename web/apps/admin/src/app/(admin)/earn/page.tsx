import { Suspense, use } from "react";

import type { Pages } from "@/api.gen/pages";
import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";
import type { Deferred } from "@/lib/page-data";

import {
  EarnProgressiveClient,
  MonitoringSkeleton,
} from "./earn-progressive-client";

function EarnMonitoringSection({ data }: Deferred<Pages["/earn"]>) {
  const {
    monitoring,
    monitoringError,
    positionDirection,
    positionSort,
    selectedMint,
  } = use(data);

  return monitoring ? (
    <EarnProgressiveClient
      initialMonitoring={monitoring}
      key={`${selectedMint ?? "all"}:${positionSort ?? "normalized"}:${
        positionDirection ?? "desc"
      }`}
      positionDirection={positionDirection}
      positionSort={positionSort}
      selectedMint={selectedMint}
    />
  ) : (
    <EarnProgressiveClient
      initialMonitoringError={monitoringError}
      positionDirection={positionDirection}
      positionSort={positionSort}
      selectedMint={selectedMint}
    />
  );
}

export default function EarnPage({ data }: Deferred<Pages["/earn"]>) {
  return (
    <PageContainer>
      <SectionHeader
        breadcrumbs={[{ label: "Earn" }]}
        subtitle="Internal Yield Neon monitoring for Earn positions and autodeposit health"
        title="Earn"
      />
      <div data-progressive-page="/earn">
        <Suspense
          fallback={
            <section
              data-progressive-section="earn-monitoring"
              data-progressive-state="loading"
            >
              <MonitoringSkeleton />
            </section>
          }
        >
          <EarnMonitoringSection data={data} />
        </Suspense>
      </div>
    </PageContainer>
  );
}
