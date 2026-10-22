import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";

import { DappList } from "./dapp-list";

type DappsPageProps = {
  dapps: Array<{
    id: string;
    origin: string;
    name: string;
    startUrl: string;
    category: string | null;
    displayOrder: number;
    isActive: boolean;
    createdAt: string;
    updatedAt: string;
  }>;
};

export default function DappsPage({ dapps }: DappsPageProps) {
  return (
    <PageContainer>
      <SectionHeader title="dApps" breadcrumbs={[{ label: "dApps" }]} />
      <DappList dapps={dapps} />
    </PageContainer>
  );
}
