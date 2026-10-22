import { PageContainer } from "@/components/layout/page-container";
import { SectionHeader } from "@/components/layout/section-header";
import { CommunityTrendLineChart } from "../community-trend-line-chart";
import { CommunityMessagesBarChart } from "./community-messages-bar-chart";
import { CommunityActiveUsersBarChart } from "./community-active-users-bar-chart";
import { CommunityNotificationSettingsCard } from "./community-notification-settings-card";
import { CommunityTopUsersBarChart } from "./community-top-users-bar-chart";
import { CommunitySummariesTable } from "./community-summaries-table";

type DailyStats = { averagePerDay: number; medianPerDay: number };

type CommunityPageProps = {
  community: {
    id: string;
    chatTitle: string;
    chatId: string;
    isActive: boolean;
    isPublic: boolean;
    summaryNotificationsEnabled: boolean;
    summaryNotificationTimeHours: 24 | 48 | null;
    summaryNotificationMessageCount: 500 | 1000 | null;
  };
  summariesPage: {
    rows: Array<{
      id: string;
      createdAt: string;
      oneliner: string;
      topics: Array<{ title: string; content: string; sources: string[] }>;
      likesCount: number;
      dislikesCount: number;
    }>;
    totalCount: number;
    currentPage: number;
    totalPages: number;
    hasPreviousPage: boolean;
    hasNextPage: boolean;
  };
  chartPoints: Array<{ date: string; summaries: number }>;
  messagesChartPoints: Array<{ date: string; messages: number }>;
  activeUsersChartPoints: Array<{ date: string; activeUsers: number }>;
  topUsersChartPoints: Array<{ userLabel: string; messageCount: number }>;
  totals30d: { summaries: number; messages: number };
  messagesStats30d: DailyStats;
  activeUsersStats30d: DailyStats;
};

export default function CommunityPage({
  community,
  summariesPage,
  chartPoints,
  messagesChartPoints,
  activeUsersChartPoints,
  topUsersChartPoints,
  totals30d,
  messagesStats30d,
  activeUsersStats30d,
}: CommunityPageProps) {
  return (
    <PageContainer>
      <SectionHeader
        title={community.chatTitle}
        breadcrumbs={[
          { label: "Community", href: "/" },
          { label: community.chatTitle },
        ]}
        subtitle={`Telegram ID: ${String(community.chatId)}`}
      />

      <div className="space-y-6">
        <CommunityNotificationSettingsCard
          communityId={community.id}
          settings={{
            isActive: community.isActive,
            isPublic: community.isPublic,
            summaryNotificationsEnabled: community.summaryNotificationsEnabled,
            summaryNotificationTimeHours: community.summaryNotificationTimeHours,
            summaryNotificationMessageCount: community.summaryNotificationMessageCount,
          }}
        />

        <CommunitySummariesTable
          summaries={summariesPage.rows}
          totalCount={summariesPage.totalCount}
          currentPage={summariesPage.currentPage}
          totalPages={summariesPage.totalPages}
          hasPreviousPage={summariesPage.hasPreviousPage}
          hasNextPage={summariesPage.hasNextPage}
          basePath={`/communities/${community.id}`}
        />

        <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
          <div className="min-w-0 h-full">
            <CommunityMessagesBarChart data={messagesChartPoints} stats={messagesStats30d} />
          </div>
          <div className="min-w-0 h-full">
            <CommunityActiveUsersBarChart
              data={activeUsersChartPoints}
              stats={activeUsersStats30d}
            />
          </div>
        </div>

        <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
          <div className="min-w-0 h-full">
            <CommunityTopUsersBarChart data={topUsersChartPoints} />
          </div>
          <div className="min-w-0 h-full">
            <CommunityTrendLineChart
              title="Activity trend"
              description="Total number of summaries created in the last 30 days for this community"
              primaryLabel="Summaries"
              secondaryLabel="Messages"
              dataKey="summaries"
              data={chartPoints}
              totals={{
                primary: totals30d.summaries,
                secondary: totals30d.messages,
              }}
              yAxisLabel="Cumulative summaries"
            />
          </div>
        </div>
      </div>
    </PageContainer>
  );
}
