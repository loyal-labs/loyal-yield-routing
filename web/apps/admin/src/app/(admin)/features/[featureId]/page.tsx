import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";

import type { FeatureFlagView, FeatureView } from "../feature-types";
import { FeatureDetail } from "./feature-detail";

export default function FeatureDetailPage({
  feature,
  availableFlags,
}: {
  feature: FeatureView;
  availableFlags: FeatureFlagView[];
}) {
  return (
    <PageContainer>
      <SectionHeader
        title={feature.title}
        breadcrumbs={[
          { label: "Features", href: "/features" },
          { label: feature.title },
        ]}
        subtitle={`Feature key: ${feature.key}`}
      />

      <FeatureDetail feature={feature} availableFlags={availableFlags} />
    </PageContainer>
  );
}
