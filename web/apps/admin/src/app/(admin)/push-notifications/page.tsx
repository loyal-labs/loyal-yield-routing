import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";

import { ManualPushPanel } from "./manual-push-panel";

type PushNotificationsPageProps = {
  counts: {
    all: number;
    android: number;
    ios: number;
  };
  actionMessage: { kind: "success" | "error"; message: string } | null;
  recentSends: Array<{
    id: string;
    source: string;
    audience: string;
    platform: string | null;
    title: string;
    status: string;
    requestedCount: number;
    ticketCount: number;
    receiptOkCount: number;
    receiptErrorCount: number;
    deviceNotRegisteredCount: number;
    sentAt: string | null;
    receiptsCheckedAt: string | null;
    createdAt: string;
    createdBy: string | null;
    receiptIdCount: number;
    lastTicketError: string | null;
  }>;
};

export default function PushNotificationsPage({
  counts,
  actionMessage,
  recentSends,
}: PushNotificationsPageProps) {
  return (
    <PageContainer>
      <SectionHeader
        title="Push notifications"
        breadcrumbs={[{ label: "Push notifications" }]}
        subtitle="Manual mobile broadcasts and Expo delivery cleanup."
      />
      <ManualPushPanel
        counts={counts}
        actionMessage={actionMessage}
        recentSends={recentSends}
      />
    </PageContainer>
  );
}
