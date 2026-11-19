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

const columns = "sm:grid-cols-[minmax(0,1.4fr)_repeat(3,minmax(0,1fr))]";

export function InvitedFriends({ friends, rewardsActive }: { friends: ReferralFriend[]; rewardsActive: boolean }) {
  const hasEarnings = friends.some((friend) => friend.earnings);
  const showRewards = rewardsActive || hasEarnings;

  return (
    <section aria-labelledby="invited-friends-title" className="mt-8 border-t border-border pt-6 sm:mt-10">
      <h2 id="invited-friends-title" className="text-xl leading-7 font-semibold tracking-tight">
        Invited friends <span className="text-foreground/60">({friends.length})</span>
      </h2>
      {friends.length === 0 ? (
        <p className="mt-2 text-sm leading-5 text-foreground/70">
          Friends appear here once they sign up with your link.
        </p>
      ) : (
        <>
          {showRewards && <p className="mt-1 text-sm leading-5 text-foreground/70">
            {hasEarnings ? "Lifetime rewards, shown in USDC." : "Referral earnings are unavailable."}
          </p>}
          <div aria-hidden="true" className={`mt-5 hidden gap-x-3 border-b border-border pb-2 text-sm text-foreground/65 sm:grid ${columns}`}>
            <span>Wallet</span>
            {showRewards && <>
              <span className="text-right">Earned</span>
              <span className="text-right">Paid</span>
              <span className="text-right">Still owed</span>
            </>}
          </div>
          <ul className="mt-3 divide-y divide-border sm:mt-0">
            {friends.map((friend) => {
              const wallet = walletLabel(friend);
              const earnings = friend.earnings;
              return (
                <li className={`grid gap-x-3 gap-y-3 py-4 text-[15px] sm:items-baseline ${columns}`} key={`${friend.bound_at}:${wallet}`}>
                  <div className="min-w-0">
                    <span className="font-semibold">{wallet}</span>
                    {earnings && (
                      <p className="mt-1 text-xs leading-4 text-foreground/65">
                        Earned through{" "}
                        <time dateTime={earnings.through}>{throughDate.format(new Date(earnings.through))}</time>
                      </p>
                    )}
                  </div>
                  {earnings ? (
                    <>
                      <dl className="grid grid-cols-3 gap-x-2 sm:contents">
                        {([
                          ["Earned", earnings.earned_micro_usdc],
                          ["Paid", earnings.paid_micro_usdc],
                          ["Still owed", earnings.outstanding_micro_usdc],
                        ] as const).map(([label, amount]) => (
                          <div className="min-w-0 sm:text-right" key={label}>
                            <dt className="text-xs text-foreground/65 sm:sr-only">{label}</dt>
                            <dd className="mt-0.5 break-words font-medium tabular-nums sm:mt-0">
                              {rewardValue(amount, Boolean(earnings.has_history_gap) && label !== "Paid")}
                            </dd>
                          </div>
                        ))}
                      </dl>
                      {earnings.has_history_gap && (
                        <p className="text-xs leading-4 text-foreground/65 sm:col-span-4">
                          Some deposit history is missing. Earned and still owed show verified minimums.
                        </p>
                      )}
                    </>
                  ) : hasEarnings ? (
                    <p className="text-sm text-foreground/65 sm:col-span-3 sm:text-right">Earnings unavailable</p>
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
