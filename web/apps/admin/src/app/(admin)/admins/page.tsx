import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";

import { AdminList } from "./admin-list";

type AdminsPageProps = {
  admins: Array<{
    id: string;
    telegramId: string;
    username: string | null;
    displayName: string;
    addedAt: string;
    addedBy: string | null;
    notes: string | null;
  }>;
};

export default function AdminsPage({ admins }: AdminsPageProps) {
  return (
    <PageContainer>
      <SectionHeader title="Admins" breadcrumbs={[{ label: "Admins" }]} />
      <AdminList admins={admins} />
    </PageContainer>
  );
}
