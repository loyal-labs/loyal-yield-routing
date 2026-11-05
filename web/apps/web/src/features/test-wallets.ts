const TEST_WALLETS: ReadonlySet<string> = new Set([
  "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ",
  "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
]);

// Go gates the same wallets in go/workers/internal/webapp/test_wallets.go; this only picks the UI. Public keys only.
export function syncTestWalletFlags(address: string | null | undefined) {
  if (!address) return;
  const open = TEST_WALLETS.has(address) || process.env.NODE_ENV === "development";
  try {
    for (const key of ["loyal.bridge", "loyal.referrals"]) {
      if (open) localStorage.setItem(key, "1");
      else localStorage.removeItem(key);
    }
  } catch {}
}
