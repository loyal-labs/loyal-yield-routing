import "server-only";

import {
  getYieldOptimizationClient,
  pushCampaignSends,
} from "@/lib/yield-optimization/yield-neon-client.server";

export async function claimPushCampaignSend(send: {
  campaign: string;
  cohortId?: string | null;
  walletAddress: string;
}): Promise<boolean> {
  const inserted = await getYieldOptimizationClient()
    .db.insert(pushCampaignSends)
    .values(send)
    .onConflictDoNothing()
    .returning({ id: pushCampaignSends.id });
  return inserted.length > 0;
}
