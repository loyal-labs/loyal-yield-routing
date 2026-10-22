import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";
import type { FlagAudience, FlagTargetEnvironment } from "@loyal-labs/db-core/schema";

import { FlagForm } from "./flag-form";
import { FlagList } from "./flag-list";

type FlagsPageProps = {
  flags: Array<{
    id: string;
    key: string;
    description: string;
    enabled: boolean;
    audience: FlagAudience;
    targetEnvironments: FlagTargetEnvironment[];
    notes: string | null;
    linkedFeatures: Array<{
      linkId: string;
      id: string;
      title: string;
      key: string;
    }>;
  }>;
  availableFeatures: Array<{
    id: string;
    title: string;
    key: string;
  }>;
};

export default function FlagsPage({ flags, availableFeatures }: FlagsPageProps) {
  return (
    <PageContainer className="space-y-6">
      <SectionHeader
        title="Flags"
        breadcrumbs={[{ label: "Flags" }]}
        subtitle="Manage frontend runtime flags for the Loyal web app."
      />

      <FlagForm />
      <FlagList flags={flags} availableFeatures={availableFeatures} />
    </PageContainer>
  );
}
