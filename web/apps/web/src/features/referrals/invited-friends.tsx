import type { ReferralFriend } from "@loyal-labs/api";
import { formatUnits } from "viem/utils";

const truncatedWallet = /^[1-9A-HJ-NP-Za-km-z]{4}…[1-9A-HJ-NP-Za-km-z]{4}$/;

function walletLabel(friend: ReferralFriend) {
  const label = friend.wallet_label ?? friend.label;
  return truncatedWallet.test(label) ? label : "Wallet unavailable";
}

const throughDate = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

function rewardValue(amount: string, minimum: boolean) {
  if (minimum && amount === "0") return "Unverified";
  return `${minimum ? "≥ " : ""}${formatUnits(BigInt(amount), 6)} USDC`;
}

export function InvitedFriends({ friends }: { friends: ReferralFriend[] }) {
  const hasEarnings = friends.some((friend) => friend.earnings);
  return (
    <section
      aria-label="Invited friends"
      className="mx-6 border-border border-t pt-5 pb-3"
    >
      <h3 className="font-semibold text-base leading-5">
        Invited friends{friends.length > 0 ? ` (${friends.length})` : ""}
      </h3>
      {friends.length === 0 ? (
        <p className="mt-2 text-muted-foreground text-sm">
          No friends have joined yet.
        </p>
      ) : (
        <>
          <p className="mt-1 text-muted-foreground text-sm">
            {hasEarnings
              ? "Rewards are paid in Earn Max shares. Amounts shown in USDC."
              : "Referral earnings are unavailable."}
          </p>
          <ul className="mt-2 max-h-48 overflow-y-auto divide-y divide-border">
            {friends.map((friend) => {
              const wallet = walletLabel(friend);
              return (
                <li
                  className="py-2.5 font-medium text-[15px] leading-5"
                  key={`${friend.bound_at}:${wallet}`}
                >
                  <span>{wallet}</span>
                  {friend.earnings ? (
                    <>
                      <dl className="mt-2 grid grid-cols-3 gap-x-2 text-xs">
                        {([
                          ["Earned", friend.earnings.earned_micro_usdc],
                          ["Paid", friend.earnings.paid_micro_usdc],
                          ["Still owed", friend.earnings.outstanding_micro_usdc],
                        ] as const).map(([label, amount]) => (
                          <div key={label}>
                            <dt className="font-normal text-muted-foreground">{label}</dt>
                            <dd className="tabular-nums">
                              {rewardValue(amount, Boolean(friend.earnings?.has_history_gap) && label !== "Paid")}
                            </dd>
                          </div>
                        ))}
                      </dl>
                      {friend.earnings.has_history_gap && (
                        <p className="mt-2 font-normal text-muted-foreground text-xs">
                          Some deposit history is missing. Earned and still owed show verified minimums.
                        </p>
                      )}
                      <p className="mt-2 font-normal text-muted-foreground text-xs">
                        Earned through{" "}
                        <time dateTime={friend.earnings.through}>
                          {throughDate.format(new Date(friend.earnings.through))}
                        </time>
                      </p>
                    </>
                  ) : hasEarnings ? (
                    <p className="mt-1 font-normal text-muted-foreground text-xs">
                      Earnings unavailable
                    </p>
                  ) : null}
                </li>
              );
            })}
          </ul>
        </>
      )}
    </section>
  );
}
