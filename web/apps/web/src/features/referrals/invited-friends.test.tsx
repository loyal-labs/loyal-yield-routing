import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import type { ReferralFriend } from "@loyal-labs/api";

import { InvitedFriends } from "./invited-friends";

const friend: ReferralFriend = {
  bound_at: "2026-11-19T12:00:00Z",
  deposited_micro: 5000000000,
  label: "f…@example.invalid",
  status: "deposited",
  wallet_label: "3vKk…T6XP",
};

test("a partially paid referral shows earned, paid and outstanding from the ledger", () => {
  const earnings = {
    earned_micro_usdc: "15000000",
    paid_micro_usdc: "9000000",
    outstanding_micro_usdc: "6000000",
    through: friend.bound_at,
  };
  const render = (row: ReferralFriend) => renderToStaticMarkup(<InvitedFriends friends={[row]} />);
  const html = render({ ...friend, earnings });
  for (const [label, amount] of [["Earned", "15"], ["Paid", "9"], ["Still owed", "6"]]) {
    expect(html).toMatch(new RegExp(label + "</dt><dd[^>]*>" + amount + " USDC"));
  }
  expect(html).toContain(friend.wallet_label);
  expect(html).not.toContain(friend.label);
  expect(render(friend)).toContain("Referral earnings are unavailable.");
  const incomplete = render({ ...friend, earnings: {
    ...earnings, has_history_gap: true, earned_micro_usdc: "9000000", outstanding_micro_usdc: "0",
  }});
  expect(incomplete).toContain("≥ 9 USDC");
  expect(incomplete).toContain("Unverified");
  expect(incomplete).toContain("verified minimums");
});
