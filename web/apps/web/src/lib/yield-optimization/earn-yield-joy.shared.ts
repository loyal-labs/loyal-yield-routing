export const EARN_JOY_MILESTONES_USD = [10, 100, 1000] as const;

export const EARN_JOY_ANNIVERSARIES = [
  { campaign: "yield-one-year", days: 365, months: 12 },
  { campaign: "yield-six-months", days: 183, months: 6 },
] as const;

export const FIRST_YIELD_CAMPAIGN = "yield-first";

export const EARN_JOY_MIN_REPORTABLE_USD = 0.01;

/** An anniversary push needs an amount worth showing. */
export const ANNIVERSARY_MIN_EARNED_USD = 1;

const DAY_MS = 24 * 60 * 60 * 1000;

export function earnDigestThresholdUsd(daysSinceLastPush: number): number {
  if (daysSinceLastPush >= 30) {
    return EARN_JOY_MIN_REPORTABLE_USD;
  }
  if (daysSinceLastPush >= 7) {
    return 0.1;
  }
  return 1;
}

export type EarnJoyState = {
  lastPushedEarnedUsd: number;
  lastPushedAt: Date | null;
  sentCampaigns: string[];
};

export type EarnJoyInput = {
  accountAgeDays: number;
  lifetimeEarnedUsd: number;
  now: Date;
  state: EarnJoyState | null;
};

export type EarnJoyPush = {
  amountUsd: number;
  campaign: string;
  months?: number;
  type: "anniversary" | "digest" | "first" | "milestone";
};

export type EarnJoyDecision =
  | { kind: "seed"; campaigns: string[] }
  | { kind: "push"; push: EarnJoyPush }
  | { kind: "none" };

export function qualifiedEarnJoyCampaigns(input: {
  accountAgeDays: number;
  lifetimeEarnedUsd: number;
}): string[] {
  const campaigns: string[] = [];
  if (input.lifetimeEarnedUsd >= EARN_JOY_MIN_REPORTABLE_USD) {
    campaigns.push(FIRST_YIELD_CAMPAIGN);
  }
  const milestone = [...EARN_JOY_MILESTONES_USD]
    .reverse()
    .find((value) => input.lifetimeEarnedUsd >= value);
  if (milestone !== undefined) {
    campaigns.push(`yield-total-${milestone}`);
  }
  if (input.lifetimeEarnedUsd >= ANNIVERSARY_MIN_EARNED_USD) {
    const anniversary = EARN_JOY_ANNIVERSARIES.find(
      (entry) => input.accountAgeDays >= entry.days
    );
    if (anniversary) {
      campaigns.push(anniversary.campaign);
    }
  }
  return campaigns;
}

/**
 * At most one push per wallet per run, in the order of how much the moment
 * means: a first yield beats a milestone, a milestone beats an anniversary,
 * and anything beats the routine digest. Three pushes in one morning is how a
 * channel gets muted.
 */
export function selectEarnJoyPush(input: EarnJoyInput): EarnJoyDecision {
  const qualified = qualifiedEarnJoyCampaigns(input);

  if (!input.state) {
    return { kind: "seed", campaigns: qualified };
  }

  const sent = new Set(input.state.sentCampaigns);
  const pending = qualified.filter((campaign) => !sent.has(campaign));

  if (pending.includes(FIRST_YIELD_CAMPAIGN)) {
    return {
      kind: "push",
      push: {
        amountUsd: input.lifetimeEarnedUsd,
        campaign: FIRST_YIELD_CAMPAIGN,
        type: "first",
      },
    };
  }

  const milestone = [...EARN_JOY_MILESTONES_USD]
    .reverse()
    .find((value) => pending.includes(`yield-total-${value}`));
  if (milestone !== undefined) {
    return {
      kind: "push",
      push: {
        amountUsd: milestone,
        campaign: `yield-total-${milestone}`,
        type: "milestone",
      },
    };
  }

  const anniversary = EARN_JOY_ANNIVERSARIES.find((entry) =>
    pending.includes(entry.campaign)
  );
  if (anniversary) {
    return {
      kind: "push",
      push: {
        amountUsd: input.lifetimeEarnedUsd,
        campaign: anniversary.campaign,
        months: anniversary.months,
        type: "anniversary",
      },
    };
  }

  const earnedSinceLastPush =
    input.lifetimeEarnedUsd - input.state.lastPushedEarnedUsd;
  const daysSinceLastPush = input.state.lastPushedAt
    ? (input.now.getTime() - input.state.lastPushedAt.getTime()) / DAY_MS
    : Number.POSITIVE_INFINITY;
  if (
    earnedSinceLastPush >= EARN_JOY_MIN_REPORTABLE_USD &&
    earnedSinceLastPush >= earnDigestThresholdUsd(daysSinceLastPush)
  ) {
    return {
      kind: "push",
      push: {
        amountUsd: earnedSinceLastPush,
        campaign: "yield-digest",
        type: "digest",
      },
    };
  }

  return { kind: "none" };
}
