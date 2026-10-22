type FundingRole =
  | "sponsorship"
  | "policy"
  | "deployment"
  | "route_fee_payer"
  | "settings_authority";

export type FundingStatus = "healthy" | "low" | "critical" | "unknown";

export type EarnFundingWallet = {
  address: string;
  balanceLamports: string | null;
  balanceObservedAt: string | null;
  balanceSlot: number | null;
  balanceError: string | null;
  configuredAddresses: string[];
  observedAddresses: string[];
  mismatch: boolean;
  roles: Array<{
    key: FundingRole;
    label: string;
    observedCount: number | null;
    latestObservedAt: string | null;
  }>;
  status: FundingStatus;
  statusDetail: string;
  minimumLamports: string | null;
  spend24hLamports: string | null;
  spend7dLamports: string | null;
  runwayHours: number | null;
};

export type OperationalWalletSpendGroup =
  | "gasless_store"
  | "gasless_top_up_to_0_01_sol"
  | "gasless_verify_telegram_init_data"
  | "lookup_table_close"
  | "lookup_table_create"
  | "lookup_table_deactivate"
  | "lookup_table_extend"
  | "lookup_table_rollover"
  | "lookup_table_verify"
  | "smart_account_sponsorship"
  | "yield_route_fee";

export type OperationalWalletSpendEvent = {
  address: string;
  amountBasis: "compiled_fee" | "confirmed_payer_outflow";
  group: OperationalWalletSpendGroup;
  lamports: string;
  occurredAt: string;
  signature: string | null;
  transactionCount: number;
};

export type EarnFundingData = {
  wallets: EarnFundingWallet[];
  missingRoles: Array<{ key: FundingRole; label: string; reason: string }>;
  spendEvents: OperationalWalletSpendEvent[];
  spendSourceErrors: string[];
  spendWindow: { endedAt: string; startedAt: string };
  sourceErrors: string[];
  observedAt: string;
};
