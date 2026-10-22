export type AdminEarnSnapshot = {
  activeAutodepositPolicies: number;
  activeAumRaw: bigint;
  activePrincipalRaw: bigint;
  ageMs: number;
  refreshedAt: string;
  state: "current" | "stale";
  uniqueEarnPolicies: number;
  uniqueEarnUsers: number;
};
