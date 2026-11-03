import type { ReferralClaim } from "@loyal-labs/api";

export type ClaimFailure =
  | NonNullable<ReferralClaim["refusal"]>
  | "malformed"
  | "rate_limited"
  | "retry";

export const failureCopy: Record<ClaimFailure, string> = {
  unknown_code: "That invite code doesn't exist. Check it and try again.",
  own_code: "That's your own invite code.",
  already_bound: "You already joined with an invite.",
  referrer_newer: "That invite can't be used on your account.",
  window_closed: "The time to add an invite code has passed.",
  malformed: "Invite codes are 7 letters and numbers.",
  rate_limited: "Too many tries. Wait a few minutes and try again.",
  retry: "Couldn't reach Loyal. Try again.",
};

export const referralCopy = {
  joined: "You joined with an invite",
  label: "Invite code",
  placeholder: "Invite code (optional)",
  apply: "Apply",
  applying: "Applying…",
  visibility: "Your inviter will see your deposit total.",
};

const closesAtFormat = new Intl.DateTimeFormat("en-US", {
  timeZone: "America/Los_Angeles",
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

export function closesCopy(closesAt: string): string {
  return `Add a code until ${closesAtFormat.format(new Date(closesAt))} PT`;
}
