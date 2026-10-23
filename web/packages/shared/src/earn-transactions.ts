export type EarnTransactionKind =
  | "autodeposit_action"
  | "balance_sweep"
  | "deposit"
  | "withdraw"
  | "rebalance"
  | "reconciliation";

export type EarnTransactionEventType =
  | "autodeposit_closed"
  | "autodeposit_created"
  | "balance_sweep"
  | "deposit_initialized"
  | "deposit_top_up"
  | "withdrawal_partial"
  | "withdrawal_full"
  | "rebalance_confirmed"
  | "snapshot_reconciled";

export function getEarnTransactionRowLabel(item: {
  eventType: EarnTransactionEventType;
  kind: EarnTransactionKind;
}): string {
  switch (item.eventType) {
    case "autodeposit_created":
      return "Create allowance";
    case "autodeposit_closed":
      return "Remove allowance";
    case "balance_sweep":
      return "Balance sweep";
    case "deposit_initialized":
      return "Create & Deposit";
    case "withdrawal_full":
      return "Withdraw & Close";
  }

  switch (item.kind) {
    case "deposit":
      return "Deposit";
    case "withdraw":
      return "Withdraw";
    case "rebalance":
      return "Rebalanced";
    case "reconciliation":
      return "Reconciled";
    case "balance_sweep":
      return "Balance sweep";
    case "autodeposit_action":
      return "Allowance";
  }
}
