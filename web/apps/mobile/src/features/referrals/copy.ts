import type { ReferralClaim } from "@loyal-labs/api";

export const REFERRAL_COPY = {
  fieldLabel: "Invite code (optional)",
  fieldPlaceholder: "Invite code",
  depositVisibility:
    "The person who invited you will see how much you deposit.",
  haveCode: "Have an invite code?",
  settingsTitle: "Have an invite code?",
  settingsApply: "Apply",
  inviteCell: "Invite friends",
  inviteCellSubtitle: "Share your code and see who joined",
  yourCode: "Your code",
  copy: "Copy code",
  copied: "Copied",
  share: "Share link",
  emptyState:
    "Friends who sign up with your link show up here with their status and how much they have deposited.",
  joinedAnInvite: "You joined with an invite.",
  unavailable: "Could not reach Loyal. Try again.",
  retry: "Retry",
} as const;

export const REFUSAL_COPY: Record<
  NonNullable<ReferralClaim["refusal"]>,
  string
> = {
  unknown_code: "That code does not match anyone. Check it and try again.",
  own_code: "That is your own code. Share it with a friend instead.",
  already_bound: "You already joined with an invite.",
  referrer_newer: "That code belongs to someone who joined after you.",
  window_closed: "Invite codes can only be added in the first 7 days.",
};

export const STATUS_LABEL = {
  joined: "Joined",
  deposited: "Deposited",
  withdrawn: "Withdrawn",
} as const;

const PT_DATE = new Intl.DateTimeFormat("en-US", {
  timeZone: "America/Los_Angeles",
  month: "short",
  day: "numeric",
  year: "numeric",
});

const PT_DATE_TIME = new Intl.DateTimeFormat("en-US", {
  timeZone: "America/Los_Angeles",
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

const USD = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
});

export function formatJoinDate(iso: string): string {
  return PT_DATE.format(new Date(iso));
}

export function formatClosesAt(iso: string): string {
  return `You can add a code until ${PT_DATE_TIME.format(new Date(iso))} PT.`;
}

export function formatDeposited(micro: number): string {
  return USD.format(micro / 1_000_000);
}
