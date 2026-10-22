import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";

import {
  AutodepositActivityChart,
  type SerializedAutodepositTimeSeriesRange,
} from "./autodeposit-activity-chart";
import {
  OptimizationVolumeChart,
  type SerializedOptimizationVolumePoint,
} from "./optimization-volume-chart";

export type OverviewPageProps = {
  autodeposit: SerializedAutodepositTimeSeriesRange[];
  optimizationVolume: SerializedOptimizationVolumePoint[];
};

export default function OverviewPage({
  autodeposit,
  optimizationVolume,
}: OverviewPageProps) {
  return (
    <PageContainer>
      <SectionHeader title="Overview" breadcrumbs={[{ label: "Overview" }]} />
      <div className="mx-auto grid w-full max-w-4xl gap-6">
        <OptimizationVolumeChart data={optimizationVolume} />
        <AutodepositActivityChart data={autodeposit} />
      </div>
    </PageContainer>
  );
}
