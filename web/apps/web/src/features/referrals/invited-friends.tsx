import type { ReferralFriend } from "@loyal-labs/api";

const truncatedWallet = /^[1-9A-HJ-NP-Za-km-z]{4}…[1-9A-HJ-NP-Za-km-z]{4}$/;

function walletLabel(friend: ReferralFriend) {
  const label = friend.wallet_label ?? friend.label;
  return truncatedWallet.test(label) ? label : "Wallet unavailable";
}

export function InvitedFriends({ friends }: { friends: ReferralFriend[] }) {
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
            Referral earnings are unavailable.
          </p>
          <ul className="mt-2 max-h-48 overflow-y-auto divide-y divide-border">
            {friends.map((friend) => {
              const wallet = walletLabel(friend);
              return (
                <li
                  className="py-2.5 font-medium text-[15px] leading-5"
                  key={`${friend.bound_at}:${wallet}`}
                >
                  {wallet}
                </li>
              );
            })}
          </ul>
        </>
      )}
    </section>
  );
}
