export type SmartAccountCreationPoint = {
  count: number;
  date: string;
};

export type SmartAccountSpendPoint = {
  amount: number;
  date: string;
  usd: number | null;
};

export type SmartAccountRegistrationRow = {
  id: string;
  registeredAt: string;
  solanaEnv: string;
  sponsorshipSignature: string | null;
  userAddress: string;
  vaultAddress: string | null;
};

export type SmartAccountsData = {
  creationPoints: SmartAccountCreationPoint[];
  registrations: SmartAccountRegistrationRow[];
  solPriceUsd: number | null;
  spendPoints: SmartAccountSpendPoint[];
  totalAccounts: number;
  totalCreated30d: number;
  totalSpentSol30d: number;
  totalSpentUsd30d: number | null;
};
