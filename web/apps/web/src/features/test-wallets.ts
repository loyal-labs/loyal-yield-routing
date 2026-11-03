const TEST_WALLETS: ReadonlySet<string> = new Set([
  "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
  "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
]);

// Owner test wallets get the gated bridge and referrals UI without a manual localStorage step. Public keys only.
export function unlockTestWalletFlags(address: string | null | undefined) {
  if (!address || !TEST_WALLETS.has(address)) return;
  try {
    localStorage.setItem("loyal.bridge", "1");
    localStorage.setItem("loyal.referrals", "1");
  } catch {}
}
